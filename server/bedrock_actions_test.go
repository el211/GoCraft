package server

import (
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/session"
)

func newBedrockActionTestServer(t *testing.T) (*Server, *player.Player) {
	t.Helper()
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	t.Cleanup(func() { w.Close() })
	g := game.New()
	p := player.New([16]byte{31}, "bedrock-builder", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Position = spatial.Vec3{X: 0.5, Y: 64, Z: 0.5}
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	return &Server{game: g, world: w, sessions: session.NewManager()}, p
}

func TestEveryHoeTillsBedrockDirtAndFarmlandHydrates(t *testing.T) {
	hoes := []string{"wooden_hoe", "stone_hoe", "iron_hoe", "golden_hoe", "diamond_hoe", "netherite_hoe"}
	for index, hoe := range hoes {
		t.Run(hoe, func(t *testing.T) {
			s, p := newBedrockActionTestServer(t)
			x := index + 1
			s.world.SetBlock(x, 64, 0, bedrockBlock("dirt", nil))
			s.world.SetBlock(x+4, 64, 0, coreworld.MakeFluid("minecraft:water", 0))
			p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:" + hoe, Count: 1}

			used := s.applyBedrockItemAction(p, intent.BlockInteractIntent{
				Position: spatial.BlockPos{X: int32(x), Y: 64, Z: 0}, Face: 1,
			}, s.world.GetBlock(x, 64, 0))
			if !used {
				t.Fatal("hoe click was not handled")
			}
			farmland := s.world.GetBlock(x, 64, 0)
			if farmland.ResourceLocation() != "minecraft:farmland" || farmland.Properties["moisture"] != "0" {
				t.Fatalf("tilled block = %+v, want dry farmland", farmland)
			}
			if got := p.Inventory[player.HotbarStart].Damage; got != 1 {
				t.Fatalf("hoe damage = %d, want 1", got)
			}
			for tick := int64(20); tick <= 400 && s.world.GetBlock(x, 64, 0).Properties["moisture"] != "7"; tick += 20 {
				s.world.TickFarmland(tick, 64)
			}
			if got := s.world.GetBlock(x, 64, 0).Properties["moisture"]; got != "7" {
				t.Fatalf("farmland moisture = %q, want 7 with nearby water", got)
			}
		})
	}
}

func TestBedrockRootedDirtHoeDropsRoots(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("rooted_dirt", nil))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:wooden_hoe", Count: 1}
	if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{
		Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 0,
	}, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("rooted dirt was not handled from its underside")
	}
	if got := s.world.GetBlock(1, 64, 0).ResourceLocation(); got != "minecraft:dirt" {
		t.Fatalf("rooted dirt became %q, want dirt", got)
	}
	for _, stack := range p.Inventory {
		if stack.ItemID == "minecraft:hanging_roots" && stack.Count == 1 {
			return
		}
	}
	t.Fatal("hanging roots were not awarded")
}

func TestBedrockPlantsEverySupportedCrop(t *testing.T) {
	tests := []struct {
		item, support, crop string
	}{
		{item: "minecraft:wheat_seeds", support: "farmland", crop: "minecraft:wheat"},
		{item: "minecraft:carrot", support: "farmland", crop: "minecraft:carrots"},
		{item: "minecraft:potato", support: "farmland", crop: "minecraft:potatoes"},
		{item: "minecraft:beetroot_seeds", support: "farmland", crop: "minecraft:beetroots"},
		{item: "minecraft:melon_seeds", support: "farmland", crop: "minecraft:melon_stem"},
		{item: "minecraft:pumpkin_seeds", support: "farmland", crop: "minecraft:pumpkin_stem"},
		{item: "minecraft:torchflower_seeds", support: "farmland", crop: "minecraft:torchflower_crop"},
		{item: "minecraft:nether_wart", support: "soul_sand", crop: "minecraft:nether_wart"},
	}
	for _, test := range tests {
		t.Run(test.item, func(t *testing.T) {
			s, p := newBedrockActionTestServer(t)
			s.world.SetBlock(1, 64, 0, bedrockBlock(test.support, map[string]string{"moisture": "0"}))
			p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: test.item, Count: 1}
			if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{
				Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 1,
			}, s.world.GetBlock(1, 64, 0)) {
				t.Fatal("planting action was not handled")
			}
			placed := s.world.GetBlock(1, 65, 0)
			if placed.ResourceLocation() != test.crop || coreworld.CropAge(placed) != 0 {
				t.Fatalf("placed crop = %+v, want %s age 0", placed, test.crop)
			}
		})
	}
}

