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
	"path/filepath"
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
	"GoCraft/core/blockloot"
	corentity "GoCraft/core/entity"
	coreexperience "GoCraft/core/experience"
	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/itemregistry"
	corepermission "GoCraft/core/permission"
	"GoCraft/core/player"
	coreplugin "GoCraft/core/plugin"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/customitems"
	"GoCraft/internal/debuglog"
	"GoCraft/java/auth"
	"GoCraft/java/handler"
	"GoCraft/java/network"
	"GoCraft/java/registry"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
	"GoCraft/java/world/anvil"

	bedrockpacket "github.com/sandertv/gophertunnel/minecraft/protocol/packet"
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

	loginHandler  *handler.LoginHandler
	statusFavicon string

	// World and Java encoding resources.
	world       *coreworld.World
	netherWorld *coreworld.World
	endWorld    *coreworld.World
	// simulationDimension is zero on the primary server and is overridden on
	// the short-lived per-dimension simulation view used by shared physics
	// helpers. Entity IDs and mutable registries remain shared.
	simulationDimension int32
	// bedrockActionWorld is set only while the sole simulation goroutine is
	// applying one Bedrock block intent, allowing shared action helpers to work
	// in all dimensions without changing their public shape.
	bedrockActionWorld *coreworld.World
	spawnX             int
	spawnZ             int
	spawnState         *worldSpawnState
	regProvider        registry.Provider
	chunkSender        *javaworld.Sender
	sessions           *session.Manager
	cmds               *handler.Dispatcher
	permissions        *corepermission.Manager
	permissionEditor   *permissionEditor

	// Custom item packs (nil when custom_items.enabled = false or no packs found).
	customItems *customitems.Manager

	// Bedrock adapter (nil when bedrock.enabled = false).
	bedrockListener *bedrock.Listener
	javaCrossKnown  map[[16]byte]map[[16]byte]crossPlayerView
	playerStore     *playerDataStore
	bedrockBlockUse map[[16]byte]bedrockRecentBlockUse

	// intentBus is the cross-adapter simulation command bus.
	// Both Java (M14.1+) and Bedrock handlers post intents here; the tick
	// goroutine drains and applies them once per tick.
	intentBus *intent.Bus

	// pluginRegistry owns the plugin runtimes and the loaded instances.
	// plugins is the event bus it exposes, handed down to the edition handlers
	// so they can emit native events without knowing about runtimes.
	pluginRegistry *coreplugin.Registry
	plugins        *coreplugin.Bus

	// pluginEffects holds what plugins asked the server to do. Writes never
	// originate from a plugin: a verdict carries them here and the tick applies
	// them, which is what keeps a handler on another thread from touching world
	// state while the simulation is reading it.
	pluginEffects *coreplugin.MutationQueue

	// commandTreeVersion is the registry version every connected client was
	// last told about. Compared once per tick, because both editions are told
	// their command list once and have no way to ask for it again.
	commandTreeVersion uint64

	// pendingJoins holds players who have joined but cannot yet be written to.
	// See announceJoinWhenReachable.
	pendingJoinsMu sync.Mutex
	pendingJoins   map[[16]byte]int

	// connCount tracks the number of active TCP connections (Java).
	connCount atomic.Int64

	// mobAIs tracks per-entity wander state indexed by entity ID.
	// Written and read only by the tick goroutine, so no lock is needed.
	mobAIs map[int32]*mobAI

	// worldAge is advanced only by the entity tick goroutine.
	worldAge                int64
	spawnRNG                *rand.Rand
	creaturePopulatedChunks map[[2]int32]struct{}
	furnaces                map[furnaceKey]*furnaceState
	brewingStands           map[brewingKey]*brewingState
	campfireCooking         map[campfireCookKey]int64

	// sleepAllTick is the worldAge tick at which ALL online players were first
	// detected sleeping.  0 means nobody is sleeping or the check hasn't fired.
	// The tick goroutine waits sleepAnimTicks before skipping the night.
	sleepAllTick int64

	// timings collects per-subsystem tick durations for /timings and /tps.
	timings         *tickTimings
	autosaveEnabled atomic.Bool
	difficulty      atomic.Int32
	defaultGameMode atomic.Uint32
	weather         atomic.Int32
	weatherTicks    atomic.Int64
	idleTimeout     atomic.Int64
	stopOnce        sync.Once
	stopRequested   chan struct{}
}

// mobAI holds the wander state for a passive mob.
// All fields are written only by the entity tick goroutine.
type mobAI struct {
	homeX, homeZ   float64    // world-space spawn/home position (homed mobs only)
	dirX, dirZ     float64    // current normalised walk direction
	wanderTick     int        // ticks until next direction pick (0 = pick now)
	panicTick      int        // remaining ticks fleeing from a recent attacker
	knockbackTick  int        // ticks retaining the configured initial hit velocity
	roaming        bool       // true = no fixed home (animals); false = homed (villagers)
	rng            *rand.Rand // per-entity PRNG seeded from entity ID
	sleepingWas    bool       // previous-tick sleeping state — detects transitions for metadata broadcast
	hasTarget      bool       // hostile AI: currently chasing a target
	targetX        float64    // hostile AI: current target world X
	targetZ        float64    // hostile AI: current target world Z
	targetEntityID int32
	attackCooldown int  // ticks until next melee swing
	bowDrawTicks   int  // ticks remaining before a skeleton releases its arrow
	fuseTick       int  // creeper fuse progress (30 ticks to detonation)
	angered        bool // enderman: true once provoked (by staring), stays true until target lost
	path           []spatial.Vec3
	pathIndex      int
	pathGoal       spatial.BlockPos
	hasPathGoal    bool
	repathTick     int
	wanderTarget   spatial.Vec3
	hasWanderGoal  bool
	lookTick       int
	lookX, lookZ   float64
	bedClaimTick   int // ticks until next unclaimed-bed scan (villagers only)
}

type crossPlayerView struct {
	player    *player.Player
	position  spatial.Vec3
	rotation  spatial.Rotation
	inventory [player.InventorySize]player.ItemStack
	heldSlot  int
	dead      bool
}

type bedrockRecentBlockUse struct {
	dimension int32
	position  spatial.BlockPos
	face      int32
	slot      int32
	at        time.Time
}

// New creates a Server with the given configuration.
// It initialises the game core and generates the RSA keypair for online-mode auth.
// The plugin drop directory is not created here. It was, against a hardcoded
// "plugins" that ignored plugins.directory: an admin who configured another
// name got an empty directory they never asked for and a scan somewhere else.
// loadPlugins creates the configured one through ScanBundles, and only when the
// subsystem is enabled.
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
	var storage, netherStorage, endStorage coreworld.Storage
	var playerStore *playerDataStore
	spawnX, spawnZ := 0, 0
	var initialWorldAge int64
	if cfg.WorldStorage == config.WorldStorageDisk {
		playerStore = newPlayerDataStore(filepath.Join(cfg.WorldDir, "playerdata"))
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
			debuglog.Info(debuglog.WorldLoading, "server: loaded Java level.dat",
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
		netherDirectory, endDirectory := dimensionWorldDirectories(cfg.WorldDir)
		if storageErr := prepareDimensionWorldDirectory(netherDirectory, filepath.Join(cfg.WorldDir, "DIM-1")); storageErr != nil {
			return nil, fmt.Errorf("server: preparing Nether directory: %w", storageErr)
		}
		if storageErr := prepareDimensionWorldDirectory(endDirectory, filepath.Join(cfg.WorldDir, "DIM1")); storageErr != nil {
			return nil, fmt.Errorf("server: preparing End directory: %w", storageErr)
		}
		netherStorageInstance, storageErr := anvil.NewStorage(netherDirectory)
		if storageErr != nil {
			return nil, fmt.Errorf("server: opening Nether storage: %w", storageErr)
		}
		endStorageInstance, storageErr := anvil.NewStorage(endDirectory)
		if storageErr != nil {
			return nil, fmt.Errorf("server: opening End storage: %w", storageErr)
		}
		netherStorage, endStorage = netherStorageInstance, endStorageInstance
		debuglog.Info(debuglog.WorldLoading, "server: opened Anvil world", "worldDir", cfg.WorldDir, "storage", "disk")
		slog.Info("dimension worlds ready",
			"overworld", cfg.WorldDir,
			"nether", netherDirectory,
			"end", endDirectory)
	} else {
		// In-memory world: resolve seed without persisting.
		cfg.WorldSeed = resolveWorldSeed(cfg.WorldSeed, "")
		debuglog.Info(debuglog.WorldLoading, "server: using memory-only world storage", "storage", "memory")
	}

	gameCore := game.New()
	if err := handler.ConfigureOperators(`ops.json`, cfg.Operators); err != nil {
		slog.Warn(`server: could not load ops.json`, `err`, err)
	}
	if err := handler.ConfigureWhitelist(`whitelist.json`, cfg.Whitelist.Enabled, cfg.Whitelist.Players); err != nil {
		slog.Warn(`server: could not load whitelist.json`, `err`, err)
	}
	if err := handler.ConfigureBans(`banned-players.json`, `banned-ips.json`); err != nil {
		return nil, fmt.Errorf("server: loading bans: %w", err)
	}
	permissionManager, err := corepermission.Load(`permissions.json`)
	if err != nil {
		return nil, fmt.Errorf("server: loading permissions: %w", err)
	}
	cmds := handler.NewDispatcher()
	cmds.SetPermissionChecker(func(p *player.Player, node string, defaultAllowed bool) bool {
		if p == nil {
			return false
		}
		return permissionManager.Allowed(p.Username, node, p.Operator, defaultAllowed)
	})
	cmds.SetGroupPrefixResolver(permissionManager.GroupPrefix)
	chatFmt, err := loadChatFormat("configuration/chatformat.yml")
	if err != nil {
		slog.Warn("could not load chat format, using default", "err", err)
		chatFmt = &chatFormatConfig{Format: defaultChatFormat}
	}
	if glyphs, glyphErr := loadGlyphs("configuration/glyphs.yml"); glyphErr != nil {
		slog.Warn("could not load glyphs", "err", glyphErr)
	} else {
		chatFmt.glyphs = glyphs
	}
	cmds.SetChatFormatter(chatFmt.apply)
	cmds.SetBedrockChatFormatter(chatFmt.applyBedrock)

	// Custom item packs — loaded before the Bedrock listener so that the
	// generated .mcaddon can be prepended to the pack list.
	var customItemsMgr *customitems.Manager
	if cfg.CustomItems.Enabled {
		mgr, err := customitems.Load(cfg.CustomItems.PacksDir)
		if err != nil {
			slog.Warn("customitems: failed to load packs, custom items disabled", "err", err)
		} else {
			customItemsMgr = mgr
			if !mgr.IsEmpty() {
				slog.Info("customitems: loaded", "count", len(mgr.Items()))
			}
		}
	}

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
	eventBudget := time.Duration(cfg.Plugins.EventBudgetMillis) * time.Millisecond
	// The queue is built here rather than left to the registry, because the
	// tick has to drain it and only something holding it can.
	pluginEffects := coreplugin.NewMutationQueue()
	pluginRegistry := coreplugin.NewRegistry(context.Background(), eventBudget, pluginEffects, nil)
	plugins := pluginRegistry.Bus()
	plugins.SetPermissionResolver(func(p *player.Player, node string) bool {
		return p != nil && permissionManager.Allowed(p.Username, node, p.Operator, false)
	})

	debuglog.Info(debuglog.WorldLoading, "server: world seed resolved", "seed", cfg.WorldSeed)
	worldInstance := coreworld.New(coreworld.NewOverworldGenerator(cfg.WorldSeed), storage, cfg.Villagers)
	netherWorld := coreworld.New(coreworld.NewNetherGenerator(cfg.WorldSeed^0x4e6574686572), netherStorage, false)
	endWorld := coreworld.New(coreworld.NewEndGenerator(cfg.WorldSeed^0x456e64), endStorage, false)
	netherWorld.SetLavaTickDelay(10)
	netherWorld.SetUltrawarm(true)
	worldInstance.SetMaxCachedChunks(cfg.MaxCachedChunks)
	netherWorld.SetMaxCachedChunks(cfg.MaxCachedChunks)
	endWorld.SetMaxCachedChunks(cfg.MaxCachedChunks)

	timings := newTickTimings(func() (int, int) {
		return gameCore.OnlineCount(), cfg.MaxPlayers
	})
	statusFavicon, iconErr := handler.LoadServerIcon(cfg.ServerIcon)
	if iconErr != nil {
		slog.Warn("server: could not load server-list icon", "path", cfg.ServerIcon, "err", iconErr)
	}

	s := &Server{
		cfg:                     cfg,
		game:                    gameCore,
		privKey:                 privKey,
		pubKeyDER:               pubKeyDER,
		statusFavicon:           statusFavicon,
		world:                   worldInstance,
		netherWorld:             netherWorld,
		endWorld:                endWorld,
		spawnX:                  spawnX,
		spawnZ:                  spawnZ,
		regProvider:             &registry.VanillaProvider{},
		chunkSender:             javaworld.DefaultSender,
		sessions:                session.NewManager(),
		cmds:                    cmds,
		permissions:             permissionManager,
		customItems:             customItemsMgr,
		intentBus:               bus,
		pluginRegistry:          pluginRegistry,
		pluginEffects:           pluginEffects,
		plugins:                 plugins,
		mobAIs:                  make(map[int32]*mobAI),
		spawnRNG:                rand.New(rand.NewSource(cfg.WorldSeed ^ 0x4d6f624372616674)),
		creaturePopulatedChunks: make(map[[2]int32]struct{}),
		worldAge:                initialWorldAge,
		furnaces:                make(map[furnaceKey]*furnaceState),
		campfireCooking:         make(map[campfireCookKey]int64),
		timings:                 timings,
		stopRequested:           make(chan struct{}),
		javaCrossKnown:          make(map[[16]byte]map[[16]byte]crossPlayerView),
		playerStore:             playerStore,
		bedrockBlockUse:         make(map[[16]byte]bedrockRecentBlockUse),
	}
	s.autosaveEnabled.Store(true)
	s.difficulty.Store(difficultyID(cfg.Difficulty) + 1)
	s.defaultGameMode.Store(uint32(configuredGameMode(cfg.DefaultGameMode)))
	s.spawnState = newWorldSpawnState(spatial.Vec3{
		X: float64(spawnX) + 0.5,
		Y: float64(s.safeSpawnY(spawnX, spawnZ)),
		Z: float64(spawnZ) + 0.5,
	})
	if cfg.WorldStorage == config.WorldStorageDisk {
		if savedSpawn, ok := loadSavedWorldSpawn(cfg.WorldDir); ok {
			s.spawnState.set(savedSpawn)
		}
	}
	// Installed once s exists, and before any listener opens: the registry is
	// empty until plugins load, so an early line simply finds nothing there.
	cmds.SetPluginCommands(s.runPluginCommand)
	cmds.SetCommandRegistry(pluginRegistry.Commands())

	s.registerSpawnCommands()
	s.registerLifecycleCommands()
	s.registerWorldSettingsCommands()
	s.registerMessagingCommands()
	s.registerRandomCommand()
	s.registerReloadCommand()
	s.registerIdleTimeoutCommand()

	// Register server-state commands as closures after s is initialised.
	cmds.Register("timings", func(ctx handler.CommandContext) error {
		report := timings.Report()
		if ctx.Reply != nil {
			return ctx.Reply(report)
		}
		return commandReply(ctx, report)
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
		return commandReply(ctx, message)
	})
	cmds.Register("mspt", func(ctx handler.CommandContext) error {
		return commandReply(ctx, timings.MSPT())
	})
	cmds.Register("time", func(ctx handler.CommandContext) error {
		if len(ctx.Args) == 0 {
			tod := s.worldAge % 24000
			return commandReply(ctx,
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
		return commandReply(ctx,
			fmt.Sprintf("Time set to %d", s.worldAge%24000))
	})
	cmds.RequireOperator(`timings`, `tps`, `mspt`, `time`)
	if cfg.PermissionEditor.Enabled {
		s.permissionEditor = newPermissionEditor(permissionManager, cfg.PermissionEditor.EditorURL, cfg.PermissionEditor.BytebinURL)
	}
	s.registerPermissionCommands()
	// Warm spawn immediately; login-time streaming will reuse this cache.
	s.world.QueuePregeneration(int32(math.Floor(float64(spawnX)/16)), int32(math.Floor(float64(spawnZ)/16)), int32(cfg.PreGenerateRadius))
	s.loginHandler = handler.NewLoginHandler(cfg, privKey, pubKeyDER)
	s.loginHandler.SetAdmissionCheck(admissionError)
	s.listener = network.NewListener(cfg.Addr(), s.handleConn)

	if cfg.Bedrock.Enabled {
		s.bedrockListener = bedrock.NewListener(
			cfg.Bedrock,
			bus,
			worldInstance,
			netherWorld,
			endWorld,
			gameCore,
			cfg.WorldSeed,
			spawnX,
			spawnZ,
			configuredGameMode(cfg.DefaultGameMode),
			difficultyID(cfg.Difficulty),
		)
		// The same tree the Java adapter renders. Installed here rather than
		// passed to the constructor because a listener without it behaves
		// exactly as this edition did before: it advertises nothing.
		s.bedrockListener.SetCommandTree(s.cmds.CommandTree)
		// Load manually-configured Bedrock packs from server.yml.
		if cfg.ResourcePack.Bedrock.Enabled && len(cfg.ResourcePack.Bedrock.Paths) > 0 {
			if packs, err := loadBedrockPacks(cfg.ResourcePack.Bedrock.Paths); err != nil {
				slog.Warn("could not load Bedrock resource packs", "err", err)
			} else {
				s.bedrockListener.SetResourcePacks(packs)
			}
		}
		// Inject auto-generated custom-item Bedrock pack and StartGame entries.
		if customItemsMgr != nil && !customItemsMgr.IsEmpty() {
			if addonData, err := customItemsMgr.BuildBedrockPack(); err != nil {
				slog.Warn("customitems: could not build Bedrock pack", "err", err)
			} else if addonData != nil {
				if pack, err := loadBedrockPackFromBytes(addonData); err != nil {
					slog.Warn("customitems: could not load Bedrock pack", "err", err)
				} else {
					s.bedrockListener.SetResourcePack(pack)
				}
			}
			s.bedrockListener.SetCustomItemEntries(customItemsMgr.BedrockItemEntries())
		}
		s.bedrockListener.SetWorldSpawn(s.currentWorldSpawn())
		s.sessions.SetMessageObserver(s.bedrockListener.BroadcastMessage)
		s.sessions.SetExternalKnockbackHandler(func(p *player.Player, sourceX, sourceZ, horizontal, vertical float64) {
			s.sendLegacyPlayerKnockback(&session.Session{Player: p}, sourceX, sourceZ, horizontal, vertical)
		})
	}
	for dimension, dimensionWorld := range map[int32]*coreworld.World{
		dimensionOverworld: worldInstance,
		dimensionNether:    netherWorld,
		dimensionEnd:       endWorld,
	} {
		dimension := dimension
		dimensionWorld := dimensionWorld
		var bedrockObserver func(coreworld.BlockChange)
		if s.bedrockListener != nil {
			bedrockObserver = s.bedrockListener.DimensionBlockObserver(dimension)
		}
		dimensionWorld.SetBlockObserver(func(change coreworld.BlockChange) {
			if bedrockObserver != nil {
				bedrockObserver(change)
			}
			handler.BroadcastBlockLightUpdatesInDimension(dimensionWorld, change, s.sessions, dimension)
		})
		dimensionWorld.SetBlockEntityObserver(func(entity coreworld.BlockEntity) {
			handler.BroadcastBlockEntityDataInDimension(entity, s.sessions, dimension)
			if s.bedrockListener != nil {
				s.bedrockListener.BroadcastBlockEntityData(dimension, entity)
			}
		})
	}
	cmds.SetPlayerTeleporter(s.teleportPlayer)
	cmds.SetPlayerDisconnector(s.disconnectPlayer)
	// The command context used to carry a *network.ClientConn, which a Bedrock
	// player does not have, so roughly twenty commands ran and told that player
	// nothing. These three bridges are what replaced it: only the server sees
	// both adapters, so only the server can write them.
	cmds.SetMessenger(s.sendPlayerMessage)
	cmds.SetLinkMessenger(s.sendPlayerLink)
	cmds.SetAbilitySync(s.syncPlayerAbilities)
	cmds.SetStatusEffectSync(s.syncPlayerStatusEffect)
	// Registered here rather than beside the registry, because a runtime that
	// can come back from a crash needs to tell the server it did — and only the
	// server knows who is online to replay it to.
	if err := s.registerPluginRuntimes(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// teleportPlayer routes a command teleport through the adapter that owns the
// target player. This keeps Java chunk tracking and Bedrock authoritative
// movement state in sync for cross-edition commands.
func (s *Server) teleportPlayer(target *player.Player, x, y, z float64) error {
	if target == nil {
		return fmt.Errorf("target player is unavailable")
	}
	position := spatial.Vec3{X: x, Y: y, Z: z}
	switch target.Edition {
	case player.ClientEditionJava:
		targetSession, ok := s.sessions.Get(target.UUID)
		if !ok || targetSession.TeleportTo == nil {
			return fmt.Errorf("Java player session is unavailable")
		}
		return targetSession.TeleportTo(x, y, z)
	case player.ClientEditionBedrock:
		if s.bedrockListener == nil {
			return fmt.Errorf("Bedrock player session is unavailable")
		}
		target.Position = position
		target.FallDistance = 0
		target.OnGround = false
		s.bedrockListener.TeleportPlayer(target, position, uint64(s.worldAge))
		return nil
	default:
		return fmt.Errorf("unsupported player edition")
	}
}

// Run starts the server and blocks until ctx is cancelled or a fatal error occurs.
// All background goroutines are tracked with a WaitGroup and are joined before
// the world is flushed to disk, ensuring clean shutdown of both listeners.
func (s *Server) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stopRequested:
			cancel()
		case <-runCtx.Done():
		}
	}()
	ctx = runCtx
	go s.runConsole(ctx)
	if err := s.loadPlugins(ctx); err != nil {
		slog.Error("plugins: startup aborted", "err", err)
		return err
	}
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
	// Start the Java custom-item pack HTTP server if we have items and Java is enabled.
	if s.cfg.JavaEnabled && s.customItems != nil && !s.customItems.IsEmpty() {
		if packData, hash, err := s.customItems.BuildJavaPack(); err != nil {
			slog.Warn("customitems: could not build Java pack", "err", err)
		} else {
			port := s.cfg.CustomItems.Java.ServePort
			if port == 0 {
				port = 8080
			}
			host := s.cfg.CustomItems.Java.PublicHost
			if ps, err := customitems.StartJavaPackServer(packData, hash, host, port); err != nil {
				slog.Warn("customitems: could not start Java pack server", "err", err)
			} else {
				// Override the Java resource pack config with the auto-generated URL/hash.
				s.cfg.ResourcePack.Java.Enabled = true
				s.cfg.ResourcePack.Java.URL = ps.URL()
				s.cfg.ResourcePack.Java.Hash = ps.HashHex()
				slog.Info("customitems: Java pack ready", "url", ps.URL())
			}
		}
	}

	slog.Info("starting GoCraft server", "motd", s.cfg.MOTD, "worldSeed", s.cfg.WorldSeed,
		"worldStorage", s.cfg.WorldStorage, "maxCachedChunks", s.cfg.MaxCachedChunks)

	// pprof profiling endpoint — http://localhost:6060/debug/pprof/
	// Use: go tool pprof http://localhost:6060/debug/pprof/goroutine
	//      go tool pprof http://localhost:6060/debug/pprof/heap
	go func() {
		pprofAddr := "localhost:6060"
		debuglog.Info(debuglog.Profiling, "pprof profiling server listening", "addr", pprofAddr)
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
	cancel()

	// ctx is now done: wait for entity tick and Bedrock listener to finish.
	wg.Wait()

	// Unload plugins while world storage is still open, so a runtime that
	// persists on shutdown can still write.
	s.unloadPlugins()

	// Flush world to disk regardless of shutdown cause.
	s.saveAllPlayerData()
	for dimension, dimensionWorld := range map[string]*coreworld.World{"overworld": s.world, "nether": s.netherWorld, "end": s.endWorld} {
		if closeErr := dimensionWorld.Close(); closeErr != nil {
			slog.Warn("server: error flushing world on shutdown", "dimension", dimension, "err", closeErr)
		}
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
	case `mspt`:
		if s.timings == nil {
			return `Timing collector is unavailable`
		}
		return stripMinecraftFormatting(s.timings.MSPT())
	case `gocraft`:
		message, err := s.executePermissionCommand(fields[1:])
		if err != nil {
			return `Error: ` + err.Error()
		}
		return message
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
	s.tickBedrockItemUse()
	s.tickJavaItemUse()
	s.tickFurnaces()
	s.tickBrewingStands()
	s.tickContainerAutomation()
	s.tickEntities()
	s.tickAuxiliaryDimensionItems()
	s.tickStationaryLavaDamage()
	s.tickPlayerBreathing()
	s.tickPlayerStatusEffects()
	s.tickPlayerHunger()
	s.tickIdleTimeout()
	s.tickWeather()
	if s.bedrockListener != nil {
		s.bedrockListener.Sync(uint64(s.worldAge))
		s.syncBedrockPlayersToJava()
	}
	if s.autosaveEnabled.Load() && s.worldAge%600 == 0 {
		for dimension, dimensionWorld := range map[string]*coreworld.World{"overworld": s.world, "nether": s.netherWorld, "end": s.endWorld} {
			if err := dimensionWorld.Flush(); err != nil {
				slog.Warn("world autosave failed", "dimension", dimension, "err", err)
			}
		}
		s.saveWorldAge()
		s.saveAllPlayerData()
	}
}

// tickStationaryLavaDamage keeps fluid collision authoritative even when a
// client sends no movement packet while standing in lava.
func (s *Server) tickStationaryLavaDamage() {
	if s.game == nil {
		return
	}
	now := time.Now()
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.Dead || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator ||
			now.Sub(p.LastEnvironmentDamage) < 500*time.Millisecond {
			return
		}
		world := s.worldForPlayer(p)
		x, y, z := int(math.Floor(p.Position.X)), int(math.Floor(p.Position.Y)), int(math.Floor(p.Position.Z))
		if world == nil || world.GetBlock(x, y, z).ResourceLocation() != "minecraft:lava" {
			return
		}
		p.LastEnvironmentDamage = now
		target := &session.Session{Player: p}
		if p.Edition == player.ClientEditionJava {
			if current, ok := s.sessions.Get(p.UUID); ok {
				target = current
			}
		}
		handler.DamagePlayer(target, 4, "tried to swim in lava", s.sessions)
	})
}

// tickPlayerHunger applies natural regeneration and starvation to both
// editions. Movement exhaustion is accumulated by each movement adapter.
func (s *Server) tickPlayerHunger() {
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.GameMode != player.GameModeSurvival || p.Dead {
			return
		}
		target := &session.Session{Player: p}
		if p.Edition == player.ClientEditionJava {
			if javaSession, ok := s.sessions.Get(p.UUID); ok && javaSession != nil {
				target = javaSession
			}
		}
		health, food, saturation, _ := p.HealthSnapshot()
		fastRegeneration := food == 20 && saturation > 0 && s.worldAge%10 == 0
		slowRegeneration := food >= 18 && (!fastRegeneration && s.worldAge%80 == 0)
		if health < p.MaxHealth && (fastRegeneration || slowRegeneration) {
			if p.Heal(1) {
				p.AddExhaustion(6)
				if p.Edition == player.ClientEditionJava {
					_ = handler.SyncPlayerHealth(target.Conn, p)
				}
			}
			return
		}
		if food == 0 && s.worldAge%80 == 0 {
			handler.DamagePlayer(target, 1, "starved to death", s.sessions)
		}
	})
}

