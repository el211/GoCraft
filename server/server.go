// Package server wires together the game core, Java and Bedrock network
// listeners, configuration, and protocol handlers into a runnable GoCraft
// server.
//
// Architecture:
//
//	server.Server
//	  ├─ core/game.Game          — edition-agnostic player registry
//	  ├─ core/intent.Queue       — simulation command bus (M14.1+)
//	  ├─ java adapter            — TCP listener + Java protocol handlers
//	  │    ├─ java/network       — ClientConn, Listener
//	  │    └─ java/handler       — Handshake, Status, Login, Config, Play
//	  └─ bedrock adapter         — RakNet/UDP listener via gophertunnel
//	       └─ bedrock.Listener   — M14.0: accept + auth; M14.1+: play loop
package server

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on the default mux
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"GoCraft/bedrock"
	"GoCraft/config"
	corentity "GoCraft/core/entity"
	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/auth"
	"GoCraft/java/handler"
	"GoCraft/java/network"
	"GoCraft/java/registry"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
	"GoCraft/java/world/anvil"
)

// Server owns the game core and both Java and Bedrock network listeners.
type Server struct {
	cfg  *config.Config
	game *game.Game

	// Java adapter resources
	listener *network.Listener

	// RSA keypair — generated once at startup, shared across all connections.
	privKey   *rsa.PrivateKey
	pubKeyDER []byte

	loginHandler *handler.LoginHandler

	// World and Java encoding resources.
	world       *coreworld.World
	spawnX      int
	spawnZ      int
	regProvider registry.Provider
	chunkSender *javaworld.Sender
	sessions    *session.Manager
	cmds        *handler.Dispatcher

	// Bedrock adapter (nil when bedrock.enabled = false).
	bedrockListener *bedrock.Listener
	javaCrossKnown  map[[16]byte]map[[16]byte]crossPlayerView

	// intentBus is the cross-adapter simulation command bus.
	// Both Java (M14.1+) and Bedrock handlers post intents here; the tick
	// goroutine drains and applies them once per tick.
	intentBus *intent.Bus

	// connCount tracks the number of active TCP connections (Java).
	connCount atomic.Int64

	// mobAIs tracks per-entity wander state indexed by entity ID.
	// Written and read only by the tick goroutine, so no lock is needed.
	mobAIs map[int32]*mobAI

	// worldAge is advanced only by the entity tick goroutine.
	worldAge int64
	spawnRNG *rand.Rand

	// sleepAllTick is the worldAge tick at which ALL online players were first
	// detected sleeping.  0 means nobody is sleeping or the check hasn't fired.
	// The tick goroutine waits sleepAnimTicks before skipping the night.
	sleepAllTick int64

	// timings collects per-subsystem tick durations for /timings and /tps.
	timings *tickTimings
}

// mobAI holds the wander state for a passive mob.
// All fields are written only by the entity tick goroutine.
type mobAI struct {
	homeX, homeZ   float64    // world-space spawn/home position (homed mobs only)
	dirX, dirZ     float64    // current normalised walk direction
	wanderTick     int        // ticks until next direction pick (0 = pick now)
	pauseTick      int        // remaining ticks of stillness (overrides wanderTick)
	panicTick      int        // remaining ticks fleeing from a recent attacker
	knockbackTick  int        // ticks retaining the configured initial hit velocity
	roaming        bool       // true = no fixed home (animals); false = homed (villagers)
	rng            *rand.Rand // per-entity PRNG seeded from entity ID
	sleepingWas    bool       // previous-tick sleeping state — detects transitions for metadata broadcast
	hasTarget      bool       // hostile AI: currently chasing a target
	targetX        float64    // hostile AI: current target world X
	targetZ        float64    // hostile AI: current target world Z
	attackCooldown int        // ticks until next melee swing
	fuseTick       int        // creeper fuse progress (30 ticks to detonation)
}

type crossPlayerView struct {
	player    *player.Player
	position  spatial.Vec3
	rotation  spatial.Rotation
	inventory [player.InventorySize]player.ItemStack
	heldSlot  int
	dead      bool
}

// New creates a Server with the given configuration.
// It initialises the game core and generates the RSA keypair for online-mode auth.
func New(cfg *config.Config) (*Server, error) {
	handler.ConfigureItemTooltips(
		cfg.ItemTooltips.ShowDurability,
		cfg.ItemTooltips.ShowAttributes,
		cfg.ItemTooltips.HideVanillaAttributes,
		!cfg.Combat.AttackCooldown,
	)
	privKey, err := auth.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("server: generating RSA keypair: %w", err)
	}
	pubKeyDER, err := auth.MarshalPublicKeyDER(&privKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("server: marshalling public key: %w", err)
	}

	// Open Anvil persistence when WorldDir is configured; fall back to a
	// seeded generation-only world otherwise. A real level.dat is authoritative
	// for the seed and spawn so generated fallback chunks do not form seams.
	var storage coreworld.Storage
	spawnX, spawnZ := 0, 0
	var initialWorldAge int64
	if cfg.WorldStorage == config.WorldStorageDisk {
		if metadata, metadataErr := anvil.LoadLevelMetadata(cfg.WorldDir); metadataErr == nil {
			cfg.WorldSeed = metadata.Seed
			spawnX, spawnZ = int(metadata.SpawnX), int(metadata.SpawnZ)
			// Resume the world clock from where the vanilla server left off so
			// the day/night cycle continues naturally across restarts.
			// gocraft_time.dat (written by saveWorldAge) takes priority because it
			// tracks the live clock; level.dat only has the time at the last
			// vanilla-server save.
			if saved, ok := loadSavedWorldAge(cfg.WorldDir); ok {
				initialWorldAge = saved
			} else if metadata.Time > 0 {
				initialWorldAge = metadata.Time
			}
			slog.Info("server: loaded Java level.dat",
				"world", metadata.LevelName,
				"dataVersion", metadata.DataVersion,
				"version", metadata.VersionName,
				"seed", metadata.Seed,
				"spawnX", metadata.SpawnX,
				"spawnY", metadata.SpawnY,
				"spawnZ", metadata.SpawnZ,
				"time", metadata.Time,
				"dayTime", metadata.DayTime,
			)
		} else if !errors.Is(metadataErr, os.ErrNotExist) {
			slog.Warn("server: could not parse level.dat", "worldDir", cfg.WorldDir, "err", metadataErr)
		}

		// Resolve the world seed: seed=0 means "random".
		// We persist the chosen seed in world/seed.txt so that restarting the
		// server always regenerates the exact same world (no chunk seams).
		cfg.WorldSeed = resolveWorldSeed(cfg.WorldSeed, cfg.WorldDir)

		st, err := anvil.NewStorage(cfg.WorldDir)
		if err != nil {
			return nil, fmt.Errorf("server: opening Anvil storage %s: %w", cfg.WorldDir, err)
		}
		storage = st
		slog.Info("server: opened Anvil world", "worldDir", cfg.WorldDir, "storage", "disk")
	} else {
		// In-memory world: resolve seed without persisting.
		cfg.WorldSeed = resolveWorldSeed(cfg.WorldSeed, "")
		slog.Info("server: using memory-only world storage", "storage", "memory")
	}

	gameCore := game.New()
	if err := handler.ConfigureOperators(`ops.json`, cfg.Operators); err != nil {
		slog.Warn(`server: could not load ops.json`, `err`, err)
	}
	if err := handler.ConfigureWhitelist(`whitelist.json`, cfg.Whitelist.Enabled, cfg.Whitelist.Players); err != nil {
		slog.Warn(`server: could not load whitelist.json`, `err`, err)
	}
	cmds := handler.NewDispatcher()
	cmds.SetEntityIDAllocator(gameCore.NextEntityID)
	cmds.SetPlayerFinder(func(name string) *player.Player {
		var found *player.Player
		gameCore.OnlinePlayers(func(candidate *player.Player) {
			if found == nil && strings.EqualFold(candidate.Username, name) {
				found = candidate
			}
		})
		return found
	})
	cmds.SetPlayerLister(func() []*player.Player {
		players := make([]*player.Player, 0, gameCore.OnlineCount())
		gameCore.OnlinePlayers(func(online *player.Player) {
			players = append(players, online)
		})
		return players
	})
	cmds.SetMaxPlayers(cfg.MaxPlayers)
	handler.RegisterBuiltins(cmds)

	bus := intent.NewBus(64, 512)

	slog.Info("server: world seed resolved", "seed", cfg.WorldSeed)
	worldInstance := coreworld.New(coreworld.NewOverworldGenerator(cfg.WorldSeed), storage, cfg.Villagers)
	worldInstance.SetMaxCachedChunks(cfg.MaxCachedChunks)

	timings := newTickTimings(func() (int, int) {
		return gameCore.OnlineCount(), cfg.MaxPlayers
	})

	s := &Server{
		cfg:            cfg,
		game:           gameCore,
		privKey:        privKey,
		pubKeyDER:      pubKeyDER,
		world:          worldInstance,
		spawnX:         spawnX,
		spawnZ:         spawnZ,
		regProvider:    &registry.VanillaProvider{},
		chunkSender:    javaworld.DefaultSender,
		sessions:       session.NewManager(),
		cmds:           cmds,
		intentBus:      bus,
		mobAIs:         make(map[int32]*mobAI),
		spawnRNG:       rand.New(rand.NewSource(cfg.WorldSeed ^ 0x4d6f624372616674)),
		worldAge:       initialWorldAge,
		timings:        timings,
		javaCrossKnown: make(map[[16]byte]map[[16]byte]crossPlayerView),
	}

	// Register server-state commands as closures after s is initialised.
	cmds.Register("timings", func(ctx handler.CommandContext) error {
		report := timings.Report()
		if ctx.Reply != nil {
			return ctx.Reply(report)
		}
		return handler.SendSystemMessage(ctx.Conn, report)
	})
	cmds.Register("tps", func(ctx handler.CommandContext) error {
		tps, avgMs := timings.TPS()
		color := "§a"
		switch {
		case tps < 15:
			color = "§c"
		case tps < 18:
			color = "§e"
		}
		message := fmt.Sprintf("TPS: %s%.1f§r  Avg tick: §f%.2fms", color, tps, avgMs)
		if ctx.Reply != nil {
			return ctx.Reply(message)
		}
		return handler.SendSystemMessage(ctx.Conn, message)
	})
	cmds.Register("time", func(ctx handler.CommandContext) error {
		if len(ctx.Args) == 0 {
			tod := s.worldAge % 24000
			return handler.SendSystemMessage(ctx.Conn,
				fmt.Sprintf("Time of day: %d (world age: %d)", tod, s.worldAge))
		}
		switch strings.ToLower(ctx.Args[0]) {
		case "day":
			// Jump to noon (6000).
			tod := s.worldAge % 24000
			if tod <= 6000 {
				s.worldAge += 6000 - tod
			} else {
				s.worldAge += 24000 - tod + 6000
			}
		case "night":
			// Jump to midnight (18000).
			tod := s.worldAge % 24000
			if tod <= 18000 {
				s.worldAge += 18000 - tod
			} else {
				s.worldAge += 24000 - tod + 18000
			}
		case "set":
			if len(ctx.Args) < 2 {
				return fmt.Errorf("usage: /time set <0-23999>")
			}
			val, err := strconv.ParseInt(ctx.Args[1], 10, 64)
			if err != nil || val < 0 || val > 23999 {
				return fmt.Errorf("time value must be 0–23999")
			}
			tod := s.worldAge % 24000
			if tod <= val {
				s.worldAge += val - tod
			} else {
				s.worldAge += 24000 - tod + val
			}
		default:
			return fmt.Errorf("usage: /time <day|night|set <0-23999>>")
		}
		handler.DispatchWorldTime(s.worldAge, s.worldAge%24000, s.sessions)
		return handler.SendSystemMessage(ctx.Conn,
			fmt.Sprintf("Time set to %d", s.worldAge%24000))
	})
	cmds.RequireOperator(`timings`, `tps`, `time`)
	// Warm spawn immediately; login-time streaming will reuse this cache.
	s.world.QueuePregeneration(int32(math.Floor(float64(spawnX)/16)), int32(math.Floor(float64(spawnZ)/16)), int32(cfg.PreGenerateRadius))
	s.loginHandler = handler.NewLoginHandler(cfg, privKey, pubKeyDER)
	s.listener = network.NewListener(cfg.Addr(), s.handleConn)

	if cfg.Bedrock.Enabled {
		s.bedrockListener = bedrock.NewListener(
			cfg.Bedrock,
			bus,
			worldInstance,
			gameCore,
			cfg.WorldSeed,
			spawnX,
			spawnZ,
			configuredGameMode(cfg.DefaultGameMode),
			difficultyID(cfg.Difficulty),
		)
		s.sessions.SetMessageObserver(s.bedrockListener.BroadcastMessage)
		s.sessions.SetExternalKnockbackHandler(func(p *player.Player, sourceX, sourceZ, horizontal, vertical float64) {
			s.sendLegacyPlayerKnockback(&session.Session{Player: p}, sourceX, sourceZ, horizontal, vertical)
		})
		worldInstance.SetBlockObserver(s.bedrockListener.BroadcastBlockChange)
	}
	return s, nil
}

// Run starts the server and blocks until ctx is cancelled or a fatal error occurs.
// All background goroutines are tracked with a WaitGroup and are joined before
// the world is flushed to disk, ensuring clean shutdown of both listeners.
func (s *Server) Run(ctx context.Context) error {
	go s.runConsole(ctx)
	if s.cfg.JavaEnabled {
		slog.Info("java listener enabled",
			"addr", s.cfg.Addr(),
			"version", s.cfg.VersionName,
			"protocol", s.cfg.ProtocolVersion,
			"onlineMode", s.cfg.OnlineMode,
		)
	}
	if s.cfg.Bedrock.Enabled {
		slog.Info("bedrock listener enabled",
			"addr", s.cfg.Bedrock.Address,
			"onlineMode", s.cfg.Bedrock.OnlineMode,
		)
	}
	slog.Info("starting GoCraft server", "motd", s.cfg.MOTD, "worldSeed", s.cfg.WorldSeed,
		"worldStorage", s.cfg.WorldStorage, "maxCachedChunks", s.cfg.MaxCachedChunks)

	// Spawn a small set of passive mobs near the world spawn for testing.
	s.spawnTestMobs()

	// pprof profiling endpoint — http://localhost:6060/debug/pprof/
	// Use: go tool pprof http://localhost:6060/debug/pprof/goroutine
	//      go tool pprof http://localhost:6060/debug/pprof/heap
	go func() {
		pprofAddr := "localhost:6060"
		slog.Info("pprof profiling server listening", "addr", pprofAddr)
		if err := http.ListenAndServe(pprofAddr, nil); err != nil {
			slog.Warn("pprof server stopped", "err", err)
		}
	}()

	var wg sync.WaitGroup

	// Entity tick + intent processing at 20 TPS.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runEntityTick(ctx)
	}()

	// Bedrock UDP listener (when enabled).
	if s.bedrockListener != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.bedrockListener.Listen(ctx); err != nil {
				slog.Error("bedrock listener stopped with error", "err", err)
			}
		}()
	}

	// Java TCP listener on the main goroutine, or block on ctx if disabled.
	var listenErr error
	if s.cfg.JavaEnabled {
		listenErr = s.listener.Listen(ctx)
	} else {
		<-ctx.Done()
	}

	// ctx is now done: wait for entity tick and Bedrock listener to finish.
	wg.Wait()

	// Flush world to disk regardless of shutdown cause.
	if closeErr := s.world.Close(); closeErr != nil {
		slog.Warn("server: error flushing world on shutdown", "err", closeErr)
	}
	s.saveWorldAge()
	return listenErr
}

// runConsole executes commands written to stdin by Pterodactyl or a local
// terminal. Scanner is intentionally not part of the shutdown WaitGroup:
// stdin cannot be cancelled portably, and the process owns its lifetime.
func (s *Server) runConsole(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == `` {
			continue
		}
		result := s.executeConsoleCommand(line)
		if strings.ContainsRune(result, '\n') {
			slog.Info(`console command`, `command`, line, `result`, `multi-line output follows`)
			fmt.Fprintln(os.Stdout, result)
		} else {
			slog.Info(`console command`, `command`, line, `result`, result)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn(`console input stopped`, `err`, err)
	}
}

