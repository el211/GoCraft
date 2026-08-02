package handler

import (
	"testing"

	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
)

func TestBoatItemRaycastsWaterAndUsesOneEntityID(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	w.SetBlock(0, 64, 3, coreworld.Block{Namespace: "minecraft", Name: "water"})
	p := player.New([16]byte{}, "sailor", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	p.Position.X, p.Position.Y, p.Position.Z = 0.5, 64, 0.5
	p.Rotation.Yaw, p.Rotation.Pitch = 0, 20
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:oak_boat", Count: 2}
	allocated := int32(100)
	calls := 0
	boat := spawnBoatFromLook(p, w, func() int32 {
		calls++
		allocated++
		return allocated
	})
	if boat == nil || boat.Type != "minecraft:oak_boat" {
		t.Fatalf("spawned boat = %+v", boat)
	}
	if calls != 1 || boat.EntityID != 101 || boat.Position.Y != 65 {
		t.Fatalf("allocator calls=%d id=%d y=%v", calls, boat.EntityID, boat.Position.Y)
	}
	consumePlacedBoat(p, nil)
	if p.Inventory[player.HotbarStart].Count != 1 {
		t.Fatalf("remaining boats = %d, want 1", p.Inventory[player.HotbarStart].Count)
	}
}
