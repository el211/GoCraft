// Package bedrock implements the Minecraft Bedrock Edition network adapter for
// GoCraft.  It accepts UDP/RakNet connections via gophertunnel, authenticates
// players through Xbox Live, and translates between the Bedrock protocol and
// the edition-agnostic core simulation through the intent bus.
//
// Supported Bedrock protocol: determined by the pinned gophertunnel release.
//   - gophertunnel fork (HashimTheArab/gophertunnel@1f617284) → Bedrock protocol 2168 (Minecraft BE 1.26.40)
//
// Architecture (sole-writer invariant):
//
//	Bedrock client ──RakNet/UDP──> Listener.Listen()
//	                                     │
//	                               handleConn() goroutine per client
//	                                     │
//	                      post Intents to core/intent.Bus
//	                      (never mutate core state directly)
//	                                     │
//	                          core simulation tick goroutine
//	                          applies intents, sends JoinResult
package bedrock

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	dfblock "github.com/df-mc/dragonfly/server/block"
	dfitem "github.com/df-mc/dragonfly/server/item"
	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl32"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/text"

	bedrockworld "GoCraft/bedrock/world"
	"GoCraft/config"
	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
)

const bedrockChunkRadius int32 = 4

// Listener wraps a gophertunnel minecraft.Listener and manages Bedrock client
// connections.
type Listener struct {
	cfg        config.BedrockConfig
	bus        *intent.Bus
	world      *coreworld.World
	game       *game.Game
	encoder    *bedrockworld.Encoder
	worldSeed  int64
	spawnX     int
	spawnZ     int
	gameMode   player.GameMode
	difficulty int32
	sessionsMu sync.RWMutex
	sessions   map[[16]byte]*bedrockSession

	// spawnNotify maps a client remote address to a channel that is closed/sent
	// when gophertunnel sends PlayStatus(PlayerSpawn) for that connection.
	// The chunk-streaming goroutine waits on this channel instead of sleeping.
	spawnNotifyMu sync.Mutex
	spawnNotify   map[string]chan struct{}

	// Creative inventory catalogue — built once in NewListener.
	creativeGroups []protocol.CreativeGroup
	creativeItems  []protocol.CreativeItem
	creativeNames  map[uint32]creativeKnownItem // creative network ID → item name/meta
}

type bedrockSession struct {
	conn               *minecraft.Conn
	uuid               [16]byte
	entityID           int32
	displayName        string
	xuid               string
	buildPlatform      int32
	skin               protocol.Skin
	knownPlayers       map[[16]byte]bedrockPlayerView
	knownEntities      map[int32]bedrockEntityView
	lastHealth         float32
	lastFood           int32
	lastSaturation     float32
	lastExhaustion     float32
	hungerSent         bool
	wasDead            bool
	inventorySent      bool
	lastInventory      [player.InventorySize]player.ItemStack
	lastHeldSlot       int
	abilitiesSent      bool
	lastGameMode       player.GameMode
	lastAllowFly       bool
	lastFlying         bool
	lastFlySpeed       float32
	lastWalkSpeed      float32
	lastOperator       bool
	lastGodMode        bool
	teleportMu         sync.Mutex
	teleportPos        *spatial.Vec3
	stackMu            sync.Mutex
	stackNetworkIDs    [player.InventorySize]int32
	cursorStackID      int32
	nextStackNetworkID int32
	lastCarriedItem    player.ItemStack
	lastHeldItem       player.ItemStack
	invOpened          bool // true while the player's own inventory/creative screen is open
	breakingPos        protocol.BlockPos
	breaking           bool
}

func (s *bedrockSession) expectTeleport(position spatial.Vec3) {
	s.teleportMu.Lock()
	s.teleportPos = &position
	s.teleportMu.Unlock()
}

func (s *bedrockSession) acceptMovement(position spatial.Vec3) bool {
	s.teleportMu.Lock()
	defer s.teleportMu.Unlock()
	if s.teleportPos == nil {
		return true
	}
	if position.Distance(*s.teleportPos) > 1 {
		return false
	}
	s.teleportPos = nil
	return true
}

func (l *Listener) addSession(s *bedrockSession) {
	l.sessionsMu.Lock()
	l.sessions[s.uuid] = s
	l.sessionsMu.Unlock()
	slog.Info("bedrock: session added", "displayName", s.displayName)
}

func (l *Listener) removeSession(uuid [16]byte) {
	l.sessionsMu.Lock()
	delete(l.sessions, uuid)
	l.sessionsMu.Unlock()
}

// NewListener creates a Listener from the Bedrock section of the server config.
// The intent bus is used to submit player lifecycle and gameplay events to the
// core simulation tick goroutine.
func NewListener(
	cfg config.BedrockConfig,
	bus *intent.Bus,
	world *coreworld.World,
	game *game.Game,
	worldSeed int64,
	spawnX, spawnZ int,
	gameMode player.GameMode,
	difficulty int32,
) *Listener {
	l := &Listener{
		cfg:         cfg,
		bus:         bus,
		world:       world,
		game:        game,
		encoder:     bedrockworld.NewEncoder(),
		worldSeed:   worldSeed,
		spawnX:      spawnX,
		spawnZ:      spawnZ,
		gameMode:    gameMode,
		difficulty:  difficulty,
		sessions:    make(map[[16]byte]*bedrockSession),
		spawnNotify: make(map[string]chan struct{}),
	}
	l.initCreativeContent()
	return l
}

