package world

import "strings"

// BrewingRecipe describes one vanilla brewing transformation.
// InputPotion is the short name (no namespace) of the required base potion,
// or "" to match any potion type stored in the bottle.
// InputItem is "minecraft:glass_bottle" for empty bottles, "" to match any
// brewed item (potion/splash/lingering).
// OutputPotion is the resulting short potion name; empty means the potion
// type is unchanged (used for splash/lingering container conversions).
// OutputItem is the resulting item ID; empty means the item type is unchanged.
type BrewingRecipe struct {
	InputItem   string
	InputPotion string
	Ingredient  string
	OutputItem  string
	OutputPotion string
}

// brewingRecipes is the canonical Java 1.21.4 brewing recipe table.
// Entries are checked in order; the first match wins.
var brewingRecipes = []BrewingRecipe{
	// ── Fuel / mundane → thick ────────────────────────────────────────────────
	// glass_bottle + water makes a water bottle (handled implicitly via cauldron;
	// here we treat "water" as the starting type for nether-wart brewing).

	// ── Base brews (water bottle → base potion) ───────────────────────────────
	{InputPotion: "water", Ingredient: "minecraft:nether_wart", OutputPotion: "awkward"},
	{InputPotion: "water", Ingredient: "minecraft:glowstone_dust", OutputPotion: "thick"},
	{InputPotion: "water", Ingredient: "minecraft:redstone", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:sugar", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:rabbit_foot", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:glistering_melon_slice", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:blaze_powder", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:spider_eye", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:ghast_tear", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:magma_cream", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:pufferfish", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:golden_carrot", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "weakness"},
	{InputPotion: "water", Ingredient: "minecraft:phantom_membrane", OutputPotion: "mundane"},
	{InputPotion: "water", Ingredient: "minecraft:turtle_scute", OutputPotion: "mundane"},

	// ── Awkward → primary effects ─────────────────────────────────────────────
	{InputPotion: "awkward", Ingredient: "minecraft:sugar", OutputPotion: "swiftness"},
	{InputPotion: "awkward", Ingredient: "minecraft:rabbit_foot", OutputPotion: "leaping"},
	{InputPotion: "awkward", Ingredient: "minecraft:glistering_melon_slice", OutputPotion: "healing"},
	{InputPotion: "awkward", Ingredient: "minecraft:spider_eye", OutputPotion: "poison"},
	{InputPotion: "awkward", Ingredient: "minecraft:ghast_tear", OutputPotion: "regeneration"},
	{InputPotion: "awkward", Ingredient: "minecraft:blaze_powder", OutputPotion: "strength"},
	{InputPotion: "awkward", Ingredient: "minecraft:magma_cream", OutputPotion: "fire_resistance"},
	{InputPotion: "awkward", Ingredient: "minecraft:pufferfish", OutputPotion: "water_breathing"},
	{InputPotion: "awkward", Ingredient: "minecraft:golden_carrot", OutputPotion: "night_vision"},
	{InputPotion: "awkward", Ingredient: "minecraft:phantom_membrane", OutputPotion: "slow_falling"},
	{InputPotion: "awkward", Ingredient: "minecraft:turtle_scute", OutputPotion: "turtle_master"},

	// ── Fermented spider eye corruptions ──────────────────────────────────────
	{InputPotion: "swiftness", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "slowness"},
	{InputPotion: "long_swiftness", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "long_slowness"},
	{InputPotion: "leaping", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "slowness"},
	{InputPotion: "long_leaping", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "long_slowness"},
	{InputPotion: "healing", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "harming"},
	{InputPotion: "strong_healing", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "strong_harming"},
	{InputPotion: "poison", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "harming"},
	{InputPotion: "long_poison", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "harming"},
	{InputPotion: "strong_poison", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "strong_harming"},
	{InputPotion: "night_vision", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "invisibility"},
	{InputPotion: "long_night_vision", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "long_invisibility"},
	{InputPotion: "fire_resistance", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "slowness"},
	{InputPotion: "long_fire_resistance", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "long_slowness"},
	{InputPotion: "water_breathing", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "slowness"},
	{InputPotion: "long_water_breathing", Ingredient: "minecraft:fermented_spider_eye", OutputPotion: "long_slowness"},

	// ── Redstone (duration extension) ────────────────────────────────────────
	{InputPotion: "swiftness", Ingredient: "minecraft:redstone", OutputPotion: "long_swiftness"},
	{InputPotion: "slowness", Ingredient: "minecraft:redstone", OutputPotion: "long_slowness"},
	{InputPotion: "leaping", Ingredient: "minecraft:redstone", OutputPotion: "long_leaping"},
	{InputPotion: "healing", Ingredient: "minecraft:redstone", OutputPotion: "long_healing"},
	{InputPotion: "harming", Ingredient: "minecraft:redstone", OutputPotion: "long_harming"},
	{InputPotion: "poison", Ingredient: "minecraft:redstone", OutputPotion: "long_poison"},
	{InputPotion: "regeneration", Ingredient: "minecraft:redstone", OutputPotion: "long_regeneration"},
	{InputPotion: "strength", Ingredient: "minecraft:redstone", OutputPotion: "long_strength"},
	{InputPotion: "fire_resistance", Ingredient: "minecraft:redstone", OutputPotion: "long_fire_resistance"},
	{InputPotion: "water_breathing", Ingredient: "minecraft:redstone", OutputPotion: "long_water_breathing"},
	{InputPotion: "night_vision", Ingredient: "minecraft:redstone", OutputPotion: "long_night_vision"},
	{InputPotion: "invisibility", Ingredient: "minecraft:redstone", OutputPotion: "long_invisibility"},
	{InputPotion: "weakness", Ingredient: "minecraft:redstone", OutputPotion: "long_weakness"},
	{InputPotion: "slow_falling", Ingredient: "minecraft:redstone", OutputPotion: "long_slow_falling"},
	{InputPotion: "turtle_master", Ingredient: "minecraft:redstone", OutputPotion: "long_turtle_master"},

	// ── Glowstone (potency increase) ─────────────────────────────────────────
	{InputPotion: "swiftness", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_swiftness"},
	{InputPotion: "slowness", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_slowness"},
	{InputPotion: "leaping", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_leaping"},
	{InputPotion: "healing", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_healing"},
	{InputPotion: "harming", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_harming"},
	{InputPotion: "poison", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_poison"},
	{InputPotion: "regeneration", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_regeneration"},
	{InputPotion: "strength", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_strength"},
	{InputPotion: "turtle_master", Ingredient: "minecraft:glowstone_dust", OutputPotion: "strong_turtle_master"},

	// ── Gunpowder: potion → splash_potion ─────────────────────────────────────
	{InputItem: "minecraft:potion", Ingredient: "minecraft:gunpowder", OutputItem: "minecraft:splash_potion"},

	// ── Dragon's breath: splash → lingering ───────────────────────────────────
	{InputItem: "minecraft:splash_potion", Ingredient: "minecraft:dragon_breath", OutputItem: "minecraft:lingering_potion"},
}

// potionName returns the short potion name from a potion_contents component string.
// Returns "" for glass bottles or items without potion contents.
func potionName(componentsJSON string) string {
	// Fast path: look for "potion":"minecraft:XXX" in the JSON.
	const prefix = `"potion":"minecraft:`
	idx := strings.Index(componentsJSON, prefix)
	if idx < 0 {
		return ""
	}
	start := idx + len(prefix)
	end := strings.IndexByte(componentsJSON[start:], '"')
	if end < 0 {
		return ""
	}
	return componentsJSON[start : start+end]
}

// BrewingResult returns the result of brewing ingredient into bottle.
// Returns the updated bottle stack and true if a recipe matched.
func BrewingResult(bottle, ingredient string, bottleComponents string) (outItem, outPotion string, ok bool) {
	inputPotion := potionName(bottleComponents)

	for _, recipe := range brewingRecipes {
		if recipe.Ingredient != ingredient {
			continue
		}
		// Check item type constraint.
		if recipe.InputItem != "" && recipe.InputItem != bottle {
			continue
		}
		// Check potion type constraint.
		if recipe.InputPotion != "" && recipe.InputPotion != inputPotion {
			continue
		}
		outItem = bottle
		if recipe.OutputItem != "" {
			outItem = recipe.OutputItem
		}
		outPotion = inputPotion
		if recipe.OutputPotion != "" {
			outPotion = recipe.OutputPotion
		}
		return outItem, outPotion, true
	}
	return "", "", false
}
