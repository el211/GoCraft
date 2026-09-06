package handler

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"GoCraft/core/intent"
	"GoCraft/core/player"
	coreplugin "GoCraft/core/plugin"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/registry"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

// ── Play state constants ──────────────────────────────────────────────────────

const (
	// Game Event reasons (not packet IDs — these are event-reason codes sent
	// inside the Game Event packet body and do not change between versions).
	gameEventStartWaitingForChunks = 13
)

const (
	overworldDimensionName = "minecraft:overworld"
	netherDimensionName    = "minecraft:the_nether"
	endDimensionName       = "minecraft:the_end"
)

func dimensionName(dimension int32) string {
	switch dimension {
	case 1:
		return netherDimensionName
	case 2:
		return endDimensionName
	default:
		return overworldDimensionName
	}
}

func dimensionCommandTarget(p *player.Player, w *coreworld.World, dimension int32) spatial.Vec3 {
	if dimension == 0 {
		return p.WorldSpawn
	}
	if dimension == 2 {
		x, z := 100, 0
		return spatial.Vec3{X: float64(x) + 0.5, Y: float64(w.SurfaceY(x, z) + 1), Z: float64(z) + 0.5}
	}
	x := int(math.Floor(p.Position.X / 8))
	z := int(math.Floor(p.Position.Z / 8))
	for y := 32; y <= 118; y++ {
		if safeRespawnSpace(w, x, y, z) {
			return spatial.Vec3{X: float64(x) + 0.5, Y: float64(y), Z: float64(z) + 0.5}
		}
	}
	return spatial.Vec3{X: float64(x) + 0.5, Y: float64(w.SurfaceY(x, z) + 1), Z: float64(z) + 0.5}
}

// keepAliveInterval is how often the server sends a Keep Alive to the client.
const keepAliveInterval = 10 * time.Second

// keepAliveTimeout is how long the server waits for a Keep Alive response.
const keepAliveTimeout = 30 * time.Second

// HandlePlay sends the initial Play-state packet burst, waits for the client
// to confirm the teleport, sends nearby chunks, then runs the keep-alive /
// packet loop until the client disconnects or the connection errors.
//
// Protocol flow (1.21.4):
//
//	S→C  Login (Play)              (0x2C) — assigns entity ID, dimension, etc.
//	S→C  Player Abilities          (0x3A) — flight / speed flags
//	S→C  Set Default Spawn Pos     (0x5B) — world spawn marker on map
//	S→C  Player Info Update        (0x40) — add self to tab list
//	S→C  Synchronize Player Pos    (0x42) — spawn co-ordinates + teleport ID 1
//	S→C  Set Center Chunk          (0x58) — chunk streaming anchor
//	S→C  Game Event reason=13      (0x23) — "start waiting for level chunks"
//	C→S  Confirm Teleport (ID 1)   (0x00)
//	S→C  Level Chunk With Light    (0x28) × (2·viewRadius+1)² — initial burst
//	     … keep-alive / movement / play loop …
func HandlePlay(conn *network.ClientConn, p *player.Player, w *coreworld.World, worldForDimension func(int32) *coreworld.World, sender *javaworld.Sender, mgr *session.Manager, cmds *Dispatcher, reg registry.Provider, worldSeed int64, worldAge func() int64, viewDistance, preGenerateRadius int32, nextEntityID func() int32, intentBus *intent.Bus, plugins *coreplugin.Bus) error {
	// ── Initial burst ────────────────────────────────────────────────────────
	if viewDistance < 2 {
		viewDistance = 2
	}
	if preGenerateRadius < viewDistance+2 {
		preGenerateRadius = viewDistance + 2
	}
	if worldForDimension != nil {
		if resolved := worldForDimension(p.Dimension); resolved != nil {
			w = resolved
		}
	}
	var dimensionTypeIDs [3]int32
	for dimension := int32(0); dimension < 3; dimension++ {
		typeID, err := reg.DimensionTypeID(dimensionName(dimension))
		if err != nil {
			return fmt.Errorf("play: resolving %s dimension type: %w", dimensionName(dimension), err)
		}
		dimensionTypeIDs[dimension] = typeID
	}
	hashedSeed := obfuscateSeed(worldSeed)
	if err := sendLoginPlay(conn, p, dimensionTypeIDs[p.Dimension], hashedSeed, viewDistance); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendPlayerAbilities(conn, p); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendCombatAttributes(conn, p); err != nil {
		return fmt.Errorf("play: combat attributes: %w", err)
	}
	if err := sendArmorAttributes(conn, p); err != nil {
		return fmt.Errorf("play: armour attributes: %w", err)
	}
	if err := sendUpdateHealth(conn, p); err != nil {
		return fmt.Errorf("play: health: %w", err)
	}
	if _, _, _, dead := p.HealthSnapshot(); dead {
		message := fmt.Sprintf("%s died", p.Username)
		if err := conn.WritePacket(buildDeathCombatEvent(p, message)); err != nil {
			return fmt.Errorf("play: restoring death screen: %w", err)
		}
	}
	if err := sendExperience(conn, p); err != nil {
		return fmt.Errorf("play: experience: %w", err)
	}
	if err := sendDefaultSpawnPosition(conn, p); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendPlayerInfoUpdate(conn, p); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	const teleportID = int32(1)
	if err := sendSyncPosition(conn, p, teleportID); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendSetCenterChunk(conn, p); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendViewDistance(conn, viewDistance); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendSimulationDistance(conn, viewDistance); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	// Tell the client to stop waiting for chunks and show the world.
	if err := sendGameEvent(conn, gameEventStartWaitingForChunks, 0); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	// Send initial inventory state and confirm the active hotbar slot.
	if err := sendSetContainerContent(conn, p, 1); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendSetHeldItem(conn, p.HeldSlot); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	// Synchronize the recipe displays for crafting, furnace, and blast furnace.
	if err := sendRecipeBook(conn); err != nil {
		return fmt.Errorf("play: recipes: %w", err)
	}
	// Send command graph for tab completion.
	if err := conn.WritePacket(buildCommandsPacket(cmds.CommandTree(p))); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	// Send the current time so the client shows the correct sky/lighting immediately.
	age := worldAge()
	if err := conn.WritePacket(buildSetTime(age, age%24000)); err != nil {
		return fmt.Errorf("play: set time: %w", err)
	}
	weatherEvent, rainLevel := byte(2), float32(0)
	if p.Raining {
		weatherEvent, rainLevel = 1, 1
	}
	if err := sendGameEvent(conn, weatherEvent, 0); err != nil {
		return fmt.Errorf("play: weather: %w", err)
	}
	if err := sendGameEvent(conn, 7, rainLevel); err != nil {
		return fmt.Errorf("play: rain level: %w", err)
	}
	thunderLevel := float32(0)
	if p.Thundering {
		thunderLevel = 1
	}
	if err := sendGameEvent(conn, 8, thunderLevel); err != nil {
		return fmt.Errorf("play: thunder level: %w", err)
	}

	slog.Info("player entered play state",
		"remote", conn.RemoteAddr(),
		"name", p.Username,
		"uuid", p.UUID,
	)

	return playLoop(conn, p, teleportID, w, worldForDimension, sender, mgr, cmds, dimensionTypeIDs, hashedSeed, viewDistance, preGenerateRadius, nextEntityID, intentBus, plugins)
}