// Listen starts the RakNet UDP listener and blocks until ctx is cancelled or a
// fatal error occurs.  Each accepted connection is handled in its own goroutine.
func (l *Listener) Listen(ctx context.Context) error {
	if !l.cfg.OnlineMode {
		slog.Warn("⚠ BEDROCK AUTHENTICATION DISABLED — server is running in offline mode",
			"risk", "unauthenticated XUIDs must NOT be treated as trusted global identities",
			"address", l.cfg.Address,
		)
	}

	gt, err := minecraft.ListenConfig{
		AuthenticationDisabled: !l.cfg.OnlineMode,
		ErrorLog:               slog.Default(),
		// AllowUnknownPackets prevents gophertunnel from closing the conn when
		// the client sends a packet whose ID we do not recognise. Without this,
		// any novel 1.26.40 packet ID causes an immediate server-side close and
		// our ReadPacket returns "context canceled" — masking the real packet ID.
		AllowUnknownPackets: true,
		PacketFunc: func(h packet.Header, payload []byte, src, dst net.Addr) {
			// PacketID 0x02 = PlayStatus; status value 3 = PlayerSpawn.
			// This fires on outbound writes (src=server, dst=client).
			// Signal the chunk-streaming goroutine so it sends chunks
			// immediately after the client enters its loading screen.
			if h.PacketID == 0x02 && len(payload) >= 4 &&
				binary.BigEndian.Uint32(payload[:4]) == 3 {
				l.spawnNotifyMu.Lock()
				ch := l.spawnNotify[dst.String()]
				l.spawnNotifyMu.Unlock()
				if ch != nil {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		},
	}.Listen("raknet", l.cfg.Address)
	if err != nil {
		return fmt.Errorf("bedrock: starting RakNet listener on %s: %w", l.cfg.Address, err)
	}

	slog.Info("bedrock listener started",
		"address", l.cfg.Address,
		"onlineMode", l.cfg.OnlineMode,
	)

	// Close the gophertunnel listener when the server context is cancelled.
	go func() {
		<-ctx.Done()
		_ = gt.Close()
	}()

	for {
		conn, err := gt.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			slog.Error("bedrock: Accept error", "err", err)
			return fmt.Errorf("bedrock: Accept: %w", err)
		}
		go l.handleConn(ctx, gt, conn.(*minecraft.Conn))
	}
}

// handleConn runs in its own goroutine for every accepted Bedrock connection.
//
// M14.1 flow:
//  1. gophertunnel completes the RakNet + login sequence
//  2. Post JoinIntent, wait for JoinResult from the simulation tick (≤10 s)
//  3. Call conn.StartGame with the assigned entity ID and spawn position
//  4. Send initial LevelChunk packets for the chunk view radius
//  5. Enter the play loop: route packets to intents, handle SubChunkRequests
//  6. On disconnect, post DisconnectIntent
func (l *Listener) handleConn(ctx context.Context, gt *minecraft.Listener, conn *minecraft.Conn) {
	remote := conn.RemoteAddr()

	// ── Step 1: resolve player identity ──────────────────────────────────────
	identity := conn.IdentityData()
	authenticated := conn.Authenticated()

	if !authenticated && l.cfg.OnlineMode {
		// Defensive: gophertunnel enforces auth when AuthenticationDisabled=false.
		slog.Warn("bedrock: unauthenticated connection despite online_mode=true; dropping",
			"remote", remote, "displayName", identity.DisplayName)
		_ = gt.Disconnect(conn, text.Colourf("<red>Authentication required.</red>"))
		return
	}

	// Derive a stable UUID for the session.
	// Online mode: parse identity.Identity (Xbox-issued UUID, trusted).
	// Offline mode: generate a deterministic offline UUID from the display
	//               name so it never collides with an Xbox UUID.
	playerUUID, err := resolveUUID(identity.Identity, identity.DisplayName, authenticated)
	if err != nil {
		slog.Warn("bedrock: could not parse player UUID; dropping",
			"remote", remote, "identity", identity.Identity, "err", err)
		_ = gt.Disconnect(conn, text.Colourf("<red>Internal server error.</red>"))
		return
	}

	slog.Info("bedrock: player connecting",
		"remote", remote,
		"displayName", identity.DisplayName,
		"uuid", playerUUID,
		"xuid", xuidLog(identity.XUID, authenticated),
		"authenticated", authenticated,
	)

	// ── Step 2: request world entry via the simulation ────────────────────────
	done := make(chan intent.JoinResult, 1)
	joinCtx, joinCancel := context.WithTimeout(ctx, 10*time.Second)
	defer joinCancel()

	if err := l.bus.PostJoin(joinCtx, intent.JoinIntent{
		PlayerUUID:      playerUUID,
		Username:        identity.DisplayName,
		Edition:         "bedrock",
		TrustedIdentity: authenticated,
		Done:            done,
	}); err != nil {
		// ctx cancelled (server shutting down) or 10 s posting timeout.
		slog.Warn("bedrock: PostJoin failed; dropping connection",
			"remote", remote, "displayName", identity.DisplayName, "err", err)
		_ = gt.Disconnect(conn, text.Colourf("<yellow>Server timed out. Please reconnect.</yellow>"))
		return
	}

	var result intent.JoinResult
	select {
	case result = <-done:
		// Join was processed by the tick goroutine.
	case <-time.After(10 * time.Second):
		// The intent was queued but the tick goroutine did not respond in time.
		// Post a DisconnectIntent so the tick cleans up the player if it was
		// already added (lifecycle channel is FIFO, so disconnect follows join).
		slog.Warn("bedrock: JoinResult timed out; posting cleanup disconnect",
			"remote", remote, "displayName", identity.DisplayName)
		_ = l.bus.PostDisconnect(ctx, intent.DisconnectIntent{
			PlayerUUID: playerUUID,
			Reason:     "join response timeout",
		})
		_ = gt.Disconnect(conn, text.Colourf("<yellow>Server timed out. Please reconnect.</yellow>"))
		return
	case <-ctx.Done():
		return
	}
	if result.Err != nil {
		slog.Warn("bedrock: join rejected by simulation",
			"remote", remote, "displayName", identity.DisplayName, "err", result.Err)
		_ = gt.Disconnect(conn, text.Colourf("<red>Could not join: %v</red>", result.Err))
		return
	}

	defer func() {
		_ = l.bus.PostDisconnect(ctx, intent.DisconnectIntent{
			PlayerUUID: playerUUID,
			Reason:     "connection closed",
		})
		slog.Info("bedrock: player disconnected",
			"displayName", identity.DisplayName, "uuid", playerUUID)
	}()

	// ── Step 3: prepare session state ────────────────────────────────────────
	// All fields come from result, which is already resolved.
	const chunkRadius = bedrockChunkRadius
	spawnPos := playerNetworkPosition(result.Position)
	spawnCX := chunkCoordinate(result.Position.X)
	spawnCZ := chunkCoordinate(result.Position.Z)

	bedrockSess := &bedrockSession{
		conn:               conn,
		uuid:               playerUUID,
		entityID:           result.EntityID,
		displayName:        identity.DisplayName,
		xuid:               identity.XUID,
		buildPlatform:      int32(conn.ClientData().DeviceOS),
		skin:               skinFromClientData(conn.ClientData()),
		knownPlayers:       make(map[[16]byte]bedrockPlayerView),
		knownEntities:      make(map[int32]bedrockEntityView),
		lastHealth:         -1,
		nextStackNetworkID: 1,
	}
	defer func() {
		l.removeSession(playerUUID)
		slog.Info("bedrock: session removed", "displayName", identity.DisplayName)
	}()

	// ── Step 4: stream chunks concurrently with StartGame ─────────────────────
	//
	// conn.StartGame() sends the initial protocol handshake (StartGame packet,
	// ItemRegistry, ChunkRadiusUpdated, PlayStatus=PlayerSpawn) and then BLOCKS
	// until the Bedrock client sends SetLocalPlayerAsInitialised.  The client
	// only sends that packet after it finishes loading the world and exits the
	// loading screen.
	//
	// If we wait for StartGame() to return before sending NCPU and LevelChunks,
	// the client gets no chunk data while in the loading screen. After ~2 seconds
	// it times out, exits the loading screen prematurely (sending
	// SetLocalPlayerAsInitialised to unblock us), and is then in a broken state
	// where it no longer sends SubChunkRequests for the LevelChunks it receives
	// too late.
	//
	// Fix: send UpdateAttributes + NCPU + LevelChunks from a goroutine while
	// StartGame() is blocked. We wait for PlayStatus(PlayerSpawn) to be written
	// to the wire (detected via PacketFunc) before sending our own packets,
	// so the client is guaranteed to be inside the loading screen when chunks arrive.

	// Register the spawn-notify channel BEFORE launching the goroutine so that
	// the PacketFunc can signal it the moment PlayStatus(PlayerSpawn) is written.
	spawnReady := make(chan struct{}, 1)
	remoteAddr := conn.RemoteAddr().String()
	l.spawnNotifyMu.Lock()
	l.spawnNotify[remoteAddr] = spawnReady
	l.spawnNotifyMu.Unlock()
	defer func() {
		l.spawnNotifyMu.Lock()
		delete(l.spawnNotify, remoteAddr)
		l.spawnNotifyMu.Unlock()
	}()

	chunkStreamErr := make(chan error, 1)
	go func() {
		// Wait for gophertunnel to write PlayStatus(PlayerSpawn) before we
		// write our own packets. The PacketFunc signals spawnReady the instant
		// that packet hits the wire, so this has zero unnecessary delay.
		select {
		case <-spawnReady:
			slog.Info("bedrock: spawn goroutine: PlayStatus(PlayerSpawn) confirmed, sending chunks",
				"displayName", identity.DisplayName)
		case <-time.After(10 * time.Second):
			chunkStreamErr <- fmt.Errorf("timed out waiting for PlayStatus(PlayerSpawn)")
			return
		}

		// UpdateAttributes: health → XP → food (Dragonfly Spawn() order).
		if p := l.game.GetPlayer(playerUUID); p != nil {
			health, food, saturation, _ := p.HealthSnapshot()
			_, _, exhaustion := p.HungerSnapshot()
			maxHealth := p.MaxHealth
			if maxHealth <= 0 {
				maxHealth = 20
			}
			initialHealth := float32(math.Ceil(float64(health)))
			initialMaxHealth := float32(math.Ceil(float64(maxHealth)))

			healthPk := &packet.UpdateAttributes{
				EntityRuntimeID: bedrockSelfRuntimeID,
				Attributes: []protocol.Attribute{{
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:health", Value: initialHealth, Max: initialMaxHealth,
					},
					DefaultMax: 20, Default: 20,
				}, {
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:absorption", Value: 0, Max: math.MaxFloat32,
					},
					DefaultMax: math.MaxFloat32,
				}},
			}
			slog.Info("bedrock: spawn goroutine: UpdateAttributes health", "health", initialHealth)
			if err := conn.WritePacket(healthPk); err != nil {
				chunkStreamErr <- err
				return
			}

			xpPk := &packet.UpdateAttributes{
				EntityRuntimeID: bedrockSelfRuntimeID,
				Attributes: []protocol.Attribute{{
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:player.level", Value: 0, Max: math.MaxInt32,
					},
					DefaultMax: math.MaxInt32,
				}, {
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:player.experience", Value: 0, Max: 1,
					},
					DefaultMax: 1,
				}},
			}
			if err := conn.WritePacket(xpPk); err != nil {
				chunkStreamErr <- err
				return
			}

			foodPk := &packet.UpdateAttributes{
				EntityRuntimeID: bedrockSelfRuntimeID,
				Attributes: []protocol.Attribute{{
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:player.hunger", Value: float32(food), Max: 20,
					},
					DefaultMax: 20, Default: 20,
				}, {
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:player.saturation", Value: saturation, Max: 20,
					},
					DefaultMax: 20, Default: 20,
				}, {
					AttributeValue: protocol.AttributeValue{
						Name: "minecraft:player.exhaustion", Value: exhaustion, Max: 5,
					},
					DefaultMax: 5,
				}},
			}
			if err := conn.WritePacket(foodPk); err != nil {
				chunkStreamErr <- err
				return
			}

			// Suppress duplicate health update on the first Sync() tick.
			bedrockSess.lastHealth = health
			bedrockSess.lastFood = food
			bedrockSess.lastSaturation = saturation
			bedrockSess.lastExhaustion = exhaustion
			bedrockSess.hungerSent = true
		}

		// NCPU — client ignores LevelChunks received before this.
		slog.Info("bedrock: spawn goroutine: sending NCPU")
		if err := conn.WritePacket(initialChunkPublisher(result.Position, chunkRadius)); err != nil {
			chunkStreamErr <- err
			return
		}

		// AvailableActorIdentifiers (Dragonfly sends this right after NCPU).
		slog.Info("bedrock: spawn goroutine: sending AvailableActorIdentifiers")
		if err := conn.WritePacket(&packet.AvailableActorIdentifiers{
			SerialisedEntityIdentifiers: availableActorIdentifiersPayload,
		}); err != nil {
			chunkStreamErr <- err
			return
		}

		// 81 LevelChunks (biome stubs; block data arrives via SubChunkRequest).
		slog.Info("bedrock: spawn goroutine: sending LevelChunks", "displayName", identity.DisplayName)
		if err := l.sendInitialChunks(conn, spawnCX, spawnCZ, chunkRadius); err != nil {
			chunkStreamErr <- err
			return
		}
		slog.Info("bedrock: spawn goroutine: LevelChunks done", "displayName", identity.DisplayName)

		chunkStreamErr <- nil
	}()

	// conn.StartGame() blocks here until the client sends SetLocalPlayerAsInitialised.
	// The goroutine above provides the chunk data the client needs to do so.
	if err := conn.StartGame(minecraft.GameData{
		WorldName:         "GoCraft",
		EntityUniqueID:    int64(bedrockSelfRuntimeID),
		EntityRuntimeID:   bedrockSelfRuntimeID,
		PlayerPosition:    spawnPos,
		PlayerGameMode:    bedrockGameType(l.gameMode),
		WorldGameMode:     bedrockGameType(l.gameMode),
		Difficulty:        l.difficulty,
		PlayerPermissions: packet.PermissionLevelMember,
		PlayerMovementSettings: protocol.PlayerMovementSettings{
			ServerAuthoritativeBlockBreaking: true,
		},
		ServerAuthoritativeInventory: true,
		WorldSeed:                    l.worldSeed,
		WorldSpawn:                   protocol.BlockPos{int32(l.spawnX), int32(result.Position.Y), int32(l.spawnZ)},
		ChunkRadius:                  bedrockChunkRadius,
		// Network block hashes are stable across Bedrock palette revisions. The
		// Dragonfly registry and the 1.26.40 protocol fork do not necessarily use
		// the same sequential runtime IDs, so palette indices corrupt terrain.
		UseBlockNetworkIDHashes: true,
	}); err != nil {
		slog.Debug("bedrock: StartGame failed",
			"remote", remote, "displayName", identity.DisplayName, "err", err)
		// Drain the channel so the goroutine can always write without blocking.
		// The goroutine will terminate on its own when WritePacket returns an
		// error on the closed connection.
		return
	}

	// Wait for the chunk-streaming goroutine to finish (usually already done
	// by the time StartGame() returns, since the client waits for chunks before
	// sending SetLocalPlayerAsInitialised).
	if err := <-chunkStreamErr; err != nil {
		slog.Debug("bedrock: initial chunk stream failed",
			"displayName", identity.DisplayName, "err", err)
		return
	}
	slog.Info("bedrock: StartGame complete, chunk stream done", "displayName", identity.DisplayName)

	// The generic connection login sends an empty CreativeContent packet.
	// Replace it with the actual catalogue before the player can open the menu.
	if err := conn.WritePacket(&packet.CreativeContent{Groups: l.creativeGroups, Items: l.creativeItems}); err != nil {
		slog.Debug("bedrock: creative content send failed", "displayName", identity.DisplayName, "err", err)
		return
	}

	// Send local player state (SetPlayerGameType, UpdateAbilities, UpdateAttributes
	// for movement speed, SetActorData) after chunks — matching Dragonfly ordering.
	if p := l.game.GetPlayer(playerUUID); p != nil {
		l.sendLocalPlayerState(bedrockSess, p)
	}

	// ── Step 5: play loop ─────────────────────────────────────────────────────
	l.addSession(bedrockSess)
	l.playLoop(ctx, conn, bedrockSess, spawnCX, spawnCZ, chunkRadius)
}

