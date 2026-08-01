package handler

// Boat packet handling for GoCraft.
//
// Boats are rideable entities: a player right-clicks to board, the client then
// sends move_vehicle packets (instead of move_player_*) to update the boat's
// position, and sneaking exits the boat.
//
// Relevant packets (Java 1.21.4 / protocol 769):
//   CB  set_passengers  (0x55 = 85)  — tells client who is riding what
//   SB  move_vehicle    (0x1B = 27)  — client updates boat position while riding
//   SB  player_input    (0x20 = 32)  — forward/sideways/flags; sneak flag = dismount
//   SB  player_command  (0x19 = 25)  — action 8 = leave vehicle

import (
	"fmt"
	"math"

	corentity "GoCraft/core/entity"
	coreplayer "GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

// ── Packet builders ───────────────────────────────────────────────────────────

// buildSetPassengers builds a CB Set Passengers packet.
//
// Wire layout (1.21.4):
//
//	VarInt  entity_id
//	VarInt  count
//	VarInt  passenger_id[0..count-1]
func buildSetPassengers(vehicleEntityID int32, passengerIDs []int32) *protocol.Packet {
	b := protocol.NewBuilder(packetIDSetPassengers).VarInt(vehicleEntityID).VarInt(int32(len(passengerIDs)))
	for _, id := range passengerIDs {
		b = b.VarInt(id)
	}
	return b.Build()
}

// BroadcastSetPassengers sends a Set Passengers packet to all sessions.
func BroadcastSetPassengers(vehicleEntityID int32, passengerIDs []int32, mgr *session.Manager) {
	pkt := buildSetPassengers(vehicleEntityID, passengerIDs)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// ── Serverbound packet handlers ───────────────────────────────────────────────

// HandleMoveVehiclePacket parses a SB Move Vehicle packet and updates the
// boat's position in the world, then broadcasts it to all other players.
//
// Wire layout (1.21.4):
//
//	Double  x, y, z
//	Float   yaw, pitch
//	Bool    on_ground
func HandleMoveVehiclePacket(pkt *protocol.Packet, p *coreplayer.Player, w *coreworld.World, conn *network.ClientConn, mgr *session.Manager) error {
	r := pkt.Reader()

	x, err := protocol.ReadDouble(r)
	if err != nil {
		return fmt.Errorf("move_vehicle: x: %w", err)
	}
	y, err := protocol.ReadDouble(r)
	if err != nil {
		return fmt.Errorf("move_vehicle: y: %w", err)
	}
	z, err := protocol.ReadDouble(r)
	if err != nil {
		return fmt.Errorf("move_vehicle: z: %w", err)
	}
	yaw, err := protocol.ReadFloat(r)
	if err != nil {
		return fmt.Errorf("move_vehicle: yaw: %w", err)
	}
	if _, err = protocol.ReadFloat(r); err != nil { // pitch (unused for boats)
		return fmt.Errorf("move_vehicle: pitch: %w", err)
	}
	onGround, err := protocol.ReadBool(r)
	if err != nil {
		return fmt.Errorf("move_vehicle: on_ground: %w", err)
	}

	boatID := p.VehicleEntityID
	if boatID == 0 {
		return nil
	}
	boat, ok := w.Entities.Get(boatID)
	if !ok {
		p.VehicleEntityID = 0
		return nil
	}

	// Reject suspiciously large jumps (> 3 blocks) as lag-spike protection.
	dx := x - boat.Position.X
	dy := y - boat.Position.Y
	dz := z - boat.Position.Z
	if math.Abs(dx) > 3 || math.Abs(dy) > 3 || math.Abs(dz) > 3 {
		_ = sendSyncPosition(conn, p, 0)
		return nil
	}

	// Update boat position.
	boat.Position.X = x
	boat.Position.Y = y
	boat.Position.Z = z
	boat.Yaw = yaw
	boat.OnGround = onGround

	// Keep player's logical position in sync (chunk loading, /tp, etc.).
	p.Position.X = x
	p.Position.Y = y + 0.35
	p.Position.Z = z

	// Broadcast to all other clients.
	broadcastBoatPositionExcept(boat, p.EntityID, mgr)
	return nil
}

// HandlePlayerInputPacket parses a SB Player Input packet.
// Sneak flag (bit 1) = exit vehicle.
//
// Wire layout (1.21.4):
//
//	Float  sideways
//	Float  forward
//	Byte   flags  (bit 0=jump, bit 1=sneak)
func HandlePlayerInputPacket(pkt *protocol.Packet, p *coreplayer.Player, w *coreworld.World, conn *network.ClientConn, mgr *session.Manager) error {
	r := pkt.Reader()
	if _, err := protocol.ReadFloat(r); err != nil {
		return fmt.Errorf("player_input: sideways: %w", err)
	}
	if _, err := protocol.ReadFloat(r); err != nil {
		return fmt.Errorf("player_input: forward: %w", err)
	}
	flags, err := protocol.ReadByte(r)
	if err != nil {
		return fmt.Errorf("player_input: flags: %w", err)
	}
	if flags&0x02 != 0 && p.VehicleEntityID != 0 {
		DismountPlayer(p, w, conn, mgr)
	}
	return nil
}

// HandlePlayerCommandPacket parses a SB Player Command packet.
// Action 8 = leave vehicle.
//
// Wire layout (1.21.4):
//
//	VarInt  entity_id
//	VarInt  action  (8 = leave vehicle)
//	VarInt  jump_boost
func HandlePlayerCommandPacket(pkt *protocol.Packet, p *coreplayer.Player, w *coreworld.World, conn *network.ClientConn, mgr *session.Manager) error {
	r := pkt.Reader()
	if _, err := protocol.ReadVarInt(r); err != nil {
		return fmt.Errorf("player_command: entity_id: %w", err)
	}
	action, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("player_command: action: %w", err)
	}
	switch action {
	case 2: // LEAVE_BED
		if p.Sleeping {
			p.Sleeping = false
			BroadcastPlayerWaking(p.EntityID, mgr)
			_ = sendSystemMessage(conn, "You left your bed.")
		}
	case 8: // LEAVE_VEHICLE
		if p.VehicleEntityID != 0 {
			DismountPlayer(p, w, conn, mgr)
		}
	}
	return nil
}

// ── Mount / dismount ──────────────────────────────────────────────────────────

// MountPlayer seats the player onto boat and broadcasts Set Passengers.
// Returns false if the boat is not found or already occupied.
func MountPlayer(p *coreplayer.Player, boatEntityID int32, w *coreworld.World, mgr *session.Manager) bool {
	boat, ok := w.Entities.Get(boatEntityID)
	if !ok || !corentity.IsBoat(boat.Type) {
		return false
	}
	if boat.RiderEntityID != 0 {
		return false // occupied
	}
	boat.RiderEntityID = p.EntityID
	p.VehicleEntityID = boatEntityID
	BroadcastSetPassengers(boatEntityID, []int32{p.EntityID}, mgr)
	return true
}

// DismountPlayer ejects the player from their vehicle and teleports them to a
// safe position beside the boat.
func DismountPlayer(p *coreplayer.Player, w *coreworld.World, conn *network.ClientConn, mgr *session.Manager) {
	boatID := p.VehicleEntityID
	if boatID == 0 {
		return
	}
	p.VehicleEntityID = 0

	if boat, ok := w.Entities.Get(boatID); ok {
		boat.RiderEntityID = 0
		p.Position.X = boat.Position.X + 1.5
		p.Position.Y = boat.Position.Y
		p.Position.Z = boat.Position.Z
	}

	BroadcastSetPassengers(boatID, nil, mgr)
	_ = sendSyncPosition(conn, p, 0)
}

// ── Broadcast helpers ─────────────────────────────────────────────────────────

func broadcastBoatPositionExcept(boat *corentity.Entity, excludeEntityID int32, mgr *session.Manager) {
	pkt := buildTeleportMob(boat)
	for _, s := range mgr.SnapshotAll() {
		if s.Player != nil && s.Player.EntityID == excludeEntityID {
			continue
		}
		_ = s.Conn.WritePacket(pkt)
	}
}
