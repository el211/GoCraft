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
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
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
}

// mobAI holds the wander state for a passive mob.
// All fields are written only by the entity tick goroutine.
type mobAI struct {
	homeX, homeZ  float64    // world-space spawn/home position (homed mobs only)
	dirX, dirZ    float64    // current normalised walk direction
	wanderTick    int        // ticks until next direction pick (0 = pick now)
	pauseTick     int        // remaining ticks of stillness (overrides wanderTick)
	panicTick     int        // remaining ticks fleeing from a recent attacker
	knockbackTick int        // ticks retaining the configured initial hit velocity
	roaming       bool       // true = no fixed home (animals); false = homed (villagers)
	rng           *rand.Rand // per-entity PRNG seeded from entity ID
}

// New creates a Server with the given configuration.
// It initialises the game core and generates the RSA keypair for online-mode auth.
func New(cfg *config.Config) (*Server, error) {
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
	if cfg.WorldStorage == config.WorldStorageDisk {
		if metadata, metadataErr := anvil.LoadLevelMetadata(cfg.WorldDir); metadataErr == nil {
			cfg.WorldSeed = metadata.Seed
			spawnX, spawnZ = int(metadata.SpawnX), int(metadata.SpawnZ)
			slog.Info("server: loaded Java level.dat",
				"world", metadata.LevelName,
				"dataVersion", metadata.DataVersion,
				"version", metadata.VersionName,
				"seed", metadata.Seed,
				"spawnX", metadata.SpawnX,
				"spawnY", metadata.SpawnY,
				"spawnZ", metadata.SpawnZ,
			)
		} else if !errors.Is(metadataErr, os.ErrNotExist) {
			slog.Warn("server: could not parse level.dat", "worldDir", cfg.WorldDir, "err", metadataErr)
		}
		st, err := anvil.NewStorage(cfg.WorldDir)
		if err != nil {
			return nil, fmt.Errorf("server: opening Anvil storage %s: %w", cfg.WorldDir, err)
		}
		storage = st
		slog.Info("server: opened Anvil world", "worldDir", cfg.WorldDir, "storage", "disk")
	} else {
		slog.Info("server: using memory-only world storage", "storage", "memory")
	}

	gameCore := game.New()
	cmds := handler.NewDispatcher()
	cmds.SetEntityIDAllocator(gameCore.NextEntityID)
	handler.RegisterBuiltins(cmds)

	bus := intent.NewBus(64, 512)

	worldInstance := coreworld.New(coreworld.NewOverworldGenerator(cfg.WorldSeed), storage, cfg.Villagers)
	worldInstance.SetMaxCachedChunks(cfg.MaxCachedChunks)

	s := &Server{
		cfg:         cfg,
		game:        gameCore,
		privKey:     privKey,
		pubKeyDER:   pubKeyDER,
		world:       worldInstance,
		spawnX:      spawnX,
		spawnZ:      spawnZ,
		regProvider: &registry.VanillaProvider{},
		chunkSender: javaworld.DefaultSender,
		sessions:    session.NewManager(),
		cmds:        cmds,
		intentBus:   bus,
		mobAIs:      make(map[int32]*mobAI),
		spawnRNG:    rand.New(rand.NewSource(cfg.WorldSeed ^ 0x4d6f624372616674)),
	}
	// Warm spawn immediately; login-time streaming will reuse this cache.
	s.world.QueuePregeneration(int32(math.Floor(float64(spawnX)/16)), int32(math.Floor(float64(spawnZ)/16)), int32(cfg.PreGenerateRadius))
	s.loginHandler = handler.NewLoginHandler(cfg, privKey, pubKeyDER)
	s.listener = network.NewListener(cfg.Addr(), s.handleConn)

	if cfg.Bedrock.Enabled {
		s.bedrockListener = bedrock.NewListener(cfg.Bedrock, bus)
	}
	return s, nil
}

// Run starts the server and blocks until ctx is cancelled or a fatal error occurs.
// All background goroutines are tracked with a WaitGroup and are joined before
// the world is flushed to disk, ensuring clean shutdown of both listeners.
func (s *Server) Run(ctx context.Context) error {
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
	return listenErr
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
			s.tickIntents()
			s.tickEntities()
			if s.worldAge%600 == 0 {
				if err := s.world.Flush(); err != nil {
					slog.Warn("world autosave failed", "err", err)
				}
			}
		}
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
		}
	}
}