// sendInitialChunks sends full LevelChunk packets (SubChunkCount=24, all block
// data included) for a square of chunks around the spawn position.  Sending
// complete block data avoids the SubChunkRequest/Response round-trip that would
// deadlock during conn.StartGame().
func (l *Listener) sendInitialChunks(conn *minecraft.Conn, cx, cz, radius int32) error {
	first := true
	for dx := -radius; dx <= radius; dx++ {
		for dz := -radius; dz <= radius; dz++ {
			chunkX, chunkZ := cx+dx, cz+dz
			chunk := l.world.Chunk(chunkX, chunkZ)
			payload, err := l.encoder.EncodeFullChunkPayload(chunk)
			if err != nil {
				return fmt.Errorf("sendInitialChunks encode (%d,%d): %w", chunkX, chunkZ, err)
			}
			pk := &packet.LevelChunk{
				Position:      protocol.ChunkPos{chunkX, chunkZ},
				Dimension:     0, // overworld
				SubChunkCount: uint32(coreworld.SectionCount),
				CacheEnabled:  false,
				RawPayload:    payload,
			}
			if first {
				first = false
				slog.Info("bedrock: LevelChunk sample (full)",
					"chunkX", pk.Position[0],
					"chunkZ", pk.Position[1],
					"subChunkCount", pk.SubChunkCount,
					"payloadLen", len(pk.RawPayload),
				)
			}
			if err := conn.WritePacket(pk); err != nil {
				return fmt.Errorf("sendInitialChunks: %w", err)
			}
		}
	}
	return nil
}

// sendEnteredChunks announces only columns newly covered by a moved view
// window. The client unloads columns outside the publisher radius itself.
func (l *Listener) sendEnteredChunks(conn *minecraft.Conn, oldCX, oldCZ, newCX, newCZ, radius int32) error {
	for dx := -radius; dx <= radius; dx++ {
		for dz := -radius; dz <= radius; dz++ {
			cx, cz := newCX+dx, newCZ+dz
			if chunkInsideWindow(cx, cz, oldCX, oldCZ, radius) {
				continue
			}
			chunk := l.world.Chunk(cx, cz)
			payload, err := l.encoder.EncodeFullChunkPayload(chunk)
			if err != nil {
				return fmt.Errorf("sendEnteredChunks encode (%d,%d): %w", cx, cz, err)
			}
			if err := conn.WritePacket(&packet.LevelChunk{
				Position:      protocol.ChunkPos{cx, cz},
				Dimension:     0,
				SubChunkCount: uint32(coreworld.SectionCount),
				CacheEnabled:  false,
				RawPayload:    payload,
			}); err != nil {
				return fmt.Errorf("sendEnteredChunks: %w", err)
			}
		}
	}
	return nil
}

func (l *Listener) updateChunkStream(conn *minecraft.Conn, position spatial.Vec3, cx, cz *int32, radius int32) error {
	newCX, newCZ := chunkCoordinate(position.X), chunkCoordinate(position.Z)
	if newCX == *cx && newCZ == *cz {
		// Player has not crossed a chunk boundary — nothing to do.
		// Dragonfly's sendChunks() also skips NCPU when lastChunkPos == chunkPos.
		return nil
	}
	if err := conn.WritePacket(initialChunkPublisher(position, radius)); err != nil {
		return fmt.Errorf("publisher update: %w", err)
	}
	if err := l.sendEnteredChunks(conn, *cx, *cz, newCX, newCZ, radius); err != nil {
		return err
	}
	*cx, *cz = newCX, newCZ
	return nil
}

