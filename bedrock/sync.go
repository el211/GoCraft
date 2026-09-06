package bedrock

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image/color"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/nbt"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"

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
	usingItem bool
	airSupply int32
	health    float32
	dead      bool
}

type bedrockEntityView struct {
	position           spatial.Vec3
	yaw                float32
	health             float32
	dead               bool
	riderID            int32
	secondRiderID      int32
	villagerVariant    corentity.VillagerVariant
	villagerProfession corentity.VillagerProfession
	villagerLevel      int32
	baby               bool
	sleeping           bool
	inLove             bool
	tamed              bool
	sitting            bool
	saddled            bool
	trusting           bool
	ownerUUID          [16]byte
	hasOwner           bool
	ownerEntityID      int32
	onFire             bool
	usingItem          bool
	pufferState        int32
	cloudRadius        float64
	woolColor          string
	sheared            bool
	collarColor        string
	hasPumpkin         bool
}

func newBedrockEntityView(entity *corentity.Entity) bedrockEntityView {
	return bedrockEntityView{
		position: entity.Position, yaw: entity.Yaw, health: entity.Health, dead: entity.Dead,
		riderID: entity.RiderEntityID, secondRiderID: entity.SecondRiderEntityID,
		villagerVariant:    entity.VillagerVariant,
		villagerProfession: entity.VillagerProfession, villagerLevel: entity.VillagerLevel,
		baby: entity.IsBaby, sleeping: entity.Sleeping, inLove: entity.LoveTicks > 0,
		tamed: entity.Tamed, sitting: entity.Sitting, saddled: entity.Saddled,
		trusting: entity.Trusting, ownerUUID: entity.TameOwnerUUID,
		hasOwner: entity.HasTameOwner, ownerEntityID: entity.TameOwnerEntityID,
		onFire:      entity.FireTicks > 0,
		usingItem:   entity.UsingItem,
		pufferState: entity.PufferState,
		cloudRadius: entity.CloudRadius,
		woolColor:   entity.WoolColor,
		sheared:     entity.Sheared,
		collarColor: entity.CollarColor,
		hasPumpkin:  entity.HasPumpkin,
	}
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
	entitiesByDimension := make(map[int32][]*corentity.Entity, len(l.worlds))
	for dimension, dimensionWorld := range l.worlds {
		if dimensionWorld != nil {
			entitiesByDimension[dimension] = dimensionWorld.Entities.Snapshot()
		}
	}

	for _, viewer := range sessions {
		if tick%20 == 0 {
			_ = viewer.conn.WritePacket(&packet.SetTime{Time: int32(tick)})
		}
		l.syncPlayerList(viewer, players, bedrockByUUID)
		l.syncPlayers(viewer, players, bedrockByUUID, tick)
		l.syncEntities(viewer, entitiesByDimension[viewer.dimension.Load()], tick)
		l.syncLocalHealth(viewer, tick)
		l.syncLocalHunger(viewer, tick)
		l.syncLocalExperience(viewer, tick)
		l.syncLocalStatusEffects(viewer, tick)
		l.syncLocalPlayerState(viewer)
		l.syncLocalInventory(viewer)
	}
}

func (l *Listener) syncLocalExperience(viewer *bedrockSession, tick uint64) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil {
		return
	}
	level, _, progress := p.ExperienceSnapshot()
	if viewer.experienceSent && viewer.lastExperienceLevel == level && viewer.lastExperience == progress {
		return
	}
	_ = viewer.conn.WritePacket(&packet.UpdateAttributes{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Attributes: []protocol.Attribute{
			{AttributeValue: protocol.AttributeValue{Name: "minecraft:player.level", Min: 0, Max: math.MaxInt32, Value: float32(level)}, DefaultMax: math.MaxInt32},
			{AttributeValue: protocol.AttributeValue{Name: "minecraft:player.experience", Min: 0, Max: 1, Value: progress}, DefaultMax: 1},
		},
		Tick: tick,
	})
	viewer.lastExperienceLevel = level
	viewer.lastExperience = progress
	viewer.experienceSent = true
}

func (l *Listener) syncLocalHunger(viewer *bedrockSession, tick uint64) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil {
		return
	}
	food, saturation, exhaustion := p.HungerSnapshot()
	if viewer.hungerSent && food == viewer.lastFood && saturation == viewer.lastSaturation && exhaustion == viewer.lastExhaustion {
		return
	}
	_ = viewer.conn.WritePacket(&packet.UpdateAttributes{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Attributes: []protocol.Attribute{
			{AttributeValue: protocol.AttributeValue{Name: "minecraft:player.hunger", Min: 0, Max: 20, Value: float32(food)}, DefaultMin: 0, DefaultMax: 20, Default: 20},
			{AttributeValue: protocol.AttributeValue{Name: "minecraft:player.saturation", Min: 0, Max: 20, Value: saturation}, DefaultMin: 0, DefaultMax: 20, Default: 20},
			{AttributeValue: protocol.AttributeValue{Name: "minecraft:player.exhaustion", Min: 0, Max: 5, Value: exhaustion}, DefaultMin: 0, DefaultMax: 5},
		},
		Tick: tick,
	})
	viewer.lastFood = food
	viewer.lastSaturation = saturation
	viewer.lastExhaustion = exhaustion
	viewer.hungerSent = true
}

func (l *Listener) sendLocalPlayerState(viewer *bedrockSession, p *player.Player) {
	_ = viewer.conn.WritePacket(&packet.SetPlayerGameType{GameType: bedrockGameType(p.GameMode)})
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

// HasSession reports whether this player can be written to.
//
// The server asks before announcing a join to plugins: a greeting sent while
// the client is still logging in has nowhere to go, and the plugin would look
// like it did nothing.
func (l *Listener) HasSession(uuid [16]byte) bool {
	l.sessionsMu.RLock()
	_, ok := l.sessions[uuid]
	l.sessionsMu.RUnlock()
	return ok
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
	viewerPlayer := l.game.GetPlayer(viewer.uuid)
	for _, p := range players {
		if p.Edition == player.ClientEditionBedrock && p.UUID != viewer.uuid && bedrockByUUID[p.UUID] == nil {
			continue // Do not publish a Bedrock player before its own world is ready.
		}
		if !bedrockPlayerInView(viewerPlayer, p) {
			continue
		}
		present[p.UUID] = struct{}{}
		previous, known := viewer.knownPlayers[p.UUID]
		if !known {
			targetSession := bedrockByUUID[p.UUID]
			platform := int32(0)
			if targetSession != nil {
				platform = targetSession.buildPlatform
			} else if p.Edition == player.ClientEditionJava {
				platform = viewer.buildPlatform
			}
			if p.UUID != viewer.uuid {
				_ = viewer.conn.WritePacket(buildAddBedrockPlayer(p, platform))
				l.sendPlayerEquipment(viewer, p)
			}
			health, _, _, dead := p.HealthSnapshot()
			viewer.knownPlayers[p.UUID] = bedrockPlayerView{entityID: p.EntityID, position: p.Position, rotation: p.Rotation, inventory: p.Inventory, heldSlot: p.HeldSlot, sleeping: p.Sleeping, airSupply: p.AirSupplySnapshot(), health: health, dead: dead}
			continue
		}
		health, _, _, dead := p.HealthSnapshot()
		airSupply := p.AirSupplySnapshot()
		if p.UUID != viewer.uuid && previous.dead && !dead {
			_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(p.EntityID))})
			platform := int32(0)
			if session := bedrockByUUID[p.UUID]; session != nil {
				platform = session.buildPlatform
			} else if p.Edition == player.ClientEditionJava {
				platform = viewer.buildPlatform
			}
			_ = viewer.conn.WritePacket(buildAddBedrockPlayer(p, platform))
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
			if bedrockRemotePlayerNeedsRespawn(previous.position, p.Position) {
				_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(p.EntityID))})
				platform := viewer.buildPlatform
				if targetSession := bedrockByUUID[p.UUID]; targetSession != nil {
					platform = targetSession.buildPlatform
				}
				_ = viewer.conn.WritePacket(buildAddBedrockPlayer(p, platform))
				l.sendPlayerEquipment(viewer, p)
			} else {
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
		}
		if p.UUID != viewer.uuid && (previous.inventory != p.Inventory || previous.heldSlot != p.HeldSlot) {
			l.sendPlayerEquipment(viewer, p)
		}
		if previous.sleeping != p.Sleeping || previous.usingItem != (p.UsingItemID != "") || previous.airSupply != airSupply {
			_ = viewer.conn.WritePacket(&packet.SetActorData{
				EntityRuntimeID: playerRuntimeIDForViewer(viewer, p),
				EntityMetadata:  bedrockPlayerMetadata(p),
				Tick:            tick,
			})
		}
		viewer.knownPlayers[p.UUID] = bedrockPlayerView{entityID: p.EntityID, position: p.Position, rotation: p.Rotation, inventory: p.Inventory, heldSlot: p.HeldSlot, sleeping: p.Sleeping, usingItem: p.UsingItemID != "", airSupply: airSupply, health: health, dead: dead}
	}

	for id, previous := range viewer.knownPlayers {
		if _, ok := present[id]; ok {
			continue
		}
		if id != viewer.uuid {
			_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(previous.entityID))})
		}
		delete(viewer.knownPlayers, id)
	}
}

