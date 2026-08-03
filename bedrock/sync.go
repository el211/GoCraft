package bedrock

import (
	"encoding/base64"
	"math"
	"strings"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"

	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

type bedrockPlayerView struct {
	entityID  int32
	position  spatial.Vec3
	rotation  spatial.Rotation
	inventory [player.InventorySize]player.ItemStack
	heldSlot  int
	sleeping  bool
	health    float32
	dead      bool
}

type bedrockEntityView struct {
	position spatial.Vec3
	yaw      float32
	health   float32
	dead     bool
	riderID  int32
}

// Sync publishes the current canonical simulation snapshot to all Bedrock
// sessions. It is called by the sole simulation tick after intents and entity
// AI have been applied, so Java and Bedrock viewers observe the same state.
func (l *Listener) Sync(tick uint64) {
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	bedrockByUUID := make(map[[16]byte]*bedrockSession, len(l.sessions))
	for id, session := range l.sessions {
		sessions = append(sessions, session)
		bedrockByUUID[id] = session
	}
	l.sessionsMu.RUnlock()
	if len(sessions) == 0 {
		return
	}

	players := make([]*player.Player, 0, l.game.OnlineCount())
	l.game.OnlinePlayers(func(p *player.Player) { players = append(players, p) })
	entities := l.world.Entities.Snapshot()

	for _, viewer := range sessions {
		if tick%20 == 0 {
			_ = viewer.conn.WritePacket(&packet.SetTime{Time: int32(tick)})
		}
		l.syncPlayers(viewer, players, bedrockByUUID, tick)
		l.syncEntities(viewer, entities, tick)
		l.syncLocalHealth(viewer, tick)
		l.syncLocalPlayerState(viewer)
		l.syncLocalInventory(viewer)
	}
}

func (l *Listener) sendLocalPlayerState(viewer *bedrockSession, p *player.Player) {
	_ = viewer.conn.WritePacket(&packet.SetPlayerGameType{GameType: bedrockGameType(p.GameMode)})
	_ = viewer.conn.WritePacket(bedrockAdventureSettings(p))
	_ = viewer.conn.WritePacket(&packet.UpdateAbilities{AbilityData: bedrockSelfAbilityData(p)})
	_ = viewer.conn.WritePacket(&packet.UpdateAttributes{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Attributes:      []protocol.Attribute{bedrockMovementAttribute(p)},
	})
	_ = viewer.conn.WritePacket(&packet.SetActorData{
		EntityRuntimeID: bedrockSelfRuntimeID,
		EntityMetadata:  bedrockPlayerMetadata(p),
	})
	viewer.abilitiesSent = true
	viewer.lastGameMode = p.GameMode
	viewer.lastAllowFly = p.AllowFlying
	viewer.lastFlying = p.Flying
	viewer.lastFlySpeed = p.FlySpeed
	viewer.lastWalkSpeed = p.WalkSpeed
	viewer.lastOperator = p.Operator
	viewer.lastGodMode = p.GodMode
}

func (l *Listener) syncLocalPlayerState(viewer *bedrockSession) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil {
		return
	}
	if viewer.abilitiesSent && viewer.lastGameMode == p.GameMode &&
		viewer.lastAllowFly == p.AllowFlying && viewer.lastFlying == p.Flying &&
		viewer.lastFlySpeed == p.FlySpeed && viewer.lastWalkSpeed == p.WalkSpeed &&
		viewer.lastOperator == p.Operator && viewer.lastGodMode == p.GodMode {
		return
	}
	l.sendLocalPlayerState(viewer, p)
}

// RefreshPlayerAbilities immediately publishes flight, operator, god-mode,
// and movement settings after a command changes canonical player state.
func (l *Listener) RefreshPlayerAbilities(p *player.Player) {
	if p == nil || p.Edition != player.ClientEditionBedrock {
		return
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer != nil {
		l.sendLocalPlayerState(viewer, p)
	}
}

// TeleportPlayer moves the local Bedrock actor and marks the destination as
// an expected server teleport so the next authoritative input is accepted.
func (l *Listener) TeleportPlayer(p *player.Player, position spatial.Vec3, tick uint64) {
	if p == nil || p.Edition != player.ClientEditionBedrock {
		return
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return
	}
	viewer.expectTeleport(position)
	_ = viewer.conn.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Position:        playerNetworkPosition(position),
		Pitch:           p.Rotation.Pitch,
		Yaw:             p.Rotation.Yaw,
		HeadYaw:         p.Rotation.Yaw,
		Mode:            packet.MoveModeTeleport,
		OnGround:        false,
		Tick:            tick,
	})
}

