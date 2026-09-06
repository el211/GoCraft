package world

import (
	"math"
	"testing"
)

func testCrop(name string, age int) Block {
	return Block{Namespace: "minecraft", Name: name, Properties: map[string]string{"age": strconvItoa(age)}}
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	if value == 1 {
		return "1"
	}
	if value == 2 {
		return "2"
	}
	if value == 3 {
		return "3"
	}
	if value == 4 {
		return "4"
	}
	if value == 5 {
		return "5"
	}
	if value == 6 {
		return "6"
	}
	return "7"
}

func testFarmland(moisture int) Block {
	return Block{Namespace: "minecraft", Name: "farmland", Properties: map[string]string{"moisture": strconvItoa(moisture)}}
}

func findCropTick(t *testing.T, predicate func(seed uint64) bool) int64 {
	t.Helper()
	for seed := uint64(1); seed < 100000; seed++ {
		if predicate(seed) {
			return int64(seed * 20)
		}
	}
	t.Fatal("no deterministic crop seed satisfied predicate")
	return 0
}

func TestCropMaximumAges(t *testing.T) {
	tests := map[string]int{
		"minecraft:wheat": 7, "minecraft:carrots": 7, "minecraft:potatoes": 7,
		"minecraft:beetroots": 3, "minecraft:nether_wart": 3,
		"minecraft:sweet_berry_bush": 3, "minecraft:torchflower_crop": 2,
		"minecraft:pumpkin_stem": 7, "minecraft:melon_stem": 7,
	}
	for name, want := range tests {
		if got, ok := CropMaxAge(name); !ok || got != want {
			t.Errorf("CropMaxAge(%q) = %d, %v; want %d, true", name, got, ok, want)
		}
	}
}

func TestStandardCropsGrowToAgeSeven(t *testing.T) {
	for _, name := range []string{"wheat", "carrots", "potatoes"} {
		t.Run(name, func(t *testing.T) {
			world := New(&FlatGenerator{}, nil, false)
			defer world.Close()
			world.SetBlock(0, 40, 0, testFarmland(7))
			world.SetBlock(0, 41, 0, testCrop(name, 0))
			for seed := uint64(1); seed < 100000 && CropAge(world.GetBlock(0, 41, 0)) < 7; seed++ {
				world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), int64(seed*20), 2)
			}
			if got := CropAge(world.GetBlock(0, 41, 0)); got != 7 {
				t.Fatalf("%s age = %d, want 7", name, got)
			}
		})
	}
}

func TestTorchflowerFinalStageConvertsBlock(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, testFarmland(7))
	world.SetBlock(0, 41, 0, testCrop("torchflower_crop", 1))
	tick := findCropTick(t, func(seed uint64) bool {
		return cropRandom(seed, 0, 41, 0, cropGateSalt, 2) != 0 &&
			cropRandom(seed, 0, 41, 0, cropGrowthSalt, CropGrowthDenominator(4)) == 0
	})
	world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), tick, 1)
	if got := world.GetBlock(0, 41, 0).ResourceLocation(); got != "minecraft:torchflower" {
		t.Fatalf("final torchflower state = %q, want minecraft:torchflower", got)
	}
}

func TestCropAvailableMoistureHydrationAndLayoutPenalty(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, testFarmland(0))
	world.SetBlock(0, 41, 0, testCrop("wheat", 0))
	if got := world.CropAvailableMoisture(0, 41, 0, world.GetBlock(0, 41, 0)); got != 2 {
		t.Fatalf("dry center moisture = %v, want 2", got)
	}
	world.SetBlock(0, 40, 0, testFarmland(7))
	if got := world.CropAvailableMoisture(0, 41, 0, world.GetBlock(0, 41, 0)); got != 4 {
		t.Fatalf("hydrated center moisture = %v, want 4", got)
	}
	world.SetBlock(1, 40, 1, testFarmland(7))
	withoutPenalty := 4.75
	if got := world.CropAvailableMoisture(0, 41, 0, world.GetBlock(0, 41, 0)); got != withoutPenalty {
		t.Fatalf("diagonal farmland moisture = %v, want %v", got, withoutPenalty)
	}
	world.SetBlock(1, 41, 1, testCrop("wheat", 0))
	if got := world.CropAvailableMoisture(0, 41, 0, world.GetBlock(0, 41, 0)); got != withoutPenalty/2 {
		t.Fatalf("crowded moisture = %v, want %v", got, withoutPenalty/2)
	}
}