func (l *Listener) syncPlayerList(viewer *bedrockSession, players []*player.Player, bedrockByUUID map[[16]byte]*bedrockSession) {
	present := make(map[[16]byte]struct{}, len(players))
	for _, p := range bedrockPlayerListCandidates(viewer.uuid, players, bedrockByUUID) {
		present[p.UUID] = struct{}{}
		if _, listed := viewer.listedPlayers[p.UUID]; listed {
			continue
		}
		targetSession := bedrockByUUID[p.UUID]
		entry := playerListEntry(p, targetSession, p.UUID == viewer.uuid)
		if targetSession == nil && p.Edition == player.ClientEditionJava {
			entry.Skin = crossEditionFallbackSkin(viewer.skin, p.UUID)
			entry.BuildPlatform = viewer.buildPlatform
		}
		entry.ActionType = protocol.PlayerListActionAdd
		_ = viewer.conn.WritePacket(&packet.PlayerList{Entries: []protocol.PlayerListEntry{entry}})
		viewer.listedPlayers[p.UUID] = struct{}{}
	}
	for id := range viewer.listedPlayers {
		if _, online := present[id]; online {
			continue
		}
		_ = viewer.conn.WritePacket(&packet.PlayerList{Entries: []protocol.PlayerListEntry{{
			ActionType: protocol.PlayerListActionRemove,
			UUID:       uuid.UUID(id),
		}}})
		delete(viewer.listedPlayers, id)
	}
}

func bedrockPlayerListCandidates(viewerUUID [16]byte, players []*player.Player, bedrockByUUID map[[16]byte]*bedrockSession) []*player.Player {
	result := make([]*player.Player, 0, len(players))
	for _, p := range players {
		if p == nil || p.Edition == player.ClientEditionBedrock && p.UUID != viewerUUID && bedrockByUUID[p.UUID] == nil {
			continue
		}
		result = append(result, p)
	}
	return result
}

func playerRuntimeIDForViewer(viewer *bedrockSession, p *player.Player) uint64 {
	if p.UUID == viewer.uuid {
		return bedrockSelfRuntimeID
	}
	return bedrockRemoteRuntimeID(p.EntityID)
}

func bedrockPlayerInView(viewer, target *player.Player) bool {
	if viewer == nil || target == nil || viewer.UUID == target.UUID {
		return viewer != nil && target != nil
	}
	if viewer.Dimension != target.Dimension {
		return false
	}
	dx := chunkCoordinate(target.Position.X) - chunkCoordinate(viewer.Position.X)
	dz := chunkCoordinate(target.Position.Z) - chunkCoordinate(viewer.Position.Z)
	return dx >= -bedrockChunkRadius && dx <= bedrockChunkRadius &&
		dz >= -bedrockChunkRadius && dz <= bedrockChunkRadius
}

// bedrockRemotePlayerNeedsRespawn separates smooth movement from an
// authoritative teleport. Re-spawning a remote actor avoids routing command
// teleports through the local-player correction path used by MoveModeTeleport.
func bedrockRemotePlayerNeedsRespawn(previous, current spatial.Vec3) bool {
	const maxNormalDelta = 8.0
	return math.Abs(current.X-previous.X) > maxNormalDelta ||
		math.Abs(current.Y-previous.Y) > maxNormalDelta ||
		math.Abs(current.Z-previous.Z) > maxNormalDelta
}

func entityInView(viewer *player.Player, entity *corentity.Entity) bool {
	if viewer == nil {
		return false
	}
	dx := chunkCoordinate(entity.Position.X) - chunkCoordinate(viewer.Position.X)
	dz := chunkCoordinate(entity.Position.Z) - chunkCoordinate(viewer.Position.Z)
	return dx >= -bedrockChunkRadius && dx <= bedrockChunkRadius &&
		dz >= -bedrockChunkRadius && dz <= bedrockChunkRadius
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

func buildAddBedrockPlayer(p *player.Player, buildPlatform int32) *packet.AddPlayer {
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
		BuildPlatform:   buildPlatform,
	}
}

func crossEditionFallbackSkin(source protocol.Skin, id [16]byte) protocol.Skin {
	result := source
	identity := uuid.UUID(id).String()
	result.PlayFabID = ""
	result.SkinID = identity
	result.FullID = identity
	result.PrimaryUser = false
	result.Trusted = true
	result.OverrideAppearance = true
	return result
}

func bedrockPlayerMetadata(p *player.Player) protocol.EntityMetadata {
	metadata := protocol.NewEntityMetadata()
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasGravity)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasCollision)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagBreathing)
	air := int16(player.MaxAirSupply)
	if p != nil {
		air = int16(p.AirSupplySnapshot())
	}
	metadata[protocol.EntityDataKeyAirSupply] = air
	metadata[protocol.EntityDataKeyAirSupplyMax] = int16(player.MaxAirSupply)
	if p != nil && p.Sleeping {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagLayingDown)
		metadata[protocol.EntityDataKeyBedPosition] = protocol.BlockPos{p.SpawnPoint.X, p.SpawnPoint.Y, p.SpawnPoint.Z}
	}
	if p != nil && p.UsingItemID != "" {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagUsingItem)
	}
	return metadata
}

// BroadcastPlayerUsingItemState immediately starts/stops the Bedrock use-item
// animation for the local player and observers. A completed use also emits the
// vanilla use-item event for eating particles and sound.
func (l *Listener) BroadcastPlayerUsingItemState(p *player.Player, completed bool) {
	if l == nil || p == nil {
		return
	}
	l.sessionsMu.RLock()
	viewers := make([]*bedrockSession, 0, len(l.sessions))
	for _, viewer := range l.sessions {
		viewers = append(viewers, viewer)
	}
	l.sessionsMu.RUnlock()
	for _, viewer := range viewers {
		if !bedrockPlayerInView(l.game.GetPlayer(viewer.uuid), p) {
			continue
		}
		runtimeID := playerRuntimeIDForViewer(viewer, p)
		_ = viewer.conn.WritePacket(&packet.SetActorData{
			EntityRuntimeID: runtimeID,
			EntityMetadata:  bedrockPlayerMetadata(p),
		})
		if completed {
			_ = viewer.conn.WritePacket(&packet.ActorEvent{EntityRuntimeID: runtimeID, EventType: packet.ActorEventUseItem})
			for _, sound := range completedFoodSoundEvents(p, runtimeID) {
				_ = viewer.conn.WritePacket(sound)
			}
		}
	}
}

// BroadcastPlayerArmSwing sends the Bedrock Animate packet to every other
// viewer in the attacker's dimension. Bedrock currently exposes one generic
// swing action, so off-hand Java swings use the same remote animation.
func (l *Listener) BroadcastPlayerArmSwing(p *player.Player) {
	if l == nil || p == nil {
		return
	}
	l.sessionsMu.RLock()
	viewers := make([]*bedrockSession, 0, len(l.sessions))
	for uuid, viewer := range l.sessions {
		if uuid != p.UUID && viewer.dimension.Load() == p.Dimension {
			viewers = append(viewers, viewer)
		}
	}
	l.sessionsMu.RUnlock()
	for _, viewer := range viewers {
		_ = viewer.conn.WritePacket(&packet.Animate{
			ActionType:      packet.AnimateActionSwingArm,
			EntityRuntimeID: bedrockRemoteRuntimeID(p.EntityID),
			SwingSource:     packet.AnimateSwingSourceAttack,
		})
	}
}

func completedFoodSoundEvents(p *player.Player, runtimeID uint64) [2]*packet.LevelSoundEvent {
	position := mgl32.Vec3{}
	if p != nil {
		position = vec32(p.Position)
	}
	makeEvent := func(soundType string) *packet.LevelSoundEvent {
		return &packet.LevelSoundEvent{
			SoundType:      soundType,
			Position:       position,
			ExtraData:      -1,
			EntityType:     "minecraft:player",
			EntityUniqueID: int64(runtimeID),
		}
	}
	return [2]*packet.LevelSoundEvent{makeEvent(packet.SoundEventEat), makeEvent(packet.SoundEventBurp)}
}