func TestBedrockCropBoneMealMatchesCoreAndRejectsNetherWart(t *testing.T) {
	t.Run("wheat advances instead of instantly maturing", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 64, 0, bedrockBlock("farmland", map[string]string{"moisture": "7"}))
		s.world.SetBlock(1, 65, 0, bedrockBlock("wheat", map[string]string{"age": "0"}))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:bone_meal", Count: 2}
		if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 65, Z: 0}}, s.world.GetBlock(1, 65, 0)) {
			t.Fatal("bone meal action was not handled")
		}
		if got := coreworld.CropAge(s.world.GetBlock(1, 65, 0)); got < 2 || got > 5 {
			t.Fatalf("wheat age = %d, want 2..5", got)
		}
		if got := p.HeldItem().Count; got != 1 {
			t.Fatalf("bone meal count = %d, want 1", got)
		}
	})

	t.Run("nether wart rejects", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 64, 0, bedrockBlock("soul_sand", nil))
		s.world.SetBlock(1, 65, 0, bedrockBlock("nether_wart", map[string]string{"age": "0"}))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:bone_meal", Count: 2}
		if s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 65, Z: 0}}, s.world.GetBlock(1, 65, 0)) {
			t.Fatal("nether wart accepted bone meal")
		}
		if got := p.HeldItem().Count; got != 2 {
			t.Fatalf("bone meal count = %d, want 2", got)
		}
	})
}

func TestBedrockBoneMealWorksOnGrassAndSaplings(t *testing.T) {
	t.Run("grass", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		// Provide a 9×9 bed of grass blocks so the scatter has eligible tiles.
		for dx := -4; dx <= 4; dx++ {
			for dz := -4; dz <= 4; dz++ {
				s.world.SetBlock(8+dx, 64, 8+dz, bedrockBlock("grass_block", nil))
			}
		}
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:bone_meal", Count: 1}
		if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 8, Y: 64, Z: 8}}, s.world.GetBlock(8, 64, 8)) {
			t.Fatal("grass rejected bone meal")
		}
		// At least one vegetation block should have been placed somewhere in the ±4 area.
		placed := false
		for dx := -4; dx <= 4 && !placed; dx++ {
			for dz := -4; dz <= 4 && !placed; dz++ {
				if b := s.world.GetBlock(8+dx, 65, 8+dz); !b.IsAir() {
					placed = true
				}
			}
		}
		if !placed {
			t.Fatal("grass bone meal placed nothing in ±4 area")
		}
	})

	t.Run("sapling", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(8, 63, 8, bedrockBlock("dirt", nil))
		s.world.SetBlock(8, 64, 8, bedrockBlock("oak_sapling", map[string]string{"stage": "1"}))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:bone_meal", Count: 1}
		if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 8, Y: 64, Z: 8}}, s.world.GetBlock(8, 64, 8)) {
			t.Fatal("sapling rejected bone meal")
		}
		if got := s.world.GetBlock(8, 64, 8).ResourceLocation(); got != "minecraft:oak_log" {
			t.Fatalf("sapling became %q, want oak log", got)
		}
	})
}

func TestBedrockHarvestsSweetBerryBush(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("dirt", nil))
	s.world.SetBlock(1, 65, 0, bedrockBlock("sweet_berry_bush", map[string]string{"age": "3"}))
	if !s.applyBedrockBlockActivation(p, spatial.BlockPos{X: 1, Y: 65, Z: 0}, s.world.GetBlock(1, 65, 0)) {
		t.Fatal("sweet berry harvest was not handled")
	}
	if got := coreworld.CropAge(s.world.GetBlock(1, 65, 0)); got != 1 {
		t.Fatalf("harvested bush age = %d, want 1", got)
	}
	berries := 0
	for _, stack := range p.Inventory {
		if stack.ItemID == "minecraft:sweet_berries" {
			berries += stack.Count
		}
	}
	if berries < 2 || berries > 3 {
		t.Fatalf("harvest berries = %d, want 2..3", berries)
	}
}