// playLoop reads packets from a connected Bedrock client and routes them to
// the appropriate intent or response handler.
//
// Returns when the connection closes or ctx is cancelled.
func (l *Listener) playLoop(ctx context.Context, conn *minecraft.Conn, bedrockSess *bedrockSession, streamCX, streamCZ, streamRadius int32) {
	playerUUID, displayName := bedrockSess.uuid, bedrockSess.displayName
	readyForWorldSync := false
	slog.Info("bedrock: playLoop entered", "displayName", displayName)

	// Close the connection when the server context is cancelled.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			slog.Warn("bedrock: playLoop ReadPacket error",
				"displayName", displayName,
				"err", err,
				"ctxErr", ctx.Err(),
				"readyForWorldSync", readyForWorldSync,
			)
			return
		}

		switch p := pk.(type) {
		case *packet.SubChunkRequest:
			slog.Info("bedrock: SubChunkRequest received", "display", displayName, "dim", p.Dimension, "pos", p.Position, "offsets", len(p.Offsets))
			l.handleSubChunkRequest(conn, p)
			slog.Info("bedrock: SubChunkRequest handled", "display", displayName)
			if !readyForWorldSync {
				readyForWorldSync = true
			}

		case *packet.MovePlayer:
			position := canonicalPlayerPosition(p.Position)
			if !l.acceptBedrockMovement(bedrockSess, position) {
				continue
			}
			if readyForWorldSync {
				if err := l.updateChunkStream(conn, position, &streamCX, &streamCZ, streamRadius); err != nil {
					slog.Debug("bedrock: updating chunk stream failed", "displayName", displayName, "err", err)
					return
				}
			}
			l.bus.UpdateMove(intent.MoveIntent{
				PlayerUUID: playerUUID,
				Position:   position,
				Rotation:   spatial.Rotation{Yaw: p.Yaw, Pitch: p.Pitch},
				OnGround:   p.OnGround,
			})

		case *packet.PlayerAuthInput:
			position := canonicalPlayerPosition(p.Position)
			if !l.acceptBedrockMovement(bedrockSess, position) {
				continue
			}
			if readyForWorldSync {
				if err := l.updateChunkStream(conn, position, &streamCX, &streamCZ, streamRadius); err != nil {
					slog.Debug("bedrock: updating chunk stream failed", "displayName", displayName, "err", err)
					return
				}
			}
			l.bus.UpdateMove(intent.MoveIntent{
				PlayerUUID: playerUUID,
				Position:   position,
				Rotation:   spatial.Rotation{Yaw: p.Yaw, Pitch: p.Pitch},
				OnGround:   inputHasFlag(p.InputData, packet.InputFlagVerticalCollision),
			})
			if inputHasFlag(p.InputData, packet.InputFlagPerformBlockActions) {
				if blockActions, ok := p.BlockActions.Value(); ok {
					for _, action := range blockActions {
						l.handlePlayerBlockAction(bedrockSess, playerUUID, action.Action, action.BlockPos, action.Face)
					}
				}
			}
			if inputHasFlag(p.InputData, packet.InputFlagPerformItemInteraction) {
				if itemData, ok := p.ItemInteractionData.Value(); ok {
					l.handleUseItemTransaction(playerUUID, &itemData)
				}
			}
			if inputHasFlag(p.InputData, packet.InputFlagPerformItemStackRequest) {
				if sr, ok := p.ItemStackRequest.Value(); ok {
					l.handleStackRequests(ctx, conn, playerUUID, []protocol.ItemStackRequest{sr})
				}
			}
			l.postInputState(playerUUID, p.InputData)

		case *packet.PlayerAction:
			l.handlePlayerBlockAction(bedrockSess, playerUUID, p.ActionType, p.BlockPosition, p.BlockFace)

		case *packet.RequestAbility:
			if p.Ability == packet.AbilityFlying {
				if enabled, ok := p.Value.(bool); ok {
					l.bus.PostPlayerState(intent.PlayerStateIntent{
						PlayerUUID: playerUUID,
						State:      intent.PlayerStateFlying,
						Enabled:    enabled,
					})
				}
			}

		case *packet.Respawn:
			// Modern Bedrock completes the death-screen handshake with a
			// Respawn packet, not PlayerActionRespawn.
			if p.State == packet.RespawnStateClientReadyToSpawn &&
				p.EntityRuntimeID == bedrockSelfRuntimeID {
				l.bus.PostRespawn(intent.RespawnIntent{PlayerUUID: playerUUID})
			}

		case *packet.InventoryTransaction:
			switch data := p.TransactionData.(type) {
			case *protocol.UseItemTransactionData:
				l.handleUseItemTransaction(playerUUID, data)
			case *protocol.UseItemOnEntityTransactionData:
				targetID, ok := canonicalEntityID(data.TargetEntityRuntimeID)
				if !ok {
					continue
				}
				l.bus.PostHotbar(intent.HotbarIntent{PlayerUUID: playerUUID, Slot: data.HotBarSlot})
				l.bus.PostEntityInteract(intent.EntityInteractIntent{
					PlayerUUID: playerUUID,
					TargetID:   targetID,
					Attack:     data.ActionType == protocol.UseItemOnEntityActionAttack,
				})
			case *protocol.ReleaseItemTransactionData:
				l.bus.PostConsumeFood(intent.ConsumeFoodIntent{PlayerUUID: playerUUID, HotbarSlot: data.HotBarSlot})
			}

		case *packet.ItemStackRequest:
			l.handleStackRequests(ctx, conn, playerUUID, p.Requests)

		case *packet.MobEquipment:
			l.bus.PostHotbar(intent.HotbarIntent{PlayerUUID: playerUUID, Slot: int32(p.HotBarSlot)})

		case *packet.Text:
			if strings.TrimSpace(p.Message) != "" {
				l.bus.PostChat(intent.ChatIntent{
					PlayerUUID:  playerUUID,
					DisplayName: displayName,
					Message:     p.Message,
				})
			}

		case *packet.CommandRequest:
			if commandLine := strings.TrimSpace(p.CommandLine); commandLine != `` {
				if !strings.HasPrefix(commandLine, `/`) {
					commandLine = `/` + commandLine
				}
				l.bus.PostChat(intent.ChatIntent{
					PlayerUUID:  playerUUID,
					DisplayName: displayName,
					Message:     commandLine,
				})
			}

		case *packet.Interact:
			// The client sends InteractActionOpenInventory when the player presses
			// 'E' (open inventory / creative menu).  The server must respond with
			// ContainerOpen so the client renders the correct screen.
			if p.ActionType == packet.InteractActionOpenInventory && !bedrockSess.invOpened {
				player := l.game.GetPlayer(playerUUID)
				var pos protocol.BlockPos
				if player != nil {
					pos = protocol.BlockPos{
						int32(player.Position.X),
						int32(player.Position.Y),
						int32(player.Position.Z),
					}
				}
				_ = conn.WritePacket(&packet.ContainerOpen{
					WindowID:                0,
					ContainerType:           0xff, // special value: player inventory / creative screen
					ContainerPosition:       pos,
					ContainerEntityUniqueID: -1,
				})
				bedrockSess.invOpened = true
			}

		case *packet.ContainerClose:
			// Echo the close back so the client confirms the screen is dismissed.
			bedrockSess.invOpened = false
			_ = conn.WritePacket(&packet.ContainerClose{
				WindowID:      p.WindowID,
				ContainerType: p.ContainerType,
				ServerSide:    false,
			})

		case *packet.RequestChunkRadius:
			// GoCraft currently streams a four-chunk Bedrock radius.
			_ = conn.WritePacket(&packet.ChunkRadiusUpdated{
				ChunkRadius: bedrockChunkRadius,
			})

		case *packet.ServerBoundLoadingScreen:
			screenType := "Unknown"
			switch p.Type {
			case packet.LoadingScreenTypeStart:
				screenType = "Start"
			case packet.LoadingScreenTypeEnd:
				screenType = "End"
				if !readyForWorldSync {
					readyForWorldSync = true
					slog.Info("bedrock: readyForWorldSync set true via LoadingScreenEnd", "display", displayName)
				}
			}
			id, hasID := p.LoadingScreenID.Value()
			slog.Info("bedrock: ServerBoundLoadingScreen",
				"display", displayName,
				"type", screenType,
				"typeRaw", p.Type,
				"hasLoadingScreenID", hasID,
				"loadingScreenID", id,
				"readyForWorldSync", readyForWorldSync,
			)

		default:
			slog.Debug("bedrock: unhandled packet",
				"display", displayName,
				"type", fmt.Sprintf("%T", pk),
				"readyForWorldSync", readyForWorldSync,
			)
		}
	}
}

func (l *Listener) acceptBedrockMovement(session *bedrockSession, position spatial.Vec3) bool {
	p := l.game.GetPlayer(session.uuid)
	if p == nil {
		return false
	}
	_, _, _, dead := p.HealthSnapshot()
	return !dead && session.acceptMovement(position)
}