// SendVelocity applies an immediate client-side impulse to one Bedrock player.
// The canonical combat code calls this for the same knockback events that are
// encoded as Set Entity Motion for Java clients.
func (l *Listener) SendVelocity(playerUUID [16]byte, velocity spatial.Vec3, tick uint64) {
	l.sessionsMu.RLock()
	viewer := l.sessions[playerUUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return
	}
	_ = viewer.conn.WritePacket(&packet.SetActorMotion{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Velocity:        mgl32.Vec3{float32(velocity.X), float32(velocity.Y), float32(velocity.Z)},
	})
}

func (l *Listener) syncPlayers(viewer *bedrockSession, players []*player.Player, bedrockByUUID map[[16]byte]*bedrockSession, tick uint64) {
	present := make(map[[16]byte]struct{}, len(players))
	for _, p := range players {
		present[p.UUID] = struct{}{}
		previous, known := viewer.knownPlayers[p.UUID]
		if !known {
			entry := playerListEntry(p, bedrockByUUID[p.UUID], p.UUID == viewer.uuid)
			_ = viewer.conn.WritePacket(&packet.PlayerList{
				ActionType: packet.PlayerListActionAdd,
				Entries:    []protocol.PlayerListEntry{entry},
			})
			if p.UUID != viewer.uuid {
				_ = viewer.conn.WritePacket(buildAddBedrockPlayer(p))
				l.sendPlayerEquipment(viewer, p)
			}
			health, _, _, dead := p.HealthSnapshot()
			viewer.knownPlayers[p.UUID] = bedrockPlayerView{entityID: p.EntityID, position: p.Position, rotation: p.Rotation, inventory: p.Inventory, heldSlot: p.HeldSlot, sleeping: p.Sleeping, health: health, dead: dead}
			continue
		}
		health, _, _, dead := p.HealthSnapshot()
		if p.UUID != viewer.uuid && previous.dead && !dead {
			_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(p.EntityID))})
			_ = viewer.conn.WritePacket(buildAddBedrockPlayer(p))
			l.sendPlayerEquipment(viewer, p)
		}
		if p.UUID != viewer.uuid && health != previous.health {
			_ = viewer.conn.WritePacket(&packet.UpdateAttributes{
				EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID),
				Attributes: []protocol.Attribute{{
					AttributeValue: protocol.AttributeValue{Name: "minecraft:health", Min: 0, Max: p.MaxHealth, Value: health},
					DefaultMin:     0, DefaultMax: p.MaxHealth, Default: p.MaxHealth,
				}}, Tick: tick,
			})
			if health < previous.health && !dead {
				_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID), EventType: packet.ActorEventHurt})
			}
		}
		if p.UUID != viewer.uuid && !previous.dead && dead {
			_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID), EventType: packet.ActorEventDeath})
		}
		if p.UUID != viewer.uuid && (previous.position != p.Position || previous.rotation != p.Rotation) {
			_ = viewer.conn.WritePacket(&packet.MovePlayer{
				EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID),
				Position:        playerNetworkPosition(p.Position),
				Pitch:           p.Rotation.Pitch,
				Yaw:             p.Rotation.Yaw,
				HeadYaw:         p.Rotation.Yaw,
				Mode:            packet.MoveModeNormal,
				OnGround:        p.OnGround,
				Tick:            tick,
			})
		}
		if p.UUID != viewer.uuid && (previous.inventory != p.Inventory || previous.heldSlot != p.HeldSlot) {
			l.sendPlayerEquipment(viewer, p)
		}
		if previous.sleeping != p.Sleeping {
			_ = viewer.conn.WritePacket(&packet.SetActorData{
				EntityRuntimeID: playerRuntimeIDForViewer(viewer, p),
				EntityMetadata:  bedrockPlayerMetadata(p),
				Tick:            tick,
			})
		}
		viewer.knownPlayers[p.UUID] = bedrockPlayerView{entityID: p.EntityID, position: p.Position, rotation: p.Rotation, inventory: p.Inventory, heldSlot: p.HeldSlot, sleeping: p.Sleeping, health: health, dead: dead}
	}

	for id, previous := range viewer.knownPlayers {
		if _, ok := present[id]; ok {
			continue
		}
		if id != viewer.uuid {
			_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(previous.entityID))})
		}
		_ = viewer.conn.WritePacket(&packet.PlayerList{
			ActionType: packet.PlayerListActionRemove,
			Entries:    []protocol.PlayerListEntry{{UUID: uuid.UUID(id)}},
		})
		delete(viewer.knownPlayers, id)
	}
}