func TestBedrockPlacesFoodOnCampfire(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("soul_campfire", map[string]string{"lit": "true"}))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:chicken", Count: 2}
	if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("campfire food placement was not handled")
	}
	items := s.world.ContainerItems(1, 64, 0)
	if len(items) != 1 || items[0].ItemID != "minecraft:chicken" || items[0].Count != 1 {
		t.Fatalf("campfire items = %+v", items)
	}
	if got := p.HeldItem().Count; got != 1 {
		t.Fatalf("held chicken = %d, want 1", got)
	}
}

func TestBedrockTorchChoosesFloorAndWallStates(t *testing.T) {
	t.Run("floor", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:torch", Count: 2}
		if !s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
		}, s.world.GetBlock(1, 63, 0)) {
			t.Fatal("floor torch click was not handled")
		}
		if got := s.world.GetBlock(1, 64, 0).ResourceLocation(); got != "minecraft:torch" {
			t.Fatalf("placed %q, want floor torch", got)
		}
		if got := p.Inventory[player.HotbarStart].Count; got != 1 {
			t.Fatalf("torch count = %d, want 1", got)
		}
	})

	t.Run("wall", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 64, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:torch", Count: 1}
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 2,
		}, s.world.GetBlock(1, 64, 0))
		placed := s.world.GetBlock(1, 64, -1)
		if placed.ResourceLocation() != "minecraft:wall_torch" || placed.Properties["facing"] != "north" {
			t.Fatalf("wall torch = %+v, want north-facing wall torch", placed)
		}
	})
}

func TestBedrockDirectionalAndRedstonePlacementParity(t *testing.T) {
	t.Run("repeater faces placing player", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		p.Rotation.Yaw = 0
		s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:repeater", Count: 1}
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
		}, s.world.GetBlock(1, 63, 0))
		if got := s.world.GetBlock(1, 64, 0).Properties["facing"]; got != "north" {
			t.Fatalf("repeater facing = %q, want north while player looks south", got)
		}
	})

	t.Run("wood follows clicked axis", func(t *testing.T) {
		for face, wantAxis := range map[int32]string{1: "y", 2: "z", 5: "x"} {
			s, p := newBedrockActionTestServer(t)
			s.world.SetBlock(1, 64, 0, bedrockBlock("stone", nil))
			p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:stripped_oak_wood", Count: 1}
			s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
				Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: face,
			}, s.world.GetBlock(1, 64, 0))
			dx, dy, dz := bedrockFaceOffset(face)
			if got := s.world.GetBlock(1+dx, 64+dy, dz).Properties["axis"]; got != wantAxis {
				t.Fatalf("face %d wood axis = %q, want %q", face, got, wantAxis)
			}
		}
	})

	t.Run("wire cannot stack on wire", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:redstone", Count: 2}
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
		}, s.world.GetBlock(1, 63, 0))
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 1,
		}, s.world.GetBlock(1, 64, 0))
		if got := s.world.GetBlock(1, 65, 0); !got.IsAir() {
			t.Fatalf("wire stacked into air as %+v", got)
		}
	})
}

func TestBedrockPlacesLitRedstoneTorch(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	p.GameMode = player.GameModeCreative
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:redstone_torch", Count: 1}
	s.world.SetBlock(0, 63, 0, bedrockBlock("stone", nil))
	s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
		Position: spatial.BlockPos{X: 0, Y: 63, Z: 0}, Face: 1,
	}, s.world.GetBlock(0, 63, 0))
	torch := s.world.GetBlock(0, 64, 0)
	if torch.ResourceLocation() != "minecraft:redstone_torch" || torch.Properties["lit"] != "true" {
		t.Fatalf("torch state = %s", torch.Key())
	}
}

func TestBedrockPlacesMinecartOnRail(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(0, 64, 0, bedrockBlock("rail", map[string]string{"shape": "east_west"}))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:minecart", Count: 1}
	if !s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 0, Y: 64, Z: 0}}, s.world.GetBlock(0, 64, 0)) {
		t.Fatal("minecart placement was not handled")
	}
	entities := s.world.Entities.Snapshot()
	if len(entities) != 1 || entities[0].Type != corentity.TypeMinecart {
		t.Fatalf("minecart entities = %+v", entities)
	}
	if !p.HeldItem().IsEmpty() {
		t.Fatalf("held after placement = %+v", p.HeldItem())
	}
}

