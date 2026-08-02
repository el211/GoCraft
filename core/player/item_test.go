package player

import "testing"

func TestDamageableItemUsesVanillaDurabilityAndBreaks(t *testing.T) {
	item := ItemStack{ItemID: "minecraft:golden_sword", Count: 1}
	if got := MaxDurability(item.ItemID); got != 32 {
		t.Fatalf("golden sword max durability = %d, want 32", got)
	}
	item.ApplyDamage(31)
	if item.IsEmpty() || item.RemainingDurability() != 1 {
		t.Fatalf("item after 31 damage = %+v, want one durability", item)
	}
	if !item.ApplyDamage(1) || !item.IsEmpty() {
		t.Fatalf("final use did not break item: %+v", item)
	}
}

func TestArmorPointsAndDamageableStacks(t *testing.T) {
	p := &Player{}
	p.Inventory[5] = ItemStack{ItemID: "minecraft:golden_helmet", Count: 1}
	p.Inventory[6] = ItemStack{ItemID: "minecraft:golden_chestplate", Count: 1}
	p.Inventory[7] = ItemStack{ItemID: "minecraft:golden_leggings", Count: 1}
	p.Inventory[8] = ItemStack{ItemID: "minecraft:golden_boots", Count: 1}
	if got := p.ArmorPoints(); got != 11 {
		t.Fatalf("gold armour points = %d, want 11", got)
	}

	inventory := &Player{}
	if !inventory.GiveItem(ItemStack{ItemID: "minecraft:iron_sword", Count: 2}) {
		t.Fatal("two swords did not fit in empty inventory")
	}
	if inventory.Inventory[HotbarStart].Count != 1 || inventory.Inventory[HotbarStart+1].Count != 1 {
		t.Fatal("damageable swords were incorrectly stacked")
	}
}
