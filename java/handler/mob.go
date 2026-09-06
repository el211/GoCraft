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
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

const (
	entityMetadataPoseIndex              byte  = 6
	livingEntityMetadataSleepingPosIndex byte  = 14
	endermanMetadataCarriedBlockIndex    byte  = 16
	metadataTypeOptionalBlockPos         int32 = 11
	metadataTypeOptionalBlockState       int32 = 15
	metadataTypePose                     int32 = 21
	entityPoseStanding                   int32 = 0
	entityPoseSleeping                   int32 = 2
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
	// For falling_block entities the data field carries the block state ID.
	data := int32(0)
	if e.Type == corentity.TypeFallingBlock {
		data = e.FallingBlockStateID
	} else if corentity.IsProjectile(e.Type) && e.Type != corentity.TypeFireworkRocket && e.OwnerEntityID != 0 {
		data = e.OwnerEntityID + 1
	}
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
		VarInt(data).
		Short(vx).Short(vy).Short(vz).
		Build(), true
}

// buildMobMetadata returns the initial tracked data required for mob variants.
// In 1.21.4 a villager's data accessor is index 18, serializer 19, followed by
// registry VarInts for type and profession and a VarInt career level.
// Pose (index 6, type 21) and the LivingEntity sleeping position (index 14,
// type 11) are included so sleeping villagers render on their assigned bed.
func buildMobMetadata(e *corentity.Entity) *protocol.Packet {
	if e.Type == corentity.TypeItem {
		if e.ItemID == "" || e.ItemCount <= 0 {
			return nil
		}
		b := protocol.NewBuilder(packetIDSetEntityData).
			VarInt(e.EntityID).
			Byte(8).  // ItemEntity DATA_ITEM metadata index
			VarInt(7) // ItemStack metadata serializer
		encodeSlot(b, player.ItemStack{
			ItemID: e.ItemID, Count: e.ItemCount, Damage: e.ItemDamage, PotDecorations: e.ItemPotDecorations,
		})
		return b.Byte(0xff).Build()
	}
	if e.Type == corentity.TypeFireworkRocket {
		b := protocol.NewBuilder(packetIDSetEntityData).
			VarInt(e.EntityID).
			Byte(8).
			VarInt(7)
		encodeSlot(b, player.ItemStack{
			ItemID: "minecraft:firework_rocket", Count: 1, HasFireworks: true, Fireworks: e.FireworkData,
		})
		return b.Byte(0xff).Build()
	}
	b := protocol.NewBuilder(packetIDSetEntityData).VarInt(e.EntityID)
	hasMetadata := false
	if e.FireTicks > 0 {
		b = b.Byte(0).VarInt(0).Byte(0x01)
		hasMetadata = true
	}
	if e.UsingItem {
		b = b.Byte(8).VarInt(0).Byte(0x01)
		hasMetadata = true
	}
	if e.Type == corentity.TypeVillager {
		b = appendLivingEntitySleepMetadata(b, e.Sleeping, e.VillageBed)
		hasMetadata = true
	}
	if e.Type == corentity.TypeVillager || corentity.IsAgeableAnimal(e.Type) {
		b = b.Byte(16).VarInt(8).Bool(e.IsBaby) // AgeableMob DATA_BABY_ID.
		hasMetadata = true
	}

	if isJavaTamableAnimal(e.Type) {
		hasMetadata = true
		flags := byte(0)
		if e.Sitting {
			flags |= 0x01
		}
		if e.Tamed {
			flags |= 0x04
		}
		b = b.Byte(17).VarInt(0).Byte(flags)
		b = b.Byte(18).VarInt(13).Bool(e.HasTameOwner)
		if e.HasTameOwner {
			b = b.UUID(protocol.UUID(e.TameOwnerUUID))
		}
	}
	if isJavaAbstractHorse(e.Type) {
		hasMetadata = true
		flags := byte(0)
		if e.Tamed {
			flags |= 0x02
		}
		if e.Saddled {
			flags |= 0x04
		}
		b = b.Byte(17).VarInt(0).Byte(flags)
		// AbstractHorse does NOT expose an owner-UUID tracked datum in protocol 769.
		// Ownership is server-side state only; the client learns "is tamed" from the
		// 0x02 flag above. Subtype-specific index-18 fields (Horse variant VarInt,
		// Donkey/Mule has_chest Boolean, Camel is_dashing Boolean) all default to
		// 0/false and need not be sent. SkeletonHorse and ZombieHorse have no index-18
		// field at all. Sending serializer-type 13 (Optional UUID) here was producing
		// a "Network Protocol Error" disconnect on every Java 1.21.4 client.
	}
	if e.Type == corentity.TypePig {
		b = b.Byte(17).VarInt(8).Bool(e.Saddled)
		hasMetadata = true
	}
	if e.Type == corentity.TypeOcelot {
		b = b.Byte(17).VarInt(8).Bool(e.Trusting)
		hasMetadata = true
	}
	if e.Type == corentity.TypeStrider {
		b = b.Byte(19).VarInt(8).Bool(e.Saddled)
		hasMetadata = true
	}
	if e.Type == corentity.TypePufferfish {
		b = b.Byte(17).VarInt(1).VarInt(e.PufferState)
		hasMetadata = true
	}
	if e.Type == corentity.TypeEnderman && e.EndermanCarriedBlock != "" {
		b = b.Byte(endermanMetadataCarriedBlockIndex).
			VarInt(metadataTypeOptionalBlockState).
			VarInt(javaworld.StateID(coreworld.BlockFromResourceLocation(e.EndermanCarriedBlock)))
		hasMetadata = true
	}
	if e.Type != corentity.TypeVillager {
		if !hasMetadata {
			return nil
		}
		return b.Byte(0xff).Build()
	}
	level := e.VillagerLevel
	if level < 1 {
		level = 1
	} else if level > 5 {
		level = 5
	}
	return b.
		Byte(18).   // DATA_VILLAGER_DATA metadata index
		VarInt(19). // VILLAGER_DATA serializer ID
		VarInt(villagerVariantProtocolID(e.VillagerVariant)).
		VarInt(villagerProfessionProtocolID(e.VillagerProfession)).
		VarInt(level).
		Byte(0xff). // metadata list terminator
		Build()
}

