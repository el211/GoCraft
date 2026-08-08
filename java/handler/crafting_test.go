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

func TestPersonalCraftingResolvesWoodVariantsToMatchingPlanks(t *testing.T) {
	tests := []struct {
		input string
		want  string
		count int
	}{
		{"minecraft:oak_log", "minecraft:oak_planks", 4},
		{"minecraft:birch_log", "minecraft:birch_planks", 4},
		{"minecraft:stripped_spruce_wood", "minecraft:spruce_planks", 4},
		{"minecraft:mangrove_log", "minecraft:mangrove_planks", 4},
		{"minecraft:crimson_stem", "minecraft:crimson_planks", 4},
		{"minecraft:bamboo_block", "minecraft:bamboo_planks", 2},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			grid := [4]player.ItemStack{{ItemID: test.input, Count: 1}}
			got := FindPersonalCraftingResult(grid)
			if got.ItemID != test.want || got.Count != test.count {
				t.Fatalf("result = %+v, want %d %s", got, test.count, test.want)
			}
		})
	}
}

func TestPersonalCraftingSupportsShapedTwoByTwoRecipes(t *testing.T) {
	grid := [4]player.ItemStack{
		{ItemID: "minecraft:oak_planks", Count: 1},
		{ItemID: "minecraft:oak_planks", Count: 1},
		{ItemID: "minecraft:oak_planks", Count: 1},
		{ItemID: "minecraft:oak_planks", Count: 1},
	}
	got := FindPersonalCraftingResult(grid)
	if got.ItemID != "minecraft:crafting_table" || got.Count != 1 {
		t.Fatalf("result = %+v, want crafting table", got)
	}
}