func TestBedrockMechanismsToggleAndButtonReleases(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("lever", map[string]string{"face": "wall", "facing": "north", "powered": "false"}))
	if !s.applyBedrockBlockActivation(p, spatial.BlockPos{X: 1, Y: 64, Z: 0}, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("lever activation was not handled")
	}
	if got := s.world.GetBlock(1, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("lever powered = %q, want true", got)
	}

	s.world.SetBlock(2, 64, 0, bedrockBlock("stone_button", map[string]string{"face": "wall", "facing": "north", "powered": "false"}))
	s.applyBedrockBlockActivation(p, spatial.BlockPos{X: 2, Y: 64, Z: 0}, s.world.GetBlock(2, 64, 0))
	if got := s.world.GetBlock(2, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("button powered = %q, want true", got)
	}
	s.worldAge = 20
	s.tickBlockPhysics()
	if got := s.world.GetBlock(2, 64, 0).Properties["powered"]; got != "false" {
		t.Fatalf("button powered after 20 ticks = %q, want false", got)
	}

	s.world.SetBlock(3, 64, 0, bedrockBlock("oak_trapdoor", map[string]string{"facing": "north", "half": "bottom", "open": "false", "powered": "false"}))
	s.applyBedrockBlockActivation(p, spatial.BlockPos{X: 3, Y: 64, Z: 0}, s.world.GetBlock(3, 64, 0))
	if got := s.world.GetBlock(3, 64, 0).Properties["open"]; got != "true" {
		t.Fatalf("trapdoor open = %q, want true", got)
	}
}

func TestBedrockWallsAndFenceGatesRefreshFromNeighbours(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	wall := bedrockBlock("cobblestone_wall", map[string]string{"waterlogged": "false"})
	s.setBedrockActionBlock(0, 64, 0, wall)
	s.setBedrockActionBlock(2, 64, 0, wall)
	gate := bedrockBlock("spruce_fence_gate", map[string]string{
		"facing": "north", "in_wall": "false", "open": "false", "powered": "false",
	})
	s.setBedrockActionBlock(1, 64, 0, gate)

	left := s.world.GetBlock(0, 64, 0)
	right := s.world.GetBlock(2, 64, 0)
	gotGate := s.world.GetBlock(1, 64, 0)
	if left.Properties["east"] != "low" || right.Properties["west"] != "low" {
		t.Fatalf("wall connections = left %v right %v", left.Properties, right.Properties)
	}
	if gotGate.Properties["in_wall"] != "true" {
		t.Fatalf("gate in_wall = %q, want true", gotGate.Properties["in_wall"])
	}

	if !s.applyBedrockBlockActivation(p, spatial.BlockPos{X: 1, Y: 64, Z: 0}, gotGate) {
		t.Fatal("spruce fence gate activation was not handled")
	}
	if got := s.world.GetBlock(1, 64, 0).Properties["open"]; got != "true" {
		t.Fatalf("gate open after one click = %q, want true", got)
	}

	// Placing a full block above a connected wall changes its arms to tall but
	// must never overwrite the wall itself (the reported glass/wall corruption).
	s.setBedrockActionBlock(0, 65, 0, bedrockBlock("glass", nil))
	left = s.world.GetBlock(0, 64, 0)
	if left.ResourceLocation() != "minecraft:cobblestone_wall" {
		t.Fatalf("block below glass became %q, want cobblestone wall", left.ResourceLocation())
	}
	if left.Properties["east"] != "tall" {
		t.Fatalf("wall connection below glass = %q, want tall", left.Properties["east"])
	}
}

func TestBedrockAdjacentFencesRemainDistinctBlocks(t *testing.T) {
	s, _ := newBedrockActionTestServer(t)
	s.setBedrockActionBlock(0, 64, 0, bedrockBlock("oak_fence", nil))
	s.setBedrockActionBlock(1, 64, 0, bedrockBlock("oak_fence", nil))
	left, right := s.world.GetBlock(0, 64, 0), s.world.GetBlock(1, 64, 0)
	if left.ResourceLocation() != "minecraft:oak_fence" || right.ResourceLocation() != "minecraft:oak_fence" {
		t.Fatalf("adjacent fences corrupted: %+v / %+v", left, right)
	}
}