func TestBeetrootAndNetherWartUseSpecialGates(t *testing.T) {
	t.Run("beetroot", func(t *testing.T) {
		world := New(&FlatGenerator{}, nil, false)
		defer world.Close()
		world.SetBlock(0, 40, 0, testFarmland(7))
		world.SetBlock(0, 41, 0, testCrop("beetroots", 0))
		failedTick := findCropTick(t, func(seed uint64) bool {
			return cropRandom(seed, 0, 41, 0, cropGateSalt, 3) != 0
		})
		world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), failedTick, 1)
		if got := CropAge(world.GetBlock(0, 41, 0)); got != 0 {
			t.Fatalf("beetroot grew through failed 1-in-3 gate: age %d", got)
		}
		passedTick := findCropTick(t, func(seed uint64) bool {
			return cropRandom(seed, 0, 41, 0, cropGateSalt, 3) == 0 &&
				cropRandom(seed, 0, 41, 0, cropGrowthSalt, CropGrowthDenominator(4)) == 0
		})
		world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), passedTick, 1)
		if got := CropAge(world.GetBlock(0, 41, 0)); got != 1 {
			t.Fatalf("beetroot age after passing both gates = %d, want 1", got)
		}
	})

	t.Run("nether_wart", func(t *testing.T) {
		world := New(&FlatGenerator{}, nil, false)
		defer world.Close()
		world.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: "soul_sand"})
		world.SetBlock(0, 41, 0, testCrop("nether_wart", 0))
		failedTick := findCropTick(t, func(seed uint64) bool {
			return cropRandom(seed, 0, 41, 0, cropGateSalt, 10) != 0
		})
		world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), failedTick, 1)
		if got := CropAge(world.GetBlock(0, 41, 0)); got != 0 {
			t.Fatalf("nether wart grew through failed 1-in-10 gate: age %d", got)
		}
		passedTick := findCropTick(t, func(seed uint64) bool {
			return cropRandom(seed, 0, 41, 0, cropGateSalt, 10) == 0
		})
		world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), passedTick, 1)
		if got := CropAge(world.GetBlock(0, 41, 0)); got != 1 {
			t.Fatalf("nether wart age after passing gate = %d, want 1", got)
		}
	})
}

func TestMatureStemsSpawnFruitAndFaceIt(t *testing.T) {
	for _, test := range []struct{ stem, fruit, attached string }{
		{stem: "pumpkin_stem", fruit: "minecraft:pumpkin", attached: "minecraft:attached_pumpkin_stem"},
		{stem: "melon_stem", fruit: "minecraft:melon", attached: "minecraft:attached_melon_stem"},
	} {
		t.Run(test.stem, func(t *testing.T) {
			world := New(&FlatGenerator{}, nil, false)
			defer world.Close()
			for dx := -1; dx <= 1; dx++ {
				for dz := -1; dz <= 1; dz++ {
					world.SetBlock(dx, 40, dz, Block{Namespace: "minecraft", Name: "dirt"})
				}
			}
			world.SetBlock(0, 40, 0, testFarmland(7))
			world.SetBlock(0, 41, 0, testCrop(test.stem, 7))
			tick := findCropTick(t, func(seed uint64) bool {
				return cropRandom(seed, 0, 41, 0, cropGrowthSalt, CropGrowthDenominator(4)) == 0
			})
			changes := world.tickCropAt(0, 41, 0, world.GetBlock(0, 41, 0), tick, 2)
			if len(changes) != 2 {
				t.Fatalf("gourd growth changes = %d, want 2", len(changes))
			}
			stem := world.GetBlock(0, 41, 0)
			if stem.ResourceLocation() != test.attached {
				t.Fatalf("stem state = %q, want %q", stem.ResourceLocation(), test.attached)
			}
			direction := horizontalCropDirections[cropRandom(uint64(tick/20), 0, 41, 0, cropDirectionSalt, 4)]
			if stem.Properties["facing"] != direction.facing {
				t.Fatalf("attached facing = %q, want %q", stem.Properties["facing"], direction.facing)
			}
			if got := world.GetBlock(direction.dx, 41, direction.dz).ResourceLocation(); got != test.fruit {
				t.Fatalf("fruit = %q, want %q", got, test.fruit)
			}
		})
	}
}

