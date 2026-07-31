package world

import (
	"testing"

	"GoCraft/core/entity"
)

func TestLoadedVillagePOIsSpawnResidentsAndGuard(t *testing.T) {
	world := New(&FlatGenerator{}, nil, true)
	defer world.Close()
	chunk := &Chunk{X: 0, Z: 0}
	sectionIndex := (64 - WorldMinY) / SectionSize
	section := NewSection()
	for index, x := range []int{1, 6, 11} {
		section.Set(x, 0, 1, Block{Namespace: "minecraft", Name: "red_bed", Properties: map[string]string{"part": "head", "facing": "north", "occupied": "false"}})
		section.Set(x, 0, 4, Block{Namespace: "minecraft", Name: "oak_door", Properties: map[string]string{"half": "lower", "facing": "south", "open": "false"}})
		job := []string{"composter", "lectern", "smithing_table"}[index]
		section.Set(x, 0, 6, Block{Namespace: "minecraft", Name: job})
	}
	chunk.Sections[sectionIndex] = section

	world.discoverLoadedVillagePopulation(chunk)
	villagers, golems := 0, 0
	for _, value := range world.Entities.Snapshot() {
		switch value.Type {
		case entity.TypeVillager:
			villagers++
			if !value.HasVillageHome || value.VillageBed == (struct{ X, Y, Z int32 }{}) {
				t.Fatalf("discovered villager is not assigned: %+v", value)
			}
		case entity.TypeIronGolem:
			golems++
		}
	}
	if villagers != 3 || golems != 1 {
		t.Fatalf("population villagers=%d golems=%d, want 3 and 1", villagers, golems)
	}

	world.discoverLoadedVillagePopulation(chunk)
	if got := len(world.Entities.Snapshot()); got != 4 {
		t.Fatalf("re-discovery duplicated entities: count=%d, want 4", got)
	}
}

func TestLegacyDoorClusterSpawnsResidentsWithoutBeds(t *testing.T) {
	world := New(&FlatGenerator{}, nil, true)
	defer world.Close()
	chunk := &Chunk{X: 2, Z: 2}
	sectionIndex := (70 - WorldMinY) / SectionSize
	section := NewSection()
	for _, x := range []int{1, 6, 11} {
		section.Set(x, (70-WorldMinY)%SectionSize, 4, Block{Namespace: "minecraft", Name: "acacia_door", Properties: map[string]string{"half": "lower", "facing": "south", "open": "false"}})
	}
	chunk.Sections[sectionIndex] = section

	world.discoverLoadedVillagePopulation(chunk)
	villagers := 0
	for _, value := range world.Entities.Snapshot() {
		if value.Type == entity.TypeVillager {
			villagers++
		}
	}
	if villagers != 3 {
		t.Fatalf("legacy village residents=%d, want one per house door (3)", villagers)
	}
}
