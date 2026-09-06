package player

import (
	"encoding/json"

	"GoCraft/core/itemregistry"
)

// InventorySize is the total number of slots in a Java Edition player inventory.
//
// Slot layout:
//
//	0      crafting output
//	1–4    2×2 crafting grid
//	5–8    armour (helmet, chestplate, leggings, boots)
//	9–35   main inventory (3 rows × 9)
//	36–44  hotbar (9 slots; index = HotbarStart + heldSlot)
//	45     off-hand
const InventorySize = 46

// HotbarStart is the slot index of the first hotbar slot.
const HotbarStart = 36

// OffhandSlot is separate from the 36-slot pickup/storage inventory. It must
// never be considered an empty destination by GiveItem: Bedrock only permits a
// limited set of items there, and server-forcing arbitrary drops into it makes
// them appear stuck or desynchronised in the client UI.
const OffhandSlot = 45

const (
	MaxFireworkExplosions = 7
	MaxFireworkColors     = 8
)

// FireworkExplosion is a bounded, comparable representation of one vanilla
// firework explosion component. Shape uses the vanilla 0-4 shape IDs.
type FireworkExplosion struct {
	Shape          uint8
	Colors         [MaxFireworkColors]int32
	ColorCount     uint8
	FadeColors     [MaxFireworkColors]int32
	FadeColorCount uint8
	Trail          bool
	Twinkle        bool
}

// FireworkData is the canonical minecraft:fireworks component shared by both
// protocol adapters and the server-side rocket entity.
type FireworkData struct {
	Flight         uint8
	ExplosionCount uint8
	Explosions     [MaxFireworkExplosions]FireworkExplosion
}

// ItemStack is a quantity of one item type occupying a single inventory slot.
// A zero-value ItemStack (or one with Count ≤ 0) represents an empty slot.
//
// ItemID uses the Minecraft resource-location format ("namespace:name"),
// e.g. "minecraft:stone".  Edition-specific numeric item IDs are resolved at
// the Java adapter boundary (java/world/items.go) and are not stored here.
type ItemStack struct {
	// ItemID is the canonical resource location of the item, e.g. "minecraft:stone".
	// Empty string means the slot is empty.
	ItemID string
	// Count is the number of items in the stack.
	Count int
	// Damage is durability already consumed. New items start at zero and break
	// when Damage reaches MaxDurability(ItemID).
	Damage int
	// Enchantments stores sorted resource-location/level pairs as a compact,
	// comparable canonical component string.
	Enchantments string `json:",omitempty"`
	// PotDecorations stores the four side decorations of a decorated-pot item.
	// An array keeps ItemStack comparable, which is important for inventory diffing.
	PotDecorations [4]string `json:",omitempty"`
	// HasFireworks distinguishes a decoded component from an absent one. The
	// effective vanilla default for a rocket is flight duration one.
	HasFireworks bool         `json:",omitempty"`
	Fireworks    FireworkData `json:",omitempty"`
	// Components stores additional edition-independent data components as a
	// canonical JSON object. A string keeps ItemStack comparable for the fixed
	// inventory snapshots used by both protocol adapters. Components with
	// dedicated hot-path fields above remain there until their codecs migrate.
	Components string `json:",omitempty"`
}

// IsEmpty reports whether the slot contains no item.
func (s ItemStack) IsEmpty() bool {
	return s.Count <= 0 || s.ItemID == ""
}

// NormalizePotDecorations returns the complete four-side decoration list.
// Vanilla treats absent entries as bricks.
func NormalizePotDecorations(decorations [4]string) [4]string {
	for index := range decorations {
		if decorations[index] == "" {
			decorations[index] = "minecraft:brick"
		}
	}
	return decorations
}

// NormalizedPotDecorations returns the meaningful decorated-pot component.
func (s ItemStack) NormalizedPotDecorations() [4]string {
	if s.ItemID != "minecraft:decorated_pot" {
		return [4]string{}
	}
	return NormalizePotDecorations(s.PotDecorations)
}

// SameItem reports whether two stacks may merge without losing components.
func (s ItemStack) SameItem(other ItemStack) bool {
	return s.ItemID == other.ItemID && s.Damage == other.Damage && s.Enchantments == other.Enchantments &&
		s.NormalizedPotDecorations() == other.NormalizedPotDecorations() &&
		s.EffectiveFireworks() == other.EffectiveFireworks() &&
		s.NormalizedComponents() == other.NormalizedComponents()
}

// EffectiveFireworks returns a validated component. Vanilla rockets without
// an explicit override inherit flight duration one and no explosions.
func (s ItemStack) EffectiveFireworks() FireworkData {
	if s.ItemID != "minecraft:firework_rocket" {
		return FireworkData{}
	}
	data := s.Fireworks
	if !s.HasFireworks {
		data.Flight = 1
	}
	if data.ExplosionCount > MaxFireworkExplosions {
		data.ExplosionCount = MaxFireworkExplosions
	}
	for index := range int(data.ExplosionCount) {
		if data.Explosions[index].Shape > 4 {
			data.Explosions[index].Shape = 0
		}
		if data.Explosions[index].ColorCount > MaxFireworkColors {
			data.Explosions[index].ColorCount = MaxFireworkColors
		}
		if data.Explosions[index].FadeColorCount > MaxFireworkColors {
			data.Explosions[index].FadeColorCount = MaxFireworkColors
		}
	}
	return data
}