func TestAttachedStemRevertsWhenFruitRemoved(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, testFarmland(7))
	world.SetBlock(0, 41, 0, Block{Namespace: "minecraft", Name: "attached_pumpkin_stem", Properties: map[string]string{"facing": "east"}})
	world.SetBlock(1, 41, 0, Block{Namespace: "minecraft", Name: "pumpkin"})
	world.SetBlock(1, 41, 0, Air)
	changes := world.UpdateAttachedStemsAround(1, 41, 0)
	if len(changes) != 1 {
		t.Fatalf("attached stem updates = %d, want 1", len(changes))
	}
	stem := world.GetBlock(0, 41, 0)
	if stem.ResourceLocation() != "minecraft:pumpkin_stem" || CropAge(stem) != 7 {
		t.Fatalf("reverted stem = %+v, want pumpkin_stem age 7", stem)
	}
}

func TestCropBoneMealBehaviour(t *testing.T) {
	for _, test := range []struct {
		name       string
		start, max int
	}{
		{name: "wheat", start: 6, max: 7},
		{name: "carrots", start: 6, max: 7},
		{name: "potatoes", start: 6, max: 7},
		{name: "beetroots", start: 2, max: 3},
		{name: "sweet_berry_bush", start: 2, max: 3},
		{name: "pumpkin_stem", start: 6, max: 7},
		{name: "melon_stem", start: 6, max: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := New(&FlatGenerator{}, nil, false)
			defer world.Close()
			world.SetBlock(0, 40, 0, testFarmland(7))
			world.SetBlock(0, 41, 0, testCrop(test.name, test.start))
			_, used := world.ApplyBoneMeal(0, 41, 0, 1)
			if !used {
				t.Fatal("bonemeal was rejected")
			}
			if got := CropAge(world.GetBlock(0, 41, 0)); got > test.max {
				t.Fatalf("bonemeal age = %d, exceeds max %d", got, test.max)
			}
		})
	}

	t.Run("beetroot integer division", func(t *testing.T) {
		world := New(&FlatGenerator{}, nil, false)
		defer world.Close()
		world.SetBlock(0, 40, 0, testFarmland(7))
		zeroSeed := uint64(0)
		for cropRandom(zeroSeed, 0, 41, 0, cropBoneMealSalt, 4) != 0 {
			zeroSeed++
		}
		world.SetBlock(0, 41, 0, testCrop("beetroots", 0))
		changes, used := world.ApplyBoneMeal(0, 41, 0, zeroSeed)
		if !used || len(changes) != 0 || CropAge(world.GetBlock(0, 41, 0)) != 0 {
			t.Fatalf("2/3 beetroot bonemeal should be accepted with zero increase: used=%v changes=%v", used, changes)
		}
	})

	t.Run("torchflower conversion", func(t *testing.T) {
		world := New(&FlatGenerator{}, nil, false)
		defer world.Close()
		world.SetBlock(0, 40, 0, testFarmland(7))
		world.SetBlock(0, 41, 0, testCrop("torchflower_crop", 1))
		if _, used := world.ApplyBoneMeal(0, 41, 0, 1); !used {
			t.Fatal("torchflower rejected bonemeal")
		}
		if got := world.GetBlock(0, 41, 0).ResourceLocation(); got != "minecraft:torchflower" {
			t.Fatalf("bonemealed torchflower = %q", got)
		}
	})

	t.Run("nether wart rejects", func(t *testing.T) {
		world := New(&FlatGenerator{}, nil, false)
		defer world.Close()
		world.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: "soul_sand"})
		world.SetBlock(0, 41, 0, testCrop("nether_wart", 0))
		if changes, used := world.ApplyBoneMeal(0, 41, 0, 1); used || len(changes) != 0 {
			t.Fatalf("nether wart bonemeal = used %v, changes %v", used, changes)
		}
	})

	t.Run("mature stem immediately attempts fruit", func(t *testing.T) {
		world := New(&FlatGenerator{}, nil, false)
		defer world.Close()
		for dx := -1; dx <= 1; dx++ {
			for dz := -1; dz <= 1; dz++ {
				world.SetBlock(dx, 40, dz, testFarmland(7))
			}
		}
		world.SetBlock(0, 41, 0, testCrop("pumpkin_stem", 6))
		stem := world.GetBlock(0, 41, 0)
		denominator := CropGrowthDenominator(world.CropAvailableMoisture(0, 41, 0, stem))
		seed := uint64(0)
		for cropRandom(seed, 0, 41, 0, cropGrowthSalt, denominator) != 0 {
			seed++
		}
		if _, used := world.ApplyBoneMeal(0, 41, 0, seed); !used {
			t.Fatal("stem rejected bonemeal")
		}
		if got := world.GetBlock(0, 41, 0).ResourceLocation(); got != "minecraft:attached_pumpkin_stem" {
			t.Fatalf("bonemealed mature stem = %q, want attached stem", got)
		}
	})
}

