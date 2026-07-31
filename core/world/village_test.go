package world

import (
	"testing"

	"GoCraft/core/entity"
	"GoCraft/core/spatial"
)

func TestVillageHouseHasBiomeDoorRoofBedAndWorkstation(t *testing.T) {
	generator := NewOverworldGenerator(1)
	chunk := &Chunk{X: 0, Z: 0}
	style := villageStyleFor("minecraft:savanna")
	setVB(chunk, 8, 71, 8, block("tall_grass"))

	generator.placeVillageHouse(chunk, 8, 70, 8, 7, 5, style, 1)

	assertVillageBlock(t, chunk, 8, 70, 8, "minecraft:acacia_planks")
	lower := chunkBlock(chunk, 8, 71, 10)
	if lower.ResourceLocation() != "minecraft:acacia_door" ||
		lower.Properties["half"] != "lower" || lower.Properties["open"] != "false" {
		t.Fatalf("lower door = %s, want closed acacia lower door", lower.Key())
	}
	upper := chunkBlock(chunk, 8, 72, 10)
	if upper.ResourceLocation() != "minecraft:acacia_door" || upper.Properties["half"] != "upper" {
		t.Fatalf("upper door = %s, want acacia upper door", upper.Key())
	}
	assertVillageBlock(t, chunk, 4, 75, 8, "minecraft:acacia_stairs")
	assertVillageBlock(t, chunk, 8, 79, 8, "minecraft:acacia_planks")
	assertVillageBlock(t, chunk, 7, 71, 9, "minecraft:red_bed")
	assertVillageBlock(t, chunk, 7, 71, 8, "minecraft:red_bed")
	assertVillageBlock(t, chunk, 10, 71, 7, "minecraft:lectern")
	if got := chunkBlock(chunk, 8, 71, 8); !got.IsAir() {
		t.Fatalf("cleared interior block = %s, want air", got.Key())
	}
}

func TestVillageFarmUsesHydratedFarmlandAndComposter(t *testing.T) {
	generator := NewOverworldGenerator(1)
	chunk := &Chunk{X: 0, Z: 0}
	generator.placeVillageFarm(chunk, 8, 70, 8, villageStyleFor("minecraft:plains"))

	farmland := chunkBlock(chunk, 7, 70, 8)
	if farmland.ResourceLocation() != "minecraft:farmland" || farmland.Properties["moisture"] != "7" {
		t.Fatalf("farmland = %s, want moisture=7", farmland.Key())
	}
	wheat := chunkBlock(chunk, 7, 71, 8)
	if wheat.ResourceLocation() != "minecraft:wheat" || wheat.Properties["age"] != "7" {
		t.Fatalf("crop = %s, want mature wheat", wheat.Key())
	}
	assertVillageBlock(t, chunk, 10, 70, 10, "minecraft:composter")
	if got := chunkBlock(chunk, 10, 71, 10); !got.IsAir() {
		t.Fatalf("block above composter = %s, want air", got.Key())
	}
}

func assertVillageBlock(t *testing.T, chunk *Chunk, x, y, z int, want string) {
	t.Helper()
	if got := chunkBlock(chunk, x, y, z).ResourceLocation(); got != want {
		t.Fatalf("block (%d,%d,%d) = %s, want %s", x, y, z, got, want)
	}
}

func TestVillagerVariantAndProfessionAssignments(t *testing.T) {
	if got := villagerVariantForBiome("minecraft:savanna"); got != "minecraft:savanna" {
		t.Fatalf("savanna variant = %q", got)
	}
	if got := villagerVariantForBiome("minecraft:snowy_plains"); got != "minecraft:snow" {
		t.Fatalf("snowy plains variant = %q", got)
	}
	want := []string{
		"minecraft:farmer",
		"minecraft:librarian",
		"minecraft:fletcher",
		"minecraft:toolsmith",
		"minecraft:armorer",
	}
	for index, profession := range want {
		if got := string(villagerProfessionForIndex(index)); got != profession {
			t.Errorf("profession %d = %q, want %q", index, got, profession)
		}
	}
}

func TestVillageResidentsHaveUniqueBedsAndJobs(t *testing.T) {
	generator := NewOverworldGenerator(99)
	village := VillageCenter{WorldX: 0, WorldZ: 0, Biome: "minecraft:plains", Hash: 0x123456789abcdef}
	residents := generator.VillageResidents(village)
	if len(residents) < 3 {
		t.Fatalf("resident count = %d, want at least 3 houses", len(residents))
	}
	beds := make(map[[3]int32]struct{}, len(residents))
	for _, resident := range residents {
		bed := [3]int32{resident.Bed.X, resident.Bed.Y, resident.Bed.Z}
		if _, duplicate := beds[bed]; duplicate {
			t.Fatalf("duplicate bed assignment at %v", bed)
		}
		beds[bed] = struct{}{}
		if resident.Profession == "" || resident.Profession == "minecraft:none" {
			t.Fatalf("resident at %v has no assigned profession", resident.Home)
		}
		if resident.Workstation == (spatial.BlockPos{}) {
			t.Fatalf("resident at %v has no workstation", resident.Home)
		}
	}
}

func TestGeneratedVillageSpawnsResidentsAndIronGolem(t *testing.T) {
	generator := NewOverworldGenerator(24680)
	center, ok := generator.NearestVillage(0, 0, 12000)
	if !ok {
		t.Fatal("test seed has no village in search radius")
	}
	world := New(generator, nil, true)
	defer world.Close()
	world.Chunk(int32(floorDiv(center.WorldX, SectionSize)), int32(floorDiv(center.WorldZ, SectionSize)))

	villagers, golems := 0, 0
	for _, spawned := range world.Entities.Snapshot() {
		switch spawned.Type {
		case entity.TypeVillager:
			villagers++
			if !spawned.HasVillageHome || spawned.VillageBed == (spatial.BlockPos{}) || spawned.VillageWorkstation == (spatial.BlockPos{}) {
				t.Fatalf("unassigned villager: %+v", spawned)
			}
			if spawned.VillageCenter == (spatial.BlockPos{}) {
				t.Fatal("villager has no village-wide roaming center")
			}
		case entity.TypeIronGolem:
			golems++
			if !spawned.HasVillageHome || spawned.VillageCenter == (spatial.BlockPos{}) {
				t.Fatal("iron golem is not homed to its patrol village")
			}
		}
	}
	if villagers < 3 {
		t.Fatalf("villager count = %d, want at least 3", villagers)
	}
	if golems < 1 {
		t.Fatalf("iron golem count = %d, want at least 1", golems)
	}
}