func (s *Server) executeConsoleCommand(input string) string {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(input), `/`))
	if len(fields) == 0 {
		return `No command entered`
	}
	switch strings.ToLower(fields[0]) {
	case `list`:
		names := make([]string, 0)
		if s.game != nil {
			names = make([]string, 0, s.game.OnlineCount())
			s.game.OnlinePlayers(func(online *player.Player) {
				if online != nil {
					names = append(names, online.Username)
				}
			})
		}
		sort.Slice(names, func(i, j int) bool {
			return strings.ToLower(names[i]) < strings.ToLower(names[j])
		})
		maximum := 0
		if s.cfg != nil {
			maximum = s.cfg.MaxPlayers
		}
		capacity := fmt.Sprintf(`%d`, len(names))
		if maximum > 0 {
			capacity = fmt.Sprintf(`%d/%d`, len(names), maximum)
		}
		joined := strings.Join(names, `, `)
		if joined == `` {
			joined = `no players`
		}
		return fmt.Sprintf(`Online (%s): %s`, capacity, joined)
	case `timings`:
		if s.timings == nil {
			return `Timing collector is unavailable`
		}
		return stripMinecraftFormatting(s.timings.Report())
	case `tps`:
		if s.timings == nil {
			return `Timing collector is unavailable`
		}
		tps, avgMs := s.timings.TPS()
		return fmt.Sprintf(`TPS: %.1f  Avg tick: %.2fms`, tps, avgMs)
	case `op`:
		if len(fields) != 2 {
			return `Usage: op <player>`
		}
		name := fields[1]
		if err := handler.SetOperator(name); err != nil {
			return fmt.Sprintf(`Could not save ops.json: %v`, err)
		}
		var target *player.Player
		s.game.OnlinePlayers(func(candidate *player.Player) {
			if target == nil && strings.EqualFold(candidate.Username, name) {
				target = candidate
			}
		})
		if target != nil {
			target.Operator = true
			if s.bedrockListener != nil {
				s.bedrockListener.RefreshPlayerAbilities(target)
			}
		}
		return fmt.Sprintf(`Made %s a server operator`, name)
	case `whitelist`:
		if len(fields) < 2 {
			return fmt.Sprintf(`Whitelist enabled: %v; players: %s`, handler.WhitelistEnabled(), strings.Join(handler.WhitelistedPlayers(), `, `))
		}
		switch strings.ToLower(fields[1]) {
		case `on`, `off`:
			enabled := strings.EqualFold(fields[1], `on`)
			if err := handler.SetWhitelistEnabled(enabled); err != nil {
				return fmt.Sprintf(`Could not save whitelist.json: %v`, err)
			}
			return fmt.Sprintf(`Whitelist enabled: %v`, enabled)
		case `add`:
			if len(fields) != 3 {
				return `Usage: whitelist add <player>`
			}
			if err := handler.AddWhitelistedPlayer(fields[2]); err != nil {
				return fmt.Sprintf(`Could not save whitelist.json: %v`, err)
			}
			return fmt.Sprintf(`Added %s to the whitelist`, fields[2])
		case `remove`:
			if len(fields) != 3 {
				return `Usage: whitelist remove <player>`
			}
			removed, err := handler.RemoveWhitelistedPlayer(fields[2])
			if err != nil {
				return fmt.Sprintf(`Could not save whitelist.json: %v`, err)
			}
			if !removed {
				return fmt.Sprintf(`%s is not whitelisted`, fields[2])
			}
			return fmt.Sprintf(`Removed %s from the whitelist`, fields[2])
		case `list`:
			return `Whitelisted players: ` + strings.Join(handler.WhitelistedPlayers(), `, `)
		default:
			return `Usage: whitelist <on|off|add|remove|list>`
		}
	default:
		return fmt.Sprintf(`Unknown console command: %s`, fields[0])
	}
}

// runEntityTick fires tickEntities and tickIntents at 20 TPS until ctx is done.
func (s *Server) runEntityTick(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.safeTick()
		}
	}
}

// safeTick wraps a single game tick in a recover so that a panic in any tick
// subsystem logs the stack trace and restarts the tick rather than crashing
// the entire server process.
func (s *Server) safeTick() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC in tick goroutine — server recovered",
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	s.tickIntents()
	s.tickEntities()
	if s.bedrockListener != nil {
		s.bedrockListener.Sync(uint64(s.worldAge))
		s.syncBedrockPlayersToJava()
	}
	if s.worldAge%600 == 0 {
		if err := s.world.Flush(); err != nil {
			slog.Warn("world autosave failed", "err", err)
		}
		s.saveWorldAge()
	}
}

// tickIntents drains the intent bus and applies each intent to world/player state.
// This is the sole point of mutating player state from adapter goroutines.
func (s *Server) tickIntents() {
	dr := s.intentBus.Drain()

	for _, l := range dr.Lifecycle {
		switch i := l.(type) {
		case intent.JoinIntent:
			s.applyJoin(i)
		case intent.DisconnectIntent:
			s.applyDisconnect(i)
		}
	}

	for _, m := range dr.Moves {
		s.applyMove(m)
	}

	for _, g := range dr.Gameplay {
		switch i := g.(type) {
		case intent.ChatIntent:
			s.applyChat(i)
		case intent.BlockInteractIntent:
			s.applyBedrockBlockInteract(i)
		case intent.EntityInteractIntent:
			s.applyBedrockEntityInteract(i)
		case intent.RespawnIntent:
			s.applyBedrockRespawn(i)
		case intent.WakeIntent:
			if p := s.game.GetPlayer(i.PlayerUUID); p != nil && p.Sleeping {
				p.Sleeping = false
				handler.BroadcastPlayerWaking(p.EntityID, s.sessions)
			}
		case intent.HotbarIntent:
			if p := s.game.GetPlayer(i.PlayerUUID); p != nil && i.Slot >= 0 && i.Slot < 9 {
				p.HeldSlot = int(i.Slot)
			}
		case intent.PlayerStateIntent:
			s.applyBedrockPlayerState(i)
		case intent.InventoryIntent:
			s.applyBedrockInventory(i)
		}
	}
}

func (s *Server) applyBedrockPlayerState(i intent.PlayerStateIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock || p.Dead {
		return
	}
	switch i.State {
	case intent.PlayerStateSprinting:
		p.Sprinting = i.Enabled
	case intent.PlayerStateFlying:
		allowed := p.AllowFlying || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator
		p.Flying = allowed && i.Enabled
	}
}

// applyJoin creates a canonical Player, registers it with the game core, and
// sends a JoinResult to the waiting adapter goroutine.
func (s *Server) applyJoin(i intent.JoinIntent) {
	if !handler.IsWhitelisted(i.Username) {
		err := fmt.Errorf("you are not whitelisted on this server")
		slog.Warn("applyJoin: player rejected by whitelist", "name", i.Username, "edition", i.Edition)
		i.Done <- intent.JoinResult{Err: err}
		return
	}
	edition := player.ClientEditionBedrock
	if i.Edition == "java" {
		edition = player.ClientEditionJava
	}

	p := player.New(i.PlayerUUID, i.Username, edition)
	p.Operator = handler.IsOperatorName(i.Username)
	p.InvulnerableUntil = time.Now().Add(3 * time.Second)
	p.GameMode = configuredGameMode(s.cfg.DefaultGameMode)
	p.AttackCooldown = s.cfg.Combat.AttackCooldown
	p.KnockbackHorizontal = s.cfg.Combat.KnockbackHorizontal
	p.KnockbackVertical = s.cfg.Combat.KnockbackVertical
	p.OnDeath = s.dropPlayerInventory
	p.Position = spatial.Vec3{
		X: float64(s.spawnX) + 0.5,
		Y: float64(s.world.SurfaceY(s.spawnX, s.spawnZ) + 1),
		Z: float64(s.spawnZ) + 0.5,
	}
	p.WorldSpawn = p.Position
	if err := s.game.AddPlayer(p); err != nil {
		slog.Warn("applyJoin: duplicate player UUID",
			"name", i.Username, "uuid", i.PlayerUUID, "err", err)
		i.Done <- intent.JoinResult{Err: err}
		return
	}

	handler.OnlineCount.Store(int32(s.game.OnlineCount()))
	slog.Info("player joined via intent",
		"name", p.Username, "uuid", p.UUID,
		"edition", i.Edition, "trusted", i.TrustedIdentity,
		"entityID", p.EntityID)

	i.Done <- intent.JoinResult{
		EntityID: p.EntityID,
		Position: p.Position,
	}
}

// applyDisconnect removes a player from the game core and logs the event.
func (s *Server) applyDisconnect(i intent.DisconnectIntent) {
	s.game.RemovePlayer(i.PlayerUUID)
	handler.OnlineCount.Store(int32(s.game.OnlineCount()))
	slog.Info("player disconnected via intent",
		"uuid", i.PlayerUUID, "reason", i.Reason)
}

// applyMove updates the canonical player position in the game core.
// The tick goroutine is the sole writer of player state, so no lock is needed.
// Broadcasting the new position to other sessions is deferred to M14.2.
func (s *Server) applyMove(m intent.MoveIntent) {
	p := s.game.GetPlayer(m.PlayerUUID)
	if p == nil {
		return // player disconnected between intent post and drain
	}
	_, _, _, dead := p.HealthSnapshot()
	if dead {
		return // death-camera movement must not move the canonical player entity
	}
	previousY, previousOnGround := p.Position.Y, p.OnGround
	p.Position = m.Position
	p.Rotation = m.Rotation
	p.OnGround = m.OnGround
	if p.VehicleEntityID != 0 {
		if vehicle, ok := s.world.Entities.Get(p.VehicleEntityID); ok && corentity.IsBoat(vehicle.Type) && vehicle.RiderEntityID == p.EntityID {
			vehicle.Position.X = m.Position.X
			vehicle.Position.Z = m.Position.Z
			vehicle.Yaw = m.Rotation.Yaw
		}
	}
	if p.Edition == player.ClientEditionBedrock {
		s.applyBedrockMovementDamage(p, previousY, previousOnGround)
	}
}

// allPlayerSessions adapts every canonical online player to the Java combat
// helper's lightweight Session shape. Bedrock players intentionally have a nil
// Conn: health/death remains canonical and their adapter publishes it on Sync.
func (s *Server) allPlayerSessions() []*session.Session {
	out := make([]*session.Session, 0, s.game.OnlineCount())
	s.game.OnlinePlayers(func(p *player.Player) {
		if javaSession, ok := s.sessions.Get(p.UUID); ok {
			out = append(out, javaSession)
			return
		}
		out = append(out, &session.Session{Player: p})
	})
	return out
}

func (s *Server) sendLegacyPlayerKnockback(target *session.Session, sourceX, sourceZ, horizontal, vertical float64) {
	if target == nil || target.Player == nil {
		return
	}
	if target.Conn != nil {
		handler.SendLegacyKnockback(target, sourceX, sourceZ, horizontal, vertical)
		return
	}
	if s.bedrockListener == nil {
		return
	}
	dx := target.Player.Position.X - sourceX
	dz := target.Player.Position.Z - sourceZ
	distance := math.Hypot(dx, dz)
	if distance < 0.0001 {
		dx, dz, distance = 0, 1, 1
	}
	resistance := 1 - float64(target.Player.KnockbackResistance())
	s.bedrockListener.SendVelocity(target.Player.UUID, spatial.Vec3{
		X: dx / distance * horizontal * resistance,
		Y: vertical * resistance,
		Z: dz / distance * horizontal * resistance,
	}, uint64(s.worldAge))
}

func (s *Server) sendPlayerVelocity(target *session.Session, x, y, z float64) {
	if target == nil || target.Player == nil {
		return
	}
	resistance := 1 - float64(target.Player.KnockbackResistance())
	x, y, z = x*resistance, y*resistance, z*resistance
	if target.Conn != nil {
		handler.SendPlayerKnockback(target.Conn, target.Player.EntityID, x, y, z)
		return
	}
	if s.bedrockListener != nil {
		s.bedrockListener.SendVelocity(target.Player.UUID, spatial.Vec3{X: x, Y: y, Z: z}, uint64(s.worldAge))
	}
}

func (s *Server) applyBedrockMovementDamage(p *player.Player, previousY float64, previousOnGround bool) {
	if p == nil || p.Dead || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator || p.Flying {
		if p != nil {
			p.FallDistance = 0
		}
		return
	}
	target := &session.Session{Player: p}
	if !p.OnGround {
		if drop := previousY - p.Position.Y; drop > 0 {
			p.FallDistance += drop
		}
	} else if !previousOnGround {
		fallDistance := p.FallDistance
		p.FallDistance = 0
		if damage := float32(math.Floor(fallDistance - 3)); damage > 0 {
			handler.DamagePlayer(target, damage, "hit the ground too hard", s.sessions)
		}
	}

	now := time.Now()
	if p.Position.Y < coreworld.WorldMinY-16 {
		if now.Sub(p.LastEnvironmentDamage) >= 500*time.Millisecond {
			p.LastEnvironmentDamage = now
			handler.DamagePlayer(target, 4, "fell out of the world", s.sessions)
		}
		return
	}
	x, y, z := int(math.Floor(p.Position.X)), int(math.Floor(p.Position.Y)), int(math.Floor(p.Position.Z))
	feet := s.world.GetBlock(x, y, z).ResourceLocation()
	head := s.world.GetBlock(x, int(math.Floor(p.Position.Y+1.62)), z).ResourceLocation()
	if head == "minecraft:water" {
		if p.UnderwaterSince.IsZero() {
			p.UnderwaterSince = now
		}
		if now.Sub(p.UnderwaterSince) >= 15*time.Second && now.Sub(p.LastEnvironmentDamage) >= time.Second {
			p.LastEnvironmentDamage = now
			handler.DamagePlayer(target, 2, "drowned", s.sessions)
		}
	} else {
		p.UnderwaterSince = time.Time{}
	}
	if now.Sub(p.LastEnvironmentDamage) < 500*time.Millisecond {
		return
	}
	switch feet {
	case "minecraft:lava":
		p.LastEnvironmentDamage = now
		handler.DamagePlayer(target, 4, "tried to swim in lava", s.sessions)
	case "minecraft:fire", "minecraft:soul_fire":
		p.LastEnvironmentDamage = now
		handler.DamagePlayer(target, 1, "went up in flames", s.sessions)
	case "minecraft:cactus", "minecraft:sweet_berry_bush":
		p.LastEnvironmentDamage = now
		handler.DamagePlayer(target, 1, "was pricked to death", s.sessions)
	}
}

// applyChat broadcasts a chat message to all active Java sessions.
func (s *Server) applyChat(i intent.ChatIntent) {
	if strings.HasPrefix(strings.TrimSpace(i.Message), `/`) {
		p := s.game.GetPlayer(i.PlayerUUID)
		if p == nil {
			return
		}
		ctx := handler.CommandContext{
			Player:  p,
			World:   s.world,
			Manager: s.sessions,
		}
		if s.bedrockListener != nil && p.Edition == player.ClientEditionBedrock {
			ctx.Reply = func(message string) error {
				s.bedrockListener.SendMessage(p.UUID, message)
				return nil
			}
			ctx.SyncAbilities = s.bedrockListener.RefreshPlayerAbilities
			ctx.TeleportTo = func(x, y, z float64) error {
				p.Position = spatial.Vec3{X: x, Y: y, Z: z}
				p.FallDistance = 0
				s.bedrockListener.TeleportPlayer(p, p.Position, uint64(s.worldAge))
				return nil
			}
		}
		s.cmds.Dispatch(i.Message, ctx)
		return
	}
	msg := fmt.Sprintf("<%s> %s", i.DisplayName, i.Message)
	handler.BroadcastSystemMessage(s.sessions, msg)
}