func TestCropSupportRemovalAndSweetBerryHarvest(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, testFarmland(7))
	world.SetBlock(0, 41, 0, testCrop("wheat", 4))
	world.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: "dirt"})
	if changes := world.BreakUnsupportedCropsAbove(0, 40, 0); len(changes) != 1 || !world.GetBlock(0, 41, 0).IsAir() {
		t.Fatalf("unsupported wheat changes = %v, block = %+v", changes, world.GetBlock(0, 41, 0))
	}

	world.SetBlock(2, 40, 0, Block{Namespace: "minecraft", Name: "dirt"})
	world.SetBlock(2, 41, 0, testCrop("sweet_berry_bush", 3))
	count, changes, harvested := world.HarvestSweetBerryBush(2, 41, 0, 1)
	if !harvested || count < 2 || count > 3 || len(changes) != 1 || CropAge(world.GetBlock(2, 41, 0)) != 1 {
		t.Fatalf("berry harvest: harvested=%v count=%d changes=%v bush=%+v", harvested, count, changes, world.GetBlock(2, 41, 0))
	}
}

func TestEveryCropSupportRule(t *testing.T) {
	tests := []struct {
		crop, support string
	}{
		{"wheat", "farmland"}, {"carrots", "farmland"}, {"potatoes", "farmland"},
		{"beetroots", "farmland"}, {"pumpkin_stem", "farmland"}, {"melon_stem", "farmland"},
		{"attached_pumpkin_stem", "farmland"}, {"attached_melon_stem", "farmland"},
		{"torchflower_crop", "farmland"}, {"nether_wart", "soul_sand"},
		{"sweet_berry_bush", "dirt"},
	}
	for _, test := range tests {
		t.Run(test.crop, func(t *testing.T) {
			w := New(&FlatGenerator{}, nil, false)
			defer w.Close()
			w.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: test.support})
			crop := testCrop(test.crop, 0)
			w.SetBlock(0, 41, 0, crop)
			if !CanCropSurvive(crop, w.GetBlock(0, 40, 0)) {
				t.Fatalf("%s rejected %s support", test.crop, test.support)
			}
			w.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: "glass"})
			if changes := w.BreakUnsupportedCropsAbove(0, 40, 0); len(changes) != 1 || !w.GetBlock(0, 41, 0).IsAir() {
				t.Fatalf("unsupported crop remained: changes=%v block=%s", changes, w.GetBlock(0, 41, 0).Key())
			}
		})
	}
}

func TestCocoaBreaksWhenJungleLogRemoved(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	// Place a jungle log and a cocoa pod facing the log (east, so log is at x+1).
	w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: "jungle_log"})
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "cocoa",
		Properties: map[string]string{"facing": "east", "age": "0"}})
	// Remove the log; cocoa at (0,64,0) should break.
	w.SetBlock(1, 64, 0, Air)
	changes := w.BreakUnsupportedCocoaAdjacentTo(1, 64, 0)
	if len(changes) != 1 || !w.GetBlock(0, 64, 0).IsAir() {
		t.Fatalf("cocoa did not break: changes=%v block=%s", changes, w.GetBlock(0, 64, 0).Key())
	}
}

func TestGrassBoneMealScattersVegetation(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	// Place a large grass platform so 128 scatter attempts have many valid spots.
	for dx := -4; dx <= 4; dx++ {
		for dz := -4; dz <= 4; dz++ {
			w.SetBlock(dx, 64, dz, Block{Namespace: "minecraft", Name: "grass_block"})
		}
	}
	changes, used := w.ApplyBoneMeal(0, 64, 0, 12345)
	if !used {
		t.Fatal("ApplyBoneMeal on grass_block returned used=false")
	}
	if len(changes) == 0 {
		t.Fatal("ApplyBoneMeal on grass_block produced no plant changes")
	}
	for _, c := range changes {
		b := w.GetBlock(c.X, c.Y, c.Z)
		if b.IsAir() {
			t.Fatalf("plant block at (%d,%d,%d) is air after bone meal scatter", c.X, c.Y, c.Z)
		}
	}
}