func TestBedrockPlacesDoorBedAndRedstoneDust(t *testing.T) {
	t.Run("door", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:oak_door", Count: 1}
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1, ClickX: 0.25, ClickZ: 0.5,
		}, s.world.GetBlock(1, 63, 0))
		lower, upper := s.world.GetBlock(1, 64, 0), s.world.GetBlock(1, 65, 0)
		if lower.ResourceLocation() != "minecraft:oak_door" || lower.Properties["half"] != "lower" || upper.Properties["half"] != "upper" {
			t.Fatalf("door halves = %+v / %+v", lower, upper)
		}
	})

	t.Run("bed", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:white_bed", Count: 1}
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
		}, s.world.GetBlock(1, 63, 0))
		foot := s.world.GetBlock(1, 64, 0)
		dx, dz := bedrockHorizontalOffset(foot.Properties["facing"])
		head := s.world.GetBlock(1+dx, 64, dz)
		if foot.Properties["part"] != "foot" || head.ResourceLocation() != "minecraft:white_bed" || head.Properties["part"] != "head" {
			t.Fatalf("bed halves = %+v / %+v", foot, head)
		}
		found := map[[3]int]bool{}
		for _, entity := range s.world.Chunk(0, 0).BlockEntities {
			if entity.Type == "minecraft:bed" {
				found[[3]int{entity.X, entity.Y, entity.Z}] = true
			}
		}
		if !found[[3]int{1, 64, 0}] || !found[[3]int{1 + dx, 64, dz}] {
			t.Fatalf("Bedrock bed block entities = %+v", found)
		}
	})

	t.Run("redstone", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:redstone", Count: 1}
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
		}, s.world.GetBlock(1, 63, 0))
		wire := s.world.GetBlock(1, 64, 0)
		if wire.ResourceLocation() != "minecraft:redstone_wire" || wire.Properties["power"] != "0" {
			t.Fatalf("redstone placement = %+v", wire)
		}
	})
}

func TestBedrockLegacyCreativeDoorPlacesBothHalvesAndDuplicateUseTogglesOnce(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	p.GameMode = player.GameModeCreative
	s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:wooden_door", Count: 1}
	place := intent.BlockInteractIntent{
		PlayerUUID: p.UUID, Action: intent.BlockActionUse,
		Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1, HotbarSlot: 0,
		ClickX: 0.25, ClickZ: 0.5,
	}
	s.applyBedrockBlockInteract(place)
	s.applyBedrockBlockInteract(place) // PlayerAuthInput/InventoryTransaction duplicate.

	lower, upper := s.world.GetBlock(1, 64, 0), s.world.GetBlock(1, 65, 0)
	if lower.ResourceLocation() != "minecraft:oak_door" || lower.Properties["half"] != "lower" ||
		upper.ResourceLocation() != "minecraft:oak_door" || upper.Properties["half"] != "upper" {
		t.Fatalf("legacy creative door halves = %+v / %+v", lower, upper)
	}

	use := intent.BlockInteractIntent{
		PlayerUUID: p.UUID, Action: intent.BlockActionUse,
		Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 2, HotbarSlot: 0,
	}
	s.applyBedrockBlockInteract(use)
	// Current Bedrock may repeat the same physical click against the other
	// door half with a different face in PlayerAuthInput.
	use.Position.Y = 65
	use.Face = 3
	s.applyBedrockBlockInteract(use)
	lower, upper = s.world.GetBlock(1, 64, 0), s.world.GetBlock(1, 65, 0)
	if lower.Properties["open"] != "true" || upper.Properties["open"] != "true" {
		t.Fatalf("duplicate door click toggled twice or desynchronised halves: lower=%v upper=%v", lower.Properties, upper.Properties)
	}
}

func TestBedrockLightBlockKeepsSelectedLevel(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:light", Count: 1, Damage: 11}
	if !s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
		Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
	}, s.world.GetBlock(1, 63, 0)) {
		t.Fatal("light block click was not handled")
	}
	light := s.world.GetBlock(1, 64, 0)
	if light.ResourceLocation() != "minecraft:light" || light.Properties["level"] != "11" {
		t.Fatalf("placed light = %+v, want level 11", light)
	}
}