func (l *Listener) syncEntities(viewer *bedrockSession, entities []*corentity.Entity, tick uint64) {
	viewerPlayer := l.game.GetPlayer(viewer.uuid)
	present := make(map[int32]struct{}, len(entities))
	for _, entity := range entities {
		// Only track and spawn entities within the player's loaded chunk radius.
		// Sending AddActor for entities in columns the client has never received
		// a LevelChunk for causes the client to crash or disconnect.
		if !entityInView(viewerPlayer, entity) {
			continue
		}
		present[entity.EntityID] = struct{}{}
		previous, known := viewer.knownEntities[entity.EntityID]
		if !known {
			if spawn := l.buildAddEntity(viewer, entity); spawn != nil {
				_ = viewer.conn.WritePacket(spawn)
				l.sendEntityEquipment(viewer, entity)
				viewer.knownEntities[entity.EntityID] = newBedrockEntityView(entity)
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
		if previous.riderID != entity.RiderEntityID || previous.secondRiderID != entity.SecondRiderEntityID {
			previousPassengers := []int32{previous.riderID, previous.secondRiderID}
			currentPassengers := entity.PassengerIDs()
			for _, riderID := range previousPassengers {
				if riderID == 0 || containsEntityID(currentPassengers, riderID) {
					continue
				}
				_ = viewer.conn.WritePacket(&packet.SetActorLink{EntityLink: protocol.EntityLink{
					RiddenEntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
					RiderEntityUniqueID:  int64(canonicalRuntimeIDForViewer(viewer, riderID)),
					Type:                 protocol.EntityLinkRemove, Immediate: true, RiderInitiated: true,
				}})
			}
			for _, riderID := range currentPassengers {
				if riderID == previous.riderID || riderID == previous.secondRiderID {
					continue
				}
				_ = viewer.conn.WritePacket(&packet.SetActorLink{EntityLink: protocol.EntityLink{
					RiddenEntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
					RiderEntityUniqueID:  int64(canonicalRuntimeIDForViewer(viewer, riderID)),
					Type:                 protocol.EntityLinkRider, RiderInitiated: true,
				}})
			}
		}
		if previous.sleeping != entity.Sleeping || previous.baby != entity.IsBaby ||
			previous.inLove != (entity.LoveTicks > 0) || previous.tamed != entity.Tamed ||
			previous.sitting != entity.Sitting || previous.saddled != entity.Saddled ||
			previous.trusting != entity.Trusting || previous.ownerEntityID != entity.TameOwnerEntityID ||
			previous.ownerUUID != entity.TameOwnerUUID || previous.hasOwner != entity.HasTameOwner ||
			previous.onFire != (entity.FireTicks > 0) ||
			previous.usingItem != entity.UsingItem ||
			previous.pufferState != entity.PufferState ||
			previous.cloudRadius != entity.CloudRadius ||
			previous.woolColor != entity.WoolColor || previous.sheared != entity.Sheared ||
			previous.collarColor != entity.CollarColor || previous.hasPumpkin != entity.HasPumpkin ||
			entity.Type == corentity.TypeVillager &&
				(previous.villagerVariant != entity.VillagerVariant || previous.villagerProfession != entity.VillagerProfession ||
					previous.villagerLevel != entity.VillagerLevel) {
			_ = viewer.conn.WritePacket(&packet.SetActorData{
				EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID),
				EntityMetadata:  l.bedrockEntityMetadata(viewer, entity),
				Tick:            tick,
			})
		}
		viewer.knownEntities[entity.EntityID] = newBedrockEntityView(entity)
	}

	for id := range viewer.knownEntities {
		if _, ok := present[id]; ok {
			continue
		}
		_ = viewer.conn.WritePacket(&packet.RemoveActor{EntityUniqueID: int64(bedrockRemoteRuntimeID(id))})
		delete(viewer.knownEntities, id)
	}
}

func (l *Listener) sendEntityEquipment(viewer *bedrockSession, entity *corentity.Entity) {
	if viewer == nil || entity == nil || entity.MainHandItemID == "" {
		return
	}
	_ = viewer.conn.WritePacket(&packet.MobEquipment{
		EntityRuntimeID: bedrockRemoteRuntimeID(entity.EntityID),
		NewItem: l.itemInstance(player.ItemStack{
			ItemID: entity.MainHandItemID,
			Count:  1,
		}, 1),
		WindowID: protocol.WindowIDInventory,
	})
}

func canonicalRuntimeIDForViewer(viewer *bedrockSession, entityID int32) uint64 {
	if viewer != nil && entityID == viewer.entityID {
		return bedrockSelfRuntimeID
	}
	return bedrockRemoteRuntimeID(entityID)
}

func containsEntityID(ids []int32, wanted int32) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

func (l *Listener) buildAddEntity(viewer *bedrockSession, entity *corentity.Entity) packet.Packet {
	metadata := l.bedrockEntityMetadata(viewer, entity)
	if entity.Type == corentity.TypeExperienceOrb {
		return &packet.SpawnExperienceOrb{
			Position:         vec32(entity.Position),
			ExperienceAmount: entity.ExperienceAmount,
		}
	}

	if entity.Type == corentity.TypeItem {
		item := l.itemInstance(entity.DroppedItem(), 1)
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
	links := make([]protocol.EntityLink, 0, 2)
	for _, riderID := range entity.PassengerIDs() {
		links = append(links, protocol.EntityLink{
			RiddenEntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
			RiderEntityUniqueID:  int64(canonicalRuntimeIDForViewer(viewer, riderID)),
			Type:                 protocol.EntityLinkRider,
			RiderInitiated:       true,
		})
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
		EntityMetadata:  metadata,
		EntityLinks:     links,
	}
}

func (l *Listener) bedrockEntityMetadata(viewer *bedrockSession, entity *corentity.Entity) protocol.EntityMetadata {
	metadata := protocol.NewEntityMetadata()
	if entity == nil || entity.Type != corentity.TypeFireworkRocket && entity.Type != corentity.TypeAreaEffectCloud {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasGravity)
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasCollision)
	}
	if entity == nil {
		return metadata
	}
	if entity.FireTicks > 0 {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagOnFire)
	}
	if entity.UsingItem {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagUsingItem)
	}
	if entity.Sleeping {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagLayingDown)
		metadata[protocol.EntityDataKeyBedPosition] = protocol.BlockPos{entity.VillageBed.X, entity.VillageBed.Y, entity.VillageBed.Z}
	}
	if entity.IsBaby {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagBaby)
	}
	if entity.LoveTicks > 0 {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagInLove)
	}
	if entity.Saddled {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagSaddled)
	}
	if entity.Sitting {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagSitting)
	}
	if entity.Tamed {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagTamed)
	}
	if entity.Trusting {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagTrusting)
	}
	if entity.HasTameOwner && entity.TameOwnerEntityID != 0 {
		metadata[protocol.EntityDataKeyOwner] = int64(canonicalRuntimeIDForViewer(viewer, entity.TameOwnerEntityID))
	}
	if entity.Type == corentity.TypeVillager {
		metadata[protocol.EntityDataKeyVariant] = bedrockVillagerProfessionID(entity.VillagerProfession)
		metadata[protocol.EntityDataKeyMarkVariant] = bedrockVillagerVariantID(entity.VillagerVariant)
		level := entity.VillagerLevel
		if level < 1 {
			level = 1
		}
		metadata[protocol.EntityDataKeyTradeTier] = level - 1
		metadata[protocol.EntityDataKeyMaxTradeTier] = int32(4)
		metadata[protocol.EntityDataKeyTradeExperience] = int32(0)
		metadata[protocol.EntityDataKeyScale] = float32(1)
		if entity.IsBaby {
			metadata[protocol.EntityDataKeyScale] = float32(0.5)
		}
	}
	if entity.Type == corentity.TypePufferfish {
		metadata[protocol.EntityDataKeyPuffedState] = entity.PufferState
	}
	if entity.Type == corentity.TypeFallingBlock && entity.FallingBlockName != "" && l != nil && l.encoder != nil {
		block := splitBlockName(entity.FallingBlockName)
		metadata[protocol.EntityDataKeyVariant] = int32(l.encoder.BlockNetworkID(block))
	}
	if entity.Type == corentity.TypeFireworkRocket {
		metadata[protocol.EntityDataKeyDisplayFirework] = bedrockFireworkNBT(entity.FireworkData)
	}
	if entity.Type == corentity.TypePotion {
		if potionID, ok := bedrockPotionID(entity.ProjectileItem); ok {
			metadata[protocol.EntityDataKeyAuxValueData] = potionID
			if potionID > 4 {
				metadata[protocol.EntityDataKeyCustomDisplay] = byte(potionID + 1)
			}
		}
	}
	if entity.Type == corentity.TypeAreaEffectCloud {
		metadata[protocol.EntityDataKeyDataRadius] = float32(entity.CloudRadius)
		metadata[protocol.EntityDataKeyDataDuration] = int32(math.MaxInt32)
		metadata[protocol.EntityDataKeyDataChangeOnPickup] = float32(math.SmallestNonzeroFloat32)
		metadata[protocol.EntityDataKeyDataChangeRate] = float32(math.SmallestNonzeroFloat32)
	}
	if entity.Type == corentity.TypeSheep {
		metadata[protocol.EntityDataKeyColorIndex] = byte(handler.SheepColorID(entity.WoolColor))
		if entity.Sheared {
			metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagSheared)
		}
	}
	if entity.Tamed && entity.CollarColor != "" &&
		(entity.Type == corentity.TypeWolf || entity.Type == corentity.TypeCat) {
		metadata[protocol.EntityDataKeyColorIndex] = byte(handler.DyeColorID(entity.CollarColor))
	}
	if entity.Type == corentity.TypeSnowGolem && !entity.HasPumpkin {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagSheared)
	}
	return metadata
}

func (l *Listener) syncLocalHealth(viewer *bedrockSession, tick uint64) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil {
		return
	}
	health, _, _, dead := p.HealthSnapshot()
	if viewer.wasDead && !dead {
		respawnPosition := playerNetworkPosition(p.Position)
		viewer.expectTeleport(p.Position)
		// The client already sent RespawnStateClientReadyToSpawn when the
		// button was clicked. Complete the handshake with ReadyToSpawn. Sending
		// MovePlayer and a full 81-chunk burst before this packet is processed
		// makes current Bedrock clients cancel the connection.
		_ = viewer.conn.WritePacket(&packet.Respawn{
			Position:        respawnPosition,
			State:           packet.RespawnStateReadyToSpawn,
			EntityRuntimeID: bedrockSelfRuntimeID,
		})
	}
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
	viewer.wasDead = dead
}