// applyJoin creates a canonical Player, registers it with the game core, and
// sends a JoinResult to the waiting adapter goroutine.
func (s *Server) applyJoin(i intent.JoinIntent) {
	edition := player.ClientEditionBedrock
	if i.Edition == "java" {
		edition = player.ClientEditionJava
	}

	p := player.New(i.PlayerUUID, i.Username, edition)
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
		Position: spatial.Vec3{X: 0, Y: 65, Z: 0},
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
	p.Position = m.Position
	p.Rotation = m.Rotation
	p.OnGround = m.OnGround
}

// applyChat broadcasts a chat message to all active Java sessions.
func (s *Server) applyChat(i intent.ChatIntent) {
	msg := fmt.Sprintf("<%s> %s", i.DisplayName, i.Message)
	handler.BroadcastSystemMessage(s.sessions, msg)
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
		deadIDs      []int32             // entity IDs removed from the world this tick
	)

	for entityID, event := range s.world.DrainEntityDamage() {
		entity, ok := s.world.Entities.Get(entityID)
		if !ok || entity.Dead {
			continue
		}
		entity.Damage(event.Amount)
		if isPassiveMob(entity.Type) && !entity.Dead {
			s.startPassiveMobPanic(entity, event)
		}
		hurtEntities = append(hurtEntities, entity)
		slog.Info("entity damaged", "type", entity.Type, "id", entityID,
			"damage", event.Amount, "health", entity.Health)
	}

	for _, e := range s.world.Entities.Snapshot() {
		// ── Dead entity cleanup ───────────────────────────────────────────────
		if e.Dead {
			s.world.Entities.Remove(e.EntityID)
			delete(s.mobAIs, e.EntityID)
			deadIDs = append(deadIDs, e.EntityID)
			slog.Info("entity died", "type", e.Type, "id", e.EntityID)
			continue
		}

		// ── Passive mob AI (wander) ───────────────────────────────────────────
		if isPassiveMob(e.Type) {
			s.tickPassiveMobAI(e)
		}

		// ── Gravity ───────────────────────────────────────────────────────────
		if !e.OnGround {
			e.VY += gravity
		}

		// ── Position integration ──────────────────────────────────────────────
		prevX, prevY, prevZ := e.Position.X, e.Position.Y, e.Position.Z
		nextX := e.Position.X + e.VX
		if s.world.CanEntityOccupy(nextX, e.Position.Y, e.Position.Z) {
			e.Position.X = nextX
		} else {
			e.VX = 0
		}
		e.Position.Y += e.VY
		nextZ := e.Position.Z + e.VZ
		if s.world.CanEntityOccupy(e.Position.X, e.Position.Y, nextZ) {
			e.Position.Z = nextZ
		} else {
			e.VZ = 0
		}

		// ── Ground detection (generated or loaded terrain) ───────────────────────
		groundY := float64(s.world.GroundYAtOrBelow(int(math.Floor(e.Position.X)), int(math.Floor(e.Position.Z)), int(math.Floor(math.Max(prevY, e.Position.Y)))) + 1)
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
	}

	// Build packets and dispatch network I/O off the tick goroutine.
	// DispatchTickBroadcast reads entity fields here (tick goroutine, sole
	// writer) to build immutable packets before spawning the send goroutine.
	handler.DispatchTickBroadcast(moved, hurtEntities, deadIDs, s.sessions)
	if s.worldAge%20 == 0 {
		handler.DispatchWorldTime(s.worldAge, s.worldAge%24000, s.sessions)
		for _, change := range s.world.TickCrops(s.worldAge, 64) {
			handler.BroadcastBlockChange(change, s.sessions)
		}
	}
	if s.worldAge%200 == 0 {
		s.spawnPassiveMobsNearPlayers()
	}

	// Warn when the CPU work in a tick exceeds the tick budget.
	// Network I/O is off-goroutine and does not count toward this budget.
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		slog.Warn("entity tick overrun", "elapsed", elapsed)
	}
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