func playerRuntimeIDForViewer(viewer *bedrockSession, p *player.Player) uint64 {
	if p.UUID == viewer.uuid {
		return bedrockSelfRuntimeID
	}
	return bedrockRemoteRuntimeID(p.EntityID)
}

func playerListEntry(p *player.Player, session *bedrockSession, self bool) protocol.PlayerListEntry {
	entityID := bedrockRemoteRuntimeID(p.EntityID)
	if self {
		entityID = bedrockSelfRuntimeID
	}
	entry := protocol.PlayerListEntry{
		UUID:           uuid.UUID(p.UUID),
		EntityUniqueID: int64(entityID),
		Username:       p.Username,
		Skin:           defaultPlayerSkin(p.UUID),
	}
	if session != nil {
		entry.XUID = session.xuid
		entry.BuildPlatform = session.buildPlatform
		entry.Skin = session.skin
	}
	return entry
}

func buildAddBedrockPlayer(p *player.Player) *packet.AddPlayer {
	return &packet.AddPlayer{
		UUID:            uuid.UUID(p.UUID),
		Username:        p.Username,
		EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID),
		Position:        playerNetworkPosition(p.Position),
		Pitch:           p.Rotation.Pitch,
		Yaw:             p.Rotation.Yaw,
		HeadYaw:         p.Rotation.Yaw,
		GameType:        int32(p.GameMode),
		EntityMetadata:  bedrockPlayerMetadata(p),
		AbilityData:     bedrockAbilityData(p),
	}
}

func bedrockPlayerMetadata(p *player.Player) protocol.EntityMetadata {
	metadata := protocol.NewEntityMetadata()
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasGravity)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasCollision)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagBreathing)
	metadata[protocol.EntityDataKeyAirSupply] = int16(300)
	metadata[protocol.EntityDataKeyAirSupplyMax] = int16(300)
	if p != nil && p.Sleeping {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagLayingDown)
		metadata[protocol.EntityDataKeyBedPosition] = protocol.BlockPos{p.SpawnPoint.X, p.SpawnPoint.Y, p.SpawnPoint.Z}
	}
	return metadata
}

func (l *Listener) syncEntities(viewer *bedrockSession, entities []*corentity.Entity, tick uint64) {
	present := make(map[int32]struct{}, len(entities))
	for _, entity := range entities {
		present[entity.EntityID] = struct{}{}
		previous, known := viewer.knownEntities[entity.EntityID]
		if !known {
			if spawn := l.buildAddEntity(viewer, entity); spawn != nil {
				_ = viewer.conn.WritePacket(spawn)
				viewer.knownEntities[entity.EntityID] = bedrockEntityView{position: entity.Position, yaw: entity.Yaw, health: entity.Health, dead: entity.Dead, riderID: entity.RiderEntityID}
			}
			continue
		}
		if entity.Health != previous.health {
			_ = viewer.conn.WritePacket(&packet.UpdateAttributes{
				EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID),
				Attributes: []protocol.Attribute{{
					AttributeValue: protocol.AttributeValue{Name: "minecraft:health", Min: 0, Max: entity.MaxHealth, Value: entity.Health},
					DefaultMin:     0, DefaultMax: entity.MaxHealth, Default: entity.MaxHealth,
				}},
				Tick: tick,
			})
			if entity.Health < previous.health && !entity.Dead {
				_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID), EventType: packet.ActorEventHurt})
			}
		}
		if !previous.dead && entity.Dead {
			_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID), EventType: packet.ActorEventDeath})
		}
		if previous.position != entity.Position || previous.yaw != entity.Yaw {
			flags := byte(0)
			if entity.OnGround {
				flags = packet.MoveFlagOnGround
			}
			_ = viewer.conn.WritePacket(&packet.MoveActorAbsolute{
				EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID),
				Flags:           flags,
				Position:        vec32(entity.Position),
				Rotation:        mgl32.Vec3{entity.Pitch, entity.Yaw, entity.Yaw},
			})
		}
		if previous.riderID != entity.RiderEntityID {
			linkType := byte(protocol.EntityLinkRider)
			riderID := entity.RiderEntityID
			if riderID == 0 {
				linkType = protocol.EntityLinkRemove
				riderID = previous.riderID
			}
			_ = viewer.conn.WritePacket(&packet.SetActorLink{EntityLink: protocol.EntityLink{
				RiddenEntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
				RiderEntityUniqueID:  int64(canonicalRuntimeIDForViewer(viewer, riderID)),
				Type:                 linkType,
				Immediate:            linkType == protocol.EntityLinkRemove,
				RiderInitiated:       true,
			}})
		}
		viewer.knownEntities[entity.EntityID] = bedrockEntityView{position: entity.Position, yaw: entity.Yaw, health: entity.Health, dead: entity.Dead, riderID: entity.RiderEntityID}
	}

	for id := range viewer.knownEntities {
		if _, ok := present[id]; ok {
			continue
		}
		_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(id))})
		delete(viewer.knownEntities, id)
	}
}

