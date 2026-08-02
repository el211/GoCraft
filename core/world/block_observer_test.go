package world

import "testing"

func TestBlockObserverSeesNormalAndInternalMutations(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	changes := make([]BlockChange, 0, 2)
	w.SetBlockObserver(func(change BlockChange) { changes = append(changes, change) })

	stone := Block{Namespace: "minecraft", Name: "stone"}
	w.SetBlock(1, 65, 2, stone)
	w.setBlockNoPhysics(1, 65, 2, Air)

	if len(changes) != 2 {
		t.Fatalf("observer received %d changes, want 2", len(changes))
	}
	if changes[0].Block.ResourceLocation() != "minecraft:stone" || !changes[1].Block.IsAir() {
		t.Fatalf("unexpected observer changes: %#v", changes)
	}
}
