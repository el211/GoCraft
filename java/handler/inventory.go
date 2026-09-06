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
	if p.HeldSlot != int(slot) {
		// Changing slots unloads the crossbow — the player must redraw.
		p.CrossbowLoaded = false
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
// goatHornSounds maps vanilla instrument index 0..7 to the canonical sound event.
var goatHornSounds = [8]string{
	"minecraft:item.goat_horn.sound.0", // ponder
	"minecraft:item.goat_horn.sound.1", // sing
	"minecraft:item.goat_horn.sound.2", // seek
	"minecraft:item.goat_horn.sound.3", // feel
	"minecraft:item.goat_horn.sound.4", // admire
	"minecraft:item.goat_horn.sound.5", // call
	"minecraft:item.goat_horn.sound.6", // yearn
	"minecraft:item.goat_horn.sound.7", // dream
}

// GoatHornSound returns the sound event for a goat horn stack. The instrument
// index is read from the minecraft:instrument component; unknown values fall
// back to index 0 (ponder).
func GoatHornSound(stack player.ItemStack) string {
	var instrument struct {
		Type string `json:"type"`
	}
	if stack.Component("minecraft:instrument", &instrument) {
		for i, s := range goatHornSounds {
			suffix := fmt.Sprintf("minecraft:item.goat_horn.sound.%d", i)
			if instrument.Type == suffix || s == instrument.Type {
				return s
			}
		}
	}
	return goatHornSounds[0]
}

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
	if UseThrowable(p, w, mgr, conn, nextEntityID) {
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	}
	if p.Inventory[heldSlot].ItemID == "minecraft:wind_charge" {
		UseWindCharge(p, w, mgr, conn, nextEntityID)
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	}
	if startJavaFoodUse(p, heldSlot, time.Now()) {
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	}
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
	case "minecraft:crossbow":
		// If the crossbow is already loaded, right-click fires it immediately
		// (vanilla two-step: draw to load, click to fire).
		if p.CrossbowLoaded {
			p.CrossbowLoaded = false
			fireCrossbowLoaded(p, w, mgr, conn, nextEntityID)
			return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
		}
		// Not loaded — start the draw animation.
		p.UsingItemID = "minecraft:crossbow"
		p.UsingItemSince = time.Now()
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:bow", "minecraft:trident":
		p.UsingItemID = p.Inventory[heldSlot].ItemID
		p.UsingItemSince = time.Now()
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:shield":
		p.UsingItemID = "minecraft:shield"
		p.UsingItemSince = time.Now()
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:map":
		// Empty map → filled map. Map ID is derived from a global counter stored
		// in the Damage field of the resulting item (damage=0 → mapID=0, etc.).
		// The client renders the map content separately; for now we just convert
		// the item so it has an item type the client recognises as a map.
		if p.GameMode != player.GameModeSpectator {
			if p.GameMode == player.GameModeCreative {
				// Creative: give a filled map without consuming the original.
				p.GiveItem(player.ItemStack{ItemID: "minecraft:filled_map", Count: 1})
			} else {
				p.Inventory[heldSlot].Count--
				normalizeStack(&p.Inventory[heldSlot])
				p.GiveItem(player.ItemStack{ItemID: "minecraft:filled_map", Count: 1})
			}
			p.ContainerStateID++
			if err := sendSetContainerContent(conn, p, p.ContainerStateID); err != nil {
				return err
			}
		}
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:ender_eye":
		UseEnderEye(p, w, mgr, nextEntityID)
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:goat_horn":
		const goatHornCooldown = 7 * time.Second
		if !p.LastGoatHornUse.IsZero() && time.Since(p.LastGoatHornUse) < goatHornCooldown {
			return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
		}
		p.LastGoatHornUse = time.Now()
		sound := GoatHornSound(p.Inventory[heldSlot])
		broadcastSoundAt(mgr, sound, soundCategoryNeutral,
			p.Position.X, p.Position.Y+1.62, p.Position.Z, 64, 1)
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:spyglass":
		p.UsingItemID = "minecraft:spyglass"
		p.UsingItemSince = time.Now()
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:written_book":
		// Send Open Book packet to trigger the reading UI on the client.
		// Hand 0 = main hand.
		_ = conn.WritePacket(protocol.NewBuilder(packetIDOpenBook).VarInt(0).Build())
		return conn.WritePacket(buildAcknowledgeBlockChange(sequence))
	case "minecraft:carrot_on_a_stick", "minecraft:warped_fungus_on_a_stick":
		// Steering rod: damage the item 7 durability per use when riding.
		if p.VehicleEntityID != 0 && p.GameMode != player.GameModeCreative {
			p.Inventory[heldSlot].ApplyDamage(7)
			normalizeStack(&p.Inventory[heldSlot])
			p.ContainerStateID++
			if err := sendSetContainerContent(conn, p, p.ContainerStateID); err != nil {
				return err
			}
		}
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

// startJavaFoodUse records the selected food stack and lets the Java client
// run its normal use animation. Completion remains server-authoritative.
func startJavaFoodUse(p *player.Player, inventorySlot int, now time.Time) bool {
	if p == nil || p.Dead || p.GameMode == player.GameModeSpectator || inventorySlot < 0 || inventorySlot >= len(p.Inventory) {
		return false
	}
	stack := p.Inventory[inventorySlot]
	if stack.IsEmpty() {
		return false
	}
	_, _, food := player.FoodValue(stack.ItemID)
	if !food && !player.IsConsumable(stack.ItemID) {
		return false
	}
	_, hunger, _, _ := p.HealthSnapshot()
	if p.GameMode != player.GameModeCreative && food && hunger >= 20 && !player.CanAlwaysEat(stack.ItemID) {
		return false
	}
	p.UsingItemID = stack.ItemID
	p.UsingItemSlot = inventorySlot - player.HotbarStart
	p.UsingItemSince = now
	return true
}

// TickJavaFoodUse completes a Java food use once its vanilla duration has
// elapsed. The server tick calls this independently of client packet traffic.
func TickJavaFoodUse(p *player.Player, conn *network.ClientConn, mgr *session.Manager, now time.Time) bool {
	if p == nil || p.UsingItemID == "" || p.UsingItemSince.IsZero() {
		return false
	}
	if !player.IsConsumable(p.UsingItemID) {
		return false
	}
	if p.UsingItemSlot < 0 || p.UsingItemSlot >= 9 || p.HeldSlot != p.UsingItemSlot {
		clearJavaFoodUse(p)
		return false
	}
	slot := player.HotbarStart + p.UsingItemSlot
	stack := p.Inventory[slot]
	if stack.IsEmpty() || stack.ItemID != p.UsingItemID {
		clearJavaFoodUse(p)
		return false
	}
	if now.Sub(p.UsingItemSince) < player.FoodUseDuration(stack.ItemID) {
		return false
	}

	nutrition, saturation, food := player.FoodValue(stack.ItemID)
	consumed := stack
	consumedID := consumed.ItemID
	if p.GameMode != player.GameModeCreative {
		if food && !p.ConsumeFoodAllowFull(nutrition, saturation, player.CanAlwaysEat(consumedID)) {
			clearJavaFoodUse(p)
			return false
		}
		p.Inventory[slot].Count--
		normalizeStack(&p.Inventory[slot])
		if remainder := player.FoodRemainder(consumedID); remainder != "" {
			if p.Inventory[slot].IsEmpty() {
				p.Inventory[slot] = player.ItemStack{ItemID: remainder, Count: 1}
			} else {
				p.GiveItem(player.ItemStack{ItemID: remainder, Count: 1})
			}
		}
		if conn != nil {
			_ = SyncPlayerInventory(conn, p)
		} else {
			p.ContainerStateID++
		}
	}
	clearJavaFoodUse(p)
	applyConsumableEffects(conn, p, consumed, mgr)
	_ = sendUpdateHealth(conn, p)
	BroadcastSoundAt(mgr, "minecraft:entity.generic.eat", soundCategoryPlayers,
		p.Position.X, p.Position.Y+1.5, p.Position.Z, 1, 1)
	BroadcastSoundAt(mgr, "minecraft:entity.player.burp", soundCategoryPlayers,
		p.Position.X, p.Position.Y+1.5, p.Position.Z, 0.5, 1)
	for _, removed := range p.ApplyConsumableCleansing(consumedID) {
		RemoveMobEffect(conn, p, removed.ID)
	}
	return true
}

// SendMobEffect adds or refreshes a status effect on a Java client.
func SendMobEffect(conn *network.ClientConn, p *player.Player, name string, amplifier, durationTicks int32) {
	if conn == nil || p == nil {
		return
	}
	effectID := javaworld.MobEffectID(name)
	if effectID < 0 {
		return
	}
	pkt := protocol.NewBuilder(packetIDUpdateMobEffect).
		VarInt(p.EntityID).
		VarInt(effectID).
		VarInt(amplifier).
		VarInt(durationTicks).
		Byte(0x06).
		Build()
	_ = conn.WritePacket(pkt)
}

// SyncPlayerStatusEffects restores canonical effects after login or respawn.
func SyncPlayerStatusEffects(conn *network.ClientConn, p *player.Player) {
	if conn == nil || p == nil {
		return
	}
	for _, effect := range p.StatusEffectsSnapshot() {
		SendMobEffect(conn, p, effect.ID, effect.Amplifier, effect.Duration)
	}
}

// RemoveMobEffect removes one expired canonical effect from a Java client.
func RemoveMobEffect(conn *network.ClientConn, p *player.Player, name string) {
	if conn == nil || p == nil {
		return
	}
	effectID := javaworld.MobEffectID(name)
	if effectID < 0 {
		return
	}
	_ = conn.WritePacket(protocol.NewBuilder(packetIDRemoveMobEffect).
		VarInt(p.EntityID).
		VarInt(effectID).
		Build())
}

func applyConsumableEffects(conn *network.ClientConn, p *player.Player, stack player.ItemStack, mgr *session.Manager) {
	if p == nil {
		return
	}
	roll := int(p.EntityID*1103515245+12345) & 0x7fffffff
	effects := player.FoodStatusEffects(stack.ItemID, roll%100)
	effects = append(effects, player.SuspiciousStewEffects(stack)...)
	if potion, ok := player.PotionOutcomeFor(stack); ok {
		if potion.Heal > 0 {
			p.Heal(potion.Heal)
		}
		if potion.Damage > 0 {
			DamagePlayerMagic(&session.Session{Player: p, Conn: conn}, potion.Damage, "was killed by magic", mgr)
		}
		effects = append(effects, potion.Effects...)
	}
	for _, effect := range effects {
		if stored, changed := p.AddStatusEffect(effect); changed {
			SendMobEffect(conn, p, stored.ID, stored.Amplifier, stored.Duration)
		}
	}
}

func clearJavaFoodUse(p *player.Player) {
	if p == nil {
		return
	}
	p.UsingItemID = ""
	p.UsingItemSince = time.Time{}
	p.UsingItemSlot = -1
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

// SendPlayerInventory refreshes the Java client after a simulation-thread
// interaction consumes an item. Bedrock inventory is diffed by its normal Sync.
func SendPlayerInventory(p *player.Player, mgr *session.Manager) {
	if p == nil || mgr == nil {
		return
	}
	if current, ok := mgr.Get(p.UUID); ok && current != nil && current.Conn != nil {
		_ = sendSetContainerContent(current.Conn, p, p.ContainerStateID)
	}
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
	enchantments := item.EnchantmentLevels()
	if maxDamage <= 0 {
		componentCount := int32(0)
		if potionID(item) >= 0 {
			componentCount++
		}
		if len(enchantments) > 0 {
			componentCount++
		}
		if item.ItemID == "minecraft:decorated_pot" {
			componentCount++
		}
		if item.ItemID == "minecraft:firework_rocket" {
			componentCount++
		}
		if item.Components != "" {
			componentCount++
		}
		b.VarInt(int32(item.Count)).
			VarInt(id).
			VarInt(componentCount).
			VarInt(0) // components_to_remove
		encodeSlotPotionContents(b, item)
		encodeSlotEnchantments(b, enchantments)
		encodeSlotPotDecorations(b, item)
		encodeSlotFireworks(b, item)
		encodeSlotExtensionComponents(b, item)
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
	if len(enchantments) > 0 {
		componentCount++
	}
	if item.Components != "" {
		componentCount++
	}
	b.VarInt(int32(item.Count)).
		VarInt(id).
		VarInt(componentCount).
		VarInt(0). // components_to_remove
		VarInt(2).VarInt(int32(maxDamage)).
		VarInt(3).VarInt(int32(damage))
	encodeSlotEnchantments(b, enchantments)
	encodeSlotExtensionComponents(b, item)
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

func potionID(item player.ItemStack) int32 {
	name, ok := player.PotionName(item)
	if !ok || name == "" {
		return -1
	}
	return javaworld.PotionID("minecraft:" + name)
}

func encodeSlotPotionContents(b *protocol.Builder, item player.ItemStack) {
	id := potionID(item)
	if id < 0 {
		return
	}
	// Component 41: optional base potion, optional colour, custom effects.
	b.VarInt(41).Bool(true).VarInt(id).Bool(false).VarInt(0)
}

func encodeSlotExtensionComponents(b *protocol.Builder, item player.ItemStack) {
	if item.Components == "" {
		return
	}
	b.VarInt(0).Bytes(nbtGoCraftComponents(item.NormalizedComponents()))
}

func encodeSlotEnchantments(b *protocol.Builder, enchantments []player.EnchantmentLevel) {
	if len(enchantments) == 0 {
		return
	}
	b.VarInt(10).VarInt(int32(len(enchantments)))
	for _, enchantment := range enchantments {
		b.VarInt(javaworld.EnchantmentID(enchantment.ID)).VarInt(int32(enchantment.Level))
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

func encodeSlotPotDecorations(b *protocol.Builder, item player.ItemStack) {
	if item.ItemID != "minecraft:decorated_pot" {
		return
	}
	decorations := item.NormalizedPotDecorations()
	b.VarInt(61).VarInt(int32(len(decorations)))
	for _, decoration := range decorations {
		b.VarInt(javaworld.ItemID(decoration))
	}
}

func encodeSlotFireworks(b *protocol.Builder, item player.ItemStack) {
	if item.ItemID != "minecraft:firework_rocket" {
		return
	}
	data := item.EffectiveFireworks()
	b.VarInt(56).VarInt(int32(data.Flight)).VarInt(int32(data.ExplosionCount))
	for index := range int(data.ExplosionCount) {
		explosion := data.Explosions[index]
		b.VarInt(int32(explosion.Shape)).VarInt(int32(explosion.ColorCount))
		for color := range int(explosion.ColorCount) {
			b.Int(explosion.Colors[color])
		}
		b.VarInt(int32(explosion.FadeColorCount))
		for color := range int(explosion.FadeColorCount) {
			b.Int(explosion.FadeColors[color])
		}
		b.Bool(explosion.Trail).Bool(explosion.Twinkle)
	}
}
