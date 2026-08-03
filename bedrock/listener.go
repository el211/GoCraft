// Package bedrock implements the Minecraft Bedrock Edition network adapter for
// GoCraft.  It accepts UDP/RakNet connections via gophertunnel, authenticates
// players through Xbox Live, and translates between the Bedrock protocol and
// the edition-agnostic core simulation through the intent bus.
//
// Supported Bedrock protocol: determined by the pinned gophertunnel release.
//   - gophertunnel v1.57.1 → Bedrock protocol 1001 (Minecraft BE 1.26.30)
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
	"encoding/hex"
	"fmt"
	dfblock "github.com/df-mc/dragonfly/server/block"
	dfitem "github.com/df-mc/dragonfly/server/item"
	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl32"
	"log/slog"
	"strings"
	"sync"
	"time"

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
}

type bedrockSession struct {
	conn          *minecraft.Conn
	uuid          [16]byte
	entityID      int32
	displayName   string
	xuid          string
	buildPlatform int32
	skin          protocol.Skin
	knownPlayers  map[[16]byte]bedrockPlayerView
	knownEntities map[int32]bedrockEntityView
	lastHealth    float32
	wasDead       bool
	inventorySent bool
	lastInventory [player.InventorySize]player.ItemStack
	lastHeldSlot  int
	abilitiesSent bool
	lastGameMode  player.GameMode
	lastAllowFly  bool
	lastFlying    bool
	lastFlySpeed  float32
	lastWalkSpeed float32
	lastOperator  bool
	lastGodMode   bool
	teleportMu    sync.Mutex
	teleportPos   *spatial.Vec3
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
	return &Listener{
		cfg:        cfg,
		bus:        bus,
		world:      world,
		game:       game,
		encoder:    bedrockworld.NewEncoder(),
		worldSeed:  worldSeed,
		spawnX:     spawnX,
		spawnZ:     spawnZ,
		gameMode:   gameMode,
		difficulty: difficulty,
		sessions:   make(map[[16]byte]*bedrockSession),
	}
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

	// ── Step 3: send StartGame ────────────────────────────────────────────────
	spawnPos := playerNetworkPosition(result.Position)

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
			ServerAuthoritativeBlockBreaking: false,
		},
		ServerAuthoritativeInventory: true,
		WorldSeed:                    l.worldSeed,
		WorldSpawn:                   protocol.BlockPos{int32(l.spawnX), int32(result.Position.Y), int32(l.spawnZ)},
		ChunkRadius:                  bedrockChunkRadius,
		UseBlockNetworkIDHashes:      false,
	}); err != nil {
		slog.Debug("bedrock: StartGame failed",
			"remote", remote, "displayName", identity.DisplayName, "err", err)
		return
	}
	bedrockSess := &bedrockSession{
		conn:          conn,
		uuid:          playerUUID,
		entityID:      result.EntityID,
		displayName:   identity.DisplayName,
		xuid:          identity.XUID,
		buildPlatform: int32(conn.ClientData().DeviceOS),
		skin:          skinFromClientData(conn.ClientData()),
		knownPlayers:  make(map[[16]byte]bedrockPlayerView),
		knownEntities: make(map[int32]bedrockEntityView),
		lastHealth:    -1,
	}
	defer l.removeSession(playerUUID)
	if p := l.game.GetPlayer(playerUUID); p != nil {
		l.sendLocalPlayerState(bedrockSess, p)
	}

	// ── Step 4: stream initial chunks ────────────────────────────────────────
	const chunkRadius = bedrockChunkRadius
	spawnCX := chunkCoordinate(result.Position.X)
	spawnCZ := chunkCoordinate(result.Position.Z)

	// Bedrock does not render LevelChunk packets until the server publishes
	// the area in which those chunks should remain loaded.
	if err := conn.WritePacket(initialChunkPublisher(result.Position, chunkRadius)); err != nil {
		slog.Debug("bedrock: chunk publisher update failed",
			"displayName", identity.DisplayName, "err", err)
		return
	}

	if err := l.sendInitialChunks(conn, spawnCX, spawnCZ, chunkRadius); err != nil {
		slog.Debug("bedrock: chunk streaming failed",
			"displayName", identity.DisplayName, "err", err)
		return
	}

	// ── Step 5: play loop ─────────────────────────────────────────────────────
	l.playLoop(ctx, conn, bedrockSess, spawnCX, spawnCZ, chunkRadius)
}

