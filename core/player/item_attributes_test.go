package player

import "testing"

func TestLegacyWeaponDamageAndModernAttributes(t *testing.T) {
	if got := LegacyAttackDamage("minecraft:diamond_sword"); got != 7 {
		t.Fatalf("legacy diamond sword damage = %v, want 7", got)
	}
	if got := LegacyAttackDamage("minecraft:diamond_axe"); got != 6 {
		t.Fatalf("legacy diamond axe damage = %v, want 6", got)
	}
	damage, speed, ok := AttackAttributes("minecraft:netherite_pickaxe")
	if !ok || damage != 6 || speed != 1.2 {
		t.Fatalf("netherite pickaxe attributes = (%v,%v,%v), want (6,1.2,true)", damage, speed, ok)
	}
	if got := BlockUseDamage("minecraft:iron_sword"); got != 2 {
		t.Fatalf("sword block wear = %d, want 2", got)
	}
}

func TestNetheriteArmorSecondaryAttributes(t *testing.T) {
	p := New([16]byte{}, "armoured", ClientEditionJava)
	for slot, item := range []string{
		"minecraft:netherite_helmet", "minecraft:netherite_chestplate",
		"minecraft:netherite_leggings", "minecraft:netherite_boots",
	} {
		p.Inventory[5+slot] = ItemStack{ItemID: item, Count: 1}
	}
	if got := p.ArmorPoints(); got != 20 {
		t.Fatalf("armour = %d, want 20", got)
	}
	if got := p.ArmorToughness(); got != 12 {
		t.Fatalf("toughness = %v, want 12", got)
	}
	if got := p.KnockbackResistance(); got < 0.399 || got > 0.401 {
		t.Fatalf("knockback resistance = %v, want 0.4", got)
	}
}
