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

	"GoCraft/core/player"
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

const overworldDimensionName = "minecraft:overworld"

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
func HandlePlay(conn *network.ClientConn, p *player.Player, w *coreworld.World, sender *javaworld.Sender, mgr *session.Manager, cmds *Dispatcher, reg registry.Provider, worldSeed int64, viewDistance, preGenerateRadius int32) error {
	// ── Initial burst ────────────────────────────────────────────────────────
	if viewDistance < 2 {
		viewDistance = 2
	}
	if preGenerateRadius < viewDistance+2 {
		preGenerateRadius = viewDistance + 2
	}
	dimensionTypeID, err := reg.DimensionTypeID(overworldDimensionName)
	if err != nil {
		return fmt.Errorf("play: resolving overworld dimension type: %w", err)
	}
	if err := sendLoginPlay(conn, p, dimensionTypeID, obfuscateSeed(worldSeed), viewDistance); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendPlayerAbilities(conn, p); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	if err := sendCombatAttributes(conn, p); err != nil {
		return fmt.Errorf("play: combat attributes: %w", err)
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
	if err := conn.WritePacket(buildCommandsPacket()); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	slog.Info("player entered play state",
		"remote", conn.RemoteAddr(),
		"name", p.Username,
		"uuid", p.UUID,
	)

	return playLoop(conn, p, teleportID, w, sender, mgr, cmds, viewDistance, preGenerateRadius)
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
		Bool(false).                    // is_hardcore
		VarInt(1).                      // dimension count
		String(overworldDimensionName). // dimension names[0]
		VarInt(20).                     // max_players (informational)
		VarInt(viewDistance).           // view_distance
		VarInt(simulationDistance).     // simulation_distance
		Bool(false).                    // reduced_debug_info
		Bool(true).                     // enable_respawn_screen
		Bool(false).                    // do_limited_crafting
		VarInt(dimensionTypeID).        // direct dimension_type registry ID
		String(overworldDimensionName). // dimension_name
		Long(hashedSeed).               // SHA-256-obfuscated world seed
		Byte(byte(p.GameMode)).         // game_mode (raw signed byte on wire)
		Byte(0xFF).                     // previous_game_mode: raw 0xFF = -1
		Bool(false).                    // is_debug
		Bool(false).                    // is_flat
		Bool(false).                    // no last_death_location follows
		VarInt(0).                      // portal_cooldown
		VarInt(63).                     // sea_level (present in protocol 769)
		Bool(false).                    // enforces_secure_chat
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
	return conn.WritePacket(buildPlayerAbilities(p))
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

// sendDefaultSpawnPosition sends the Set Default Spawn Position packet (0x5B S→C).
//
// Fields:
//
//	Long   position (64-bit packed block position)
//	Float  angle    (spawn compass bearing)
func sendDefaultSpawnPosition(conn *network.ClientConn, p *player.Player) error {
	x := int64(p.Position.X)
	y := int64(p.Position.Y)
	z := int64(p.Position.Z)
	// Minecraft 64-bit packed position: X(26 bits) | Z(26 bits) | Y(12 bits)
	packed := ((x & 0x3FFFFFF) << 38) | ((z & 0x3FFFFFF) << 12) | (y & 0xFFF)
	pkt := protocol.NewBuilder(packetIDSpawnPosition).
		Long(packed).
		Float(0).
		Build()
	return conn.WritePacket(pkt)
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
	pkt := protocol.NewBuilder(packetIDGameEvent).
		Byte(reason).
		Float(value).
		Build()
	return conn.WritePacket(pkt)
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
func playLoop(conn *network.ClientConn, p *player.Player, spawnTeleportID int32, w *coreworld.World, sender *javaworld.Sender, mgr *session.Manager, cmds *Dispatcher, viewRadius, preGenerateRadius int32) error {
	// Must receive Confirm Teleport for the spawn position before anything else.
	if err := readConfirmTeleport(conn, spawnTeleportID); err != nil {
		return fmt.Errorf("play loop: %w", err)
	}

	// ── Multiplayer registration ─────────────────────────────────────────────
	// Register before announcing so ForEachExcept in onPlayerJoin can see us,
	// and the joiner can see existing players.
	// Defers run LIFO: onPlayerLeave fires first (broadcasts while still in map),
	// then Remove cleans up the map entry.
	sess := &session.Session{Player: p, Conn: conn}
	mgr.Add(sess)
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
		if err := sender.SendChunk(conn, c); err != nil {
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

	// teleportTo is given to CommandContext so /tp (and any future teleport
	// command) can reposition the player and immediately stream destination
	// chunks without waiting for the next movement packet.
	//
	// The closure captures sentChunks, lastChunkX, and lastChunkZ by reference
	// so it keeps the play loop's chunk-tracking state consistent.  After it
	// returns, the bottom-of-loop chunk check sees newChunkX == lastChunkX and
	// skips a redundant re-send.
	teleportTo := func(x, y, z float64) error {
		p.Position.X, p.Position.Y, p.Position.Z = x, y, z
		if err := sendSyncPosition(conn, p, 0); err != nil {
			return fmt.Errorf("sync position: %w", err)
		}
		if err := sendSetCenterChunk(conn, p); err != nil {
			return fmt.Errorf("set center chunk: %w", err)
		}
		newCX := posToChunk(x)
		newCZ := posToChunk(z)
		if err := updateChunkView(conn, w, sender, sentChunks, newCX, newCZ, viewRadius, preGenerateRadius); err != nil {
			return fmt.Errorf("update chunk view: %w", err)
		}
		broadcastGeneratedEntities(w, mgr)
		sendExistingMobsInViewTo(conn, w.Entities.Snapshot(), newCX, newCZ, viewRadius)
		lastChunkX, lastChunkZ = newCX, newCZ
		return nil
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
		broadcastGeneratedEntities(w, mgr)

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

		posChanged, err := handlePlayPacket(pkt, p, &pendingAliveID)
		if err != nil {
			return fmt.Errorf("play loop: packet 0x%02X: %w", pkt.ID, err)
		}

		// Broadcast position/rotation to all other sessions on every movement.
		if posChanged {
			broadcastPosition(mgr, p)
		}

		// Chat and commands need the session manager and dispatcher.
		if pkt.ID == packetIDChatMessage || pkt.ID == packetIDChatCommand {
			if err := handleChatPacket(pkt, p, mgr, cmds, w, conn, teleportTo); err != nil {
				slog.Warn("chat error", "player", p.Username, "err", err)
			}
		}

		// Block interaction needs both the world and the session manager.
		if pkt.ID == packetIDPlayerAction || pkt.ID == packetIDUseItemOn {
			if err := handleBlockPacket(pkt, p, w, mgr, conn); err != nil {
				slog.Warn("block interaction error", "player", p.Username, "err", err)
			}
		}

		// Entity interaction (right-click mob) — used for villager trading.
		if pkt.ID == packetIDInteract {
			if err := handleInteractPacket(pkt, p, w, conn); err != nil {
				slog.Warn("interact error", "player", p.Username, "err", err)
			}
		}

		// Inventory management (held item, creative set item, and open containers).
		if pkt.ID == packetIDSetHeldItemCS || pkt.ID == packetIDCreativeModeSetItem {
			if err := handleInventoryPacket(pkt, p); err != nil {
				slog.Warn("inventory error", "player", p.Username, "err", err)
			}
		}
		if pkt.ID == packetIDContainerClick || pkt.ID == packetIDContainerClose {
			if err := handleContainerPacket(pkt, p, conn, w); err != nil {
				slog.Warn("container error", "player", p.Username, "err", err)
			}
		}

		if pkt.ID == packetIDPlaceRecipe {
			if err := handlePlaceRecipeRequest(pkt, p, conn); err != nil {
				slog.Warn("recipe request error", "player", p.Username, "err", err)
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

func broadcastGeneratedEntities(w *coreworld.World, mgr *session.Manager) {
	for _, entity := range w.DrainSpawnedEntities() {
		BroadcastSpawnMob(entity, mgr)
	}
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
			if err := sender.SendChunk(conn, c); err != nil {
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