// MaxDurability returns the canonical vanilla maximum durability. A zero
// result means the item is unknown or is not damageable.
func MaxDurability(itemID string) int {
	if definition, ok := itemregistry.Lookup(itemID); ok {
		return definition.MaxDurability
	}
	return 0
}

// MaxStackSize returns the stack limit used by the inventory implementation.
func MaxStackSize(itemID string) int {
	if definition, ok := itemregistry.Lookup(itemID); ok {
		return definition.MaxStackSize
	}
	// Unknown/custom items retain the historical default until the custom item
	// manager registers definitions directly.
	return 64
}

// RemainingDurability returns remaining uses, or zero for non-damageable items.
// DisplayName returns the plain-text custom name stored in the
// minecraft:custom_name component, or "" when the stack has no custom name.
// It handles both raw strings and JSON text-component objects.
func (s ItemStack) DisplayName() string {
	var raw json.RawMessage
	if !s.Component("minecraft:custom_name", &raw) {
		return ""
	}
	// Try plain string first.
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	// Try {"text":"..."} text component.
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Text
	}
	return ""
}

func (s ItemStack) RemainingDurability() int {
	max := MaxDurability(s.ItemID)
	if max == 0 {
		return 0
	}
	remaining := max - s.Damage
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ApplyDamage consumes durability and reports whether the stack broke.
func (s *ItemStack) ApplyDamage(amount int) bool {
	max := MaxDurability(s.ItemID)
	if max == 0 || amount <= 0 || s.IsEmpty() {
		return false
	}
	// Unbreaking: each point reduces damage by ~1/(level+1) on average.
	// We reduce the amount directly for simplicity (no per-hit RNG).
	if lvl := s.EnchantmentLevel("minecraft:unbreaking"); lvl > 0 {
		if reduced := amount / (lvl + 1); reduced > 0 {
			amount = reduced
		} else {
			amount = 1
		}
	}
	s.Damage += amount
	if s.Damage < max {
		return false
	}
	s.Count--
	if s.Count <= 0 {
		*s = ItemStack{}
	} else {
		s.Damage = 0
	}
	return true
}

// ArmorPoints returns the vanilla armour value contributed by an armour item.
func ArmorPoints(itemID string) int {
	if definition, ok := itemregistry.Lookup(itemID); ok && definition.Equipment != nil {
		return definition.Equipment.Armor
	}
	return 0
}

// ArmorToughness returns the vanilla toughness supplied by one armour piece.
func ArmorToughness(itemID string) float32 {
	if definition, ok := itemregistry.Lookup(itemID); ok && definition.Equipment != nil {
		return definition.Equipment.Toughness
	}
	return 0
}

// ArmorKnockbackResistance returns the vanilla knockback resistance supplied
// by one armour piece. Netherite contributes 0.1 per equipped piece.
func ArmorKnockbackResistance(itemID string) float32 {
	if definition, ok := itemregistry.Lookup(itemID); ok && definition.Equipment != nil {
		return definition.Equipment.KnockbackResistance
	}
	return 0
}

// LegacyAttackDamage returns total melee damage for pre-1.9-style combat.
// Newer materials are extended consistently from the old progression.
func LegacyAttackDamage(itemID string) float32 {
	switch itemID {
	case "minecraft:wooden_sword", "minecraft:golden_sword":
		return 4
	case "minecraft:stone_sword":
		return 5
	case "minecraft:iron_sword":
		return 6
	case "minecraft:diamond_sword":
		return 7
	case "minecraft:netherite_sword":
		return 8
	case "minecraft:wooden_axe", "minecraft:golden_axe":
		return 3
	case "minecraft:stone_axe":
		return 4
	case "minecraft:iron_axe":
		return 5
	case "minecraft:diamond_axe":
		return 6
	case "minecraft:netherite_axe":
		return 7
	case "minecraft:trident":
		return 9
	case "minecraft:mace":
		return 6
	default:
		return 1
	}
}

// IsSword reports whether the item is a sword (eligible for sweep attack).
func IsSword(itemID string) bool {
	switch itemID {
	case "minecraft:wooden_sword", "minecraft:golden_sword",
		"minecraft:stone_sword", "minecraft:iron_sword",
		"minecraft:diamond_sword", "minecraft:netherite_sword":
		return true
	}
	return false
}

// AttackAttributes returns the 1.21.4 attack damage and speed shown by vanilla
// for a tool or weapon. The bool is false for items without attack modifiers.
func AttackAttributes(itemID string) (damage, speed float32, ok bool) {
	if definition, found := itemregistry.Lookup(itemID); found && definition.Combat != nil {
		return definition.Combat.AttackDamage, definition.Combat.AttackSpeed, true
	}
	return 0, 0, false
}

// BlockUseDamage returns how much durability a successful block-breaking use
// consumes. Swords take two durability when used to break blocks.
func BlockUseDamage(itemID string) int {
	if definition, ok := itemregistry.Lookup(itemID); ok {
		if definition.Tool != nil {
			return definition.Tool.BlockDamageCost
		}
		if definition.MaxDurability > 0 {
			return 1
		}
	}
	return 0
}