// spawnPassiveMobsNearPlayers provides a small, capped natural-spawn loop.
// It intentionally implements only common surface animals for now.
func (s *Server) spawnPassiveMobsNearPlayers() {
	animals := 0
	for _, e := range s.world.Entities.Snapshot() {
		if e.Type != corentity.TypeVillager && isPassiveMob(e.Type) {
			animals++
		}
	}
	if animals >= 24 {
		return
	}
	types := [...]corentity.EntityType{corentity.TypeCow, corentity.TypePig, corentity.TypeSheep, corentity.TypeChicken}
	s.game.OnlinePlayers(func(p *player.Player) {
		if animals >= 24 {
			return
		}
		angle := s.spawnRNG.Float64() * 2 * math.Pi
		distance := 12 + s.spawnRNG.Float64()*12
		x := p.Position.X + math.Cos(angle)*distance
		z := p.Position.Z + math.Sin(angle)*distance
		y := float64(s.world.SurfaceY(int(math.Floor(x)), int(math.Floor(z))) + 1)
		if !s.world.CanEntityOccupy(x, y, z) {
			return
		}
		e := corentity.New(s.game.NextEntityID(), newRandomUUID(), types[s.spawnRNG.Intn(len(types))], x, y, z)
		e.OnGround = true
		s.world.Entities.Add(e)
		handler.BroadcastSpawnMob(e, s.sessions)
		animals++
		slog.Info("passive mob spawned", "type", e.Type, "id", e.EntityID, "x", x, "y", y, "z", z)
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
		corentity.TypeDolphin, corentity.TypePolarBear,
		corentity.TypeIronGolem, corentity.TypeSnowGolem:
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
//
// Villagers are homed: they stay within 8 blocks of their spawn point.
// All other passive mobs roam freely, occasionally pausing.
func (s *Server) tickPassiveMobAI(e *corentity.Entity) {
	ai := s.mobAIFor(e)

	if ai.knockbackTick > 0 {
		ai.knockbackTick--
		e.Yaw = float32(math.Atan2(-ai.dirX, ai.dirZ) * 180 / math.Pi)
		return
	}

	if ai.panicTick > 0 {
		ai.panicTick--
		e.VX, e.VZ = ai.dirX*0.28, ai.dirZ*0.28
		e.Yaw = float32(math.Atan2(-ai.dirX, ai.dirZ) * 180 / math.Pi)
		return
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
				e.VX, e.VZ = 0, 0
				e.Sleeping = true
				return
			}
			e.Sleeping = false
			e.VX, e.VZ = dx/distance*0.1, dz/distance*0.1
			e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
			return
		}
		e.Sleeping = false
	}

	// While paused, hold still.
	if ai.pauseTick > 0 {
		ai.pauseTick--
		e.VX, e.VZ = 0, 0
		return
	}

	ai.wanderTick--

	if ai.wanderTick <= 0 {
		if ai.roaming {
			// Roaming mobs: 25 % chance to pause instead of moving.
			if ai.rng.Intn(4) == 0 {
				ai.pauseTick = 40 + ai.rng.Intn(60) // 2–5 s pause
				e.VX, e.VZ = 0, 0
				return
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

		if err := handler.HandlePlay(conn, p, s.world, s.chunkSender, s.sessions, s.cmds, s.regProvider, s.cfg.WorldSeed, int32(s.cfg.ViewDistance), int32(s.cfg.PreGenerateRadius)); err != nil {
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
	p.AttackCooldown = s.cfg.Combat.AttackCooldown
	// Spawn on the highest generated or loaded block at the world origin.
	p.Position.X = float64(s.spawnX) + 0.5
	p.Position.Y = float64(s.world.SurfaceY(s.spawnX, s.spawnZ) + 1)
	p.Position.Z = float64(s.spawnZ) + 0.5

	if err := s.game.AddPlayer(p); err != nil {
		// Duplicate UUID — extremely rare; log and continue with assigned ID.
		slog.Warn("duplicate player UUID", "name", p.Username, "uuid", p.UUID, "err", err)
	}

	// Update the status-ping online count.
	handler.OnlineCount.Store(int32(s.game.OnlineCount()))
	slog.Info("player joined", "name", p.Username, "uuid", p.UUID, "entityID", p.EntityID)
	return p
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

// Shutdown closes the listener immediately.
func (s *Server) Shutdown() error {
	if err := s.listener.Close(); err != nil {
		return fmt.Errorf("server: closing listener: %w", err)
	}
	return nil
}