// tickIntents drains the intent bus and applies each intent to world/player state.
// This is the sole point of mutating player state from adapter goroutines.
func (s *Server) tickIntents() {
	s.announceReachableJoins()
	s.applyPluginEffects()
	s.resendChangedCommands()
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
		case intent.BellRingIntent:
			s.applyBellRing(i)
		case intent.FireworkUseIntent:
			s.applyFireworkUse(i)
		case intent.ConsumeFoodIntent:
			s.applyBedrockConsumeFood(i)
		case intent.StartUseItemIntent:
			s.applyBedrockStartUseItem(i)
		case intent.ArmSwingIntent:
			s.applyArmSwing(i)
		case intent.EntityInteractIntent:
			s.applyEntityInteract(i)
		case intent.VehicleMoveIntent:
			s.applyVehicleMove(i)
		case intent.RespawnIntent:
			s.applyBedrockRespawn(i)
		case intent.WakeIntent:
			if p := s.game.GetPlayer(i.PlayerUUID); p != nil && p.Sleeping {
				p.Sleeping = false
				handler.BroadcastPlayerWaking(p.EntityID, s.sessions)
			}
		case intent.HotbarIntent:
			if p := s.game.GetPlayer(i.PlayerUUID); p != nil && i.Slot >= 0 && i.Slot < 9 {
				slog.Debug("bedrock hotbar selection applied",
					"packet_type", "HotbarIntent", "incoming_slot", i.Slot, "current_server_slot", p.HeldSlot, "outgoing_slot", "none")
				p.HeldSlot = int(i.Slot)
			}
		case intent.PlayerStateIntent:
			s.applyBedrockPlayerState(i)
		case intent.InventoryIntent:
			s.applyBedrockInventory(i)
		case intent.ContainerCloseIntent:
			s.applyBedrockContainerClose(i)
		case intent.FurnaceOutputTakenIntent:
			if p := s.game.GetPlayer(i.PlayerUUID); p != nil {
				s.awardFurnaceExperienceAt(p, i.Dimension, i.Position)
			}
		}
	}
}

func (s *Server) applyArmSwing(i intent.ArmSwingIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Dead || (i.Hand != 0 && i.Hand != 1) {
		return
	}
	handler.BroadcastPlayerArmSwing(p, i.Hand, s.sessions)
	if s.bedrockListener != nil {
		s.bedrockListener.BroadcastPlayerArmSwing(p)
	}
}

func (s *Server) applyBedrockContainerClose(i intent.ContainerCloseIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock {
		return
	}
	if p.OpenContainerKind == "minecraft:crafting_table" {
		for slot := range p.CraftingGrid {
			if p.CraftingGrid[slot].IsEmpty() || !p.GiveItem(p.CraftingGrid[slot]) {
				continue
			}
			p.CraftingGrid[slot] = player.ItemStack{}
		}
		p.CraftingResult = handler.FindBedrockCraftingTableResult(p.CraftingGrid)
	}
	if handler.IsFurnaceContainer(p.OpenContainerKind) {
		persistFurnaceSlots(s.worldForPlayer(p), p.OpenContainerPos, p.OpenContainerKind, p.ContainerSlots)
	} else if isBedrockGenericContainer(p.OpenContainerKind) {
		s.persistBedrockGenericContainer(p)
	} else if isBedrockWorkstation(p.OpenContainerKind) {
		s.returnBedrockWorkstationItems(p)
	}
	p.ContainerSlots = nil
	p.OpenContainerPos = spatial.BlockPos{}
	p.OpenContainerPartnerPos = spatial.BlockPos{}
	p.OpenContainerHasPartner = false
	p.OpenContainerID = 0
	p.OpenContainerKind = ""
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
	case intent.PlayerStateSneaking:
		p.Sneaking = i.Enabled
		if i.Enabled && p.VehicleEntityID != 0 {
			s.dismountPlayer(p)
		}
	}
}

// applyJoin creates a canonical Player, registers it with the game core, and
// sends a JoinResult to the waiting adapter goroutine.
func (s *Server) applyJoin(i intent.JoinIntent) {
	if err := admissionError(i.Username, i.RemoteAddress); err != nil {
		slog.Warn("applyJoin: player rejected", "name", i.Username, "edition", i.Edition, "err", err)
		i.Done <- intent.JoinResult{Err: err}
		return
	}
	edition := player.ClientEditionBedrock
	if i.Edition == "java" {
		edition = player.ClientEditionJava
	}

	p := player.New(i.PlayerUUID, i.Username, edition)
	p.RemoteAddress = i.RemoteAddress
	p.Raining, p.Thundering = s.currentWeather()
	p.Operator = handler.IsOperatorName(i.Username)
	p.InvulnerableUntil = time.Now().Add(3 * time.Second)
	p.GameMode = player.GameMode(s.defaultGameMode.Load())
	p.AttackCooldown = s.cfg.Combat.AttackCooldown
	p.KnockbackHorizontal = s.cfg.Combat.KnockbackHorizontal
	p.KnockbackVertical = s.cfg.Combat.KnockbackVertical
	p.OnDeath = s.dropPlayerInventory
	p.Position = s.currentWorldSpawn()
	p.WorldSpawn = p.Position
	s.loadPlayerData(p)
	s.ensurePlayerPositionClear(p)
	if p.Dimension != dimensionOverworld {
		p.InvulnerableUntil = time.Now().Add(10 * time.Second)
	}
	if err := s.game.AddPlayer(p); err != nil {
		slog.Warn("applyJoin: duplicate player UUID",
			"name", i.Username, "uuid", i.PlayerUUID, "err", err)
		i.Done <- intent.JoinResult{Err: err}
		return
	}

	handler.OnlineCount.Store(int32(s.game.OnlineCount()))
	s.announceJoinWhenReachable(p)
	slog.Info("player joined via intent",
		"name", p.Username, "uuid", p.UUID,
		"edition", i.Edition, "trusted", i.TrustedIdentity,
		"entityID", p.EntityID)

	i.Done <- intent.JoinResult{
		EntityID:  p.EntityID,
		Position:  p.Position,
		Dimension: p.Dimension,
	}
}

// safeSpawnY returns the Y coordinate where a player should spawn at (x, z).
// It starts at the highest non-air block, then scans upward past fluid blocks
// (water, lava) so the player is not placed inside a liquid column.
func (s *Server) safeSpawnY(x, z int) int {
	y := s.world.SurfaceY(x, z) + 1
	for y <= coreworld.WorldMaxY {
		loc := s.world.GetBlock(x, y, z).ResourceLocation()
		if loc != "minecraft:water" && loc != "minecraft:lava" {
			break
		}
		y++
	}
	return y
}

func (s *Server) worldForDimension(dimension int32) *coreworld.World {
	switch dimension {
	case 1:
		if s.netherWorld != nil {
			return s.netherWorld
		}
	case 2:
		if s.endWorld != nil {
			return s.endWorld
		}
	}
	return s.world
}

func (s *Server) worldForPlayer(p *player.Player) *coreworld.World {
	if p == nil {
		return s.world
	}
	return s.worldForDimension(p.Dimension)
}

func (s *Server) bedrockWorld() *coreworld.World {
	if s.bedrockActionWorld != nil {
		return s.bedrockActionWorld
	}
	return s.world
}

// applyDisconnect removes a player from the game core and logs the event.
func (s *Server) applyDisconnect(i intent.DisconnectIntent) {
	if p := s.game.GetPlayer(i.PlayerUUID); p != nil {
		if p.VehicleEntityID != 0 {
			s.dismountPlayer(p)
		}
		if p.Edition == player.ClientEditionBedrock && p.OpenContainerKind != "" {
			s.applyBedrockContainerClose(intent.ContainerCloseIntent{PlayerUUID: p.UUID, WindowID: byte(p.OpenContainerID)})
		}
		s.savePlayerData(p)
	}
	s.game.RemovePlayer(i.PlayerUUID)
	delete(s.bedrockBlockUse, i.PlayerUUID)
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
	previousPosition, previousOnGround := p.Position, p.OnGround
	p.Position = m.Position
	p.Rotation = m.Rotation
	p.OnGround = m.OnGround
	if p.RecordMovementVibration() {
		if movementWorld := s.worldForPlayer(p); movementWorld != nil {
			movementWorld.EmitVibration(int(math.Floor(p.Position.X)), int(math.Floor(p.Position.Y)), int(math.Floor(p.Position.Z)))
		}
	}
	if p.VehicleEntityID != 0 {
		if vehicle, ok := s.worldForPlayer(p).Entities.Get(p.VehicleEntityID); ok && corentity.IsRideableVehicle(vehicle.Type) && vehicle.RiderEntityID == p.EntityID {
			vehicle.Position.X = m.Position.X
			vehicle.Position.Z = m.Position.Z
			vehicle.Yaw = m.Rotation.Yaw
			handler.BroadcastEntityPosition(vehicle, s.sessions)
		}
	}
	if p.Edition == player.ClientEditionBedrock {
		if p.Sprinting && !p.Flying && p.GameMode == player.GameModeSurvival {
			distance := math.Hypot(p.Position.X-previousPosition.X, p.Position.Z-previousPosition.Z)
			// Reject teleport-sized deltas: teleports are not physical exertion.
			if distance <= 10 {
				p.AddExhaustion(float32(distance * 0.1))
			}
		}
		s.applyBedrockMovementDamage(p, previousPosition, previousOnGround)
		s.tryBedrockPortalTravel(p)
	}
}

func (s *Server) applyBedrockConsumeFood(i intent.ConsumeFoodIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock || p.GameMode == player.GameModeSpectator {
		return
	}
	if i.HotbarSlot < 0 || i.HotbarSlot >= 9 {
		return
	}
	stack := p.Inventory[player.HotbarStart+int(i.HotbarSlot)]
	started := p.UsingItemSlot == int(i.HotbarSlot) && p.UsingItemID == stack.ItemID && !p.UsingItemSince.IsZero()
	if !started || time.Since(p.UsingItemSince) < player.FoodUseDuration(stack.ItemID) {
		s.clearBedrockItemUse(p, false)
		return
	}
	s.finishBedrockFoodUse(p)
}

func (s *Server) applyBedrockStartUseItem(i intent.StartUseItemIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock || p.GameMode == player.GameModeSpectator || p.Dead {
		return
	}
	hotbar := int(i.HotbarSlot)
	if hotbar < 0 {
		hotbar = p.HeldSlot
	}
	if hotbar < 0 || hotbar >= 9 {
		return
	}
	stack := p.Inventory[player.HotbarStart+hotbar]
	previousHeldSlot := p.HeldSlot
	p.HeldSlot = hotbar
	if handler.UseThrowable(p, s.worldForPlayer(p), s.sessions, nil, s.game.NextEntityID) {
		p.HeldSlot = previousHeldSlot
		return
	}
	if stack.ItemID == "minecraft:ender_eye" {
		handler.UseEnderEye(p, s.worldForPlayer(p), s.sessions, s.game.NextEntityID)
		p.HeldSlot = previousHeldSlot
		return
	}
	p.HeldSlot = previousHeldSlot
	if stack.ItemID == "minecraft:goat_horn" {
		const goatHornCooldown = 7 * time.Second
		if !p.LastGoatHornUse.IsZero() && time.Since(p.LastGoatHornUse) < goatHornCooldown {
			return
		}
		p.LastGoatHornUse = time.Now()
		sound := handler.GoatHornSound(stack)
		handler.BroadcastSoundAt(s.sessions, sound, handler.SoundCategoryNeutral,
			p.Position.X, p.Position.Y+1.62, p.Position.Z, 64, 1)
		return
	}
	if stack.ItemID == "minecraft:spyglass" {
		p.UsingItemID = "minecraft:spyglass"
		p.UsingItemSince = time.Now()
		return
	}
	if (stack.ItemID == "minecraft:carrot_on_a_stick" || stack.ItemID == "minecraft:warped_fungus_on_a_stick") &&
		p.VehicleEntityID != 0 && p.GameMode != player.GameModeCreative {
		s.damageBedrockHeldItem(p, 7)
		return
	}
	if stack.ItemID == "minecraft:wind_charge" {
		previousHeldSlot := p.HeldSlot
		p.HeldSlot = hotbar
		used := handler.UseWindCharge(p, s.worldForPlayer(p), s.sessions, nil, s.game.NextEntityID)
		p.HeldSlot = previousHeldSlot
		if used && s.bedrockListener != nil {
			s.bedrockListener.BroadcastWindChargeSound(spatial.Vec3{X: p.Position.X, Y: p.Position.Y + 1.52, Z: p.Position.Z}, false)
		}
		return
	}
	_, _, food := player.FoodValue(stack.ItemID)
	if stack.IsEmpty() || !food && !player.IsConsumable(stack.ItemID) {
		return
	}
	_, hunger, _, _ := p.HealthSnapshot()
	if p.GameMode != player.GameModeCreative && food && hunger >= 20 && !player.CanAlwaysEat(stack.ItemID) {
		return
	}
	if p.UsingItemID != stack.ItemID || p.UsingItemSlot != hotbar || p.UsingItemSince.IsZero() {
		p.UsingItemID = stack.ItemID
		p.UsingItemSlot = hotbar
		p.UsingItemSince = time.Now()
	}
	if s.bedrockListener != nil {
		s.bedrockListener.BroadcastPlayerUsingItemState(p, false)
	}
}

// tickBedrockItemUse completes consumables server-side when their vanilla use
// duration expires. Pumpkin follows this model too: ReleaseItem cancels an
// early use, but a client is not required to send a second transaction for a
// successfully completed eating animation.
func (s *Server) tickBedrockItemUse() {
	if s.game == nil {
		return
	}
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.Edition != player.ClientEditionBedrock || p.UsingItemID == "" || p.UsingItemSince.IsZero() {
			return
		}
		if p.UsingItemSlot < 0 || p.UsingItemSlot >= 9 {
			s.clearBedrockItemUse(p, false)
			return
		}
		stack := p.Inventory[player.HotbarStart+p.UsingItemSlot]
		if stack.IsEmpty() || stack.ItemID != p.UsingItemID {
			s.clearBedrockItemUse(p, false)
			return
		}
		if time.Since(p.UsingItemSince) >= player.FoodUseDuration(stack.ItemID) {
			s.finishBedrockFoodUse(p)
		}
	})
}

func (s *Server) tickJavaItemUse() {
	if s.game == nil {
		return
	}
	now := time.Now()
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.Edition != player.ClientEditionJava || p.UsingItemID == "" || p.UsingItemSince.IsZero() {
			return
		}
		current, ok := s.sessions.Get(p.UUID)
		if !ok || current == nil {
			return
		}
		handler.TickJavaFoodUse(p, current.Conn, s.sessions, now)
	})
}