// sendInitialChunks sends LevelChunk packets for a square of chunks around the
// spawn position, using SubChunkRequestModeLimitless so the client requests
// block sub-chunks on demand.  Each packet carries the minimum valid biome
// payload (24 sub-chunks of plains biome data + border block count of 0).
func (l *Listener) sendInitialChunks(conn *minecraft.Conn, cx, cz, radius int32) error {
	biomePayload := bedrockworld.EncodeLevelChunkPayload()
	for dx := -radius; dx <= radius; dx++ {
		for dz := -radius; dz <= radius; dz++ {
			if err := conn.WritePacket(&packet.LevelChunk{
				Position:      protocol.ChunkPos{cx + dx, cz + dz},
				Dimension:     0, // overworld
				SubChunkCount: protocol.SubChunkRequestModeLimitless,
				CacheEnabled:  false,
				RawPayload:    biomePayload,
			}); err != nil {
				return fmt.Errorf("sendInitialChunks: %w", err)
			}
		}
	}
	return nil
}

// sendEnteredChunks announces only columns newly covered by a moved view
// window. The client unloads columns outside the publisher radius itself.
func (l *Listener) sendEnteredChunks(conn *minecraft.Conn, oldCX, oldCZ, newCX, newCZ, radius int32) error {
	biomePayload := bedrockworld.EncodeLevelChunkPayload()
	for dx := -radius; dx <= radius; dx++ {
		for dz := -radius; dz <= radius; dz++ {
			cx, cz := newCX+dx, newCZ+dz
			if chunkInsideWindow(cx, cz, oldCX, oldCZ, radius) {
				continue
			}
			if err := conn.WritePacket(&packet.LevelChunk{
				Position:      protocol.ChunkPos{cx, cz},
				Dimension:     0,
				SubChunkCount: protocol.SubChunkRequestModeLimitless,
				CacheEnabled:  false,
				RawPayload:    biomePayload,
			}); err != nil {
				return fmt.Errorf("sendEnteredChunks: %w", err)
			}
		}
	}
	return nil
}