func (s *Server) applyBedrockBlockInteract(i intent.BlockInteractIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock || p.GameMode == player.GameModeSpectator {
		return
	}
	if i.HotbarSlot >= 0 && i.HotbarSlot < 9 {
		p.HeldSlot = int(i.HotbarSlot)
	}
	center := spatial.Vec3{X: float64(i.Position.X) + 0.5, Y: float64(i.Position.Y) + 0.5, Z: float64(i.Position.Z) + 0.5}
	if p.Position.Distance(center) > 6.5 {
		return
	}

	x, y, z := int(i.Position.X), int(i.Position.Y), int(i.Position.Z)
	switch i.Action {
	case intent.BlockActionBreak:
		block := s.world.GetBlock(x, y, z)
		if block.IsAir() || block.ResourceLocation() == "minecraft:bedrock" {
			return
		}
		partnerY, partnerHalf, hasPartner := coreworld.DoublePlantPartnerY(block, y)
		partner := coreworld.Air
		if hasPartner {
			partner = s.world.GetBlock(x, partnerY, z)
			hasPartner = partner.ResourceLocation() == block.ResourceLocation() &&
				partner.Properties["half"] == partnerHalf
		}
		if s.bedrockListener != nil {
			s.bedrockListener.BroadcastBlockBreakEffect(i.Position, block)
		}
		s.world.SetBlock(x, y, z, coreworld.Air)
		change := coreworld.BlockChange{X: x, Y: y, Z: z, Block: coreworld.Air}
		handler.BroadcastBlockChange(change, s.sessions)
		if hasPartner {
			s.world.SetBlock(x, partnerY, z, coreworld.Air)
			handler.BroadcastBlockChange(coreworld.BlockChange{X: x, Y: partnerY, Z: z, Block: coreworld.Air}, s.sessions)
		}
		if p.GameMode != player.GameModeCreative {
			drop := player.ItemStack{ItemID: block.ResourceLocation(), Count: 1}
			if !p.GiveItem(drop) {
				s.newDroppedItem(drop, p.Position, 0)
			}
			s.damageBedrockHeldItem(p, 1)
		}

	case intent.BlockActionUse:
		held := p.HeldItem()
		clicked := s.world.GetBlock(x, y, z)
		if strings.HasSuffix(clicked.ResourceLocation(), "_bed") {
			p.SpawnPoint = spatial.BlockPos{X: int32(x), Y: int32(y), Z: int32(z)}
			p.HasSpawnPoint = true
			if s.bedrockListener != nil {
				s.bedrockListener.SendMessage(p.UUID, "Respawn point set")
			}
			tod := s.worldAge % 24000
			if tod < 12541 || tod > 23459 {
				if s.bedrockListener != nil {
					s.bedrockListener.SendMessage(p.UUID, "You can only sleep at night.")
				}
				return
			}
			p.Sleeping = true
			handler.BroadcastPlayerSleeping(p.EntityID, p.SpawnPoint, s.sessions)
			return
		}
		if boatType, ok := bedrockBoatType(held.ItemID); ok {
			spawn := spatial.Vec3{X: float64(x) + 0.5, Y: float64(y) + 1, Z: float64(z) + 0.5}
			boat := corentity.New(s.game.NextEntityID(), newRandomUUID(), boatType, spawn.X, spawn.Y, spawn.Z)
			boat.Yaw = p.Rotation.Yaw
			boat.OnGround = true
			s.world.Entities.Add(boat)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				if p.Inventory[slot].Count <= 0 {
					p.Inventory[slot] = player.ItemStack{}
				}
			}
			return
		}
		block, ok := placementBlockForItem(held.ItemID)
		if !ok {
			return
		}
		if !s.world.GetBlock(x, y, z).IsAir() {
			dx, dy, dz := bedrockFaceOffset(i.Face)
			x, y, z = x+dx, y+dy, z+dz
		}
		if y < coreworld.WorldMinY || y > coreworld.WorldMaxY || !s.world.GetBlock(x, y, z).IsAir() {
			return
		}
		s.world.SetBlock(x, y, z, block)
		change := coreworld.BlockChange{X: x, Y: y, Z: z, Block: block}
		handler.BroadcastBlockChange(change, s.sessions)
		if p.GameMode != player.GameModeCreative {
			slot := player.HotbarStart + p.HeldSlot
			p.Inventory[slot].Count--
			if p.Inventory[slot].Count <= 0 {
				p.Inventory[slot] = player.ItemStack{}
			}
		}
	}
}

func (s *Server) applyBedrockEntityInteract(i intent.EntityInteractIntent) {
	attacker := s.game.GetPlayer(i.PlayerUUID)
	if attacker == nil {
		return
	}
	if !i.Attack {
		if i.TargetID == 0 && attacker.VehicleEntityID != 0 {
			vehicleID := attacker.VehicleEntityID
			if vehicle, ok := s.world.Entities.Get(vehicleID); ok && vehicle.RiderEntityID == attacker.EntityID {
				vehicle.RiderEntityID = 0
			}
			attacker.VehicleEntityID = 0
			handler.BroadcastSetPassengers(vehicleID, nil, s.sessions)
			return
		}
		if entity, ok := s.world.Entities.Get(i.TargetID); ok && corentity.IsBoat(entity.Type) && entity.RiderEntityID == 0 && attacker.Position.Distance(entity.Position) <= 4 {
			entity.RiderEntityID = attacker.EntityID
			attacker.VehicleEntityID = entity.EntityID
			attacker.Position = entity.Position
			handler.BroadcastSetPassengers(entity.EntityID, []int32{attacker.EntityID}, s.sessions)
		}
		return
	}
	if attacker == nil || attacker.Dead || attacker.GameMode == player.GameModeSpectator {
		return
	}
	damage := player.LegacyAttackDamage(attacker.HeldItem().ItemID)
	if attacker.AttackCooldown {
		if value, _, ok := player.AttackAttributes(attacker.HeldItem().ItemID); ok {
			damage = value
		}
		if !attacker.LastAttack.IsZero() && time.Since(attacker.LastAttack) < 625*time.Millisecond {
			return
		}
	}

	var targetPlayer *player.Player
	s.game.OnlinePlayers(func(candidate *player.Player) {
		if candidate.EntityID == i.TargetID {
			targetPlayer = candidate
		}
	})
	if targetPlayer != nil {
		if targetPlayer == attacker || attacker.Position.Distance(targetPlayer.Position) > 3.25 {
			return
		}
		targetSession, ok := s.sessions.Get(targetPlayer.UUID)
		if !ok {
			targetSession = &session.Session{Player: targetPlayer}
		}
		var damaged bool
		if attacker.AttackCooldown {
			damaged = handler.DamagePlayer(targetSession, damage, "was slain by "+attacker.Username, s.sessions)
		} else {
			damaged = handler.DamagePlayerLegacy(targetSession, damage, "was slain by "+attacker.Username, s.sessions)
		}
		if damaged {
			horizontal, vertical := attacker.KnockbackHorizontal, attacker.KnockbackVertical
			if horizontal <= 0 {
				horizontal = 0.4
			}
			if vertical <= 0 {
				vertical = 0.4
			}
			if attacker.Sprinting {
				horizontal *= 2
			}
			if targetSession.Conn != nil {
				handler.SendLegacyKnockback(targetSession, attacker.Position.X, attacker.Position.Z, horizontal, vertical)
			} else if s.sessions != nil {
				s.sessions.KnockbackExternal(targetPlayer, attacker.Position.X, attacker.Position.Z, horizontal, vertical)
			}
			attacker.LastAttack = time.Now()
			s.damageBedrockHeldItem(attacker, 1)
		}
		return
	}

	if entity, ok := s.world.Entities.Get(i.TargetID); ok && !entity.Dead && attacker.Position.Distance(entity.Position) <= 3.25 {
		if s.world.QueueEntityDamageFrom(entity.EntityID, damage, attacker.Position.X, attacker.Position.Z) {
			attacker.LastAttack = time.Now()
			s.damageBedrockHeldItem(attacker, 1)
		}
	}
}

func (s *Server) applyBedrockRespawn(i intent.RespawnIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock {
		return
	}
	_, _, _, dead := p.HealthSnapshot()
	if !dead {
		return
	}
	if vehicleID := p.VehicleEntityID; vehicleID != 0 {
		if vehicle, ok := s.world.Entities.Get(vehicleID); ok && vehicle.RiderEntityID == p.EntityID {
			vehicle.RiderEntityID = 0
		}
		p.VehicleEntityID = 0
		handler.BroadcastSetPassengers(vehicleID, nil, s.sessions)
	}
	p.Revive()
	if bedSpawn, ok := handler.ResolveBedRespawn(p, s.world); ok {
		p.Position = bedSpawn
	} else {
		p.Position = p.WorldSpawn
	}
}

func (s *Server) applyBedrockInventory(i intent.InventoryIntent) {
	accepted := false
	defer func() {
		if i.Done != nil {
			i.Done <- intent.InventoryResult{Accepted: accepted}
		}
	}()
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock || p.Dead || len(i.Actions) == 0 {
		return
	}
	inventory := p.Inventory
	carried := p.CarriedItem
	drops := make([]player.ItemStack, 0)
	get := func(slot int16) (player.ItemStack, bool) {
		if slot == intent.InventoryCursorSlot {
			return carried, true
		}
		if slot < 0 || int(slot) >= len(inventory) {
			return player.ItemStack{}, false
		}
		return inventory[slot], true
	}
	set := func(slot int16, stack player.ItemStack) bool {
		if stack.Count <= 0 {
			stack = player.ItemStack{}
		}
		if slot == intent.InventoryCursorSlot {
			carried = stack
			return true
		}
		if slot < 0 || int(slot) >= len(inventory) || !canPlaceCanonicalInventorySlot(int(slot), stack) {
			return false
		}
		inventory[slot] = stack
		return true
	}

	for _, action := range i.Actions {
		if action.Kind == intent.InventoryActionCreativeGive {
			// Creative give: spawn an item into the cursor from the creative pool.
			// No source slot — only allowed in creative mode.
			if p.GameMode != player.GameModeCreative || action.Count <= 0 {
				return
			}
			given := player.ItemStack{ItemID: action.Item.ItemID, Count: action.Count, Damage: action.Item.Damage}
			if !set(action.Destination, given) {
				return
			}
			continue
		}

		source, ok := get(action.Source)
		if !ok || source.IsEmpty() {
			return
		}
		switch action.Kind {
		case intent.InventoryActionMove:
			if action.Count <= 0 || action.Count > source.Count || action.Source == action.Destination {
				return
			}
			destination, ok := get(action.Destination)
			if !ok || (!destination.IsEmpty() && (destination.ItemID != source.ItemID || destination.Damage != source.Damage)) {
				return
			}
			newDestination := destination
			if newDestination.IsEmpty() {
				newDestination = player.ItemStack{ItemID: source.ItemID, Damage: source.Damage}
			}
			newDestination.Count += action.Count
			limit := player.MaxStackSize(newDestination.ItemID)
			if action.Destination >= 5 && action.Destination <= 8 {
				limit = 1
			}
			if newDestination.Count > limit || !set(action.Destination, newDestination) {
				return
			}
			source.Count -= action.Count
			if !set(action.Source, source) {
				return
			}

		case intent.InventoryActionSwap:
			destination, ok := get(action.Destination)
			if !ok || action.Source == action.Destination || !set(action.Source, destination) || !set(action.Destination, source) {
				return
			}

		case intent.InventoryActionDrop:
			if action.Count <= 0 || action.Count > source.Count {
				return
			}
			drops = append(drops, player.ItemStack{ItemID: source.ItemID, Count: action.Count, Damage: source.Damage})
			source.Count -= action.Count
			if !set(action.Source, source) {
				return
			}

		case intent.InventoryActionDestroy:
			if p.GameMode != player.GameModeCreative || action.Count <= 0 || action.Count > source.Count {
				return
			}
			source.Count -= action.Count
			if !set(action.Source, source) {
				return
			}

		default:
			return
		}
	}
	p.Inventory = inventory
	p.CarriedItem = carried
	for index, stack := range drops {
		if dropped := s.newDroppedItem(stack, p.Position, index); dropped != nil {
			handler.BroadcastSpawnMob(dropped, s.sessions)
		}
	}
	accepted = true
}

func canPlaceCanonicalInventorySlot(slot int, stack player.ItemStack) bool {
	if stack.IsEmpty() || slot < 5 || slot > 8 {
		return true
	}
	name := strings.TrimPrefix(stack.ItemID, "minecraft:")
	switch slot {
	case 5:
		return strings.HasSuffix(name, "_helmet") || name == "turtle_helmet" || name == "carved_pumpkin" || strings.HasSuffix(name, "_head") || strings.HasSuffix(name, "_skull")
	case 6:
		return strings.HasSuffix(name, "_chestplate") || name == "elytra"
	case 7:
		return strings.HasSuffix(name, "_leggings")
	case 8:
		return strings.HasSuffix(name, "_boots")
	default:
		return false
	}
}

func (s *Server) damageBedrockHeldItem(p *player.Player, amount int) {
	if p == nil || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	if player.MaxDurability(p.Inventory[slot].ItemID) != 0 {
		p.Inventory[slot].ApplyDamage(amount)
	}
}

func placementBlockForItem(itemID string) (coreworld.Block, bool) {
	parts := strings.SplitN(itemID, ":", 2)
	if len(parts) != 2 || parts[0] != "minecraft" {
		return coreworld.Block{}, false
	}
	block := coreworld.Block{Namespace: parts[0], Name: parts[1]}
	if block.IsAir() || javaworld.StateID(block) == 0 {
		return coreworld.Block{}, false
	}
	return block, true
}

func bedrockFaceOffset(face int32) (x, y, z int) {
	switch face {
	case 0:
		return 0, -1, 0
	case 1:
		return 0, 1, 0
	case 2:
		return 0, 0, -1
	case 3:
		return 0, 0, 1
	case 4:
		return -1, 0, 0
	case 5:
		return 1, 0, 0
	default:
		return 0, 0, 0
	}
}

func bedrockBoatType(itemID string) (corentity.EntityType, bool) {
	types := map[string]corentity.EntityType{
		"minecraft:oak_boat":            corentity.TypeOakBoat,
		"minecraft:spruce_boat":         corentity.TypeSpruceBoat,
		"minecraft:birch_boat":          corentity.TypeBirchBoat,
		"minecraft:jungle_boat":         corentity.TypeJungleBoat,
		"minecraft:acacia_boat":         corentity.TypeAcaciaBoat,
		"minecraft:dark_oak_boat":       corentity.TypeDarkOakBoat,
		"minecraft:mangrove_boat":       corentity.TypeMangroveBoat,
		"minecraft:cherry_boat":         corentity.TypeCherryBoat,
		"minecraft:bamboo_raft":         corentity.TypeBambooRaft,
		"minecraft:oak_chest_boat":      corentity.TypeOakChestBoat,
		"minecraft:spruce_chest_boat":   corentity.TypeSpruceChestBoat,
		"minecraft:birch_chest_boat":    corentity.TypeBirchChestBoat,
		"minecraft:jungle_chest_boat":   corentity.TypeJungleChestBoat,
		"minecraft:acacia_chest_boat":   corentity.TypeAcaciaChestBoat,
		"minecraft:dark_oak_chest_boat": corentity.TypeDarkOakChestBoat,
		"minecraft:mangrove_chest_boat": corentity.TypeMangroveChestBoat,
		"minecraft:cherry_chest_boat":   corentity.TypeCherryChestBoat,
		"minecraft:bamboo_chest_raft":   corentity.TypeBambooChestRaft,
	}
	entityType, ok := types[itemID]
	return entityType, ok
}

func (s *Server) syncBedrockPlayersToJava() {
	bedrockPlayers := make(map[[16]byte]*player.Player)
	externalPlayers := make([]*player.Player, 0)
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.Edition == player.ClientEditionBedrock {
			bedrockPlayers[p.UUID] = p
			externalPlayers = append(externalPlayers, p)
		}
	})
	s.sessions.ReplaceExternalPlayers(externalPlayers)

	activeJava := make(map[[16]byte]struct{})
	for _, viewer := range s.sessions.SnapshotAll() {
		activeJava[viewer.Player.UUID] = struct{}{}
		known := s.javaCrossKnown[viewer.Player.UUID]
		if known == nil {
			known = make(map[[16]byte]crossPlayerView)
			s.javaCrossKnown[viewer.Player.UUID] = known
		}
		for id, p := range bedrockPlayers {
			previous, ok := known[id]
			_, _, _, dead := p.HealthSnapshot()
			if !ok || (previous.dead && !dead) {
				if ok {
					handler.SendExternalPlayerLeave(viewer.Conn, p)
				}
				handler.SendExternalPlayerJoin(viewer.Conn, p)
				handler.SendExternalPlayerEquipment(viewer.Conn, p)
			} else if !dead && (previous.position != p.Position || previous.rotation != p.Rotation) {
				handler.SendExternalPlayerPosition(viewer.Conn, p)
			}
			if ok && !dead && !(previous.dead && !dead) && (previous.inventory != p.Inventory || previous.heldSlot != p.HeldSlot) {
				handler.SendExternalPlayerEquipment(viewer.Conn, p)
			}
			known[id] = crossPlayerView{player: p, position: p.Position, rotation: p.Rotation, inventory: p.Inventory, heldSlot: p.HeldSlot, dead: dead}
		}
		for id, previous := range known {
			if _, ok := bedrockPlayers[id]; ok {
				continue
			}
			handler.SendExternalPlayerLeave(viewer.Conn, previous.player)
			delete(known, id)
		}
	}
	for id := range s.javaCrossKnown {
		if _, ok := activeJava[id]; !ok {
			delete(s.javaCrossKnown, id)
		}
	}
}

