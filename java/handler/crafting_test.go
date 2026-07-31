package handler

import (
	"testing"

	"GoCraft/core/player"
)

func TestRecipePlacementUsesInventoryAndProducesCraftingResult(t *testing.T) {
	var stick recipeDisplay
	found := false
	for _, recipe := range javaRecipeDisplays {
		if recipe.name == "minecraft:stick" {
			stick, found = recipe, true
			break
		}
	}
	if !found {
		t.Fatal("complete catalog does not contain minecraft:stick")
	}
	template, err := craftingTemplate(stick)
	if err != nil {
		t.Fatal(err)
	}
	var inventory [player.InventorySize]player.ItemStack
	inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:oak_planks", Count: 2}
	var grid [9]player.ItemStack
	if !placeRecipeOnce(&inventory, &grid, template) {
		t.Fatal("automatic placement rejected two oak planks for the stick recipe")
	}
	if inventory[player.HotbarStart].Count != 0 || grid[0].ItemID != "minecraft:oak_planks" || grid[3].ItemID != "minecraft:oak_planks" {
		t.Fatalf("placement inventory=%+v grid[0]=%+v grid[3]=%+v", inventory[player.HotbarStart], grid[0], grid[3])
	}
	if result := findCraftingResult(grid); result.ItemID != "minecraft:stick" || result.Count != 4 {
		t.Fatalf("crafting result = %+v, want four sticks", result)
	}
}

func TestTakingCraftingResultConsumesOneIngredientPerOccupiedSlot(t *testing.T) {
	p := player.New([16]byte{}, "crafter", player.ClientEditionJava)
	p.CraftingGrid[0] = player.ItemStack{ItemID: "minecraft:oak_planks", Count: 2}
	p.CraftingGrid[3] = player.ItemStack{ItemID: "minecraft:oak_planks", Count: 2}
	p.CraftingResult = player.ItemStack{ItemID: "minecraft:stick", Count: 4}
	takeCraftingResult(p)
	if p.CarriedItem.ItemID != "minecraft:stick" || p.CarriedItem.Count != 4 {
		t.Fatalf("cursor = %+v, want four sticks", p.CarriedItem)
	}
	if p.CraftingGrid[0].Count != 1 || p.CraftingGrid[3].Count != 1 {
		t.Fatalf("remaining grid counts = %d/%d, want 1/1", p.CraftingGrid[0].Count, p.CraftingGrid[3].Count)
	}
	if next := findCraftingResult(p.CraftingGrid); next.ItemID != "minecraft:stick" || next.Count != 4 {
		t.Fatalf("next result = %+v, want another four sticks", next)
	}
}
