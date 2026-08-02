package server

import (
	"testing"

	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/java/session"
)

func TestBedrockInventoryCanEquipArmorAuthoritatively(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{9}, "bedrock", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:iron_helmet", Count: 1}
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	s := &Server{game: g, sessions: session.NewManager()}
	done := make(chan intent.InventoryResult, 1)
	s.applyBedrockInventory(intent.InventoryIntent{
		PlayerUUID: p.UUID,
		Actions: []intent.InventoryAction{{
			Kind: intent.InventoryActionMove, Source: player.HotbarStart, Destination: 5, Count: 1,
		}},
		Done: done,
	})
	if result := <-done; !result.Accepted {
		t.Fatal("valid armour move was rejected")
	}
	if p.Inventory[5].ItemID != "minecraft:iron_helmet" || !p.Inventory[player.HotbarStart].IsEmpty() {
		t.Fatalf("unexpected inventory after equip: helmet=%+v source=%+v", p.Inventory[5], p.Inventory[player.HotbarStart])
	}
}

func TestBedrockInventoryRejectsNonArmorInArmorSlot(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{10}, "bedrock", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:stone", Count: 1}
	_ = g.AddPlayer(p)
	s := &Server{game: g, sessions: session.NewManager()}
	done := make(chan intent.InventoryResult, 1)
	s.applyBedrockInventory(intent.InventoryIntent{
		PlayerUUID: p.UUID,
		Actions: []intent.InventoryAction{{
			Kind: intent.InventoryActionMove, Source: player.HotbarStart, Destination: 5, Count: 1,
		}},
		Done: done,
	})
	if result := <-done; result.Accepted {
		t.Fatal("stone was accepted as a helmet")
	}
	if p.Inventory[player.HotbarStart].Count != 1 || !p.Inventory[5].IsEmpty() {
		t.Fatal("rejected transaction mutated inventory")
	}
}
