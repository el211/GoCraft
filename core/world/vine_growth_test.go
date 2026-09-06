package world

import "testing"

func newVineTestWorld() *World {
	w := New(&FlatGenerator{}, nil, false)
	for cx := int32(-1); cx <= 1; cx++ {
		for cz := int32(-1); cz <= 1; cz++ {
			w.Chunk(cx, cz)
		}
	}
	return w
}

func vineBlock(north, south, east, west bool) Block {
	boolStr := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	return Block{
		Namespace: "minecraft",
		Name:      "vine",
		Properties: map[string]string{
			"north": boolStr(north),
			"south": boolStr(south),
			"east":  boolStr(east),
			"west":  boolStr(west),
			"up":    "false",
		},
	}
}

// TestVineSpreadDown verifies that a vine spreads downward into air.
func TestVineSpreadDown(t *testing.T) {
	w := newVineTestWorld()
	// Solid support block to the south (z+1) for the south-facing vine.
	w.SetBlock(0, 65, 1, Block{Namespace: "minecraft", Name: "stone"})
	vine := vineBlock(false, true, false, false)
	w.SetBlock(0, 65, 0, vine)

	for tick := int64(0); tick < 10000; tick += 20 {
		changes := w.tickVineAt(0, 65, 0, vine, tick)
		for _, ch := range changes {
			if ch.Y == 64 && ch.Block.ResourceLocation() == "minecraft:vine" {
				return
			}
		}
	}
	t.Fatal("vine never spread downward in 10000 ticks")
}

// TestVineSpreadHorizontal verifies horizontal spread. A south-facing vine at
// (0,65,0) (stone at z+1) spreading westward needs stone at (-1, 65, 1) to
// support the south face of the new west-adjacent vine.
func TestVineSpreadHorizontal(t *testing.T) {
	w := newVineTestWorld()
	// South support for original vine.
	w.SetBlock(0, 65, 1, Block{Namespace: "minecraft", Name: "stone"})
	// South support for potential west vine at (-1, 65, 0).
	w.SetBlock(-1, 65, 1, Block{Namespace: "minecraft", Name: "stone"})
	vine := vineBlock(false, true, false, false)
	w.SetBlock(0, 65, 0, vine)

	for tick := int64(0); tick < 40000; tick += 20 {
		changes := w.tickVineAt(0, 65, 0, vine, tick)
		for _, ch := range changes {
			if ch.Block.ResourceLocation() == "minecraft:vine" && ch.Y == 65 &&
				(ch.X != 0 || ch.Z != 0) {
				return
			}
		}
	}
	t.Fatal("vine never spread horizontally in 40000 ticks")
}