// ── Clientbound packet helpers ────────────────────────────────────────────────

// sendLoginPlay sends the Login (Play) packet (S→C, minecraft:login).
//
// Field order for 1.21.4 / protocol 769 (Prismarine protocol.json and Mojang codec):
//
//	Int     entity_id
//	Bool    is_hardcore
//	VarInt  dimension_count           (1)
//	String  dimension_names[0]        "minecraft:overworld"
//	VarInt  max_players               (informational)
//	VarInt  view_distance
//	VarInt  simulation_distance
//	Bool    reduced_debug_info
//	Bool    enable_respawn_screen
//	Bool    do_limited_crafting
//	VarInt  dimension_type            registry index (0 = overworld); NOT a string
//	String  dimension_name            current world identifier
//	Long    hashed_seed
//	Byte    game_mode                 raw signed byte (0=survival 1=creative …)
//	Byte    prev_game_mode            raw byte; 0xFF = −1 = undefined
//	Bool    is_debug
//	Bool    is_flat
//	Bool    has_death_location        false → no following death position
//	VarInt  portal_cooldown
//	VarInt  sea_level                 63 for the overworld
//	Bool    enforce_secure_chat
func sendLoginPlay(conn *network.ClientConn, p *player.Player, dimensionTypeID int32, hashedSeed int64, viewDistances ...int32) error {
	if conn.State != network.StatePlay {
		return fmt.Errorf("refusing to send clientbound/minecraft:login in %s state", conn.State)
	}

	viewDistance := int32(10)
	if len(viewDistances) > 0 {
		viewDistance = viewDistances[0]
	}
	pkt := buildLoginPlayWithDistances(p, dimensionTypeID, hashedSeed, viewDistance, viewDistance)
	frame, err := protocol.MarshalPacket(pkt)
	if err != nil {
		return fmt.Errorf("framing clientbound/minecraft:login: %w", err)
	}

	slog.Info("java packet diagnostic",
		"semanticName", "clientbound/minecraft:login",
		"packetID", pkt.ID,
		"payloadLength", len(pkt.Data),
		"payloadHex", hex.EncodeToString(pkt.Data),
		"framedPacketHex", hex.EncodeToString(frame),
		"compressionEnabled", conn.CompressionEnabled(),
		"protocolState", conn.State.String(),
	)
	return conn.WritePacket(pkt)
}

func buildLoginPlay(p *player.Player, dimensionTypeID int32, hashedSeed int64) *protocol.Packet {
	return buildLoginPlayWithDistances(p, dimensionTypeID, hashedSeed, 10, 10)
}

func buildLoginPlayWithDistances(p *player.Player, dimensionTypeID int32, hashedSeed int64, viewDistance, simulationDistance int32) *protocol.Packet {
	return protocol.NewBuilder(packetIDPlayLogin).
		Int(p.EntityID).
		Bool(false).                        // is_hardcore
		VarInt(3).                          // dimension count
		String(overworldDimensionName).     // dimension names[0]
		String(netherDimensionName).        // dimension names[1]
		String(endDimensionName).           // dimension names[2]
		VarInt(20).                         // max_players (informational)
		VarInt(viewDistance).               // view_distance
		VarInt(simulationDistance).         // simulation_distance
		Bool(false).                        // reduced_debug_info
		Bool(true).                         // enable_respawn_screen
		Bool(false).                        // do_limited_crafting
		VarInt(dimensionTypeID).            // direct dimension_type registry ID
		String(dimensionName(p.Dimension)). // dimension_name
		Long(hashedSeed).                   // SHA-256-obfuscated world seed
		Byte(byte(p.GameMode)).             // game_mode (raw signed byte on wire)
		Byte(0xFF).                         // previous_game_mode: raw 0xFF = -1
		Bool(false).                        // is_debug
		Bool(false).                        // is_flat
		Bool(false).                        // no last_death_location follows
		VarInt(0).                          // portal_cooldown
		VarInt(63).                         // sea_level (present in protocol 769)
		Bool(false).                        // enforces_secure_chat
		Build()
}