func (l *Listener) syncLocalInventory(viewer *bedrockSession) {
	p := l.game.GetPlayer(viewer.uuid)
	if p == nil {
		return
	}
	heldItem := p.HeldItem()
	inventoryChanged := !viewer.inventorySent ||
		viewer.lastInventory != p.Inventory ||
		viewer.lastCarriedItem != p.CarriedItem
	selectionChanged := viewer.inventorySent && viewer.lastHeldSlot != p.HeldSlot
	if !inventoryChanged && !selectionChanged {
		return
	}
	// The selected hotbar slot is client-owned during normal play. MobEquipment
	// received from the client is applied by the simulation before Sync runs.
	// Echoing that state back to the same client here is unsafe: while the player
	// scrolls quickly, this tick may contain the previous selection and visually
	// roll a newer client selection back. A slot-only change has no inventory
	// content to synchronise, so only advance the local snapshot.
	if !inventoryChanged {
		viewer.lastHeldSlot = p.HeldSlot
		viewer.lastHeldItem = heldItem
		return
	}

	viewer.stackMu.Lock()
	sendInitialSelection := shouldBootstrapHotbarSelection(viewer.inventorySent, viewer.clientHeldSlotSeen)
	for slot := 0; slot < player.InventorySize; slot++ {
		stack := p.Inventory[slot]
		if stack.IsEmpty() {
			viewer.stackNetworkIDs[slot] = 0
			continue
		}
		previous := viewer.lastInventory[slot]
		if viewer.stackNetworkIDs[slot] == 0 || (!previous.IsEmpty() && !previous.SameItem(stack)) {
			viewer.stackNetworkIDs[slot] = viewer.allocateStackNetworkID()
		}
	}
	if p.CarriedItem.IsEmpty() {
		viewer.cursorStackID = 0
	} else if viewer.cursorStackID == 0 ||
		(!viewer.lastCarriedItem.IsEmpty() && !viewer.lastCarriedItem.SameItem(p.CarriedItem)) {
		viewer.cursorStackID = viewer.allocateStackNetworkID()
	}

	var content []protocol.ItemInstance
	var armour []protocol.ItemInstance
	var offhand []protocol.ItemInstance
	var uiContent []protocol.ItemInstance
	playerScreen := viewer.invOpened && p.OpenContainerKind == ""
	if inventoryChanged {
		// Bedrock's Inventory container is always 36 slots in hotbar-then-main
		// order, including while the player screen is open. Player-screen slots
		// are synchronised separately below, like Pumpkin's slot update path.
		content = make([]protocol.ItemInstance, 36)
		for slot := range content {
			canonical := bedrockInventoryCanonicalSlot(slot)
			content[slot] = l.itemInstance(p.Inventory[canonical], viewer.stackNetworkIDs[canonical])
		}
		armour = []protocol.ItemInstance{
			l.itemInstance(p.Inventory[5], viewer.stackNetworkIDs[5]),
			l.itemInstance(p.Inventory[6], viewer.stackNetworkIDs[6]),
			l.itemInstance(p.Inventory[7], viewer.stackNetworkIDs[7]),
			l.itemInstance(p.Inventory[8], viewer.stackNetworkIDs[8]),
		}
		offhand = []protocol.ItemInstance{
			l.itemInstance(p.Inventory[player.OffhandSlot], viewer.stackNetworkIDs[player.OffhandSlot]),
		}
		// The Bedrock cursor is slot 0 of the 54-slot UI inventory. Sending an
		// entirely empty UI inventory here clears the client's mouse cursor after
		// every authoritative change. That made picked-up stacks disappear and
		// left their original inventory slots looking stuck/ghosted.
		uiContent = make([]protocol.ItemInstance, 54)
		uiContent[0] = l.itemInstance(p.CarriedItem, viewer.cursorStackID)
		for _, update := range craftingSlotUpdates(p) {
			if update.windowID != protocol.WindowIDUI || int(update.slot) >= len(uiContent) {
				continue
			}
			stack := canonicalStackAt(p, update.canonical)
			stackID := viewer.stackNetworkIDAt(update.canonical)
			uiContent[update.slot] = l.itemInstance(stack, stackID)
		}
	}
	viewer.stackMu.Unlock()

	if inventoryChanged {
		// Keep Bedrock's native inventory containers intact. Crafting screen slots
		// are sent last so UI content cannot overwrite the authoritative result.
		_ = viewer.conn.WritePacket(&packet.InventoryContent{
			WindowID: protocol.WindowIDInventory,
			Content:  content,
			Container: protocol.FullContainerName{
				ContainerID: protocol.ContainerInventory,
			},
		})
		// WindowIDUI: 54 slots matching Dragonfly's UI inventory.
		_ = viewer.conn.WritePacket(&packet.InventoryContent{WindowID: protocol.WindowIDUI, Content: uiContent})
		_ = viewer.conn.WritePacket(&packet.InventoryContent{
			WindowID: protocol.WindowIDOffHand, Content: offhand,
			Container: protocol.FullContainerName{ContainerID: protocol.ContainerOffhand},
		})
		_ = viewer.conn.WritePacket(&packet.InventoryContent{
			WindowID: protocol.WindowIDArmour, Content: armour,
			Container: protocol.FullContainerName{ContainerID: protocol.ContainerArmor},
		})
		if playerScreen || p.OpenContainerKind == "minecraft:crafting_table" {
			l.sendPersonalCraftingSlots(viewer.conn, viewer, p)
		}
	}
	if sendInitialSelection {
		// Bootstrap the persisted server selection only when the client has not
		// already supplied a newer one during login. Runtime corrections use this
		// dedicated selection packet too; MobEquipment is for held-item visibility.
		slog.Debug("bedrock hotbar selection bootstrap sent",
			"packet_type", "InventorySync", "incoming_slot", "none", "current_server_slot", p.HeldSlot,
			"outgoing_slot", p.HeldSlot, "outgoing_packet", "PlayerHotBar")
		_ = viewer.conn.WritePacket(&packet.PlayerHotBar{
			SelectedHotBarSlot: uint32(p.HeldSlot),
			WindowID:           protocol.WindowIDInventory,
			SelectHotBarSlot:   true,
		})
	}

	viewer.inventorySent = true
	viewer.lastInventory = p.Inventory
	viewer.lastHeldSlot = p.HeldSlot
	viewer.lastHeldItem = heldItem
	viewer.lastCarriedItem = p.CarriedItem
}

// shouldBootstrapHotbarSelection reports whether the server still owns the
// initial selected-slot bootstrap. Once the client has sent any selection, or
// the first inventory snapshot was sent, runtime selection remains client-owned.
func shouldBootstrapHotbarSelection(inventorySent, clientHeldSlotSeen bool) bool {
	return !inventorySent && !clientHeldSlotSeen
}

func bedrockInventoryCanonicalSlot(slot int) int {
	if slot < 0 || slot >= 36 {
		return -1
	}
	if slot < 9 {
		return player.HotbarStart + slot
	}
	return slot
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

// BroadcastVillagerUnhappy plays Bedrock's villager rejection sound for the
// same baby/unemployed/nitwit interaction handled by the canonical server.
func (l *Listener) BroadcastVillagerUnhappy(entity *corentity.Entity) {
	if l == nil || entity == nil {
		return
	}
	event := &packet.LevelSoundEvent{
		SoundType:      packet.SoundEventHaggleNo,
		Position:       vec32(entity.Position),
		ExtraData:      -1,
		EntityType:     "minecraft:villager_v2",
		BabyMob:        entity.IsBaby,
		EntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
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

// BroadcastSculkSensorSound mirrors the sensor phase sound to Bedrock clients.
func (l *Listener) BroadcastSculkSensorSound(position spatial.Vec3, active bool) {
	if l == nil {
		return
	}
	sound := packet.SoundEventSculkSensorPowerOff
	if active {
		sound = packet.SoundEventSculkSensorPowerOn
	}
	event := &packet.LevelSoundEvent{SoundType: sound, Position: vec32(position), ExtraData: -1}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		sessions = append(sessions, current)
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(event)
	}
}

// BroadcastWindChargeSound sends the native Bedrock throw/burst event.
func (l *Listener) BroadcastWindChargeSound(position spatial.Vec3, burst bool) {
	if l == nil {
		return
	}
	sound := packet.SoundEventThrow
	if burst {
		sound = packet.SoundEventWindChargeBurst
	}
	event := &packet.LevelSoundEvent{SoundType: sound, Position: vec32(position), ExtraData: -1}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		sessions = append(sessions, current)
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(event)
	}
}

// BroadcastExperienceOrbPickup sends Bedrock's native pickup sound to viewers
// in the dimension where the canonical orb was collected.
func (l *Listener) BroadcastExperienceOrbPickup(dimension int32, position spatial.Vec3) {
	if l == nil {
		return
	}
	event := &packet.LevelEvent{EventType: packet.LevelEventSoundExperienceOrbPickup, Position: vec32(position)}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		if current.dimension.Load() == dimension {
			sessions = append(sessions, current)
		}
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(event)
	}
}