// tickEntities advances every registered non-player entity by one game tick:
//   - Gravity is applied when the entity is airborne.
//   - Position is integrated from velocity.
//   - Ground collision follows the generated or loaded terrain surface.
//   - Dead entities are removed from the manager this tick.
//   - Packets for position updates and despawns are built synchronously, then
//     handed to a goroutine so slow clients cannot stall the simulation.
//
// Ownership: this method is the sole writer of entity spatial/health fields.
// See the concurrency comment on core/entity.Entity for the full invariant.
func (s *Server) tickEntities() {
	start := time.Now()
	s.worldAge++

	const (
		gravity = -0.08 // blocks/tick² downward acceleration
		drag    = 0.98  // horizontal velocity multiplier per tick
		minVel  = 1e-6  // below this threshold, zero velocity to avoid float noise
	)

	var (
		moved        []*corentity.Entity // entities whose position changed this tick
		hurtEntities []*corentity.Entity // entities damaged during this tick
		deathIDs     []int32             // entities beginning the vanilla death animation
		deadIDs      []int32             // entity IDs removed from the world this tick
		spawned      []*corentity.Entity // item drops spawned by deaths this tick
	)

	endDamage := s.timings.measure(sectionDamage)
	for entityID, event := range s.world.DrainEntityDamage() {
		entity, ok := s.world.Entities.Get(entityID)
		if !ok || entity.Dead {
			continue
		}
		entity.Damage(event.Amount)
		if isPassiveMob(entity.Type) && !entity.Dead {
			s.startPassiveMobPanic(entity, event)
		}
		if (entity.Type == corentity.TypeIronGolem || entity.Type == corentity.TypeSnowGolem) &&
			!entity.Dead && event.HasSource {
			ai := s.mobAIFor(entity)
			ai.hasTarget = true
			ai.targetX = event.SourceX
			ai.targetZ = event.SourceZ
			ai.attackCooldown = 0
		}
		hurtEntities = append(hurtEntities, entity)
		slog.Info("entity damaged", "type", entity.Type, "id", entityID,
			"damage", event.Amount, "health", entity.Health)
	}
	endDamage()

	allEntities := s.world.Entities.Snapshot()

	// ── Parallel passive mob AI ───────────────────────────────────────────────
	// Passive wander AI is pure per-entity computation (position/velocity writes
	// on the entity pointer; read-only world access) so it is safe to run
	// concurrently.  Hostile and golem AI mutate shared player state and stay
	// serial below.
	//
	// We pre-seed every passive mob's AI struct from the tick goroutine first so
	// the map reads inside tickPassiveMobAI are guaranteed to be read-only
	// (safe for concurrent goroutines).
	for _, e := range allEntities {
		if !e.Dead && isPassiveMob(e.Type) {
			_ = s.mobAIFor(e) // ensure AI struct exists before parallel phase
		}
	}
	endPassiveAI := s.timings.measure(sectionAI)
	type villagerWakeEntry struct{ e *corentity.Entity }
	var villagerWakesMu sync.Mutex
	var villagerWakes []villagerWakeEntry
	var passiveAIWG sync.WaitGroup
	for _, e := range allEntities {
		if e.Dead || !isPassiveMob(e.Type) {
			continue
		}
		passiveAIWG.Add(1)
		go func(e *corentity.Entity) {
			defer passiveAIWG.Done()
			if s.tickPassiveMobAI(e) && e.Type == corentity.TypeVillager {
				villagerWakesMu.Lock()
				villagerWakes = append(villagerWakes, villagerWakeEntry{e})
				villagerWakesMu.Unlock()
			}
		}(e)
	}
	passiveAIWG.Wait()
	endPassiveAI()
	for _, vw := range villagerWakes {
		handler.BroadcastVillagerSleepState(vw.e, s.sessions)
	}

	for _, e := range allEntities {
		e.AgeTicks++
		// ── Dead entity cleanup ───────────────────────────────────────────────
		if e.Dead {
			if e.DeathTicks == 0 {
				deathIDs = append(deathIDs, e.EntityID)
				spawned = append(spawned, s.spawnMobDrops(e)...)
				slog.Info("entity died", "type", e.Type, "id", e.EntityID)
			}
			e.DeathTicks++
			if e.DeathTicks >= 20 {
				s.world.Entities.Remove(e.EntityID)
				delete(s.mobAIs, e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
			}
			continue
		}

		if e.Type == corentity.TypeItem && s.tryPickupDroppedItem(e) {
			s.world.Entities.Remove(e.EntityID)
			deadIDs = append(deadIDs, e.EntityID)
			continue
		}

		// ── Primed TNT fuse countdown ────────────────────────────────────────
		if e.Type == corentity.TypePrimedTNT {
			e.FuseTicks--
			if e.FuseTicks <= 0 {
				s.explodeTNT(e.Position.X, e.Position.Y, e.Position.Z)
				s.world.Entities.Remove(e.EntityID)
				delete(s.mobAIs, e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
				e.Dead = true
				continue
			}
			// TNT falls with gravity during fuse.
			if !e.OnGround {
				e.VY += gravity
				e.Position.Y += e.VY
				moved = append(moved, e)
			}
			continue
		}

		// ── Boat physics ─────────────────────────────────────────────────────
		if corentity.IsBoat(e.Type) {
			s.tickBoatPhysics(e)
			// If a rider is controlling the boat, the client sends move_vehicle
			// and we skip server-side movement — just track for broadcast.
			if e.RiderEntityID != 0 {
				// Position is authoritative from client; still broadcast to others.
				// (broadcastBoatPositionExcept is called inside HandleMoveVehiclePacket)
				continue
			}
			if e.VX != 0 || e.VY != 0 || e.VZ != 0 {
				moved = append(moved, e)
			}
			continue
		}

		// ── FallingBlock landing ──────────────────────────────────────────────
		if e.Type == corentity.TypeFallingBlock && e.OnGround {
			// Place the block at the landing position and despawn the entity.
			lx := int(math.Round(e.Position.X - 0.5))
			ly := int(e.Position.Y)
			lz := int(math.Round(e.Position.Z - 0.5))
			landBlock := coreworld.Block{
				Namespace:  "minecraft",
				Name:       strings.TrimPrefix(e.FallingBlockName, "minecraft:"),
				Properties: map[string]string{},
			}
			s.world.SetBlock(lx, ly, lz, landBlock)
			deadIDs = append(deadIDs, e.EntityID)
			e.Dead = true
			handler.BroadcastBlockChange(coreworld.BlockChange{X: lx, Y: ly, Z: lz, Block: landBlock}, s.sessions)
			s.world.Entities.Remove(e.EntityID)
			delete(s.mobAIs, e.EntityID)
			continue
		}

		if corentity.IsProjectile(e.Type) {
			prevX, prevY, prevZ := e.Position.X, e.Position.Y, e.Position.Z
			if s.tickProjectile(e) {
				s.world.Entities.Remove(e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
				continue
			}
			if e.Position.X != prevX || e.Position.Y != prevY || e.Position.Z != prevZ {
				moved = append(moved, e)
			}
			continue
		}

		// ── Mob AI (golem / hostile) ──────────────────────────────────────────
		// Passive mob AI already ran in the parallel pre-pass above.
		prevX, prevY, prevZ := e.Position.X, e.Position.Y, e.Position.Z
		if !isPassiveMob(e.Type) {
			endAI := s.timings.measure(sectionAI)
			if e.Type == corentity.TypeIronGolem || e.Type == corentity.TypeSnowGolem {
				s.tickGolemAI(e)
			} else if isHostileMob(e.Type) {
				s.tickHostileMobAI(e)
			}
			endAI()
		}
		if e.Dead {
			// Damage during AI starts its animation on the next simulation tick.
			continue
		}
		if e.Sleeping {
			e.VX, e.VY, e.VZ = 0, 0, 0
			e.OnGround = true
			if e.Position.X != prevX || e.Position.Y != prevY || e.Position.Z != prevZ {
				moved = append(moved, e)
			}
			continue
		}

		// ── Gravity + physics ─────────────────────────────────────────────────
		endPhys := s.timings.measure(sectionPhysics)
		// ── Gravity ───────────────────────────────────────────────────────────
		if !e.OnGround {
			e.VY += gravity
		}

		// ── Position integration with step-up ────────────────────────────────
		nextX := e.Position.X + e.VX
		if canX, xLoaded := s.world.CanEntityOccupyIfLoaded(nextX, e.Position.Y, e.Position.Z); xLoaded && canX {
			e.Position.X = nextX
		} else if xLoaded && e.OnGround && e.VX != 0 {
			if canXUp, xUpLoaded := s.world.CanEntityOccupyIfLoaded(nextX, e.Position.Y+1, e.Position.Z); xUpLoaded && canXUp {
				// Step up over a 1-block obstacle.
				e.Position.X = nextX
				e.Position.Y = math.Floor(e.Position.Y) + 1
				e.VY = 0
			} else if xUpLoaded {
				e.VX = 0
			}
			// If chunk not loaded, preserve velocity and let entity try next tick.
		} else if xLoaded {
			e.VX = 0
		}
		e.Position.Y += e.VY
		nextZ := e.Position.Z + e.VZ
		if canZ, zLoaded := s.world.CanEntityOccupyIfLoaded(e.Position.X, e.Position.Y, nextZ); zLoaded && canZ {
			e.Position.Z = nextZ
		} else if zLoaded && e.OnGround && e.VZ != 0 {
			if canZUp, zUpLoaded := s.world.CanEntityOccupyIfLoaded(e.Position.X, e.Position.Y+1, nextZ); zUpLoaded && canZUp {
				// Step up over a 1-block obstacle.
				e.Position.Z = nextZ
				e.Position.Y = math.Floor(e.Position.Y) + 1
				e.VY = 0
			} else if zUpLoaded {
				e.VZ = 0
			}
			// If chunk not loaded, preserve velocity and let entity try next tick.
		} else if zLoaded {
			e.VZ = 0
		}

		// ── Ground detection (generated or loaded terrain) ───────────────────────
		// Only run the ground check when the chunk under the entity is cached;
		// skip the expensive GroundYAtOrBelow call that would trigger disk I/O.
		ex, ez := int(math.Floor(e.Position.X)), int(math.Floor(e.Position.Z))
		ecx, ecz := coreworld.ChunkCoordsFor(ex, ez)
		var groundY float64
		if s.world.IsChunkLoaded(ecx, ecz) {
			groundY = float64(s.world.GroundYAtOrBelow(ex, ez, int(math.Floor(math.Max(prevY, e.Position.Y)))) + 1)
		} else {
			groundY = prevY // freeze Y until chunk loads to prevent falling through unloaded terrain
		}
		if e.Position.Y <= groundY {
			e.Position.Y = groundY
			e.VY = 0
			e.OnGround = true
		} else {
			e.OnGround = false
		}

		// ── Horizontal drag ───────────────────────────────────────────────────
		e.VX *= drag
		e.VZ *= drag
		if math.Abs(e.VX) < minVel {
			e.VX = 0
		}
		if math.Abs(e.VZ) < minVel {
			e.VZ = 0
		}

		// ── Collect moved entities for broadcast ──────────────────────────────
		if e.Position.X != prevX || e.Position.Y != prevY || e.Position.Z != prevZ {
			moved = append(moved, e)
		}
		endPhys()
	}

	s.tickClearLag(&deadIDs)

	// Build packets and dispatch network I/O off the tick goroutine.
	// DispatchTickBroadcast reads entity fields here (tick goroutine, sole
	// writer) to build immutable packets before spawning the send goroutine.
	endBcast := s.timings.measure(sectionBroadcast)
	handler.DispatchTickBroadcast(moved, hurtEntities, deathIDs, deadIDs, spawned, s.sessions)
	endBcast()

	// Publish time-of-day for handler code (e.g. bed sleep check).
	endTime := s.timings.measure(sectionTime)
	s.world.SetWorldTime(s.worldAge % 24000)
	// Drain a player-requested time skip (sleeping in a bed at night).
	if s.world.DrainTimeSkip() {
		tod := s.worldAge % 24000
		if tod < 6000 {
			s.worldAge += 6000 - tod
		} else {
			s.worldAge += 24000 - tod + 6000
		}
		handler.DispatchWorldTime(s.worldAge, s.worldAge%24000, s.sessions)
	}
	if s.worldAge%20 == 0 {
		handler.DispatchWorldTime(s.worldAge, s.worldAge%24000, s.sessions)
		for _, change := range s.world.TickFarmland(s.worldAge, 64) {
			handler.BroadcastBlockChange(change, s.sessions)
		}
		for _, change := range s.world.TickCrops(s.worldAge, 64) {
			handler.BroadcastBlockChange(change, s.sessions)
		}
	}
	endTime()

	if s.worldAge%80 == 0 {
		endSpawnP := s.timings.measure(sectionSpawnPassive)
		s.spawnPassiveMobsNearPlayers()
		endSpawnP()
	}
	if s.worldAge%100 == 0 && s.cfg.Difficulty != "peaceful" {
		endSpawnH := s.timings.measure(sectionSpawnHostile)
		s.spawnHostileMobsNearPlayers()
		endSpawnH()
	}
	// Sleeping: if all online players are sleeping, skip night.
	s.tickSleep()
	// Villager baby grow-up: every tick, age all babies; grow up after ~5 min.
	s.tickVillagerAging()
	// Villager breeding: every 2 minutes, villages with adults + free beds breed.
	if s.worldAge%2400 == 0 && s.worldAge > 0 {
		s.tickVillagerBreeding()
	}
	// Block physics: falling blocks + fluid spreading.
	endBP := s.timings.measure(sectionBlockPhysics)
	s.tickBlockPhysics()
	endBP()
	// Every 5 minutes: force a GC cycle and return freed pages to the OS.
	// Go's runtime retains freed heap pages by default; this keeps RSS in check.
	if s.worldAge%6000 == 0 && s.worldAge > 0 {
		go func() {
			runtime.GC()
			debug.FreeOSMemory()
		}()
	}

	// Record this tick into the rolling timing window.
	elapsed := time.Since(start)
	s.timings.commit(elapsed)

	// Warn when the CPU work in a tick exceeds the tick budget.
	// Network I/O is off-goroutine and does not count toward this budget.
	if elapsed > 50*time.Millisecond {
		tps, avgMs := s.timings.TPS()
		slog.Warn("entity tick overrun",
			"elapsed", elapsed.Round(time.Millisecond),
			"tps", fmt.Sprintf("%.1f", tps),
			"avg_ms", fmt.Sprintf("%.2f", avgMs),
			"entities", len(s.world.Entities.Snapshot()),
		)
	}
}

// spawnMobDrops creates the basic vanilla loot for supported living entities.
// Looting, fire-aspect cooking, equipment drops, and rare player-kill-only
// drops are intentionally left for the enchantment/loot-table layer.
func (s *Server) spawnMobDrops(e *corentity.Entity) []*corentity.Entity {
	stacks := mobDrops(e.Type, s.spawnRNG)
	spawned := make([]*corentity.Entity, 0, len(stacks))
	for index, stack := range stacks {
		if dropped := s.newDroppedItem(stack, e.Position, index); dropped != nil {
			spawned = append(spawned, dropped)
		}
	}
	return spawned
}

func mobDrops(entityType corentity.EntityType, rng *rand.Rand) []player.ItemStack {
	between := func(minimum, maximum int) int {
		if maximum <= minimum {
			return minimum
		}
		return minimum + rng.Intn(maximum-minimum+1)
	}
	drops := make([]player.ItemStack, 0, 3)
	add := func(itemID string, count int) {
		if count > 0 {
			drops = append(drops, player.ItemStack{ItemID: itemID, Count: count})
		}
	}

	switch entityType {
	case corentity.TypeCow, corentity.TypeMooshroom:
		add("minecraft:leather", between(0, 2))
		add("minecraft:beef", between(1, 3))
	case corentity.TypePig:
		add("minecraft:porkchop", between(1, 3))
	case corentity.TypeSheep:
		add("minecraft:white_wool", 1)
		add("minecraft:mutton", between(1, 2))
	case corentity.TypeChicken:
		add("minecraft:feather", between(0, 2))
		add("minecraft:chicken", 1)
	case corentity.TypeRabbit:
		add("minecraft:rabbit_hide", between(0, 1))
		add("minecraft:rabbit", 1)
		if between(1, 10) == 1 {
			add("minecraft:rabbit_foot", 1)
		}
	case corentity.TypeHorse, corentity.TypeDonkey, corentity.TypeMule,
		corentity.TypeLlama, corentity.TypeTraderLlama, corentity.TypeCamel:
		add("minecraft:leather", between(0, 2))
	case corentity.TypeCod:
		add("minecraft:cod", 1)
	case corentity.TypeSalmon:
		add("minecraft:salmon", 1)
	case corentity.TypeTropicalFish:
		add("minecraft:tropical_fish", 1)
	case corentity.TypePufferfish:
		add("minecraft:pufferfish", 1)
	case corentity.TypeSquid:
		add("minecraft:ink_sac", between(1, 3))
	case corentity.TypeGlowSquid:
		add("minecraft:glow_ink_sac", between(1, 3))
	case corentity.TypeTurtle:
		add("minecraft:seagrass", between(0, 2))
	case corentity.TypeHoglin:
		add("minecraft:porkchop", between(2, 4))
		add("minecraft:leather", between(0, 1))
	case corentity.TypeStrider:
		add("minecraft:string", between(2, 5))
	case corentity.TypeZombie, corentity.TypeHusk, corentity.TypeDrowned,
		corentity.TypeZombieVillager, corentity.TypeZombifiedPiglin:
		add("minecraft:rotten_flesh", between(0, 2))
	case corentity.TypeSkeleton, corentity.TypeStray, corentity.TypeBogged,
		corentity.TypeWitherSkeleton:
		add("minecraft:bone", between(0, 2))
		add("minecraft:arrow", between(0, 2))
	case corentity.TypeCreeper:
		add("minecraft:gunpowder", between(0, 2))
	case corentity.TypeSpider, corentity.TypeCaveSpider:
		add("minecraft:string", between(0, 2))
	case corentity.TypeEnderman:
		add("minecraft:ender_pearl", between(0, 1))
	case corentity.TypeBlaze:
		add("minecraft:blaze_rod", between(0, 1))
	case corentity.TypeIronGolem:
		add("minecraft:iron_ingot", between(3, 5))
		add("minecraft:poppy", between(0, 2))
	case corentity.TypeSnowGolem:
		add("minecraft:snowball", between(0, 15))
	}
	return drops
}

func (s *Server) newDroppedItem(stack player.ItemStack, position spatial.Vec3, ordinal int) *corentity.Entity {
	if stack.IsEmpty() {
		return nil
	}
	id := s.game.NextEntityID()
	dropped := corentity.New(id, newRandomUUID(), corentity.TypeItem,
		position.X, position.Y+0.25, position.Z)
	dropped.ItemID = stack.ItemID
	dropped.ItemCount = stack.Count
	dropped.ItemDamage = stack.Damage
	angle := float64(id+int32(ordinal)*17) * 2.399963229728653
	dropped.VX = math.Cos(angle) * 0.1
	dropped.VY = 0.2
	dropped.VZ = math.Sin(angle) * 0.1
	s.world.Entities.Add(dropped)
	return dropped
}

// dropPlayerInventory applies vanilla's default keepInventory=false behaviour
// for survival/adventure deaths. Creative and spectator inventories are not
// turned into world drops.
func (s *Server) dropPlayerInventory(p *player.Player) {
	if p == nil || (p.GameMode != player.GameModeSurvival && p.GameMode != player.GameModeAdventure) {
		return
	}
	stacks := make([]player.ItemStack, 0, player.InventorySize+len(p.CraftingGrid)+1)
	for slot := 1; slot < player.InventorySize; slot++ {
		if !p.Inventory[slot].IsEmpty() {
			stacks = append(stacks, p.Inventory[slot])
		}
		p.Inventory[slot] = player.ItemStack{}
	}
	// Slot zero is only a derived crafting result and must never duplicate the
	// ingredients that created it.
	p.Inventory[0] = player.ItemStack{}
	if !p.CarriedItem.IsEmpty() {
		stacks = append(stacks, p.CarriedItem)
	}
	p.CarriedItem = player.ItemStack{}
	for index := range p.CraftingGrid {
		if !p.CraftingGrid[index].IsEmpty() {
			stacks = append(stacks, p.CraftingGrid[index])
		}
		p.CraftingGrid[index] = player.ItemStack{}
	}
	p.CraftingResult = player.ItemStack{}
	p.ContainerSlots = nil
	p.OpenContainerKind = ""

	for index, stack := range stacks {
		if dropped := s.newDroppedItem(stack, p.Position, index); dropped != nil {
			handler.BroadcastSpawnMob(dropped, s.sessions)
		}
	}
	if sess, ok := s.sessions.Get(p.UUID); ok {
		_ = handler.SyncPlayerInventory(sess.Conn, p)
	}
}

func (s *Server) tryPickupDroppedItem(e *corentity.Entity) bool {
	if e.AgeTicks < 10 || e.ItemID == "" || e.ItemCount <= 0 {
		return false
	}
	for _, sess := range s.allPlayerSessions() {
		p := sess.Player
		if p == nil || p.Dead || p.GameMode == player.GameModeSpectator {
			continue
		}
		dx := p.Position.X - e.Position.X
		dy := p.Position.Y + 0.5 - e.Position.Y
		dz := p.Position.Z - e.Position.Z
		if dx*dx+dy*dy+dz*dz > 2.25 {
			continue
		}
		stack := player.ItemStack{ItemID: e.ItemID, Count: e.ItemCount, Damage: e.ItemDamage}
		if !p.GiveItem(stack) {
			continue
		}
		handler.BroadcastCollectItem(e.EntityID, p.EntityID, e.ItemCount, s.sessions)
		if sess.Conn != nil {
			_ = handler.SyncPlayerInventory(sess.Conn, p)
		}
		return true
	}
	return false
}

func (s *Server) tickClearLag(deadIDs *[]int32) {
	clearLag := s.cfg.ClearLag
	if !clearLag.Enabled || s.worldAge <= 0 || s.worldAge%20 != 0 {
		return
	}
	intervalTicks := int64(clearLag.IntervalSeconds * 20)
	remainingTicks := intervalTicks - s.worldAge%intervalTicks
	if remainingTicks == intervalTicks {
		remainingTicks = 0
	}
	remainingSeconds := int(remainingTicks / 20)
	if remainingSeconds > 0 {
		for _, warning := range clearLag.WarningSeconds {
			if warning == remainingSeconds {
				message := strings.ReplaceAll(clearLag.WarningMessage, "{seconds}", strconv.Itoa(remainingSeconds))
				handler.BroadcastSystemMessage(s.sessions, message)
				break
			}
		}
		return
	}

	minimumAge := int64(clearLag.MinimumEntityAgeSeconds * 20)
	removed := 0
	for _, e := range s.world.Entities.Snapshot() {
		if e.AgeTicks < minimumAge || !clearLagRemoves(e.Type, clearLag.Remove) {
			continue
		}
		s.world.Entities.Remove(e.EntityID)
		delete(s.mobAIs, e.EntityID)
		*deadIDs = append(*deadIDs, e.EntityID)
		removed++
	}
	message := strings.ReplaceAll(clearLag.CompleteMessage, "{count}", strconv.Itoa(removed))
	handler.BroadcastSystemMessage(s.sessions, message)
}

func clearLagRemoves(t corentity.EntityType, targets config.ClearLagTargets) bool {
	switch {
	case t == corentity.TypeItem:
		return targets.DroppedItems
	case t == corentity.TypeExperienceOrb:
		return targets.ExperienceOrbs
	case corentity.IsProjectile(t):
		return targets.Projectiles
	case t == corentity.TypePrimedTNT:
		return targets.PrimedTNT
	case t == corentity.TypeFallingBlock:
		return targets.FallingBlocks
	case corentity.IsBoat(t):
		return targets.Boats
	case isPassiveMob(t):
		return targets.PassiveMobs
	case isHostileMob(t):
		return targets.HostileMobs
	}
	return false
}

// spawnTestMobs populates the world near the spawn point with a handful of
// passive mobs so the entity system can be verified with a vanilla client.
// This is removed or made config-driven in a later milestone.
func (s *Server) spawnTestMobs() {
	type spawn struct {
		t    corentity.EntityType
		x, z float64
	}
	mobs := []spawn{
		{corentity.TypeCow, float64(s.spawnX + 6), float64(s.spawnZ)},
		{corentity.TypeCow, float64(s.spawnX - 6), float64(s.spawnZ + 4)},
		{corentity.TypePig, float64(s.spawnX), float64(s.spawnZ + 7)},
		{corentity.TypeSheep, float64(s.spawnX + 4), float64(s.spawnZ - 5)},
		{corentity.TypeChicken, float64(s.spawnX - 4), float64(s.spawnZ - 6)},
	}
	for _, m := range mobs {
		id := s.game.NextEntityID()
		uuid := newRandomUUID()
		y := float64(s.world.SurfaceY(int(math.Floor(m.x)), int(math.Floor(m.z))) + 1)
		e := corentity.New(id, uuid, m.t, m.x, y, m.z)
		e.OnGround = true // spawned on the surface; skip first-tick gravity drop
		s.world.Entities.Add(e)
		slog.Info("spawned entity", "type", m.t, "id", id,
			"x", m.x, "y", y, "z", m.z)
	}
}

// spawnPassiveMobsNearPlayers provides a natural-spawn loop for passive mobs.
// Cap scales with player count (10 per player, min 15) like vanilla.
func (s *Server) spawnPassiveMobsNearPlayers() {
	playerCount := 0
	s.game.OnlinePlayers(func(_ *player.Player) { playerCount++ })
	if playerCount == 0 {
		return
	}
	cap := 10 * playerCount
	if cap < 15 {
		cap = 15
	}

	animals := 0
	for _, e := range s.world.Entities.Snapshot() {
		if e.Type != corentity.TypeVillager && isPassiveMob(e.Type) {
			animals++
		}
	}
	if animals >= cap {
		return
	}

	passiveTypes := []corentity.EntityType{
		corentity.TypeCow, corentity.TypeCow,
		corentity.TypePig, corentity.TypeSheep, corentity.TypeSheep,
		corentity.TypeChicken, corentity.TypeChicken,
		corentity.TypeRabbit, corentity.TypeFox,
	}

	s.game.OnlinePlayers(func(p *player.Player) {
		if animals >= cap {
			return
		}
		// Spawn a group of 2-4 animals near the player.
		groupSize := 2 + s.spawnRNG.Intn(3)
		mobType := passiveTypes[s.spawnRNG.Intn(len(passiveTypes))]
		angle := s.spawnRNG.Float64() * 2 * math.Pi
		distance := 24 + s.spawnRNG.Float64()*40
		cx := p.Position.X + math.Cos(angle)*distance
		cz := p.Position.Z + math.Sin(angle)*distance
		for i := 0; i < groupSize && animals < cap; i++ {
			ox := cx + (s.spawnRNG.Float64()-0.5)*6
			oz := cz + (s.spawnRNG.Float64()-0.5)*6
			sy, syLoaded := s.world.SurfaceYIfLoaded(int(math.Floor(ox)), int(math.Floor(oz)))
			if !syLoaded {
				continue // don't trigger disk I/O on the tick goroutine
			}
			y := float64(sy + 1)
			if canOccupy, occLoaded := s.world.CanEntityOccupyIfLoaded(ox, y, oz); !occLoaded || !canOccupy {
				continue
			}
			e := corentity.New(s.game.NextEntityID(), newRandomUUID(), mobType, ox, y, oz)
			e.OnGround = true
			s.world.Entities.Add(e)
			handler.BroadcastSpawnMob(e, s.sessions)
			animals++
			slog.Debug("passive mob spawned", "type", e.Type, "id", e.EntityID, "x", ox, "y", y, "z", oz)
		}
	})
}

// spawnHostileMobsNearPlayers spawns hostile mobs at night or underground.
// Skipped entirely on peaceful difficulty.
func (s *Server) spawnHostileMobsNearPlayers() {
	playerCount := 0
	s.game.OnlinePlayers(func(_ *player.Player) { playerCount++ })
	if playerCount == 0 {
		return
	}

	// Hostile cap: 70 per player in vanilla; we use 15 per player (scaled).
	cap := 15 * playerCount
	if cap < 20 {
		cap = 20
	}
	hostiles := 0
	for _, e := range s.world.Entities.Snapshot() {
		if isHostileMob(e.Type) {
			hostiles++
		}
	}
	if hostiles >= cap {
		return
	}

	// Vanilla spawns surface hostiles only at night (time 13000-23000).
	// We allow underground spawning regardless of time.
	timeOfDay := s.worldAge % 24000
	isNight := timeOfDay >= 13000 && timeOfDay <= 23000

	// Difficulty-based extra spawn weight.
	extraChance := 0.0
	switch s.cfg.Difficulty {
	case "easy":
		extraChance = 0.5
	case "normal":
		extraChance = 1.0
	case "hard":
		extraChance = 1.5
	}

	surfaceHostiles := []corentity.EntityType{
		corentity.TypeZombie, corentity.TypeZombie,
		corentity.TypeSkeleton, corentity.TypeSkeleton,
		corentity.TypeCreeper,
		corentity.TypeSpider,
		corentity.TypeEnderman,
		corentity.TypeWitch,
	}

	s.game.OnlinePlayers(func(p *player.Player) {
		if hostiles >= cap {
			return
		}
		if !isNight && s.spawnRNG.Float64() > extraChance*0.3 {
			// During the day only a small fraction spawn (caves/shade).
			return
		}
		// Spawn 1-3 hostiles per player per call.
		groupSize := 1 + s.spawnRNG.Intn(3)
		mobType := surfaceHostiles[s.spawnRNG.Intn(len(surfaceHostiles))]
		angle := s.spawnRNG.Float64() * 2 * math.Pi
		distance := 24 + s.spawnRNG.Float64()*56
		cx := p.Position.X + math.Cos(angle)*distance
		cz := p.Position.Z + math.Sin(angle)*distance
		for i := 0; i < groupSize && hostiles < cap; i++ {
			ox := cx + (s.spawnRNG.Float64()-0.5)*8
			oz := cz + (s.spawnRNG.Float64()-0.5)*8
			sy, syLoaded := s.world.SurfaceYIfLoaded(int(math.Floor(ox)), int(math.Floor(oz)))
			if !syLoaded {
				continue // don't trigger disk I/O on the tick goroutine
			}
			y := float64(sy + 1)
			if canOccupy, occLoaded := s.world.CanEntityOccupyIfLoaded(ox, y, oz); !occLoaded || !canOccupy {
				continue
			}
			e := corentity.New(s.game.NextEntityID(), newRandomUUID(), mobType, ox, y, oz)
			e.OnGround = true
			s.world.Entities.Add(e)
			handler.BroadcastSpawnMob(e, s.sessions)
			hostiles++
			slog.Debug("hostile mob spawned", "type", e.Type, "id", e.EntityID, "x", ox, "y", y, "z", oz)
		}
	})
}

// isPassiveMob reports whether the given entity type uses passive-mob wander AI.
func isPassiveMob(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeVillager, corentity.TypeWanderingTrader,
		// Farm animals
		corentity.TypeCow, corentity.TypeMooshroom, corentity.TypePig,
		corentity.TypeSheep, corentity.TypeChicken,
		// Pets / tameable
		corentity.TypeWolf, corentity.TypeCat, corentity.TypeOcelot, corentity.TypeParrot,
		// Rideable
		corentity.TypeHorse, corentity.TypeDonkey, corentity.TypeMule,
		corentity.TypeCamel, corentity.TypeStrider,
		corentity.TypeSkeletonHorse, corentity.TypeZombieHorse,
		// Llamas
		corentity.TypeLlama, corentity.TypeTraderLlama,
		// Misc passive
		corentity.TypeGoat, corentity.TypePanda, corentity.TypeFox,
		corentity.TypeRabbit, corentity.TypeSniffer, corentity.TypeAxolotl,
		corentity.TypeArmadillo, corentity.TypeAllay,
		corentity.TypeTurtle, corentity.TypeTadpole,
		corentity.TypeSquid, corentity.TypeGlowSquid,
		corentity.TypeCod, corentity.TypeSalmon, corentity.TypeTropicalFish,
		corentity.TypePufferfish,
		corentity.TypeBat, corentity.TypeFrog, corentity.TypeBee,
		corentity.TypeDolphin, corentity.TypePolarBear:
		return true
	}
	return false
}

func isHostileMob(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeBlaze, corentity.TypeBogged, corentity.TypeBreeze,
		corentity.TypeCaveSpider, corentity.TypeCreaker, corentity.TypeCreeper,
		corentity.TypeDrowned, corentity.TypeElderGuardian, corentity.TypeEnderman,
		corentity.TypeEndermite, corentity.TypeEvoker, corentity.TypeGhast,
		corentity.TypeGuardian, corentity.TypeHoglin, corentity.TypeHusk,
		corentity.TypeIllusioner, corentity.TypeMagmaCube, corentity.TypePhantom,
		corentity.TypePiglin, corentity.TypePiglinBrute, corentity.TypePillager,
		corentity.TypeRavager, corentity.TypeShulker, corentity.TypeSilverfish,
		corentity.TypeSkeleton, corentity.TypeSlime, corentity.TypeSpider,
		corentity.TypeStray, corentity.TypeVex, corentity.TypeVindicator,
		corentity.TypeWarden, corentity.TypeWitch, corentity.TypeWither,
		corentity.TypeWitherSkeleton, corentity.TypeZoglin, corentity.TypeZombie,
		corentity.TypeZombieVillager:
		return true
	}
	return false
}

func (s *Server) mobAIFor(e *corentity.Entity) *mobAI {
	ai, ok := s.mobAIs[e.EntityID]
	if ok {
		return ai
	}
	roaming := !e.HasVillageHome
	homeX, homeZ := e.Position.X, e.Position.Z
	if e.HasVillageHome {
		homeX = float64(e.VillageCenter.X) + 0.5
		homeZ = float64(e.VillageCenter.Z) + 0.5
	}
	ai = &mobAI{
		homeX:   homeX,
		homeZ:   homeZ,
		roaming: roaming,
		rng:     rand.New(rand.NewSource(int64(e.EntityID) * 6364136223846793005)),
	}
	s.mobAIs[e.EntityID] = ai
	return ai
}

// startPassiveMobPanic makes a recently hurt passive mob jump and sprint away
// from its attacker. It is called only by the entity tick goroutine.
func (s *Server) startPassiveMobPanic(e *corentity.Entity, hit coreworld.EntityDamage) {
	ai := s.mobAIFor(e)
	dx, dz := e.Position.X-hit.SourceX, e.Position.Z-hit.SourceZ
	distance := math.Hypot(dx, dz)
	if !hit.HasSource || distance < 0.01 {
		angle := ai.rng.Float64() * 2 * math.Pi
		dx, dz, distance = math.Cos(angle), math.Sin(angle), 1
	}
	ai.dirX, ai.dirZ = dx/distance, dz/distance
	ai.panicTick = 60
	ai.knockbackTick = 8
	ai.pauseTick = 0
	ai.wanderTick = 60
	horizontal, vertical := 0.4, 0.4
	if s.cfg != nil {
		horizontal = s.cfg.Combat.KnockbackHorizontal
		vertical = s.cfg.Combat.KnockbackVertical
	}
	e.VX = ai.dirX * horizontal
	e.VZ = ai.dirZ * horizontal
	if e.OnGround {
		e.VY = vertical
	}
	e.Sleeping = false
}

// tickPassiveMobAI advances wander AI for a single passive mob.
// Returns true if the entity's Sleeping state changed this tick (so the caller
// can broadcast a pose metadata update).
//
// Villagers are homed: they stay within 8 blocks of their spawn point.
// All other passive mobs roam freely, occasionally pausing.
func (s *Server) tickPassiveMobAI(e *corentity.Entity) bool {
	ai := s.mobAIFor(e)
	wasAsleep := ai.sleepingWas
	if wasAsleep && !e.Sleeping && e.Type == corentity.TypeVillager {
		s.wakeVillagerBesideBed(e)
	}

	if ai.knockbackTick > 0 {
		ai.knockbackTick--
		e.Yaw = float32(math.Atan2(-ai.dirX, ai.dirZ) * 180 / math.Pi)
		ai.sleepingWas = e.Sleeping
		return wasAsleep != e.Sleeping
	}

	if ai.panicTick > 0 {
		ai.panicTick--
		e.VX, e.VZ = ai.dirX*0.28, ai.dirZ*0.28
		e.Yaw = float32(math.Atan2(-ai.dirX, ai.dirZ) * 180 / math.Pi)
		ai.sleepingWas = e.Sleeping
		return wasAsleep != e.Sleeping
	}

	// Assigned villagers return to their own bed at night and stay there until
	// morning. The canonical Sleeping flag is adapter-independent state.
	if e.Type == corentity.TypeVillager && e.HasVillageHome {
		dayTime := s.worldAge % 24000
		if dayTime >= 12542 && dayTime < 23460 {
			targetX := float64(e.VillageBed.X) + 0.5
			targetZ := float64(e.VillageBed.Z) + 0.5
			dx, dz := targetX-e.Position.X, targetZ-e.Position.Z
			distance := math.Hypot(dx, dz)
			if distance <= 0.6 {
				e.Position.X = targetX
				e.Position.Y = float64(e.VillageBed.Y) + 0.6875
				e.Position.Z = targetZ
				e.VX, e.VY, e.VZ = 0, 0, 0
				e.OnGround = true
				e.Sleeping = true
				changed := !wasAsleep
				ai.sleepingWas = true
				return changed
			}
			e.Sleeping = false
			e.VX, e.VZ = dx/distance*0.1, dz/distance*0.1
			e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
			changed := wasAsleep
			ai.sleepingWas = false
			return changed
		}
		e.Sleeping = false
		if wasAsleep {
			s.wakeVillagerBesideBed(e)
			ai.sleepingWas = false
			return true
		}
	}

	// While paused, hold still.
	if ai.pauseTick > 0 {
		ai.pauseTick--
		e.VX, e.VZ = 0, 0
		ai.sleepingWas = e.Sleeping
		return wasAsleep != e.Sleeping
	}

	ai.wanderTick--

	if ai.wanderTick <= 0 {
		if ai.roaming {
			// Roaming mobs: 25 % chance to pause instead of moving.
			if ai.rng.Intn(4) == 0 {
				ai.pauseTick = 40 + ai.rng.Intn(60) // 2–5 s pause
				e.VX, e.VZ = 0, 0
				ai.sleepingWas = e.Sleeping
				return wasAsleep != e.Sleeping
			}
			angle := ai.rng.Float64() * 2 * math.Pi
			ai.dirX = math.Cos(angle)
			ai.dirZ = math.Sin(angle)
		} else {
			// Homed (villager): walk back toward home if too far, else random.
			dx := e.Position.X - ai.homeX
			dz := e.Position.Z - ai.homeZ
			if dx*dx+dz*dz > 676 { // beyond 26 blocks: return toward village center
				d := math.Sqrt(dx*dx + dz*dz)
				ai.dirX = -dx / d
				ai.dirZ = -dz / d
			} else {
				angle := ai.rng.Float64() * 2 * math.Pi
				ai.dirX = math.Cos(angle)
				ai.dirZ = math.Sin(angle)
			}
		}
		ai.wanderTick = 60 + ai.rng.Intn(60) // 3–6 s before next direction change
	}

	// Walk speed: chickens are a bit slower; horses a bit faster.
	speed := 0.1
	switch e.Type {
	case corentity.TypeChicken, corentity.TypeRabbit:
		speed = 0.07
	case corentity.TypeHorse, corentity.TypeDonkey, corentity.TypeMule:
		speed = 0.18
	case corentity.TypeSniffer:
		speed = 0.06
	}

	e.VX = ai.dirX * speed
	e.VZ = ai.dirZ * speed

	if ai.dirX != 0 || ai.dirZ != 0 {
		yawRad := math.Atan2(-ai.dirX, ai.dirZ)
		e.Yaw = float32(yawRad * 180 / math.Pi)
	}
	ai.sleepingWas = e.Sleeping
	return wasAsleep != e.Sleeping
}

func (s *Server) wakeVillagerBesideBed(e *corentity.Entity) {
	headX, headY, headZ := int(e.VillageBed.X), int(e.VillageBed.Y), int(e.VillageBed.Z)
	dx, dz := 0, 1
	switch s.world.GetBlock(headX, headY, headZ).Properties["facing"] {
	case "north":
		dx, dz = 0, -1
	case "south":
		dx, dz = 0, 1
	case "east":
		dx, dz = 1, 0
	case "west":
		dx, dz = -1, 0
	}
	footX, footZ := headX-dx, headZ-dz
	leftX, leftZ := -dz, dx
	candidates := [][2]int{
		{headX + leftX, headZ + leftZ},
		{headX - leftX, headZ - leftZ},
		{footX + leftX, footZ + leftZ},
		{footX - leftX, footZ - leftZ},
		{headX + dx, headZ + dz},
		{footX - dx, footZ - dz},
	}
	for _, candidate := range candidates {
		x, z := candidate[0], candidate[1]
		ok, loaded := s.world.CanEntityOccupyIfLoaded(float64(x)+0.5, float64(headY), float64(z)+0.5)
		if !loaded || !ok || s.world.GroundYAtOrBelow(x, z, headY) != headY-1 {
			continue
		}
		e.Position.X = float64(x) + 0.5
		e.Position.Y = float64(headY)
		e.Position.Z = float64(z) + 0.5
		e.VX, e.VY, e.VZ = 0, 0, 0
		e.OnGround = true
		return
	}
	// Fallback: place beyond the foot instead of leaving the villager in bed.
	e.Position.X = float64(footX-dx) + 0.5
	e.Position.Y = float64(headY)
	e.Position.Z = float64(footZ-dz) + 0.5
	e.VX, e.VY, e.VZ = 0, 0, 0
	e.OnGround = true
}

// tickGolemAI handles iron and snow golem behaviour.
//
// When the golem has been hit by a player, it charges at the attacker's last
// known position. The target refreshes each tick from the nearest attackable
// player and a successful swing applies health damage and knockback.
func (s *Server) tickGolemAI(e *corentity.Entity) {
	ai := s.mobAIFor(e)

	if !ai.hasTarget {
		// No target: wander like a homed passive mob near the village centre.
		_ = s.tickPassiveMobAI(e)
		return
	}

	// Refresh target from the nearest player within 24 blocks.
	nearestDist := 24.0
	for _, sess := range s.allPlayerSessions() {
		if sess.Player == nil || sess.Player.Dead ||
			sess.Player.GameMode == player.GameModeCreative ||
			sess.Player.GameMode == player.GameModeSpectator {
			continue
		}
		dx := sess.Player.Position.X - e.Position.X
		dz := sess.Player.Position.Z - e.Position.Z
		d := math.Hypot(dx, dz)
		if d < nearestDist {
			ai.targetX = sess.Player.Position.X
			ai.targetZ = sess.Player.Position.Z
			nearestDist = d
		}
	}
	if nearestDist >= 24.0 {
		// No player nearby — give up chase.
		ai.hasTarget = false
		e.VX, e.VZ = 0, 0
		return
	}

	dx, dz := ai.targetX-e.Position.X, ai.targetZ-e.Position.Z
	dist := math.Hypot(dx, dz)

	if ai.attackCooldown > 0 {
		ai.attackCooldown--
	}

	if dist <= 2.5 {
		// In melee range — stop and swing.
		e.VX, e.VZ = 0, 0
		if ai.attackCooldown == 0 {
			ai.attackCooldown = 20 // 1-second cooldown between hits
			// Find the nearest player, deal the iron golem's vanilla random
			// 7-21 damage, and launch the target upward.
			for _, sess := range s.allPlayerSessions() {
				if sess.Player == nil || sess.Player.Dead ||
					sess.Player.GameMode == player.GameModeCreative ||
					sess.Player.GameMode == player.GameModeSpectator {
					continue
				}
				pdx := sess.Player.Position.X - e.Position.X
				pdz := sess.Player.Position.Z - e.Position.Z
				if math.Hypot(pdx, pdz) > 2.5 {
					continue
				}
				// Knockback velocity away from golem.
				kbDist := math.Hypot(pdx, pdz)
				var kbX, kbZ float64
				if kbDist > 0 {
					kbX = pdx / kbDist * 0.8
					kbZ = pdz / kbDist * 0.8
				}
				damage := float32(7 + ai.rng.Intn(15))
				healthBefore, _, _, _ := sess.Player.HealthSnapshot()
				handler.DamagePlayer(sess, damage, "was slain by an Iron Golem", s.sessions)
				healthAfter, _, _, _ := sess.Player.HealthSnapshot()
				if healthAfter < healthBefore {
					s.sendPlayerVelocity(sess, kbX, 0.4, kbZ)
				}
				handler.BroadcastSoundAt(s.sessions, "minecraft:entity.iron_golem.attack", handler.SoundCategoryHostile,
					e.Position.X, e.Position.Y+1, e.Position.Z, 1, 1)
				break
			}
		}
		return
	}

	// Charge at the target at golem speed.
	speed := 0.14
	e.VX = dx / dist * speed
	e.VZ = dz / dist * speed
	e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
}

// tickHostileMobAI gives ground hostiles a common pursuit/melee controller.
// Creepers replace the melee swing with the vanilla 30-tick fuse.
func (s *Server) tickHostileMobAI(e *corentity.Entity) {
	ai := s.mobAIFor(e)
	var target *session.Session
	nearest := 32.0
	for _, candidate := range s.allPlayerSessions() {
		if candidate.Player == nil || candidate.Player.Dead ||
			candidate.Player.GameMode == player.GameModeCreative ||
			candidate.Player.GameMode == player.GameModeSpectator {
			continue
		}
		dx := candidate.Player.Position.X - e.Position.X
		dz := candidate.Player.Position.Z - e.Position.Z
		distance := math.Hypot(dx, dz)
		if distance < nearest && math.Abs(candidate.Player.Position.Y-e.Position.Y) <= 4 {
			nearest = distance
			target = candidate
		}
	}
	if target == nil {
		e.VX, e.VZ = 0, 0
		if e.Type == corentity.TypeCreeper && ai.fuseTick > 0 {
			ai.fuseTick -= 2
			if ai.fuseTick <= 0 {
				ai.fuseTick = 0
				handler.BroadcastCreeperSwell(e.EntityID, false, s.sessions)
			}
		}
		return
	}

	dx := target.Player.Position.X - e.Position.X
	dz := target.Player.Position.Z - e.Position.Z
	distance := math.Hypot(dx, dz)
	if distance > 0.001 {
		e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
	}

	if e.Type == corentity.TypeCreeper {
		if distance <= 3 {
			e.VX, e.VZ = 0, 0
			ai.fuseTick++
			if ai.fuseTick == 1 {
				handler.BroadcastCreeperSwell(e.EntityID, true, s.sessions)
				handler.BroadcastSoundAt(s.sessions, "minecraft:entity.creeper.primed", handler.SoundCategoryHostile,
					e.Position.X, e.Position.Y, e.Position.Z, 1, 1)
			}
			if ai.fuseTick >= 30 {
				s.explodeAt(e.Position.X, e.Position.Y, e.Position.Z, 3, "was blown up by a Creeper")
				e.Dead = true
			}
			return
		}
		if distance > 7 && ai.fuseTick > 0 {
			ai.fuseTick -= 2
			if ai.fuseTick <= 0 {
				ai.fuseTick = 0
				handler.BroadcastCreeperSwell(e.EntityID, false, s.sessions)
			}
		}
	}

	if ai.attackCooldown > 0 {
		ai.attackCooldown--
	}
	if distance <= 1.8 && e.Type != corentity.TypeCreeper {
		e.VX, e.VZ = 0, 0
		if ai.attackCooldown == 0 {
			ai.attackCooldown = 20
			damage := hostileAttackDamage(e.Type)
			healthBefore, _, _, _ := target.Player.HealthSnapshot()
			switch s.cfg.Difficulty {
			case "easy":
				damage *= 0.5
			case "hard":
				damage *= 1.5
			}
			name := strings.ReplaceAll(strings.TrimPrefix(string(e.Type), "minecraft:"), "_", " ")
			handler.DamagePlayer(target, damage, "was slain by a "+name, s.sessions)
			healthAfter, _, _, _ := target.Player.HealthSnapshot()
			if healthAfter < healthBefore {
				s.sendLegacyPlayerKnockback(target, e.Position.X, e.Position.Z, 0.4, 0.4)
			}
		}
		return
	}

	if distance > 0.001 {
		speed := 0.1
		e.VX = dx / distance * speed
		e.VZ = dz / distance * speed
	}
}

func hostileAttackDamage(t corentity.EntityType) float32 {
	switch t {
	case corentity.TypeSilverfish, corentity.TypeEndermite:
		return 1
	case corentity.TypeSpider, corentity.TypeCaveSpider, corentity.TypeSlime:
		return 2
	case corentity.TypeZombie, corentity.TypeZombieVillager, corentity.TypeHusk, corentity.TypeDrowned:
		return 3
	case corentity.TypePiglin:
		return 5
	case corentity.TypeHoglin, corentity.TypeZoglin:
		return 6
	case corentity.TypeEnderman, corentity.TypePiglinBrute:
		return 7
	case corentity.TypeWitherSkeleton:
		return 8
	case corentity.TypeRavager:
		return 12
	case corentity.TypeVindicator:
		return 13
	case corentity.TypeWarden:
		return 30
	default:
		return 3
	}
}

func (s *Server) tickProjectile(projectile *corentity.Entity) bool {
	if projectile.AgeTicks > 1200 {
		return true
	}
	start := projectile.Position
	projectile.VY -= 0.05
	projectile.Position.X += projectile.VX
	projectile.Position.Y += projectile.VY
	projectile.Position.Z += projectile.VZ
	projectile.VX *= 0.99
	projectile.VY *= 0.99
	projectile.VZ *= 0.99
	horizontal := math.Hypot(projectile.VX, projectile.VZ)
	projectile.Yaw = float32(math.Atan2(-projectile.VX, projectile.VZ) * 180 / math.Pi)
	projectile.Pitch = float32(math.Atan2(-projectile.VY, horizontal) * 180 / math.Pi)

	// Sample the swept path so fast arrows cannot tunnel through a one-block wall.
	distance := math.Sqrt(
		(projectile.Position.X-start.X)*(projectile.Position.X-start.X) +
			(projectile.Position.Y-start.Y)*(projectile.Position.Y-start.Y) +
			(projectile.Position.Z-start.Z)*(projectile.Position.Z-start.Z))
	steps := int(math.Ceil(distance * 4))
	if steps < 1 {
		steps = 1
	}
	for step := 1; step <= steps; step++ {
		t := float64(step) / float64(steps)
		x := start.X + (projectile.Position.X-start.X)*t
		y := start.Y + (projectile.Position.Y-start.Y)*t
		z := start.Z + (projectile.Position.Z-start.Z)*t
		block := s.world.GetBlock(int(math.Floor(x)), int(math.Floor(y)), int(math.Floor(z)))
		if coreworld.IsEntitySupportBlock(block.ResourceLocation()) {
			handler.BroadcastSoundAt(s.sessions, "minecraft:entity.arrow.hit", handler.SoundCategoryPlayers, x, y, z, 1, 1)
			return true
		}
	}

	for _, target := range s.allPlayerSessions() {
		if target.Player == nil || target.Player.EntityID == projectile.OwnerEntityID || target.Player.Dead {
			continue
		}
		centre := spatial.Vec3{X: target.Player.Position.X, Y: target.Player.Position.Y + 0.9, Z: target.Player.Position.Z}
		if pointSegmentDistanceSquared(centre, start, projectile.Position) > 0.8*0.8 {
			continue
		}
		shooterName := "a projectile"
		for _, shooter := range s.allPlayerSessions() {
			if shooter.Player != nil && shooter.Player.EntityID == projectile.OwnerEntityID {
				shooterName = shooter.Player.Username
				break
			}
		}
		if handler.DamagePlayerLegacy(target, projectile.ProjectileDamage, "was shot by "+shooterName, s.sessions) {
			s.sendLegacyPlayerKnockback(target, start.X, start.Z, 0.25, 0.1)
		}
		return true
	}

	for _, target := range s.world.Entities.Snapshot() {
		if target == projectile || target.Dead || target.EntityID == projectile.OwnerEntityID ||
			corentity.IsProjectile(target.Type) {
			continue
		}
		centre := spatial.Vec3{X: target.Position.X, Y: target.Position.Y + 0.8, Z: target.Position.Z}
		if pointSegmentDistanceSquared(centre, start, projectile.Position) <= 0.8*0.8 {
			s.world.QueueEntityDamageFrom(target.EntityID, projectile.ProjectileDamage, start.X, start.Z)
			return true
		}
	}
	return false
}

func pointSegmentDistanceSquared(point, start, end spatial.Vec3) float64 {
	dx, dy, dz := end.X-start.X, end.Y-start.Y, end.Z-start.Z
	lengthSquared := dx*dx + dy*dy + dz*dz
	if lengthSquared == 0 {
		x, y, z := point.X-start.X, point.Y-start.Y, point.Z-start.Z
		return x*x + y*y + z*z
	}
	t := ((point.X-start.X)*dx + (point.Y-start.Y)*dy + (point.Z-start.Z)*dz) / lengthSquared
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	closestX := start.X + dx*t
	closestY := start.Y + dy*t
	closestZ := start.Z + dz*t
	x, y, z := point.X-closestX, point.Y-closestY, point.Z-closestZ
	return x*x + y*y + z*z
}

// newRandomUUID generates a random RFC 4122 version-4 UUID.
func newRandomUUID() [16]byte {
	var uuid [16]byte
	if _, err := cryptorand.Read(uuid[:]); err != nil {
		// crypto/rand failure is extremely rare; panic is acceptable here
		// because the server cannot safely assign unique entity identities.
		panic("server: crypto/rand failed: " + err.Error())
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 1 (RFC 4122)
	return uuid
}

// handleConn is called in its own goroutine for every accepted TCP connection.
func (s *Server) handleConn(conn *network.ClientConn) {
	s.connCount.Add(1)
	defer s.connCount.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC in connection goroutine — client disconnected",
				"remote", conn.RemoteAddr(),
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	remote := conn.RemoteAddr()

	// ── Handshake ────────────────────────────────────────────────────────────
	_, err := handler.Handshake(conn, s.cfg)
	if err != nil {
		slog.Debug("handshake failed", "remote", remote, "err", err)
		return
	}

	// ── Route by state ───────────────────────────────────────────────────────
	switch conn.State {
	case network.StateStatus:
		if err := handler.HandleStatus(conn, s.cfg); err != nil {
			slog.Debug("status error", "remote", remote, "err", err)
		}

	case network.StateLogin:
		result, err := s.loginHandler.Handle(conn)
		if err != nil {
			slog.Debug("login error", "remote", remote, "err", err)
			return
		}

		// ── Configuration state ──────────────────────────────────────────────
		if err := handler.HandleConfiguration(conn, s.regProvider); err != nil {
			slog.Warn("configuration error", "remote", remote, "err", err)
			return
		}

		// ── Play state ───────────────────────────────────────────────────────
		p := s.registerPlayer(result)
		defer s.game.RemovePlayer(p.UUID)

		if err := handler.HandlePlay(conn, p, s.world, s.chunkSender, s.sessions, s.cmds, s.regProvider, s.cfg.WorldSeed, func() int64 { return s.worldAge }, int32(s.cfg.ViewDistance), int32(s.cfg.PreGenerateRadius), s.game.NextEntityID); err != nil {
			slog.Debug("play error", "remote", remote, "err", err)
		}

	default:
		slog.Warn("unhandled state after handshake", "remote", remote, "state", conn.State)
	}
}

// registerPlayer creates a core Player from a LoginResult, assigns an entity ID
// via the game core, and updates the global online count used in status pings.
func (s *Server) registerPlayer(result *handler.LoginResult) *player.Player {
	// protocol.UUID is [16]byte — convertible to the core's raw [16]byte UUID.
	p := player.New([16]byte(result.UUID), result.Name, player.ClientEditionJava)
	p.Operator = handler.IsOperatorName(result.Name)
	p.InvulnerableUntil = time.Now().Add(3 * time.Second)
	p.GameMode = configuredGameMode(s.cfg.DefaultGameMode)
	p.AttackCooldown = s.cfg.Combat.AttackCooldown
	p.KnockbackHorizontal = s.cfg.Combat.KnockbackHorizontal
	p.KnockbackVertical = s.cfg.Combat.KnockbackVertical
	p.OnDeath = s.dropPlayerInventory
	// Spawn on the highest generated or loaded block at the world origin.
	p.Position.X = float64(s.spawnX) + 0.5
	p.Position.Y = float64(s.world.SurfaceY(s.spawnX, s.spawnZ) + 1)
	p.Position.Z = float64(s.spawnZ) + 0.5
	p.WorldSpawn = p.Position

	if err := s.game.AddPlayer(p); err != nil {
		// Duplicate UUID — extremely rare; log and continue with assigned ID.
		slog.Warn("duplicate player UUID", "name", p.Username, "uuid", p.UUID, "err", err)
	}

	// Update the status-ping online count.
	handler.OnlineCount.Store(int32(s.game.OnlineCount()))
	slog.Info("player joined", "name", p.Username, "uuid", p.UUID, "entityID", p.EntityID)
	return p
}

func configuredGameMode(name string) player.GameMode {
	switch strings.ToLower(name) {
	case "creative":
		return player.GameModeCreative
	case "adventure":
		return player.GameModeAdventure
	case "spectator":
		return player.GameModeSpectator
	default:
		return player.GameModeSurvival
	}
}

func difficultyID(name string) int32 {
	switch strings.ToLower(name) {
	case "peaceful":
		return 0
	case "easy":
		return 1
	case "hard":
		return 3
	default:
		return 2
	}
}

// ActiveConns returns the current number of open TCP connections.
func (s *Server) ActiveConns() int64 {
	return s.connCount.Load()
}

// OnlineCount returns the number of players registered with the game core.
func (s *Server) OnlineCount() int {
	return s.game.OnlineCount()
}

// Config returns the server's configuration (read-only).
func (s *Server) Config() *config.Config {
	return s.cfg
}

// saveWorldAge writes the current worldAge to <worldDir>/gocraft_time.dat so
// the day/night cycle survives server restarts.  Disk-only; no-op for memory worlds.
func (s *Server) saveWorldAge() {
	if s.cfg.WorldStorage != config.WorldStorageDisk {
		return
	}
	path := s.cfg.WorldDir + "/gocraft_time.dat"
	data := strconv.FormatInt(s.worldAge, 10)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		slog.Warn("could not save world age", "err", err)
	}
}

// loadSavedWorldAge reads gocraft_time.dat from worldDir.
// Returns (age, true) if the file exists and parses successfully.
func loadSavedWorldAge(worldDir string) (int64, bool) {
	data, err := os.ReadFile(worldDir + "/gocraft_time.dat")
	if err != nil {
		return 0, false
	}
	age, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || age < 0 {
		return 0, false
	}
	return age, true
}

// tickBlockPhysics drains all due block-physics updates and processes them:
//   - updateFall: if the block is still a gravity block with air below, convert
//     it to a FallingBlock entity and clear the world block.
//   - updateFluid: spread water or lava into adjacent passable positions.
func (s *Server) tickBlockPhysics() {
	due := s.world.BlockPhysics.DrainDue(s.worldAge)

	// Flush redstone — may produce visual changes and newly-powered loads.
	redstone := s.world.Redstone.FlushUpdates()

	if len(due) == 0 && len(redstone.Changes) == 0 {
		return
	}

	var blockChanges []coreworld.BlockChange

	for _, u := range due {
		switch u.Kind {
		case coreworld.UpdateFall:
			s.processFallUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateFluid:
			s.processFluidUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateLeafDecay:
			s.processLeafDecayUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateFire:
			s.processFireUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateIce:
			s.processIceUpdate(u.X, u.Y, u.Z, &blockChanges)
		}
	}

	// Activate loads that just received redstone power.
	for _, pos := range redstone.PoweredLoads {
		block := s.world.GetBlock(pos[0], pos[1], pos[2])
		switch block.ResourceLocation() {
		case "minecraft:tnt":
			s.activateTNT(pos[0], pos[1], pos[2], &blockChanges)
		}
		// Pistons, dispensers, etc. can be added here.
	}

	blockChanges = append(blockChanges, redstone.Changes...)

	// Broadcast all block changes to clients in one go.
	for _, bc := range blockChanges {
		handler.BroadcastBlockChange(bc, s.sessions)
	}
}

// processFallUpdate converts a gravity block to a FallingBlock entity if it
// still has no support below.
func (s *Server) processFallUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if !coreworld.IsGravityBlock(block.ResourceLocation()) {
		return
	}
	below := s.world.GetBlock(x, y-1, z)
	if coreworld.IsSolidLandingSurface(below.ResourceLocation()) {
		return // still has support — no fall needed
	}

	// Remove block from world.
	s.world.SetBlock(x, y, z, coreworld.Air)
	*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: coreworld.Air})

	// Spawn a FallingBlock entity at the block's centre.
	id := s.game.NextEntityID()
	uuid := newRandomUUID()
	fb := corentity.New(id, uuid, corentity.TypeFallingBlock,
		float64(x)+0.5, float64(y), float64(z)+0.5)
	fb.FallingBlockName = block.ResourceLocation()
	fb.FallingBlockStateID = javaworld.StateID(block)
	fb.VY = -0.04 // initial downward nudge
	fb.OnGround = false
	s.world.Entities.Add(fb)
	handler.BroadcastSpawnMob(fb, s.sessions)
}