// obfuscateSeed matches BiomeManager.obfuscateSeed in Minecraft 1.21.4:
// SHA-256 over the seed's little-endian bytes, interpreted from the digest's
// first eight bytes as a big-endian signed long.
func obfuscateSeed(seed int64) int64 {
	var input [8]byte
	binary.LittleEndian.PutUint64(input[:], uint64(seed))
	digest := sha256.Sum256(input[:])
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

// sendPlayerAbilities sends the Player Abilities packet (0x3A S→C).
//
// Flags bitmask:
//
//	0x01 invulnerable
//	0x02 flying
//	0x04 allow_flying
//	0x08 instant_build (creative)
func sendPlayerAbilities(conn *network.ClientConn, p *player.Player) error {
	if conn == nil {
		return nil
	}
	return conn.WritePacket(buildPlayerAbilities(p))
}

// SyncPlayerState republishes a Java player's game mode and the flight, speed
// and instant-build flags that depend on it.
//
// It is the Java half of the ability-sync bridge a command context carries, and
// the mirror of what the Bedrock adapter sends in one go: game mode first,
// because the abilities that follow are read against it.
//
// Game Event reason 3 is change_game_mode, with the mode as a float32.
func SyncPlayerState(conn *network.ClientConn, p *player.Player) error {
	if conn == nil || p == nil {
		return nil
	}
	if err := sendGameEvent(conn, 3, float32(p.GameMode)); err != nil {
		return fmt.Errorf("sending game mode: %w", err)
	}
	if err := sendPlayerAbilities(conn, p); err != nil {
		return fmt.Errorf("sending abilities: %w", err)
	}
	return nil
}

func buildPlayerAbilities(p *player.Player) *protocol.Packet {
	var flags byte
	allowFlying := p.AllowFlying ||
		p.GameMode == player.GameModeCreative ||
		p.GameMode == player.GameModeSpectator
	if p.GameMode == player.GameModeCreative {
		flags |= 0x01 | 0x08 // invulnerable + instant build
	}
	if p.GameMode == player.GameModeSpectator {
		flags |= 0x01 // invulnerable
	}
	if allowFlying {
		flags |= 0x04
	} else {
		p.Flying = false
	}
	if allowFlying && (p.Flying || p.GameMode == player.GameModeSpectator) {
		flags |= 0x02
	}
	flySpeed := p.FlySpeed
	if flySpeed <= 0 {
		flySpeed = 0.05
	}
	walkSpeed := p.WalkSpeed
	if walkSpeed <= 0 {
		walkSpeed = 0.1
	}
	return protocol.NewBuilder(packetIDPlayerAbilities).
		Byte(flags).
		Float(flySpeed).
		Float(walkSpeed).
		Build()
}

// sendCombatAttributes removes the modern client attack meter when legacy
// spam combat is selected. Attribute registry ID 4 is generic.attack_speed in
// Java 1.21.4; a very high base value keeps item modifiers effectively instant.
func sendCombatAttributes(conn *network.ClientConn, p *player.Player) error {
	packet := buildCombatAttributes(p)
	if packet == nil {
		return nil
	}
	return conn.WritePacket(packet)
}

func buildCombatAttributes(p *player.Player) *protocol.Packet {
	if p.AttackCooldown {
		return nil
	}
	return protocol.NewBuilder(packetIDUpdateAttributes).
		VarInt(p.EntityID).
		VarInt(1).
		VarInt(4).
		Double(1024).
		VarInt(0).
		Build()
}

// sendArmorAttributes updates the generic.armor attribute which controls the
// armour HUD above the player's health bar. Registry ID 0 is armor in 1.21.4.
func sendArmorAttributes(conn *network.ClientConn, p *player.Player) error {
	return conn.WritePacket(buildArmorAttributes(p))
}

func buildArmorAttributes(p *player.Player) *protocol.Packet {
	return protocol.NewBuilder(packetIDUpdateAttributes).
		VarInt(p.EntityID).
		VarInt(1).
		VarInt(0).
		Double(float64(p.ArmorPoints())).
		VarInt(0).
		Build()
}

// sendDefaultSpawnPosition sends the Set Default Spawn Position packet (0x5B S→C).
//
// Fields:
//
//	Long   position (64-bit packed block position)
//	Float  angle    (spawn compass bearing)
func sendDefaultSpawnPosition(conn *network.ClientConn, p *player.Player) error {
	x := int64(math.Floor(p.WorldSpawn.X))
	y := int64(math.Floor(p.WorldSpawn.Y))
	z := int64(math.Floor(p.WorldSpawn.Z))
	// Minecraft 64-bit packed position: X(26 bits) | Z(26 bits) | Y(12 bits)
	packed := ((x & 0x3FFFFFF) << 38) | ((z & 0x3FFFFFF) << 12) | (y & 0xFFF)
	pkt := protocol.NewBuilder(packetIDSpawnPosition).
		Long(packed).
		Float(0).
		Build()
	return conn.WritePacket(pkt)
}

// SyncDefaultSpawnPosition updates a Java client's compass/world-spawn target.
func SyncDefaultSpawnPosition(conn *network.ClientConn, p *player.Player) error {
	return sendDefaultSpawnPosition(conn, p)
}

// sendPlayerInfoUpdate sends Player Info Update (0x40 S→C) to add the player
// to the tab list.
//
// Action bitmask (1.21.4):
//
//	0x01  ADD_PLAYER        name + properties
//	0x02  INITIALIZE_CHAT   (omitted — no chat session)
//	0x04  UPDATE_GAME_MODE  game mode
//	0x08  UPDATE_LISTED     show in tab list
//	0x10  UPDATE_LATENCY    ping value
//	0x20  UPDATE_DISPLAY_NAME
func sendPlayerInfoUpdate(conn *network.ClientConn, p *player.Player) error {
	// Actions: ADD_PLAYER (0x01) + UPDATE_LISTED (0x08)
	const actions byte = 0x01 | 0x08

	b := protocol.NewBuilder(packetIDPlayerInfoUpdate).
		Byte(actions).
		VarInt(1).                  // 1 player entry
		UUID(protocol.UUID(p.UUID)) // player UUID

	// ADD_PLAYER (0x01) data: name + 0 properties
	b.String(p.Username).VarInt(0)

	// UPDATE_LISTED (0x08) data: listed = true
	b.Bool(true)

	return conn.WritePacket(b.Build())
}

// sendSyncPosition sends Synchronize Player Position (0x42 S→C).
//
// Fields (1.21.4 / protocol 769) — Teleport ID moved to FIRST since 1.21.2:
//
//	VarInt  teleport_id
//	Double  x, y, z
//	Double  velocity_x, velocity_y, velocity_z
//	Float   yaw, pitch
//	Int     flags (bitmask; 0 = all absolute)
func sendSyncPosition(conn *network.ClientConn, p *player.Player, teleportID int32) error {
	pkt := protocol.NewBuilder(packetIDSyncPosition).
		VarInt(teleportID).
		Double(p.Position.X).
		Double(p.Position.Y).
		Double(p.Position.Z).
		Double(0). // velocity x
		Double(0). // velocity y
		Double(0). // velocity z
		Float(p.Rotation.Yaw).
		Float(p.Rotation.Pitch).
		Int(0). // flags: 0 = absolute position
		Build()
	return conn.WritePacket(pkt)
}

// sendSetCenterChunk sends Set Center Chunk (0x58 S→C).
// This tells the client which chunk the player is in, controlling which
// chunks are loaded / unloaded around them.
func sendSetCenterChunk(conn *network.ClientConn, p *player.Player) error {
	chunkX := posToChunk(p.Position.X)
	chunkZ := posToChunk(p.Position.Z)
	pkt := protocol.NewBuilder(packetIDSetCenterChunk).
		VarInt(chunkX).
		VarInt(chunkZ).
		Build()
	return conn.WritePacket(pkt)
}

// sendViewDistance advertises the actual server chunk radius to the client.
func sendViewDistance(conn *network.ClientConn, radius int32) error {
	return conn.WritePacket(protocol.NewBuilder(packetIDSetViewDistance).VarInt(radius).Build())
}

func sendSimulationDistance(conn *network.ClientConn, radius int32) error {
	return conn.WritePacket(protocol.NewBuilder(packetIDSimulationDistance).VarInt(radius).Build())
}

func sendChunkBatchStart(conn *network.ClientConn) error {
	return conn.WritePacket(protocol.NewBuilder(packetIDChunkBatchStart).Build())
}

func sendChunkBatchFinished(conn *network.ClientConn, count int32) error {
	return conn.WritePacket(protocol.NewBuilder(packetIDChunkBatchFinished).VarInt(count).Build())
}

// sendGameEvent sends a Game Event packet (0x23 S→C).
//
// Fields:
//
//	Unsigned Byte  reason
//	Float          value
func sendGameEvent(conn *network.ClientConn, reason byte, value float32) error {
	if conn == nil {
		return nil
	}
	pkt := protocol.NewBuilder(packetIDGameEvent).
		Byte(reason).
		Float(value).
		Build()
	return conn.WritePacket(pkt)
}

// BroadcastGameEvent sends a world-state event to all Java sessions.
func BroadcastGameEvent(manager *session.Manager, reason byte, value float32) {
	if manager == nil {
		return
	}
	for _, current := range manager.SnapshotAll() {
		_ = sendGameEvent(current.Conn, reason, value)
	}
}

// sendForgetChunk sends Forget Level Chunk (0x22 S→C), instructing the client
// to unload the given chunk column.
// Wire order for this packet is Z then X.
func sendForgetChunk(conn *network.ClientConn, cx, cz int32) error {
	return conn.WritePacket(protocol.NewBuilder(packetIDForgetLevelChunk).
		Int(cz).Int(cx).Build())
}

// ── Play loop ─────────────────────────────────────────────────────────────────

// playLoop is the main body for an in-game player session.
//
// After the teleport is confirmed it registers the session with mgr, announces
// the join to other players, sends the initial chunk burst, then enters a tight
// loop that:
//   - Sends periodic Keep Alive packets and validates the client's response.
//   - Reads and dispatches incoming packets (movement, keep-alive, etc.).
//   - Broadcasts position updates to all other sessions on every movement packet.
//   - Streams new chunks and unloads old chunks whenever the player crosses a
//     chunk boundary.
//
// On exit the session is removed from mgr and all other players are notified.
func playLoop(conn *network.ClientConn, p *player.Player, spawnTeleportID int32, w *coreworld.World, worldForDimension func(int32) *coreworld.World, sender *javaworld.Sender, mgr *session.Manager, cmds *Dispatcher, dimensionTypeIDs [3]int32, hashedSeed int64, viewRadius, preGenerateRadius int32, nextEntityID func() int32, intentBus *intent.Bus, plugins *coreplugin.Bus) error {
	// Must receive Confirm Teleport for the spawn position before anything else.
	if err := readConfirmTeleport(conn, spawnTeleportID); err != nil {
		return fmt.Errorf("play loop: %w", err)
	}

	// ── Multiplayer registration ─────────────────────────────────────────────
	// Register before announcing so ForEachExcept in onPlayerJoin can see us,
	// and the joiner can see existing players.
	// Defers run LIFO: onPlayerLeave fires first (broadcasts while still in map),
	// then Remove cleans up the map entry.
	// Foreign command goroutines enqueue teleports here. The target's own play
	// loop applies them so chunk maps and teleport-confirmation state remain
	// single-threaded.
	teleportRequests := make(chan spatial.Vec3, 8)
	type dimensionRequest struct {
		dimension int32
		position  spatial.Vec3
	}
	dimensionRequests := make(chan dimensionRequest, 4)
	sess := &session.Session{
		Player: p,
		Conn:   conn,
		TeleportTo: func(x, y, z float64) error {
			select {
			case teleportRequests <- spatial.Vec3{X: x, Y: y, Z: z}:
				return nil
			default:
				return fmt.Errorf("player teleport queue is full")
			}
		},
		ChangeDimension: func(dimension int32, position spatial.Vec3) error {
			if dimension < 0 || dimension > 2 {
				return fmt.Errorf("invalid dimension %d", dimension)
			}
			select {
			case dimensionRequests <- dimensionRequest{dimension: dimension, position: position}:
				return nil
			default:
				return fmt.Errorf("player dimension queue is full")
			}
		},
	}
	mgr.Add(sess)
	SyncPlayerStatusEffects(conn, p)
	defer mgr.Remove(p.UUID)
	defer onPlayerLeave(mgr, sess)
	// Entities generated before this player joined are included in the full
	// snapshot below, so they no longer need a separate spawn announcement.
	_ = w.DrainSpawnedEntities()
	onPlayerJoin(mgr, sess, w.Entities.Snapshot())

	// ── Initial chunk burst ──────────────────────────────────────────────────
	chunkX := posToChunk(p.Position.X)
	chunkZ := posToChunk(p.Position.Z)

	// Warm a larger area before the foreground streamer asks for it.
	w.QueuePregeneration(chunkX, chunkZ, preGenerateRadius)

	// sentChunks tracks which chunk columns the client currently has loaded.
	sentChunks := make(map[[2]int32]struct{})
	initialKeys := chunkKeysAround(chunkX, chunkZ, viewRadius)
	if err := sendChunkBatchStart(conn); err != nil {
		return fmt.Errorf("play loop: starting initial chunk batch: %w", err)
	}
	for _, key := range initialKeys {
		c := w.Chunk(key[0], key[1])
		if err := sender.SendChunkFromWorld(conn, w, c); err != nil {
			return fmt.Errorf("play loop: initial chunk (%d,%d): %w", key[0], key[1], err)
		}
		sentChunks[key] = struct{}{}
	}
	if err := sendChunkBatchFinished(conn, int32(len(initialKeys))); err != nil {
		return fmt.Errorf("play loop: finishing initial chunk batch: %w", err)
	}
	broadcastGeneratedEntities(w, mgr)
	sendExistingMobsInViewTo(conn, w.Entities.Snapshot(), chunkX, chunkZ, viewRadius)
	lastChunkX, lastChunkZ := chunkX, chunkZ
	nextTeleportID := spawnTeleportID
	pendingTeleportID := int32(-1)
	var pendingRespawnChunks [][2]int32
	streamRespawn := false

	// teleportTo is given to CommandContext so /tp (and any future teleport
	// command) can reposition the player and immediately stream destination
	// chunks without waiting for the next movement packet.
	//
	// The closure captures sentChunks, lastChunkX, and lastChunkZ by reference
	// so it keeps the play loop's chunk-tracking state consistent.  After it
	// returns, the bottom-of-loop chunk check sees newChunkX == lastChunkX and
	// skips a redundant re-send.
	teleportTo := func(x, y, z float64) error {
		pendingRespawnChunks = nil
		p.Position.X, p.Position.Y, p.Position.Z = x, y, z
		p.FallDistance = 0
		nextTeleportID++
		if err := sendSyncPosition(conn, p, nextTeleportID); err != nil {
			return fmt.Errorf("sync position: %w", err)
		}
		pendingTeleportID = nextTeleportID
		if err := sendSetCenterChunk(conn, p); err != nil {
			return fmt.Errorf("set center chunk: %w", err)
		}
		newCX := posToChunk(x)
		newCZ := posToChunk(z)
		if streamRespawn {
			streamRespawn = false
			keys := chunkKeysAround(newCX, newCZ, viewRadius)
			// Finish a 3x3 batch promptly so the client can leave Loading terrain.
			// Encoding a 5x5 batch here used to block packet reads during respawn.
			nearCount := respawnBootstrapCount(viewRadius, len(keys))
			if err := sendChunkKeys(conn, w, sender, sentChunks, keys[:nearCount]); err != nil {
				return fmt.Errorf(`send nearby respawn chunks: %w`, err)
			}
			// Warm the same bootstrap radius. The rest remains background work so
			// large configured view distances do not stall the respawn handshake.
			if preGenerateRadius > 0 {
				w.QueuePregeneration(newCX, newCZ, 1)
			}
			pendingRespawnChunks = append(pendingRespawnChunks, keys[nearCount:]...)
			broadcastGeneratedEntities(w, mgr)
			sendExistingMobsInViewTo(conn, w.Entities.Snapshot(), newCX, newCZ, viewRadius)
			lastChunkX, lastChunkZ = newCX, newCZ
			return nil
		}
		if err := updateChunkView(conn, w, sender, sentChunks, newCX, newCZ, viewRadius, preGenerateRadius); err != nil {
			return fmt.Errorf("update chunk view: %w", err)
		}
		broadcastGeneratedEntities(w, mgr)
		sendExistingMobsInViewTo(conn, w.Entities.Snapshot(), newCX, newCZ, viewRadius)
		lastChunkX, lastChunkZ = newCX, newCZ
		return nil
	}
	changeDimension := func(dimension int32, target spatial.Vec3) error {
		if dimension < 0 || dimension > 2 || worldForDimension == nil {
			return fmt.Errorf("dimension %d is unavailable", dimension)
		}
		destinationWorld := worldForDimension(dimension)
		if destinationWorld == nil {
			return fmt.Errorf("dimension %d is unavailable", dimension)
		}
		target = destinationWorld.EnsureSafeArrival(target, dimension)
		p.InvulnerableUntil = time.Now().Add(10 * time.Second)
		p.Dimension = dimension
		p.Position = target
		p.FallDistance = 0
		p.OnGround = false
		w = destinationWorld
		if err := conn.WritePacket(buildRespawn(p, dimensionTypeIDs[dimension], hashedSeed)); err != nil {
			return fmt.Errorf("respawn packet: %w", err)
		}
		_ = sendUpdateHealth(conn, p)
		SyncPlayerStatusEffects(conn, p)
		_ = sendPlayerAbilities(conn, p)
		_ = sendCombatAttributes(conn, p)
		_ = sendArmorAttributes(conn, p)
		_ = sendSetContainerContent(conn, p, p.ContainerStateID)
		sentChunks = make(map[[2]int32]struct{})
		pendingRespawnChunks = nil
		streamRespawn = true
		if err := teleportTo(target.X, target.Y, target.Z); err != nil {
			return fmt.Errorf("destination position: %w", err)
		}
		broadcastPosition(mgr, p)
		return nil
	}
	changeWorldCommand := func(dimension int32) error {
		if dimension == p.Dimension {
			return nil
		}
		destinationWorld := worldForDimension(dimension)
		if destinationWorld == nil {
			return fmt.Errorf("destination world is unavailable")
		}
		return changeDimension(dimension, dimensionCommandTarget(p, destinationWorld, dimension))
	}
	// ── Keep-alive state ─────────────────────────────────────────────────────
	var (
		keepAliveSeq   atomic.Int64
		lastKASent     time.Time
		pendingAliveID int64 = -1 // -1 = no outstanding keep-alive
	)
	lastKASent = time.Now()

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	// ── Main loop ────────────────────────────────────────────────────────────
	for {
		select {
		case request := <-dimensionRequests:
			if err := changeDimension(request.dimension, request.position); err != nil {
				return fmt.Errorf("play loop: applying queued dimension change: %w", err)
			}
		case destination := <-teleportRequests:
			if err := teleportTo(destination.X, destination.Y, destination.Z); err != nil {
				return fmt.Errorf("play loop: applying queued teleport: %w", err)
			}
		default:
		}
		broadcastGeneratedEntities(w, mgr)
		if len(pendingRespawnChunks) > 0 {
			batchSize := 8
			if len(pendingRespawnChunks) < batchSize {
				batchSize = len(pendingRespawnChunks)
			}
			if err := sendChunkKeys(conn, w, sender, sentChunks, pendingRespawnChunks[:batchSize]); err != nil {
				return fmt.Errorf(`play loop: streaming respawn chunks: %w`, err)
			}
			pendingRespawnChunks = pendingRespawnChunks[batchSize:]
		}

		// Check keep-alive timeout.
		if pendingAliveID >= 0 && time.Since(lastKASent) > keepAliveTimeout {
			return fmt.Errorf("play loop: keep-alive timeout for player %s", p.Username)
		}

		// Send keep-alive if the interval has elapsed.
		select {
		case <-ticker.C:
			id := keepAliveSeq.Add(1)
			if err := sendKeepAlive(conn, id); err != nil {
				return fmt.Errorf("play loop: sending keep-alive: %w", err)
			}
			pendingAliveID = id
			lastKASent = time.Now()
		default:
		}

		// Read the next packet (ReadPacket sets its own deadline).
		pkt, err := conn.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				slog.Info("player disconnected", "name", p.Username, "remote", conn.RemoteAddr())
				return nil
			}
			return fmt.Errorf("play loop: reading packet: %w", err)
		}
		p.TouchActivity()

		if pkt.ID == packetIDConfirmTeleport {
			teleportID, readErr := protocol.ReadVarInt(pkt.Reader())
			if readErr != nil {
				return fmt.Errorf("play loop: confirm teleport: %w", readErr)
			}
			if teleportID == pendingTeleportID {
				pendingTeleportID = -1
			}
			continue
		}
		// The client may send one or more movement packets from the old death
		// location before acknowledging a respawn teleport. Accepting them
		// snaps the canonical player back into the original danger/void.
		if pendingTeleportID >= 0 && isPlayerMovementPacket(pkt.ID) {
			continue
		}

		// Save position before the packet mutates it so we can revert if needed.
		prevX, prevY, prevZ := p.Position.X, p.Position.Y, p.Position.Z
		prevOnGround := p.OnGround

		posChanged, err := handlePlayPacket(pkt, p, &pendingAliveID)
		if err != nil {
			return fmt.Errorf("play loop: packet 0x%02X: %w", pkt.ID, err)
		}

		// If the player moved into a chunk the client hasn't received yet,
		// bounce them back to their previous position.  This matches vanilla's
		// "Loading terrain…" behaviour: movement is gated on chunk delivery.
		if posChanged {
			destCX := int32(posToChunk(p.Position.X))
			destCZ := int32(posToChunk(p.Position.Z))
			if _, alreadySent := sentChunks[[2]int32{destCX, destCZ}]; !alreadySent {
				p.Position.X, p.Position.Y, p.Position.Z = prevX, prevY, prevZ
				p.OnGround = prevOnGround
				nextTeleportID++
				_ = sendSyncPosition(conn, p, nextTeleportID)
				pendingTeleportID = nextTeleportID
				// Queue the ahead chunks so they arrive as fast as possible.
				w.QueuePregeneration(destCX, destCZ, 2)
				posChanged = false
			}
		}

		// Broadcast position/rotation to all other sessions on every movement.
		if posChanged {
			if p.RecordMovementVibration() {
				w.EmitVibration(int(math.Floor(p.Position.X)), int(math.Floor(p.Position.Y)), int(math.Floor(p.Position.Z)))
			}
			applyJavaMovementExhaustion(p, prevX, prevZ, conn)
			applyPlayerFallDamage(sess, prevY, prevOnGround, w, mgr)
			applyPlayerEnvironmentalDamage(sess, w, mgr, prevX, prevZ)
			broadcastPosition(mgr, p)
		}

		// The death screen sends Client Command action 0 when the player clicks
		// Respawn. Recreate the player in the same dimension and resend its view.
		if pkt.ID == packetIDClientCommand {
			action, readErr := protocol.ReadVarInt(pkt.Reader())
			if readErr != nil {
				return fmt.Errorf("play loop: client command: %w", readErr)
			}
			_, _, _, dead := p.HealthSnapshot()
			if action == 0 && dead {
				// Death can occur while mounted. Clear both sides of the link before
				// resetting the player so the client cannot immediately snap back to
				// the death location after it accepts the respawn teleport.
				if vehicleID := p.VehicleEntityID; vehicleID != 0 {
					if vehicle, ok := w.Entities.Get(vehicleID); ok && vehicle.HasPassenger(p.EntityID) {
						vehicle.RemovePassenger(p.EntityID)
						BroadcastSetPassengers(vehicleID, vehicle.PassengerIDs(), mgr)
					}
					p.VehicleEntityID = 0
				}
				destinationWorld := w
				if worldForDimension != nil {
					if overworld := worldForDimension(0); overworld != nil {
						destinationWorld = overworld
					}
				}
				respawnPlayerInOverworld(p, destinationWorld)
				w = destinationWorld
				if err := conn.WritePacket(buildRespawn(p, dimensionTypeIDs[p.Dimension], hashedSeed)); err != nil {
					return fmt.Errorf("play loop: respawn packet: %w", err)
				}
				if err := sendUpdateHealth(conn, p); err != nil {
					return fmt.Errorf("play loop: respawn health: %w", err)
				}
				_ = sendPlayerAbilities(conn, p)
				_ = sendCombatAttributes(conn, p)
				_ = sendArmorAttributes(conn, p)
				_ = sendSetContainerContent(conn, p, p.ContainerStateID)
				_ = sendDefaultSpawnPosition(conn, p)
				sentChunks = make(map[[2]int32]struct{})
				streamRespawn = true
				if err := teleportTo(p.Position.X, p.Position.Y, p.Position.Z); err != nil {
					return fmt.Errorf("play loop: respawn position: %w", err)
				}
				broadcastPosition(mgr, p)
			}
		}

		// Chat and commands need the session manager and dispatcher.
		if pkt.ID == packetIDChatMessage || pkt.ID == packetIDChatCommand {
			if err := handleChatPacket(pkt, p, mgr, cmds, w, conn, teleportTo, changeWorldCommand); err != nil {
				slog.Warn("chat error", "player", p.Username, "err", err)
			}
		}

		// Block interaction needs both the world and the session manager.
		if pkt.ID == packetIDPlayerAction || pkt.ID == packetIDUseItemOn {
			if err := handleBlockPacket(pkt, p, w, mgr, conn, nextEntityID, plugins, intentBus); err != nil {
				slog.Warn("block interaction error", "player", p.Username, "err", err)
			}
		}

		// Entity interaction (right-click mob) — villager trading + boat mount.
		if pkt.ID == packetIDInteract {
			if err := handleInteractPacket(pkt, p, w, conn, mgr, intentBus); err != nil {
				slog.Warn("interact error", "player", p.Username, "err", err)
			}
		}
		if pkt.ID == packetIDSwingArm {
			if err := handleSwingArmPacket(pkt, p, intentBus); err != nil {
				slog.Warn("swing arm error", "player", p.Username, "err", err)
			}
		}

		// Boat / vehicle packets.
		if pkt.ID == packetIDMoveVehicle {
			if err := HandleMoveVehiclePacket(pkt, p, w, conn, mgr, intentBus); err != nil {
				slog.Warn("move_vehicle error", "player", p.Username, "err", err)
			}
		}
		if pkt.ID == packetIDPlayerInput {
			if err := HandlePlayerInputPacket(pkt, p, w, conn, mgr, intentBus); err != nil {
				slog.Warn("player_input error", "player", p.Username, "err", err)
			}
		}
		if pkt.ID == packetIDPlayerCommand {
			if err := HandlePlayerCommandPacket(pkt, p, w, conn, mgr, intentBus); err != nil {
				slog.Warn("player_command error", "player", p.Username, "err", err)
			}
		}

		// Inventory management (held item, creative set item, and open containers).
		if pkt.ID == packetIDSetHeldItemCS || pkt.ID == packetIDCreativeModeSetItem {
			if err := handleInventoryPacket(pkt, p, conn); err != nil {
				slog.Warn("inventory error", "player", p.Username, "err", err)
			}
		}
		if pkt.ID == packetIDUseItem {
			if err := handleUseItem(pkt, p, conn, w, mgr, nextEntityID); err != nil {
				slog.Warn("use item error", "player", p.Username, "err", err)
			}
		}
		if pkt.ID == packetIDContainerClick || pkt.ID == packetIDContainerClose || pkt.ID == packetIDContainerButtonClick {
			if err := handleContainerPacket(pkt, p, conn, w, intentBus); err != nil {
				slog.Warn("container error", "player", p.Username, "err", err)
			}
		}

		if pkt.ID == packetIDPlaceRecipe {
			if err := handlePlaceRecipeRequest(pkt, p, conn); err != nil {
				slog.Warn("recipe request error", "player", p.Username, "err", err)
			}
		}

		if pkt.ID == packetIDSignUpdate {
			if err := handleSignUpdate(pkt, p, w, mgr); err != nil {
				slog.Warn("sign update error", "player", p.Username, "err", err)
			}
		}

		if pkt.ID == packetIDRenameItem {
			if err := handleAnvilRename(pkt, p, conn); err != nil {
				slog.Warn("rename item error", "player", p.Username, "err", err)
			}
		}

		// ── Chunk streaming on boundary crossing ─────────────────────────────
		newChunkX := posToChunk(p.Position.X)
		newChunkZ := posToChunk(p.Position.Z)
		if newChunkX != lastChunkX || newChunkZ != lastChunkZ {
			if err := sendSetCenterChunk(conn, p); err != nil {
				return fmt.Errorf("play loop: center chunk: %w", err)
			}
			if err := updateChunkView(conn, w, sender, sentChunks, newChunkX, newChunkZ, viewRadius, preGenerateRadius); err != nil {
				return fmt.Errorf("play loop: streaming chunks: %w", err)
			}
			lastChunkX, lastChunkZ = newChunkX, newChunkZ
		}
	}
}