func buildMobEquipment(e *corentity.Entity) *protocol.Packet {
	if e == nil || e.MainHandItemID == "" {
		return nil
	}
	b := protocol.NewBuilder(packetIDSetEquipment).
		VarInt(e.EntityID).
		Byte(0) // Main hand, final equipment entry.
	encodeSlot(b, player.ItemStack{ItemID: e.MainHandItemID, Count: 1})
	return b.Build()
}

func isJavaTamableAnimal(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeCat, corentity.TypeParrot, corentity.TypeWolf:
		return true
	default:
		return false
	}
}

func isJavaAbstractHorse(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeCamel, corentity.TypeDonkey, corentity.TypeHorse, corentity.TypeMule,
		corentity.TypeSkeletonHorse, corentity.TypeZombieHorse, corentity.TypeLlama,
		corentity.TypeTraderLlama:
		return true
	default:
		return false
	}
}

// BroadcastVillagerMetadata sends a Set Entity Data packet for a villager to
// all connected sessions — used when a baby villager grows into an adult.
func BroadcastVillagerMetadata(e *corentity.Entity, mgr *session.Manager) {
	BroadcastMobMetadata(e, mgr)
}

// BroadcastMobMetadata publishes canonical baby/tame/owner/saddle state.
func BroadcastMobMetadata(e *corentity.Entity, mgr *session.Manager) {
	if mgr == nil {
		return
	}
	pkt := buildMobMetadata(e)
	if pkt == nil {
		return
	}
	go func() {
		for _, s := range mgr.SnapshotAll() {
			_ = s.Conn.WritePacket(pkt)
		}
	}()
}

