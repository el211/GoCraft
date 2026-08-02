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
	"sync/atomic"
	"time"

	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

type itemTooltipOptions struct {
	showDurability        bool
	showAttributes        bool
	hideVanillaAttributes bool
	legacyCombat          bool
}

var configuredItemTooltips atomic.Value

func init() {
	configuredItemTooltips.Store(itemTooltipOptions{
		showDurability:        true,
		showAttributes:        true,
		hideVanillaAttributes: true,
	})
}

// ConfigureItemTooltips installs the server-wide presentation used for all
// outgoing item stacks. The gameplay attributes remain authoritative even
// when their vanilla tooltip section is hidden.
func ConfigureItemTooltips(showDurability, showAttributes, hideVanillaAttributes, legacyCombat bool) {
	configuredItemTooltips.Store(itemTooltipOptions{
		showDurability:        showDurability,
		showAttributes:        showAttributes,
		hideVanillaAttributes: hideVanillaAttributes,
		legacyCombat:          legacyCombat,
	})
}

// ── Dispatch ──────────────────────────────────────────────────────────────────

// handleInventoryPacket dispatches an incoming inventory packet.
// Called from the play loop for the two C→S inventory packet IDs.
func handleInventoryPacket(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn) error {
	switch pkt.ID {
	case packetIDSetHeldItemCS:
		return handleSetHeldItem(pkt, p)
	case packetIDCreativeModeSetItem:
		before := p.ArmorPoints()
		if err := handleCreativeModeSetItem(pkt, p); err != nil {
			return err
		}
		if p.ArmorPoints() != before {
			return sendArmorAttributes(conn, p)
		}
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

	// Slot index -1 is used when the player drags an item to the cursor
	// (outside valid inventory bounds). Ignore those.
	if slot < 0 || int(slot) >= player.InventorySize {
		return nil
	}
	item, err := readPlainSlot(r)
	if err != nil {
		return fmt.Errorf("creative set item: reading item: %w", err)
	}
	p.Inventory[slot] = item
	slog.Debug("inventory slot set",
		"player", p.Username, "slot", slot, "item", item.ItemID, "count", item.Count)
	return nil
}

// handleUseItem equips armour from the selected hotbar slot when the player
// right-clicks it. This prevents the client-only predicted equip from being
// rolled back and keeps the armour attribute authoritative.
func handleUseItem(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn, w *coreworld.World, mgr *session.Manager, nextEntityID func() int32) error {
	r := pkt.Reader()
	hand, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("use item: reading hand: %w", err)
	}
	sequence, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("use item: reading sequence: %w", err)
	}
	yaw, err := protocol.ReadFloat(r)
	if err != nil {
		return fmt.Errorf("use item: reading yaw: %w", err)
	}
	pitch, err := protocol.ReadFloat(r)
	if err != nil {
		return fmt.Errorf("use item: reading pitch: %w", err)
	}
	p.Rotation.Yaw, p.Rotation.Pitch = yaw, pitch
	if r.Len() != 0 {
		return fmt.Errorf("use item: %d trailing bytes", r.Len())
	}
	if hand != 0 {
		_ = conn.WritePacket(buildAcknowledgeBlockChange(sequence))
		return nil
	}
	heldSlot := player.HotbarStart + p.HeldSlot
	if isBoatItem(p.Inventory[heldSlot].ItemID) &&
		p.GameMode != player.GameModeAdventure && p.GameMode != player.GameModeSpectator {
		if boat := spawnBoatFromLook(p, w, nextEntityID); boat != nil {
			BroadcastSpawnMob(boat, mgr)
			consumePlacedBoat(p, conn)
			slog.Info("player placed boat", "player", p.Username, "type", boat.Type)
		}
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	}
	switch p.Inventory[heldSlot].ItemID {
	case "minecraft:bow", "minecraft:crossbow", "minecraft:trident":
		p.UsingItemID = p.Inventory[heldSlot].ItemID
		p.UsingItemSince = time.Now()
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	}
	armorSlot := armorInventorySlot(p.Inventory[heldSlot].ItemID)
	if armorSlot < 5 {
		_ = conn.WritePacket(buildAcknowledgeBlockChange(sequence))
		return nil
	}
	before := p.ArmorPoints()
	p.Inventory[heldSlot], p.Inventory[armorSlot] = p.Inventory[armorSlot], p.Inventory[heldSlot]
	p.ContainerStateID++
	if err := sendSetContainerContent(conn, p, p.ContainerStateID); err != nil {
		return err
	}
	if p.ArmorPoints() != before {
		if err := sendArmorAttributes(conn, p); err != nil {
			return err
		}
	}
	return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
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
	maxDamage := player.MaxDurability(item.ItemID)
	if maxDamage <= 0 {
		b.VarInt(int32(item.Count)).
			VarInt(id).
			VarInt(0). // components_to_add
			VarInt(0)  // components_to_remove
		return
	}
	damage := item.Damage
	if damage < 0 {
		damage = 0
	} else if damage >= maxDamage {
		damage = maxDamage - 1
	}
	options := configuredItemTooltips.Load().(itemTooltipOptions)
	type loreLine struct {
		text  string
		color string
	}
	lore := make([]loreLine, 0, 7)
	if attackDamage, attackSpeed, ok := player.AttackAttributes(item.ItemID); ok && options.showAttributes {
		speedText := fmt.Sprintf("%g", attackSpeed)
		if options.legacyCombat {
			speedText = "Instant"
		}
		lore = append(lore,
			loreLine{"", "gray"},
			loreLine{"When in Main Hand:", "gray"},
			loreLine{fmt.Sprintf(" %g Attack Damage", attackDamage), "dark_green"},
			loreLine{" " + speedText + " Attack Speed", "dark_green"})
	}
	if armour := player.ArmorPoints(item.ItemID); armour > 0 && options.showAttributes {
		lore = append(lore,
			loreLine{"", "gray"},
			loreLine{armorTooltipHeading(item.ItemID), "gray"},
			loreLine{fmt.Sprintf(" %d Armor", armour), "blue"})
		if toughness := player.ArmorToughness(item.ItemID); toughness > 0 {
			lore = append(lore, loreLine{fmt.Sprintf(" %g Armor Toughness", toughness), "blue"})
		}
		if resistance := player.ArmorKnockbackResistance(item.ItemID); resistance > 0 {
			lore = append(lore, loreLine{fmt.Sprintf(" %g%% Knockback Resistance", resistance*100), "blue"})
		}
	}
	if options.showDurability {
		remaining := maxDamage - damage
		color := "green"
		if remaining*5 <= maxDamage {
			color = "red"
		} else if remaining*2 <= maxDamage {
			color = "yellow"
		}
		lore = append(lore, loreLine{fmt.Sprintf("Durability: %d / %d", remaining, maxDamage), color})
	}
	componentCount := int32(2) // max_damage + damage
	if len(lore) > 0 {
		componentCount++
	}
	if options.hideVanillaAttributes {
		componentCount++
	}
	b.VarInt(int32(item.Count)).
		VarInt(id).
		VarInt(componentCount).
		VarInt(0). // components_to_remove
		VarInt(2).VarInt(int32(maxDamage)).
		VarInt(3).VarInt(int32(damage))
	if len(lore) > 0 {
		b.VarInt(8).VarInt(int32(len(lore)))
		for _, line := range lore {
			b.Bytes(nbtLoreTextComponent(line.text, line.color))
		}
	}
	if options.hideVanillaAttributes {
		// Component 13 overrides the item's default attribute list with an empty
		// non-displayed list. This hides the client-visible 1024 attack speed;
		// actual damage, armour, and cooldown remain server-authoritative.
		b.VarInt(13).VarInt(0).Bool(false)
	}
}

