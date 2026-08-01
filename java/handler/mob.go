package handler

// Mob (non-player entity) packet handling for Milestone 11.
//
// Builds and broadcasts the Java Edition packets needed to spawn, move, and
// despawn non-player entities.  The Spawn Entity packet (0x01) is reused for
// all entity types in 1.18+; this file handles mob-specific wiring.
//
// All packet IDs are estimates for 1.21.4 (protocol 769); verify against a
// 1.21.4 packet capture if an entity spawns incorrectly.

import (
	"math"

	corentity "GoCraft/core/entity"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

// ── Packet builders ───────────────────────────────────────────────────────────

// buildSpawnMob constructs a Spawn Entity (0x01 S→C) packet for a non-player
// entity.  Returns (packet, true) on success, or (nil, false) when the entity
// type is not in the Java registry table (unknown types must not be sent
// because the client would disconnect on an invalid registry index).
//
// Wire layout (1.21.4, same as buildSpawnPlayer):
//
//	VarInt  entity_id
//	UUID    entity_uuid
//	VarInt  entity_type   (numeric registry index)
//	Double  x, y, z
//	Angle   pitch, yaw    (Byte, 256 units = 360°)
//	Angle   head_yaw      (Byte)
//	VarInt  data          (0 for most mobs)
//	Short   velocity_x/y/z (fixed-point blocks/tick × 8000)
func buildSpawnMob(e *corentity.Entity) (*protocol.Packet, bool) {
	typeID := javaworld.EntityTypeID(string(e.Type))
	if typeID < 0 {
		return nil, false
	}
	// Velocity wire format: blocks/tick × 8000, clamped to short range.
	vx := velToShort(e.VX)
	vy := velToShort(e.VY)
	vz := velToShort(e.VZ)
	return protocol.NewBuilder(packetIDSpawnEntity).
		VarInt(e.EntityID).
		UUID(protocol.UUID(e.UUID)).
		VarInt(typeID).
		Double(e.Position.X).
		Double(e.Position.Y).
		Double(e.Position.Z).
		Byte(degToAngle(e.Pitch)).
		Byte(degToAngle(e.Yaw)).
		Byte(degToAngle(e.Yaw)). // head yaw = body yaw at spawn
		VarInt(0).               // data (type-specific; 0 = default state)
		Short(vx).Short(vy).Short(vz).
		Build(), true
}

// buildMobMetadata returns the initial tracked data required for mob variants.
// In 1.21.4 a villager's data accessor is index 18, serializer 19, followed by
// registry VarInts for type and profession and a VarInt career level.
func buildMobMetadata(e *corentity.Entity) *protocol.Packet {
	if e.Type != corentity.TypeVillager {
		return nil
	}
	level := e.VillagerLevel
	if level < 1 {
		level = 1
	} else if level > 5 {
		level = 5
	}
	return protocol.NewBuilder(packetIDSetEntityData).
		VarInt(e.EntityID).
		Byte(18).   // DATA_VILLAGER_DATA metadata index
		VarInt(19). // VILLAGER_DATA serializer ID
		VarInt(villagerVariantProtocolID(e.VillagerVariant)).
		VarInt(villagerProfessionProtocolID(e.VillagerProfession)).
		VarInt(level).
		Byte(0xff). // metadata list terminator
		Build()
}

func villagerVariantProtocolID(variant corentity.VillagerVariant) int32 {
	switch variant {
	case corentity.VillagerVariantDesert:
		return 0
	case corentity.VillagerVariantJungle:
		return 1
	case corentity.VillagerVariantSavanna:
		return 3
	case corentity.VillagerVariantSnow:
		return 4
	case corentity.VillagerVariantSwamp:
		return 5
	case corentity.VillagerVariantTaiga:
		return 6
	default:
		return 2 // minecraft:plains
	}
}

func villagerProfessionProtocolID(profession corentity.VillagerProfession) int32 {
	switch profession {
	case corentity.VillagerProfessionArmorer:
		return 1
	case corentity.VillagerProfessionButcher:
		return 2
	case corentity.VillagerProfessionCartographer:
		return 3
	case corentity.VillagerProfessionCleric:
		return 4
	case corentity.VillagerProfessionFarmer:
		return 5
	case corentity.VillagerProfessionFisherman:
		return 6
	case corentity.VillagerProfessionFletcher:
		return 7
	case corentity.VillagerProfessionLeatherworker:
		return 8
	case corentity.VillagerProfessionLibrarian:
		return 9
	case corentity.VillagerProfessionMason:
		return 10
	case corentity.VillagerProfessionNitwit:
		return 11
	case corentity.VillagerProfessionShepherd:
		return 12
	case corentity.VillagerProfessionToolsmith:
		return 13
	case corentity.VillagerProfessionWeaponsmith:
		return 14
	default:
		return 0 // minecraft:none
	}
}

// buildTeleportMob constructs a Teleport Entity packet for a non-player entity.
// Uses the same packet ID and layout as buildTeleportEntity (players).
//
// Wire layout (1.21.4 teleport_entity / entity_position_sync):
//
//	VarInt  entity_id
//	Double  x, y, z
//	Double  velocity_x, velocity_y, velocity_z
//	Float   yaw   (4-byte float, same as player teleport)
//	Float   pitch (4-byte float, same as player teleport)
//	Bool    on_ground
func buildTeleportMob(e *corentity.Entity) *protocol.Packet {
	return protocol.NewBuilder(packetIDTeleportEntity).
		VarInt(e.EntityID).
		Double(e.Position.X).
		Double(e.Position.Y).
		Double(e.Position.Z).
		Double(e.VX).
		Double(e.VY).
		Double(e.VZ).
		Float(e.Yaw).
		Float(e.Pitch).
		Int(0). // relative movement flags
		Bool(e.OnGround).
		Build()
}

// buildHurtAnimation constructs the clientbound Hurt Animation packet.
func buildHurtAnimation(entityID int32, yaw float32) *protocol.Packet {
	return protocol.NewBuilder(packetIDHurtAnimation).
		VarInt(entityID).
		Float(yaw).
		Build()
}

// buildEntityMotion sends the velocity separately from the absolute position
// synchronization. Vanilla clients use this packet for visible knockback.
func buildEntityMotion(e *corentity.Entity) *protocol.Packet {
	return protocol.NewBuilder(packetIDSetEntityMotion).
		VarInt(e.EntityID).
		Short(velToShort(e.VX)).
		Short(velToShort(e.VY)).
		Short(velToShort(e.VZ)).
		Build()
}

// velToShort converts a velocity in blocks/tick to the Minecraft short wire
// format (blocks/tick × 8000, clamped to int16 range).
func velToShort(v float64) int16 {
	s := int64(v * 8000)
	if s > 32767 {
		return 32767
	}
	if s < -32768 {
		return -32768
	}
	return int16(s)
}

// ── Session-level helpers ─────────────────────────────────────────────────────

// sendExistingMobsTo sends a Spawn Entity packet for each entity in mobs to
// the given connection.  Called during player join so newly-connected clients
// see all currently-loaded non-player entities.
func sendExistingMobsTo(conn *network.ClientConn, mobs []*corentity.Entity) {
	for _, e := range mobs {
		pkt, ok := buildSpawnMob(e)
		if !ok {
			continue // unknown entity type — skip rather than disconnect client
		}
		_ = conn.WritePacket(pkt)
		if metadata := buildMobMetadata(e); metadata != nil {
			_ = conn.WritePacket(metadata)
		}
	}
}

// sendExistingMobsInViewTo re-announces entities after their chunks have been
// delivered. This is required after long-distance teleports and also protects
// clients from discarding an entity spawn received before its chunk existed.
func sendExistingMobsInViewTo(conn *network.ClientConn, mobs []*corentity.Entity, centerX, centerZ, radius int32) {
	for _, e := range mobs {
		entityChunkX := int32(math.Floor(e.Position.X / 16))
		entityChunkZ := int32(math.Floor(e.Position.Z / 16))
		if abs32(entityChunkX-centerX) > radius+1 || abs32(entityChunkZ-centerZ) > radius+1 {
			continue
		}
		pkt, ok := buildSpawnMob(e)
		if !ok {
			continue
		}
		_ = conn.WritePacket(pkt)
		if metadata := buildMobMetadata(e); metadata != nil {
			_ = conn.WritePacket(metadata)
		}
	}
}

// BroadcastSpawnMob sends a Spawn Entity packet for e to every connected
// session.  Call this after adding the entity to the world's entity manager.
func BroadcastSpawnMob(e *corentity.Entity, mgr *session.Manager) {
	pkt, ok := buildSpawnMob(e)
	if !ok {
		return
	}
	metadata := buildMobMetadata(e)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
		if metadata != nil {
			_ = s.Conn.WritePacket(metadata)
		}
	}
}

