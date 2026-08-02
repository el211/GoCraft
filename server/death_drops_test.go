package server

import (
	"math/rand"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/game"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/session"
)

func TestCowDropsVanillaFood(t *testing.T) {
	drops := mobDrops(corentity.TypeCow, rand.New(rand.NewSource(1)))
	foundBeef := false
	for _, drop := range drops {
		if drop.ItemID == "minecraft:beef" {
			foundBeef = drop.Count >= 1 && drop.Count <= 3
		}
	}
	if !foundBeef {
		t.Fatalf("cow drops = %+v, want 1-3 raw beef", drops)
	}
}

func TestSurvivalDeathDropsAndClearsInventory(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	s := &Server{game: game.New(), world: w, sessions: session.NewManager()}
	p := player.New([16]byte{1}, "dropper", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	p.Position = spatial.Vec3{X: 2, Y: 64, Z: 3}
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:iron_sword", Count: 1, Damage: 12}
	p.Inventory[5] = player.ItemStack{ItemID: "minecraft:iron_helmet", Count: 1, Damage: 4}
	if err := s.game.AddPlayer(p); err != nil {
		t.Fatal(err)
	}

	s.dropPlayerInventory(p)
	if !p.Inventory[player.HotbarStart].IsEmpty() || !p.Inventory[5].IsEmpty() {
		t.Fatalf("inventory was not cleared: hotbar=%+v helmet=%+v", p.Inventory[player.HotbarStart], p.Inventory[5])
	}
	entities := w.Entities.Snapshot()
	if len(entities) != 2 {
		t.Fatalf("dropped entity count = %d, want 2", len(entities))
	}
	for _, entity := range entities {
		if entity.Type != corentity.TypeItem || entity.ItemID == "" || entity.ItemCount != 1 {
			t.Fatalf("invalid dropped item entity: %+v", entity)
		}
	}
}