func armorTooltipHeading(itemID string) string {
	switch armorInventorySlot(itemID) {
	case 5:
		return "When on Head:"
	case 6:
		return "When on Body:"
	case 7:
		return "When on Legs:"
	case 8:
		return "When on Feet:"
	default:
		return "When Worn:"
	}
}

// SyncPlayerInventory refreshes every player slot after server-side changes
// such as picking up a dropped item.
func SyncPlayerInventory(conn *network.ClientConn, p *player.Player) error {
	p.ContainerStateID++
	return sendSetContainerContent(conn, p, p.ContainerStateID)
}

// damageHeldItem consumes one use and immediately refreshes the visible item.
func damageHeldItem(p *player.Player, conn *network.ClientConn, amount int) bool {
	if p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator {
		return false
	}
	slot := player.HotbarStart + p.HeldSlot
	if player.MaxDurability(p.Inventory[slot].ItemID) == 0 {
		return false
	}
	p.Inventory[slot].ApplyDamage(amount)
	p.ContainerStateID++
	if conn != nil {
		_ = sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	return true
}

// DamagePlayerArmor consumes durability on every equipped armour piece after a
// server-side hit and refreshes both the items and HUD if a piece breaks.
func DamagePlayerArmor(p *player.Player, conn *network.ClientConn, amount int) {
	if p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator || amount <= 0 {
		return
	}
	before := p.ArmorPoints()
	changed := false
	for slot := 5; slot <= 8; slot++ {
		if player.MaxDurability(p.Inventory[slot].ItemID) == 0 {
			continue
		}
		p.Inventory[slot].ApplyDamage(amount)
		changed = true
	}
	if !changed {
		return
	}
	p.ContainerStateID++
	if conn != nil {
		_ = sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	if p.ArmorPoints() != before {
		if conn != nil {
			_ = sendArmorAttributes(conn, p)
		}
	}
}