// BroadcastMobFireState updates the shared entity flags even when fire was
// extinguished. buildMobMetadata intentionally omits default metadata for most
// hostiles, so the explicit zero flag is required to clear the flame renderer.
func BroadcastMobFireState(e *corentity.Entity, mgr *session.Manager) {
	if e == nil || mgr == nil {
		return
	}
	flags := byte(0)
	if e.FireTicks > 0 {
		flags = 0x01
	}
	pkt := protocol.NewBuilder(packetIDSetEntityData).
		VarInt(e.EntityID).
		Byte(0).
		VarInt(0).
		Byte(flags).
		Byte(0xff).
		Build()
	for _, sess := range mgr.SnapshotAll() {
		_ = sess.Conn.WritePacket(pkt)
	}
}

// BroadcastMobUsingItemState starts or stops the living-entity main-hand use
// animation. Skeletons keep this active for the same 20 draw ticks as Pumpkin.
func BroadcastMobUsingItemState(e *corentity.Entity, mgr *session.Manager) {
	if e == nil || mgr == nil {
		return
	}
	flags := byte(0)
	if e.UsingItem {
		flags = 0x01
	}
	pkt := protocol.NewBuilder(packetIDSetEntityData).
		VarInt(e.EntityID).
		Byte(8).
		VarInt(0).
		Byte(flags).
		Byte(0xff).
		Build()
	for _, sess := range mgr.SnapshotAll() {
		_ = sess.Conn.WritePacket(pkt)
	}
}