func handleSwingArmPacket(pkt *protocol.Packet, p *player.Player, bus *intent.Bus) error {
	hand, err := protocol.ReadVarInt(pkt.Reader())
	if err != nil {
		return fmt.Errorf("reading swing hand: %w", err)
	}
	if hand < 0 || hand > 1 {
		return fmt.Errorf("invalid swing hand %d", hand)
	}
	if bus != nil {
		bus.PostArmSwing(intent.ArmSwingIntent{PlayerUUID: p.UUID, Hand: hand})
	}
	return nil
}

func applyJavaMovementExhaustion(p *player.Player, previousX, previousZ float64, conn *network.ClientConn) {
	if p == nil || p.GameMode != player.GameModeSurvival || p.Dead || p.Flying || !p.Sprinting {
		return
	}
	distance := math.Hypot(p.Position.X-previousX, p.Position.Z-previousZ)
	if distance <= 0 || distance > 10 {
		return
	}
	foodBefore, saturationBefore, _ := p.HungerSnapshot()
	p.AddExhaustion(float32(distance * 0.1))
	foodAfter, saturationAfter, _ := p.HungerSnapshot()
	if foodAfter != foodBefore || saturationAfter != saturationBefore {
		_ = sendUpdateHealth(conn, p)
	}
}

// resolveBedRespawn validates the saved bed and finds a two-block-high space
// beside either half. If every side is obstructed, the top of the bed is used
// when clear; a missing/fully blocked bed falls back to world spawn.
func resolveBedRespawn(p *player.Player, w *coreworld.World) (spatial.Vec3, bool) {
	if p == nil || w == nil || !p.HasSpawnPoint {
		return spatial.Vec3{}, false
	}
	x, y, z := int(p.SpawnPoint.X), int(p.SpawnPoint.Y), int(p.SpawnPoint.Z)
	bed := w.GetBlock(x, y, z)
	if !isBedBlock(bed.ResourceLocation()) {
		p.HasSpawnPoint = false
		return spatial.Vec3{}, false
	}
	dx, dz := bedHeadOffset(bed.Properties["facing"])
	if bed.Properties["part"] == "head" {
		x, z = x-dx, z-dz
	}
	hx, hz := x+dx, z+dz
	candidates := [][3]int{
		{x - dz, y, z + dx}, {x + dz, y, z - dx},
		{hx - dz, y, hz + dx}, {hx + dz, y, hz - dx},
		{x - dx, y, z - dz}, {hx + dx, y, hz + dz},
	}
	for _, candidate := range candidates {
		if safeRespawnSpace(w, candidate[0], candidate[1], candidate[2]) {
			return spatial.Vec3{X: float64(candidate[0]) + 0.5, Y: float64(candidate[1]), Z: float64(candidate[2]) + 0.5}, true
		}
	}
	if respawnPassable(w.GetBlock(x, y+1, z)) && respawnPassable(w.GetBlock(x, y+2, z)) {
		return spatial.Vec3{X: float64(x) + 0.5, Y: float64(y + 1), Z: float64(z) + 0.5}, true
	}
	return spatial.Vec3{}, false
}

