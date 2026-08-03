package server

import (
	"math/rand"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/game"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
)

func TestVillagersDoNotBreedWithoutARealFreeBed(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	center := spatial.BlockPos{X: 0, Y: 64, Z: 0}
	for id := int32(1); id <= 2; id++ {
		villager := corentity.New(id, [16]byte{byte(id)}, corentity.TypeVillager, float64(id), 64, 0)
		villager.HasVillageHome = true
		villager.VillageCenter = center
		villager.VillageBed = spatial.BlockPos{X: id * 2, Y: 64, Z: 0}
		w.Entities.Add(villager)
	}
	s := &Server{world: w, game: game.New(), spawnRNG: rand.New(rand.NewSource(1))}
	s.tickVillagerBreeding()
	if got := len(w.Entities.Snapshot()); got != 2 {
		t.Fatalf("villager count = %d, want hard bed cap of 2", got)
	}
}