func canonicalRuntimeIDForViewer(viewer *bedrockSession, entityID int32) uint64 {
	if entityID == viewer.entityID {
		return bedrockSelfRuntimeID
	}
	return bedrockRemoteRuntimeID(entityID)
}

func (l *Listener) buildAddEntity(viewer *bedrockSession, entity *corentity.Entity) packet.Packet {
	metadata := protocol.NewEntityMetadata()
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasGravity)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasCollision)
	if entity.Sleeping {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagLayingDown)
		metadata[protocol.EntityDataKeyBedPosition] = protocol.BlockPos{entity.VillageBed.X, entity.VillageBed.Y, entity.VillageBed.Z}
	}

	if entity.Type == corentity.TypeItem {
		item := l.itemInstance(player.ItemStack{ItemID: entity.ItemID, Count: entity.ItemCount, Damage: entity.ItemDamage}, 1)
		if item.Stack.NetworkID == 0 {
			return nil
		}
		return &packet.AddItemActor{
			EntityUniqueID:  int64(bedrockRemoteRuntimeID(entity.EntityID)),
			EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID),
			Item:            item,
			Position:        vec32(entity.Position),
			Velocity:        mgl32.Vec3{float32(entity.VX), float32(entity.VY), float32(entity.VZ)},
			EntityMetadata:  metadata,
		}
	}

	entityType := bedrockEntityType(entity.Type)
	if entityType == "" {
		return nil
	}
	if entity.Type == corentity.TypeFallingBlock && entity.FallingBlockName != "" {
		block := splitBlockName(entity.FallingBlockName)
		metadata[protocol.EntityDataKeyVariant] = int32(l.encoder.BlockNetworkID(block))
	}
	links := []protocol.EntityLink(nil)
	if entity.RiderEntityID != 0 {
		links = []protocol.EntityLink{{
			RiddenEntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
			RiderEntityUniqueID:  int64(canonicalRuntimeIDForViewer(viewer, entity.RiderEntityID)),
			Type:                 protocol.EntityLinkRider,
			RiderInitiated:       true,
		}}
	}
	return &packet.AddActor{
		EntityUniqueID:  int64(bedrockRemoteRuntimeID(entity.EntityID)),
		EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID),
		EntityType:      entityType,
		Position:        vec32(entity.Position),
		Velocity:        mgl32.Vec3{float32(entity.VX), float32(entity.VY), float32(entity.VZ)},
		Pitch:           entity.Pitch,
		Yaw:             entity.Yaw,
		HeadYaw:         entity.Yaw,
		BodyYaw:         entity.Yaw,
		Attributes: []protocol.AttributeValue{{
			Name: "minecraft:health", Min: 0, Max: entity.MaxHealth, Value: entity.Health,
		}},
		EntityMetadata: metadata,
		EntityLinks:    links,
	}
}