// processFluidUpdate spreads water or lava from (x,y,z) into adjacent air.
func (s *Server) processFluidUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	name := block.ResourceLocation()
	if !coreworld.IsFluidBlock(name) {
		return
	}
	level := coreworld.FluidLevel(block)
	if level < 0 {
		return
	}

	isLava := name == "minecraft:lava"
	// Lava spreads at most 3 blocks, water at most 7.
	maxLevel := 7
	if isLava {
		maxLevel = 3
	}
	// Delay between spreads: water=5 ticks, lava=30 ticks.
	spreadDelay := int64(5)
	if isLava {
		spreadDelay = 30
	}

	// Try to fall down first — falling fluid keeps the same level.
	below := s.world.GetBlock(x, y-1, z)
	belowName := below.ResourceLocation()

	// Water+lava collision below.
	if opposite := fluidOpposite(name); belowName == opposite {
		result := fluidCollisionResult(name, below)
		s.world.SetBlock(x, y-1, z, result)
		*changes = append(*changes, coreworld.BlockChange{X: x, Y: y - 1, Z: z, Block: result})
	} else if belowName == "minecraft:air" || belowName == "minecraft:cave_air" ||
		belowName == "minecraft:void_air" || coreworld.IsFluidPassable(belowName) {
		if y-1 >= coreworld.WorldMinY {
			// Harden concrete powder if it falls into water.
			if coreworld.IsFluidBlock(name) && name == "minecraft:water" &&
				coreworld.IsConcretePowder(belowName) {
				hardened := coreworld.Block{
					Namespace:  "minecraft",
					Name:       strings.TrimPrefix(coreworld.ConcreteName(belowName), "minecraft:"),
					Properties: map[string]string{},
				}
				s.world.SetBlock(x, y-1, z, hardened)
				*changes = append(*changes, coreworld.BlockChange{X: x, Y: y - 1, Z: z, Block: hardened})
			} else {
				newBlock := coreworld.MakeFluid(name, level) // falling keeps level
				s.world.SetBlock(x, y-1, z, newBlock)
				*changes = append(*changes, coreworld.BlockChange{X: x, Y: y - 1, Z: z, Block: newBlock})
				s.world.BlockPhysics.ScheduleFluid(x, y-1, z, s.worldAge, spreadDelay)
			}
		}
	}

	// Spread horizontally if not too diluted.
	if level < maxLevel {
		newLevel := level + 1
		dirs := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
		for _, d := range dirs {
			nx, nz := x+d[0], z+d[1]
			nb := s.world.GetBlock(nx, y, nz)
			nbName := nb.ResourceLocation()

			// Water+lava collision horizontal.
			if opposite := fluidOpposite(name); nbName == opposite {
				result := fluidCollisionResult(name, nb)
				s.world.SetBlock(nx, y, nz, result)
				*changes = append(*changes, coreworld.BlockChange{X: nx, Y: y, Z: nz, Block: result})
				continue
			}

			// Harden concrete powder touched by water.
			if name == "minecraft:water" && coreworld.IsConcretePowder(nbName) {
				hardened := coreworld.Block{
					Namespace:  "minecraft",
					Name:       strings.TrimPrefix(coreworld.ConcreteName(nbName), "minecraft:"),
					Properties: map[string]string{},
				}
				s.world.SetBlock(nx, y, nz, hardened)
				*changes = append(*changes, coreworld.BlockChange{X: nx, Y: y, Z: nz, Block: hardened})
				continue
			}

			// Only spread into air / passable — don't overwrite blocks.
			if nbName == "minecraft:air" || nbName == "minecraft:cave_air" ||
				nbName == "minecraft:void_air" || coreworld.IsFluidPassable(nbName) {
				existingLevel := coreworld.FluidLevel(s.world.GetBlock(nx, y, nz))
				if existingLevel < 0 || existingLevel > newLevel {
					newBlock := coreworld.MakeFluid(name, newLevel)
					s.world.SetBlock(nx, y, nz, newBlock)
					*changes = append(*changes, coreworld.BlockChange{X: nx, Y: y, Z: nz, Block: newBlock})
					s.world.BlockPhysics.ScheduleFluid(nx, y, nz, s.worldAge, spreadDelay)
				}
			}
		}
	}
}