func (s *Server) finishBedrockFoodUse(p *player.Player) {
	if p == nil || p.UsingItemSlot < 0 || p.UsingItemSlot >= 9 {
		return
	}
	slot := player.HotbarStart + p.UsingItemSlot
	stack := p.Inventory[slot]
	nutrition, saturation, food := player.FoodValue(stack.ItemID)
	if stack.IsEmpty() || stack.ItemID != p.UsingItemID || !food && !player.IsConsumable(stack.ItemID) {
		s.clearBedrockItemUse(p, false)
		return
	}
	consumedID := stack.ItemID
	if p.GameMode != player.GameModeCreative {
		if food && !p.ConsumeFoodAllowFull(nutrition, saturation, player.CanAlwaysEat(stack.ItemID)) {
			s.clearBedrockItemUse(p, false)
			return
		}
		p.Inventory[slot].Count--
		if p.Inventory[slot].Count <= 0 {
			p.Inventory[slot] = player.ItemStack{}
		}
		if remainder := bedrockFoodRemainder(consumedID); remainder != "" {
			if p.Inventory[slot].IsEmpty() {
				p.Inventory[slot] = player.ItemStack{ItemID: remainder, Count: 1}
			} else {
				p.GiveItem(player.ItemStack{ItemID: remainder, Count: 1})
			}
		}
	}
	s.applyBedrockConsumableEffects(p, stack)
	for _, removed := range p.ApplyConsumableCleansing(consumedID) {
		if s.bedrockListener != nil {
			s.bedrockListener.RemovePlayerMobEffect(p, bedrockEffectType(removed.ID))
		}
	}
	s.clearBedrockItemUse(p, true)
}

func (s *Server) clearBedrockItemUse(p *player.Player, completed bool) {
	if p == nil {
		return
	}
	p.UsingItemID = ""
	p.UsingItemSince = time.Time{}
	p.UsingItemSlot = -1
	if s.bedrockListener != nil {
		s.bedrockListener.BroadcastPlayerUsingItemState(p, completed)
	}
}

func bedrockFoodRemainder(itemID string) string {
	return player.FoodRemainder(itemID)
}

// allPlayerSessions adapts every canonical online player to the Java combat
// helper's lightweight Session shape. Bedrock players intentionally have a nil
// Conn: health/death remains canonical and their adapter publishes it on Sync.
func (s *Server) allPlayerSessions() []*session.Session {
	out := make([]*session.Session, 0, s.game.OnlineCount())
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.Dimension != s.simulationDimension {
			return
		}
		if javaSession, ok := s.sessions.Get(p.UUID); ok {
			out = append(out, javaSession)
			return
		}
		out = append(out, &session.Session{Player: p})
	})
	return out
}

func (s *Server) javaSessionsForDimension(dimension int32) *session.Manager {
	filtered := session.NewManager()
	if s == nil || s.sessions == nil {
		return filtered
	}
	for _, current := range s.sessions.SnapshotAll() {
		if current != nil && current.Player != nil && current.Player.Dimension == dimension {
			filtered.Add(current)
		}
	}
	return filtered
}

// dimensionSimulation returns a lightweight view over one canonical dimension.
// Fields containing locks/atomics are deliberately not copied; mutable maps and
// allocators that are owned by the single simulation goroutine stay shared.
func (s *Server) dimensionSimulation(dimension int32, dimensionWorld *coreworld.World) *Server {
	return &Server{
		cfg: s.cfg, game: s.game,
		world: dimensionWorld, netherWorld: s.netherWorld, endWorld: s.endWorld,
		simulationDimension: dimension,
		spawnX:              s.spawnX,
		spawnZ:              s.spawnZ,
		spawnState:          s.spawnState,
		regProvider:         s.regProvider,
		chunkSender:         s.chunkSender,
		sessions:            s.javaSessionsForDimension(dimension),
		bedrockListener:     s.bedrockListener,
		mobAIs:              s.mobAIs,
		worldAge:            s.worldAge,
		spawnRNG:            s.spawnRNG,
		furnaces:            s.furnaces,
	}
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

func (s *Server) applyBedrockMovementDamage(p *player.Player, previousPosition spatial.Vec3, previousOnGround bool) {
	if p == nil || p.Dead || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator || p.Flying {
		if p != nil {
			p.FallDistance = 0
		}
		return
	}
	target := &session.Session{Player: p}
	playerWorld := s.worldForPlayer(p)
	if playerWorld != nil && playerWorld.TouchesWater(p.Position.X, p.Position.Y, p.Position.Z) {
		p.FallDistance = 0
	} else if !p.OnGround {
		if drop := previousPosition.Y - p.Position.Y; drop > 0 {
			p.FallDistance += drop
		}
	} else if !previousOnGround {
		fallDistance := p.FallDistance
		p.FallDistance = 0
		safeHeight := 3.0 + float64(p.Inventory[8].EnchantmentLevel("minecraft:feather_falling"))*3.0
		if damage := float32(math.Floor(fallDistance - safeHeight)); damage > 0 {
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
	feetBlock := playerWorld.GetBlock(x, y, z)
	feet := feetBlock.ResourceLocation()
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
	case "minecraft:cactus":
		p.LastEnvironmentDamage = now
		handler.DamagePlayer(target, 1, "was pricked to death", s.sessions)
	case "minecraft:sweet_berry_bush":
		if coreworld.CropAge(feetBlock) > 0 &&
			(math.Abs(p.Position.X-previousPosition.X) >= 0.003 || math.Abs(p.Position.Z-previousPosition.Z) >= 0.003) {
			p.LastEnvironmentDamage = now
			handler.DamagePlayer(target, 1, "was pricked to death", s.sessions)
		}
	}
}

func (s *Server) tickPlayerBreathing() {
	if s.game == nil {
		return
	}
	s.game.OnlinePlayers(func(p *player.Player) {
		world := s.worldForPlayer(p)
		underwater := false
		if world != nil {
			x := int(math.Floor(p.Position.X))
			y := int(math.Floor(p.Position.Y + 1.62))
			z := int(math.Floor(p.Position.Z))
			head := world.GetBlock(x, y, z).ResourceLocation()
			underwater = head == "minecraft:water" || head == "minecraft:bubble_column"
		}
		_, changed, drown := p.TickBreathing(underwater)
		if changed {
			handler.SyncPlayerAirSupply(p, s.sessions)
		}
		if drown {
			target := &session.Session{Player: p}
			if current, ok := s.sessions.Get(p.UUID); ok {
				target = current
			}
			handler.DamagePlayer(target, 2, "drowned", s.sessions)
		}
	})
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
			World:   s.worldForPlayer(p),
			Manager: s.sessions,
		}
		// Reply and SyncAbilities are not set here. Dispatch fills them from
		// the bridges, which route by edition, so this path and the Java one in
		// chat.go now answer through the same code — the two used to diverge,
		// which is how commands ended up working on one edition only.
		//
		// Teleport and dimension change stay: they move this player through
		// this adapter, which is not something a shared bridge can do.
		if s.bedrockListener != nil && p.Edition == player.ClientEditionBedrock {
			ctx.TeleportTo = func(x, y, z float64) error {
				p.Position = spatial.Vec3{X: x, Y: y, Z: z}
				p.FallDistance = 0
				s.bedrockListener.TeleportPlayer(p, p.Position, uint64(s.worldAge))
				return nil
			}
			ctx.ChangeWorld = func(dimension int32) error {
				if dimension < dimensionOverworld || dimension > dimensionEnd {
					return fmt.Errorf("invalid dimension %d", dimension)
				}
				destinationWorld := s.worldForDimension(dimension)
				target := destinationWorld.EnsureSafeArrival(s.commandWorldTarget(p, dimension), dimension)
				destinationCX, destinationCZ := coreworld.ChunkCoordsFor(int(math.Floor(target.X)), int(math.Floor(target.Z)))
				destinationWorld.QueuePregeneration(destinationCX, destinationCZ, min(int32(s.cfg.PreGenerateRadius), 2))
				p.InvulnerableUntil = time.Now().Add(10 * time.Second)
				p.Dimension = dimension
				p.Position = target
				p.FallDistance = 0
				p.OnGround = false
				s.bedrockListener.ChangeDimension(p, dimension, target)
				return nil
			}
		}
		s.cmds.Dispatch(i.Message, ctx)
		return
	}
	// Java and Bedrock receive separately formatted strings: Java supports
	// §x hex colors and gradients; Bedrock only supports basic §-codes.
	// Use the Java-only broadcast so the Bedrock observer is not triggered
	// with the Java-formatted (gradient) string.
	javaMsg := s.cmds.FormatChat(i.DisplayName, i.Message)
	handler.BroadcastSystemMessageJavaOnly(s.sessions, javaMsg)
	if s.bedrockListener != nil {
		bedrockMsg := s.cmds.FormatBedrockChat(i.DisplayName, i.Message)
		s.bedrockListener.BroadcastMessage(bedrockMsg)
	}
}

func (s *Server) applyBedrockBlockInteract(i intent.BlockInteractIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Edition != player.ClientEditionBedrock || p.GameMode == player.GameModeSpectator {
		return
	}
	actionWorld := s.worldForPlayer(p)
	previousActionWorld := s.bedrockActionWorld
	s.bedrockActionWorld = actionWorld
	defer func() { s.bedrockActionWorld = previousActionWorld }()
	previousHeldSlot := p.HeldSlot
	if i.HotbarSlot >= 0 && i.HotbarSlot < 9 && int(i.HotbarSlot) != previousHeldSlot {
		p.HeldSlot = int(i.HotbarSlot)
		defer func() { p.HeldSlot = previousHeldSlot }()
	}
	center := spatial.Vec3{X: float64(i.Position.X) + 0.5, Y: float64(i.Position.Y) + 0.5, Z: float64(i.Position.Z) + 0.5}
	if p.Position.Distance(center) > 6.5 {
		return
	}
	if i.Action == intent.BlockActionUse && s.duplicateBedrockBlockUse(p, i) {
		return
	}

	x, y, z := int(i.Position.X), int(i.Position.Y), int(i.Position.Z)
	switch i.Action {
	case intent.BlockActionBreak:
		block := actionWorld.GetBlock(x, y, z)
		if block.IsAir() || block.ResourceLocation() == "minecraft:bedrock" {
			return
		}
		if block.ResourceLocation() == "minecraft:dragon_egg" && p.GameMode != player.GameModeCreative {
			s.bedrockDragonEggTeleport(actionWorld, x, y, z)
			return
		}
		held := p.HeldItem()
		if s.plugins != nil && !s.plugins.EmitBlockBreak(p, i.Position, block, held.ItemID) {
			if s.bedrockListener != nil {
				s.bedrockListener.DimensionBlockObserver(p.Dimension)(coreworld.BlockChange{X: x, Y: y, Z: z, Block: block})
			}
			return
		}
		lootBlock := block
		potDecorations := [4]string{}
		if block.ResourceLocation() == "minecraft:decorated_pot" {
			potDecorations = actionWorld.DecoratedPotDecorations(x, y, z)
		}
		if block.ResourceLocation() == "minecraft:decorated_pot" && held.EnchantmentLevel("minecraft:silk_touch") == 0 && blockloot.BreaksDecoratedPot(held.ItemID) {
			lootBlock = bedrockCopyBlock(block)
			lootBlock.Properties["cracked"] = "true"
		}
		enchantments := make(map[string]int)
		for _, enchantment := range held.EnchantmentLevels() {
			enchantments[enchantment.ID] = enchantment.Level
		}
		lootContext := blockloot.Context{
			Block:          lootBlock,
			Tool:           held,
			Enchantments:   enchantments,
			PotDecorations: potDecorations,
			BlockAt: func(dx, dy, dz int) coreworld.Block {
				return actionWorld.GetBlock(x+dx, y+dy, z+dz)
			},
		}
		drops := blockloot.Drops(lootContext)
		containerItems := []coreworld.ContainerItem(nil)
		if bedrockSpillingContainer(block.ResourceLocation()) ||
			block.ResourceLocation() == "minecraft:jukebox" ||
			block.ResourceLocation() == "minecraft:lectern" ||
			block.ResourceLocation() == "minecraft:chiseled_bookshelf" {
			containerItems = actionWorld.ContainerItems(x, y, z)
		}
		partnerY, partnerHalf, hasPartner := coreworld.DoublePlantPartnerY(block, y)
		partner := coreworld.Air
		if hasPartner {
			partner = actionWorld.GetBlock(x, partnerY, z)
			hasPartner = partner.ResourceLocation() == block.ResourceLocation() &&
				partner.Properties["half"] == partnerHalf
		}
		if s.bedrockListener != nil {
			s.bedrockListener.BroadcastDimensionBlockBreakEffect(p.Dimension, i.Position, block)
		}
		s.setBedrockActionBlock(x, y, z, coreworld.Air)
		if hasPartner {
			s.setBedrockActionBlock(x, partnerY, z, coreworld.Air)
		}
		s.breakBedrockLinkedBlock(x, y, z, block)
		s.breakBedrockUnsupportedAbove(p, x, y, z)
		if block.ResourceLocation() == "minecraft:redstone_wire" {
			s.refreshBedrockWireConnections(x, y, z)
		}
		if p.GameMode != player.GameModeCreative {
			if coreworld.IsShulkerBox(block.ResourceLocation()) {
				// Pack contents into the dropped shulker box item.
				for i2, drop := range drops {
					if drop.ItemID != "" {
						drops[i2] = coreworld.ShulkerBoxDropItem(drop.ItemID, containerItems)
					}
				}
			} else {
				for _, item := range containerItems {
					stack := item.Stack()
					if dropped := s.newDroppedItemForPlayer(p, stack, center, item.Slot+1); dropped != nil {
						handler.BroadcastSpawnMobInDimension(dropped, s.sessions, p.Dimension)
					}
				}
			}
			for index, drop := range drops {
				if dropped := s.newDroppedItemForPlayer(p, drop, center, len(containerItems)+index); dropped != nil {
					handler.BroadcastSpawnMobInDimension(dropped, s.sessions, p.Dimension)
				}
			}
			for _, orb := range coreexperience.SpawnOrbs(actionWorld, s.game.NextEntityID, center, blockloot.Experience(lootContext)) {
				handler.BroadcastSpawnMobInDimension(orb, s.sessions, p.Dimension)
			}
			if wear := player.BlockUseDamage(held.ItemID); wear > 0 {
				s.damageBedrockHeldItem(p, wear)
			}
		}
		if bedrockSpillingContainer(block.ResourceLocation()) ||
			block.ResourceLocation() == "minecraft:jukebox" ||
			block.ResourceLocation() == "minecraft:lectern" ||
			block.ResourceLocation() == "minecraft:chiseled_bookshelf" {
			actionWorld.SetContainerItems(x, y, z, block.ResourceLocation(), nil)
		}

	case intent.BlockActionUse:
		held := p.HeldItem()
		clicked := actionWorld.GetBlock(x, y, z)
		// Item behaviour has priority over the clicked block, matching vanilla
		// and Pumpkin (for example, a hoe tills dirt before placement is tried).
		if s.applyBedrockItemAction(p, i, clicked) {
			return
		}
		bypassActivation := p.Sneaking && !held.IsEmpty()
		if !bypassActivation && clicked.ResourceLocation() == "minecraft:dragon_egg" && p.GameMode != player.GameModeCreative {
			s.bedrockDragonEggTeleport(actionWorld, x, y, z)
			return
		}
		if !bypassActivation && clicked.ResourceLocation() == "minecraft:bell" {
			if direction, valid := coreworld.BellRingDirection(clicked, i.Face, i.ClickY); valid {
				s.ringBell(actionWorld, p.Dimension, i.Position, direction)
				return
			}
		}
		if !bypassActivation && coreworld.IsChiseledBookshelf(clicked.ResourceLocation()) && p.GameMode != player.GameModeSpectator {
			bx2, by2, bz2 := int(i.Position.X), int(i.Position.Y), int(i.Position.Z)
			facing := clicked.Properties["facing"]
			slot := coreworld.ChiseledBookshelfSlot(facing, float64(i.ClickX), float64(i.ClickY), float64(i.ClickZ))
			be := actionWorld.GetBlockEntity(bx2, by2, bz2)
			slotProp := fmt.Sprintf("slot_%d_occupied", slot)
			if clicked.Properties[slotProp] == "true" {
				storedID := ""
				for _, ci := range be.Items {
					if ci.Slot == slot {
						storedID = ci.ItemID
						break
					}
				}
				if _, cleared, ok2 := coreworld.EjectBookshelfBook(clicked, slot, storedID); ok2 {
					s.setBedrockActionBlock(bx2, by2, bz2, cleared)
					newItems := make([]coreworld.ContainerItem, 0, 6)
					for _, ci := range be.Items {
						if ci.Slot != slot {
							newItems = append(newItems, ci)
						}
					}
					actionWorld.SetContainerItems(bx2, by2, bz2, "minecraft:chiseled_bookshelf", newItems)
					actionWorld.SetBookshelfLastSlot(bx2, by2, bz2, slot+1)
					s.giveBedrockActionItem(p, player.ItemStack{ItemID: storedID, Count: 1})
				}
			} else if coreworld.IsBookshelfBook(held.ItemID) {
				if updated, ok2 := coreworld.InsertBookshelfBook(clicked, slot, held.ItemID); ok2 {
					s.setBedrockActionBlock(bx2, by2, bz2, updated)
					newItems := append(be.Items, coreworld.ContainerItem{Slot: slot, ItemID: held.ItemID, Count: 1})
					actionWorld.SetContainerItems(bx2, by2, bz2, "minecraft:chiseled_bookshelf", newItems)
					actionWorld.SetBookshelfLastSlot(bx2, by2, bz2, slot+1)
					s.consumeBedrockHeldItem(p, 1)
				}
			}
			return
		}
		if !bypassActivation && s.applyBedrockBlockActivation(p, i.Position, clicked) {
			return
		}
		if !bypassActivation && s.bedrockListener != nil {
			if handler.IsFurnaceContainer(clicked.ResourceLocation()) {
				s.openBedrockFurnace(p, i.Position, clicked.ResourceLocation())
			} else if isBedrockGenericContainer(clicked.ResourceLocation()) {
				s.openBedrockGenericContainer(p, i.Position, clicked.ResourceLocation())
			} else if isBedrockWorkstation(clicked.ResourceLocation()) {
				s.openBedrockWorkstation(p, i.Position, clicked.ResourceLocation())
			}
			if s.bedrockListener.OpenContainerBlock(p.UUID, int32(x), int32(y), int32(z), clicked.ResourceLocation()) {
				if isBedrockGenericContainer(clicked.ResourceLocation()) {
					s.bedrockListener.SyncGenericContainer(p)
				} else if clicked.ResourceLocation() == "minecraft:brewing_stand" {
					state := s.brewingStateForDimension(p.Dimension, i.Position)
					s.bedrockListener.SyncBrewingContainer(p, state.BrewTime, state.FuelAmount)
				} else if isBedrockWorkstation(clicked.ResourceLocation()) {
					s.bedrockListener.SyncWorkstationContainer(p)
				} else if !handler.IsFurnaceContainer(clicked.ResourceLocation()) {
					p.OpenContainerKind = clicked.ResourceLocation()
					p.OpenContainerID = 1
				} else {
					state := s.furnaceStateForDimension(p.Dimension, i.Position)
					s.bedrockListener.SyncFurnaceContainer(p, state.CookTime, state.BurnTime, state.BurnDuration, state.CookDuration)
				}
				return
			}
		}
		if !bypassActivation && strings.HasSuffix(clicked.ResourceLocation(), "_bed") {
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
			actionWorld.Entities.Add(boat)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				if p.Inventory[slot].Count <= 0 {
					p.Inventory[slot] = player.ItemStack{}
				}
			}
			return
		}
		s.placeBedrockHeldBlock(p, i, clicked)
	}
}

// duplicateBedrockBlockUse coalesces the two equivalent interaction reports a
// modern Bedrock client may send (PlayerAuthInput plus InventoryTransaction).
// Without this simulation-side guard a door or gate is toggled twice and seems
// to close immediately even when the adapter-level packets arrived on separate
// network ticks.
func (s *Server) duplicateBedrockBlockUse(p *player.Player, i intent.BlockInteractIntent) bool {
	if p == nil {
		return false
	}
	if s.bedrockBlockUse == nil {
		s.bedrockBlockUse = make(map[[16]byte]bedrockRecentBlockUse)
	}
	// Bedrock may report a door click once against the lower half and once
	// against the upper half. Normalise both to the lower coordinate before
	// comparing, and do not include the packet-specific face in the identity.
	position := i.Position
	if world := s.worldForPlayer(p); world != nil {
		block := world.GetBlock(int(position.X), int(position.Y), int(position.Z))
		if bedrockIsDoor(block.ResourceLocation()) && block.Properties["half"] == "upper" {
			position.Y--
		}
	}
	now := time.Now()
	previous, exists := s.bedrockBlockUse[p.UUID]
	duplicate := exists && previous.dimension == p.Dimension && previous.position == position &&
		previous.slot == i.HotbarSlot && now.Sub(previous.at) >= 0 &&
		now.Sub(previous.at) < 300*time.Millisecond
	s.bedrockBlockUse[p.UUID] = bedrockRecentBlockUse{
		dimension: p.Dimension,
		position:  position,
		face:      i.Face,
		slot:      i.HotbarSlot,
		at:        now,
	}
	return duplicate
}

func bedrockSpillingContainer(blockID string) bool {
	return (isBedrockGenericContainer(blockID) && blockID != "minecraft:ender_chest") ||
		blockID == "minecraft:decorated_pot" || handler.IsFurnaceContainer(blockID)
}

