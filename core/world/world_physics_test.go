package world

import "testing"

func TestBreakingPlantDoesNotScheduleLeafDecay(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()

	leaves := Block{Namespace: "minecraft", Name: "oak_leaves"}
	plant := Block{Namespace: "minecraft", Name: "short_grass"}
	w.SetBlock(1, 65, 0, leaves)
	w.SetBlock(0, 65, 0, plant)
	w.BlockPhysics.DrainDue(1 << 30)

	w.SetBlock(0, 65, 0, Air)
	for _, update := range w.BlockPhysics.DrainDue(1 << 30) {
		if update.Kind == UpdateLeafDecay {
			t.Fatalf("breaking a plant scheduled leaf decay at (%d,%d,%d)", update.X, update.Y, update.Z)
		}
	}
}

func TestRemovingLogSchedulesNearbyLeafDecay(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()

	log := Block{Namespace: "minecraft", Name: "oak_log"}
	leaves := Block{Namespace: "minecraft", Name: "oak_leaves"}
	w.SetBlock(0, 65, 0, log)
	w.SetBlock(1, 65, 0, leaves)
	w.BlockPhysics.DrainDue(1 << 30)

	w.SetBlock(0, 65, 0, Air)
	for _, update := range w.BlockPhysics.DrainDue(1 << 30) {
		if update.Kind == UpdateLeafDecay && update.X == 1 && update.Y == 65 && update.Z == 0 {
			return
		}
	}
	t.Fatal("removing a log did not schedule decay for nearby leaves")
}