// fluidOpposite returns the opposing fluid name for water/lava collision.
func fluidOpposite(name string) string {
	if name == "minecraft:water" {
		return "minecraft:lava"
	}
	if name == "minecraft:lava" {
		return "minecraft:water"
	}
	return ""
}

// fluidCollisionResult returns the block produced when fluid meets its opposite.
// Vanilla rules:
//   - Lava source (level 0) + water → obsidian
//   - Flowing lava (level > 0) + water → cobblestone
//   - Water + lava source → obsidian (lava wins)
func fluidCollisionResult(fluid string, oppositeBlock coreworld.Block) coreworld.Block {
	var lavaLevel int
	if fluid == "minecraft:lava" {
		lavaLevel = 0 // the spreading fluid is lava source
	} else {
		lavaLevel = coreworld.FluidLevel(oppositeBlock)
	}
	if lavaLevel == 0 {
		return coreworld.Block{Namespace: "minecraft", Name: "obsidian", Properties: map[string]string{}}
	}
	return coreworld.Block{Namespace: "minecraft", Name: "cobblestone", Properties: map[string]string{}}
}

// ─── Leaf decay ──────────────────────────────────────────────────────────────

// processLeafDecayUpdate checks if a leaf block is too far from any log and
// decays it to air if so. Schedules adjacent leaves for re-check.
func (s *Server) processLeafDecayUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if !coreworld.IsLeafBlock(block.ResourceLocation()) {
		return // already gone or changed
	}
	// Player-placed (persistent) leaves never decay.
	if block.Properties["persistent"] == "true" {
		return
	}

	// BFS to find nearest log within LeafDecayRadius steps.
	type pos3 struct{ x, y, z int }
	start := pos3{x, y, z}
	visited := map[pos3]int{start: 0}
	queue := []pos3{start}
	found := false
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		d := visited[cur]
		if d > coreworld.LeafDecayRadius {
			break
		}
		for _, nb := range [6][3]int{
			{cur.x + 1, cur.y, cur.z}, {cur.x - 1, cur.y, cur.z},
			{cur.x, cur.y + 1, cur.z}, {cur.x, cur.y - 1, cur.z},
			{cur.x, cur.y, cur.z + 1}, {cur.x, cur.y, cur.z - 1},
		} {
			p := pos3{nb[0], nb[1], nb[2]}
			if _, seen := visited[p]; seen {
				continue
			}
			nbName := s.world.GetBlock(nb[0], nb[1], nb[2]).ResourceLocation()
			if coreworld.IsLogBlock(nbName) {
				found = true
				break
			}
			if coreworld.IsLeafBlock(nbName) {
				visited[p] = d + 1
				queue = append(queue, p)
			}
		}
		if found {
			break
		}
	}

	if !found {
		// Decay this leaf.
		s.world.SetBlock(x, y, z, coreworld.Air)
		*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: coreworld.Air})
		// Schedule adjacent leaves to re-check (they may now also be too far).
		for _, nb := range [6][3]int{
			{x + 1, y, z}, {x - 1, y, z},
			{x, y + 1, z}, {x, y - 1, z},
			{x, y, z + 1}, {x, y, z - 1},
		} {
			nbBlock := s.world.GetBlock(nb[0], nb[1], nb[2])
			if coreworld.IsLeafBlock(nbBlock.ResourceLocation()) {
				s.world.BlockPhysics.ScheduleLeafDecay(nb[0], nb[1], nb[2], s.worldAge, 5)
			}
		}
	}
}