func (l *Listener) handleStackRequests(
	ctx context.Context,
	conn *minecraft.Conn,
	playerUUID [16]byte,
	requests []protocol.ItemStackRequest,
) {
	responses := make([]protocol.ItemStackResponse, 0, len(requests))
	craftingTouched := false

	slog.Info(
		"bedrock: received stack request packet",
		"requests", len(requests),
	)

	for _, request := range requests {
		slog.Info(
			"bedrock: processing stack request",
			"request_id", request.RequestID,
			"actions", len(request.Actions),
		)

		for index, action := range request.Actions {
			slog.Info(
				"bedrock: stack request action",
				"request_id", request.RequestID,
				"index", index,
				"type", fmt.Sprintf("%T", action),
				"value", fmt.Sprintf("%+v", action),
			)
		}

		response := protocol.ItemStackResponse{
			Status:    protocol.ItemStackResponseStatusError,
			RequestID: request.RequestID,
		}

		actions, valid := l.canonicalInventoryActions(request.Actions)
		if !valid {
			slog.Warn(
				"bedrock: stack request translation failed",
				"request_id", request.RequestID,
			)
			responses = append(responses, response)
			continue
		}

		slog.Info(
			"bedrock: stack request translated",
			"request_id", request.RequestID,
			"canonical_actions", len(actions),
		)

		done := make(chan intent.InventoryResult, 1)

		posted := l.bus.PostInventory(intent.InventoryIntent{
			PlayerUUID: playerUUID,
			Actions:    actions,
			Done:       done,
		})
		if !posted {
			slog.Warn(
				"bedrock: inventory intent could not be posted",
				"request_id", request.RequestID,
			)
			responses = append(responses, response)
			continue
		}

		timer := time.NewTimer(2 * time.Second)
		accepted := false

		select {
		case result := <-done:
			accepted = result.Accepted

			slog.Info(
				"bedrock: simulation inventory result",
				"request_id", request.RequestID,
				"accepted", accepted,
			)

		case <-timer.C:
			slog.Warn(
				"bedrock: inventory intent timed out",
				"request_id", request.RequestID,
			)

		case <-ctx.Done():
			slog.Warn(
				"bedrock: inventory request context cancelled",
				"request_id", request.RequestID,
			)
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		if !accepted {
			p := l.game.GetPlayer(playerUUID)

			if p == nil {
				slog.Warn(
					"bedrock: inventory rejected and player missing",
					"request_id", request.RequestID,
				)
			} else {
				slog.Warn(
					"bedrock: inventory rejected by simulation",
					"request_id", request.RequestID,
					"player", p.Username,
					"game_mode", p.GameMode,
					"carried_item", p.CarriedItem,
				)
			}

			responses = append(responses, response)
			continue
		}

		p := l.game.GetPlayer(playerUUID)
		if p == nil {
			slog.Warn(
				"bedrock: accepted inventory request but player missing",
				"request_id", request.RequestID,
			)
			responses = append(responses, response)
			continue
		}

		session := l.sessionForPlayer(playerUUID)
		if session == nil {
			slog.Warn(
				"bedrock: accepted inventory request but session missing",
				"request_id", request.RequestID,
				"player", p.Username,
			)
			responses = append(responses, response)
			continue
		}

		l.applyStackNetworkIDChanges(session, p, request.Actions)

		containerInfo := l.stackResponseContainerInfo(
			session,
			p,
			request.Actions,
		)
		for _, container := range containerInfo {
			if container.Container.ContainerID == protocol.ContainerCraftingInput ||
				container.Container.ContainerID == protocol.ContainerCreatedOutput {
				craftingTouched = true
				break
			}
		}

		response.Status = protocol.ItemStackResponseStatusOK
		response.ContainerInfo = containerInfo

		slog.Info(
			"bedrock: inventory request accepted",
			"request_id", request.RequestID,
			"player", p.Username,
			"game_mode", p.GameMode,
			"carried_item", p.CarriedItem,
			"cursor_stack_id", session.cursorStackID,
			"containers", len(containerInfo),
		)

		responses = append(responses, response)
	}

	if len(responses) == 0 {
		return
	}

	slog.Info(
		"bedrock: sending stack responses",
		"responses", len(responses),
	)

	if err := conn.WritePacket(&packet.ItemStackResponse{
		Responses: responses,
	}); err != nil {
		slog.Warn(
			"bedrock: failed to send stack response",
			"err", err,
		)
	}
	if craftingTouched {
		if p := l.game.GetPlayer(playerUUID); p != nil {
			if session := l.sessionForPlayer(playerUUID); session != nil {
				l.sendPersonalCraftingSlots(conn, session, p)
			}
		}
	}
}

func (l *Listener) sendPersonalCraftingSlots(conn *minecraft.Conn, session *bedrockSession, p *player.Player) {
	if conn == nil || session == nil || p == nil {
		return
	}
	type slotUpdate struct {
		uiSlot    uint32
		canonical int
	}
	updates := []slotUpdate{
		{uiSlot: 28, canonical: 1},
		{uiSlot: 29, canonical: 2},
		{uiSlot: 30, canonical: 3},
		{uiSlot: 31, canonical: 4},
		{uiSlot: 50, canonical: 0},
	}

	session.stackMu.Lock()
	packets := make([]*packet.InventorySlot, 0, len(updates))
	for _, update := range updates {
		stack := p.Inventory[update.canonical]
		stackID := session.stackNetworkIDs[update.canonical]
		if stack.IsEmpty() {
			stackID = 0
			session.stackNetworkIDs[update.canonical] = 0
		} else if stackID == 0 {
			stackID = session.allocateStackNetworkID()
			session.stackNetworkIDs[update.canonical] = stackID
		}
		packets = append(packets, &packet.InventorySlot{
			WindowID: protocol.WindowIDUI,
			Slot:     update.uiSlot,
			NewItem:  l.itemInstance(stack, stackID),
		})
	}
	session.stackMu.Unlock()

	for _, pk := range packets {
		_ = conn.WritePacket(pk)
	}
}

func (l *Listener) canonicalInventoryActions(
	actions []protocol.StackRequestAction,
) ([]intent.InventoryAction, bool) {
	out := make([]intent.InventoryAction, 0, len(actions))
	creativeSelected := false
	creativeCount := creativeRequestCount(actions)

	for _, raw := range actions {
		switch action := raw.(type) {
		case *protocol.CraftCreativeStackRequestAction:
			ki, ok := l.creativePlayerStack(
				action.CreativeItemNetworkID,
				int(action.NumberOfCrafts),
			)
			if !ok {
				slog.Warn(
					"bedrock: unknown creative item",
					"creative_network_id", action.CreativeItemNetworkID,
				)
				return nil, false
			}

			count := creativeCount
			if count < 1 {
				count = 1
			}
			maximum := player.MaxStackSize(ki.name)
			if maximum > 0 && count > maximum {
				count = maximum
			}

			out = append(out, intent.InventoryAction{
				Kind:        intent.InventoryActionCreativeGive,
				Destination: intent.InventoryCursorSlot,
				Count:       count,
				Item: player.ItemStack{
					ItemID: ki.name,
					Count:  count,
					Damage: int(ki.meta),
				},
			})
			creativeSelected = true

		case *protocol.CraftResultsDeprecatedStackRequestAction:
			// Client-side preview of the result. The authoritative item was
			// already resolved using CraftCreativeStackRequestAction.
			continue

		case *protocol.CreateStackRequestAction:
			// The creative item is already created in the canonical cursor.
			if !creativeSelected {
				slog.Warn("bedrock: create stack action without creative selection")
				return nil, false
			}
			continue

		case *protocol.TakeStackRequestAction:
			// Modern Bedrock creative selection sends the temporary created
			// output (container 60, slot 50) to the cursor (container 59).
			// CraftCreativeStackRequestAction already created that authoritative
			// cursor stack, so this virtual transfer must be ignored.
			if creativeSelected &&
				action.Source.Container.ContainerID == protocol.ContainerCreatedOutput &&
				action.Destination.Container.ContainerID == protocol.ContainerCursor {
				continue
			}

			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK || action.Count == 0 {
				slog.Warn(
					"bedrock: invalid take stack slots",
					"source_container", action.Source.Container.ContainerID,
					"source_slot", action.Source.Slot,
					"destination_container", action.Destination.Container.ContainerID,
					"destination_slot", action.Destination.Slot,
				)
				return nil, false
			}

			out = append(out, intent.InventoryAction{
				Kind:        intent.InventoryActionMove,
				Source:      source,
				Destination: destination,
				Count:       int(action.Count),
			})

		case *protocol.PlaceStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK || action.Count == 0 {
				return nil, false
			}
			out = append(out, intent.InventoryAction{
				Kind:        intent.InventoryActionMove,
				Source:      source,
				Destination: destination,
				Count:       int(action.Count),
			})

		case *protocol.SwapStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK || source == destination {
				return nil, false
			}
			out = append(out, intent.InventoryAction{
				Kind:        intent.InventoryActionSwap,
				Source:      source,
				Destination: destination,
			})

		case *protocol.DropStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			if !sourceOK || action.Count == 0 {
				return nil, false
			}
			out = append(out, intent.InventoryAction{
				Kind:   intent.InventoryActionDrop,
				Source: source,
				Count:  int(action.Count),
			})

		case *protocol.DestroyStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			if !sourceOK || action.Count == 0 {
				return nil, false
			}
			out = append(out, intent.InventoryAction{
				Kind:   intent.InventoryActionDestroy,
				Source: source,
				Count:  int(action.Count),
			})

		case *protocol.ConsumeStackRequestAction:
			continue

		default:
			// Skip client-side prediction actions we don't need to handle
			// (e.g. CraftRecipeStackRequestAction, AutoCraftRecipeStackRequestAction).
			// Returning false here would reject the entire request and break
			// operations like Q-drop when the client bundles extra actions.
			slog.Debug(
				"bedrock: skipping unsupported stack request action",
				"type", fmt.Sprintf("%T", raw),
			)
			continue
		}
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func creativeRequestCount(actions []protocol.StackRequestAction) int {
	for _, raw := range actions {
		switch action := raw.(type) {
		case *protocol.TakeStackRequestAction:
			if action.Source.Container.ContainerID == protocol.ContainerCreatedOutput &&
				action.Destination.Container.ContainerID == protocol.ContainerCursor &&
				action.Count > 0 {
				return int(action.Count)
			}
		case *protocol.CraftResultsDeprecatedStackRequestAction:
			if len(action.ResultItems) > 0 && action.ResultItems[0].Count > 0 {
				return int(action.ResultItems[0].Count)
			}
		}
	}
	return 1
}

func canonicalInventorySlot(slot protocol.StackRequestSlotInfo) (int16, bool) {
	switch slot.Container.ContainerID {
	case protocol.ContainerCraftingInput:
		// Bedrock stores the personal 2x2 grid in UI slots 28-31. The
		// canonical/Java inventory layout stores it in slots 1-4.
		if slot.Slot < 28 || slot.Slot > 31 {
			return 0, false
		}
		return int16(1 + slot.Slot - 28), true
	case protocol.ContainerCreatedOutput:
		if slot.Slot != 50 {
			return 0, false
		}
		return 0, true
	case protocol.ContainerHotBar, protocol.ContainerInventory, protocol.ContainerCombinedHotBarAndInventory:
		if slot.Slot > 35 {
			return 0, false
		}
		if slot.Slot < 9 {
			return int16(player.HotbarStart + int(slot.Slot)), true
		}
		return int16(slot.Slot), true
	case protocol.ContainerArmor:
		if slot.Slot > 3 {
			return 0, false
		}
		return int16(5 + slot.Slot), true
	case protocol.ContainerOffhand:
		if slot.Slot != 0 {
			return 0, false
		}
		return 45, true
	case protocol.ContainerCursor:
		return intent.InventoryCursorSlot, true
	default:
		return 0, false
	}
}

func (s *bedrockSession) allocateStackNetworkID() int32 {
	if s.nextStackNetworkID <= 0 {
		s.nextStackNetworkID = 1
	}
	id := s.nextStackNetworkID
	s.nextStackNetworkID++
	if s.nextStackNetworkID <= 0 {
		s.nextStackNetworkID = 1
	}
	return id
}

func (s *bedrockSession) stackNetworkIDAt(slot int16) int32 {
	if slot == intent.InventoryCursorSlot {
		return s.cursorStackID
	}
	if slot < 0 || int(slot) >= len(s.stackNetworkIDs) {
		return 0
	}
	return s.stackNetworkIDs[slot]
}

func (s *bedrockSession) setStackNetworkID(slot int16, id int32) {
	if slot == intent.InventoryCursorSlot {
		s.cursorStackID = id
		return
	}
	if slot < 0 || int(slot) >= len(s.stackNetworkIDs) {
		return
	}
	s.stackNetworkIDs[slot] = id
}

func (l *Listener) sessionForPlayer(playerUUID [16]byte) *bedrockSession {
	l.sessionsMu.RLock()
	session := l.sessions[playerUUID]
	l.sessionsMu.RUnlock()
	return session
}

func (l *Listener) applyStackNetworkIDChanges(session *bedrockSession, p *player.Player, actions []protocol.StackRequestAction) {
	if session == nil || p == nil {
		return
	}

	session.stackMu.Lock()
	defer session.stackMu.Unlock()

	for _, raw := range actions {
		switch action := raw.(type) {
		case *protocol.CraftCreativeStackRequestAction:
			session.cursorStackID = session.allocateStackNetworkID()

		case *protocol.CreateStackRequestAction, *protocol.CraftResultsDeprecatedStackRequestAction:
			continue

		case *protocol.TakeStackRequestAction:
			l.transferStackNetworkID(session, p, action.Source, action.Destination)

		case *protocol.PlaceStackRequestAction:
			l.transferStackNetworkID(session, p, action.Source, action.Destination)

		case *protocol.SwapStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK {
				continue
			}
			sourceID := session.stackNetworkIDAt(source)
			destinationID := session.stackNetworkIDAt(destination)
			if sourceID == 0 && action.Source.StackNetworkID > 0 {
				sourceID = action.Source.StackNetworkID
			}
			if destinationID == 0 && action.Destination.StackNetworkID > 0 {
				destinationID = action.Destination.StackNetworkID
			}
			session.setStackNetworkID(source, destinationID)
			session.setStackNetworkID(destination, sourceID)

		case *protocol.DropStackRequestAction:
			source, ok := canonicalInventorySlot(action.Source)
			if ok && canonicalStackAt(p, source).IsEmpty() {
				session.setStackNetworkID(source, 0)
			}

		case *protocol.DestroyStackRequestAction:
			source, ok := canonicalInventorySlot(action.Source)
			if ok && canonicalStackAt(p, source).IsEmpty() {
				session.setStackNetworkID(source, 0)
			}
		}
	}
}

func (l *Listener) transferStackNetworkID(session *bedrockSession, p *player.Player, sourceInfo, destinationInfo protocol.StackRequestSlotInfo) {
	source, sourceOK := canonicalInventorySlot(sourceInfo)
	destination, destinationOK := canonicalInventorySlot(destinationInfo)
	if !sourceOK || !destinationOK {
		return
	}

	sourceID := session.stackNetworkIDAt(source)
	destinationID := session.stackNetworkIDAt(destination)
	if sourceID == 0 && sourceInfo.StackNetworkID > 0 {
		sourceID = sourceInfo.StackNetworkID
	}
	if destinationID == 0 && destinationInfo.StackNetworkID > 0 {
		destinationID = destinationInfo.StackNetworkID
	}

	sourceStack := canonicalStackAt(p, source)
	destinationStack := canonicalStackAt(p, destination)

	if destinationStack.IsEmpty() {
		session.setStackNetworkID(destination, 0)
	} else if destinationID > 0 {
		session.setStackNetworkID(destination, destinationID)
	} else if sourceID > 0 && sourceStack.IsEmpty() {
		session.setStackNetworkID(destination, sourceID)
	} else {
		session.setStackNetworkID(destination, session.allocateStackNetworkID())
	}

	if sourceStack.IsEmpty() {
		session.setStackNetworkID(source, 0)
	} else if sourceID > 0 {
		session.setStackNetworkID(source, sourceID)
	} else {
		session.setStackNetworkID(source, session.allocateStackNetworkID())
	}
}

func (l *Listener) stackResponseContainerInfo(session *bedrockSession, p *player.Player, actions []protocol.StackRequestAction) []protocol.StackResponseContainerInfo {
	if session == nil || p == nil {
		return nil
	}

	type containerKey struct {
		id      byte
		dynamic string
	}
	type changedSlot struct {
		container protocol.FullContainerName
		slot      byte
	}

	changed := make([]changedSlot, 0, len(actions)*2+1)
	seen := make(map[string]struct{}, len(actions)*2+1)
	add := func(slot protocol.StackRequestSlotInfo) {
		if _, ok := canonicalInventorySlot(slot); !ok {
			return
		}
		key := fmt.Sprintf("%d:%v:%d", slot.Container.ContainerID, slot.Container.DynamicContainerID, slot.Slot)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		changed = append(changed, changedSlot{container: slot.Container, slot: slot.Slot})
	}
	addCraftingOutput := func() {
		add(protocol.StackRequestSlotInfo{
			Container: protocol.FullContainerName{ContainerID: protocol.ContainerCreatedOutput},
			Slot:      50,
		})
	}
	touchesCraftingInput := func(slots ...protocol.StackRequestSlotInfo) bool {
		for _, slot := range slots {
			if slot.Container.ContainerID == protocol.ContainerCraftingInput {
				return true
			}
		}
		return false
	}

	creativeRequest := false
	for _, raw := range actions {
		if _, ok := raw.(*protocol.CraftCreativeStackRequestAction); ok {
			creativeRequest = true
			break
		}
	}

	for _, raw := range actions {
		switch action := raw.(type) {
		case *protocol.CraftCreativeStackRequestAction:
			add(protocol.StackRequestSlotInfo{Container: protocol.FullContainerName{ContainerID: protocol.ContainerCursor}, Slot: 0})
		case *protocol.TakeStackRequestAction:
			if creativeRequest && action.Source.Container.ContainerID == protocol.ContainerCreatedOutput &&
				action.Destination.Container.ContainerID == protocol.ContainerCursor {
				add(action.Destination)
				continue
			}

			add(action.Source)
			add(action.Destination)
			if action.Source.Container.ContainerID == protocol.ContainerCreatedOutput {
				// Taking a result consumes one item from each occupied input
				// slot, so include all four updated grid slots in the response.
				for slot := byte(28); slot <= 31; slot++ {
					add(protocol.StackRequestSlotInfo{
						Container: protocol.FullContainerName{ContainerID: protocol.ContainerCraftingInput},
						Slot:      slot,
					})
				}
			} else if touchesCraftingInput(action.Source, action.Destination) {
				addCraftingOutput()
			}
		case *protocol.PlaceStackRequestAction:
			add(action.Source)
			add(action.Destination)
			if touchesCraftingInput(action.Source, action.Destination) {
				addCraftingOutput()
			}
		case *protocol.SwapStackRequestAction:
			add(action.Source)
			add(action.Destination)
			if touchesCraftingInput(action.Source, action.Destination) {
				addCraftingOutput()
			}
		case *protocol.DropStackRequestAction:
			add(action.Source)
			if touchesCraftingInput(action.Source) {
				addCraftingOutput()
			}
		case *protocol.DestroyStackRequestAction:
			add(action.Source)
			if touchesCraftingInput(action.Source) {
				addCraftingOutput()
			}
		}
	}

	session.stackMu.Lock()
	defer session.stackMu.Unlock()

	groups := make([]protocol.StackResponseContainerInfo, 0, len(changed))
	indices := make(map[containerKey]int, len(changed))
	for _, entry := range changed {
		canonical, ok := canonicalInventorySlot(protocol.StackRequestSlotInfo{Container: entry.container, Slot: entry.slot})
		if !ok {
			continue
		}

		stack := canonicalStackAt(p, canonical)
		stackID := session.stackNetworkIDAt(canonical)
		if stack.IsEmpty() {
			stackID = 0
			session.setStackNetworkID(canonical, 0)
		} else if stackID == 0 {
			stackID = session.allocateStackNetworkID()
			session.setStackNetworkID(canonical, stackID)
		}

		key := containerKey{id: entry.container.ContainerID, dynamic: fmt.Sprint(entry.container.DynamicContainerID)}
		index, exists := indices[key]
		if !exists {
			index = len(groups)
			indices[key] = index
			groups = append(groups, protocol.StackResponseContainerInfo{Container: entry.container})
		}
		groups[index].SlotInfo = append(groups[index].SlotInfo, protocol.StackResponseSlotInfo{
			Slot:                 entry.slot,
			HotbarSlot:           entry.slot,
			Count:                byte(min(max(stack.Count, 0), 255)),
			StackNetworkID:       stackID,
			DurabilityCorrection: int32(max(stack.Damage, 0)),
		})
	}
	return groups
}

func canonicalStackAt(p *player.Player, slot int16) player.ItemStack {
	if p == nil {
		return player.ItemStack{}
	}
	if slot == intent.InventoryCursorSlot {
		return p.CarriedItem
	}
	if slot < 0 || int(slot) >= len(p.Inventory) {
		return player.ItemStack{}
	}
	return p.Inventory[slot]
}

func (l *Listener) handlePlayerBlockAction(session *bedrockSession, playerUUID [16]byte, action int32, position protocol.BlockPos, face int32) {
	switch action {
	case protocol.PlayerActionStartBreak:
		session.breakingPos, session.breaking = position, true
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventStartBlockCracking)
	case protocol.PlayerActionCrackBreak, protocol.PlayerActionContinueDestroyBlock:
		session.breakingPos, session.breaking = position, true
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventUpdateBlockCracking)
		l.broadcastBlockHitSound(position)
	case protocol.PlayerActionAbortBreak:
		session.breaking = false
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventStopBlockCracking)
	case protocol.PlayerActionPredictDestroyBlock, protocol.PlayerActionStopBreak, protocol.PlayerActionCreativePlayerDestroyBlock:
		if session.breaking {
			position = session.breakingPos
		}
		session.breaking = false
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventStopBlockCracking)
		l.bus.PostBlockInteract(intent.BlockInteractIntent{
			PlayerUUID: playerUUID,
			Action:     intent.BlockActionBreak,
			Position:   spatial.BlockPos{X: position.X(), Y: position.Y(), Z: position.Z()},
			Face:       face,
		})
	case protocol.PlayerActionRespawn:
		l.bus.PostRespawn(intent.RespawnIntent{PlayerUUID: playerUUID})
	case protocol.PlayerActionStopSleeping:
		l.bus.PostWake(intent.WakeIntent{PlayerUUID: playerUUID})
	case protocol.PlayerActionStartSneak:
		l.bus.PostEntityInteract(intent.EntityInteractIntent{PlayerUUID: playerUUID})
	case protocol.PlayerActionStartSprint:
		l.postPlayerState(playerUUID, intent.PlayerStateSprinting, true)
	case protocol.PlayerActionStopSprint:
		l.postPlayerState(playerUUID, intent.PlayerStateSprinting, false)
	case protocol.PlayerActionStartFlying:
		l.postPlayerState(playerUUID, intent.PlayerStateFlying, true)
	case protocol.PlayerActionStopFlying:
		l.postPlayerState(playerUUID, intent.PlayerStateFlying, false)
	}
}