func TestBedrockAttachedBlocksRequireSupport(t *testing.T) {
	for _, item := range []string{"minecraft:torch", "minecraft:lever", "minecraft:stone_button", "minecraft:ladder"} {
		t.Run(item, func(t *testing.T) {
			s, p := newBedrockActionTestServer(t)
			p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: item, Count: 1}
			s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
				Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 2,
			}, s.world.GetBlock(1, 64, 0))
			if got := s.world.GetBlock(1, 64, 0); !got.IsAir() {
				t.Fatalf("unsupported item placed as %+v", got)
			}
			if got := p.Inventory[player.HotbarStart].Count; got != 1 {
				t.Fatalf("unsupported placement consumed item; count=%d", got)
			}
		})
	}
}

func TestSneakingPlacesHeldBlockInsteadOfActivatingMechanism(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	p.Sneaking = true
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:stone", Count: 1}
	s.world.SetBlock(1, 64, 0, bedrockBlock("lever", map[string]string{"face": "floor", "facing": "north", "powered": "false"}))
	s.applyBedrockBlockInteract(intent.BlockInteractIntent{
		PlayerUUID: p.UUID, Action: intent.BlockActionUse,
		Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}, Face: 1, HotbarSlot: 0,
	})
	if got := s.world.GetBlock(1, 64, 0).Properties["powered"]; got != "false" {
		t.Fatalf("lever powered = %q, want false while bypassing activation", got)
	}
	if got := s.world.GetBlock(1, 65, 0).ResourceLocation(); got != "minecraft:stone" {
		t.Fatalf("sneak placement = %q, want minecraft:stone", got)
	}
}

func TestBedrockRedstoneDustConnectsToNeighbour(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	for x := 1; x <= 2; x++ {
		s.world.SetBlock(x, 63, 0, bedrockBlock("stone", nil))
	}
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:redstone", Count: 2}
	for x := 1; x <= 2; x++ {
		s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
			Position: spatial.BlockPos{X: int32(x), Y: 63, Z: 0}, Face: 1,
		}, s.world.GetBlock(x, 63, 0))
	}
	left, right := s.world.GetBlock(1, 64, 0), s.world.GetBlock(2, 64, 0)
	if left.Properties["east"] != "side" || right.Properties["west"] != "side" {
		t.Fatalf("wire connections = left %v right %v", left.Properties, right.Properties)
	}
}

func TestBedrockPumpkinShearsAndComposterLifecycle(t *testing.T) {
	t.Run("pumpkin", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 64, 0, bedrockBlock("pumpkin", nil))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:shears", Count: 1}
		if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, s.world.GetBlock(1, 64, 0)) {
			t.Fatal("shears did not carve pumpkin")
		}
		if got := s.world.GetBlock(1, 64, 0).ResourceLocation(); got != "minecraft:carved_pumpkin" {
			t.Fatalf("pumpkin became %q", got)
		}
		var seeds int
		for _, stack := range p.Inventory {
			if stack.ItemID == "minecraft:pumpkin_seeds" {
				seeds += stack.Count
			}
		}
		if seeds != 4 {
			t.Fatalf("pumpkin seeds = %d, want 4", seeds)
		}
	})

	t.Run("composter", func(t *testing.T) {
		s, p := newBedrockActionTestServer(t)
		s.world.SetBlock(1, 64, 0, bedrockBlock("composter", map[string]string{"level": "0"}))
		p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:wheat_seeds", Count: 1}
		s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, s.world.GetBlock(1, 64, 0))
		if got := s.world.GetBlock(1, 64, 0).Properties["level"]; got != "1" {
			t.Fatalf("first compost level = %q, want 1", got)
		}

		s.world.SetBlock(1, 64, 0, bedrockBlock("composter", map[string]string{"level": "7"}))
		s.world.BlockPhysics.ScheduleComposter(1, 64, 0, 0, 20)
		s.worldAge = 20
		s.tickBlockPhysics()
		if got := s.world.GetBlock(1, 64, 0).Properties["level"]; got != "8" {
			t.Fatalf("ready compost level = %q, want 8", got)
		}
		s.applyBedrockBlockActivation(p, spatial.BlockPos{X: 1, Y: 64, Z: 0}, s.world.GetBlock(1, 64, 0))
		if got := s.world.GetBlock(1, 64, 0).Properties["level"]; got != "0" {
			t.Fatalf("emptied compost level = %q, want 0", got)
		}
	})
}

