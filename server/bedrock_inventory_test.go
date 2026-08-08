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

func TestBedrockInventoryCursorRoundTrip(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{11}, "bedrock", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Inventory[9] = player.ItemStack{ItemID: "minecraft:dirt", Count: 8}
	_ = g.AddPlayer(p)
	s := &Server{game: g, sessions: session.NewManager()}

	apply := func(action intent.InventoryAction) {
		t.Helper()
		done := make(chan intent.InventoryResult, 1)
		s.applyBedrockInventory(intent.InventoryIntent{
			PlayerUUID: p.UUID,
			Actions:    []intent.InventoryAction{action},
			Done:       done,
		})
		if result := <-done; !result.Accepted {
			t.Fatalf("inventory action was rejected: %+v", action)
		}
	}

	apply(intent.InventoryAction{
		Kind: intent.InventoryActionMove, Source: 9, Destination: intent.InventoryCursorSlot, Count: 8,
	})
	if !p.Inventory[9].IsEmpty() || p.CarriedItem.ItemID != "minecraft:dirt" || p.CarriedItem.Count != 8 {
		t.Fatalf("dirt was not moved to cursor: source=%+v cursor=%+v", p.Inventory[9], p.CarriedItem)
	}

	apply(intent.InventoryAction{
		Kind: intent.InventoryActionMove, Source: intent.InventoryCursorSlot, Destination: 10, Count: 8,
	})
	if !p.CarriedItem.IsEmpty() || p.Inventory[10].ItemID != "minecraft:dirt" || p.Inventory[10].Count != 8 {
		t.Fatalf("dirt was not moved out of cursor: destination=%+v cursor=%+v", p.Inventory[10], p.CarriedItem)
	}
}

func TestBedrockCreativeGiveCanBePlacedFromCursor(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{12}, "builder", player.ClientEditionBedrock)
	p.GameMode = player.GameModeCreative
	_ = g.AddPlayer(p)
	s := &Server{game: g, sessions: session.NewManager()}
	done := make(chan intent.InventoryResult, 1)

	s.applyBedrockInventory(intent.InventoryIntent{
		PlayerUUID: p.UUID,
		Actions: []intent.InventoryAction{
			{
				Kind: intent.InventoryActionCreativeGive, Destination: intent.InventoryCursorSlot,
				Count: 64, Item: player.ItemStack{ItemID: "minecraft:oak_log", Count: 64},
			},
			{
				Kind: intent.InventoryActionMove, Source: intent.InventoryCursorSlot,
				Destination: player.HotbarStart, Count: 64,
			},
		},
		Done: done,
	})
	if result := <-done; !result.Accepted {
		t.Fatal("creative give and placement was rejected")
	}
	if !p.CarriedItem.IsEmpty() || p.Inventory[player.HotbarStart].ItemID != "minecraft:oak_log" || p.Inventory[player.HotbarStart].Count != 64 {
		t.Fatalf("creative log did not reach hotbar: hotbar=%+v cursor=%+v", p.Inventory[player.HotbarStart], p.CarriedItem)
	}
}

func TestBedrockPersonalCraftingProducesAndConsumesRecipe(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{13}, "crafter", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:oak_log", Count: 2}
	_ = g.AddPlayer(p)
	s := &Server{game: g, sessions: session.NewManager()}

	apply := func(action intent.InventoryAction) {
		t.Helper()
		done := make(chan intent.InventoryResult, 1)
		s.applyBedrockInventory(intent.InventoryIntent{
			PlayerUUID: p.UUID,
			Actions:    []intent.InventoryAction{action},
			Done:       done,
		})
		if result := <-done; !result.Accepted {
			t.Fatalf("inventory action was rejected: %+v", action)
		}
	}

	apply(intent.InventoryAction{
		Kind: intent.InventoryActionMove, Source: player.HotbarStart, Destination: 1, Count: 2,
	})
	if p.Inventory[0].ItemID != "minecraft:oak_planks" || p.Inventory[0].Count != 4 {
		t.Fatalf("crafting output = %+v, want four oak planks", p.Inventory[0])
	}

	apply(intent.InventoryAction{
		Kind: intent.InventoryActionMove, Source: 0, Destination: intent.InventoryCursorSlot, Count: 4,
	})
	if p.CarriedItem.ItemID != "minecraft:oak_planks" || p.CarriedItem.Count != 4 {
		t.Fatalf("cursor = %+v, want four oak planks", p.CarriedItem)
	}
	if p.Inventory[1].Count != 1 {
		t.Fatalf("remaining log count = %d, want 1", p.Inventory[1].Count)
	}
	if p.Inventory[0].ItemID != "minecraft:oak_planks" || p.Inventory[0].Count != 4 {
		t.Fatalf("next crafting output = %+v, want four oak planks", p.Inventory[0])
	}
}