func (l *Listener) syncLocalHealth(viewer *bedrockSession, tick uint64) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil {
		return
	}
	health, _, _, dead := p.HealthSnapshot()
	if viewer.lastHealth > 0 && health < viewer.lastHealth && !dead {
		_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: bedrockSelfRuntimeID, EventType: packet.ActorEventHurt})
	}
	if health != viewer.lastHealth {
		_ = viewer.conn.WritePacket(&packet.UpdateAttributes{
			EntityRuntimeID: bedrockSelfRuntimeID,
			Attributes: []protocol.Attribute{{
				AttributeValue: protocol.AttributeValue{Name: "minecraft:health", Min: 0, Max: p.MaxHealth, Value: health},
				DefaultMin:     0, DefaultMax: p.MaxHealth, Default: p.MaxHealth,
			}},
			Tick: tick,
		})
		viewer.lastHealth = health
	}
	if !viewer.wasDead && dead {
		_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: bedrockSelfRuntimeID, EventType: packet.ActorEventDeath})
	}
	if viewer.wasDead && !dead {
		respawnPosition := playerNetworkPosition(p.Position)
		viewer.expectTeleport(p.Position)
		// Teleport the dead actor first, then complete Bedrock's three-state
		// respawn handshake, matching the vanilla server packet order.
		_ = viewer.conn.WritePacket(&packet.MovePlayer{
			EntityRuntimeID: bedrockSelfRuntimeID,
			Position:        respawnPosition,
			Pitch:           p.Rotation.Pitch,
			Yaw:             p.Rotation.Yaw,
			HeadYaw:         p.Rotation.Yaw,
			Mode:            packet.MoveModeTeleport,
			Tick:            tick,
		})
		_ = viewer.conn.WritePacket(&packet.Respawn{
			Position:        respawnPosition,
			State:           packet.RespawnStateReadyToSpawn,
			EntityRuntimeID: bedrockSelfRuntimeID,
		})

		// Death may have happened far from the bed/world spawn. Publish and
		// announce the respawn area again so the client does not wake into sky.
		const chunkRadius = 4
		_ = viewer.conn.WritePacket(initialChunkPublisher(p.Position, chunkRadius))
		_ = l.sendInitialChunks(viewer.conn, chunkCoordinate(p.Position.X), chunkCoordinate(p.Position.Z), chunkRadius)
	}
	viewer.wasDead = dead
}

func (l *Listener) syncLocalInventory(viewer *bedrockSession) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil || (viewer.inventorySent && viewer.lastInventory == p.Inventory && viewer.lastHeldSlot == p.HeldSlot) {
		return
	}
	content := make([]protocol.ItemInstance, 36)
	for slot := 0; slot < 9; slot++ {
		content[slot] = l.itemInstance(p.Inventory[player.HotbarStart+slot], int32(slot+1))
	}
	for slot := 9; slot < 36; slot++ {
		content[slot] = l.itemInstance(p.Inventory[slot], int32(slot+1))
	}
	_ = viewer.conn.WritePacket(&packet.InventoryContent{WindowID: protocol.WindowIDInventory, Content: content})
	_ = viewer.conn.WritePacket(&packet.InventoryContent{
		WindowID: protocol.WindowIDArmour,
		Content: []protocol.ItemInstance{
			l.itemInstance(p.Inventory[5], 101),
			l.itemInstance(p.Inventory[6], 102),
			l.itemInstance(p.Inventory[7], 103),
			l.itemInstance(p.Inventory[8], 104),
		},
	})
	_ = viewer.conn.WritePacket(&packet.InventoryContent{
		WindowID: protocol.WindowIDOffHand,
		Content:  []protocol.ItemInstance{l.itemInstance(p.Inventory[45], 105)},
	})
	_ = viewer.conn.WritePacket(&packet.MobEquipment{
		EntityRuntimeID: bedrockSelfRuntimeID,
		NewItem:         l.itemInstance(p.HeldItem(), int32(p.HeldSlot+1)),
		InventorySlot:   byte(p.HeldSlot),
		HotBarSlot:      byte(p.HeldSlot),
		WindowID:        protocol.WindowIDInventory,
	})
	viewer.inventorySent = true
	viewer.lastInventory = p.Inventory
	viewer.lastHeldSlot = p.HeldSlot
}