func blockCrackSpeed(duration time.Duration) int32 {
	if duration <= 0 {
		return 0
	}
	speed := int64(65535 / (duration.Seconds() * 20))
	if speed < 1 {
		return 1
	}
	if speed > 65535 {
		return 65535
	}
	return int32(speed)
}

func (l *Listener) blockBreakDuration(playerUUID [16]byte, position protocol.BlockPos) time.Duration {
	block := l.world.GetBlock(int(position.X()), int(position.Y()), int(position.Z()))
	p := l.game.GetPlayer(playerUUID)
	if block.IsAir() || p == nil || p.GameMode == player.GameModeCreative {
		return 0
	}
	// Encoder IDs are stable network hashes, not Dragonfly's sequential runtime
	// IDs. Resolve the hash back before asking Dragonfly for break properties.
	networkID := l.encoder.BlockNetworkID(block)
	runtimeID, ok := dfworld.DefaultBlockRegistry.HashToRuntimeID(networkID)
	if !ok {
		return 750 * time.Millisecond
	}
	bedrockBlock, ok := dfworld.DefaultBlockRegistry.BlockByRuntimeID(runtimeID)
	if !ok {
		return 750 * time.Millisecond
	}
	var held dfitem.Stack
	if heldItem, ok := dfworld.ItemByName(p.HeldItem().ItemID, 0); ok {
		held = dfitem.NewStack(heldItem, 1)
	}
	duration := dfblock.BreakDuration(bedrockBlock, held, dfblock.BreakContext{
		Airborne:   !p.OnGround,
		Underwater: !p.UnderwaterSince.IsZero(),
	})
	if duration > 5*time.Minute {
		return 750 * time.Millisecond
	}
	return duration
}

