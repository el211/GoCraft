package player

import "strings"

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
}

// IsEmpty reports whether the slot contains no item.
func (s ItemStack) IsEmpty() bool {
	return s.Count <= 0 || s.ItemID == ""
}

// MaxDurability returns Java Edition's vanilla maximum durability for the
// damageable items GoCraft currently supports. A zero result means the item is
// not damageable.
func MaxDurability(itemID string) int {
	switch itemID {
	case "minecraft:leather_helmet":
		return 55
	case "minecraft:leather_chestplate":
		return 80
	case "minecraft:leather_leggings":
		return 75
	case "minecraft:leather_boots":
		return 65
	case "minecraft:chainmail_helmet", "minecraft:iron_helmet":
		return 165
	case "minecraft:chainmail_chestplate", "minecraft:iron_chestplate":
		return 240
	case "minecraft:chainmail_leggings", "minecraft:iron_leggings":
		return 225
	case "minecraft:chainmail_boots", "minecraft:iron_boots":
		return 195
	case "minecraft:golden_helmet":
		return 77
	case "minecraft:golden_chestplate":
		return 112
	case "minecraft:golden_leggings":
		return 105
	case "minecraft:golden_boots":
		return 91
	case "minecraft:diamond_helmet":
		return 363
	case "minecraft:diamond_chestplate":
		return 528
	case "minecraft:diamond_leggings":
		return 495
	case "minecraft:diamond_boots":
		return 429
	case "minecraft:netherite_helmet":
		return 407
	case "minecraft:netherite_chestplate":
		return 592
	case "minecraft:netherite_leggings":
		return 555
	case "minecraft:netherite_boots":
		return 481
	case "minecraft:turtle_helmet":
		return 275
	case "minecraft:bow":
		return 384
	case "minecraft:crossbow":
		return 465
	case "minecraft:trident":
		return 250
	case "minecraft:shield":
		return 336
	case "minecraft:fishing_rod", "minecraft:flint_and_steel", "minecraft:brush":
		return 64
	case "minecraft:shears":
		return 238
	case "minecraft:elytra":
		return 432
	case "minecraft:mace":
		return 500
	case "minecraft:carrot_on_a_stick":
		return 25
	case "minecraft:warped_fungus_on_a_stick":
		return 100
	case "minecraft:wolf_armor":
		return 64
	}
	for material, durability := range map[string]int{
		"wooden": 59, "stone": 131, "iron": 250,
		"golden": 32, "diamond": 1561, "netherite": 2031,
	} {
		prefix := "minecraft:" + material + "_"
		if !strings.HasPrefix(itemID, prefix) {
			continue
		}
		switch strings.TrimPrefix(itemID, prefix) {
		case "sword", "pickaxe", "axe", "shovel", "hoe":
			return durability
		}
	}
	return 0
}

// MaxStackSize returns the stack limit used by the inventory implementation.
func MaxStackSize(itemID string) int {
	if MaxDurability(itemID) > 0 {
		return 1
	}
	return 64
}

// RemainingDurability returns remaining uses, or zero for non-damageable items.
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
	switch itemID {
	case "minecraft:leather_helmet", "minecraft:leather_boots", "minecraft:golden_boots",
		"minecraft:chainmail_boots":
		return 1
	case "minecraft:leather_leggings", "minecraft:golden_helmet", "minecraft:chainmail_helmet",
		"minecraft:iron_helmet", "minecraft:iron_boots", "minecraft:turtle_helmet":
		return 2
	case "minecraft:leather_chestplate", "minecraft:golden_leggings", "minecraft:diamond_helmet",
		"minecraft:diamond_boots", "minecraft:netherite_helmet", "minecraft:netherite_boots":
		return 3
	case "minecraft:chainmail_leggings":
		return 4
	case "minecraft:golden_chestplate", "minecraft:chainmail_chestplate", "minecraft:iron_leggings":
		return 5
	case "minecraft:iron_chestplate", "minecraft:diamond_leggings", "minecraft:netherite_leggings":
		return 6
	case "minecraft:diamond_chestplate", "minecraft:netherite_chestplate":
		return 8
	}
	return 0
}

// ArmorToughness returns the vanilla toughness supplied by one armour piece.
func ArmorToughness(itemID string) float32 {
	if strings.HasPrefix(itemID, "minecraft:diamond_") {
		return 2
	}
	if strings.HasPrefix(itemID, "minecraft:netherite_") {
		return 3
	}
	return 0
}

// ArmorKnockbackResistance returns the vanilla knockback resistance supplied
// by one armour piece. Netherite contributes 0.1 per equipped piece.
func ArmorKnockbackResistance(itemID string) float32 {
	if strings.HasPrefix(itemID, "minecraft:netherite_") {
		return 0.1
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

// AttackAttributes returns the 1.21.4 attack damage and speed shown by vanilla
// for a tool or weapon. The bool is false for items without attack modifiers.
func AttackAttributes(itemID string) (damage, speed float32, ok bool) {
	materialDamage := func(material string) float32 {
		switch material {
		case "wooden", "golden":
			return 0
		case "stone":
			return 1
		case "iron":
			return 2
		case "diamond":
			return 3
		case "netherite":
			return 4
		}
		return 0
	}
	for _, material := range []string{"wooden", "stone", "iron", "golden", "diamond", "netherite"} {
		prefix := "minecraft:" + material + "_"
		if !strings.HasPrefix(itemID, prefix) {
			continue
		}
		bonus := materialDamage(material)
		switch strings.TrimPrefix(itemID, prefix) {
		case "sword":
			return 4 + bonus, 1.6, true
		case "shovel":
			return 2.5 + bonus, 1, true
		case "pickaxe":
			return 2 + bonus, 1.2, true
		case "axe":
			damage = map[string]float32{"wooden": 7, "stone": 9, "iron": 9, "golden": 7, "diamond": 9, "netherite": 10}[material]
			speed = map[string]float32{"wooden": 0.8, "stone": 0.8, "iron": 0.9, "golden": 1, "diamond": 1, "netherite": 1}[material]
			return damage, speed, true
		case "hoe":
			speed = map[string]float32{"wooden": 1, "stone": 2, "iron": 3, "golden": 1, "diamond": 4, "netherite": 4}[material]
			return 1, speed, true
		}
	}
	switch itemID {
	case "minecraft:trident":
		return 9, 1.1, true
	case "minecraft:mace":
		return 6, 0.6, true
	}
	return 0, 0, false
}

// BlockUseDamage returns how much durability a successful block-breaking use
// consumes. Swords take two durability when used to break blocks.
func BlockUseDamage(itemID string) int {
	if strings.HasSuffix(itemID, "_sword") {
		return 2
	}
	if MaxDurability(itemID) > 0 {
		return 1
	}
	return 0
}
