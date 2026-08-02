package handler

import (
	"testing"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
)

func TestBedRespawnFindsSafeSpaceBesideBed(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	w.SetBlock(0, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "red_bed", Properties: map[string]string{"part": "foot", "facing": "south"}})
	w.SetBlock(0, 64, 1, coreworld.Block{Namespace: "minecraft", Name: "red_bed", Properties: map[string]string{"part": "head", "facing": "south"}})
	p := player.New([16]byte{}, "sleeper", player.ClientEditionJava)
	p.HasSpawnPoint = true
	p.SpawnPoint = spatial.BlockPos{X: 0, Y: 64, Z: 0}

	position, ok := resolveBedRespawn(p, w)
	if !ok {
		t.Fatal("valid bed did not produce a respawn position")
	}
	if int(position.X-0.5) == 0 && int(position.Z-0.5) == 0 {
		t.Fatalf("respawn position remained inside bed: %+v", position)
	}
}

func TestMissingBedFallsBackToWorldSpawn(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	p := player.New([16]byte{}, "sleeper", player.ClientEditionJava)
	p.HasSpawnPoint = true
	p.SpawnPoint = spatial.BlockPos{X: 9, Y: 64, Z: 9}
	if _, ok := resolveBedRespawn(p, w); ok || p.HasSpawnPoint {
		t.Fatal("missing bed remained a valid personal spawn")
	}
}