func (l *Listener) sendPlayerEquipment(viewer *bedrockSession, p *player.Player) {
	_ = viewer.conn.WritePacket(&packet.MobEquipment{
		EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID),
		NewItem:         l.itemInstance(p.HeldItem(), int32(p.HeldSlot+1)),
		InventorySlot:   byte(p.HeldSlot),
		HotBarSlot:      byte(p.HeldSlot),
		WindowID:        protocol.WindowIDInventory,
	})
	_ = viewer.conn.WritePacket(&packet.MobArmourEquipment{
		EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID),
		Helmet:          l.itemInstance(p.Inventory[5], 101),
		Chestplate:      l.itemInstance(p.Inventory[6], 102),
		Leggings:        l.itemInstance(p.Inventory[7], 103),
		Boots:           l.itemInstance(p.Inventory[8], 104),
	})
}

// BroadcastMessage sends a server/system chat line to every Bedrock session.
func (l *Listener) BroadcastMessage(message string) {
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, session := range l.sessions {
		sessions = append(sessions, session)
	}
	l.sessionsMu.RUnlock()
	for _, session := range sessions {
		_ = session.conn.WritePacket(&packet.Text{TextType: packet.TextTypeSystem, Message: message})
	}
}

func (l *Listener) SendMessage(playerUUID [16]byte, message string) {
	l.sessionsMu.RLock()
	viewer := l.sessions[playerUUID]
	l.sessionsMu.RUnlock()
	if viewer != nil {
		_ = viewer.conn.WritePacket(&packet.Text{TextType: packet.TextTypeSystem, Message: message})
	}
}

// BroadcastBlockChange sends one canonical mutation to every Bedrock viewer.
func (l *Listener) BroadcastBlockChange(change coreworld.BlockChange) {
	networkID := l.encoder.BlockNetworkID(change.Block)
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, session := range l.sessions {
		sessions = append(sessions, session)
	}
	l.sessionsMu.RUnlock()
	for _, session := range sessions {
		_ = session.conn.WritePacket(&packet.UpdateBlock{
			Position:          protocol.BlockPos{int32(change.X), int32(change.Y), int32(change.Z)},
			NewBlockRuntimeID: networkID,
			Flags:             packet.BlockUpdateNetwork | packet.BlockUpdateNeighbours,
		})
	}
}

// BroadcastBlockBreakEffect emits Bedrock's combined destroy particles and
// block-specific break sound. UpdateBlock alone changes the world silently.
func (l *Listener) BroadcastBlockBreakEffect(position spatial.BlockPos, block coreworld.Block) {
	runtimeID := l.encoder.BlockNetworkID(block)
	blockPosition := mgl32.Vec3{float32(position.X), float32(position.Y), float32(position.Z)}
	centre := mgl32.Vec3{blockPosition.X() + 0.5, blockPosition.Y() + 0.5, blockPosition.Z() + 0.5}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, session := range l.sessions {
		sessions = append(sessions, session)
	}
	l.sessionsMu.RUnlock()
	for _, session := range sessions {
		_ = session.conn.WritePacket(&packet.LevelEvent{
			EventType: packet.LevelEventStopBlockCracking,
			Position:  blockPosition,
		})
		_ = session.conn.WritePacket(&packet.LevelEvent{
			EventType: packet.LevelEventParticlesDestroyBlock,
			Position:  centre,
			EventData: int32(runtimeID),
		})
	}
}

func bedrockEntityType(entityType corentity.EntityType) string {
	switch entityType {
	case corentity.TypeOakBoat, corentity.TypeSpruceBoat, corentity.TypeBirchBoat, corentity.TypeJungleBoat,
		corentity.TypeAcaciaBoat, corentity.TypeDarkOakBoat, corentity.TypeMangroveBoat, corentity.TypeCherryBoat:
		return "minecraft:boat"
	case corentity.TypeOakChestBoat, corentity.TypeSpruceChestBoat, corentity.TypeBirchChestBoat, corentity.TypeJungleChestBoat,
		corentity.TypeAcaciaChestBoat, corentity.TypeDarkOakChestBoat, corentity.TypeMangroveChestBoat, corentity.TypeCherryChestBoat:
		return "minecraft:chest_boat"
	case corentity.TypePrimedTNT:
		return "minecraft:tnt"
	case corentity.TypeFallingBlock:
		return "minecraft:falling_block"
	case corentity.TypeExperienceOrb:
		return "minecraft:xp_orb"
	case corentity.TypeSpectralArrow:
		return "minecraft:arrow"
	case corentity.TypeBambooRaft:
		return "minecraft:bamboo_raft"
	case corentity.TypeBambooChestRaft:
		return "minecraft:bamboo_chest_raft"
	default:
		return string(entityType)
	}
}

