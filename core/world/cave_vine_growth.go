package world

// Cave vines hang downward from cave ceilings. The growing tip is
// minecraft:cave_vines (age 0..25) and each segment left above becomes
// minecraft:cave_vines_plant. Both tip and plant blocks carry a "berries"
// boolean property; berries grow independently of the vine tip mechanic.
// This mirrors Pumpkin/vanilla cave vine random-tick behaviour.

const (
	caveVineGrowthSalt  = uint64(0x9c87e31d5a120b67)
	caveVineBerryChance = 11 // ~1-in-11 per tick
	caveVineGrowChance  = 11 // ~1-in-11 per tick; vanilla ~9% per tick
	caveVineMaxAge      = 25
)

func isCaveVineTip(name string) bool {
	return name == "minecraft:cave_vines"
}

func isCaveVineBlock(name string) bool {
	return name == "minecraft:cave_vines" || name == "minecraft:cave_vines_plant"
}

func makeCaveVine(name, age, berries string) Block {
	return Block{
		Namespace:  "minecraft",
		Name:       name,
		Properties: map[string]string{"age": age, "berries": berries},
	}
}

// TickCaveVineAt advances a cave_vines tip: it may grow one block downward into
// air, and may independently gain berries. Returns all block changes.
func (w *World) TickCaveVineAt(x, y, z int, vine Block, tick int64) []BlockChange {
	if !isCaveVineTip(vine.ResourceLocation()) {
		return nil
	}
	seed := uint64(tick / 20)
	age := kelpAge(vine) // reuse the 0..25 age parser from kelp
	changes := make([]BlockChange, 0, 2)

	// Berry growth: tip and plant blocks can independently grow berries.
	if vine.Properties["berries"] != "true" &&
		cropRandom(seed, x, y, z, caveVineBerryChance, caveVineBerryChance) == 0 {
		withBerries := copyWorldBlock(vine)
		withBerries.Properties["berries"] = "true"
		w.SetBlock(x, y, z, withBerries)
		changes = append(changes, BlockChange{X: x, Y: y, Z: z, Block: withBerries})
		// Return early; vanilla does not grow the vine on the same tick it grows berries.
		return changes
	}

	// Tip growth: grow one block downward into air when the vine is young enough.
	if age >= caveVineMaxAge {
		return changes
	}
	below, loaded := w.blockIfLoaded(x, y-1, z)
	if !loaded || !below.IsAir() {
		return changes
	}
	if cropRandom(seed, x, y, z, caveVineGrowthSalt, caveVineGrowChance) != 0 {
		return changes
	}

	// Convert old tip to a plant body.
	plant := Block{Namespace: "minecraft", Name: "cave_vines_plant",
		Properties: map[string]string{"berries": vine.Properties["berries"]}}
	w.SetBlock(x, y, z, plant)
	changes = append(changes, BlockChange{X: x, Y: y, Z: z, Block: plant})

	// Place new tip below.
	newAge := age + 1
	newAgeStr := itoa(newAge)
	newTip := Block{Namespace: "minecraft", Name: "cave_vines",
		Properties: map[string]string{"age": newAgeStr, "berries": "false"}}
	w.SetBlock(x, y-1, z, newTip)
	changes = append(changes, BlockChange{X: x, Y: y - 1, Z: z, Block: newTip})
	return changes
}

// HarvestCaveVineBerries handles a player right-clicking a cave vine tip or
// plant to harvest glow berries. Returns the item count and block changes.
func (w *World) HarvestCaveVineBerries(x, y, z int) (count int, changes []BlockChange, harvested bool) {
	block := w.GetBlock(x, y, z)
	if !isCaveVineBlock(block.ResourceLocation()) {
		return 0, nil, false
	}
	if block.Properties["berries"] != "true" {
		return 0, nil, false
	}
	stripped := copyWorldBlock(block)
	stripped.Properties["berries"] = "false"
	w.SetBlock(x, y, z, stripped)
	return 1, []BlockChange{{X: x, Y: y, Z: z, Block: stripped}}, true
}