func TestMyceliumBoneMealScattersMushrooms(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	for dx := -4; dx <= 4; dx++ {
		for dz := -4; dz <= 4; dz++ {
			w.SetBlock(dx, 64, dz, Block{Namespace: "minecraft", Name: "mycelium"})
		}
	}
	changes, used := w.ApplyBoneMeal(0, 64, 0, 99999)
	if !used {
		t.Fatal("ApplyBoneMeal on mycelium returned used=false")
	}
	if len(changes) == 0 {
		t.Fatal("ApplyBoneMeal on mycelium placed no mushrooms")
	}
	for _, c := range changes {
		name := c.Block.ResourceLocation()
		if name != "minecraft:brown_mushroom" && name != "minecraft:red_mushroom" {
			t.Fatalf("unexpected block %q placed by mycelium bone meal", name)
		}
	}
}

func TestMossBoneMealSpreadsAndConverts(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	// Place dirt blocks around the central moss block so the scatter has targets.
	for dx := -4; dx <= 4; dx++ {
		for dz := -4; dz <= 4; dz++ {
			w.SetBlock(dx, 64, dz, Block{Namespace: "minecraft", Name: "dirt"})
		}
	}
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "moss_block"})
	changes, used := w.ApplyBoneMeal(0, 64, 0, 77777)
	if !used {
		t.Fatal("ApplyBoneMeal on moss_block returned used=false")
	}
	if len(changes) == 0 {
		t.Fatal("ApplyBoneMeal on moss_block produced no changes")
	}
	// Some non-moss surface should have been converted.
	anyMoss := false
	for _, c := range changes {
		if c.Block.ResourceLocation() == "minecraft:moss_block" {
			anyMoss = true
		}
	}
	if !anyMoss {
		t.Fatal("moss bone meal did not convert any surface to moss_block")
	}
}

func TestCoveredCropUsesCurrentPumpkinLightTODOBehaviour(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, testFarmland(7))
	world.SetBlock(0, 41, 0, testCrop("wheat", 0))
	world.SetBlock(0, 42, 0, Block{Namespace: "minecraft", Name: "stone"})
	tick := findCropTick(t, func(seed uint64) bool {
		return cropRandom(seed, 0, 41, 0, cropGrowthSalt, CropGrowthDenominator(4)) == 0
	})
	world.TickCrops(tick, 1)
	if got := CropAge(world.GetBlock(0, 41, 0)); got != 1 {
		t.Fatalf("covered wheat age = %d, want 1 while Pumpkin light check remains TODO", got)
	}
}

func TestSweetBerryGrowthStopsUnderSolidBlock(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: "dirt"})
	w.SetBlock(0, 41, 0, testCrop("sweet_berry_bush", 1))
	w.SetBlock(0, 42, 0, Block{Namespace: "minecraft", Name: "stone"})
	seed := uint64(0)
	for cropRandom(seed, 0, 41, 0, cropGateSalt, 5) != 0 {
		seed++
	}
	w.TickCrops(int64(seed*20), 1)
	if got := CropAge(w.GetBlock(0, 41, 0)); got != 1 {
		t.Fatalf("obstructed berry age = %d, want 1", got)
	}
	w.SetBlock(0, 42, 0, Air)
	w.TickCrops(int64(seed*20), 1)
	if got := CropAge(w.GetBlock(0, 41, 0)); got != 2 {
		t.Fatalf("unobstructed berry age = %d, want 2", got)
	}
}

func TestCropGrowthDenominator(t *testing.T) {
	if got := CropGrowthDenominator(2); got != 13 {
		t.Fatalf("dry denominator = %d, want 13", got)
	}
	if got := CropGrowthDenominator(4); got != 7 {
		t.Fatalf("hydrated denominator = %d, want 7", got)
	}
	if got := CropGrowthDenominator(10); got != 3 {
		t.Fatalf("maximum farmland denominator = %d, want 3", got)
	}
	if math.IsNaN(float64(CropGrowthDenominator(0))) {
		t.Fatal("zero moisture produced NaN")
	}
}

func TestBoneMealMutationsReachBlockObserver(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, testFarmland(7))
	world.SetBlock(0, 41, 0, testCrop("wheat", 0))
	var observed []BlockChange
	world.SetBlockObserver(func(change BlockChange) {
		observed = append(observed, change)
	})
	changes, used := world.ApplyBoneMeal(0, 41, 0, 1)
	if !used || len(changes) != 1 || len(observed) != 1 {
		t.Fatalf("bonemeal observer: used=%v changes=%v observed=%v", used, changes, observed)
	}
	if observed[0].Block.Key() != changes[0].Block.Key() {
		t.Fatalf("observed block %s != returned change %s", observed[0].Block.Key(), changes[0].Block.Key())
	}
}