// ResolveBedRespawn exposes the edition-neutral bed validation to the server's
// Bedrock respawn intent without duplicating safety rules in another adapter.
func ResolveBedRespawn(p *player.Player, w *coreworld.World) (spatial.Vec3, bool) {
	return resolveBedRespawn(p, w)
}

func respawnPlayerInOverworld(p *player.Player, w *coreworld.World) {
	p.Dimension = 0
	p.Revive()
	if bedSpawn, ok := resolveBedRespawn(p, w); ok {
		p.Position = bedSpawn
	} else {
		p.Position = p.WorldSpawn
	}
}

func safeRespawnSpace(w *coreworld.World, x, y, z int) bool {
	feet := w.GetBlock(x, y, z)
	head := w.GetBlock(x, y+1, z)
	below := w.GetBlock(x, y-1, z)
	return respawnPassable(feet) && respawnPassable(head) &&
		!below.IsAir() && !coreworld.IsFluidBlock(below.ResourceLocation())
}

func respawnPassable(block coreworld.Block) bool {
	return block.IsAir() || placementReplaceable(block.ResourceLocation())
}

func broadcastGeneratedEntities(w *coreworld.World, mgr *session.Manager) {
	for _, entity := range w.DrainSpawnedEntities() {
		BroadcastSpawnMob(entity, mgr)
	}
}

