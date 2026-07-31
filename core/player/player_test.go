package player

import "testing"

func TestGiveItemMergesWithoutOverflow(t *testing.T) {
	p := &Player{}
	p.Inventory[HotbarStart] = ItemStack{ItemID: "minecraft:stone", Count: 60}
	if !p.GiveItem(ItemStack{ItemID: "minecraft:stone", Count: 10}) {
		t.Fatal("GiveItem rejected available inventory")
	}
	if got := p.Inventory[HotbarStart].Count; got != 64 {
		t.Fatalf("merged count = %d, want 64", got)
	}
	if got := p.Inventory[HotbarStart+1]; got.ItemID != "minecraft:stone" || got.Count != 6 {
		t.Fatalf("overflow stack = %+v, want 6 stone", got)
	}
}

func TestGiveItemFailureIsAtomic(t *testing.T) {
	p := &Player{}
	for slot := 9; slot < InventorySize; slot++ {
		p.Inventory[slot] = ItemStack{ItemID: "minecraft:dirt", Count: 64}
	}
	p.Inventory[HotbarStart] = ItemStack{ItemID: "minecraft:stone", Count: 63}
	if p.GiveItem(ItemStack{ItemID: "minecraft:stone", Count: 2}) {
		t.Fatal("GiveItem accepted an inventory with only one free item of capacity")
	}
	if got := p.Inventory[HotbarStart].Count; got != 63 {
		t.Fatalf("failed GiveItem mutated stack to %d", got)
	}
}
