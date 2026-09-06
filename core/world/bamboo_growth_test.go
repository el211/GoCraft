package world

import "testing"

func TestBambooSaplingConverts(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.Chunk(0, 0)
	w.SetBlock(0, 63, 0, Block{Namespace: "minecraft", Name: "grass_block"})
	sapling := Block{Namespace: "minecraft", Name: "bamboo_sapling"}
	w.SetBlock(0, 64, 0, sapling)

	var changes []BlockChange
	for tick := int64(0); tick < 200000 && len(changes) == 0; tick++ {
		changes = w.tickBambooAt(0, 64, 0, w.GetBlock(0, 64, 0), tick)
	}
	if len(changes) == 0 {
		t.Fatal("bamboo_sapling never converted to bamboo")
	}
	if got := w.GetBlock(0, 64, 0).ResourceLocation(); got != "minecraft:bamboo" {
		t.Fatalf("after conversion block = %q, want minecraft:bamboo", got)
	}
}

func TestBambooTipGrowsUpward(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.Chunk(0, 0)
	w.SetBlock(0, 63, 0, Block{Namespace: "minecraft", Name: "grass_block"})
	w.SetBlock(0, 64, 0, makeBambooBlock("small"))

	var grew bool
	for tick := int64(0); tick < 200000 && !grew; tick++ {
		changes := w.tickBambooAt(0, 64, 0, w.GetBlock(0, 64, 0), tick)
		if len(changes) > 0 {
			grew = true
		}
	}
	if !grew {
		t.Fatal("bamboo tip never grew upward")
	}
	if got := w.GetBlock(0, 65, 0).ResourceLocation(); got != "minecraft:bamboo" {
		t.Fatalf("block above after growth = %q, want minecraft:bamboo", got)
	}
}

func TestBambooLeavesTransitionOnGrowth(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.Chunk(0, 0)
	w.SetBlock(0, 63, 0, Block{Namespace: "minecraft", Name: "grass_block"})
	// Pre-build a 3-block column.
	w.SetBlock(0, 64, 0, makeBambooBlock("none"))
	w.SetBlock(0, 65, 0, makeBambooBlock("large"))
	w.SetBlock(0, 66, 0, makeBambooBlock("small"))

	var grew bool
	for tick := int64(0); tick < 200000 && !grew; tick++ {
		changes := w.tickBambooAt(0, 66, 0, w.GetBlock(0, 66, 0), tick)
		if len(changes) > 0 {
			grew = true
		}
	}
	if !grew {
		t.Fatal("3-block bamboo column never grew")
	}
	if got := w.GetBlock(0, 67, 0).Properties["leaves"]; got != "small" {
		t.Fatalf("new tip leaves = %q, want small", got)
	}
	if got := w.GetBlock(0, 66, 0).Properties["leaves"]; got != "large" {
		t.Fatalf("old tip leaves = %q, want large", got)
	}
	if got := w.GetBlock(0, 65, 0).Properties["leaves"]; got != "none" {
		t.Fatalf("third-from-top leaves = %q, want none", got)
	}
}

func TestBambooStopsAtTargetHeight(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	for cx := int32(-1); cx <= 1; cx++ {
		for cz := int32(-1); cz <= 1; cz++ {
			w.Chunk(cx, cz)
		}
	}
	w.SetBlock(0, 63, 0, Block{Namespace: "minecraft", Name: "grass_block"})
	w.SetBlock(0, 64, 0, makeBambooBlock("small"))

	// Grow until the column stabilises.
	for tick := int64(0); tick < 5_000_000; tick++ {
		// Find the current tip.
		tip := 64
		for w.GetBlock(0, tip+1, 0).ResourceLocation() == "minecraft:bamboo" {
			tip++
		}
		w.tickBambooAt(0, tip, 0, w.GetBlock(0, tip, 0), tick)
	}

	// Measure the final height by counting from the base.
	height := w.bambooColumnHeight(0, 64, 0)
	// bambooColumnHeight counts downward from given y; call it from a high y.
	finalTip := 64
	for w.GetBlock(0, finalTip+1, 0).ResourceLocation() == "minecraft:bamboo" {
		finalTip++
	}
	height = w.bambooColumnHeight(0, finalTip, 0)
	if height < bambooMinHeight || height > bambooMinHeight+bambooHeightRange {
		t.Fatalf("bamboo final height = %d, want %d..%d", height, bambooMinHeight, bambooMinHeight+bambooHeightRange)
	}
}