// OpenVillagerTrade opens Bedrock's native trading screen and publishes the
// same canonical offer list used by Java Edition.
func (l *Listener) OpenVillagerTrade(playerUUID [16]byte, entity *corentity.Entity) bool {
	if l == nil || entity == nil || entity.Type != corentity.TypeVillager || !entity.CanTradeAsVillager() {
		return false
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[playerUUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return false
	}
	p := l.game.GetPlayer(playerUUID)
	if p == nil {
		return false
	}
	level := max(entity.VillagerLevel, 1)
	offers := handler.VillagerTrades(entity.VillagerProfession, level)
	serialised, err := bedrockVillagerOffersNBT(offers)
	if err != nil {
		slog.Warn("bedrock: encode villager offers", "err", err)
		return false
	}
	_ = viewer.conn.WritePacket(&packet.ContainerOpen{
		WindowID:                1,
		ContainerType:           protocol.ContainerTypeTrade,
		ContainerPosition:       protocol.BlockPos{int32(entity.Position.X), int32(entity.Position.Y), int32(entity.Position.Z)},
		ContainerEntityUniqueID: int64(bedrockRemoteRuntimeID(entity.EntityID)),
	})
	_ = viewer.conn.WritePacket(&packet.UpdateTrade{
		WindowID:          1,
		WindowType:        protocol.ContainerTypeTrade,
		Size:              int32(len(offers)),
		TradeTier:         level - 1,
		VillagerUniqueID:  int64(bedrockRemoteRuntimeID(entity.EntityID)),
		EntityUniqueID:    int64(bedrockSelfRuntimeID),
		DisplayName:       bedrockVillagerTradeTitle(entity.VillagerProfession, level),
		NewTradeUI:        true,
		DemandBasedPrices: true,
		SerialisedOffers:  serialised,
	})
	p.OpenContainerID = 1
	p.OpenContainerKind = "minecraft:villager"
	p.ContainerSlots = make([]player.ItemStack, 3)
	return true
}

func bedrockVillagerOffersNBT(offers []handler.VillagerTrade) ([]byte, error) {
	recipes := make([]map[string]any, 0, len(offers))
	for _, offer := range offers {
		buyA, ok := bedrockTradeItemNBT(offer.Input1, true)
		if !ok {
			continue
		}
		sell, ok := bedrockTradeItemNBT(offer.Output, false)
		if !ok {
			continue
		}
		priceMultiplier := offer.PriceMultiplier
		if priceMultiplier == 0 {
			priceMultiplier = 0.05
		}
		recipe := map[string]any{
			"buyA": buyA, "sell": sell,
			"buyCountA": int32(offer.Input1.Count), "buyCountB": int32(0),
			"uses": int32(0), "maxUses": offer.MaxUses,
			"rewardExp": byte(1), "traderExp": offer.XP,
			"priceMultiplierA": priceMultiplier, "priceMultiplierB": float32(0),
			"demand": int32(0), "tier": offer.Tier,
		}
		if !offer.Input2.IsEmpty() {
			if buyB, present := bedrockTradeItemNBT(offer.Input2, true); present {
				recipe["buyB"] = buyB
				recipe["buyCountB"] = int32(offer.Input2.Count)
			}
		}
		recipes = append(recipes, recipe)
	}
	return nbt.Marshal(struct {
		Recipes             []map[string]any   `nbt:"Recipes"`
		TierExpRequirements []map[string]int32 `nbt:"TierExpRequirements"`
	}{Recipes: recipes, TierExpRequirements: []map[string]int32{
		{"0": 0}, {"1": 10}, {"2": 70}, {"3": 150}, {"4": 250},
	}})
}

func bedrockTradeItemNBT(stack player.ItemStack, cost bool) (map[string]any, bool) {
	name, metadata, ok := bedrockItemIdentity(stack.ItemID)
	if !ok || stack.IsEmpty() || stack.Count > 127 {
		return nil, false
	}
	damage := int16(metadata)
	if cost && damage == 0 {
		// Bedrock vanilla uses the wildcard damage value for ingredients that
		// do not require a particular legacy metadata variant.
		damage = 32767
	}
	return map[string]any{
		"Count":       byte(stack.Count),
		"Damage":      damage,
		"Name":        name,
		"WasPickedUp": byte(0),
	}, true
}

func bedrockVillagerTradeTitle(profession corentity.VillagerProfession, level int32) string {
	name := strings.TrimPrefix(string(profession), "minecraft:")
	if name == "" || name == "none" {
		name = "villager"
	}
	name = strings.ToUpper(name[:1]) + strings.ReplaceAll(name[1:], "_", " ")
	rank := [...]string{"Novice", "Apprentice", "Journeyman", "Expert", "Master"}
	if level < 1 || level > 5 {
		level = 1
	}
	return name + " - " + rank[level-1]
}

// BroadcastActorEvent mirrors canonical animal feedback (feeding, hearts and
// taming result) to every Bedrock viewer.
func (l *Listener) BroadcastActorEvent(entityID int32, eventType byte) {
	if l == nil || entityID == 0 {
		return
	}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		sessions = append(sessions, current)
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(&packet.ActorEvent{
			EntityRuntimeID: bedrockRemoteRuntimeID(entityID),
			EventType:       eventType,
		})
	}
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

// SetDifficulty updates future joins and every connected Bedrock client.
func (l *Listener) SetDifficulty(difficulty int32) {
	l.difficulty = difficulty
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		sessions = append(sessions, current)
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(&packet.SetDifficulty{Difficulty: uint32(difficulty)})
	}
}

func (l *Listener) SetDefaultGameMode(mode player.GameMode) {
	l.gameMode.Store(uint32(mode))
}

// SetWeather publishes vanilla rain level events to every Bedrock session.
func (l *Listener) SetWeather(raining, thundering bool) {
	state := uint32(0)
	if raining {
		state = 1
	}
	if thundering {
		state = 2
	}
	l.weather.Store(state)
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		sessions = append(sessions, current)
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		l.sendWeather(current, raining, thundering)
	}
}

func (l *Listener) sendWeather(current *bedrockSession, raining, thundering bool) {
	event, data := packet.LevelEventStopRaining, int32(0)
	if raining {
		event, data = packet.LevelEventStartRaining, 65535
	}
	_ = current.conn.WritePacket(&packet.LevelEvent{EventType: int32(event), EventData: data})
	thunderEvent, thunderData := packet.LevelEventStopThunderstorm, int32(0)
	if thundering {
		thunderEvent = packet.LevelEventStartThunderstorm
		thunderData = 65535
	}
	_ = current.conn.WritePacket(&packet.LevelEvent{EventType: int32(thunderEvent), EventData: thunderData})
}

