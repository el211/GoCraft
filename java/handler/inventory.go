package handler

// Inventory handling for Milestone 10.
//
// Receives C→S Set Held Item and Creative Mode Set Item packets, updates the
// canonical player model, and sends the initial S→C inventory state on join.
//
// The slot wire format changed in 1.20.5 to the component system:
//
//	VarInt  item_count           (0 = empty slot)
//	VarInt  item_type            (numeric registry index; only present if count > 0)
//	VarInt  components_to_add    (ignored in M10; components parsed but skipped)
//	VarInt  components_to_remove (ignored in M10)
//
// Creative Mode Set Item packets are fully framed — unread component bytes are
// dropped at packet end, so partial reads are safe and don't desync the stream.
//
// Packet IDs are resolved from the protocol-769 data table.

import (
	"fmt"
	"log/slog"

	"GoCraft/core/player"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

// ── Dispatch ──────────────────────────────────────────────────────────────────

// handleInventoryPacket dispatches an incoming inventory packet.
// Called from the play loop for the two C→S inventory packet IDs.
func handleInventoryPacket(pkt *protocol.Packet, p *player.Player) error {
	switch pkt.ID {
	case packetIDSetHeldItemCS:
		return handleSetHeldItem(pkt, p)
	case packetIDCreativeModeSetItem:
		return handleCreativeModeSetItem(pkt, p)
	}
	return nil
}

// ── C→S handlers ─────────────────────────────────────────────────────────────

// handleSetHeldItem handles C→S Set Held Item.
//
// Wire layout (1.21.4, estimate):
//
//	Short  slot  (0–8, hotbar slot index)
func handleSetHeldItem(pkt *protocol.Packet, p *player.Player) error {
	r := pkt.Reader()
	slot, err := protocol.ReadShort(r)
	if err != nil {
		return fmt.Errorf("set held item: reading slot: %w", err)
	}
	if slot < 0 || slot > 8 {
		return fmt.Errorf("set held item: slot %d out of range 0-8", slot)
	}
	p.HeldSlot = int(slot)
	slog.Debug("held item changed", "player", p.Username, "slot", slot)
	return nil
}

// handleCreativeModeSetItem handles C→S Creative Mode Set Item.
//
// Wire layout (1.21.4, estimate):
//
//	Short   slot        (inventory slot index; negative = drag to cursor)
//	VarInt  item_count  (0 = empty slot)
//	VarInt  item_type   (numeric item registry index; only present if count > 0)
//	…       components  (ignored; safe to drop — packet is fully framed)
func handleCreativeModeSetItem(pkt *protocol.Packet, p *player.Player) error {
	r := pkt.Reader()

	slot, err := protocol.ReadShort(r)
	if err != nil {
		return fmt.Errorf("creative set item: reading slot: %w", err)
	}

	count, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("creative set item: reading count: %w", err)
	}

	// Slot index -1 is used when the player drags an item to the cursor
	// (outside valid inventory bounds). Ignore those.
	if slot < 0 || int(slot) >= player.InventorySize {
		return nil
	}

	if count <= 0 {
		p.Inventory[slot] = player.ItemStack{} // clear slot
		return nil
	}

	itemType, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("creative set item: reading item type: %w", err)
	}
	// Remaining fields (components_to_add, components_to_remove, component
	// payloads) are beyond what M10 needs; they are dropped at packet end.

	// Map numeric item type to a canonical resource location.
	itemName := javaworld.ItemName(itemType)
	if itemName == "" {
		// Unknown numeric IDs are retained as placeholders. This should only
		// occur when the client and embedded registry versions disagree.
		itemName = fmt.Sprintf("unknown:%d", itemType)
		slog.Debug("creative set item: unknown item type",
			"player", p.Username, "type", itemType, "slot", slot)
	}

	p.Inventory[slot] = player.ItemStack{ItemID: itemName, Count: int(count)}
	slog.Debug("inventory slot set",
		"player", p.Username, "slot", slot, "item", itemName, "count", count)
	return nil
}

// ── S→C senders ──────────────────────────────────────────────────────────────

// sendSetContainerContent sends the full player inventory to the client.
// containerID 0 is always the player's own inventory.
//
// Wire layout (1.21.4, estimate):
//
//	VarInt  container_id
//	VarInt  state_id     (monotonic; client uses this for ack)
//	VarInt  slot_count
//	Slot[]  slots        (one per slot in the container)
//	Slot    carried_item (item currently held on cursor)
//
// Each empty Slot is a single VarInt(0).
// Each non-empty Slot is: VarInt(count) VarInt(item_type) VarInt(0) VarInt(0).
func sendSetContainerContent(conn *network.ClientConn, p *player.Player, stateID int32) error {
	b := protocol.NewBuilder(packetIDSetContainerContent).
		VarInt(0).       // container_id: 0 = player inventory
		VarInt(stateID). // state_id
		VarInt(player.InventorySize)

	for i := 0; i < player.InventorySize; i++ {
		encodeSlot(b, p.Inventory[i])
	}

	// Carried item (cursor) persists while containers open and close.
	encodeSlot(b, p.CarriedItem)

	return conn.WritePacket(b.Build())
}

// sendSetHeldItem sends Set Held Item (S→C) to confirm the active hotbar slot.
//
// Wire layout (1.21.4, estimate):
//
//	VarInt  slot  (0–8)
func sendSetHeldItem(conn *network.ClientConn, slot int) error {
	return conn.WritePacket(
		protocol.NewBuilder(packetIDSetHeldItemSC).VarInt(int32(slot)).Build(),
	)
}

// ── Slot encoding ─────────────────────────────────────────────────────────────

// encodeSlot appends a 1.20.5+ slot encoding to b.
//
//	Empty:     VarInt(0)
//	Non-empty: VarInt(count) VarInt(item_type) VarInt(0/*add*/) VarInt(0/*remove*/)
func encodeSlot(b *protocol.Builder, item player.ItemStack) {
	if item.IsEmpty() {
		b.VarInt(0)
		return
	}
	id := javaworld.ItemID(item.ItemID)
	if id < 0 {
		// Item not in our table — send as empty rather than sending a wrong ID.
		b.VarInt(0)
		return
	}
	b.VarInt(int32(item.Count)).
		VarInt(id).
		VarInt(0). // components_to_add
		VarInt(0)  // components_to_remove
}