// BroadcastEntityStatus triggers vanilla entity feedback such as love hearts
// (18) and taming failure/success (6/7).
func BroadcastEntityStatus(entityID int32, status byte, mgr *session.Manager) {
	if mgr == nil {
		return
	}
	pkt := buildEntityEvent(entityID, status)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastEntityStatusInDimension emits a transient entity event only to
// viewers of the canonical world that owns the entity.
func BroadcastEntityStatusInDimension(entityID int32, status byte, mgr *session.Manager, dimension int32) {
	if mgr == nil {
		return
	}
	pkt := buildEntityEvent(entityID, status)
	for _, current := range mgr.SnapshotAll() {
		if current.Player != nil && current.Player.Dimension == dimension {
			_ = current.Conn.WritePacket(pkt)
		}
	}
}

// BroadcastVillagerSleepState updates the tracked pose and bed position in
// packet order. Sleeping entities stay at their canonical navigation position;
// the Java renderer anchors the sleeping pose to the tracked bed position.
func BroadcastVillagerSleepState(e *corentity.Entity, mgr *session.Manager) {
	metadata := buildMobMetadata(e)
	if metadata == nil {
		return
	}
	for _, s := range mgr.SnapshotAll() {
		if !e.Sleeping {
			teleport := buildTeleportMob(e)
			_ = s.Conn.WritePacket(teleport)
		}
		_ = s.Conn.WritePacket(metadata)
	}
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

func buildEntityAnimation(entityID int32, animation byte) *protocol.Packet {
	return protocol.NewBuilder(packetIDEntityAnimation).
		VarInt(entityID).
		Byte(animation).
		Build()
}

// BroadcastPlayerArmSwing mirrors Vanilla's Animate packet to Java viewers in
// the same dimension. The source client already predicts its own swing.
func BroadcastPlayerArmSwing(p *player.Player, hand int32, mgr *session.Manager) {
	if p == nil || mgr == nil {
		return
	}
	animation := byte(0) // main arm
	if hand == 1 {
		animation = 3 // off hand
	}
	pkt := buildEntityAnimation(p.EntityID, animation)
	for _, viewer := range mgr.Snapshot(p.UUID) {
		if viewer.Player != nil && viewer.Player.Dimension == p.Dimension {
			_ = viewer.Conn.WritePacket(pkt)
		}
	}
}

// buildEntityEvent constructs ClientboundEntityEventPacket. Status 3 starts
// the standard living-entity death animation on the vanilla client.
func buildEntityEvent(entityID int32, status byte) *protocol.Packet {
	return protocol.NewBuilder(packetIDEntityEvent).
		Int(entityID).
		Byte(status).
		Build()
}

func buildCollectItem(itemEntityID, collectorEntityID int32, count int) *protocol.Packet {
	return protocol.NewBuilder(packetIDTakeItemEntity).
		VarInt(itemEntityID).
		VarInt(collectorEntityID).
		VarInt(int32(count)).
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
		if equipment := buildMobEquipment(e); equipment != nil {
			_ = conn.WritePacket(equipment)
		}
		if passengers := e.PassengerIDs(); len(passengers) > 0 {
			_ = conn.WritePacket(buildSetPassengers(e.EntityID, passengers))
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
		if equipment := buildMobEquipment(e); equipment != nil {
			_ = conn.WritePacket(equipment)
		}
		if passengers := e.PassengerIDs(); len(passengers) > 0 {
			_ = conn.WritePacket(buildSetPassengers(e.EntityID, passengers))
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
		if equipment := buildMobEquipment(e); equipment != nil {
			_ = s.Conn.WritePacket(equipment)
		}
	}
}

// BroadcastSpawnMobInDimension sends an entity only to Java players viewing
// the world that owns it. Bedrock discovers the same canonical entity through
// its dimension-aware synchroniser.
func BroadcastSpawnMobInDimension(e *corentity.Entity, mgr *session.Manager, dimension int32) {
	if e == nil || mgr == nil {
		return
	}
	pkt, ok := buildSpawnMob(e)
	if !ok {
		return
	}
	metadata := buildMobMetadata(e)
	equipment := buildMobEquipment(e)
	for _, current := range mgr.SnapshotAll() {
		if current.Player == nil || current.Player.Dimension != dimension {
			continue
		}
		_ = current.Conn.WritePacket(pkt)
		if metadata != nil {
			_ = current.Conn.WritePacket(metadata)
		}
		if equipment != nil {
			_ = current.Conn.WritePacket(equipment)
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

// BroadcastDeathAnimation starts the vanilla red/rotating death animation.
func BroadcastDeathAnimation(entityID int32, mgr *session.Manager) {
	pkt := buildEntityEvent(entityID, 3)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastCollectItem animates a dropped stack flying into its collector.
func BroadcastCollectItem(itemEntityID, collectorEntityID int32, count int, mgr *session.Manager) {
	pkt := buildCollectItem(itemEntityID, collectorEntityID, count)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastCreeperSwell updates the creeper swell direction tracked value.
// Creeper inherits Mob metadata through index 15; index 16 is its VarInt swell
// state (-1 idle, 1 fusing) in protocol 769.
func BroadcastCreeperSwell(entityID int32, swelling bool, mgr *session.Manager) {
	state := int32(-1)
	if swelling {
		state = 1
	}
	pkt := protocol.NewBuilder(packetIDSetEntityData).
		VarInt(entityID).
		Byte(16).
		VarInt(1).
		VarInt(state).
		Byte(0xff).
		Build()
	for _, sess := range mgr.SnapshotAll() {
		_ = sess.Conn.WritePacket(pkt)
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

// ── Sleep animation ───────────────────────────────────────────────────────────

// appendLivingEntitySleepMetadata writes the two tracked values vanilla changes
// when a living entity enters or leaves a bed. Protocol 769 uses Pose index 6
// with serializer 21, and Optional BlockPos index 14 with serializer 11.
// Entity flags at index 0 do not contain a sleeping bit.
func appendLivingEntitySleepMetadata(b *protocol.Builder, sleeping bool, bedPos spatial.BlockPos) *protocol.Builder {
	pose := entityPoseStanding
	if sleeping {
		pose = entityPoseSleeping
	}
	b = b.
		Byte(entityMetadataPoseIndex).
		VarInt(metadataTypePose).
		VarInt(pose).
		Byte(livingEntityMetadataSleepingPosIndex).
		VarInt(metadataTypeOptionalBlockPos).
		Bool(sleeping)
	if sleeping {
		b = b.Long(bedPos.Encode())
	}
	return b
}

// buildPlayerPoseMetadata builds a Set Entity Data packet that changes a
// player's pose and optional sleeping position together.
//
// Canonical pose values:
//
//	0 = STANDING
//	2 = SLEEPING
func buildPlayerPoseMetadata(entityID int32, sleeping bool, bedPos spatial.BlockPos) *protocol.Packet {
	b := protocol.NewBuilder(packetIDSetEntityData).VarInt(entityID)
	return appendLivingEntitySleepMetadata(b, sleeping, bedPos).
		Byte(0xFF).
		Build()
}

// BroadcastPlayerSleeping sends the SLEEPING pose and bed position for the
// given entity to all connected sessions, including the sleeping player.
func BroadcastPlayerSleeping(entityID int32, bedPos spatial.BlockPos, mgr *session.Manager) {
	pkt := buildPlayerPoseMetadata(entityID, true, bedPos)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastPlayerWaking sends the STANDING pose and clears the sleeping
// position for the given entity, ending the sleep animation.
func BroadcastPlayerWaking(entityID int32, mgr *session.Manager) {
	pkt := buildPlayerPoseMetadata(entityID, false, spatial.BlockPos{})
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// buildSetTime builds a Set Time (minecraft:set_time) packet.
// age = world age (total ticks), dayTime = time of day (0–23999).
func buildSetTime(age, dayTime int64) *protocol.Packet {
	return protocol.NewBuilder(packetIDSetTime).
		Long(age).
		Long(dayTime).
		Bool(true). // time_of_day_increasing — server controls the clock
		Build()
}

// DispatchWorldTime synchronizes the Java client day/night cycle with the
// simulation clock without blocking the entity tick on socket writes.
func DispatchWorldTime(age, dayTime int64, mgr *session.Manager) {
	pkt := buildSetTime(age, dayTime)
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
func DispatchTickBroadcast(moved []*corentity.Entity, hurtEntities []*corentity.Entity, deathIDs, deadIDs []int32, spawned []*corentity.Entity, mgr *session.Manager) {
	dispatchTickBroadcast(moved, hurtEntities, deathIDs, deadIDs, spawned, mgr, nil)
}

// DispatchTickBroadcastInDimension keeps entity movement and removal inside
// the canonical world that owns it.
func DispatchTickBroadcastInDimension(moved []*corentity.Entity, hurtEntities []*corentity.Entity, deathIDs, deadIDs []int32, spawned []*corentity.Entity, mgr *session.Manager, dimension int32) {
	dispatchTickBroadcast(moved, hurtEntities, deathIDs, deadIDs, spawned, mgr, &dimension)
}

func dispatchTickBroadcast(moved []*corentity.Entity, hurtEntities []*corentity.Entity, deathIDs, deadIDs []int32, spawned []*corentity.Entity, mgr *session.Manager, dimension *int32) {
	if mgr == nil {
		return
	}
	pkts := make([]*protocol.Packet, 0, len(moved)+len(hurtEntities)+len(deathIDs)+len(deadIDs)+len(spawned)*2)
	for _, e := range spawned {
		if spawn, ok := buildSpawnMob(e); ok {
			pkts = append(pkts, spawn)
			if metadata := buildMobMetadata(e); metadata != nil {
				pkts = append(pkts, metadata)
			}
			if equipment := buildMobEquipment(e); equipment != nil {
				pkts = append(pkts, equipment)
			}
		}
	}
	for _, e := range moved {
		pkts = append(pkts, buildTeleportMob(e))
	}
	for _, e := range hurtEntities {
		pkts = append(pkts, buildHurtAnimation(e.EntityID, e.Yaw), buildEntityMotion(e))
		if sound := buildEntitySound(hurtSound(e), soundCategoryNeutral, e.EntityID, 1, 1); sound != nil {
			pkts = append(pkts, sound)
		}
	}
	for _, id := range deathIDs {
		pkts = append(pkts, buildEntityEvent(id, 3))
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
			if dimension != nil && (s.Player == nil || s.Player.Dimension != *dimension) {
				continue
			}
			for _, pkt := range pkts {
				_ = s.Conn.WritePacket(pkt)
			}
		}
	}()
}