// OpenContainerBlock sends a ContainerOpen packet to the player for the given
// block position. Returns true if the block is a supported interactive container,
// false if it is not (so the caller can fall through to block placement logic).
func (l *Listener) OpenContainerBlock(playerUUID [16]byte, x, y, z int32, blockName string) bool {
	containerType, ok := bedrockContainerType(blockName)
	if !ok {
		return false
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[playerUUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return false
	}
	if handler.IsFurnaceContainer(blockName) {
		viewer.stackMu.Lock()
		viewer.furnaceSent = false
		viewer.lastFurnaceKind = ""
		viewer.stackMu.Unlock()
	}
	viewer.stackMu.Lock()
	clear(viewer.containerNetworkIDs[:])
	viewer.stackMu.Unlock()
	_ = viewer.conn.WritePacket(&packet.ContainerOpen{
		WindowID:                1,
		ContainerType:           containerType,
		ContainerPosition:       protocol.BlockPos{x, y, z},
		ContainerEntityUniqueID: -1,
	})
	return true
}

// SyncGenericContainer sends the contents of a chest-like block using the
// LevelEntity container used by Bedrock stack requests.
func (l *Listener) SyncGenericContainer(p *player.Player) {
	if p == nil || len(p.ContainerSlots) == 0 || len(p.ContainerSlots) > 54 {
		return
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return
	}

	viewer.stackMu.Lock()
	content := make([]protocol.ItemInstance, len(p.ContainerSlots))
	for slot, stack := range p.ContainerSlots {
		if stack.IsEmpty() {
			viewer.containerNetworkIDs[slot] = 0
		} else if viewer.containerNetworkIDs[slot] == 0 {
			viewer.containerNetworkIDs[slot] = viewer.allocateStackNetworkID()
		}
		content[slot] = l.itemInstance(stack, viewer.containerNetworkIDs[slot])
	}
	viewer.stackMu.Unlock()

	containerID := byte(protocol.ContainerLevelEntity)
	if p.OpenContainerKind == "minecraft:crafter" {
		containerID = protocol.ContainerCrafterLevelEntity
	}
	_ = viewer.conn.WritePacket(&packet.InventoryContent{
		WindowID: 1,
		Content:  content,
		Container: protocol.FullContainerName{
			ContainerID: containerID,
		},
	})
}

type bedrockWorkstationSlot struct {
	containerID byte
	slot        uint32
	index       int
}

// SyncWorkstationContainer publishes each workstation slot through the
// protocol-specific container IDs used by Bedrock stack requests. Without
// these initial slots, the UI opens visually but rejects every item move.
func (l *Listener) SyncWorkstationContainer(p *player.Player) {
	if p == nil {
		return
	}
	descriptors := bedrockWorkstationSlots(p.OpenContainerKind)
	if len(descriptors) == 0 || len(p.ContainerSlots) == 0 {
		return
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return
	}

	viewer.stackMu.Lock()
	packets := make([]*packet.InventorySlot, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.index < 0 || descriptor.index >= len(p.ContainerSlots) {
			continue
		}
		stack := p.ContainerSlots[descriptor.index]
		if stack.IsEmpty() {
			viewer.containerNetworkIDs[descriptor.index] = 0
		} else if viewer.containerNetworkIDs[descriptor.index] == 0 {
			viewer.containerNetworkIDs[descriptor.index] = viewer.allocateStackNetworkID()
		}
		packets = append(packets, &packet.InventorySlot{
			WindowID: 1,
			Slot:     descriptor.slot,
			Container: protocol.Option(protocol.FullContainerName{
				ContainerID: descriptor.containerID,
			}),
			NewItem: l.itemInstance(stack, viewer.containerNetworkIDs[descriptor.index]),
		})
	}
	viewer.stackMu.Unlock()
	for _, pk := range packets {
		_ = viewer.conn.WritePacket(pk)
	}
}

func bedrockWorkstationSlots(kind string) []bedrockWorkstationSlot {
	slot := func(containerID byte, index int) bedrockWorkstationSlot {
		return bedrockWorkstationSlot{containerID: containerID, index: index}
	}
	switch kind {
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil":
		return []bedrockWorkstationSlot{slot(protocol.ContainerAnvilInput, 0), slot(protocol.ContainerAnvilMaterial, 1), slot(protocol.ContainerAnvilResultPreview, 2)}
	case "minecraft:enchanting_table":
		return []bedrockWorkstationSlot{slot(protocol.ContainerEnchantingInput, 0), slot(protocol.ContainerEnchantingMaterial, 1)}
	case "minecraft:grindstone":
		return []bedrockWorkstationSlot{slot(protocol.ContainerGrindstoneInput, 0), slot(protocol.ContainerGrindstoneAdditional, 1), slot(protocol.ContainerGrindstoneResultPreview, 2)}
	case "minecraft:loom":
		return []bedrockWorkstationSlot{slot(protocol.ContainerLoomInput, 0), slot(protocol.ContainerLoomDye, 1), slot(protocol.ContainerLoomMaterial, 2), slot(protocol.ContainerLoomResultPreview, 3)}
	case "minecraft:smithing_table":
		return []bedrockWorkstationSlot{slot(protocol.ContainerSmithingTableTemplate, 0), slot(protocol.ContainerSmithingTableInput, 1), slot(protocol.ContainerSmithingTableMaterial, 2), slot(protocol.ContainerSmithingTableResultPreview, 3)}
	case "minecraft:stonecutter":
		return []bedrockWorkstationSlot{slot(protocol.ContainerStonecutterInput, 0), slot(protocol.ContainerStonecutterResultPreview, 1)}
	case "minecraft:cartography_table":
		return []bedrockWorkstationSlot{slot(protocol.ContainerCartographyInput, 0), slot(protocol.ContainerCartographyAdditional, 1), slot(protocol.ContainerCartographyResultPreview, 2)}
	case "minecraft:brewing_stand":
		return []bedrockWorkstationSlot{
			{containerID: protocol.ContainerBrewingStandResult, slot: 0, index: 0},
			{containerID: protocol.ContainerBrewingStandResult, slot: 1, index: 1},
			{containerID: protocol.ContainerBrewingStandResult, slot: 2, index: 2},
			slot(protocol.ContainerBrewingStandInput, 3), slot(protocol.ContainerBrewingStandFuel, 4),
		}
	case "minecraft:beacon":
		return []bedrockWorkstationSlot{slot(protocol.ContainerBeaconPayment, 0)}
	default:
		return nil
	}
}

// SyncFurnaceContainer publishes the authoritative three furnace slots and
// progress properties to the player that currently has the block open.
func (l *Listener) SyncFurnaceContainer(p *player.Player, cookTime, burnTime, burnDuration, cookDuration int) {
	if p == nil || !handler.IsFurnaceContainer(p.OpenContainerKind) {
		return
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return
	}

	var slots [3]player.ItemStack
	copy(slots[:], p.ContainerSlots)
	properties := [4]int32{int32(cookTime), int32(burnTime), int32(burnDuration), int32(cookDuration)}

	viewer.stackMu.Lock()
	changed := !viewer.furnaceSent || viewer.lastFurnaceKind != p.OpenContainerKind ||
		viewer.lastFurnaceSlots != slots || viewer.lastFurnaceData != properties
	if !changed {
		viewer.stackMu.Unlock()
		return
	}
	for index, stack := range slots {
		previous := viewer.lastFurnaceSlots[index]
		if stack.IsEmpty() {
			viewer.furnaceNetworkIDs[index] = 0
		} else if !viewer.furnaceSent || viewer.furnaceNetworkIDs[index] == 0 ||
			(!previous.IsEmpty() && !previous.SameItem(stack)) {
			viewer.furnaceNetworkIDs[index] = viewer.allocateStackNetworkID()
		}
	}
	instances := [3]protocol.ItemInstance{
		l.itemInstance(slots[0], viewer.furnaceNetworkIDs[0]),
		l.itemInstance(slots[1], viewer.furnaceNetworkIDs[1]),
		l.itemInstance(slots[2], viewer.furnaceNetworkIDs[2]),
	}
	viewer.lastFurnaceSlots = slots
	viewer.lastFurnaceData = properties
	viewer.lastFurnaceKind = p.OpenContainerKind
	viewer.furnaceSent = true
	viewer.stackMu.Unlock()

	ingredientContainer := byte(protocol.ContainerFurnaceIngredient)
	switch strings.TrimPrefix(p.OpenContainerKind, "minecraft:") {
	case "blast_furnace", "lit_blast_furnace":
		ingredientContainer = protocol.ContainerBlastFurnaceIngredient
	case "smoker", "lit_smoker":
		ingredientContainer = protocol.ContainerSmokerIngredient
	}
	containers := [3]byte{ingredientContainer, protocol.ContainerFurnaceFuel, protocol.ContainerFurnaceResult}
	for index := range instances {
		_ = viewer.conn.WritePacket(&packet.InventorySlot{
			WindowID: 1,
			Slot:     uint32(index),
			Container: protocol.Option(protocol.FullContainerName{
				ContainerID: containers[index],
			}),
			NewItem: instances[index],
		})
	}
	// Bedrock exposes only cooking progress, remaining burn time, and total
	// burn duration. Key 3 is reserved, unlike Java's four-property delegate.
	for key, value := range properties[:3] {
		_ = viewer.conn.WritePacket(&packet.ContainerSetData{WindowID: 1, Key: int32(key), Value: value})
	}
}

// SyncBrewingContainer sends updated brewing stand slot contents and the two
// progress data values (key 0 = brew_time 400→0, key 1 = fuel_amount 0-20).
func (l *Listener) SyncBrewingContainer(p *player.Player, brewTime, fuelAmount int) {
	if p == nil || p.OpenContainerKind != "minecraft:brewing_stand" {
		return
	}
	l.SyncWorkstationContainer(p)
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer == nil {
		return
	}
	_ = viewer.conn.WritePacket(&packet.ContainerSetData{WindowID: 1, Key: 0, Value: int32(brewTime)})
	_ = viewer.conn.WritePacket(&packet.ContainerSetData{WindowID: 1, Key: 1, Value: int32(fuelAmount)})
}

// bedrockContainerType maps a block resource location to the Bedrock protocol
// container type used in the ContainerOpen packet. Returns false if the block
// is not an interactive container.
func bedrockContainerType(blockName string) (byte, bool) {
	switch blockName {
	case "minecraft:crafting_table":
		return protocol.ContainerTypeWorkbench, true
	case "minecraft:furnace", "minecraft:lit_furnace":
		return protocol.ContainerTypeFurnace, true
	case "minecraft:blast_furnace", "minecraft:lit_blast_furnace":
		return protocol.ContainerTypeBlastFurnace, true
	case "minecraft:smoker", "minecraft:lit_smoker":
		return protocol.ContainerTypeSmoker, true
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil":
		return protocol.ContainerTypeAnvil, true
	case "minecraft:enchanting_table":
		return protocol.ContainerTypeEnchantment, true
	case "minecraft:grindstone":
		return protocol.ContainerTypeGrindstone, true
	case "minecraft:loom":
		return protocol.ContainerTypeLoom, true
	case "minecraft:smithing_table":
		return protocol.ContainerTypeSmithingTable, true
	case "minecraft:stonecutter":
		return protocol.ContainerTypeStonecutter, true
	case "minecraft:brewing_stand":
		return protocol.ContainerTypeBrewingStand, true
	case "minecraft:cartography_table":
		return protocol.ContainerTypeCartography, true
	case "minecraft:beacon":
		return protocol.ContainerTypeBeacon, true
	case "minecraft:chest", "minecraft:trapped_chest", "minecraft:barrel", "minecraft:ender_chest":
		return protocol.ContainerTypeContainer, true
	case "minecraft:hopper":
		return protocol.ContainerTypeHopper, true
	case "minecraft:dispenser":
		return protocol.ContainerTypeDispenser, true
	case "minecraft:dropper":
		return protocol.ContainerTypeDropper, true
	case "minecraft:crafter":
		return protocol.ContainerTypeCrafter, true
	}
	return 0, false
}

// BroadcastBlockChange sends one canonical mutation to every Bedrock viewer.
func (l *Listener) BroadcastBlockChange(change coreworld.BlockChange) {
	l.broadcastDimensionBlockChange(packet.DimensionOverworld, change)
}

// SetWorldSpawn updates new joins and every connected Bedrock compass target.
func (l *Listener) SetWorldSpawn(position spatial.Vec3) {
	blockPosition := protocol.BlockPos{
		int32(math.Floor(position.X)), int32(math.Floor(position.Y)), int32(math.Floor(position.Z)),
	}
	l.spawnMu.Lock()
	l.spawnX, l.spawnY, l.spawnZ = int(blockPosition[0]), int(blockPosition[1]), int(blockPosition[2])
	l.spawnMu.Unlock()
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		sessions = append(sessions, current)
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(&packet.SetSpawnPosition{
			SpawnType: packet.SpawnTypeWorld, Position: blockPosition,
			Dimension: packet.DimensionOverworld, SpawnPosition: blockPosition,
		})
	}
}