func (l *Listener) updateChunkStream(conn *minecraft.Conn, position spatial.Vec3, cx, cz *int32, radius int32) error {
	if err := conn.WritePacket(initialChunkPublisher(position, radius)); err != nil {
		return fmt.Errorf("publisher update: %w", err)
	}
	newCX, newCZ := chunkCoordinate(position.X), chunkCoordinate(position.Z)
	if newCX == *cx && newCZ == *cz {
		return nil
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
	connDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = conn.Close()
		close(connDone)
	}()

	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			return
		}

		switch p := pk.(type) {
		case *packet.SubChunkRequest:
			l.handleSubChunkRequest(conn, p)
			if !readyForWorldSync {
				// Add the session only after its first requested terrain batch was
				// queued. AddPlayer packets sent earlier may be discarded by the
				// Bedrock client while the destination chunks do not exist yet.
				l.addSession(bedrockSess)
				readyForWorldSync = true
			}

		case *packet.MovePlayer:
			position := canonicalPlayerPosition(p.Position)
			if !l.acceptBedrockMovement(bedrockSess, position) {
				continue
			}
			if err := l.updateChunkStream(conn, position, &streamCX, &streamCZ, streamRadius); err != nil {
				slog.Debug("bedrock: updating chunk stream failed", "displayName", displayName, "err", err)
				return
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
			if err := l.updateChunkStream(conn, position, &streamCX, &streamCZ, streamRadius); err != nil {
				slog.Debug("bedrock: updating chunk stream failed", "displayName", displayName, "err", err)
				return
			}
			l.bus.UpdateMove(intent.MoveIntent{
				PlayerUUID: playerUUID,
				Position:   position,
				Rotation:   spatial.Rotation{Yaw: p.Yaw, Pitch: p.Pitch},
				OnGround:   p.InputData.Load(packet.InputFlagVerticalCollision),
			})
			if p.InputData.Load(packet.InputFlagPerformBlockActions) {
				for _, action := range p.BlockActions {
					l.handlePlayerBlockAction(playerUUID, action.Action, action.BlockPos, action.Face)
				}
			}
			if p.InputData.Load(packet.InputFlagPerformItemInteraction) {
				l.handleUseItemTransaction(playerUUID, &p.ItemInteractionData)
			}
			if p.InputData.Load(packet.InputFlagPerformItemStackRequest) {
				l.handleStackRequests(ctx, conn, playerUUID, []protocol.ItemStackRequest{p.ItemStackRequest})
			}
			l.postInputState(playerUUID, p.InputData)

		case *packet.PlayerAction:
			l.handlePlayerBlockAction(playerUUID, p.ActionType, p.BlockPosition, p.BlockFace)

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

		case *packet.RequestChunkRadius:
			// GoCraft currently streams a four-chunk Bedrock radius.
			_ = conn.WritePacket(&packet.ChunkRadiusUpdated{
				ChunkRadius: bedrockChunkRadius,
			})
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

func (l *Listener) handleStackRequests(ctx context.Context, conn *minecraft.Conn, playerUUID [16]byte, requests []protocol.ItemStackRequest) {
	responses := make([]protocol.ItemStackResponse, 0, len(requests))
	for _, request := range requests {
		actions, ok := canonicalInventoryActions(request.Actions)
		accepted := false
		if ok {
			done := make(chan intent.InventoryResult, 1)
			if l.bus.PostInventory(intent.InventoryIntent{PlayerUUID: playerUUID, Actions: actions, Done: done}) {
				timer := time.NewTimer(2 * time.Second)
				select {
				case result := <-done:
					accepted = result.Accepted
				case <-timer.C:
				case <-ctx.Done():
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
		}
		status := uint8(protocol.ItemStackResponseStatusError)
		if accepted {
			status = protocol.ItemStackResponseStatusOK
		}
		responses = append(responses, protocol.ItemStackResponse{Status: status, RequestID: request.RequestID})
	}
	if len(responses) != 0 {
		_ = conn.WritePacket(&packet.ItemStackResponse{Responses: responses})
	}
}

func canonicalInventoryActions(actions []protocol.StackRequestAction) ([]intent.InventoryAction, bool) {
	out := make([]intent.InventoryAction, 0, len(actions))
	for _, raw := range actions {
		switch action := raw.(type) {
		case *protocol.TakeStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK {
				return nil, false
			}
			out = append(out, intent.InventoryAction{Kind: intent.InventoryActionMove, Source: source, Destination: destination, Count: int(action.Count)})
		case *protocol.PlaceStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK {
				return nil, false
			}
			out = append(out, intent.InventoryAction{Kind: intent.InventoryActionMove, Source: source, Destination: destination, Count: int(action.Count)})
		case *protocol.SwapStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			destination, destinationOK := canonicalInventorySlot(action.Destination)
			if !sourceOK || !destinationOK {
				return nil, false
			}
			out = append(out, intent.InventoryAction{Kind: intent.InventoryActionSwap, Source: source, Destination: destination})
		case *protocol.DropStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			if !sourceOK {
				return nil, false
			}
			out = append(out, intent.InventoryAction{Kind: intent.InventoryActionDrop, Source: source, Count: int(action.Count)})
		case *protocol.DestroyStackRequestAction:
			source, sourceOK := canonicalInventorySlot(action.Source)
			if !sourceOK {
				return nil, false
			}
			out = append(out, intent.InventoryAction{Kind: intent.InventoryActionDestroy, Source: source, Count: int(action.Count)})
		case *protocol.ConsumeStackRequestAction:
			// Consumption accompanies another authoritative action and carries no
			// independent slot destination for this inventory implementation.
			continue
		default:
			return nil, false
		}
	}
	return out, len(out) != 0
}

func canonicalInventorySlot(slot protocol.StackRequestSlotInfo) (int16, bool) {
	switch slot.Container.ContainerID {
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

func (l *Listener) handlePlayerBlockAction(playerUUID [16]byte, action int32, position protocol.BlockPos, face int32) {
	switch action {
	case protocol.PlayerActionStartBreak:
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventStartBlockCracking)
	case protocol.PlayerActionCrackBreak, protocol.PlayerActionContinueDestroyBlock:
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventUpdateBlockCracking)
		l.broadcastBlockHitSound(position)
	case protocol.PlayerActionAbortBreak:
		l.broadcastBlockCrack(playerUUID, position, packet.LevelEventStopBlockCracking)
	case protocol.PlayerActionPredictDestroyBlock, protocol.PlayerActionStopBreak, protocol.PlayerActionCreativePlayerDestroyBlock:
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
	runtimeID := l.encoder.BlockNetworkID(block)
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

func (l *Listener) postInputState(playerUUID [16]byte, input protocol.Bitset) {
	if input.Load(packet.InputFlagStartSprinting) {
		l.postPlayerState(playerUUID, intent.PlayerStateSprinting, true)
	}
	if input.Load(packet.InputFlagStopSprinting) {
		l.postPlayerState(playerUUID, intent.PlayerStateSprinting, false)
	}
	if input.Load(packet.InputFlagStartFlying) {
		l.postPlayerState(playerUUID, intent.PlayerStateFlying, true)
	}
	if input.Load(packet.InputFlagStopFlying) {
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
			entry.HeightMapType, entry.HeightMapData = subChunkHeightMap(chunk, subY)
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
					entry.RawPayload = append(payload, blockActors...)
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