func sendChunkKeys(
	conn *network.ClientConn,
	w *coreworld.World,
	sender *javaworld.Sender,
	sent map[[2]int32]struct{},
	keys [][2]int32,
) error {
	if len(keys) == 0 {
		return nil
	}
	if err := sendChunkBatchStart(conn); err != nil {
		return fmt.Errorf(`starting chunk batch: %w`, err)
	}
	for _, key := range keys {
		if err := sender.SendChunkFromWorld(conn, w, w.Chunk(key[0], key[1])); err != nil {
			return fmt.Errorf(`chunk (%d,%d): %w`, key[0], key[1], err)
		}
		sent[key] = struct{}{}
	}
	if err := sendChunkBatchFinished(conn, int32(len(keys))); err != nil {
		return fmt.Errorf(`finishing chunk batch: %w`, err)
	}
	return nil
}

// updateChunkView sends chunks newly entering the view square and unloads
// chunks leaving it when the player moves from (oldCX,oldCZ) to (newCX,newCZ).
//
// Only chunks that are in the new square but not in the sent set are sent.
// Only chunks that are in the old square but no longer in the new square are
// forgotten. This keeps network traffic proportional to movement speed rather
// than reloading the entire view on every step.
func updateChunkView(
	conn *network.ClientConn,
	w *coreworld.World,
	sender *javaworld.Sender,
	sent map[[2]int32]struct{},
	newCX, newCZ, viewRadius, preGenerateRadius int32,
) error {
	w.QueuePregeneration(newCX, newCZ, preGenerateRadius)

	newKeys := make([][2]int32, 0)
	for _, key := range chunkKeysAround(newCX, newCZ, viewRadius) {
		if _, ok := sent[key]; !ok {
			newKeys = append(newKeys, key)
		}
	}
	if len(newKeys) > 0 {
		if err := sendChunkBatchStart(conn); err != nil {
			return fmt.Errorf("starting chunk batch: %w", err)
		}
		for _, key := range newKeys {
			c := w.Chunk(key[0], key[1])
			if err := sender.SendChunkFromWorld(conn, w, c); err != nil {
				return fmt.Errorf("chunk (%d,%d): %w", key[0], key[1], err)
			}
			sent[key] = struct{}{}
		}
		if err := sendChunkBatchFinished(conn, int32(len(newKeys))); err != nil {
			return fmt.Errorf("finishing chunk batch: %w", err)
		}
	}

	// Keep two extra rings behind the player. This hysteresis prevents chunks
	// disappearing the instant they cross the advertised view edge.
	retainRadius := viewRadius + 2
	for key := range sent {
		if abs32(key[0]-newCX) <= retainRadius && abs32(key[1]-newCZ) <= retainRadius {
			continue
		}
		if err := sendForgetChunk(conn, key[0], key[1]); err != nil {
			return fmt.Errorf("forgetting chunk (%d,%d): %w", key[0], key[1], err)
		}
		delete(sent, key)
	}
	return nil
}