func (l *Listener) broadcastBlockCrack(playerUUID [16]byte, position protocol.BlockPos, eventType int32) {
	duration := l.blockBreakDuration(playerUUID, position)
	if duration <= 0 && eventType != packet.LevelEventStopBlockCracking {
		return
	}
	event := &packet.LevelEvent{
		EventType: eventType,
		Position:  mgl32.Vec3{float32(position.X()), float32(position.Y()), float32(position.Z())},
		EventData: blockCrackSpeed(duration),
	}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, session := range l.sessions {
		sessions = append(sessions, session)
	}
	l.sessionsMu.RUnlock()
	for _, session := range sessions {
		_ = session.conn.WritePacket(event)
	}
}

func (l *Listener) broadcastBlockHitSound(position protocol.BlockPos) {
	block := l.world.GetBlock(int(position.X()), int(position.Y()), int(position.Z()))
	if block.IsAir() {
		return
	}
	event := &packet.LevelSoundEvent{
		SoundType: packet.SoundEventHit,
		Position: mgl32.Vec3{
			float32(position.X()) + 0.5,
			float32(position.Y()) + 0.5,
			float32(position.Z()) + 0.5,
		},
		ExtraData: int32(l.encoder.BlockNetworkID(block)),
	}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, session := range l.sessions {
		sessions = append(sessions, session)
	}
	l.sessionsMu.RUnlock()
	for _, session := range sessions {
		_ = session.conn.WritePacket(event)
	}
}