// ─── Fire spreading ──────────────────────────────────────────────────────────

// fireTicksKey tracks how many fire-spread ticks have occurred at a position
// so fire can self-extinguish after FireBurnTicks total ticks without fuel.
// Stored as a per-server map since it's only written by the tick goroutine.
var fireAgeMap = map[[3]int]int64{}

// processFireUpdate spreads fire to adjacent flammable blocks and may burn out.
func (s *Server) processFireUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if !coreworld.IsFireBlock(block.ResourceLocation()) {
		delete(fireAgeMap, [3]int{x, y, z})
		return
	}
	below := s.world.GetBlock(x, y-1, z)

	// Fire on netherrack / soul sand is permanent.
	isPermanent := coreworld.IsNetherrack(below.ResourceLocation())

	key := [3]int{x, y, z}
	fireAgeMap[key]++
	age := fireAgeMap[key]

	// Burn out after FireBurnTicks if not on permanent fuel.
	if !isPermanent && age > coreworld.FireBurnTicks {
		s.world.SetBlock(x, y, z, coreworld.Air)
		*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: coreworld.Air})
		delete(fireAgeMap, key)
		return
	}

	fireBlock := coreworld.Block{Namespace: "minecraft", Name: "fire", Properties: map[string]string{}}

	// Try to spread to adjacent flammable blocks (each direction independently).
	dirs := [6][3]int{
		{x + 1, y, z}, {x - 1, y, z},
		{x, y + 1, z}, {x, y - 1, z},
		{x, y, z + 1}, {x, y, z - 1},
	}
	for _, d := range dirs {
		target := s.world.GetBlock(d[0], d[1], d[2])
		tName := target.ResourceLocation()
		score := coreworld.FlammabilityScore(tName)
		if score <= 0 {
			continue
		}
		// Probability check: higher score = more likely to ignite.
		// Use a simple deterministic-ish check based on worldAge + position.
		roll := int((s.worldAge + int64(d[0]*31+d[1]*17+d[2]*7)) % 100)
		if roll >= score {
			continue
		}
		// Ignite: if it's TNT, spawn primed TNT instead.
		if tName == "minecraft:tnt" {
			s.activateTNT(d[0], d[1], d[2], changes)
			continue
		}
		// Place fire at the target position.
		s.world.SetBlock(d[0], d[1], d[2], fireBlock)
		*changes = append(*changes, coreworld.BlockChange{X: d[0], Y: d[1], Z: d[2], Block: fireBlock})
		s.world.BlockPhysics.ScheduleFire(d[0], d[1], d[2], s.worldAge, 20)
	}

	// Reschedule this fire for next spread tick.
	s.world.BlockPhysics.ScheduleFire(x, y, z, s.worldAge, 20+age%10)
}

