package handler

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

// handleSignUpdate processes the serverbound Update Sign packet (0x34 S→C).
// The client sends this after finishing sign text editing. The server validates
// the position, stores the text as block entity data, and broadcasts the
// update to nearby players.
//
// Packet layout (Java 1.21.4):
//
//	Long   position (block position)
//	Bool   is_front_text
//	String line1..line4 (4 × VarInt-prefixed UTF-8, each ≤ 384 bytes)
func handleSignUpdate(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager) error {
	r := pkt.Reader()
	posRaw, err := protocol.ReadLong(r)
	if err != nil {
		return fmt.Errorf("sign update: reading position: %w", err)
	}
	bx, by, bz := decodeBlockPos(posRaw)

	isFront, err := protocol.ReadBool(r)
	if err != nil {
		return fmt.Errorf("sign update: reading is_front_text: %w", err)
	}
	var lines [4]string
	for i := range lines {
		lines[i], err = protocol.ReadString(r)
		if err != nil {
			return fmt.Errorf("sign update: reading line %d: %w", i, err)
		}
		// Vanilla truncates sign lines to 15 characters.
		if len([]rune(lines[i])) > 15 {
			runes := []rune(lines[i])
			lines[i] = string(runes[:15])
		}
	}

	block := w.GetBlock(bx, by, bz)
	if !isSignBlock(block.ResourceLocation()) {
		return nil
	}

	data := buildSignNBT(lines, isFront)
	w.SetBlockEntity(bx, by, bz, "minecraft:sign", data)
	entity := w.GetBlockEntity(bx, by, bz)
	BroadcastBlockEntityDataInDimension(entity, mgr, p.Dimension)
	return nil
}

// isSignBlock returns true for any standing or wall sign block.
func isSignBlock(name string) bool {
	return strings.HasSuffix(name, "_sign") || strings.HasSuffix(name, "_wall_sign") ||
		strings.HasSuffix(name, "_hanging_sign") || strings.HasSuffix(name, "_wall_hanging_sign")
}

// decodeBlockPos unpacks a 64-bit packed block position into x, y, z.
func decodeBlockPos(pos int64) (x, y, z int) {
	x = int(pos >> 38)
	y = int(pos << 52 >> 52)
	z = int(pos << 26 >> 38)
	return
}

// buildSignNBT encodes the four sign lines into a minimal network-NBT compound
// matching the Java 1.21.4 sign block entity format. Text is stored as plain
// JSON text components in the messages list of the front or back text object.
func buildSignNBT(lines [4]string, isFront bool) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x0A) // root TAG_Compound (no name in network NBT)
	if isFront {
		writeSignTextNBT(&buf, "front_text", lines)
		writeSignEmptyNBT(&buf, "back_text")
	} else {
		writeSignEmptyNBT(&buf, "front_text")
		writeSignTextNBT(&buf, "back_text", lines)
	}
	buf.WriteByte(0x00) // TAG_End
	return buf.Bytes()
}

func writeSignTextNBT(buf *bytes.Buffer, key string, lines [4]string) {
	// Write a compound entry named key.
	buf.WriteByte(0x0A) // TAG_Compound
	writeNBTKey(buf, key)

	// "messages" TAG_List of 4 TAG_String
	buf.WriteByte(0x09) // TAG_List
	writeNBTKey(buf, "messages")
	buf.WriteByte(0x08)                                                // element type: TAG_String
	binary.Write(buf, binary.BigEndian, int32(len(lines)))             //nolint:errcheck
	for _, line := range lines {
		component := `{"text":"` + escapeJSONString(line) + `"}`
		binary.Write(buf, binary.BigEndian, uint16(len(component))) //nolint:errcheck
		buf.WriteString(component)
	}

	// "has_glowing_text" TAG_Byte = 0
	buf.WriteByte(0x01) // TAG_Byte
	writeNBTKey(buf, "has_glowing_text")
	buf.WriteByte(0x00)

	buf.WriteByte(0x00) // TAG_End closes the compound
}

func writeSignEmptyNBT(buf *bytes.Buffer, key string) {
	buf.WriteByte(0x0A) // TAG_Compound
	writeNBTKey(buf, key)

	// "messages" TAG_List of 4 empty strings
	buf.WriteByte(0x09)
	writeNBTKey(buf, "messages")
	buf.WriteByte(0x08)
	binary.Write(buf, binary.BigEndian, int32(4)) //nolint:errcheck
	for range 4 {
		component := `{"text":""}`
		binary.Write(buf, binary.BigEndian, uint16(len(component))) //nolint:errcheck
		buf.WriteString(component)
	}

	buf.WriteByte(0x01)
	writeNBTKey(buf, "has_glowing_text")
	buf.WriteByte(0x00)

	buf.WriteByte(0x00)
}

func writeNBTKey(buf *bytes.Buffer, key string) {
	binary.Write(buf, binary.BigEndian, uint16(len(key))) //nolint:errcheck
	buf.WriteString(key)
}

// escapeJSONString escapes special characters for embedding in a JSON string.
func escapeJSONString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}

// openSignEditor sends the Open Sign Editor clientbound packet so the client
// opens the sign text UI when the player places a sign.
func openSignEditor(conn interface{ WritePacket(*protocol.Packet) error }, x, y, z int, isFront bool) error {
	pos := packBlockPos(x, y, z)
	return conn.WritePacket(protocol.NewBuilder(packetIDOpenSignEditor).
		Long(pos).
		Bool(isFront).
		Build())
}

// sendOpenSignEditorToSessions is exported for the server to open sign UI
// when a player places a new sign.
func sendOpenSignEditorToSessions(mgr *session.Manager, p *player.Player, x, y, z int) {
	if mgr == nil {
		return
	}
	sess, ok := mgr.Get(p.UUID)
	if !ok {
		return
	}
	_ = openSignEditor(sess.Conn, x, y, z, true)
}

