package server

import (
	"math/rand"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/game"
	coreworld "GoCraft/core/world"
	"GoCraft/java/session"
)

func TestWetEndermanTeleportsToDryLoadedGround(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	for cx := int32(-2); cx <= 2; cx++ {
		for cz := int32(-2); cz <= 2; cz++ {
			w.Chunk(cx, cz)
		}
	}
	w.SetBlock(0, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "water", Properties: map[string]string{"level": "0"}})
	enderman := corentity.New(1, [16]byte{}, corentity.TypeEnderman, 0.5, 64, 0.5)
	s := &Server{world: w, sessions: session.NewManager(), simulationDimension: dimensionOverworld}
	for s.worldAge = 0; ; s.worldAge++ {
		roll := uint64(enderman.EntityID)*0x9e3779b97f4a7c15 ^ uint64(s.worldAge)*0xbf58476d1ce4e5b9
		if roll%10 != 0 {
			break
		}
	}
	hurt := []*corentity.Entity{}
	s.tickEndermanWater(enderman, &hurt)
	if len(hurt) != 1 || enderman.Health != enderman.MaxHealth-1 {
		t.Fatalf("water damage: hurt=%d health=%.1f", len(hurt), enderman.Health)
	}
	if enderman.Position.X == 0.5 && enderman.Position.Z == 0.5 {
		t.Fatal("wet enderman did not teleport")
	}
	if w.TouchesWater(enderman.Position.X, enderman.Position.Y, enderman.Position.Z) {
		t.Fatalf("enderman teleported into water at %+v", enderman.Position)
	}
}

// tickEndermanUntil advances the deterministic carry inputs (worldAge for the
// probability roll, AgeTicks for the sampled position) until done reports true.
func tickEndermanUntil(s *Server, e *corentity.Entity, done func() bool, limit int) bool {
	for tick := 0; tick < limit; tick++ {
		if done() {
			return true
		}
		s.worldAge++
		e.AgeTicks++
		s.tickEndermanBlockCarry(e)
	}
	return done()
}

// countEndermanTestGrass counts grass blocks in the sampling box used below.
func countEndermanTestGrass(w *coreworld.World) int {
	total := 0
	for x := -2; x <= 2; x++ {
		for y := 62; y <= 66; y++ {
			for z := -2; z <= 2; z++ {
				if w.GetBlock(x, y, z).ResourceLocation() == "minecraft:grass_block" {
					total++
				}
			}
		}
	}
	return total
}

func TestEndermanPicksUpAndReplacesHoldableBlock(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	for cx := int32(-1); cx <= 1; cx++ {
		for cz := int32(-1); cz <= 1; cz++ {
			w.Chunk(cx, cz)
		}
	}
	// FlatGenerator lays a single stone layer at y=63; swap the reachable part
	// of it for a holdable block so every sampled ground position qualifies.
	for x := -2; x <= 2; x++ {
		for z := -2; z <= 2; z++ {
			w.SetBlock(x, 63, z, coreworld.Block{Namespace: "minecraft", Name: "grass_block"})
		}
	}
	enderman := corentity.New(1, [16]byte{}, corentity.TypeEnderman, 0.5, 64, 0.5)
	s := &Server{world: w, sessions: session.NewManager(), simulationDimension: dimensionOverworld}

	if !tickEndermanUntil(s, enderman, func() bool { return enderman.EndermanCarriedBlock != "" }, 100000) {
		t.Fatal("enderman never picked up a block")
	}
	if enderman.EndermanCarriedBlock != "minecraft:grass_block" {
		t.Fatalf("carried block = %q, want minecraft:grass_block", enderman.EndermanCarriedBlock)
	}
	if got := countEndermanTestGrass(w); got != 24 {
		t.Fatalf("grass blocks after pickup = %d, want 24", got)
	}

	if !tickEndermanUntil(s, enderman, func() bool { return enderman.EndermanCarriedBlock == "" }, 400000) {
		t.Fatal("enderman never placed its carried block")
	}
	if got := countEndermanTestGrass(w); got != 25 {
		t.Fatalf("grass blocks after placement = %d, want 25", got)
	}
}

func TestEndermanIgnoresBlocksOutsideHoldableSet(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	for cx := int32(-1); cx <= 1; cx++ {
		for cz := int32(-1); cz <= 1; cz++ {
			w.Chunk(cx, cz)
		}
	}
	// The generator's stone floor is not in EndermanPickupBlocks.
	enderman := corentity.New(1, [16]byte{}, corentity.TypeEnderman, 0.5, 64, 0.5)
	s := &Server{world: w, sessions: session.NewManager(), simulationDimension: dimensionOverworld}

	tickEndermanUntil(s, enderman, func() bool { return false }, 20000)
	if enderman.EndermanCarriedBlock != "" {
		t.Fatalf("enderman picked up %q, want nothing", enderman.EndermanCarriedBlock)
	}
}

func TestKilledEndermanDropsCarriedBlock(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	s := &Server{
		world:               w,
		game:                game.New(),
		sessions:            session.NewManager(),
		simulationDimension: dimensionOverworld,
		spawnRNG:            rand.New(rand.NewSource(1)),
	}
	enderman := corentity.New(1, [16]byte{}, corentity.TypeEnderman, 0.5, 64, 0.5)
	enderman.EndermanCarriedBlock = "minecraft:grass_block"

	var carried *corentity.Entity
	for _, dropped := range s.spawnMobDrops(enderman) {
		if dropped.ItemID == "minecraft:grass_block" {
			carried = dropped
		}
	}
	if carried == nil {
		t.Fatal("killed enderman did not drop its carried block")
	}
	if carried.ItemCount != 1 {
		t.Fatalf("dropped stack count = %d, want 1", carried.ItemCount)
	}
	if enderman.EndermanCarriedBlock != "" {
		t.Fatalf("carried block still %q after death", enderman.EndermanCarriedBlock)
	}
}
