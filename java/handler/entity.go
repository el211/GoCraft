package handler

// Entity packet handling for Milestone 6 (multiplayer sync).
//
// This file owns the S→C packets that describe player entities to other clients:
// spawning, despawning, position/rotation updates, and the tab-list bookkeeping
// that accompanies them.  All packet IDs are for Java Edition 1.21.4 (protocol 769).
//
// IDs annotated "(estimate)" were derived by comparison with known-good IDs from
// earlier protocol versions.  Verify against a 1.21.4 packet capture if the client
// crashes or shows wrong entity behaviour after M6.

import (
	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

// playerEntityTypeID is the vanilla 1.21.4 entity type registry index for
// "minecraft:player", resolved from the embedded registries.json at init time.
var playerEntityTypeID = javaworld.EntityTypeID("minecraft:player")

// ── Angle encoding ────────────────────────────────────────────────────────────

// degToAngle converts a rotation angle in degrees to the Minecraft Angle wire
// type: a single unsigned byte where 256 units represent a full 360° rotation.
func degToAngle(deg float32) byte {
	return byte(int32(deg*256.0/360.0) & 0xFF)
}

// ── Packet builders ───────────────────────────────────────────────────────────

// buildSpawnPlayer builds a Spawn Entity (0x01) packet that spawns p as a
// player-type entity in another client's world.
//
// Wire layout (1.21.4):
//
//	VarInt  entity_id
//	UUID    entity_uuid
//	VarInt  type               (playerEntityTypeID)
//	Double  x, y, z
//	Angle   pitch, yaw         (Byte, 256 units = 360°)
//	Angle   head_yaw           (Byte)
//	VarInt  data               (0 — no special spawn data for players)
//	Short   velocity_x/y/z     (0 — server does not track velocity)
func buildSpawnPlayer(p *player.Player) *protocol.Packet {
	return protocol.NewBuilder(packetIDSpawnEntity).
		VarInt(p.EntityID).
		UUID(protocol.UUID(p.UUID)).
		VarInt(playerEntityTypeID).
		Double(p.Position.X).
		Double(p.Position.Y).
		Double(p.Position.Z).
		Byte(degToAngle(p.Rotation.Pitch)).
		Byte(degToAngle(p.Rotation.Yaw)).
		Byte(degToAngle(p.Rotation.Yaw)). // head yaw = body yaw at spawn
		VarInt(0).                        // data
		Short(0).Short(0).Short(0).       // velocity x, y, z
		Build()
}

// buildRemoveEntities builds a Remove Entities packet for a single entity ID.
func buildRemoveEntities(entityID int32) *protocol.Packet {
	return protocol.NewBuilder(packetIDRemoveEntities).
		VarInt(1).
		VarInt(entityID).
		Build()
}

// buildPlayerInfoRemove builds a Player Info Remove packet to remove one player
// from the tab list of receiving clients.
func buildPlayerInfoRemove(uuid [16]byte) *protocol.Packet {
	return protocol.NewBuilder(packetIDPlayerInfoRemove).
		VarInt(1).
		UUID(protocol.UUID(uuid)).
		Build()
}

// buildPlayerInfoUpdatePkt builds a Player Info Update (ADD_PLAYER + UPDATE_LISTED)
// packet describing p, for broadcasting to other sessions.
func buildPlayerInfoUpdatePkt(p *player.Player) *protocol.Packet {
	const actions byte = 0x01 | 0x08 // ADD_PLAYER + UPDATE_LISTED
	return protocol.NewBuilder(packetIDPlayerInfoUpdate).
		Byte(actions).
		VarInt(1).
		UUID(protocol.UUID(p.UUID)).
		String(p.Username).VarInt(0). // name + 0 skin properties
		Bool(true).                   // listed in tab
		Build()

}

// buildTeleportEntity builds a Teleport Entity packet with p's current position
// and rotation.  Used to broadcast movement updates to other players.
//
// Wire layout (1.21.4 — verify against packet dump):
//
//	VarInt  entity_id
//	Double  x, y, z
//	Double  velocity_x/y/z   (0 — not tracked server-side in M6)
//	Float   yaw, pitch
//	Bool    on_ground
func buildTeleportEntity(p *player.Player) *protocol.Packet {
	return protocol.NewBuilder(packetIDTeleportEntity).
		VarInt(p.EntityID).
		Double(p.Position.X).
		Double(p.Position.Y).
		Double(p.Position.Z).
		Double(0).Double(0).Double(0). // velocity
		Float(p.Rotation.Yaw).
		Float(p.Rotation.Pitch).
		Bool(p.OnGround).
		Build()
}

// buildSetHeadRotation builds a Set Head Rotation packet so the player's
// head tracks their look direction for other clients.
func buildSetHeadRotation(p *player.Player) *protocol.Packet {
	return protocol.NewBuilder(packetIDSetHeadRotation).
		VarInt(p.EntityID).
		Byte(degToAngle(p.Rotation.Yaw)).
		Build()
}

// ── Session-level helpers ─────────────────────────────────────────────────────

// spawnExistingPlayersFor sends Player Info Update + Spawn Entity for every
// session in mgr (except the joining player) to the joiner's connection, so
// they immediately see all currently-online players.
// Snapshots the session list so the manager lock is not held during writes.
func spawnExistingPlayersFor(conn *network.ClientConn, mgr *session.Manager, joinerUUID [16]byte) {
	for _, s := range mgr.Snapshot(joinerUUID) {
		_ = conn.WritePacket(buildPlayerInfoUpdatePkt(s.Player))
		_ = conn.WritePacket(buildSpawnPlayer(s.Player))
	}
}

// onPlayerJoin is called after the joining session has been added to mgr.
// It:
//  1. Sends the joiner info about all existing players (so they see others).
//  2. Sends the joiner all currently-loaded non-player entities (mobs, etc.).
//  3. Broadcasts the joiner's info + spawn packet to all existing players.
//
// Snapshots the session list so the manager lock is not held during writes.
func onPlayerJoin(mgr *session.Manager, joining *session.Session, mobs []*corentity.Entity) {
	// Step 1 — tell the joining player about everyone already online.
	spawnExistingPlayersFor(joining.Conn, mgr, joining.Player.UUID)

	// Step 2 — send existing non-player entities to the joining player.
	sendExistingMobsTo(joining.Conn, mobs)

	// Step 3 — tell everyone else about the joining player.
	infoUpdate := buildPlayerInfoUpdatePkt(joining.Player)
	spawnPkt := buildSpawnPlayer(joining.Player)
	for _, existing := range mgr.Snapshot(joining.Player.UUID) {
		_ = existing.Conn.WritePacket(infoUpdate)
		_ = existing.Conn.WritePacket(spawnPkt)
	}
}

// onPlayerLeave is called before the leaving session is removed from mgr.
// It broadcasts Remove Entities and Player Info Remove to all remaining sessions.
// Snapshots the session list so the manager lock is not held during writes.
func onPlayerLeave(mgr *session.Manager, leaving *session.Session) {
	removePkt := buildRemoveEntities(leaving.Player.EntityID)
	infoRemove := buildPlayerInfoRemove(leaving.Player.UUID)
	for _, s := range mgr.Snapshot(leaving.Player.UUID) {
		_ = s.Conn.WritePacket(removePkt)
		_ = s.Conn.WritePacket(infoRemove)
	}
}

// broadcastPosition sends Teleport Entity and Set Head Rotation for p to all
// sessions except p's own.  Called on every movement packet from the client.
// BroadcastExcept already snapshots internally, so the lock is not held during
// the two write passes.
func broadcastPosition(mgr *session.Manager, p *player.Player) {
	teleport := buildTeleportEntity(p)
	headRot := buildSetHeadRotation(p)
	// Snapshot once and reuse to avoid two independent lock acquisitions.
	for _, s := range mgr.Snapshot(p.UUID) {
		_ = s.Conn.WritePacket(teleport)
		_ = s.Conn.WritePacket(headRot)
	}
}

// SendExternalPlayerJoin introduces a player owned by another protocol adapter
// (currently Bedrock) to one Java session.
func SendExternalPlayerJoin(conn *network.ClientConn, p *player.Player) {
	_ = conn.WritePacket(buildPlayerInfoUpdatePkt(p))
	_ = conn.WritePacket(buildSpawnPlayer(p))
}

// SendExternalPlayerLeave removes a cross-edition player from one Java session.
func SendExternalPlayerLeave(conn *network.ClientConn, p *player.Player) {
	_ = conn.WritePacket(buildRemoveEntities(p.EntityID))
	_ = conn.WritePacket(buildPlayerInfoRemove(p.UUID))
}

// SendExternalPlayerPosition updates a cross-edition player for one Java viewer.
func SendExternalPlayerPosition(conn *network.ClientConn, p *player.Player) {
	_ = conn.WritePacket(buildTeleportEntity(p))
	_ = conn.WritePacket(buildSetHeadRotation(p))
}

// SendExternalPlayerEquipment synchronizes a cross-edition player's visible
// hands and armour. Protocol 769 uses a continuation bit on each equipment slot
// byte instead of an array length.
func SendExternalPlayerEquipment(conn *network.ClientConn, p *player.Player) {
	if conn == nil || p == nil {
		return
	}
	b := protocol.NewBuilder(packetIDSetEquipment).VarInt(p.EntityID)
	equipment := []struct {
		slot byte
		item player.ItemStack
	}{
		{0, p.HeldItem()},
		{1, p.Inventory[45]},
		{2, p.Inventory[8]},
		{3, p.Inventory[7]},
		{4, p.Inventory[6]},
		{5, p.Inventory[5]},
	}
	for index, entry := range equipment {
		slot := entry.slot
		if index != len(equipment)-1 {
			slot |= 0x80
		}
		b.Byte(slot)
		encodeSlot(b, entry.item)
	}
	_ = conn.WritePacket(b.Build())
}