type namedItem struct {
	name string
	meta int16
}

func (i namedItem) EncodeItem() (string, int16) { return i.name, i.meta }

func (l *Listener) itemInstance(stack player.ItemStack, stackNetworkID int32) protocol.ItemInstance {
	if stack.IsEmpty() {
		return protocol.ItemInstance{}
	}
	runtimeID, meta, ok := dfworld.ItemRuntimeID(namedItem{name: stack.ItemID})
	if !ok {
		return protocol.ItemInstance{}
	}
	var nbtData map[string]any
	if stack.Damage > 0 {
		nbtData = map[string]any{"Damage": int32(stack.Damage)}
	}
	blockRuntimeID := int32(0)
	block := splitBlockName(stack.ItemID)
	if networkID := l.encoder.BlockNetworkID(block); networkID != l.encoder.BlockNetworkID(coreworld.Air) {
		blockRuntimeID = int32(networkID)
	}
	return protocol.ItemInstance{
		StackNetworkID: stackNetworkID,
		Stack: protocol.ItemStack{
			ItemType:       protocol.ItemType{NetworkID: runtimeID, MetadataValue: uint32(uint16(meta))},
			Count:          uint16(min(stack.Count, math.MaxUint16)),
			BlockRuntimeID: blockRuntimeID,
			HasNetworkID:   true,
			NBTData:        nbtData,
		},
	}
}

func splitBlockName(name string) coreworld.Block {
	parts := strings.SplitN(name, ":", 2)
	if len(parts) == 1 {
		return coreworld.Block{Namespace: "minecraft", Name: name}
	}
	return coreworld.Block{Namespace: parts[0], Name: parts[1]}
}

func vec32(position spatial.Vec3) mgl32.Vec3 {
	return mgl32.Vec3{float32(position.X), float32(position.Y), float32(position.Z)}
}

func skinFromClientData(data login.ClientData) protocol.Skin {
	skinPixels, _ := base64.StdEncoding.DecodeString(data.SkinData)
	capePixels, _ := base64.StdEncoding.DecodeString(data.CapeData)
	resourcePatch, _ := base64.StdEncoding.DecodeString(data.SkinResourcePatch)
	geometry, _ := base64.StdEncoding.DecodeString(data.SkinGeometry)
	geometryVersion, _ := base64.StdEncoding.DecodeString(data.SkinGeometryVersion)
	return protocol.Skin{
		SkinID:                    data.SkinID,
		PlayFabID:                 data.PlayFabID,
		SkinResourcePatch:         resourcePatch,
		SkinImageWidth:            uint32(data.SkinImageWidth),
		SkinImageHeight:           uint32(data.SkinImageHeight),
		SkinData:                  skinPixels,
		CapeImageWidth:            uint32(data.CapeImageWidth),
		CapeImageHeight:           uint32(data.CapeImageHeight),
		CapeData:                  capePixels,
		SkinGeometry:              geometry,
		GeometryDataEngineVersion: geometryVersion,
		PremiumSkin:               data.PremiumSkin,
		PersonaSkin:               data.PersonaSkin,
		PersonaCapeOnClassicSkin:  data.CapeOnClassicSkin,
		PrimaryUser:               true,
		CapeID:                    data.CapeID,
		FullID:                    data.SkinID,
		SkinColour:                data.SkinColour,
		ArmSize:                   data.ArmSize,
		Trusted:                   data.TrustedSkin,
		OverrideAppearance:        true,
	}
}

func defaultPlayerSkin(id [16]byte) protocol.Skin {
	pixels := make([]byte, 64*64*4)
	for index := 3; index < len(pixels); index += 4 {
		pixels[index] = 0xff
	}
	return protocol.Skin{
		SkinID:                    uuid.UUID(id).String(),
		SkinResourcePatch:         []byte(`{"geometry":{"default":"geometry.humanoid.custom"}}`),
		SkinImageWidth:            64,
		SkinImageHeight:           64,
		SkinData:                  pixels,
		GeometryDataEngineVersion: []byte(protocol.CurrentVersion),
		FullID:                    uuid.UUID(id).String(),
		ArmSize:                   "wide",
		SkinColour:                "#b37b62",
		Trusted:                   true,
		OverrideAppearance:        true,
	}
}