func TestBedrockHarvestsFullBeehive(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("beehive", map[string]string{"honey_level": "5", "facing": "east"}))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:glass_bottle", Count: 1}
	if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("glass bottle did not harvest the beehive")
	}
	hive := s.world.GetBlock(1, 64, 0)
	if hive.Properties["honey_level"] != "0" || hive.Properties["facing"] != "east" {
		t.Fatalf("harvested hive = %+v", hive)
	}
	if stack := p.Inventory[player.HotbarStart]; stack.ItemID != "minecraft:honey_bottle" || stack.Count != 1 {
		t.Fatalf("harvest result = %+v", stack)
	}
}

func TestBedrockAddsCandleToCake(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("cake", map[string]string{"bites": "0"}))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:red_candle", Count: 1}
	if !s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("candle was not added to cake")
	}
	if got := s.world.GetBlock(1, 64, 0); got.ResourceLocation() != "minecraft:red_candle_cake" || got.Properties["lit"] != "false" {
		t.Fatalf("candle cake = %+v", got)
	}
}

func TestBedrockFlowerPotInsertAndRemove(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("flower_pot", nil))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:azalea", Count: 1}
	interaction := intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}
	if !s.applyBedrockItemAction(p, interaction, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("azalea was not inserted into flower pot")
	}
	if got := s.world.GetBlock(1, 64, 0).ResourceLocation(); got != "minecraft:potted_azalea_bush" {
		t.Fatalf("potted block = %q", got)
	}
	if !s.applyBedrockItemAction(p, interaction, s.world.GetBlock(1, 64, 0)) {
		t.Fatal("azalea was not removed from flower pot")
	}
	if got := s.world.GetBlock(1, 64, 0).ResourceLocation(); got != "minecraft:flower_pot" {
		t.Fatalf("emptied pot = %q", got)
	}
	if p.HeldItem().ItemID != "minecraft:azalea" {
		t.Fatalf("returned plant = %+v", p.HeldItem())
	}
}

func TestBedrockBucketFillsAndEmptiesCauldron(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 64, 0, bedrockBlock("cauldron", nil))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:water_bucket", Count: 1}
	s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, s.world.GetBlock(1, 64, 0))
	filled := s.world.GetBlock(1, 64, 0)
	if filled.ResourceLocation() != "minecraft:water_cauldron" || filled.Properties["level"] != "3" {
		t.Fatalf("filled cauldron = %+v", filled)
	}
	if got := p.Inventory[player.HotbarStart].ItemID; got != "minecraft:bucket" {
		t.Fatalf("held item after filling = %q, want bucket", got)
	}
	s.applyBedrockItemAction(p, intent.BlockInteractIntent{Position: spatial.BlockPos{X: 1, Y: 64, Z: 0}}, filled)
	if got := s.world.GetBlock(1, 64, 0).ResourceLocation(); got != "minecraft:cauldron" {
		t.Fatalf("emptied cauldron = %q", got)
	}
	if got := p.Inventory[player.HotbarStart].ItemID; got != "minecraft:water_bucket" {
		t.Fatalf("held item after emptying = %q, want water bucket", got)
	}
}

func TestBedrockPressurePlateTracksPlayerOccupancy(t *testing.T) {
	s, p := newBedrockActionTestServer(t)
	s.world.SetBlock(1, 63, 0, bedrockBlock("stone", nil))
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:oak_pressure_plate", Count: 1}
	s.placeBedrockHeldBlock(p, intent.BlockInteractIntent{
		Position: spatial.BlockPos{X: 1, Y: 63, Z: 0}, Face: 1,
	}, s.world.GetBlock(1, 63, 0))
	p.Position = spatial.Vec3{X: 1.5, Y: 65, Z: 0.5}
	s.worldAge = 1
	s.tickBlockPhysics()
	if got := s.world.GetBlock(1, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("occupied pressure plate powered = %q, want true", got)
	}
	p.Position = spatial.Vec3{X: 4, Y: 65, Z: 4}
	s.worldAge = 3
	s.tickBlockPhysics()
	if got := s.world.GetBlock(1, 64, 0).Properties["powered"]; got != "false" {
		t.Fatalf("empty pressure plate powered = %q, want false", got)
	}
}
