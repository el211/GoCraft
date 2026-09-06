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

	// Preserve existing glowing/color state across text edits.
	existing := w.GetBlockEntity(bx, by, bz)
	state := coreworld.SignState{
		FrontGlowing: existing.SignFrontGlowing,
		BackGlowing:  existing.SignBackGlowing,
		FrontColor:   existing.SignFrontColor,
		BackColor:    existing.SignBackColor,
	}
	if isFront {
		state.FrontLines = lines
		state.BackLines = existing.SignBackLines
	} else {
		state.FrontLines = existing.SignFrontLines
		state.BackLines = lines
	}
	data := buildSignNBTFull(lines, isFront, state.FrontGlowing, state.BackGlowing, state.FrontColor, state.BackColor)
	w.SetBlockEntitySign(bx, by, bz, data, state)
	entity := w.GetBlockEntity(bx, by, bz)
	BroadcastBlockEntityDataInDimension(entity, mgr, p.Dimension)
	return nil
}

// IsSignBlock returns true for any standing or wall sign block.
func IsSignBlock(name string) bool {
	return strings.HasSuffix(name, "_sign") || strings.HasSuffix(name, "_wall_sign") ||
		strings.HasSuffix(name, "_hanging_sign") || strings.HasSuffix(name, "_wall_hanging_sign")
}

// isSignBlock is the package-local alias.
func isSignBlock(name string) bool { return IsSignBlock(name) }

// decodeBlockPos unpacks a 64-bit packed block position into x, y, z.
func decodeBlockPos(pos int64) (x, y, z int) {
	x = int(pos >> 38)
	y = int(pos << 52 >> 52)
	z = int(pos << 26 >> 38)
	return
}

// buildSignNBTFull encodes sign lines into a minimal network-NBT compound
// matching the Java 1.21.4 sign block entity format, preserving glowing and
// color state for both faces.
func buildSignNBTFull(lines [4]string, isFront bool, frontGlowing, backGlowing bool, frontColor, backColor string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x0A) // root TAG_Compound (no name in network NBT)
	if isFront {
		writeSignTextNBT(&buf, "front_text", lines, frontGlowing, frontColor)
		writeSignEmptyNBT(&buf, "back_text", backGlowing, backColor)
	} else {
		writeSignEmptyNBT(&buf, "front_text", frontGlowing, frontColor)
		writeSignTextNBT(&buf, "back_text", lines, backGlowing, backColor)
	}
	buf.WriteByte(0x00) // TAG_End
	return buf.Bytes()
}

// buildSignNBT is a convenience wrapper that builds sign NBT with no color or glowing.
func buildSignNBT(lines [4]string, isFront bool) []byte {
	return buildSignNBTFull(lines, isFront, false, false, "", "")
}

func writeSignTextNBT(buf *bytes.Buffer, key string, lines [4]string, glowing bool, color string) {
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

	// "has_glowing_text" TAG_Byte
	buf.WriteByte(0x01) // TAG_Byte
	writeNBTKey(buf, "has_glowing_text")
	if glowing {
		buf.WriteByte(0x01)
	} else {
		buf.WriteByte(0x00)
	}

	// "color" TAG_String (only written when non-default)
	if color != "" {
		buf.WriteByte(0x08) // TAG_String
		writeNBTKey(buf, "color")
		binary.Write(buf, binary.BigEndian, uint16(len(color))) //nolint:errcheck
		buf.WriteString(color)
	}

	buf.WriteByte(0x00) // TAG_End closes the compound
}

func writeSignEmptyNBT(buf *bytes.Buffer, key string, glowing bool, color string) {
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
	if glowing {
		buf.WriteByte(0x01)
	} else {
		buf.WriteByte(0x00)
	}

	if color != "" {
		buf.WriteByte(0x08)
		writeNBTKey(buf, "color")
		binary.Write(buf, binary.BigEndian, uint16(len(color))) //nolint:errcheck
		buf.WriteString(color)
	}

	buf.WriteByte(0x00)
}

func writeNBTKey(buf *bytes.Buffer, key string) {
	binary.Write(buf, binary.BigEndian, uint16(len(key))) //nolint:errcheck
	buf.WriteString(key)
}

// BuildSignNBTFromState builds the full sign NBT from a SignState (both faces).
// Exported for use by Bedrock sign interactions.
func BuildSignNBTFromState(state coreworld.SignState) []byte {
	return buildSignNBTFromState(state)
}

func buildSignNBTFromState(state coreworld.SignState) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x0A) // root TAG_Compound
	writeSignTextNBT(&buf, "front_text", state.FrontLines, state.FrontGlowing, state.FrontColor)
	writeSignTextNBT(&buf, "back_text", state.BackLines, state.BackGlowing, state.BackColor)
	buf.WriteByte(0x00) // TAG_End
	return buf.Bytes()
}

// SignDyeColor maps a dye item ID to the vanilla sign color name.
// Returns "" when the item is not a dye.
func SignDyeColor(itemID string) string { return signDyeColor(itemID) }

func signDyeColor(itemID string) string {
	switch itemID {
	case "minecraft:black_dye":
		return "black"
	case "minecraft:red_dye":
		return "dark_red"
	case "minecraft:green_dye":
		return "dark_green"
	case "minecraft:brown_dye":
		return "dark_gray"
	case "minecraft:blue_dye":
		return "dark_blue"
	case "minecraft:purple_dye":
		return "dark_purple"
	case "minecraft:cyan_dye":
		return "dark_aqua"
	case "minecraft:light_gray_dye":
		return "gray"
	case "minecraft:gray_dye":
		return "dark_gray"
	case "minecraft:pink_dye":
		return "light_purple"
	case "minecraft:lime_dye":
		return "green"
	case "minecraft:yellow_dye":
		return "yellow"
	case "minecraft:light_blue_dye":
		return "aqua"
	case "minecraft:magenta_dye":
		return "light_purple"
	case "minecraft:orange_dye":
		return "gold"
	case "minecraft:white_dye":
		return "white"
	default:
		return ""
	}
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