// BroadcastEntityPosition sends a Teleport Entity packet for e to every
// connected session.  Called by the entity tick goroutine after position
// integration.
func BroadcastEntityPosition(e *corentity.Entity, mgr *session.Manager) {
	pkt := buildTeleportMob(e)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastRemoveEntity sends a Remove Entities packet for entityID to every
// connected session.  Called when an entity dies or is otherwise despawned.
func BroadcastRemoveEntity(entityID int32, mgr *session.Manager) {
	pkt := buildRemoveEntities(entityID)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastHurtAnimation sends a Hurt Animation packet for entityID to all
// connected sessions. yaw is the attack source direction in degrees.
func BroadcastHurtAnimation(entityID int32, yaw float32, mgr *session.Manager) {
	pkt := buildHurtAnimation(entityID, yaw)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// SendPlayerKnockback sends a Set Entity Motion packet addressed to playerEntityID
// to the player's own connection, applying a knockback impulse the client physics
// engine will integrate. This is the standard mechanism for server-side knockback
// delivered to the player themselves.
func SendPlayerKnockback(conn *network.ClientConn, playerEntityID int32, vx, vy, vz float64) {
	pkt := protocol.NewBuilder(packetIDSetEntityMotion).
		VarInt(playerEntityID).
		Short(velToShort(vx)).
		Short(velToShort(vy)).
		Short(velToShort(vz)).
		Build()
	_ = conn.WritePacket(pkt)
}

// DispatchWorldTime synchronizes the Java client day/night cycle with the
// simulation clock without blocking the entity tick on socket writes.
func DispatchWorldTime(age, dayTime int64, mgr *session.Manager) {
	pkt := protocol.NewBuilder(packetIDSetTime).
		Long(age).
		Long(dayTime).
		Bool(true).
		Build()
	go func() {
		for _, session := range mgr.SnapshotAll() {
			_ = session.Conn.WritePacket(pkt)
		}
	}()
}

// DispatchTickBroadcast is called at the end of each entity tick to send
// position-update and despawn packets to all connected sessions without
// blocking the tick goroutine.
//
// The caller passes:
//   - moved: entities whose position changed this tick (live pointers; read
//     here, while still inside the tick goroutine, before any race window).
//   - deadIDs: entity IDs that were removed from the world this tick.
//
// All packets are built synchronously before the goroutine is spawned.
// The goroutine therefore only reads from immutable []byte packet buffers —
// it never touches entity fields, so there is no data race with the next tick.
func DispatchTickBroadcast(moved []*corentity.Entity, hurtEntities []*corentity.Entity, deadIDs []int32, mgr *session.Manager) {
	pkts := make([]*protocol.Packet, 0, len(moved)+len(hurtEntities)+len(deadIDs))
	for _, e := range moved {
		pkts = append(pkts, buildTeleportMob(e))
	}
	for _, e := range hurtEntities {
		pkts = append(pkts, buildHurtAnimation(e.EntityID, e.Yaw), buildEntityMotion(e))
		if sound := buildEntitySound(hurtSound(e), soundCategoryNeutral, e.EntityID, 1, 1); sound != nil {
			pkts = append(pkts, sound)
		}
	}
	for _, id := range deadIDs {
		pkts = append(pkts, buildRemoveEntities(id))
	}
	if len(pkts) == 0 {
		return
	}
	// Hand off the immutable packet slice so slow clients (10 s write deadline)
	// cannot stall the simulation tick.
	go func() {
		for _, s := range mgr.SnapshotAll() {
			for _, pkt := range pkts {
				_ = s.Conn.WritePacket(pkt)
			}
		}
	}()
}