// DimensionBlockObserver returns a world observer scoped to one Bedrock
// dimension, preventing updates at identical coordinates leaking to viewers in
// another dimension.
func (l *Listener) DimensionBlockObserver(dimension int32) func(coreworld.BlockChange) {
	return func(change coreworld.BlockChange) {
		l.broadcastDimensionBlockChange(dimension, change)
	}
}

func (l *Listener) broadcastDimensionBlockChange(dimension int32, change coreworld.BlockChange) {
	networkID := l.encoder.BlockNetworkID(change.Block)
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, session := range l.sessions {
		sessions = append(sessions, session)
	}
	l.sessionsMu.RUnlock()
	for _, session := range sessions {
		if session.dimension.Load() != dimension {
			continue
		}
		_ = session.conn.WritePacket(&packet.UpdateBlock{
			Position:          protocol.BlockPos{int32(change.X), int32(change.Y), int32(change.Z)},
			NewBlockRuntimeID: networkID,
			Flags:             packet.BlockUpdateNetwork | packet.BlockUpdateNeighbours,
		})
	}
}

// ChangeDimension switches a connected Bedrock player and immediately seeds
// the destination view so the loading screen can complete.
func (l *Listener) ChangeDimension(p *player.Player, dimension int32, position spatial.Vec3) {
	l.changeDimension(p, dimension, position, false)
}

// ChangeDimensionForRespawn performs the same world switch while marking it
// as death-driven, which current Bedrock clients require when respawning from
// the Nether or End into the Overworld.
func (l *Listener) ChangeDimensionForRespawn(p *player.Player, dimension int32, position spatial.Vec3) {
	l.changeDimension(p, dimension, position, true)
}

// SendPlayerMobEffect applies a client-visible effect to one Bedrock player.
func (l *Listener) SendPlayerMobEffect(p *player.Player, effectType, amplifier, duration int32) {
	if l == nil || p == nil {
		return
	}
	l.sessionsMu.RLock()
	session := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if session == nil {
		return
	}
	_ = session.conn.WritePacket(&packet.MobEffect{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Operation:       packet.MobEffectAdd,
		EffectType:      effectType,
		Amplifier:       amplifier,
		Particles:       true,
		Duration:        duration,
		Tick:            uint64(time.Now().UnixMilli() / 50),
	})
}

// RemovePlayerMobEffect removes one expired canonical effect from a Bedrock client.
func (l *Listener) RemovePlayerMobEffect(p *player.Player, effectType int32) {
	if l == nil || p == nil || effectType == 0 {
		return
	}
	l.sessionsMu.RLock()
	session := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if session == nil {
		return
	}
	_ = session.conn.WritePacket(&packet.MobEffect{
		EntityRuntimeID: bedrockSelfRuntimeID,
		Operation:       packet.MobEffectRemove,
		EffectType:      effectType,
		Tick:            uint64(time.Now().UnixMilli() / 50),
	})
}

func (l *Listener) changeDimension(p *player.Player, dimension int32, position spatial.Vec3, respawn bool) {
	if p == nil {
		return
	}
	l.sessionsMu.RLock()
	viewer := l.sessions[p.UUID]
	l.sessionsMu.RUnlock()
	if viewer == nil || (!respawn && viewer.dimension.Load() == dimension) {
		return
	}
	viewer.dimension.Store(dimension)
	viewer.expectTeleport(position)
	screenID := l.screenID.Add(1)
	_ = viewer.conn.WritePacket(&packet.ChangeDimension{
		Dimension:       dimension,
		Position:        playerNetworkPosition(position),
		Respawn:         respawn,
		LoadingScreenID: protocol.Option(screenID),
	})
	_ = viewer.conn.WritePacket(initialChunkPublisher(position, bedrockChunkRadius))
	cx, cz := chunkCoordinate(position.X), chunkCoordinate(position.Z)
	// The destination centre must arrive before DimensionChangeDone. Sending
	// that acknowledgement first allowed the client to leave its loading
	// screen with no terrain and render an indefinitely black world.
	if err := l.sendInitialChunks(viewer.conn, cx, cz, 0, dimension); err != nil {
		slog.Debug("bedrock: destination centre chunk failed", "dimension", dimension, "err", err)
		return
	}
	_ = viewer.conn.WritePacket(&packet.PlayStatus{Status: packet.PlayStatusPlayerSpawn})
	_ = viewer.conn.WritePacket(&packet.PlayerAction{
		EntityRuntimeID: bedrockSelfRuntimeID,
		ActionType:      protocol.PlayerActionDimensionChangeDone,
	})
	go func() {
		if err := l.sendSurroundingChunks(viewer.conn, cx, cz, bedrockChunkRadius, dimension); err != nil {
			slog.Debug("bedrock: dimension chunk stream failed", "dimension", dimension, "err", err)
		}
	}()
}

// BroadcastBlockBreakEffect emits Bedrock's combined destroy particles and
// block-specific break sound. UpdateBlock alone changes the world silently.
func (l *Listener) BroadcastBlockBreakEffect(position spatial.BlockPos, block coreworld.Block) {
	l.BroadcastDimensionBlockBreakEffect(packet.DimensionOverworld, position, block)
}

// BroadcastDimensionBlockBreakEffect scopes break particles and sounds to
// viewers of the world where the block was actually broken.
func (l *Listener) BroadcastDimensionBlockBreakEffect(dimension int32, position spatial.BlockPos, block coreworld.Block) {
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
		if session.dimension.Load() != dimension {
			continue
		}
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
	case corentity.TypeVillager:
		return "minecraft:villager_v2"
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
	case corentity.TypeWindCharge:
		return "minecraft:wind_charge_projectile"
	case corentity.TypeExperienceBottle:
		return "minecraft:xp_bottle"
	case corentity.TypePotion:
		return "minecraft:thrown_potion"
	case corentity.TypeBambooRaft:
		return "minecraft:bamboo_raft"
	case corentity.TypeBambooChestRaft:
		return "minecraft:bamboo_chest_raft"
	case corentity.TypeFireworkRocket:
		return "minecraft:fireworks_rocket"
	default:
		return string(entityType)
	}
}

func bedrockVillagerProfessionID(profession corentity.VillagerProfession) int32 {
	switch profession {
	case corentity.VillagerProfessionFarmer:
		return 1
	case corentity.VillagerProfessionFisherman:
		return 2
	case corentity.VillagerProfessionShepherd:
		return 3
	case corentity.VillagerProfessionFletcher:
		return 4
	case corentity.VillagerProfessionLibrarian:
		return 5
	case corentity.VillagerProfessionCartographer:
		return 6
	case corentity.VillagerProfessionCleric:
		return 7
	case corentity.VillagerProfessionArmorer:
		return 8
	case corentity.VillagerProfessionWeaponsmith:
		return 9
	case corentity.VillagerProfessionToolsmith:
		return 10
	case corentity.VillagerProfessionButcher:
		return 11
	case corentity.VillagerProfessionLeatherworker:
		return 12
	case corentity.VillagerProfessionMason:
		return 13
	case corentity.VillagerProfessionNitwit:
		return 14
	default:
		return 0
	}
}

func bedrockVillagerVariantID(variant corentity.VillagerVariant) int32 {
	switch variant {
	case corentity.VillagerVariantDesert:
		return 1
	case corentity.VillagerVariantJungle:
		return 2
	case corentity.VillagerVariantSavanna:
		return 3
	case corentity.VillagerVariantSnow:
		return 4
	case corentity.VillagerVariantSwamp:
		return 5
	case corentity.VillagerVariantTaiga:
		return 6
	default:
		return 0
	}
}

// actorIdentifierEntry matches Dragonfly's private actorIdentifier struct
// in server/session/session.go — same field name, same NBT tag.
type actorIdentifierEntry struct {
	ID string `nbt:"id"`
}

