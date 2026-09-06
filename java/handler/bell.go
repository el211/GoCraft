package handler

import (
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

func buildBellRingPackets(position spatial.BlockPos, direction string) []*protocol.Packet {
	bellID, ok := javaworld.BlockTypeID("minecraft:bell")
	if !ok {
		return nil
	}
	blockEvent := protocol.NewBuilder(packetIDBlockAction).
		Long(position.Encode()).
		Byte(1).
		Byte(javaBellDirection(direction)).
		VarInt(bellID).
		Build()
	sound := buildSoundAt("minecraft:block.bell.use", soundCategoryBlocks,
		float64(position.X)+0.5, float64(position.Y)+0.5, float64(position.Z)+0.5, 2, 1)
	if sound == nil {
		return []*protocol.Packet{blockEvent}
	}
	return []*protocol.Packet{blockEvent, sound}
}

func javaBellDirection(direction string) byte {
	switch direction {
	case "south":
		return 3
	case "west":
		return 4
	case "east":
		return 5
	default:
		return 2
	}
}

// broadcastNoteBlockAction sends the block_action packet that makes the Java
// client render the floating note particle above the note block. The note value
// (0–24) selects the particle colour. Both the block-position encoding and the
// block-type ID (VarInt) are required by the protocol.
func broadcastNoteBlockAction(x, y, z, note int, block coreworld.Block, mgr *session.Manager, dimension int32) {
	if mgr == nil {
		return
	}
	noteBlockID, ok := javaworld.BlockTypeID(block.ResourceLocation())
	if !ok {
		return
	}
	pos := spatial.BlockPos{X: int32(x), Y: int32(y), Z: int32(z)}
	pkt := protocol.NewBuilder(packetIDBlockAction).
		Long(pos.Encode()).
		Byte(0).            // action type 0 = play note
		Byte(byte(note)).   // note value 0–24
		VarInt(noteBlockID).
		Build()
	for _, cur := range mgr.SnapshotAll() {
		if cur.Player == nil || cur.Player.Dimension != dimension {
			continue
		}
		_ = cur.Conn.WritePacket(pkt)
	}
}

// BroadcastNoteBlockAction is the exported version for use from server/bell.go.
func BroadcastNoteBlockAction(x, y, z, note int, block coreworld.Block, mgr *session.Manager, dimension int32) {
	broadcastNoteBlockAction(x, y, z, note, block, mgr, dimension)
}

// BroadcastBellRing sends the transient bell animation and sound to Java
// viewers in the bell's dimension. The permanent block state is untouched.
func BroadcastBellRing(position spatial.BlockPos, direction string, dimension int32, mgr *session.Manager) {
	if mgr == nil {
		return
	}
	packets := buildBellRingPackets(position, direction)
	for _, current := range mgr.SnapshotAll() {
		if current.Player == nil || current.Player.Dimension != dimension {
			continue
		}
		for _, packet := range packets {
			_ = current.Conn.WritePacket(packet)
		}
	}
}
