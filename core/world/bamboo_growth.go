package world


// Bamboo grows upward through random ticks. A bamboo_sapling converts to a
// single bamboo segment on its first tick. The tip of a column grows into air
// above until the stalk reaches its target height (12–16 blocks, chosen once
// per stalk via a position hash). Each segment carries a leaves property:
// the top segment is "small", the second from top is "large", all others
// "none". This mirrors Pumpkin/vanilla bamboo growth behaviour.

const (
	bambooMinHeight    = 12
	bambooHeightRange  = 5 // 12..16 inclusive
	bambooGrowthSalt   = uint64(0xf39cc0605cedc834)
	bambooHeightSalt   = uint64(0xd2a98b26625eee7b)
	bambooSaplingOneIn = 3
)

// isBambooGrowthBlock reports whether the block participates in bamboo ticking.
func isBambooGrowthBlock(name string) bool {
	return name == "minecraft:bamboo_sapling" || name == "minecraft:bamboo"
}

// validBambooSoil returns true for blocks that can anchor the base of a bamboo
// column, matching the vanilla planting rules.
func validBambooSoil(name string) bool {
	switch name {
	case "minecraft:dirt", "minecraft:grass_block", "minecraft:coarse_dirt",
		"minecraft:podzol", "minecraft:rooted_dirt", "minecraft:mycelium",
		"minecraft:moss_block", "minecraft:mud", "minecraft:sand",
		"minecraft:red_sand", "minecraft:gravel":
		return true
	default:
		return false
	}
}

// bambooColumnHeight counts the consecutive bamboo blocks downward from (x,y,z)
// inclusive, stopping at the first non-bamboo block. The sapling is not counted.
func (w *World) bambooColumnHeight(x, y, z int) int {
	height := 0
	for dy := 0; dy <= bambooMinHeight+bambooHeightRange+2; dy++ {
		b, loaded := w.blockIfLoaded(x, y-dy, z)
		if !loaded || b.ResourceLocation() != "minecraft:bamboo" {
			break
		}
		height++
	}
	return height
}

// bambooTargetHeight returns the target column height for the stalk rooted at
// (x, baseY, z). The value is stable for a given position so the stalk does
// not flip between growing and stopping.
func bambooTargetHeight(x, baseY, z int) int {
	roll := uint64(int64(x)*0x9e3779b1) ^ uint64(int64(baseY)*0x85ebca77) ^
		uint64(int64(z)*0xc2b2ae3d) ^ bambooHeightSalt
	roll ^= roll >> 33
	roll *= 0xff51afd7ed558ccd
	roll ^= roll >> 33
	return bambooMinHeight + int(roll%uint64(bambooHeightRange))
}

// bambooLeaves returns the correct leaves value for a segment at segmentIndex
// from the base (0=bottom), given the total column height.
func bambooLeaves(segmentIndex, height int) string {
	switch height - 1 - segmentIndex {
	case 0:
		return "small"
	case 1:
		return "large"
	default:
		return "none"
	}
}

func makeBambooBlock(leaves string) Block {
	return Block{
		Namespace:  "minecraft",
		Name:       "bamboo",
		Properties: map[string]string{"age": "1", "leaves": leaves, "stage": "0"},
	}
}

// tickBambooAt advances a bamboo_sapling or bamboo tip. Saplings convert to a
// single bamboo segment. Tip segments grow one block upward when the column has
// not yet reached its target height.
func (w *World) tickBambooAt(x, y, z int, block Block, tick int64) []BlockChange {
	name := block.ResourceLocation()
	seed := uint64(tick / 20)

	if name == "minecraft:bamboo_sapling" {
		// Convert sapling to first bamboo segment ~1-in-3 ticks.
		if cropRandom(seed, x, y, z, bambooGrowthSalt, bambooSaplingOneIn) != 0 {
			return nil
		}
		below, loaded := w.blockIfLoaded(x, y-1, z)
		if !loaded || !validBambooSoil(below.ResourceLocation()) {
			return nil
		}
		above, aboveLoaded := w.blockIfLoaded(x, y+1, z)
		if !aboveLoaded || !above.IsAir() {
			return nil
		}
		segment := makeBambooBlock("small")
		w.SetBlock(x, y, z, segment)
		return []BlockChange{{X: x, Y: y, Z: z, Block: segment}}
	}

	// Only tick the tip (no bamboo directly above).
	above, loaded := w.blockIfLoaded(x, y+1, z)
	if !loaded || above.ResourceLocation() == "minecraft:bamboo" {
		return nil
	}
	if !above.IsAir() {
		return nil
	}
	// Gate: ~1-in-3 growth chance per tick.
	if cropRandom(seed, x, y, z, bambooGrowthSalt, 3) != 0 {
		return nil
	}
	height := w.bambooColumnHeight(x, y, z)
	baseY := y - height + 1
	target := bambooTargetHeight(x, baseY, z)
	if height >= target {
		return nil
	}

	changes := make([]BlockChange, 0, 3)

	// New tip.
	newTip := makeBambooBlock("small")
	w.SetBlock(x, y+1, z, newTip)
	changes = append(changes, BlockChange{X: x, Y: y + 1, Z: z, Block: newTip})

	// Old tip becomes second-from-top → leaves="large".
	oldTip := makeBambooBlock("large")
	w.SetBlock(x, y, z, oldTip)
	changes = append(changes, BlockChange{X: x, Y: y, Z: z, Block: oldTip})

	// Third from new top loses its "large" leaf to "none".
	if height >= 2 {
		third := makeBambooBlock("none")
		w.SetBlock(x, y-1, z, third)
		changes = append(changes, BlockChange{X: x, Y: y - 1, Z: z, Block: third})
	}
	return changes
}
