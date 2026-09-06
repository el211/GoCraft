package world

import "testing"

func TestDrySpongeAbsorbsConnectedWater(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	for x := 1; x <= 3; x++ {
		w.SetBlock(x, 64, 0, MakeFluid("minecraft:water", 0))
	}
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "sponge"})

	if got := w.GetBlock(0, 64, 0).ResourceLocation(); got != "minecraft:wet_sponge" {
		t.Fatalf("sponge = %s, want minecraft:wet_sponge after absorbing", got)
	}
	for x := 1; x <= 3; x++ {
		if !w.GetBlock(x, 64, 0).IsAir() {
			t.Fatalf("water at x=%d was not absorbed", x)
		}
	}
}

func TestDrySpongeDrainsWaterloggedBlock(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	kelp := Block{Namespace: "minecraft", Name: "kelp_plant", Properties: map[string]string{"waterlogged": "true"}}
	w.SetBlock(1, 64, 0, kelp)
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "sponge"})

	drained := w.GetBlock(1, 64, 0)
	if drained.ResourceLocation() != "minecraft:kelp_plant" || drained.Properties["waterlogged"] != "false" {
		t.Fatalf("waterlogged block = %s waterlogged=%s, want drained in place", drained.ResourceLocation(), drained.Properties["waterlogged"])
	}
	if got := w.GetBlock(0, 64, 0).ResourceLocation(); got != "minecraft:wet_sponge" {
		t.Fatalf("sponge = %s, want wet_sponge", got)
	}
}

func TestDrySpongeStaysDryWithoutWater(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "sponge"})
	if got := w.GetBlock(0, 64, 0).ResourceLocation(); got != "minecraft:sponge" {
		t.Fatalf("dry sponge = %s, want it to stay minecraft:sponge", got)
	}
}

func TestWetSpongeDriesInUltrawarm(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetUltrawarm(true)
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "wet_sponge"})
	if got := w.GetBlock(0, 64, 0).ResourceLocation(); got != "minecraft:sponge" {
		t.Fatalf("wet_sponge in Nether = %s, want minecraft:sponge (instant dry)", got)
	}
}

func TestSpongeAbsorptionRespectsTaxicabDistance(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	// A connected water line of length 7; only the first six are within range.
	for x := 1; x <= 7; x++ {
		w.SetBlock(x, 64, 0, MakeFluid("minecraft:water", 0))
	}
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "sponge"})

	for x := 1; x <= 6; x++ {
		if !w.GetBlock(x, 64, 0).IsAir() {
			t.Fatalf("water at distance %d should be absorbed", x)
		}
	}
	if w.GetBlock(7, 64, 0).ResourceLocation() != "minecraft:water" {
		t.Fatal("water at distance 7 should remain beyond the sponge range")
	}
}