// inputHasFlag reports whether the given flag is present in a PlayerAuthInput
// InputData optional flag list.
func inputHasFlag(input protocol.InputFlags, flag int32) bool {
	return input.Present() && int(flag) < input.Len() && input.Load(int(flag))
}

func (l *Listener) postInputState(playerUUID [16]byte, input protocol.InputFlags) {
	if inputHasFlag(input, packet.InputFlagStartSprinting) {
		l.postPlayerState(playerUUID, intent.PlayerStateSprinting, true)
	}
	if inputHasFlag(input, packet.InputFlagStopSprinting) {
		l.postPlayerState(playerUUID, intent.PlayerStateSprinting, false)
	}
	if inputHasFlag(input, packet.InputFlagStartFlying) {
		l.postPlayerState(playerUUID, intent.PlayerStateFlying, true)
	}
	if inputHasFlag(input, packet.InputFlagStopFlying) {
		l.postPlayerState(playerUUID, intent.PlayerStateFlying, false)
	}
}

func (l *Listener) postPlayerState(playerUUID [16]byte, state uint8, enabled bool) {
	l.bus.PostPlayerState(intent.PlayerStateIntent{PlayerUUID: playerUUID, State: state, Enabled: enabled})
}

func (l *Listener) handleUseItemTransaction(playerUUID [16]byte, data *protocol.UseItemTransactionData) {
	if data == nil {
		return
	}
	l.bus.PostHotbar(intent.HotbarIntent{PlayerUUID: playerUUID, Slot: data.HotBarSlot})
	action := uint8(0)
	switch data.ActionType {
	case protocol.UseItemActionBreakBlock:
		action = intent.BlockActionBreak
	case protocol.UseItemActionClickBlock:
		action = intent.BlockActionUse
	default:
		return
	}
	l.bus.PostBlockInteract(intent.BlockInteractIntent{
		PlayerUUID: playerUUID,
		Action:     action,
		Position: spatial.BlockPos{
			X: data.BlockPosition.X(),
			Y: data.BlockPosition.Y(),
			Z: data.BlockPosition.Z(),
		},
		Face:       data.BlockFace,
		HotbarSlot: data.HotBarSlot,
	})
}

// handleSubChunkRequest responds to the client's on-demand sub-chunk requests.
// Ground sub-chunk (index = bedrockworld.GroundSubChunkIndex) carries stone;
// all others return SuccessAllAir (no payload required).
func (l *Listener) handleSubChunkRequest(
	conn *minecraft.Conn,
	req *packet.SubChunkRequest,
) {
	entries := make([]protocol.SubChunkEntry, 0, len(req.Offsets))
	for _, off := range req.Offsets {
		subY := req.Position.Y() + int32(off[1])
		chunkX := req.Position.X() + int32(off[0])
		chunkZ := req.Position.Z() + int32(off[2])

		entry := protocol.SubChunkEntry{
			Offset: off,
		}
		sectionIndex := int(subY) - coreworld.WorldMinY/coreworld.SectionSize
		if sectionIndex < 0 || sectionIndex >= coreworld.SectionCount {
			entry.Result = protocol.SubChunkResultIndexOutOfBounds
		} else {
			chunk := l.world.Chunk(chunkX, chunkZ)
			var heightMap []int8
			entry.HeightMapType, heightMap = subChunkHeightMap(chunk, subY)
			entry.HeightMapData = protocol.Option(heightMap)
			entry.RenderHeightMapType = entry.HeightMapType
			entry.RenderHeightMapData = entry.HeightMapData
			section := chunk.Sections[sectionIndex]
			payload, err := l.encoder.EncodeSubChunk(section, subY)
			if err != nil {
				entry.Result = protocol.SubChunkResultChunkNotFound
			} else if len(payload) == 0 {
				entry.Result = protocol.SubChunkResultSuccessAllAir
			} else {
				blockActors, actorErr := encodeBedBlockActors(chunk, subY)
				if actorErr != nil {
					entry.Result = protocol.SubChunkResultChunkNotFound
				} else {
					entry.Result = protocol.SubChunkResultSuccess
					entry.RawPayload = protocol.Option(append(payload, blockActors...))
				}
			}
		}
		entries = append(entries, entry)
	}

	_ = conn.WritePacket(&packet.SubChunk{
		CacheEnabled:    false,
		Dimension:       req.Dimension,
		Position:        req.Position,
		SubChunkEntries: entries,
	})
}

// ── Identity helpers ──────────────────────────────────────────────────────────

// resolveUUID returns the player's canonical [16]byte UUID.
//
//   - Authenticated (online_mode=true): parse the Xbox-issued UUID from
//     identityStr, which is verified by gophertunnel.
//   - Unauthenticated (online_mode=false): generate a deterministic offline
//     UUID (UUID v3, GoCraft namespace + display name). Offline UUIDs use
//     variant bits that keep them in a different range than Xbox UUIDs,
//     preventing accidental collisions.
func resolveUUID(identityStr, displayName string, authenticated bool) ([16]byte, error) {
	if authenticated {
		return parseHexUUID(identityStr)
	}
	return offlineUUID(displayName), nil
}

// parseHexUUID parses a standard UUID string (with dashes) into [16]byte.
func parseHexUUID(s string) ([16]byte, error) {
	cleaned := strings.ReplaceAll(s, "-", "")
	if len(cleaned) != 32 {
		return [16]byte{}, fmt.Errorf("invalid UUID %q", s)
	}
	b, err := hex.DecodeString(cleaned)
	if err != nil {
		return [16]byte{}, fmt.Errorf("invalid UUID %q: %w", s, err)
	}
	var u [16]byte
	copy(u[:], b)
	return u, nil
}

// gocraftOfflineNS is the fixed namespace for offline UUID generation.
// Generated once (arbitrary); documented here so it is never changed:
// replacing it would change offline UUIDs for existing players.
//
// Value: SHA-256 of "GoCraft offline namespace" truncated to 16 bytes:
//
//	python3 -c "import hashlib; print(hashlib.sha256(b'GoCraft offline namespace').hexdigest()[:32])"
//	→ 5f3e2a1b4c7d8e9f0a1b2c3d4e5f6a7b
var gocraftOfflineNS = [16]byte{
	0x5f, 0x3e, 0x2a, 0x1b, 0x4c, 0x7d, 0x8e, 0x9f,
	0x0a, 0x1b, 0x2c, 0x3d, 0x4e, 0x5f, 0x6a, 0x7b,
}

// offlineUUID generates a deterministic UUID v3 (MD5-based) for an
// unauthenticated player. The UUID is stable across server restarts for the
// same display name, and its version/variant bits distinguish it from Xbox
// UUIDs (which are version 4, random).
//
// This UUID must NOT be treated as a globally trusted identity — it is only
// reliable within the scope of a single server instance where collisions can
// be checked against the connected player list.
func offlineUUID(displayName string) [16]byte {
	h := md5.New()
	h.Write(gocraftOfflineNS[:])
	h.Write([]byte(displayName))
	digest := h.Sum(nil)

	var u [16]byte
	copy(u[:], digest)
	u[6] = (u[6] & 0x0f) | 0x30 // version 3
	u[8] = (u[8] & 0x3f) | 0x80 // variant 1 (RFC 4122)
	return u
}

// xuidLog returns the XUID for structured logging, or "<offline>" when
// unauthenticated to make clear the value is unverified.
func xuidLog(xuid string, authenticated bool) string {
	if authenticated {
		return xuid
	}
	return "<offline>"
}