// chunkKeysAround returns centre-first Chebyshev rings. Nearby terrain reaches
// the client before distant terrain in each chunk batch.
func chunkKeysAround(cx, cz, radius int32) [][2]int32 {
	keys := make([][2]int32, 0, int((radius*2+1)*(radius*2+1)))
	for ring := int32(0); ring <= radius; ring++ {
		for dx := -ring; dx <= ring; dx++ {
			for dz := -ring; dz <= ring; dz++ {
				if ring > 0 && abs32(dx) != ring && abs32(dz) != ring {
					continue
				}
				keys = append(keys, [2]int32{cx + dx, cz + dz})
			}
		}
	}
	return keys
}

func respawnBootstrapCount(viewRadius int32, available int) int {
	radius := min(viewRadius, int32(1))
	count := int((radius*2 + 1) * (radius*2 + 1))
	return min(count, available)
}

func isPlayerMovementPacket(packetID int32) bool {
	switch packetID {
	case packetIDSetPlayerPosition,
		packetIDSetPlayerPositionAndRotation,
		packetIDSetPlayerRotation,
		packetIDSetPlayerOnGround:
		return true
	default:
		return false
	}
}

// handlePlayPacket dispatches a single incoming Play-state packet.
// Returns (true, nil) when the player's position or rotation was updated so the
// caller can broadcast the change to other sessions.
// Returns a non-nil error only for fatal protocol violations.
func handlePlayPacket(pkt *protocol.Packet, p *player.Player, pendingAliveID *int64) (posChanged bool, err error) {
	switch pkt.ID {
	case packetIDClientKeepAlive:
		// Client echoes the Long ID we sent; validate it.
		if len(pkt.Data) < 8 {
			return false, fmt.Errorf("keep-alive packet too short: %d bytes", len(pkt.Data))
		}
		id := int64(binary.BigEndian.Uint64(pkt.Data[:8]))
		if id == *pendingAliveID {
			*pendingAliveID = -1
		}

	case packetIDSetPlayerPosition:
		// C→S: x, y, z (Double×3), flags (Byte; bit 0 = on_ground)
		r := pkt.Reader()
		x, err := protocol.ReadDouble(r)
		if err != nil {
			return false, fmt.Errorf("reading position x: %w", err)
		}
		y, err := protocol.ReadDouble(r)
		if err != nil {
			return false, fmt.Errorf("reading position y: %w", err)
		}
		z, err := protocol.ReadDouble(r)
		if err != nil {
			return false, fmt.Errorf("reading position z: %w", err)
		}
		flags, err := protocol.ReadByte(r)
		if err != nil {
			return false, fmt.Errorf("reading position flags: %w", err)
		}
		p.Position.X, p.Position.Y, p.Position.Z = x, y, z
		p.OnGround = flags&0x01 != 0
		return true, nil

	case packetIDSetPlayerPositionAndRotation:
		// C→S: x, y, z (Double×3), yaw, pitch (Float×2), flags (Byte)
		r := pkt.Reader()
		x, err := protocol.ReadDouble(r)
		if err != nil {
			return false, fmt.Errorf("reading position x: %w", err)
		}
		y, err := protocol.ReadDouble(r)
		if err != nil {
			return false, fmt.Errorf("reading position y: %w", err)
		}
		z, err := protocol.ReadDouble(r)
		if err != nil {
			return false, fmt.Errorf("reading position z: %w", err)
		}
		yaw, err := protocol.ReadFloat(r)
		if err != nil {
			return false, fmt.Errorf("reading yaw: %w", err)
		}
		pitch, err := protocol.ReadFloat(r)
		if err != nil {
			return false, fmt.Errorf("reading pitch: %w", err)
		}
		flags, err := protocol.ReadByte(r)
		if err != nil {
			return false, fmt.Errorf("reading movement flags: %w", err)
		}
		p.Position.X, p.Position.Y, p.Position.Z = x, y, z
		p.Rotation.Yaw, p.Rotation.Pitch = yaw, pitch
		p.OnGround = flags&0x01 != 0
		return true, nil

	case packetIDSetPlayerRotation:
		// C→S: yaw, pitch (Float×2), flags (Byte)
		r := pkt.Reader()
		yaw, err := protocol.ReadFloat(r)
		if err != nil {
			return false, fmt.Errorf("reading yaw: %w", err)
		}
		pitch, err := protocol.ReadFloat(r)
		if err != nil {
			return false, fmt.Errorf("reading pitch: %w", err)
		}
		flags, err := protocol.ReadByte(r)
		if err != nil {
			return false, fmt.Errorf("reading rotation flags: %w", err)
		}
		p.Rotation.Yaw, p.Rotation.Pitch = yaw, pitch
		p.OnGround = flags&0x01 != 0
		return true, nil

	case packetIDSetPlayerOnGround:
		// C→S: flags (Byte; bit 0 = on_ground)
		if len(pkt.Data) >= 1 {
			p.OnGround = pkt.Data[0]&0x01 != 0
		}

	case packetIDPlayerAbilitiesCS:
		// The server controls whether flight is allowed; the client only reports
		// transitions into or out of the flying state (flag 0x02).
		if len(pkt.Data) >= 1 {
			allowed := p.AllowFlying ||
				p.GameMode == player.GameModeCreative ||
				p.GameMode == player.GameModeSpectator
			p.Flying = allowed && pkt.Data[0]&0x02 != 0
		}
	case packetIDConfirmTeleport:
		// Late teleport confirm — ignore, already processed.

	default:
		// Silently drop all other play packets (chat, interaction, etc.).
		// Future milestones will register handlers here.
	}
	return false, nil
}