// dragonflyDefaultActorIdentifiers is Dragonfly's DefaultRegistry entity list
// (server/entity/register.go) in registration order, using each type's
// EncodeEntity() string exactly as Dragonfly's sendAvailableEntities() would.
// Order is not guaranteed by Dragonfly (map iteration), but the set is fixed.
var dragonflyDefaultActorIdentifiers = []actorIdentifierEntry{
	{ID: "minecraft:area_effect_cloud"},
	{ID: "minecraft:arrow"},
	{ID: "minecraft:xp_bottle"},
	{ID: "minecraft:egg"},
	{ID: "minecraft:ender_pearl"},
	{ID: "minecraft:xp_orb"},
	{ID: "minecraft:falling_block"},
	{ID: "minecraft:fireworks_rocket"},
	{ID: "minecraft:item"},
	{ID: "minecraft:lightning_bolt"},
	{ID: "minecraft:lingering_potion"},
	{ID: "minecraft:snowball"},
	{ID: "minecraft:wind_charge_projectile"},
	{ID: "minecraft:splash_potion"},
	{ID: "minecraft:tnt"},
	{ID: "dragonfly:text"},
}

// availableActorIdentifiersPayload is the pre-marshalled NBT payload for the
// AvailableActorIdentifiers packet. It exactly replicates what Dragonfly's
// sendAvailableEntities() produces: nbt.Marshal(map[string]any{"idlist": [...]})
// using Little Endian encoding (gophertunnel default).
var availableActorIdentifiersPayload []byte

func init() {
	var err error
	availableActorIdentifiersPayload, err = nbt.Marshal(map[string]any{
		"idlist": dragonflyDefaultActorIdentifiers,
	})
	if err != nil {
		panic("bedrock: failed to marshal AvailableActorIdentifiers payload: " + err.Error())
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
	mapping, mapped := javaToBedrockItemMappings[stack.ItemID]
	var runtimeID int32
	var metadata uint32
	lightLevel := -1
	if stack.ItemID == "minecraft:light" {
		lightLevel = min(max(stack.Damage, 0), 15)
		mapped = false
	}
	if creativeRuntimeID, ok := pumpkinInventoryRuntimeID(stack.ItemID, mapping, mapped); ok && lightLevel < 0 {
		// Prefer Pumpkin's current Bedrock palette over compatibility mappings.
		// This matters for newly added items whose generated Java fallback still
		// points at minecraft:unknown or an older substitute.
		runtimeID = creativeRuntimeID
		metadata = uint32(uint16(stack.Damage))
	} else if mapped {
		runtimeID, metadata = mapping.runtimeID, mapping.metadata
	} else {
		itemName := stack.ItemID
		if lightLevel >= 0 {
			itemName = fmt.Sprintf("minecraft:light_block_%d", lightLevel)
		}
		if creativeRuntimeID, ok := pumpkinCreativeRuntimeID(itemName); ok {
			runtimeID = creativeRuntimeID
			metadata = uint32(uint16(stack.Damage))
		} else {
			var meta int16
			var ok bool
			runtimeID, meta, ok = dfworld.ItemRuntimeID(namedItem{name: itemName})
			if !ok {
				return protocol.ItemInstance{}
			}
			metadata = uint32(uint16(meta))
		}
	}
	if potionID, ok := bedrockPotionID(stack); ok {
		metadata = uint32(uint16(potionID))
	}
	if stewVariant, ok := bedrockStewVariant(stack); ok {
		metadata = uint32(uint16(stewVariant))
	}
	nbtData := map[string]any{}
	if stack.Damage > 0 && lightLevel < 0 {
		nbtData["Damage"] = int32(stack.Damage)
	}
	if enchantments := bedrockEnchantments(stack); len(enchantments) > 0 {
		nbtData["ench"] = enchantments
	}
	if stack.ItemID == "minecraft:decorated_pot" {
		decorations := stack.NormalizedPotDecorations()
		sherds := make([]any, 0, len(decorations))
		for _, decoration := range decorations {
			sherds = append(sherds, decoration)
		}
		nbtData["id"] = "DecoratedPot"
		nbtData["sherds"] = sherds
	}
	if stack.ItemID == "minecraft:firework_rocket" {
		for key, value := range bedrockFireworkNBT(stack.EffectiveFireworks()) {
			nbtData[key] = value
		}
	}
	if stack.Components != "" {
		nbtData[goCraftComponentsNBTKey] = stack.NormalizedComponents()
	}
	if len(nbtData) == 0 {
		nbtData = nil
	}
	blockRuntimeID := mapping.blockRuntimeID
	block := splitBlockName(stack.ItemID)
	if lightLevel >= 0 {
		block.Properties = map[string]string{"level": strconv.Itoa(lightLevel)}
	}
	if networkID := l.encoder.BlockNetworkID(block); blockRuntimeID == 0 && networkID != l.encoder.BlockNetworkID(coreworld.Air) {
		blockRuntimeID = int32(networkID)
	}
	return protocol.ItemInstance{
		StackNetworkID: stackNetworkID,
		Stack: protocol.ItemStack{
			ItemType:       protocol.ItemType{NetworkID: runtimeID, MetadataValue: metadata},
			Count:          uint16(min(stack.Count, math.MaxUint16)),
			BlockRuntimeID: blockRuntimeID,
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
		SkinColour:                hexToRGBA(data.SkinColour),
		ArmSize:                   armSizeToUint8(data.ArmSize),
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
		ArmSize:                   protocol.ArmSizeWide,
		SkinColour:                hexToRGBA("#b37b62"),
		Trusted:                   true,
		OverrideAppearance:        true,
	}
}

// hexToRGBA converts a hex colour string (e.g. "#b37b62") to color.RGBA.
func hexToRGBA(s string) color.RGBA {
	s = strings.TrimPrefix(s, "#")
	if len(s) < 6 {
		return color.RGBA{A: 0xff}
	}
	b, err := hex.DecodeString(s[:6])
	if err != nil || len(b) < 3 {
		return color.RGBA{A: 0xff}
	}
	return color.RGBA{R: b[0], G: b[1], B: b[2], A: 0xff}
}

// armSizeToUint8 converts a login arm-size string ("slim"/"wide") to the
// protocol constant.
func armSizeToUint8(s string) uint8 {
	if s == "slim" {
		return protocol.ArmSizeSlim
	}
	return protocol.ArmSizeWide
}

// BroadcastBlockEntityData translates canonical block-entity state for
// Bedrock viewers in the affected dimension.
func (l *Listener) BroadcastBlockEntityData(dimension int32, entity coreworld.BlockEntity) {
	if l == nil {
		return
	}
	dimensionWorld := l.worldForDimension(dimension)
	if dimensionWorld == nil {
		return
	}
	block := dimensionWorld.GetBlock(entity.X, entity.Y, entity.Z)
	data := bedrockBlockEntityData(entity, block)
	if data == nil {
		return
	}
	l.sessionsMu.RLock()
	sessions := make([]*bedrockSession, 0, len(l.sessions))
	for _, current := range l.sessions {
		if current.dimension.Load() == dimension {
			sessions = append(sessions, current)
		}
	}
	l.sessionsMu.RUnlock()
	for _, current := range sessions {
		_ = current.conn.WritePacket(&packet.BlockActorData{
			Position: protocol.BlockPos{int32(entity.X), int32(entity.Y), int32(entity.Z)},
			NBTData:  data,
		})
	}
}

func bedrockBlockEntityData(entity coreworld.BlockEntity, block coreworld.Block) map[string]any {
	position := map[string]any{"x": int32(entity.X), "y": int32(entity.Y), "z": int32(entity.Z)}
	switch strings.TrimPrefix(entity.Type, "minecraft:") {
	case "decorated_pot":
		decorations := player.NormalizePotDecorations(entity.PotDecorations)
		position["id"] = "DecoratedPot"
		position["sherds"] = []string{decorations[0], decorations[1], decorations[2], decorations[3]}
	case "sign", "hanging_sign":
		position["id"] = "Sign"
		position["IsWaxed"] = uint8(0)
		position["FrontText"] = emptyBedrockSignSide()
		position["BackText"] = emptyBedrockSignSide()
	case "banner":
		position["id"] = "Banner"
		position["Base"] = bedrockBannerBase(block.ResourceLocation())
		position["Patterns"] = []any{}
		position["Type"] = int32(0)
	default:
		return nil
	}
	return position
}

func emptyBedrockSignSide() map[string]any {
	return map[string]any{
		"SignTextColor": int32(-16777216), "IgnoreLighting": uint8(0),
		"Text": "", "TextOwner": "", "PersistFormatting": uint8(1),
	}
}

func bedrockBannerBase(name string) int32 {
	colours := []string{
		"white", "orange", "magenta", "light_blue", "yellow", "lime", "pink", "gray",
		"light_gray", "cyan", "purple", "blue", "brown", "green", "red", "black",
	}
	base := strings.TrimPrefix(name, "minecraft:")
	base = strings.TrimSuffix(strings.TrimSuffix(base, "_wall_banner"), "_banner")
	for index, colour := range colours {
		if base == colour {
			return int32(index ^ 15)
		}
	}
	return 15
}
