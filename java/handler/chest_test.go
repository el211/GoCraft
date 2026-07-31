package handler

import (
	"testing"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

func TestSurvivalPlacesChestIntoReplaceableGrass(t *testing.T) {
	p := player.New([16]byte{}, "builder", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:chest", Count: 2}
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()
	w.SetBlock(4, 64, 4, coreworld.Block{Namespace: "minecraft", Name: "short_grass"})

	pkt := protocol.NewBuilder(packetIDUseItemOn).
		VarInt(0).
		Long(packBlockPos(4, 64, 4)).
		VarInt(1).
		Float(0.5).
		Float(0.5).
		Float(0.5).
		Bool(false).
		Bool(false).
		VarInt(500).
		Build()
	if err := handleUseItemOn(pkt, p, w, mgr, nil); err != nil {
		t.Fatalf("handleUseItemOn: %v", err)
	}
	if got := w.GetBlock(4, 64, 4).ResourceLocation(); got != "minecraft:chest" {
		t.Fatalf("placed block = %q, want minecraft:chest", got)
	}
	if got := p.Inventory[player.HotbarStart].Count; got != 1 {
		t.Fatalf("survival chest count = %d, want 1", got)
	}
	chunk := w.Chunk(0, 0)
	found := false
	for _, blockEntity := range chunk.BlockEntities {
		if blockEntity.X == 4 && blockEntity.Y == 64 && blockEntity.Z == 4 && blockEntity.Type == "minecraft:chest" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("placed chest did not create a canonical block entity")
	}
}

func TestChestSlotsMoveItemsAndPersistToWorld(t *testing.T) {
	p := player.New([16]byte{}, "storage", player.ClientEditionJava)
	p.OpenContainerID = chestContainerID
	p.OpenContainerKind = "minecraft:chest"
	p.OpenContainerPos = spatial.BlockPos{X: 2, Y: 64, Z: 3}
	p.ContainerSlots = make([]player.ItemStack, chestSlotCount)
	p.CarriedItem = player.ItemStack{ItemID: "minecraft:diamond", Count: 12}
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(2, 64, 3, coreworld.Block{Namespace: "minecraft", Name: "chest"})

	handleChestClick(p, w, 0, 0, 0)
	if !p.CarriedItem.IsEmpty() {
		t.Fatalf("cursor = %+v, want empty after placing stack", p.CarriedItem)
	}
	if got := p.ContainerSlots[0]; got.ItemID != "minecraft:diamond" || got.Count != 12 {
		t.Fatalf("chest slot 0 = %+v, want 12 diamonds", got)
	}
	items := w.ContainerItems(2, 64, 3)
	if len(items) != 1 || items[0].Slot != 0 || items[0].ItemID != "minecraft:diamond" || items[0].Count != 12 {
		t.Fatalf("canonical chest items = %+v", items)
	}

	p.Inventory[9] = player.ItemStack{ItemID: "minecraft:stone", Count: 64}
	handleChestClick(p, w, chestSlotCount, 0, 1)
	if !p.Inventory[9].IsEmpty() {
		t.Fatalf("shift-click source = %+v, want empty", p.Inventory[9])
	}
	if got := p.ContainerSlots[1]; got.ItemID != "minecraft:stone" || got.Count != 64 {
		t.Fatalf("shift-click destination = %+v, want 64 stone", got)
	}
}
