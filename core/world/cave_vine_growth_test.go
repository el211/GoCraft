package world

import "testing"

func TestCaveVineGrowsDownward(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.Chunk(0, 0)
	tip := Block{Namespace: "minecraft", Name: "cave_vines",
		Properties: map[string]string{"age": "0", "berries": "false"}}
	w.SetBlock(0, 80, 0, tip)

	var grew bool
	for tick := int64(0); tick < 200_000 && !grew; tick++ {
		changes := w.TickCaveVineAt(0, 80, 0, w.GetBlock(0, 80, 0), tick)
		for _, c := range changes {
			if c.Y == 79 && c.Block.ResourceLocation() == "minecraft:cave_vines" {
				grew = true
			}
		}
	}
	if !grew {
		t.Fatal("cave vine never grew downward")
	}
	if got := w.GetBlock(0, 80, 0).ResourceLocation(); got != "minecraft:cave_vines_plant" {
		t.Fatalf("old tip = %q after growth, want cave_vines_plant", got)
	}
}

func TestCaveVineGrowsBerries(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.Chunk(0, 0)
	// Fully aged tip (cannot grow further) to isolate berry growth.
	tip := Block{Namespace: "minecraft", Name: "cave_vines",
		Properties: map[string]string{"age": "25", "berries": "false"}}
	w.SetBlock(0, 80, 0, tip)

	var hasBerries bool
	for tick := int64(0); tick < 200_000 && !hasBerries; tick++ {
		w.TickCaveVineAt(0, 80, 0, w.GetBlock(0, 80, 0), tick)
		if w.GetBlock(0, 80, 0).Properties["berries"] == "true" {
			hasBerries = true
		}
	}
	if !hasBerries {
		t.Fatal("cave vine never grew berries")
	}
}

func TestCaveVineHarvestBerries(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.Chunk(0, 0)
	tip := Block{Namespace: "minecraft", Name: "cave_vines",
		Properties: map[string]string{"age": "5", "berries": "true"}}
	w.SetBlock(0, 80, 0, tip)

	count, changes, harvested := w.HarvestCaveVineBerries(0, 80, 0)
	if !harvested {
		t.Fatal("harvest reported not harvested")
	}
	if count != 1 {
		t.Fatalf("harvest count = %d, want 1", count)
	}
	if len(changes) == 0 {
		t.Fatal("no block changes from harvest")
	}
	if got := w.GetBlock(0, 80, 0).Properties["berries"]; got != "false" {
		t.Fatalf("berries after harvest = %q, want false", got)
	}
}
