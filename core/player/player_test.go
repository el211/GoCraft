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

func TestHealFullRestoresLivingSurvivalState(t *testing.T) {
	p := New([16]byte{}, `healer`, ClientEditionJava)
	p.Health = 3
	p.Food = 4
	p.Saturation = 0
	p.LastDamageCause = `test damage`
	if !p.HealFull() {
		t.Fatal(`HealFull rejected a living player`)
	}
	health, food, saturation, dead := p.HealthSnapshot()
	if health != 20 || food != 20 || saturation != 5 || dead {
		t.Fatalf(`healed state = health %.1f food %d saturation %.1f dead %v`, health, food, saturation, dead)
	}
}

func TestHealFullDoesNotReviveDeadPlayer(t *testing.T) {
	p := New([16]byte{}, `dead`, ClientEditionJava)
	p.ApplyDamage(20, `test damage`)
	if p.HealFull() {
		t.Fatal(`HealFull revived a dead player`)
	}
}

func TestHungerExhaustionAndFoodConsumption(t *testing.T) {
	p := New([16]byte{2}, "hungry", ClientEditionBedrock)
	p.Saturation = 1
	p.AddExhaustion(4)
	food, saturation, exhaustion := p.HungerSnapshot()
	if food != 20 || saturation != 0 || exhaustion != 0 {
		t.Fatalf("after first rollover = food %d saturation %.1f exhaustion %.1f", food, saturation, exhaustion)
	}
	p.AddExhaustion(4)
	food, saturation, _ = p.HungerSnapshot()
	if food != 19 || saturation != 0 {
		t.Fatalf("after second rollover = food %d saturation %.1f", food, saturation)
	}
	if !p.ConsumeFood(5, 0.6) {
		t.Fatal("hungry player could not eat bread")
	}
	food, saturation, _ = p.HungerSnapshot()
	if food != 20 || saturation != 6 {
		t.Fatalf("after bread = food %d saturation %.1f, want 20/6", food, saturation)
	}
}