// ─── Ice melting ─────────────────────────────────────────────────────────────

// processIceUpdate melts an ice block to water if exposed above (no opaque block
// directly above, indicating sky exposure / warmth).
func (s *Server) processIceUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if !coreworld.IsIceBlock(block.ResourceLocation()) {
		return
	}
	// Packed/blue ice never melts.
	if coreworld.IsPackedIce(block.ResourceLocation()) {
		return
	}
	// Melt: replace with water.
	water := coreworld.MakeFluid("minecraft:water", 0)
	s.world.SetBlock(x, y, z, water)
	*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: water})
	s.world.BlockPhysics.ScheduleFluid(x, y, z, s.worldAge, 5)
}

// ─── TNT ─────────────────────────────────────────────────────────────────────

// activateTNT removes the TNT block and spawns a primed TNT entity with an 80-tick fuse.
func (s *Server) activateTNT(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if block.ResourceLocation() != "minecraft:tnt" {
		return
	}
	s.world.SetBlock(x, y, z, coreworld.Air)
	*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: coreworld.Air})

	id := s.game.NextEntityID()
	uuid := newRandomUUID()
	tnt := corentity.New(id, uuid, corentity.TypePrimedTNT,
		float64(x)+0.5, float64(y), float64(z)+0.5)
	tnt.FuseTicks = 80
	tnt.VY = 0.2 // small upward pop like vanilla
	s.world.Entities.Add(tnt)
	handler.BroadcastSpawnMob(tnt, s.sessions)
}

// explodeTNT applies an explosion centred at (cx,cy,cz) with radius 4.
// Destroys blocks in a rough sphere, damages nearby entities, and broadcasts
// the block changes to all clients.
func (s *Server) explodeTNT(cx, cy, cz float64) {
	s.explodeAt(cx, cy, cz, 4, "blew up")
}

func (s *Server) explodeAt(cx, cy, cz, radius float64, playerDeathCause string) {
	var changes []coreworld.BlockChange

	// Destroy blocks in explosion radius with probability based on distance.
	for dx := -int(radius) - 1; dx <= int(radius)+1; dx++ {
		for dy := -int(radius) - 1; dy <= int(radius)+1; dy++ {
			for dz := -int(radius) - 1; dz <= int(radius)+1; dz++ {
				dist := math.Sqrt(float64(dx*dx + dy*dy + dz*dz))
				if dist > radius {
					continue
				}
				bx := int(math.Round(cx)) + dx
				by := int(math.Round(cy)) + dy
				bz := int(math.Round(cz)) + dz
				block := s.world.GetBlock(bx, by, bz)
				name := block.ResourceLocation()
				if name == "" || name == "minecraft:air" || name == "minecraft:bedrock" {
					continue
				}
				// Probability: blocks at the edge have ~30% chance, centre ~100%.
				breakChance := 1.0 - (dist / (radius * 1.3))
				roll := float64((int64(bx*31+by*17+bz*7)+s.worldAge)%100) / 100.0
				if roll > breakChance {
					continue
				}
				// Chain TNT.
				if name == "minecraft:tnt" {
					s.activateTNT(bx, by, bz, &changes)
					continue
				}
				s.world.SetBlock(bx, by, bz, coreworld.Air)
				changes = append(changes, coreworld.BlockChange{X: bx, Y: by, Z: bz, Block: coreworld.Air})
			}
		}
	}

	// Damage nearby entities.
	for _, e := range s.world.Entities.Snapshot() {
		dx := e.Position.X - cx
		dy := e.Position.Y - cy
		dz := e.Position.Z - cz
		dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if dist > radius*2 {
			continue
		}
		damage := float32((1.0 - dist/(radius*2)) * 40)
		if damage > 0 {
			s.world.QueueEntityDamageFrom(e.EntityID, damage, cx, cz)
		}
	}

	// Player health is not stored in the entity manager, so explosions must
	// damage online sessions separately.
	for _, sess := range s.allPlayerSessions() {
		if sess.Player == nil {
			continue
		}
		dx := sess.Player.Position.X - cx
		dy := sess.Player.Position.Y - cy
		dz := sess.Player.Position.Z - cz
		distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if distance > radius*2 {
			continue
		}
		impact := 1 - distance/(radius*2)
		damage := float32(((impact*impact+impact)/2)*7*(radius*2) + 1)
		if handler.DamagePlayer(sess, damage, playerDeathCause, s.sessions) {
			s.sendLegacyPlayerKnockback(sess, cx, cz, impact*1.2, impact*0.8)
		}
	}

	handler.BroadcastSoundAt(s.sessions, "minecraft:entity.generic.explode", handler.SoundCategoryHostile,
		cx, cy, cz, 4, 1)

	for _, bc := range changes {
		handler.BroadcastBlockChange(bc, s.sessions)
	}
}

// ─── Boat physics ────────────────────────────────────────────────────────────

// tickBoatPhysics updates an unoccupied boat's position each tick.
// Boats float on the water surface and decelerate from drag.
func (s *Server) tickBoatPhysics(e *corentity.Entity) {
	const boatDrag = 0.9
	const boatGravity = -0.04
	const minBoatVel = 0.001

	bx := int(math.Floor(e.Position.X))
	by := int(math.Floor(e.Position.Y))
	bz := int(math.Floor(e.Position.Z))

	// Check block at boat's Y and one below for water.
	atBlock := s.world.GetBlock(bx, by, bz)
	belowBlock := s.world.GetBlock(bx, by-1, bz)
	inWater := coreworld.IsFluidBlock(atBlock.ResourceLocation())
	onWater := coreworld.IsFluidBlock(belowBlock.ResourceLocation())

	if inWater || onWater {
		// Float: nudge Y to sit on the water surface.
		targetY := float64(by) + 0.0625 // boats sit just above the water block
		if onWater && !inWater {
			targetY = float64(by) // sit on top of water block below
		}
		diff := targetY - e.Position.Y
		e.VY = diff * 0.1
		e.OnGround = false
	} else if !e.OnGround {
		// Apply gravity if airborne and not on water.
		e.VY += boatGravity
	} else {
		e.VY = 0
	}

	e.Position.X += e.VX
	e.Position.Y += e.VY
	e.Position.Z += e.VZ

	// Drag.
	e.VX *= boatDrag
	e.VZ *= boatDrag
	if math.Abs(e.VX) < minBoatVel {
		e.VX = 0
	}
	if math.Abs(e.VZ) < minBoatVel {
		e.VZ = 0
	}

	// Ground detection (simplified: if boat sinks below a solid block, snap up).
	groundY := float64(s.world.GroundYAtOrBelow(bx, bz, by+1))
	if e.Position.Y < groundY+0.5 {
		e.Position.Y = groundY + 0.5
		e.VY = 0
		e.OnGround = true
	}
}

// babyGrowUpTicks is how many ticks a baby villager takes to become an adult.
// 6000 ticks = 5 minutes at 20 TPS.
const babyGrowUpTicks = 6000

// sleepAnimTicks is the number of ticks the server waits after all players
// fall asleep before skipping to morning. Vanilla considers a player fully
// asleep after 100 ticks (5 seconds at 20 TPS), allowing the full transition.
const sleepAnimTicks = 100

// tickSleep checks whether all online players are sleeping and, if so, waits
// sleepAnimTicks for the animation to play, then skips the clock to morning
// and wakes everyone.
func (s *Server) tickSleep() {
	players := make([]*player.Player, 0, s.game.OnlineCount())
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.GameMode != player.GameModeSpectator {
			players = append(players, p)
		}
	})
	if len(players) == 0 {
		s.sleepAllTick = 0
		return
	}
	tod := s.worldAge % 24000
	if tod < 12541 || tod > 23459 {
		// Not night — wake anyone who was still waiting for other players.
		for _, p := range players {
			if !p.Sleeping {
				continue
			}
			p.Sleeping = false
			handler.BroadcastPlayerWaking(p.EntityID, s.sessions)
		}
		s.sleepAllTick = 0
		return
	}
	total, sleeping := 0, 0
	for _, p := range players {
		total++
		if p.Sleeping {
			sleeping++
		}
	}
	if total == 0 || sleeping < total {
		// Not everyone sleeping — reset the timer if it was running.
		s.sleepAllTick = 0
		return
	}
	// All players are sleeping.
	if s.sleepAllTick == 0 {
		// First tick everyone is sleeping — start the animation countdown.
		s.sleepAllTick = s.worldAge
		return
	}
	if s.worldAge-s.sleepAllTick < sleepAnimTicks {
		// Still within the animation window — wait.
		return
	}
	// Animation window elapsed: skip night and wake everyone.
	s.sleepAllTick = 0
	handler.SkipNightAndWake(s.world, s.sessions)
	for _, p := range players {
		if p.Sleeping {
			p.Sleeping = false
			handler.BroadcastPlayerWaking(p.EntityID, s.sessions)
		}
	}
	tod2 := s.worldAge % 24000
	if tod2 < 6000 {
		s.worldAge += 6000 - tod2
	} else {
		s.worldAge += 24000 - tod2 + 6000
	}
	handler.DispatchWorldTime(s.worldAge, s.worldAge%24000, s.sessions)
	_ = s.world.DrainTimeSkip()
}

// tickVillagerAging increments the age of every baby villager each tick and
// grows them into adults when babyGrowUpTicks is reached.
// Called by the entity tick goroutine — no locking needed.
func (s *Server) tickVillagerAging() {
	for _, e := range s.world.Entities.Snapshot() {
		if e.Type != corentity.TypeVillager || !e.IsBaby || e.Dead {
			continue
		}
		e.BabyAgeTicks++
		if e.BabyAgeTicks >= babyGrowUpTicks {
			e.IsBaby = false
			e.BabyAgeTicks = 0
			handler.BroadcastVillagerMetadata(e, s.sessions)
			slog.Info("baby villager grew up", "id", e.EntityID,
				"x", e.Position.X, "z", e.Position.Z)
		}
	}
}

// tickVillagerBreeding checks each village cluster and, when there are at least
// 2 adults and at least one unoccupied bed, spawns a new baby villager.
// Called every 2400 ticks (~2 minutes).
func (s *Server) tickVillagerBreeding() {
	all := s.world.Entities.Snapshot()

	// Group villagers by village center (cluster key).
	type villageInfo struct {
		adults   []*corentity.Entity
		babies   int
		center   spatial.BlockPos
		occupied map[[3]int32]struct{}
	}
	villages := make(map[[2]int]*villageInfo)

	for _, e := range all {
		if e.Type != corentity.TypeVillager || !e.HasVillageHome || e.Dead {
			continue
		}
		key := [2]int{int(e.VillageCenter.X / 64), int(e.VillageCenter.Z / 64)}
		info := villages[key]
		if info == nil {
			info = &villageInfo{center: e.VillageCenter, occupied: make(map[[3]int32]struct{})}
			villages[key] = info
		}
		bedKey := [3]int32{e.VillageBed.X, e.VillageBed.Y, e.VillageBed.Z}
		info.occupied[bedKey] = struct{}{}
		if e.IsBaby {
			info.babies++
		} else {
			info.adults = append(info.adults, e)
		}
	}

	for _, info := range villages {
		if len(info.adults) < 2 {
			continue
		}
		// Cap babies at 1/3 of adults.
		if info.babies*3 >= len(info.adults) {
			continue
		}
		var freeBed spatial.BlockPos
		foundFreeBed := false
		for _, bed := range s.world.VillageBedsNear(info.center, 48) {
			key := [3]int32{bed.X, bed.Y, bed.Z}
			if _, taken := info.occupied[key]; !taken {
				freeBed, foundFreeBed = bed, true
				break
			}
		}
		if !foundFreeBed {
			continue
		}
		parent := info.adults[s.spawnRNG.Intn(len(info.adults))]

		// Spawn the baby near the parent.
		id := s.game.NextEntityID()
		uuid := newRandomUUID()
		bx := parent.Position.X + (s.spawnRNG.Float64()-0.5)*4
		bz := parent.Position.Z + (s.spawnRNG.Float64()-0.5)*4
		by := parent.Position.Y
		baby := corentity.New(id, uuid, corentity.TypeVillager, bx, by, bz)
		baby.VillagerVariant = parent.VillagerVariant
		baby.VillagerProfession = corentity.VillagerProfessionNone // babies have no profession
		baby.VillagerLevel = 1
		baby.HasVillageHome = parent.HasVillageHome
		baby.VillageHome = parent.VillageHome
		baby.VillageCenter = parent.VillageCenter
		baby.VillageBed = freeBed
		baby.VillageWorkstation = parent.VillageWorkstation
		baby.IsBaby = true
		baby.OnGround = true
		s.world.Entities.Add(baby)
		handler.BroadcastSpawnMob(baby, s.sessions)
		slog.Info("villager baby born", "id", id,
			"x", bx, "z", bz,
			"centerX", parent.VillageCenter.X, "centerZ", parent.VillageCenter.Z,
			"adults", len(info.adults), "babies", info.babies)
	}
}

// Shutdown closes the listener immediately.
func (s *Server) Shutdown() error {
	if err := s.listener.Close(); err != nil {
		return fmt.Errorf("server: closing listener: %w", err)
	}
	return nil
}