func (s *Server) applyEntityInteract(i intent.EntityInteractIntent) {
	attacker := s.game.GetPlayer(i.PlayerUUID)
	if attacker == nil {
		return
	}
	previousHeldSlot := attacker.HeldSlot
	if i.HotbarSlot >= 0 && i.HotbarSlot < 9 && int(i.HotbarSlot) != previousHeldSlot {
		attacker.HeldSlot = int(i.HotbarSlot)
		defer func() { attacker.HeldSlot = previousHeldSlot }()
	}
	if !i.Attack {
		if i.TargetID == 0 && attacker.VehicleEntityID != 0 {
			s.dismountPlayer(attacker)
			return
		}
		if entity, ok := s.worldForPlayer(attacker).Entities.Get(i.TargetID); ok && attacker.Position.Distance(entity.Position) <= 4 {
			if attacker.Edition == player.ClientEditionBedrock &&
				attacker.HeldItem().ItemID == "minecraft:name_tag" &&
				attacker.GameMode != player.GameModeSpectator {
				name := attacker.HeldItem().DisplayName()
				if name != "" {
					entity.DisplayName = name
					entity.CustomNameVisible = true
					handler.BroadcastMobMetadata(entity, s.sessions)
					s.consumeAnimalItem(attacker, "")
					s.syncPlayerInventory(attacker)
					return
				}
			}
			if entity.Type == corentity.TypeVillager && !entity.CanTradeAsVillager() {
				handler.BroadcastVillagerUnhappy(s.sessions, entity)
				if s.bedrockListener != nil {
					s.bedrockListener.BroadcastVillagerUnhappy(entity)
				}
				return
			}
			if entity.Type == corentity.TypeVillager && attacker.Edition == player.ClientEditionBedrock && s.bedrockListener != nil {
				s.bedrockListener.OpenVillagerTrade(attacker.UUID, entity)
				return
			}
			if corentity.IsAgeableAnimal(entity.Type) || corentity.IsTameableAnimal(entity.Type) || corentity.IsAnimalVehicle(entity.Type) {
				if s.interactAnimal(attacker, entity) {
					s.syncPlayerInventory(attacker)
				}
				return
			}
			if corentity.IsBoat(entity.Type) || corentity.IsMinecart(entity.Type) {
				s.mountPlayer(attacker, entity)
			}
		}
		return
	}
	if attacker == nil || attacker.Dead || attacker.GameMode == player.GameModeSpectator {
		return
	}
	heldID := attacker.HeldItem().ItemID
	damage := player.LegacyAttackDamage(heldID)
	if attacker.AttackCooldown {
		if value, _, ok := player.AttackAttributes(heldID); ok {
			damage = value
		}
		if !attacker.LastAttack.IsZero() && time.Since(attacker.LastAttack) < 625*time.Millisecond {
			return
		}
	}
	// Sharpness adds 0.5 + 0.5*level bonus damage (vanilla 1.9+ formula).
	held := attacker.HeldItem()
	if lvl := held.EnchantmentLevel("minecraft:sharpness"); lvl > 0 {
		damage += 0.5 + float32(lvl)*0.5
	}
	// Knockback enchantment increases horizontal knockback (stored for later use).
	attacker.KnockbackHorizontal = 0.4 + float64(held.EnchantmentLevel("minecraft:knockback"))*0.5

	// Mace smash attack: while falling, each block of fall distance adds bonus damage.
	// Vanilla formula: bonus = 3 * floor(fallDistance) when fallDistance ≥ 1.5.
	// Source: vanilla Mace item and PumpkinMC mace item logic.
	isMaceSmash := heldID == "minecraft:mace" && !attacker.OnGround && attacker.FallDistance >= 1.5
	if isMaceSmash {
		damage += float32(math.Floor(attacker.FallDistance)) * 3
	}

	// Critical hit: falling (not on ground, not flying) with any weapon except mace.
	// Vanilla applies a 1.5x damage multiplier. Sprinting also disqualifies in 1.9+,
	// but GoCraft doesn't track sprint-start precisely, so we skip that check.
	isCrit := !isMaceSmash && !attacker.OnGround && !attacker.Flying &&
		attacker.FallDistance > 0
	if isCrit {
		damage = float32(math.Floor(float64(damage) * 1.5))
	}

	var targetPlayer *player.Player
	s.game.OnlinePlayers(func(candidate *player.Player) {
		if candidate.EntityID == i.TargetID && candidate.Dimension == attacker.Dimension {
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
			damaged = handler.DamagePlayerFromSource(targetSession, damage, "was slain by "+attacker.Username, s.sessions, attacker.Position.X, attacker.Position.Z)
		} else {
			damaged = handler.DamagePlayerFromSource(targetSession, damage, "was slain by "+attacker.Username, s.sessions, attacker.Position.X, attacker.Position.Z)
		}
		if damaged {
			// Mace smash: reset fall distance so the attacker takes no fall damage.
			if isMaceSmash {
				attacker.FallDistance = 0
			}
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
			if player.IsSword(heldID) && attacker.OnGround && attacker.AttackCooldown && !isCrit {
				if attackerWorld := s.worldForPlayer(attacker); attackerWorld != nil {
					s.applySweepAttack(attacker, targetPlayer.EntityID, targetPlayer.Position.X, targetPlayer.Position.Z, attackerWorld)
				}
			}
		}
		return
	}

	if attackerWorld := s.worldForPlayer(attacker); attackerWorld != nil {
		if entity, ok := attackerWorld.Entities.Get(i.TargetID); ok && !entity.Dead && attacker.Position.Distance(entity.Position) <= 3.25 &&
			attackerWorld.QueueEntityDamageFromPlayer(entity.EntityID, damage, attacker.Position.X, attacker.Position.Z, attacker.UUID) {
			attacker.LastAttack = time.Now()
			attacker.LastAttackedEntityID = entity.EntityID
			s.damageBedrockHeldItem(attacker, 1)
			// Sweep attack: sword + on ground + full cooldown + not crit.
			if player.IsSword(heldID) && attacker.OnGround && attacker.AttackCooldown && !isCrit {
				s.applySweepAttack(attacker, entity.EntityID, entity.Position.X, entity.Position.Z, attackerWorld)
			}
		}
	}
}

// applySweepAttack deals 1 sweep damage to all entities within 3.3 blocks of
// the primary target, excluding the primary target itself and the attacker.
func (s *Server) applySweepAttack(attacker *player.Player, primaryID int32, primaryX, primaryZ float64, w *coreworld.World) {
	const sweepRadius = 3.3
	const sweepDamage = float32(1)
	entities := w.Entities.Snapshot()
	for _, e := range entities {
		if e == nil || e.Dead || e.EntityID == primaryID {
			continue
		}
		dx := e.Position.X - attacker.Position.X
		dy := e.Position.Y - attacker.Position.Y
		dz := e.Position.Z - attacker.Position.Z
		dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if dist <= sweepRadius {
			w.QueueEntityDamageFromPlayer(e.EntityID, sweepDamage, primaryX, primaryZ, attacker.UUID)
		}
	}
	// Also sweep nearby players.
	s.game.OnlinePlayers(func(p2 *player.Player) {
		if p2.UUID == attacker.UUID || p2.EntityID == primaryID || p2.Dimension != attacker.Dimension {
			return
		}
		dx := p2.Position.X - attacker.Position.X
		dy := p2.Position.Y - attacker.Position.Y
		dz := p2.Position.Z - attacker.Position.Z
		dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if dist <= sweepRadius {
			if sess, ok := s.sessions.Get(p2.UUID); ok {
				handler.DamagePlayerFromSource(sess, sweepDamage, "was swept by "+attacker.Username, s.sessions, primaryX, primaryZ)
			}
		}
	})
}

// Kept for existing tests and callers while all editions now share the same
// canonical implementation.
func (s *Server) applyBedrockEntityInteract(i intent.EntityInteractIntent) {
	s.applyEntityInteract(i)
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
	if p.VehicleEntityID != 0 {
		s.dismountPlayer(p)
	}
	previousDimension := p.Dimension
	p.Revive()
	if anchorSpawn, ok := handler.ResolveAnchorRespawn(p, s.netherWorld); ok {
		p.Dimension = dimensionNether
		p.Position = anchorSpawn
	} else if bedSpawn, ok := handler.ResolveBedRespawn(p, s.world); ok {
		p.Dimension = dimensionOverworld
		p.Position = bedSpawn
	} else {
		p.Dimension = dimensionOverworld
		p.Position = p.WorldSpawn
	}
	if previousDimension != p.Dimension && s.bedrockListener != nil {
		s.bedrockListener.ChangeDimensionForRespawn(p, p.Dimension, p.Position)
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
	craftingGrid := p.CraftingGrid
	craftingResult := handler.FindBedrockCraftingTableResult(craftingGrid)
	furnaceSlots := append([]player.ItemStack(nil), p.ContainerSlots...)
	furnaceOpen := handler.IsFurnaceContainer(p.OpenContainerKind) && len(furnaceSlots) == furnaceSlotCount
	furnaceOutputBefore := player.ItemStack{}
	if furnaceOpen {
		furnaceOutputBefore = furnaceSlots[2]
	}
	containerSlots := append([]player.ItemStack(nil), p.ContainerSlots...)
	containerOpen := (isBedrockGenericContainer(p.OpenContainerKind) || isBedrockWorkstation(p.OpenContainerKind)) &&
		len(containerSlots) > 0 && len(containerSlots) <= 54
	workstationOpen := isBedrockWorkstation(p.OpenContainerKind) && containerOpen
	if workstationOpen {
		handler.UpdateWorkstationResult(p.OpenContainerKind, containerSlots, p.WorkstationSelection)
	}
	carried := p.CarriedItem
	updateBedrockPersonalCrafting(&inventory)
	drops := make([]player.ItemStack, 0)
	type foodEffect struct {
		nutrition  int32
		saturation float32
		itemID     string
	}
	foodEffects := make([]foodEffect, 0, 1)
	_, simulatedFood, _, _ := p.HealthSnapshot()
	get := func(slot int16) (player.ItemStack, bool) {
		if slot == intent.InventoryCursorSlot {
			return carried, true
		}
		if slot >= intent.InventoryCraftingTableStart && slot < intent.InventoryCraftingTableOutput {
			return craftingGrid[slot-intent.InventoryCraftingTableStart], true
		}
		if slot == intent.InventoryCraftingTableOutput {
			return craftingResult, true
		}
		if slot >= intent.InventoryFurnaceInput && slot <= intent.InventoryFurnaceOutput {
			if !furnaceOpen {
				return player.ItemStack{}, false
			}
			return furnaceSlots[slot-intent.InventoryFurnaceInput], true
		}
		if slot >= intent.InventoryContainerStart {
			index := int(slot - intent.InventoryContainerStart)
			if !containerOpen || index < 0 || index >= len(containerSlots) {
				return player.ItemStack{}, false
			}
			return containerSlots[index], true
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
		if slot >= intent.InventoryCraftingTableStart && slot < intent.InventoryCraftingTableOutput {
			craftingGrid[slot-intent.InventoryCraftingTableStart] = stack
			craftingResult = handler.FindBedrockCraftingTableResult(craftingGrid)
			return true
		}
		if slot == intent.InventoryCraftingTableOutput {
			return stack.IsEmpty()
		}
		if slot >= intent.InventoryFurnaceInput && slot <= intent.InventoryFurnaceOutput {
			if !furnaceOpen {
				return false
			}
			furnaceSlots[slot-intent.InventoryFurnaceInput] = stack
			return true
		}
		if slot >= intent.InventoryContainerStart {
			index := int(slot - intent.InventoryContainerStart)
			if !containerOpen || index < 0 || index >= len(containerSlots) {
				return false
			}
			if workstationOpen && index == bedrockWorkstationOutputIndex(p.OpenContainerKind) && !stack.IsEmpty() {
				return false
			}
			containerSlots[index] = stack
			if workstationOpen {
				handler.UpdateWorkstationResult(p.OpenContainerKind, containerSlots, p.WorkstationSelection)
			}
			return true
		}
		if slot < 0 || int(slot) >= len(inventory) || !canPlaceCanonicalInventorySlot(int(slot), stack) {
			return false
		}
		inventory[slot] = stack
		if slot >= 1 && slot <= 4 {
			updateBedrockPersonalCrafting(&inventory)
		}
		return true
	}

	for _, action := range i.Actions {
		if action.Kind == intent.InventoryActionCreativeGive {
			// Creative give: spawn an item into the cursor from the creative pool.
			// No source slot — only allowed in creative mode.
			if p.GameMode != player.GameModeCreative || action.Count <= 0 {
				return
			}
			given := action.Item
			given.Count = action.Count
			if !canPlaceBedrockFurnaceSlot(action.Destination, given) || !set(action.Destination, given) {
				return
			}
			continue
		}

		source, ok := get(action.Source)
		if !ok || source.IsEmpty() {
			return
		}
		workstationOutput := workstationOpen && action.Source >= intent.InventoryContainerStart &&
			int(action.Source-intent.InventoryContainerStart) == bedrockWorkstationOutputIndex(p.OpenContainerKind)
		switch action.Kind {
		case intent.InventoryActionMove:
			crafts := max(action.CraftCount, 1)
			if action.Source == 0 || action.Source == intent.InventoryCraftingTableOutput {
				if source.Count <= 0 || crafts > 255 || source.Count > 255/crafts {
					return
				}
				source.Count *= crafts
			}
			if action.Count <= 0 || action.Count > source.Count || action.Source == action.Destination || action.Destination == 0 || action.Destination == intent.InventoryCraftingTableOutput || action.Destination == intent.InventoryFurnaceOutput {
				return
			}
			if (action.Source == 0 || action.Source == intent.InventoryCraftingTableOutput || workstationOutput) && action.Count != source.Count {
				// Crafting outputs are indivisible: One result stack consumes one
				// item from every occupied ingredient slot.
				return
			}
			destination, ok := get(action.Destination)
			if !ok || (!destination.IsEmpty() && !destination.SameItem(source)) {
				return
			}
			newDestination := destination
			if newDestination.IsEmpty() {
				newDestination = source
				newDestination.Count = 0
			}
			newDestination.Count += action.Count
			limit := player.MaxStackSize(newDestination.ItemID)
			if action.Destination >= 5 && action.Destination <= 8 {
				limit = 1
			}
			if newDestination.Count > limit || !canPlaceBedrockFurnaceSlot(action.Destination, newDestination) || !set(action.Destination, newDestination) {
				return
			}
			source.Count -= action.Count
			if action.Source == 0 {
				for range crafts {
					current := inventory[0]
					if !current.SameItem(source) || current.Count*crafts != action.Count {
						return
					}
					consumeBedrockPersonalCrafting(&inventory)
				}
			} else if action.Source == intent.InventoryCraftingTableOutput {
				for range crafts {
					current := handler.FindBedrockCraftingTableResult(craftingGrid)
					if !current.SameItem(source) || current.Count*crafts != action.Count {
						return
					}
					consumeBedrockCraftingTable(&craftingGrid)
				}
				craftingResult = handler.FindBedrockCraftingTableResult(craftingGrid)
			} else if workstationOutput {
				result, _, ok := handler.TakeWorkstationResult(p.OpenContainerKind, containerSlots, p.WorkstationSelection)
				if !ok || !result.SameItem(source) || result.Count != action.Count {
					return
				}
			} else if !set(action.Source, source) {
				return
			}

		case intent.InventoryActionSwap:
			if workstationOutput || (workstationOpen && action.Destination >= intent.InventoryContainerStart &&
				int(action.Destination-intent.InventoryContainerStart) == bedrockWorkstationOutputIndex(p.OpenContainerKind)) {
				return
			}
			destination, ok := get(action.Destination)
			if !ok || action.Source == action.Destination ||
				!canPlaceBedrockFurnaceSlot(action.Source, destination) || !canPlaceBedrockFurnaceSlot(action.Destination, source) ||
				!set(action.Source, destination) || !set(action.Destination, source) {
				return
			}

		case intent.InventoryActionDrop:
			if action.Count <= 0 || action.Count > source.Count {
				return
			}
			drop := source
			drop.Count = action.Count
			drops = append(drops, drop)
			if workstationOutput {
				if action.Count != source.Count {
					return
				}
				result, _, ok := handler.TakeWorkstationResult(p.OpenContainerKind, containerSlots, p.WorkstationSelection)
				if !ok || !result.SameItem(source) || result.Count != action.Count {
					return
				}
				continue
			}
			source.Count -= action.Count
			if !set(action.Source, source) {
				return
			}

		case intent.InventoryActionDestroy:
			if workstationOutput || p.GameMode != player.GameModeCreative || action.Count <= 0 || action.Count > source.Count {
				return
			}
			source.Count -= action.Count
			if !set(action.Source, source) {
				return
			}

		case intent.InventoryActionConsume:
			if action.Source < player.HotbarStart || action.Source >= player.HotbarStart+9 || action.Count != 1 || simulatedFood >= 20 {
				return
			}
			nutrition, saturation, ok := player.FoodValue(source.ItemID)
			if !ok {
				return
			}
			source.Count--
			if !set(action.Source, source) {
				return
			}
			foodEffects = append(foodEffects, foodEffect{nutrition: nutrition, saturation: saturation, itemID: source.ItemID})
			simulatedFood = min(20, simulatedFood+nutrition)

		default:
			return
		}
	}
	updateBedrockPersonalCrafting(&inventory)
	for _, effect := range foodEffects {
		if !p.ConsumeFood(effect.nutrition, effect.saturation) {
			return
		}
		s.applyBedrockFoodEffect(p, effect.itemID)
	}
	p.Inventory = inventory
	p.CraftingGrid = craftingGrid
	p.CraftingResult = handler.FindBedrockCraftingTableResult(craftingGrid)
	p.CarriedItem = carried
	if furnaceOpen {
		p.ContainerSlots = furnaceSlots
		persistFurnaceSlots(s.worldForPlayer(p), p.OpenContainerPos, p.OpenContainerKind, furnaceSlots)
		s.furnaceStateForDimension(p.Dimension, p.OpenContainerPos)
		if !furnaceOutputBefore.IsEmpty() && furnaceSlots[2].Count < furnaceOutputBefore.Count {
			s.awardFurnaceExperience(p)
		}
	} else if containerOpen {
		if workstationOpen {
			handler.UpdateWorkstationResult(p.OpenContainerKind, containerSlots, p.WorkstationSelection)
		}
		p.ContainerSlots = containerSlots
		if isBedrockGenericContainer(p.OpenContainerKind) {
			s.persistBedrockGenericContainer(p)
		}
	}
	for index, stack := range drops {
		if dropped := s.newDroppedItemForPlayer(p, stack, p.Position, index); dropped != nil && p.Dimension == dimensionOverworld {
			handler.BroadcastSpawnMob(dropped, s.sessions)
		}
	}
	if len(foodEffects) > 0 {
		s.clearBedrockItemUse(p, true)
	}
	accepted = true
}

func canPlaceBedrockFurnaceSlot(slot int16, stack player.ItemStack) bool {
	if stack.IsEmpty() || slot < intent.InventoryFurnaceInput || slot > intent.InventoryFurnaceOutput {
		return true
	}
	switch slot {
	case intent.InventoryFurnaceInput:
		return true
	case intent.InventoryFurnaceFuel:
		return handler.CanPlaceFurnaceFuelSlot(stack.ItemID)
	case intent.InventoryFurnaceOutput:
		return false
	default:
		return false
	}
}

func (s *Server) applyBedrockFoodEffect(p *player.Player, itemID string) {
	s.applyBedrockConsumableEffects(p, player.ItemStack{ItemID: itemID})
}

func (s *Server) applyBedrockConsumableEffects(p *player.Player, stack player.ItemStack) {
	if p == nil {
		return
	}
	roll := int(p.EntityID*1103515245+12345) & 0x7fffffff
	effects := player.FoodStatusEffects(stack.ItemID, roll%100)
	effects = append(effects, player.SuspiciousStewEffects(stack)...)
	if potion, ok := player.PotionOutcomeFor(stack); ok {
		if potion.Heal > 0 {
			p.Heal(potion.Heal)
		}
		if potion.Damage > 0 {
			handler.DamagePlayerMagic(&session.Session{Player: p}, potion.Damage, "was killed by magic", s.sessions)
		}
		effects = append(effects, potion.Effects...)
	}
	glowingApplied := false
	for _, effect := range effects {
		stored, changed := p.AddStatusEffect(effect)
		if !changed {
			continue
		}
		if effectType := bedrockEffectType(stored.ID); effectType != 0 && s.bedrockListener != nil {
			s.bedrockListener.SendPlayerMobEffect(p, effectType, stored.Amplifier, stored.Duration)
		}
		if stored.ID == "minecraft:glowing" && p.Edition == player.ClientEditionJava {
			glowingApplied = true
		}
	}
	if glowingApplied && s.sessions != nil {
		handler.BroadcastPlayerSharedFlags(p.EntityID, handler.PlayerSharedFlags(p), s.sessions)
	}
}

func bedrockEffectType(id string) int32 {
	return bedrock.EffectType(id)
}

func (s *Server) tickPufferfishContact(entities []*corentity.Entity) {
	if s == nil || s.game == nil || s.world == nil {
		return
	}
	for _, fish := range entities {
		if fish == nil || fish.Dead || fish.Type != corentity.TypePufferfish {
			continue
		}
		before := fish.PufferState
		if updatePufferState(fish, s.pufferfishThreatNearby(fish, entities)) {
			handler.BroadcastMobMetadata(fish, s.sessions)
			sound := "minecraft:entity.puffer_fish.blow_out"
			if fish.PufferState > before {
				sound = "minecraft:entity.puffer_fish.blow_up"
			}
			handler.BroadcastSoundAt(s.sessions, sound, handler.SoundCategoryNeutral,
				fish.Position.X, fish.Position.Y, fish.Position.Z, 1, 1)
		}
		if fish.PufferState == 0 || s.worldAge%20 != 0 {
			continue
		}
		stung := false
		s.game.OnlinePlayers(func(p *player.Player) {
			if p == nil || p.Dead || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator ||
				p.Dimension != s.simulationDimension {
				return
			}
			dx, dy, dz := p.Position.X-fish.Position.X, p.Position.Y-fish.Position.Y, p.Position.Z-fish.Position.Z
			if dx*dx+dy*dy+dz*dz > 2.25 || time.Since(p.LastEnvironmentDamage) < time.Second {
				return
			}
			p.LastEnvironmentDamage = time.Now()
			handler.DamagePlayer(&session.Session{Player: p}, float32(1+fish.PufferState), "was stung by a pufferfish", s.sessions)
			stung = true
			if p.Edition == player.ClientEditionJava {
				if target, ok := s.sessions.Get(p.UUID); ok {
					handler.SendMobEffect(target.Conn, p, "minecraft:poison", 0, fish.PufferState*60)
				}
			} else if s.bedrockListener != nil {
				s.bedrockListener.SendPlayerMobEffect(p, bedrockpacket.EffectPoison, 0, fish.PufferState*60)
			}
		})
		for _, target := range entities {
			if target == nil || target == fish || target.Dead || target.Type == corentity.TypePufferfish {
				continue
			}
			if _, living := pumpkinEntitySpawnSettingsByType[string(target.Type)]; !living ||
				!nearPufferfish(fish, target.Position.X, target.Position.Y, target.Position.Z, 1.5) {
				continue
			}
			s.world.QueueEntityDamage(target.EntityID, float32(1+fish.PufferState))
			target.PoisonTicks = max(target.PoisonTicks, fish.PufferState*60)
			stung = true
		}
		if stung {
			handler.BroadcastSoundAt(s.sessions, "minecraft:entity.puffer_fish.sting", handler.SoundCategoryNeutral,
				fish.Position.X, fish.Position.Y, fish.Position.Z, 1, 1)
		}
	}
}

func consumeBedrockCraftingTable(grid *[9]player.ItemStack) {
	for slot := range grid {
		if grid[slot].IsEmpty() {
			continue
		}
		grid[slot].Count--
		if grid[slot].Count <= 0 {
			grid[slot] = player.ItemStack{}
		}
	}
}

func updateBedrockPersonalCrafting(inventory *[player.InventorySize]player.ItemStack) {
	if inventory == nil {
		return
	}
	grid := [4]player.ItemStack{
		inventory[1], inventory[2], inventory[3], inventory[4],
	}
	inventory[0] = handler.FindPersonalCraftingResult(grid)
}

func consumeBedrockPersonalCrafting(inventory *[player.InventorySize]player.ItemStack) {
	if inventory == nil {
		return
	}
	for slot := 1; slot <= 4; slot++ {
		if inventory[slot].IsEmpty() {
			continue
		}
		inventory[slot].Count--
		if inventory[slot].Count <= 0 {
			inventory[slot] = player.ItemStack{}
		}
	}
	updateBedrockPersonalCrafting(inventory)
}

func canPlaceCanonicalInventorySlot(slot int, stack player.ItemStack) bool {
	if slot == 0 && !stack.IsEmpty() {
		return false
	}
	if stack.IsEmpty() || slot < 5 || slot > 8 {
		return true
	}
	definition, ok := itemregistry.Lookup(stack.ItemID)
	if !ok || definition.Equipment == nil {
		return false
	}
	switch definition.Equipment.Slot {
	case "head":
		return slot == 5
	case "chest":
		return slot == 6
	case "legs":
		return slot == 7
	case "feet":
		return slot == 8
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
	switch itemID {
	case "minecraft:wooden_door":
		// Bedrock still uses the legacy item identity for oak doors.
		itemID = "minecraft:oak_door"
	case "minecraft:fence_gate":
		itemID = "minecraft:oak_fence_gate"
	case "minecraft:wooden_button":
		itemID = "minecraft:oak_button"
	case "minecraft:wooden_pressure_plate":
		itemID = "minecraft:oak_pressure_plate"
	}
	if itemID == "minecraft:redstone" {
		itemID = "minecraft:redstone_wire"
	}
	if itemID == "minecraft:light_block" || strings.HasPrefix(itemID, "minecraft:light_block_") {
		itemID = "minecraft:light"
	}
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
		if p.Edition == player.ClientEditionBedrock && p.Dimension == dimensionOverworld {
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
	// Java movement is handled directly by its play loop rather than posted as
	// a MoveIntent, so check its portal occupancy from the common server tick.
	if s.game != nil {
		s.game.OnlinePlayers(func(p *player.Player) {
			if p.Edition == player.ClientEditionJava {
				s.tryBedrockPortalTravel(p)
			}
		})
	}

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
		if entity.Dead && event.HasPlayerSource {
			entity.ExperienceKillerUUID = event.SourcePlayerUUID
			entity.HasExperienceKiller = true
		}
		if !entity.Dead && event.HasSource {
			// Apply knockback to all mobs when damaged by a player.
			s.applyMobKnockback(entity, event)
		}
		if isPassiveMob(entity.Type) && !entity.Dead {
			s.startPassiveMobPanic(entity, event)
		}
		if (entity.Type == corentity.TypeIronGolem || entity.Type == corentity.TypeSnowGolem) &&
			!entity.Dead && event.HasSource {
			ai := s.mobAIFor(entity)
			ai.hasTarget = true
			ai.targetEntityID = 0
			ai.targetX = event.SourceX
			ai.targetZ = event.SourceZ
			ai.attackCooldown = 0
		}
		hurtEntities = append(hurtEntities, entity)
		debuglog.Info(debuglog.EntityEvents, "entity damaged", "type", entity.Type, "id", entityID,
			"damage", event.Amount, "health", entity.Health)
	}
	endDamage()

	simulationPlayers := s.naturalSpawnPlayers()
	s.despawnDistantNaturalMobs(simulationPlayers, &deadIDs)
	allEntities := s.world.Entities.Snapshot()
	s.tickAnimalLifecycle(allEntities)
	s.tickPufferfishContact(allEntities)

	// ── Parallel passive mob AI ───────────────────────────────────────────────
	// Passive per-entity computation is dispatched through a bounded worker
	// group. Hostile and golem AI can mutate shared players/world state and stay
	// serial below.
	endPassiveAI := s.timings.measure(sectionAI)
	villagerWakes := s.tickPassiveAIParallel(allEntities, simulationPlayers)
	endPassiveAI()
	for _, villager := range villagerWakes {
		handler.BroadcastVillagerSleepState(villager, s.sessions)
	}

	for _, e := range allEntities {
		e.AgeTicks++
		if !e.Dead {
			s.tickMobSunlight(e, &hurtEntities)
			s.tickEndermanBlockCarry(e)
		}
		// ── Dead entity cleanup ───────────────────────────────────────────────
		if e.Dead {
			if e.DeathTicks == 0 {
				s.dismountEntityPassengers(e)
				deathIDs = append(deathIDs, e.EntityID)
				spawned = append(spawned, s.spawnMobDrops(e)...)
				spawned = append(spawned, s.spawnMobExperience(e)...)
				debuglog.Info(debuglog.EntityEvents, "entity died", "type", e.Type, "id", e.EntityID)
			}
			e.DeathTicks++
			if e.DeathTicks >= 20 {
				s.world.Entities.Remove(e.EntityID)
				delete(s.mobAIs, e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
			}
			continue
		}

		if e.Type == corentity.TypeItem && s.tryPickupDroppedItem(e, dimensionOverworld) {
			s.world.Entities.Remove(e.EntityID)
			deadIDs = append(deadIDs, e.EntityID)
			continue
		}
		if e.Type == corentity.TypeExperienceOrb &&
			(s.tryPickupExperienceOrb(e, dimensionOverworld) || e.AgeTicks >= 6000 || e.Position.Y < coreworld.WorldMinY-16) {
			s.world.Entities.Remove(e.EntityID)
			deadIDs = append(deadIDs, e.EntityID)
			continue
		}
		if e.Type == corentity.TypeAreaEffectCloud {
			if s.tickAreaEffectCloud(e) {
				s.world.Entities.Remove(e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
			} else if e.AgeTicks >= areaEffectCloudWarmup && e.AgeTicks%10 == 0 {
				handler.BroadcastMobMetadataInDimension(e, s.sessions, dimensionOverworld)
			}
			continue
		}
		if e.Type == corentity.TypeFireworkRocket {
			if s.tickFireworkRocket(e) {
				s.world.Entities.Remove(e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
				continue
			}
			moved = append(moved, e)
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
		if corentity.IsMinecart(e.Type) {
			s.tickMinecartPhysics(e)
			tickMinecartCollisions(e, allEntities)
			s.syncMinecartPassengers(e)
			if s.tickTNTMinecartFuse(e) {
				s.world.Entities.Remove(e.EntityID)
				deadIDs = append(deadIDs, e.EntityID)
				continue
			}
			if e.VX != 0 || e.VY != 0 || e.VZ != 0 {
				moved = append(moved, e)
			}
			continue
		}

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
		if _, isMob := pumpkinEntitySpawnSettingsByType[string(e.Type)]; isMob && !entityWithinSimulationRange(e, simulationPlayers, 128) {
			e.VX, e.VY, e.VZ = 0, 0, 0
			continue
		}
		// Passive mob AI already ran in the parallel pre-pass above.
		prevX, prevY, prevZ := e.Position.X, e.Position.Y, e.Position.Z
		if !isPassiveMob(e.Type) {
			endAI := s.timings.measure(sectionAI)
			if e.Type == corentity.TypeIronGolem || e.Type == corentity.TypeSnowGolem {
				s.tickGolemAI(e)
			} else if isHostileMob(e.Type) {
				// Stagger hostile AI: run full AI every 2 ticks per mob.
				// This halves hostile-mob CPU cost without losing reactivity.
				if e.AgeTicks%2 == 0 {
					s.tickHostileMobAI(e)
				}
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
		inWater := s.entityInWater(e)
		if !e.OnGround && !inWater && !isFlyingMob(e.Type) {
			if e.Type == corentity.TypeExperienceOrb {
				e.VY -= 0.03
			} else {
				e.VY += gravity
			}
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
		horizontalDrag := drag
		if inWater {
			horizontalDrag = 0.8
			e.VY *= 0.8
		}
		e.VX *= horizontalDrag
		e.VZ *= horizontalDrag
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
	handler.DispatchTickBroadcastInDimension(moved, hurtEntities, deathIDs, deadIDs, spawned, s.sessions, dimensionOverworld)
	endBcast()

	// Publish time-of-day for handler code (e.g. bed sleep check).
	endTime := s.timings.measure(sectionTime)
	s.world.SetWorldTime(s.worldAge % 24000)
	for _, dimensionWorld := range []*coreworld.World{s.world, s.netherWorld, s.endWorld} {
		if dimensionWorld != nil {
			dimensionWorld.SetPhysicsTime(s.worldAge)
		}
	}
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
		for _, villager := range s.world.RefreshVillagerProfessions(10) {
			handler.BroadcastVillagerMetadata(villager, s.sessions)
		}
		for _, change := range s.world.TickFarmland(s.worldAge, 64) {
			handler.BroadcastBlockChange(change, s.sessions)
		}
		for _, change := range s.world.TickCrops(s.worldAge, 64) {
			handler.BroadcastBlockChange(change, s.sessions)
		}
	}
	endTime()

	endSpawn := s.timings.measure(sectionSpawnNatural)
	s.tickNaturalSpawning()
	endSpawn()
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
	if elapsed > 50*time.Millisecond && debuglog.Enabled(debuglog.EntityTickOverruns) {
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
	// A killed enderman drops the block it was carrying.
	if e.EndermanCarriedBlock != "" {
		stacks = append(stacks, player.ItemStack{ItemID: e.EndermanCarriedBlock, Count: 1})
		e.EndermanCarriedBlock = ""
	}
	spawned := make([]*corentity.Entity, 0, len(stacks))
	for index, stack := range stacks {
		if dropped := s.newDroppedItemInWorld(s.world, stack, e.Position, index); dropped != nil {
			spawned = append(spawned, dropped)
		}
	}
	return spawned
}

func (s *Server) spawnMobExperience(e *corentity.Entity) []*corentity.Entity {
	if e == nil || e.IsBaby || !e.HasExperienceKiller {
		return nil
	}
	return coreexperience.SpawnOrbs(s.world, s.game.NextEntityID, e.Position, pumpkinExperienceRewardByType[string(e.Type)])
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
	return s.newDroppedItemInWorld(s.bedrockWorld(), stack, position, ordinal)
}

func (s *Server) newDroppedItemForPlayer(p *player.Player, stack player.ItemStack, position spatial.Vec3, ordinal int) *corentity.Entity {
	return s.newDroppedItemInWorld(s.worldForPlayer(p), stack, position, ordinal)
}

func (s *Server) newDroppedItemInWorld(dimensionWorld *coreworld.World, stack player.ItemStack, position spatial.Vec3, ordinal int) *corentity.Entity {
	if dimensionWorld == nil || stack.IsEmpty() {
		return nil
	}
	id := s.game.NextEntityID()
	dropped := corentity.New(id, newRandomUUID(), corentity.TypeItem,
		position.X, position.Y+0.25, position.Z)
	dropped.SetDroppedItem(stack)
	angle := float64(id+int32(ordinal)*17) * 2.399963229728653
	dropped.VX = math.Cos(angle) * 0.1
	dropped.VY = 0.2
	dropped.VZ = math.Sin(angle) * 0.1
	dimensionWorld.Entities.Add(dropped)
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
		if dropped := s.newDroppedItemForPlayer(p, stack, p.Position, index); dropped != nil {
			handler.BroadcastSpawnMobInDimension(dropped, s.sessions, p.Dimension)
		}
	}
	level, _, _ := p.ExperienceSnapshot()
	reward := min(level*7, 100)
	for _, orb := range coreexperience.SpawnOrbs(s.worldForPlayer(p), s.game.NextEntityID, p.Position, reward) {
		handler.BroadcastSpawnMobInDimension(orb, s.sessions, p.Dimension)
	}
	p.SetTotalExperience(0)
	handler.SyncPlayerExperience(p, s.sessions)
	if sess, ok := s.sessions.Get(p.UUID); ok {
		_ = handler.SyncPlayerInventory(sess.Conn, p)
	}
}

func (s *Server) tryPickupDroppedItem(e *corentity.Entity, dimension int32) bool {
	if e.AgeTicks < 10 || e.ItemID == "" || e.ItemCount <= 0 {
		return false
	}
	for _, sess := range s.allPlayerSessions() {
		p := sess.Player
		if p == nil || p.Dimension != dimension || p.Dead || p.GameMode == player.GameModeSpectator {
			continue
		}
		dx := p.Position.X - e.Position.X
		dy := p.Position.Y + 0.5 - e.Position.Y
		dz := p.Position.Z - e.Position.Z
		if dx*dx+dy*dy+dz*dz > 2.25 {
			continue
		}
		stack := e.DroppedItem()
		if !p.GiveItem(stack) {
			continue
		}
		if dimension == dimensionOverworld {
			handler.BroadcastCollectItem(e.EntityID, p.EntityID, e.ItemCount, s.sessions)
		}
		if sess.Conn != nil {
			_ = handler.SyncPlayerInventory(sess.Conn, p)
		}
		return true
	}
	return false
}

func (s *Server) tryPickupExperienceOrb(e *corentity.Entity, dimension int32) bool {
	// Bedrock discovers orbs during the post-entity sync. Keep a new orb alive
	// through that first sync so its native spawn packet reaches the client.
	if e == nil || e.ExperienceAmount <= 0 || e.AgeTicks < 2 {
		return false
	}
	for _, sess := range s.allPlayerSessions() {
		p := sess.Player
		if p == nil || p.Dimension != dimension || p.Dead || p.GameMode == player.GameModeSpectator {
			continue
		}
		dx := p.Position.X - e.Position.X
		dy := p.Position.Y + 0.5 - e.Position.Y
		dz := p.Position.Z - e.Position.Z
		if dx*dx+dy*dy+dz*dz > 2.25 || !p.TryPickupExperience(e.ExperienceAmount, s.worldAge) {
			continue
		}
		handler.SyncPlayerExperience(p, s.sessions)
		handler.BroadcastSoundAtDimension(s.sessions, dimension, "minecraft:entity.experience_orb.pickup",
			handler.SoundCategoryPlayers, e.Position.X, e.Position.Y, e.Position.Z, 0.1, 1)
		if s.bedrockListener != nil {
			s.bedrockListener.BroadcastExperienceOrbPickup(dimension, e.Position)
		}
		return true
	}
	return false
}

// tickAuxiliaryDimensionItems advances entities in dimensions that do not use
// the Overworld's primary entity pass. Keeping a dimension-local Server view is
// important: AI navigation, collisions, damage, projectiles and broadcasts must
// all use the same world and may never target players in another dimension.
func (s *Server) tickAuxiliaryDimensionItems() {
	for dimension, dimensionWorld := range map[int32]*coreworld.World{
		dimensionNether: s.netherWorld,
		dimensionEnd:    s.endWorld,
	} {
		if dimensionWorld == nil {
			continue
		}
		simulation := s.dimensionSimulation(dimension, dimensionWorld)

		var (
			moved        []*corentity.Entity
			hurtEntities []*corentity.Entity
			deathIDs     []int32
			deadIDs      []int32
			spawned      []*corentity.Entity
		)
		for entityID, event := range dimensionWorld.DrainEntityDamage() {
			entity, ok := dimensionWorld.Entities.Get(entityID)
			if !ok || entity.Dead {
				continue
			}
			entity.Damage(event.Amount)
			if entity.Dead && event.HasPlayerSource {
				entity.ExperienceKillerUUID = event.SourcePlayerUUID
				entity.HasExperienceKiller = true
			}
			if !entity.Dead && event.HasSource {
				simulation.applyMobKnockback(entity, event)
			}
			if isPassiveMob(entity.Type) && !entity.Dead {
				simulation.startPassiveMobPanic(entity, event)
			}
			hurtEntities = append(hurtEntities, entity)
		}

		simulationPlayers := simulation.naturalSpawnPlayers()
		allEntities := dimensionWorld.Entities.Snapshot()
		simulation.tickAnimalLifecycle(allEntities)
		for _, entity := range allEntities {
			entity.AgeTicks++
			if entity.Dead {
				if entity.DeathTicks == 0 {
					simulation.dismountEntityPassengers(entity)
					deathIDs = append(deathIDs, entity.EntityID)
					spawned = append(spawned, simulation.spawnMobDrops(entity)...)
					spawned = append(spawned, simulation.spawnMobExperience(entity)...)
				}
				entity.DeathTicks++
				if entity.DeathTicks >= 20 {
					dimensionWorld.Entities.Remove(entity.EntityID)
					delete(s.mobAIs, entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
				}
				continue
			}
			switch {
			case entity.Type == corentity.TypeAreaEffectCloud:
				if simulation.tickAreaEffectCloud(entity) {
					dimensionWorld.Entities.Remove(entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
				} else if entity.AgeTicks >= areaEffectCloudWarmup && entity.AgeTicks%10 == 0 {
					handler.BroadcastMobMetadataInDimension(entity, s.sessions, dimension)
				}
			case entity.Type == corentity.TypeItem:
				if simulation.tryPickupDroppedItem(entity, dimension) || entity.AgeTicks >= 6000 || entity.Position.Y < coreworld.WorldMinY-16 {
					dimensionWorld.Entities.Remove(entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
					continue
				}
				if !entity.OnGround {
					entity.VY -= 0.04
				}
				entity.Position.X += entity.VX
				entity.Position.Y += entity.VY
				entity.Position.Z += entity.VZ
				x, z := int(math.Floor(entity.Position.X)), int(math.Floor(entity.Position.Z))
				groundY := float64(dimensionWorld.GroundYAtOrBelow(x, z, int(math.Floor(entity.Position.Y))) + 1)
				if entity.Position.Y <= groundY {
					entity.Position.Y = groundY
					entity.VY = 0
					entity.OnGround = true
				} else {
					entity.OnGround = false
					entity.VY *= 0.98
				}
				entity.VX *= 0.98
				entity.VZ *= 0.98
				moved = append(moved, entity)
			case entity.Type == corentity.TypeExperienceOrb:
				if simulation.tryPickupExperienceOrb(entity, dimension) || entity.AgeTicks >= 6000 || entity.Position.Y < coreworld.WorldMinY-16 {
					dimensionWorld.Entities.Remove(entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
					continue
				}
				if !entity.OnGround {
					entity.VY -= 0.03
				}
				entity.Position.X += entity.VX
				entity.Position.Y += entity.VY
				entity.Position.Z += entity.VZ
				x, z := int(math.Floor(entity.Position.X)), int(math.Floor(entity.Position.Z))
				groundY := float64(dimensionWorld.GroundYAtOrBelow(x, z, int(math.Floor(entity.Position.Y))) + 1)
				if entity.Position.Y <= groundY {
					entity.Position.Y, entity.VY, entity.OnGround = groundY, 0, true
				} else {
					entity.OnGround = false
				}
				entity.VX *= 0.98
				entity.VZ *= 0.98
				moved = append(moved, entity)
			case entity.Type == corentity.TypeFireworkRocket:
				if simulation.tickFireworkRocket(entity) {
					dimensionWorld.Entities.Remove(entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
					continue
				}
				moved = append(moved, entity)
			case corentity.IsProjectile(entity.Type):
				if simulation.tickProjectile(entity) || entity.Position.Y < coreworld.WorldMinY-16 {
					dimensionWorld.Entities.Remove(entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
					continue
				}
				moved = append(moved, entity)
			case entity.Type == corentity.TypePrimedTNT:
				entity.FuseTicks--
				if entity.FuseTicks <= 0 {
					simulation.explodeTNT(entity.Position.X, entity.Position.Y, entity.Position.Z)
					dimensionWorld.Entities.Remove(entity.EntityID)
					deadIDs = append(deadIDs, entity.EntityID)
					continue
				}
				entity.VY -= 0.04
				entity.Position.X += entity.VX
				entity.Position.Y += entity.VY
				entity.Position.Z += entity.VZ
				moved = append(moved, entity)
			case isPassiveMob(entity.Type), isHostileMob(entity.Type),
				entity.Type == corentity.TypeIronGolem, entity.Type == corentity.TypeSnowGolem:
				if !entityWithinSimulationRange(entity, simulationPlayers, 128) {
					entity.VX, entity.VY, entity.VZ = 0, 0, 0
					continue
				}
				previous := entity.Position
				if isPassiveMob(entity.Type) {
					if entity.Type == corentity.TypeVillager {
						simulation.tickVillagerBedClaim(entity, simulation.mobAIFor(entity))
					}
					if simulation.tickPassiveMobAI(entity) && entity.Type == corentity.TypeVillager {
						handler.BroadcastVillagerSleepState(entity, simulation.sessions)
					}
				} else if entity.Type == corentity.TypeIronGolem || entity.Type == corentity.TypeSnowGolem {
					simulation.tickGolemAI(entity)
				} else {
					simulation.tickHostileMobAI(entity)
				}
				if entity.Dead {
					continue
				}
				if simulation.tickAuxiliaryMobPhysics(entity, previous) {
					moved = append(moved, entity)
				}
			}
		}
		handler.DispatchTickBroadcastInDimension(moved, hurtEntities, deathIDs, deadIDs, spawned, s.sessions, dimension)
	}
}

func (s *Server) tickAuxiliaryMobPhysics(entity *corentity.Entity, previous spatial.Vec3) bool {
	if entity.Sleeping {
		entity.VX, entity.VY, entity.VZ = 0, 0, 0
		entity.OnGround = true
		return entity.Position != previous
	}
	const (
		gravity = -0.08
		drag    = 0.98
		minVel  = 1e-6
	)
	inWater := s.entityInWater(entity)
	if !entity.OnGround && !inWater && !isFlyingMob(entity.Type) {
		entity.VY += gravity
	}

	nextX := entity.Position.X + entity.VX
	if canMove, loaded := s.world.CanEntityOccupyIfLoaded(nextX, entity.Position.Y, entity.Position.Z); loaded && canMove {
		entity.Position.X = nextX
	} else if loaded && entity.OnGround && entity.VX != 0 {
		if canStep, stepLoaded := s.world.CanEntityOccupyIfLoaded(nextX, entity.Position.Y+1, entity.Position.Z); stepLoaded && canStep {
			entity.Position.X = nextX
			entity.Position.Y = math.Floor(entity.Position.Y) + 1
			entity.VY = 0
		} else if stepLoaded {
			entity.VX = 0
		}
	} else if loaded {
		entity.VX = 0
	}
	entity.Position.Y += entity.VY
	nextZ := entity.Position.Z + entity.VZ
	if canMove, loaded := s.world.CanEntityOccupyIfLoaded(entity.Position.X, entity.Position.Y, nextZ); loaded && canMove {
		entity.Position.Z = nextZ
	} else if loaded && entity.OnGround && entity.VZ != 0 {
		if canStep, stepLoaded := s.world.CanEntityOccupyIfLoaded(entity.Position.X, entity.Position.Y+1, nextZ); stepLoaded && canStep {
			entity.Position.Z = nextZ
			entity.Position.Y = math.Floor(entity.Position.Y) + 1
			entity.VY = 0
		} else if stepLoaded {
			entity.VZ = 0
		}
	} else if loaded {
		entity.VZ = 0
	}

	x, z := int(math.Floor(entity.Position.X)), int(math.Floor(entity.Position.Z))
	cx, cz := coreworld.ChunkCoordsFor(x, z)
	groundY := previous.Y
	if s.world.IsChunkLoaded(cx, cz) {
		groundY = float64(s.world.GroundYAtOrBelow(x, z, int(math.Floor(math.Max(previous.Y, entity.Position.Y)))) + 1)
	}
	if entity.Position.Y <= groundY {
		entity.Position.Y = groundY
		entity.VY = 0
		entity.OnGround = true
	} else {
		entity.OnGround = false
	}
	horizontalDrag := drag
	if inWater {
		horizontalDrag = 0.8
		entity.VY *= 0.8
	}
	entity.VX *= horizontalDrag
	entity.VZ *= horizontalDrag
	if math.Abs(entity.VX) < minVel {
		entity.VX = 0
	}
	if math.Abs(entity.VZ) < minVel {
		entity.VZ = 0
	}
	return entity.Position != previous
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

func isFlyingMob(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeAllay, corentity.TypeBat, corentity.TypeBee, corentity.TypeBlaze,
		corentity.TypeGhast, corentity.TypeParrot, corentity.TypePhantom,
		corentity.TypeVex, corentity.TypeWither:
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

// applyMobKnockback applies a hit-direction velocity impulse to any mob that
// was just damaged. Passive mobs also get a full panic via startPassiveMobPanic;
// this function ensures hostile mobs also visually fly back.
func (s *Server) applyMobKnockback(e *corentity.Entity, hit coreworld.EntityDamage) {
	if !hit.HasSource {
		return
	}
	ai := s.mobAIFor(e)
	dx, dz := e.Position.X-hit.SourceX, e.Position.Z-hit.SourceZ
	distance := math.Hypot(dx, dz)
	if distance < 0.01 {
		angle := ai.rng.Float64() * 2 * math.Pi
		dx, dz, distance = math.Cos(angle), math.Sin(angle), 1
	}
	horizontal := 0.4
	vertical := 0.4
	if s.cfg != nil {
		horizontal = s.cfg.Combat.KnockbackHorizontal
		vertical = s.cfg.Combat.KnockbackVertical
	}
	e.VX = dx / distance * horizontal
	e.VZ = dz / distance * horizontal
	if e.OnGround {
		e.VY = vertical
	}
	ai.knockbackTick = 4
	ai.dirX = dx / distance
	ai.dirZ = dz / distance
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
	ai.targetX = e.Position.X + ai.dirX*10
	ai.targetZ = e.Position.Z + ai.dirZ*10
	ai.hasPathGoal = false
	ai.panicTick = 60
	ai.knockbackTick = 8
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

// tickTamedWolfCombat handles combat for a tamed wolf when its owner is under
// attack or actively fighting. Implements OwnerHurtByTargetGoal and
// OwnerHurtTargetGoal from PumpkinMC wolf.rs. Returns true when the wolf is
// actively pursuing a target (so normal wander AI should be skipped).
func (s *Server) tickTamedWolfCombat(e *corentity.Entity, ai *mobAI) bool {
	if e == nil || !e.Tamed || e.Sitting || s.world == nil {
		return false
	}
	// Find the owner session.
	var ownerSession *session.Session
	for _, sess := range s.allPlayerSessions() {
		if sess.Player == nil {
			continue
		}
		if e.TameOwnerEntityID != 0 && sess.Player.EntityID == e.TameOwnerEntityID {
			ownerSession = sess
			break
		}
	}
	if ownerSession == nil || ownerSession.Player == nil {
		return false
	}
	owner := ownerSession.Player

	// Determine which entity to target:
	// 1. Whoever just hurt the owner (OwnerHurtByTargetGoal)
	// 2. Whoever the owner just attacked (OwnerHurtTargetGoal)
	targetEntityID := int32(0)
	if owner.LastAttackerEntityID != 0 {
		targetEntityID = owner.LastAttackerEntityID
	} else if owner.LastAttackedEntityID != 0 {
		targetEntityID = owner.LastAttackedEntityID
	}
	if targetEntityID == 0 {
		// No combat cue — but if the wolf already has a target, continue chasing.
		if ai.hasTarget && ai.targetEntityID != 0 {
			targetEntityID = ai.targetEntityID
		} else {
			return false
		}
	}

	target, ok := s.world.Entities.Get(targetEntityID)
	if !ok || target.Dead || target.EntityID == e.EntityID {
		ai.hasTarget = false
		ai.targetEntityID = 0
		return false
	}

	// Wolf stays within 16 blocks of its owner; give up if the target wanders too far.
	distOwner := math.Hypot(e.Position.X-owner.Position.X, e.Position.Z-owner.Position.Z)
	if distOwner > 16 {
		ai.hasTarget = false
		ai.targetEntityID = 0
		return false
	}

	ai.hasTarget = true
	ai.targetEntityID = targetEntityID
	ai.targetX, ai.targetZ = target.Position.X, target.Position.Z

	dx := target.Position.X - e.Position.X
	dz := target.Position.Z - e.Position.Z
	dist := math.Hypot(dx, dz)
	if dist > 0.001 {
		e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
	}

	if ai.attackCooldown > 0 {
		ai.attackCooldown--
	}

	const wolfMeleeRange = 1.8
	if dist <= wolfMeleeRange && ai.attackCooldown == 0 {
		ai.attackCooldown = 20
		if s.mobHasLineOfSight(e, target.Position, 1.4) {
			// Wolf deals 2-4 damage depending on variant (use 4 base).
			s.world.QueueEntityDamageFrom(target.EntityID, 4, e.Position.X, e.Position.Z)
			handler.BroadcastSoundAt(s.sessions, "minecraft:entity.wolf.hurt", handler.SoundCategoryHostile,
				e.Position.X, e.Position.Y, e.Position.Z, 1, 1)
		}
		e.VX, e.VZ = 0, 0
		return true
	}

	const wolfSpeed = 0.3
	if !s.navigateMob(e, ai, spatial.Vec3{X: ai.targetX, Y: e.Position.Y, Z: ai.targetZ}, wolfSpeed) {
		e.VX, e.VZ = dx/dist*wolfSpeed, dz/dist*wolfSpeed
	}
	return true
}

// tickPassiveMobAI advances wander AI for a single passive mob.
// Returns true if the entity's Sleeping state changed this tick (so the caller
// can broadcast a pose metadata update).
//
// Villagers are homed: they stay within 8 blocks of their spawn point.
// All other passive mobs roam freely, occasionally pausing.
func (s *Server) tickPassiveMobAI(e *corentity.Entity) bool {
	ai := s.mobAIFor(e)

	// Tamed wolves with a live target assist their owner in combat.
	if e.Type == corentity.TypeWolf && e.Tamed && !e.Sitting {
		if s.tickTamedWolfCombat(e, ai) {
			return false
		}
	}
	wasAsleep := ai.sleepingWas
	validBed := s.validVillagerBed(e)
	if e.Type == corentity.TypeVillager && e.Sleeping && !validBed {
		e.Sleeping = false
	}
	if wasAsleep && !e.Sleeping && e.Type == corentity.TypeVillager && validBed {
		s.setVillagerBedOccupied(e, false)
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
		if s.world == nil || !s.navigateMob(e, ai, spatial.Vec3{X: ai.targetX, Y: e.Position.Y, Z: ai.targetZ}, pumpkinMovementSpeed(e.Type, 2.0)) {
			e.VX, e.VZ = ai.dirX*0.28, ai.dirZ*0.28
			e.Yaw = float32(math.Atan2(-ai.dirX, ai.dirZ) * 180 / math.Pi)
		}
		ai.sleepingWas = e.Sleeping
		return wasAsleep != e.Sleeping
	}
	if e.Sitting || len(e.PassengerIDs()) > 0 {
		clearMobNavigation(e, ai)
		ai.sleepingWas = e.Sleeping
		return wasAsleep != e.Sleeping
	}
	if e.BreedingMateEntityID != 0 && e.LoveTicks > 0 {
		if mate, ok := s.world.Entities.Get(e.BreedingMateEntityID); ok && !mate.Dead && mate.LoveTicks > 0 {
			dx, dz := mate.Position.X-e.Position.X, mate.Position.Z-e.Position.Z
			e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
			if dx*dx+dz*dz > 2.5*2.5 {
				s.navigateMob(e, ai, mate.Position, pumpkinMovementSpeed(e.Type, 1.0))
			} else {
				clearMobNavigation(e, ai)
			}
			ai.sleepingWas = e.Sleeping
			return wasAsleep != e.Sleeping
		}
	}
	if isAquaticMob(e.Type) {
		s.tickAquaticMobAI(e, ai)
		ai.sleepingWas = e.Sleeping
		return wasAsleep != e.Sleeping
	}
	// Pumpkin's priority-0 SwimGoal runs alongside MOVE goals.
	if s.entityInWater(e) && ai.rng.Float64() < 0.8 {
		e.VY = 0.12
	}

	// Assigned villagers return to their own bed at night and stay there until
	// morning. The canonical Sleeping flag is adapter-independent state.
	if e.Type == corentity.TypeVillager && e.HasVillageHome && validBed {
		dayTime := s.worldAge % 24000
		if dayTime >= 12000 && dayTime <= 23000 {
			targetX := float64(e.VillageBed.X) + 0.5
			targetY := float64(e.VillageBed.Y)
			targetZ := float64(e.VillageBed.Z) + 0.5
			dx, dy, dz := targetX-e.Position.X, targetY-e.Position.Y, targetZ-e.Position.Z
			distanceSquared := dx*dx + dy*dy + dz*dz
			bed := s.world.GetBlock(int(e.VillageBed.X), int(e.VillageBed.Y), int(e.VillageBed.Z))
			if distanceSquared <= 4 && (e.Sleeping || bed.Properties["occupied"] != "true") {
				if !e.Sleeping {
					s.setVillagerBedOccupied(e, true)
				}
				e.VX, e.VY, e.VZ = 0, 0, 0
				e.Sleeping = true
				clearMobNavigation(e, ai)
				changed := !wasAsleep
				ai.sleepingWas = true
				return changed
			}
			e.Sleeping = false
			if distanceSquared > 4 && !s.navigateMob(e, ai, spatial.Vec3{X: targetX, Y: targetY, Z: targetZ}, pumpkinMovementSpeed(e.Type, 1.0)) {
				distance := math.Hypot(dx, dz)
				if distance > 0 {
					e.VX, e.VZ = dx/distance*0.1, dz/distance*0.1
				}
				e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
			}
			changed := wasAsleep
			ai.sleepingWas = false
			return changed
		}
		e.Sleeping = false
		if wasAsleep {
			s.setVillagerBedOccupied(e, false)
			s.wakeVillagerBesideBed(e)
			ai.sleepingWas = false
			return true
		}
	}
	if !ai.roaming {
		dx, dz := e.Position.X-ai.homeX, e.Position.Z-ai.homeZ
		if dx*dx+dz*dz > 26*26 {
			ai.hasWanderGoal = false
			s.navigateMob(e, ai, spatial.Vec3{X: ai.homeX, Y: e.Position.Y, Z: ai.homeZ}, pumpkinMovementSpeed(e.Type, 1.0))
			ai.sleepingWas = e.Sleeping
			return wasAsleep != e.Sleeping
		}
	}
	s.tickPassiveIdleGoals(e, ai)
	ai.sleepingWas = e.Sleeping
	return wasAsleep != e.Sleeping

}

func (s *Server) tickVillagerBedClaim(e *corentity.Entity, ai *mobAI) {
	if ai.bedClaimTick <= 0 {
		s.claimVillagerBed(e)
		ai.bedClaimTick = 20
		return
	}
	ai.bedClaimTick--
}

func (s *Server) setVillagerBedOccupied(e *corentity.Entity, occupied bool) {
	bed := s.world.GetBlock(int(e.VillageBed.X), int(e.VillageBed.Y), int(e.VillageBed.Z))
	want := strconv.FormatBool(occupied)
	if !strings.HasSuffix(bed.ResourceLocation(), "_bed") || bed.Properties["occupied"] == want {
		return
	}
	bed.Properties = copyStringMap(bed.Properties)
	bed.Properties["occupied"] = want
	s.world.SetBlock(int(e.VillageBed.X), int(e.VillageBed.Y), int(e.VillageBed.Z), bed)
}

// validVillagerBed prevents a POI coordinate (or the zero value) from being
// treated as a bed. Legacy door-only villages used to copy their house door
// into VillageBed, which made villagers render the sleeping pose on doors and
// arbitrary replacement blocks.
func (s *Server) validVillagerBed(e *corentity.Entity) bool {
	if s == nil || s.world == nil || e == nil || e.Type != corentity.TypeVillager || !e.HasVillageHome || e.VillageBed == (spatial.BlockPos{}) {
		return false
	}
	bed := s.world.GetBlock(int(e.VillageBed.X), int(e.VillageBed.Y), int(e.VillageBed.Z))
	if !strings.HasSuffix(bed.ResourceLocation(), "_bed") {
		return false
	}
	part := bed.Properties["part"]
	return part == "" || part == "head"
}

// claimVillagerBed scans the ±16 X/Z, ±4 Y block neighbourhood for an
// unclaimed bed head, mirroring PumpkinMC's brute-force POI scan. When one is
// found the villager's HasVillageHome / VillageBed are set so it will navigate
// to the bed at night. The scan is cheap enough to run once per second.
func (s *Server) claimVillagerBed(e *corentity.Entity) {
	if s == nil || s.world == nil || e == nil || e.Type != corentity.TypeVillager {
		return
	}
	// If the villager already has a home, validate it; clear if the bed is gone.
	if e.HasVillageHome {
		if !s.validVillagerBed(e) {
			e.HasVillageHome = false
			e.VillageBed = spatial.BlockPos{}
		}
		return
	}
	// Build a set of already-claimed beds to skip.
	occupied := make(map[[3]int32]struct{})
	for _, other := range s.world.Entities.Snapshot() {
		if other.EntityID != e.EntityID && other.Type == corentity.TypeVillager && other.HasVillageHome {
			occupied[[3]int32{other.VillageBed.X, other.VillageBed.Y, other.VillageBed.Z}] = struct{}{}
		}
	}
	// Scan ±16 X/Z, ±4 Y for the nearest unclaimed bed head.
	px, py, pz := int(math.Floor(e.Position.X)), int(math.Floor(e.Position.Y)), int(math.Floor(e.Position.Z))
	bestDistSq := math.MaxFloat64
	found := false
	var best spatial.BlockPos
	for bx := px - 16; bx <= px+16; bx++ {
		for by := py - 4; by <= py+4; by++ {
			for bz := pz - 16; bz <= pz+16; bz++ {
				blk := s.world.GetBlock(bx, by, bz)
				if !strings.HasSuffix(blk.ResourceLocation(), "_bed") {
					continue
				}
				part := blk.Properties["part"]
				if part != "" && part != "head" {
					continue
				}
				key := [3]int32{int32(bx), int32(by), int32(bz)}
				if _, taken := occupied[key]; taken {
					continue
				}
				dx := float64(bx) + 0.5 - e.Position.X
				dy := float64(by) + 0.5 - e.Position.Y
				dz := float64(bz) + 0.5 - e.Position.Z
				distSq := dx*dx + dy*dy + dz*dz
				if distSq < bestDistSq {
					bestDistSq = distSq
					best = spatial.BlockPos{X: int32(bx), Y: int32(by), Z: int32(bz)}
					found = true
				}
			}
		}
	}
	if found {
		e.VillageBed = best
		e.HasVillageHome = true
		e.VillageCenter = spatial.BlockPos{X: best.X, Y: best.Y, Z: best.Z}
	}
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

	if !ai.hasTarget && e.Type == corentity.TypeIronGolem {
		nearest := 24.0
		for _, candidate := range s.world.Entities.Snapshot() {
			if candidate.Dead || candidate.Type != corentity.TypeZombie {
				continue
			}
			distance := math.Hypot(candidate.Position.X-e.Position.X, candidate.Position.Z-e.Position.Z)
			if distance < nearest {
				nearest = distance
				ai.hasTarget = true
				ai.targetEntityID = candidate.EntityID
				ai.targetX, ai.targetZ = candidate.Position.X, candidate.Position.Z
			}
		}
	}
	if !ai.hasTarget {
		// No target: wander like a homed passive mob near the village centre.
		_ = s.tickPassiveMobAI(e)
		return
	}
	ai.hasWanderGoal = false

	nearestDist := 24.0
	if ai.targetEntityID != 0 {
		if target, ok := s.world.Entities.Get(ai.targetEntityID); ok && !target.Dead && target.Type == corentity.TypeZombie {
			ai.targetX, ai.targetZ = target.Position.X, target.Position.Z
			nearestDist = math.Hypot(target.Position.X-e.Position.X, target.Position.Z-e.Position.Z)
		} else {
			ai.hasTarget = false
			ai.targetEntityID = 0
		}
	} else {
		// A player that struck the golem remains its revenge target.
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
	}
	if nearestDist >= 24.0 {
		// No player nearby — give up chase.
		ai.hasTarget = false
		ai.targetEntityID = 0
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
			if ai.targetEntityID != 0 {
				if target, ok := s.world.Entities.Get(ai.targetEntityID); ok && !target.Dead &&
					s.mobHasLineOfSight(e, target.Position, 1.4) {
					damage := float32(7 + ai.rng.Intn(15))
					s.world.QueueEntityDamageFrom(target.EntityID, damage, e.Position.X, e.Position.Z)
				}
				return
			}
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
				if !s.mobHasLineOfSight(e, sess.Player.Position, 1.62) {
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

	// Charge at the target through Pumpkin's navigator.
	if !s.navigateMob(e, ai, spatial.Vec3{X: ai.targetX, Y: e.Position.Y, Z: ai.targetZ}, pumpkinMovementSpeed(e.Type, 1.0)) {
		e.VX, e.VZ = dx/dist*0.14, dz/dist*0.14
		e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
	}
}

// tickHostileMobAI gives ground hostiles a common pursuit/melee controller.
// Creepers replace the melee swing with the vanilla 30-tick fuse.
func (s *Server) tickHostileMobAI(e *corentity.Entity) {
	ai := s.mobAIFor(e)
	var target *session.Session
	nearest := 16.0
	if settings, ok := pumpkinEntitySpawnSettingsByType[string(e.Type)]; ok && settings.followRange > 0 {
		nearest = settings.followRange
	}
	for _, candidate := range s.allPlayerSessions() {
		if candidate.Player == nil || candidate.Player.Dead ||
			candidate.Player.GameMode == player.GameModeCreative ||
			candidate.Player.GameMode == player.GameModeSpectator {
			continue
		}
		dx := candidate.Player.Position.X - e.Position.X
		dz := candidate.Player.Position.Z - e.Position.Z
		distance := math.Hypot(dx, dz)
		if distance < nearest && math.Abs(candidate.Player.Position.Y-e.Position.Y) <= 16 {
			// Endermen only target players who stare at them (unless already angered).
			if e.Type == corentity.TypeEnderman && !ai.angered {
				if !isPlayerStaringAtEnderman(candidate.Player, e) {
					continue
				}
				ai.angered = true
			}
			nearest = distance
			target = candidate
		}
	}
	if target == nil {
		if e.Type == corentity.TypeEnderman {
			ai.angered = false
		}
		if entityTarget := s.closestPumpkinEntityTarget(e, nearest); entityTarget != nil {
			s.tickHostileAgainstEntity(e, ai, entityTarget)
			return
		}
		if isSkeletonArcher(e.Type) {
			s.setMobUsingItem(e, false)
			ai.bowDrawTicks = 0
		}
		ai.targetEntityID = 0
		if e.Type == corentity.TypeCreeper && ai.fuseTick > 0 {
			ai.fuseTick -= 2
			if ai.fuseTick <= 0 {
				ai.fuseTick = 0
				handler.BroadcastCreeperSwell(e.EntityID, false, s.sessions)
			}
		}
		if isAquaticMob(e.Type) {
			s.tickAquaticMobAI(e, ai)
		} else {
			s.tickHostileIdleGoals(e, ai)
		}
		return
	}
	ai.hasWanderGoal = false

	dx := target.Player.Position.X - e.Position.X
	dz := target.Player.Position.Z - e.Position.Z
	distance := math.Hypot(dx, dz)
	visible := s.mobHasLineOfSight(e, target.Player.Position, 1.62)
	if distance > 0.001 {
		e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
	}

	if e.Type == corentity.TypeCreeper {
		if distance <= 3 && visible {
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

	if projectileType, speed, damage, attackRange, cooldown, sound, ranged := hostileRangedAttack(e.Type); ranged {
		if ai.attackCooldown > 0 {
			ai.attackCooldown--
		}
		if visible && distance <= attackRange && ai.attackCooldown == 0 {
			s.shootMobProjectile(e, target.Player, projectileType, speed, damage, sound)
			ai.attackCooldown = cooldown
		}
		if isFlyingMob(e.Type) {
			destinationY := target.Player.Position.Y + 2
			e.VY = math.Max(-0.15, math.Min(0.15, (destinationY-e.Position.Y)*0.04))
		}
		if distance <= attackRange*0.75 {
			e.VX, e.VZ = 0, 0
			return
		}
		if distance > 0.001 {
			s.navigateMob(e, ai, target.Player.Position, pumpkinMovementSpeed(e.Type, 1.0))
		}
		return
	}

	if isSkeletonArcher(e.Type) {
		if ai.attackCooldown > 0 {
			ai.attackCooldown--
		}
		if ai.bowDrawTicks > 0 {
			if !visible || distance > 15 {
				ai.bowDrawTicks = 0
				s.setMobUsingItem(e, false)
			} else {
				ai.bowDrawTicks--
				if ai.bowDrawTicks == 0 {
					s.setMobUsingItem(e, false)
					s.shootMobArrow(e, target.Player)
					ai.attackCooldown = 20
				}
			}
		}
		if visible && distance <= 15 && ai.attackCooldown == 0 && ai.bowDrawTicks == 0 {
			ai.bowDrawTicks = 20
			s.setMobUsingItem(e, true)
		}
		if distance <= 15 {
			e.VX, e.VZ = 0, 0
			return
		}
		if distance > 0.001 {
			s.navigateMob(e, ai, target.Player.Position, pumpkinMovementSpeed(e.Type, 1.0))
		}
		return
	}

	if ai.attackCooldown > 0 {
		ai.attackCooldown--
	}
	if distance <= 1.8 && e.Type != corentity.TypeCreeper && visible {
		e.VX, e.VZ = 0, 0
		if ai.attackCooldown == 0 {
			ai.attackCooldown = 20
			damage := hostileAttackDamage(e.Type)
			if settings, ok := pumpkinEntitySpawnSettingsByType[string(e.Type)]; ok && settings.attackDamage > 0 {
				damage = float32(settings.attackDamage)
			}
			healthBefore, _, _, _ := target.Player.HealthSnapshot()
			switch s.currentDifficulty() {
			case 1:
				damage *= 0.5
			case 3:
				damage *= 1.5
			}
			name := strings.ReplaceAll(strings.TrimPrefix(string(e.Type), "minecraft:"), "_", " ")
			handler.DamagePlayerFromSource(target, damage, "was slain by a "+name, s.sessions, e.Position.X, e.Position.Z)
			healthAfter, _, _, _ := target.Player.HealthSnapshot()
			if healthAfter < healthBefore {
				target.Player.LastAttackerEntityID = e.EntityID
				s.sendLegacyPlayerKnockback(target, e.Position.X, e.Position.Z, 0.4, 0.4)
			}
		}
		return
	}

	if distance > 0.001 {
		if isAquaticMob(e.Type) && s.entityInWater(e) {
			speed := pumpkinMovementSpeed(e.Type, 1.0)
			dy := target.Player.Position.Y - e.Position.Y
			fullDistance := math.Sqrt(dx*dx + dy*dy + dz*dz)
			if fullDistance > 0.001 {
				e.VX, e.VY, e.VZ = dx/fullDistance*speed, dy/fullDistance*speed, dz/fullDistance*speed
			}
			return
		}
		if s.entityInWater(e) && ai.rng.Float64() < 0.8 {
			e.VY = 0.12
		}
		modifier := 1.0
		switch e.Type {
		case corentity.TypeSkeleton, corentity.TypeBogged, corentity.TypeStray:
			modifier = 1.2
		}
		if !s.navigateMob(e, ai, target.Player.Position, pumpkinMovementSpeed(e.Type, modifier)) {
			e.VX, e.VZ = dx/distance*0.1, dz/distance*0.1
		}
	}
}

func hostileRangedAttack(t corentity.EntityType) (corentity.EntityType, float64, float32, float64, int, string, bool) {
	switch t {
	case corentity.TypeBlaze:
		return corentity.TypeSmallFireball, 0.75, 5, 16, 40, "minecraft:entity.blaze.shoot", true
	case corentity.TypeGhast:
		return corentity.TypeFireball, 0.6, 0, 32, 60, "minecraft:entity.ghast.shoot", true
	case corentity.TypeBreeze:
		return corentity.TypeWindCharge, 0.7, 1, 24, 30, "minecraft:entity.breeze.shoot", true
	case corentity.TypeWitch:
		return corentity.TypePotion, 0.75, 6, 10, 60, "minecraft:entity.witch.throw", true
	case corentity.TypePillager, corentity.TypeIllusioner:
		return corentity.TypeArrow, 1.6, 4, 15, 40, "minecraft:entity.arrow.shoot", true
	}
	return "", 0, 0, 0, 0, "", false
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
	if projectile.Type == corentity.TypeEyeOfEnder {
		expired := tickEyeOfEnder(projectile)
		if expired {
			s.expireEyeOfEnder(projectile)
		}
		return expired
	}
	if projectile.AgeTicks > 1200 {
		return true
	}
	start := projectile.Position
	switch projectile.Type {
	case corentity.TypeWindCharge, corentity.TypeSmallFireball, corentity.TypeFireball:
		// Constant-velocity projectiles do not fall.
	case corentity.TypeSnowball, corentity.TypeEgg, corentity.TypeEnderPearl:
		projectile.VY -= 0.03
	case corentity.TypeExperienceBottle, corentity.TypePotion:
		projectile.VY -= 0.07
	default:
		projectile.VY -= 0.05
	}
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
			if block.ResourceLocation() == "minecraft:bell" {
				face := coreworld.BellProjectileFace(
					projectile.Position.X-start.X, projectile.Position.Y-start.Y, projectile.Position.Z-start.Z,
				)
				if direction, valid := coreworld.BellRingDirection(block, face, float32(y-math.Floor(y))); valid {
					position := spatial.BlockPos{X: int32(math.Floor(x)), Y: int32(math.Floor(y)), Z: int32(math.Floor(z))}
					s.ringBell(s.world, s.simulationDimension, position, direction)
				}
			}
			if projectile.Type == corentity.TypeWindCharge {
				s.explodeWindCharge(projectile, spatial.Vec3{X: x, Y: y, Z: z})
			}
			s.resolveProjectileImpact(projectile, spatial.Vec3{X: x, Y: y, Z: z})
			if projectile.Type == corentity.TypeArrow || projectile.Type == corentity.TypeSpectralArrow || projectile.Type == corentity.TypeTrident {
				handler.BroadcastSoundAt(s.sessions, "minecraft:entity.arrow.hit", handler.SoundCategoryPlayers, x, y, z, 1, 1)
			}
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
		if projectile.Type == corentity.TypeWindCharge {
			s.explodeWindCharge(projectile, projectile.Position)
		}
		s.resolveProjectileImpact(projectile, projectile.Position)
		return true
	}

	for _, target := range s.world.Entities.Snapshot() {
		if target == projectile || target.Dead || target.EntityID == projectile.OwnerEntityID ||
			corentity.IsProjectile(target.Type) {
			continue
		}
		centre := spatial.Vec3{X: target.Position.X, Y: target.Position.Y + 0.8, Z: target.Position.Z}
		if pointSegmentDistanceSquared(centre, start, projectile.Position) <= 0.8*0.8 {
			if projectile.Type == corentity.TypeWindCharge {
				s.explodeWindCharge(projectile, projectile.Position)
			} else if damage := projectileDamageAgainst(projectile, target); damage > 0 {
				if owner := s.playerByEntityID(projectile.OwnerEntityID); owner != nil {
					s.world.QueueEntityDamageFromPlayer(target.EntityID, damage, start.X, start.Z, owner.UUID)
				} else {
					s.world.QueueEntityDamageFrom(target.EntityID, damage, start.X, start.Z)
				}
			} else if projectile.Type == corentity.TypeSnowball {
				s.world.QueueEntityImpactFrom(target.EntityID, start.X, start.Z)
			}
			s.resolveProjectileImpact(projectile, projectile.Position)
			return true
		}
	}
	return false
}

func (s *Server) playerByEntityID(entityID int32) *player.Player {
	if s == nil || s.game == nil || entityID == 0 {
		return nil
	}
	var found *player.Player
	s.game.OnlinePlayers(func(candidate *player.Player) {
		if candidate.EntityID == entityID {
			found = candidate
		}
	})
	return found
}

// tickEyeOfEnder mirrors Pumpkin's no-gravity movement and homing blend.
func tickEyeOfEnder(eye *corentity.Entity) bool {
	oldVX, oldVY, oldVZ := eye.VX, eye.VY, eye.VZ
	eye.Position.X += oldVX
	eye.Position.Y += oldVY
	eye.Position.Z += oldVZ
	if eye.HasEyeTarget {
		dx := eye.EyeTarget.X - eye.Position.X
		dz := eye.EyeTarget.Z - eye.Position.Z
		horizontalDistance := math.Hypot(dx, dz)
		if horizontalDistance > 0 {
			oldSpeed := math.Hypot(oldVX, oldVZ)
			wantedSpeed := oldSpeed + 0.0025*(horizontalDistance-oldSpeed)
			moveY := oldVY
			if horizontalDistance < 1 {
				wantedSpeed *= 0.8
				moveY *= 0.8
			}
			wantedY := -1.0
			if eye.Position.Y-oldVY < eye.EyeTarget.Y {
				wantedY = 1
			}
			eye.VX = dx / horizontalDistance * wantedSpeed
			eye.VY = moveY + (wantedY-moveY)*0.015
			eye.VZ = dz / horizontalDistance * wantedSpeed
		}
	}
	horizontalSpeed := math.Hypot(eye.VX, eye.VZ)
	eye.Yaw = float32(math.Atan2(-eye.VX, eye.VZ) * 180 / math.Pi)
	eye.Pitch = float32(math.Atan2(-eye.VY, horizontalSpeed) * 180 / math.Pi)
	return eye.AgeTicks > 80
}

func (s *Server) expireEyeOfEnder(eye *corentity.Entity) {
	if s == nil || eye == nil {
		return
	}
	handler.BroadcastSoundAt(s.sessions, "minecraft:entity.ender_eye.death", handler.SoundCategoryNeutral,
		eye.Position.X, eye.Position.Y, eye.Position.Z, 1, 1)
	if !eye.EyeSurvives || s.game == nil || s.world == nil {
		return
	}
	dropped := s.newDroppedItemInWorld(s.world, player.ItemStack{ItemID: "minecraft:ender_eye", Count: 1}, eye.Position, 0)
	if dropped != nil {
		handler.BroadcastSpawnMob(dropped, s.sessions)
	}
}

func projectileDamageAgainst(projectile, target *corentity.Entity) float32 {
	if projectile == nil || target == nil {
		return 0
	}
	if projectile.Type == corentity.TypeSnowball {
		if target.Type == corentity.TypeBlaze {
			return 3
		}
		return 0
	}
	return projectile.ProjectileDamage
}

func (s *Server) resolveProjectileImpact(projectile *corentity.Entity, position spatial.Vec3) {
	if projectile == nil {
		return
	}
	switch projectile.Type {
	case corentity.TypeEnderPearl:
		for _, target := range s.allPlayerSessions() {
			if target.Player == nil || target.Player.EntityID != projectile.OwnerEntityID || target.Player.Dead {
				continue
			}
			destination := position
			destination.Y = math.Max(float64(coreworld.WorldMinY+1), math.Min(float64(coreworld.WorldMaxY-2), destination.Y))
			if ok, loaded := s.world.CanEntityOccupyIfLoaded(destination.X, destination.Y, destination.Z); loaded && !ok {
				destination.Y = math.Floor(destination.Y) + 1
			}
			target.Player.Position = destination
			target.Player.FallDistance = 0
			if target.Conn != nil && target.TeleportTo != nil {
				_ = target.TeleportTo(destination.X, destination.Y, destination.Z)
			} else if s.bedrockListener != nil {
				s.bedrockListener.TeleportPlayer(target.Player, destination, uint64(s.worldAge))
			}
			handler.DamagePlayerLegacy(target, 5, "hit the ground too hard", s.sessions)
			handler.BroadcastSoundAt(s.sessions, "minecraft:entity.enderman.teleport", handler.SoundCategoryPlayers,
				destination.X, destination.Y, destination.Z, 1, 1)
			return
		}
	case corentity.TypeEgg:
		if s.game == nil {
			return
		}
		roll := int(projectile.EntityID*1103515245+12345) & 0x7fffffff
		if roll%8 != 0 {
			return
		}
		count := 1
		if roll%32 == 0 {
			count = 4
		}
		for index := 0; index < count; index++ {
			chicken := corentity.New(s.game.NextEntityID(), newRandomUUID(), corentity.TypeChicken,
				position.X+float64(index%2)*0.2, position.Y, position.Z+float64(index/2)*0.2)
			chicken.IsBaby = true
			s.world.Entities.Add(chicken)
			handler.BroadcastSpawnMob(chicken, s.sessions)
		}
	case corentity.TypeSmallFireball:
		x, y, z := int(math.Floor(position.X)), int(math.Floor(position.Y)), int(math.Floor(position.Z))
		for _, candidate := range [][3]int{{x, y + 1, z}, {x, y, z}, {x + 1, y, z}, {x - 1, y, z}, {x, y, z + 1}, {x, y, z - 1}} {
			if s.world.GetBlock(candidate[0], candidate[1], candidate[2]).ResourceLocation() != "minecraft:air" {
				continue
			}
			below := s.world.GetBlock(candidate[0], candidate[1]-1, candidate[2])
			if !coreworld.IsEntitySupportBlock(below.ResourceLocation()) {
				continue
			}
			fire := coreworld.Block{Namespace: "minecraft", Name: "fire", Properties: map[string]string{"age": "0"}}
			s.world.SetBlock(candidate[0], candidate[1], candidate[2], fire)
			handler.BroadcastBlockChange(coreworld.BlockChange{X: candidate[0], Y: candidate[1], Z: candidate[2], Block: fire}, s.sessions)
			break
		}
		handler.BroadcastSoundAt(s.sessions, "minecraft:entity.blaze.burn", handler.SoundCategoryHostile,
			position.X, position.Y, position.Z, 1, 1)
	case corentity.TypeFireball:
		s.explodeAt(position.X, position.Y, position.Z, 1, "was fireballed")
	case corentity.TypePotion:
		if projectile.ProjectileItem.ItemID == "minecraft:lingering_potion" {
			s.applySplashPotionScaled(projectile.ProjectileItem, position, 0.25)
			if s.game != nil && s.world != nil {
				cloud := corentity.NewAreaEffectCloud(s.game.NextEntityID(), newRandomUUID(),
					position.X, position.Y, position.Z, projectile.ProjectileItem)
				s.world.Entities.Add(cloud)
				handler.BroadcastSpawnMobInDimension(cloud, s.sessions, s.simulationDimension)
			}
		} else {
			s.applySplashPotion(projectile.ProjectileItem, position)
		}
		handler.BroadcastSoundAt(s.sessions, "minecraft:entity.splash_potion.break", handler.SoundCategoryHostile,
			position.X, position.Y, position.Z, 1, 1)
	case corentity.TypeExperienceBottle:
		handler.BroadcastSoundAt(s.sessions, "minecraft:entity.splash_potion.break", handler.SoundCategoryPlayers,
			position.X, position.Y, position.Z, 1, 1)
		reward := int32(3)
		if s.spawnRNG != nil {
			reward += int32(s.spawnRNG.Intn(5) + s.spawnRNG.Intn(5))
		}
		for _, orb := range coreexperience.SpawnOrbs(s.world, s.game.NextEntityID, position, reward) {
			handler.BroadcastSpawnMobInDimension(orb, s.sessions, s.simulationDimension)
		}
	}
}

func (s *Server) explodeWindCharge(projectile *corentity.Entity, position spatial.Vec3) {
	const radius = 5.0
	handler.BroadcastSoundAt(s.sessions, "minecraft:entity.wind_charge.wind_burst", handler.SoundCategoryPlayers,
		position.X, position.Y, position.Z, 1, 1)
	if s.bedrockListener != nil {
		s.bedrockListener.BroadcastWindChargeSound(position, true)
	}
	if s.world != nil {
		s.world.EmitVibration(int(math.Floor(position.X)), int(math.Floor(position.Y)), int(math.Floor(position.Z)))
	}
	for _, target := range s.allPlayerSessions() {
		if target == nil || target.Player == nil || target.Player.Dead {
			continue
		}
		dx := target.Player.Position.X - position.X
		dy := target.Player.Position.Y + 0.9 - position.Y
		dz := target.Player.Position.Z - position.Z
		distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if distance >= radius {
			continue
		}
		if distance < 0.1 {
			dx, dy, dz, distance = 0, 1, 0, 1
		}
		strength := (1 - distance/radius) * 1.35
		s.sendPlayerVelocity(target, dx/distance*strength, max(0.35, dy/distance*strength+0.35), dz/distance*strength)
	}
	for _, target := range s.world.Entities.Snapshot() {
		if target == projectile || target.Dead || corentity.IsProjectile(target.Type) {
			continue
		}
		dx, dy, dz := target.Position.X-position.X, target.Position.Y+0.8-position.Y, target.Position.Z-position.Z
		distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if distance >= radius {
			continue
		}
		if distance < 0.1 {
			dx, dy, dz, distance = 0, 1, 0, 1
		}
		strength := (1 - distance/radius) * 1.35
		target.VX += dx / distance * strength
		target.VY += max(0.35, dy/distance*strength+0.35)
		target.VZ += dz / distance * strength
		target.OnGround = false
	}
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
		if err := handler.HandleStatus(conn, s.cfg, s.statusFavicon); err != nil {
			slog.Debug("status error", "remote", remote, "err", err)
		}

	case network.StateLogin:
		result, err := s.loginHandler.Handle(conn)
		if err != nil {
			slog.Debug("login error", "remote", remote, "err", err)
			return
		}

		// ── Configuration state ──────────────────────────────────────────────
		if err := handler.HandleConfiguration(conn, s.regProvider, s.cfg.ResourcePack.Java); err != nil {
			slog.Warn("configuration error", "remote", remote, "err", err)
			return
		}

		// ── Play state ───────────────────────────────────────────────────────
		p := s.registerPlayer(result, remote.String())
		defer func() {
			if p.VehicleEntityID != 0 {
				s.dismountPlayer(p)
			}
			s.savePlayerData(p)
			s.game.RemovePlayer(p.UUID)
			handler.OnlineCount.Store(int32(s.game.OnlineCount()))
		}()

		if err := handler.HandlePlay(conn, p, s.world, s.worldForDimension, s.chunkSender, s.sessions, s.cmds, s.regProvider, s.cfg.WorldSeed, func() int64 { return s.worldAge }, int32(s.cfg.ViewDistance), int32(s.cfg.PreGenerateRadius), s.game.NextEntityID, s.intentBus, s.plugins); err != nil {
			slog.Debug("play error", "remote", remote, "err", err)
		}

	default:
		slog.Warn("unhandled state after handshake", "remote", remote, "state", conn.State)
	}
}

// registerPlayer creates a core Player from a LoginResult, assigns an entity ID
// via the game core, and updates the global online count used in status pings.
func (s *Server) registerPlayer(result *handler.LoginResult, remoteAddress string) *player.Player {
	// protocol.UUID is [16]byte — convertible to the core's raw [16]byte UUID.
	p := player.New([16]byte(result.UUID), result.Name, player.ClientEditionJava)
	p.RemoteAddress = remoteAddress
	p.Raining, p.Thundering = s.currentWeather()
	p.Operator = handler.IsOperatorName(result.Name)
	p.InvulnerableUntil = time.Now().Add(3 * time.Second)
	p.GameMode = player.GameMode(s.defaultGameMode.Load())
	p.AttackCooldown = s.cfg.Combat.AttackCooldown
	p.KnockbackHorizontal = s.cfg.Combat.KnockbackHorizontal
	p.KnockbackVertical = s.cfg.Combat.KnockbackVertical
	p.OnDeath = s.dropPlayerInventory
	p.Position = s.currentWorldSpawn()
	p.WorldSpawn = p.Position
	s.loadPlayerData(p)

	if err := s.game.AddPlayer(p); err != nil {
		// Duplicate UUID — extremely rare; log and continue with assigned ID.
		slog.Warn("duplicate player UUID", "name", p.Username, "uuid", p.UUID, "err", err)
	}
	s.announceJoinWhenReachable(p)

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
	for dimension, dimensionWorld := range map[int32]*coreworld.World{
		dimensionOverworld: s.world,
		dimensionNether:    s.netherWorld,
		dimensionEnd:       s.endWorld,
	} {
		if dimensionWorld == nil {
			continue
		}
		simulation := s.dimensionSimulation(dimension, dimensionWorld)
		simulation.tickBlockPhysicsWorld()
	}
}

func (s *Server) tickBlockPhysicsWorld() {
	due := s.world.BlockPhysics.DrainDue(s.worldAge)
	blockChanges := s.activateSculkSensors(s.world.DrainVibrations())

	// Flush redstone — may produce visual changes and newly-powered loads.
	redstone := s.world.Redstone.FlushUpdates()

	if len(due) == 0 && len(redstone.Changes) == 0 && len(redstone.PoweredLoads) == 0 && len(redstone.UnpoweredLoads) == 0 && len(blockChanges) == 0 {
		return
	}

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
		case coreworld.UpdateButton:
			s.processButtonUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateComposter:
			s.processComposterUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdatePressurePlate:
			s.processPressurePlateUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateSculkSensor:
			s.processSculkSensorUpdate(u.X, u.Y, u.Z, &blockChanges)
		case coreworld.UpdateCoralDeath:
			if change, ok := s.world.ApplyCoralDeath(u.X, u.Y, u.Z); ok {
				blockChanges = append(blockChanges, change)
			}
		case coreworld.UpdateObserver:
			observer := s.world.GetBlock(u.X, u.Y, u.Z)
			if observer.ResourceLocation() == "minecraft:observer" {
				observer = bedrockCopyBlock(observer)
				if observer.Properties["powered"] == "true" {
					observer.Properties["powered"] = "false"
				} else {
					observer.Properties["powered"] = "true"
					s.world.BlockPhysics.ScheduleObserver(u.X, u.Y, u.Z, s.worldAge, 2)
				}
				s.world.SetBlock(u.X, u.Y, u.Z, observer)
				blockChanges = append(blockChanges, coreworld.BlockChange{X: u.X, Y: u.Y, Z: u.Z, Block: observer})
			}
		}
	}

	// Activate loads that just received redstone power.
	for _, pos := range redstone.PoweredLoads {
		block := s.world.GetBlock(pos[0], pos[1], pos[2])
		switch block.ResourceLocation() {
		case "minecraft:tnt":
			s.activateTNT(pos[0], pos[1], pos[2], &blockChanges)
		case "minecraft:dropper", "minecraft:dispenser":
			s.activateDropperOrDispenser(s.world, s.simulationDimension, pos[0], pos[1], pos[2], block.ResourceLocation(), &blockChanges)
		case "minecraft:crafter":
			s.activateCrafter(s.world, s.simulationDimension, pos[0], pos[1], pos[2])
		case "minecraft:bell":
			position := spatial.BlockPos{X: int32(pos[0]), Y: int32(pos[1]), Z: int32(pos[2])}
			s.ringBell(s.world, s.simulationDimension, position, coreworld.BellFacingDirection(block))
		case "minecraft:piston", "minecraft:sticky_piston":
			blockChanges = append(blockChanges, s.world.ApplyPistonPower(pos[0], pos[1], pos[2], true)...)
		case "minecraft:note_block":
			s.playNoteBlock(pos[0], pos[1], pos[2], block)
		}
		// Pistons are handled by their dedicated movement system.
	}
	for _, pos := range redstone.UnpoweredLoads {
		block := s.world.GetBlock(pos[0], pos[1], pos[2])
		if block.ResourceLocation() == "minecraft:piston" || block.ResourceLocation() == "minecraft:sticky_piston" {
			blockChanges = append(blockChanges, s.world.ApplyPistonPower(pos[0], pos[1], pos[2], false)...)
		}
	}

	blockChanges = append(blockChanges, redstone.Changes...)
	neighborChanges := append([]coreworld.BlockChange(nil), blockChanges...)
	for _, change := range neighborChanges {
		blockChanges = append(blockChanges, s.world.BreakUnsupportedCropsAbove(change.X, change.Y, change.Z)...)
		blockChanges = append(blockChanges, s.world.BreakUnsupportedCocoaAdjacentTo(change.X, change.Y, change.Z)...)
		blockChanges = append(blockChanges, s.world.UpdateAttachedStemsAround(change.X, change.Y, change.Z)...)
		blockChanges = append(blockChanges, s.world.UpdateBubbleColumnsAround(change.X, change.Y, change.Z)...)
	}

	// Broadcast all block changes to clients in one go.
	for _, bc := range blockChanges {
		handler.BroadcastBlockChange(bc, s.sessions)
	}
}

func (s *Server) activateSculkSensors(events [][3]int) []coreworld.BlockChange {
	if s == nil || s.world == nil || len(events) == 0 {
		return nil
	}
	activated := make(map[[3]int]struct{})
	changes := make([]coreworld.BlockChange, 0)
	for _, event := range events {
		for y := max(coreworld.WorldMinY, event[1]-8); y <= min(coreworld.WorldMaxY, event[1]+8); y++ {
			for z := event[2] - 8; z <= event[2]+8; z++ {
				for x := event[0] - 8; x <= event[0]+8; x++ {
					dx, dy, dz := x-event[0], y-event[1], z-event[2]
					distanceSquared := dx*dx + dy*dy + dz*dz
					chunkX, chunkZ := int32(math.Floor(float64(x)/16)), int32(math.Floor(float64(z)/16))
					if distanceSquared > 64 || !s.world.IsChunkLoaded(chunkX, chunkZ) {
						continue
					}
					position := [3]int{x, y, z}
					if _, exists := activated[position]; exists {
						continue
					}
					block := s.world.GetBlock(x, y, z)
					name := block.ResourceLocation()
					phase := block.Properties["sculk_sensor_phase"]
					if (name != "minecraft:sculk_sensor" && name != "minecraft:calibrated_sculk_sensor") ||
						(phase != "" && phase != "inactive") {
						continue
					}
					replacement := bedrockCopyBlock(block)
					replacement.Properties["sculk_sensor_phase"] = "active"
					distance := math.Sqrt(float64(distanceSquared))
					power := max(1, 15-int(math.Floor(distance*15/8)))
					replacement.Properties["power"] = strconv.Itoa(power)
					s.world.SetBlock(x, y, z, replacement)
					s.world.Redstone.NotifyChange(x, y, z)
					s.world.BlockPhysics.ScheduleSculkSensor(x, y, z, s.worldAge, 30)
					activated[position] = struct{}{}
					changes = append(changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: replacement})
					handler.BroadcastSoundAt(s.sessions, "minecraft:block.sculk_sensor.clicking", handler.SoundCategoryBlocks,
						float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
					if s.bedrockListener != nil {
						s.bedrockListener.BroadcastSculkSensorSound(spatial.Vec3{X: float64(x) + 0.5, Y: float64(y) + 0.5, Z: float64(z) + 0.5}, true)
					}
				}
			}
		}
	}
	return changes
}

func (s *Server) processSculkSensorUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	name := block.ResourceLocation()
	if name != "minecraft:sculk_sensor" && name != "minecraft:calibrated_sculk_sensor" {
		return
	}
	replacement := bedrockCopyBlock(block)
	switch block.Properties["sculk_sensor_phase"] {
	case "active":
		replacement.Properties["sculk_sensor_phase"] = "cooldown"
		replacement.Properties["power"] = "0"
		s.world.BlockPhysics.ScheduleSculkSensor(x, y, z, s.worldAge, 10)
		handler.BroadcastSoundAt(s.sessions, "minecraft:block.sculk_sensor.clicking_stop", handler.SoundCategoryBlocks,
			float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
		if s.bedrockListener != nil {
			s.bedrockListener.BroadcastSculkSensorSound(spatial.Vec3{X: float64(x) + 0.5, Y: float64(y) + 0.5, Z: float64(z) + 0.5}, false)
		}
	case "cooldown":
		replacement.Properties["sculk_sensor_phase"] = "inactive"
		replacement.Properties["power"] = "0"
	default:
		return
	}
	s.world.SetBlock(x, y, z, replacement)
	s.world.Redstone.NotifyChange(x, y, z)
	*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: replacement})
}

func (s *Server) processButtonUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if !strings.HasSuffix(block.ResourceLocation(), "_button") || block.Properties["powered"] != "true" {
		return
	}
	block = bedrockCopyBlock(block)
	block.Properties["powered"] = "false"
	s.world.SetBlock(x, y, z, block)
	*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: block})
}

func (s *Server) processComposterUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	if block.ResourceLocation() != "minecraft:composter" || block.Properties["level"] != "7" {
		return
	}
	block = bedrockCopyBlock(block)
	block.Properties["level"] = "8"
	s.world.SetBlock(x, y, z, block)
	*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: block})
}

func (s *Server) processPressurePlateUpdate(x, y, z int, changes *[]coreworld.BlockChange) {
	block := s.world.GetBlock(x, y, z)
	name := block.ResourceLocation()
	if !strings.HasSuffix(name, "_pressure_plate") {
		return
	}
	occupants := 0
	if s.game != nil {
		s.game.OnlinePlayers(func(p *player.Player) {
			if !p.Dead && p.GameMode != player.GameModeSpectator && p.Position.X >= float64(x) && p.Position.X < float64(x+1) &&
				p.Position.Z >= float64(z) && p.Position.Z < float64(z+1) && p.Position.Y >= float64(y) && p.Position.Y < float64(y)+1.5 {
				occupants++
			}
		})
	}
	for _, entity := range s.world.Entities.Snapshot() {
		if entity.Position.X >= float64(x) && entity.Position.X < float64(x+1) && entity.Position.Z >= float64(z) &&
			entity.Position.Z < float64(z+1) && entity.Position.Y >= float64(y) && entity.Position.Y < float64(y)+1.5 {
			occupants++
		}
	}
	replacement := bedrockCopyBlock(block)
	changed := false
	if name == "minecraft:light_weighted_pressure_plate" || name == "minecraft:heavy_weighted_pressure_plate" {
		power := min(occupants, 15)
		if name == "minecraft:heavy_weighted_pressure_plate" && occupants > 0 {
			power = max(1, (occupants*15+149)/150)
		}
		next := strconv.Itoa(power)
		changed = block.Properties["power"] != next
		replacement.Properties["power"] = next
	} else {
		next := strconv.FormatBool(occupants > 0)
		changed = block.Properties["powered"] != next
		replacement.Properties["powered"] = next
	}
	if changed {
		s.world.SetBlock(x, y, z, replacement)
		*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: replacement})
	}
	s.world.BlockPhysics.SchedulePressurePlate(x, y, z, s.worldAge, 2)
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
	changeStart := len(*changes)
	defer func() {
		fluidChanges := append([]coreworld.BlockChange(nil), (*changes)[changeStart:]...)
		for _, change := range fluidChanges {
			*changes = append(*changes, s.world.UpdateBubbleColumnsAround(change.X, change.Y, change.Z)...)
		}
	}()

	dropOff, spreadDelay := fluidSpreadRules(name, s.simulationDimension)
	if name == "minecraft:lava" {
		for _, offset := range [5][3]int{{1, 0, 0}, {-1, 0, 0}, {0, 0, 1}, {0, 0, -1}, {0, 1, 0}} {
			if s.world.GetBlock(x+offset[0], y+offset[1], z+offset[2]).ResourceLocation() == "minecraft:water" {
				result := hardenedLava(level)
				s.world.SetBlock(x, y, z, result)
				*changes = append(*changes, coreworld.BlockChange{X: x, Y: y, Z: z, Block: result})
				return
			}
		}
	}
	if name == "minecraft:water" {
		for _, offset := range [5][3]int{{1, 0, 0}, {-1, 0, 0}, {0, 0, 1}, {0, 0, -1}, {0, -1, 0}} {
			nx, ny, nz := x+offset[0], y+offset[1], z+offset[2]
			lava := s.world.GetBlock(nx, ny, nz)
			if lava.ResourceLocation() == "minecraft:lava" {
				result := hardenedLava(coreworld.FluidLevel(lava))
				s.world.SetBlock(nx, ny, nz, result)
				*changes = append(*changes, coreworld.BlockChange{X: nx, Y: ny, Z: nz, Block: result})
			}
		}
	}

	// Try to fall down first — falling fluid keeps the same level.
	below := s.world.GetBlock(x, y-1, z)
	belowName := below.ResourceLocation()

	// Water+lava collision below.
	if opposite := fluidOpposite(name); belowName == opposite {
		result := coreworld.Block{Namespace: "minecraft", Name: "stone"}
		if name == "minecraft:water" {
			result = hardenedLava(coreworld.FluidLevel(below))
		}
		s.world.SetBlock(x, y-1, z, result)
		*changes = append(*changes, coreworld.BlockChange{X: x, Y: y - 1, Z: z, Block: result})
		if name == "minecraft:lava" {
			return
		}
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
				newBlock := coreworld.MakeFluid(name, 8)
				s.world.SetBlock(x, y-1, z, newBlock)
				*changes = append(*changes, coreworld.BlockChange{X: x, Y: y - 1, Z: z, Block: newBlock})
				s.world.BlockPhysics.ScheduleFluid(x, y-1, z, s.worldAge, spreadDelay)
				return
			}
		}
	}

	// Spread horizontally if not too diluted.
	newLevel := level + dropOff
	if level >= 8 {
		newLevel = dropOff
	}
	if newLevel <= 7 {
		dirs := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
		for _, d := range dirs {
			nx, nz := x+d[0], z+d[1]
			nb := s.world.GetBlock(nx, y, nz)
			nbName := nb.ResourceLocation()

			// Water+lava collision horizontal.
			if opposite := fluidOpposite(name); nbName == opposite {
				if name == "minecraft:lava" {
					continue
				}
				result := hardenedLava(coreworld.FluidLevel(nb))
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

// fluidSpreadRules mirrors Pumpkin's level drop-off and ultrawarm timing.
func fluidSpreadRules(name string, dimension int32) (dropOff int, delay int64) {
	if name != "minecraft:lava" {
		return 1, 5
	}
	if dimension == dimensionNether {
		return 1, 10
	}
	return 2, 30
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

// hardenedLava returns obsidian for a source and cobblestone for flowing lava.
func hardenedLava(level int) coreworld.Block {
	if level == 0 {
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
	tnt := corentity.NewPrimedTNT(id, uuid,
		float64(x)+0.5, float64(y), float64(z)+0.5)
	s.world.Entities.Add(tnt)
	handler.BroadcastSpawnMob(tnt, s.sessions)
}

// explodeTNT applies an explosion centred at (cx,cy,cz) with radius 4.
// Destroys blocks in a rough sphere, damages nearby entities, and broadcasts
// the block changes to all clients.
func (s *Server) explodeTNT(cx, cy, cz float64) {
	s.explodeAt(cx, cy, cz, 4, "blew up")
}

// explosionBlastResistance returns the blast resistance for a block resource
// location. Values follow the vanilla Minecraft table (most blocks 0-6,
// stone/ores 6, obsidian/reinforced-deepslate 1200). Bedrock is represented
// as math.MaxFloat64 so rays always terminate at it.
func explosionBlastResistance(name string) float64 {
	switch name {
	case "minecraft:bedrock", "minecraft:barrier", "minecraft:command_block",
		"minecraft:chain_command_block", "minecraft:repeating_command_block",
		"minecraft:structure_block", "minecraft:jigsaw":
		return math.MaxFloat64
	case "minecraft:obsidian", "minecraft:crying_obsidian",
		"minecraft:reinforced_deepslate", "minecraft:end_portal_frame",
		"minecraft:end_portal", "minecraft:end_gateway":
		return 1200
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil":
		return 1200
	case "minecraft:ender_chest":
		return 22.5
	case "minecraft:water", "minecraft:lava":
		return 100
	case "minecraft:stone", "minecraft:cobblestone", "minecraft:stone_bricks",
		"minecraft:mossy_cobblestone", "minecraft:mossy_stone_bricks",
		"minecraft:cracked_stone_bricks", "minecraft:chiseled_stone_bricks",
		"minecraft:infested_stone", "minecraft:deepslate",
		"minecraft:cobbled_deepslate", "minecraft:polished_deepslate",
		"minecraft:deepslate_bricks", "minecraft:cracked_deepslate_bricks",
		"minecraft:deepslate_tiles", "minecraft:cracked_deepslate_tiles",
		"minecraft:chiseled_deepslate", "minecraft:smooth_stone",
		"minecraft:granite", "minecraft:polished_granite",
		"minecraft:diorite", "minecraft:polished_diorite",
		"minecraft:andesite", "minecraft:polished_andesite",
		"minecraft:bricks", "minecraft:nether_bricks",
		"minecraft:red_nether_bricks", "minecraft:sandstone",
		"minecraft:red_sandstone", "minecraft:end_stone",
		"minecraft:end_stone_bricks", "minecraft:purpur_block",
		"minecraft:purpur_pillar":
		return 6
	case "minecraft:iron_block", "minecraft:gold_block",
		"minecraft:diamond_block", "minecraft:emerald_block",
		"minecraft:netherite_block":
		return 6
	// Default: soft blocks (dirt, sand, wood, leaves, etc.) ≈ 0.
	default:
		return 0
	}
}

func (s *Server) explodeAt(cx, cy, cz, radius float64, playerDeathCause string) {
	// Vanilla ray-cast explosion algorithm (converted from PumpkinMC explosion.rs).
	// Cast rays from all points on the outer face of a 16×16×16 cube centred on
	// the explosion. Each ray gets a randomised power and loses energy from block
	// resistance as it travels in 0.3-block steps.
	type blockKey struct{ x, y, z int }
	destroyed := make(map[blockKey]bool)

	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			for z := 0; z < 16; z++ {
				// Only the outer shell of the 16³ cube.
				if x > 0 && x < 15 && y > 0 && y < 15 && z > 0 && z < 15 {
					continue
				}
				dirX := float64(x)/7.5 - 1.0
				dirY := float64(y)/7.5 - 1.0
				dirZ := float64(z)/7.5 - 1.0
				length := math.Sqrt(dirX*dirX + dirY*dirY + dirZ*dirZ)
				if length == 0 {
					continue
				}
				dirX /= length
				dirY /= length
				dirZ /= length

				posX, posY, posZ := cx, cy, cz
				// Per-ray random power: power × (rand×0.6 + 0.7), matching vanilla.
				h := radius * (rand.Float64()*0.6 + 0.7) //nolint:gosec

				for h > 0 {
					bx := int(math.Floor(posX))
					by := int(math.Floor(posY))
					bz := int(math.Floor(posZ))

					block := s.world.GetBlock(bx, by, bz)
					name := block.ResourceLocation()
					if name != "" && name != "minecraft:air" {
						resistance := explosionBlastResistance(name)
						if resistance >= math.MaxFloat64 {
							break // bedrock and similar — ray terminates
						}
						h -= (resistance + 0.3) * 0.3
						if h > 0 {
							destroyed[blockKey{bx, by, bz}] = true
						}
					}
					posX += dirX * 0.3
					posY += dirY * 0.3
					posZ += dirZ * 0.3
					h -= 0.225
				}
			}
		}
	}

	var changes []coreworld.BlockChange
	for key := range destroyed {
		block := s.world.GetBlock(key.x, key.y, key.z)
		name := block.ResourceLocation()
		if name == "" || name == "minecraft:air" {
			continue
		}
		if name == "minecraft:tnt" {
			s.activateTNT(key.x, key.y, key.z, &changes)
			continue
		}
		s.world.SetBlock(key.x, key.y, key.z, coreworld.Air)
		changes = append(changes, coreworld.BlockChange{X: key.x, Y: key.y, Z: key.z, Block: coreworld.Air})
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
		if handler.DamagePlayerFromSource(sess, damage, playerDeathCause, s.sessions, cx, cz) {
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
			debuglog.Info(debuglog.MobSpawning, "baby villager grew up", "id", e.EntityID,
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
		baby.VillageWorkstation = spatial.BlockPos{}
		baby.HasVillageWorkstation = false
		baby.IsBaby = true
		baby.OnGround = true
		s.world.Entities.Add(baby)
		handler.BroadcastSpawnMob(baby, s.sessions)
		debuglog.Info(debuglog.MobSpawning, "villager baby born", "id", id,
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
