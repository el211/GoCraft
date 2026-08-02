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
	if err := handleUseItemOn(pkt, p, w, mgr, nil, nil); err != nil {
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

func TestDoubleChestPairsOnPlacement(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()

	// Place the first chest at (5,64,5); player faces south (yaw=0 → chest facing=north).
	p := player.New([16]byte{1}, "builder", player.ClientEditionJava)
	p.GameMode = player.GameModeCreative
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:chest", Count: 64}
	w.SetBlock(5, 64, 5, coreworld.Air)
	placeChestBlock(p, 5, 64, 5, "minecraft:chest", w, mgr)
	w.SetContainerItems(5, 64, 5, "minecraft:chest", nil)

	first := w.GetBlock(5, 64, 5)
	if first.ResourceLocation() != "minecraft:chest" {
		t.Fatalf("first chest not placed: %q", first.ResourceLocation())
	}
	if got := first.Properties["type"]; got != "single" {
		t.Fatalf("first chest type = %q, want single", got)
	}

	// Place second chest to the east (x+1); facing=north → east is the right side.
	// New chest should be type=right, existing should become type=left.
	placeChestBlock(p, 6, 64, 5, "minecraft:chest", w, mgr)
	w.SetContainerItems(6, 64, 5, "minecraft:chest", nil)

	second := w.GetBlock(6, 64, 5)
	updated := w.GetBlock(5, 64, 5)
	if second.Properties["type"] != "right" {
		t.Fatalf("new (east) chest type = %q, want right", second.Properties["type"])
	}
	if updated.Properties["type"] != "left" {
		t.Fatalf("existing (west) chest type = %q, want left", updated.Properties["type"])
	}
}

func TestDoubleChestOpenGives54Slots(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()

	// Set up a pre-linked double chest pair facing north.
	// right half (type=right) at (5,64,5) — east.
	// left half  (type=left)  at (4,64,5) — west.
	props := map[string]string{"facing": "north", "waterlogged": "false"}
	rProps := make(map[string]string)
	for k, v := range props {
		rProps[k] = v
	}
	rProps["type"] = "right"
	lProps := make(map[string]string)
	for k, v := range props {
		lProps[k] = v
	}
	lProps["type"] = "left"
	w.SetBlock(5, 64, 5, coreworld.Block{Namespace: "minecraft", Name: "chest", Properties: rProps})
	w.SetBlock(4, 64, 5, coreworld.Block{Namespace: "minecraft", Name: "chest", Properties: lProps})
	w.SetContainerItems(5, 64, 5, "minecraft:chest", []coreworld.ContainerItem{{Slot: 0, ItemID: "minecraft:diamond", Count: 5}})
	w.SetContainerItems(4, 64, 5, "minecraft:chest", []coreworld.ContainerItem{{Slot: 1, ItemID: "minecraft:gold_ingot", Count: 3}})

	p := player.New([16]byte{2}, "opener", player.ClientEditionJava)
	block := w.GetBlock(5, 64, 5)
	rPos, lPos, hasPartner := chestDoubleHalves(block, spatial.BlockPos{X: 5, Y: 64, Z: 5}, w)
	if !hasPartner {
		t.Fatal("chestDoubleHalves: expected hasPartner=true")
	}
	if rPos.X != 5 || lPos.X != 4 {
		t.Fatalf("halves: right=%v left=%v", rPos, lPos)
	}

	// Load items as openChest would.
	p.ContainerSlots = make([]player.ItemStack, doubleChestSlotCount)
	for _, item := range w.ContainerItems(int(rPos.X), int(rPos.Y), int(rPos.Z)) {
		if item.Slot >= 0 && item.Slot < chestSlotCount {
			p.ContainerSlots[item.Slot] = player.ItemStack{ItemID: item.ItemID, Count: item.Count}
		}
	}
	for _, item := range w.ContainerItems(int(lPos.X), int(lPos.Y), int(lPos.Z)) {
		if item.Slot >= 0 && item.Slot < chestSlotCount {
			p.ContainerSlots[item.Slot+chestSlotCount] = player.ItemStack{ItemID: item.ItemID, Count: item.Count}
		}
	}
	if p.ContainerSlots[0].ItemID != "minecraft:diamond" {
		t.Fatalf("right-half slot 0 = %+v, want diamond", p.ContainerSlots[0])
	}
	if p.ContainerSlots[chestSlotCount+1].ItemID != "minecraft:gold_ingot" {
		t.Fatalf("left-half slot 28 = %+v, want gold_ingot", p.ContainerSlots[chestSlotCount+1])
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
