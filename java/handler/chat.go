package handler

// Chat handling for Milestone 7.
//
// Receives C→S Chat Message and Chat Command packets, formats them, and
// broadcasts them as S→C System Chat Message to all online sessions.
// Messages starting with '/' (or arriving via Chat Command) are dispatched
// to the built-in command dispatcher.
//
// All packet IDs are estimates for 1.21.4 (protocol 769); verify against a
// packet capture if the client never shows chat or commands don't fire.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"

	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

// ── Chat limits ───────────────────────────────────────────────────────────────

// maxChatLength is the maximum number of Unicode code points allowed in a chat
// message, matching the vanilla server's enforced limit.
const maxChatLength = 256

// ── Dispatch ──────────────────────────────────────────────────────────────────

// handleChatPacket dispatches an incoming chat or command packet.
// Called from the play loop after handlePlayPacket for packets that need
// access to the session manager.
func handleChatPacket(pkt *protocol.Packet, p *player.Player, mgr *session.Manager, cmds *Dispatcher, w *coreworld.World, conn *network.ClientConn, teleportTo func(x, y, z float64) error) error {
	switch pkt.ID {
	case packetIDChatMessage:
		return handleChatMessage(pkt, p, mgr, cmds, w, conn, teleportTo)
	case packetIDChatCommand:
		return handleChatCommand(pkt, p, mgr, cmds, w, conn, teleportTo)
	}
	return nil
}

// handleChatMessage parses C→S Chat Message and either broadcasts the text or
// dispatches it as a command if the player typed a '/' prefix in chat.
//
// Wire layout (1.21.4, fields after Message are ignored in M7):
//
//	String  message (≤ 256 chars)
//	Long    timestamp
//	Long    salt
//	Bool    has_signature
//	Byte[]  signature (256 bytes, only if has_signature)
//	VarInt  message_count
//	Fixed BitSet (20 bits) acknowledged
func handleChatMessage(pkt *protocol.Packet, p *player.Player, mgr *session.Manager, cmds *Dispatcher, w *coreworld.World, conn *network.ClientConn, teleportTo func(x, y, z float64) error) error {
	r := pkt.Reader()
	msg, err := protocol.ReadString(r)
	if err != nil {
		return fmt.Errorf("reading chat message string: %w", err)
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}
	if len([]rune(msg)) > maxChatLength {
		_ = sendSystemMessage(conn,
			fmt.Sprintf("Message too long (max %d characters)", maxChatLength))
		return nil
	}

	// Some clients still prefix commands with '/' in Chat Message even though
	// 1.19+ has a dedicated Chat Command packet. Handle both paths.
	if strings.HasPrefix(msg, "/") {
		slog.Info("command (via chat)", "player", p.Username, "input", msg)
		cmds.Dispatch(msg, CommandContext{Player: p, Conn: conn, World: w, Manager: mgr, TeleportTo: teleportTo})
		return nil
	}

	text := fmt.Sprintf("<%s> %s", p.Username, msg)
	slog.Info("chat", "player", p.Username, "message", msg)
	broadcastSystemMessage(mgr, text)
	return nil
}

// handleChatCommand parses C→S Chat Command (command without leading '/').
//
// Wire layout (1.21.4, fields after Command are ignored in M7):
//
//	String  command (no leading '/')
//	Long    timestamp
//	Long    salt
//	VarInt  argument_count
//	…       argument signatures × argument_count (ignored)
//	VarInt  message_count
//	Fixed BitSet (20 bits) acknowledged
func handleChatCommand(pkt *protocol.Packet, p *player.Player, mgr *session.Manager, cmds *Dispatcher, w *coreworld.World, conn *network.ClientConn, teleportTo func(x, y, z float64) error) error {
	r := pkt.Reader()
	cmd, err := protocol.ReadString(r)
	if err != nil {
		return fmt.Errorf("reading chat command string: %w", err)
	}
	slog.Info("command", "player", p.Username, "command", cmd)
	cmds.Dispatch(cmd, CommandContext{Player: p, Conn: conn, World: w, Manager: mgr, TeleportTo: teleportTo})
	return nil
}

// ── Send helpers ──────────────────────────────────────────────────────────────

// BroadcastSystemMessage sends a plain-text System Chat Message to every
// currently-online Java session.  Exported for use by the server simulation
// layer (e.g. applyChat, which processes intents from both adapters).
func BroadcastSystemMessage(mgr *session.Manager, text string) {
	broadcastSystemMessage(mgr, text)
}

// broadcastSystemMessage sends a plain-text System Chat Message to every
// currently-online session.  Snapshots the session list so the manager lock
// is not held during packet writes.
func broadcastSystemMessage(mgr *session.Manager, text string) {
	pkt := buildSystemChatMessage(text, false)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
	mgr.ObserveMessage(text)
}

// sendSystemMessage sends a plain-text System Chat Message to one connection.
func sendSystemMessage(conn *network.ClientConn, text string) error {
	if conn == nil {
		return nil
	}
	return conn.WritePacket(buildSystemChatMessage(text, false))
}

// SendSystemMessage is the exported variant of sendSystemMessage for use by
// server-level code that registers commands as closures outside this package.
func SendSystemMessage(conn *network.ClientConn, text string) error {
	return sendSystemMessage(conn, text)
}

// buildSystemChatMessage constructs a System Chat Message (S→C) packet.
//
// Wire layout (1.21.4):
//
//	Text Component (NBT)  content
//	Boolean               overlay  (false = chat box, true = action bar)
func buildSystemChatMessage(text string, overlay bool) *protocol.Packet {
	return protocol.NewBuilder(packetIDSystemChatMessage).
		Bytes(nbtTextComponent(text)).
		Bool(overlay).
		Build()
}

// ── Text component NBT encoder ────────────────────────────────────────────────

// nbtTextComponent encodes a plain text string as a minimal Network NBT text
// component, as required by System Chat Message and other packets in 1.20.3+.
//
// Encoding: a root TAG_Compound (no name — network NBT format) containing a
// single TAG_String field named "text", followed by TAG_End.
//
//	0x0A              TAG_Compound root (no name)
//	0x08 "text" …    TAG_String: name="text", value=text
//	0x00              TAG_End
func nbtTextComponent(text string) []byte {
	var buf bytes.Buffer

	buf.WriteByte(0x0A) // TAG_Compound root
	writeNBTStringEntry(&buf, "text", text)
	buf.WriteByte(0x00) // TAG_End
	return buf.Bytes()
}

// nbtLoreTextComponent encodes an explicitly styled lore line. Item lore is
// dark-purple and italic by default, so both fields must be present to produce
// the compact vanilla-like tooltip used by GoCraft.
func nbtLoreTextComponent(text, color string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x0A) // TAG_Compound root
	writeNBTStringEntry(&buf, "text", text)
	writeNBTStringEntry(&buf, "color", color)
	buf.WriteByte(0x01) // TAG_Byte
	writeNBTString(&buf, "italic")
	buf.WriteByte(0) // false
	buf.WriteByte(0x00)
	return buf.Bytes()
}

func writeNBTStringEntry(buf *bytes.Buffer, name, value string) {
	buf.WriteByte(0x08) // TAG_String
	writeNBTString(buf, name)
	writeNBTString(buf, value)
}

func writeNBTString(buf *bytes.Buffer, value string) {
	nameBytes := []byte(value)
	var nameLen [2]byte
	binary.BigEndian.PutUint16(nameLen[:], uint16(len(nameBytes)))
	buf.Write(nameLen[:])
	buf.Write(nameBytes)
}