func applyPlayerFallDamage(sess *session.Session, previousY float64, previousOnGround bool, w *coreworld.World, mgr *session.Manager) {
	if sess == nil || sess.Player == nil {
		return
	}
	p := sess.Player
	if p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator || p.Flying || p.Dead {
		p.FallDistance = 0
		return
	}
	if w != nil && w.TouchesWater(p.Position.X, p.Position.Y, p.Position.Z) {
		p.FallDistance = 0
		return
	}
	if !p.OnGround {
		if drop := previousY - p.Position.Y; drop > 0 {
			p.FallDistance += drop
		}
		return
	}
	if !previousOnGround {
		fallDistance := p.FallDistance
		p.FallDistance = 0
		// Feather Falling reduces safe fall height by 3 blocks per level.
		safeHeight := 3.0 + float64(p.Inventory[8].EnchantmentLevel("minecraft:feather_falling"))*3.0
		damage := float32(math.Floor(fallDistance - safeHeight))
		if damage > 0 {
			DamagePlayer(sess, damage, "hit the ground too hard", mgr)
		}
	}
}

func applyPlayerEnvironmentalDamage(sess *session.Session, w *coreworld.World, mgr *session.Manager, previousX, previousZ float64) {
	if sess == nil || sess.Player == nil || w == nil {
		return
	}
	p := sess.Player
	if p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator || p.Dead {
		p.UnderwaterSince = time.Time{}
		return
	}
	now := time.Now()
	if p.Position.Y < coreworld.WorldMinY-16 {
		if now.Sub(p.LastEnvironmentDamage) >= 500*time.Millisecond {
			p.LastEnvironmentDamage = now
			DamagePlayer(sess, 4, "fell out of the world", mgr)
		}
		return
	}
	x := int(math.Floor(p.Position.X))
	y := int(math.Floor(p.Position.Y))
	z := int(math.Floor(p.Position.Z))
	feetBlock := w.GetBlock(x, y, z)
	feet := feetBlock.ResourceLocation()
	head := w.GetBlock(x, int(math.Floor(p.Position.Y+1.62)), z).ResourceLocation()
	if head == "minecraft:water" || head == "minecraft:bubble_column" {
		if p.UnderwaterSince.IsZero() {
			p.UnderwaterSince = now
		}
		if now.Sub(p.UnderwaterSince) >= 15*time.Second &&
			now.Sub(p.LastEnvironmentDamage) >= time.Second {
			p.LastEnvironmentDamage = now
			DamagePlayer(sess, 2, "drowned", mgr)
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
		DamagePlayer(sess, 4, "tried to swim in lava", mgr)
	case "minecraft:fire", "minecraft:soul_fire":
		p.LastEnvironmentDamage = now
		DamagePlayer(sess, 1, "went up in flames", mgr)
	case "minecraft:cactus":
		p.LastEnvironmentDamage = now
		DamagePlayer(sess, 1, "was pricked to death", mgr)
	case "minecraft:sweet_berry_bush":
		if coreworld.CropAge(feetBlock) > 0 &&
			(math.Abs(p.Position.X-previousX) >= 0.003 || math.Abs(p.Position.Z-previousZ) >= 0.003) {
			p.LastEnvironmentDamage = now
			DamagePlayer(sess, 1, "was pricked to death", mgr)
		}
	}
}

// readConfirmTeleport reads one Confirm Teleport packet and verifies the ID.
func readConfirmTeleport(conn *network.ClientConn, wantID int32) error {
	pkt, err := conn.ReadPacket()
	if err != nil {
		return fmt.Errorf("reading Confirm Teleport: %w", err)
	}
	if pkt.ID != packetIDConfirmTeleport {
		return fmt.Errorf("expected 0x00 (Confirm Teleport), got 0x%02X", pkt.ID)
	}
	r := pkt.Reader()
	gotID, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("reading teleport ID: %w", err)
	}
	if gotID != wantID {
		return fmt.Errorf("teleport ID mismatch: got %d, want %d", gotID, wantID)
	}
	return nil
}

// sendKeepAlive sends a Keep Alive packet (0x26 S→C) with the given ID.
func sendKeepAlive(conn *network.ClientConn, id int64) error {
	return conn.WritePacket(protocol.NewBuilder(packetIDServerKeepAlive).Long(id).Build())
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// posToChunk converts a world coordinate to a chunk coordinate using floor
// division so that negative positions map correctly (e.g. X=-1 → chunk -1).
func posToChunk(pos float64) int32 {
	return int32(math.Floor(pos)) >> 4
}

// abs32 returns the absolute value of n.
func abs32(n int32) int32 {
	if n < 0 {
		return -n
	}
	return n
}
