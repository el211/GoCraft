package handler

import (
	"strings"
	"testing"

	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

func TestCreativeInventoryUsesProtocol769ItemIDs(t *testing.T) {
	tests := []struct {
		itemID int32
		name   string
	}{
		{40, "minecraft:acacia_planks"},
		{195, "minecraft:glass"},
		{314, "minecraft:crafting_table"},
		{316, "minecraft:furnace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := player.New([16]byte{}, "builder", player.ClientEditionJava)
			pkt := protocol.NewBuilder(packetIDCreativeModeSetItem).
				Short(player.HotbarStart).
				VarInt(64).
				VarInt(tc.itemID).
				VarInt(0).
				VarInt(0).
				Build()
			if err := handleCreativeModeSetItem(pkt, p); err != nil {
				t.Fatalf("handleCreativeModeSetItem: %v", err)
			}
			got := p.Inventory[player.HotbarStart]
			if got.ItemID != tc.name || got.Count != 64 {
				t.Fatalf("hotbar item = %+v, want ItemID=%q Count=64", got, tc.name)
			}
		})
	}
}

func TestUseItemOnProtocol769LayoutPlacesExactBlock(t *testing.T) {
	p := player.New([16]byte{}, "builder", player.ClientEditionJava)
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:acacia_planks", Count: 64}
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	mgr := session.NewManager()

	pkt := protocol.NewBuilder(packetIDUseItemOn).
		VarInt(0).
		Long(packBlockPos(0, 63, 0)).
		VarInt(1).
		Float(0.5).
		Float(1.0).
		Float(0.5).
		Bool(false).
		Bool(false).
		VarInt(300).
		Build()
	if err := handleUseItemOn(pkt, p, w, mgr, nil, nil); err != nil {
		t.Fatalf("handleUseItemOn: %v", err)
	}
	got := w.GetBlock(0, 64, 0)
	if got.ResourceLocation() != "minecraft:acacia_planks" {
		t.Fatalf("placed block = %q, want minecraft:acacia_planks", got.ResourceLocation())
	}
}

func TestUseItemOnRequiresSequenceAfterWorldBorderHit(t *testing.T) {
	p := player.New([16]byte{}, "builder", player.ClientEditionJava)
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	mgr := session.NewManager()

	pkt := protocol.NewBuilder(packetIDUseItemOn).
		VarInt(0).
		Long(packBlockPos(0, 63, 0)).
		VarInt(1).
		Float(0.5).
		Float(1.0).
		Float(0.5).
		Bool(false).
		Bool(false).
		Build()
	err := handleUseItemOn(pkt, p, w, mgr, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("handleUseItemOn error = %v, want missing sequence error after world_border_hit", err)
	}
}

func TestBreakingOneGlassBlockDoesNotBreakAnother(t *testing.T) {
	p := player.New([16]byte{}, "builder", player.ClientEditionJava)
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	mgr := session.NewManager()
	glass := coreworld.Block{Namespace: "minecraft", Name: "glass"}
	w.SetBlock(1, 64, 0, glass)
	w.SetBlock(2, 64, 0, glass)

	pkt := protocol.NewBuilder(packetIDPlayerAction).
		VarInt(actionStatusStartDigging).
		Long(packBlockPos(1, 64, 0)).
		Byte(1).
		VarInt(301).
		Build()
	if err := handlePlayerAction(pkt, p, w, mgr); err != nil {
		t.Fatalf("handlePlayerAction: %v", err)
	}
	if got := w.GetBlock(1, 64, 0); !got.IsAir() {
		t.Fatalf("target block = %q, want air", got.ResourceLocation())
	}
	if got := w.GetBlock(2, 64, 0); !got.Equal(glass) {
		t.Fatalf("neighbor block = %q, want glass", got.ResourceLocation())
	}
}

func TestUseItemOnTogglesBothDoorHalves(t *testing.T) {
	p := player.New([16]byte{}, "builder", player.ClientEditionJava)
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	mgr := session.NewManager()
	lower := coreworld.Block{
		Namespace: "minecraft",
		Name:      "acacia_door",
		Properties: map[string]string{
			"facing": "south", "half": "lower", "hinge": "left",
			"open": "false", "powered": "false",
		},
	}
	upper := copyBlockProperties(lower)
	upper.Properties["half"] = "upper"
	w.SetBlock(0, 64, 0, lower)
	w.SetBlock(0, 65, 0, upper)

	pkt := protocol.NewBuilder(packetIDUseItemOn).
		VarInt(0).
		Long(packBlockPos(0, 64, 0)).
		VarInt(3).
		Float(0.5).
		Float(0.5).
		Float(0.5).
		Bool(false).
		Bool(false).
		VarInt(302).
		Build()
	if err := handleUseItemOn(pkt, p, w, mgr, nil, nil); err != nil {
		t.Fatalf("handleUseItemOn: %v", err)
	}
	for y := 64; y <= 65; y++ {
		if got := w.GetBlock(0, y, 0).Properties["open"]; got != "true" {
			t.Fatalf("door half at y=%d open=%q, want true", y, got)
		}
	}
}

func TestSurvivalBreaksOnlyOnFinishDigging(t *testing.T) {
	p := player.New([16]byte{}, "survivor", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()
	w.SetBlock(3, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "dirt"})

	start := protocol.NewBuilder(packetIDPlayerAction).
		VarInt(actionStatusStartDigging).
		Long(packBlockPos(3, 64, 0)).
		Byte(1).
		VarInt(303).
		Build()
	if err := handlePlayerAction(start, p, w, mgr); err != nil {
		t.Fatal(err)
	}
	if got := w.GetBlock(3, 64, 0); got.IsAir() {
		t.Fatal("survival START_DIGGING broke the block before mining finished")
	}

	finish := protocol.NewBuilder(packetIDPlayerAction).
		VarInt(actionStatusFinishDigging).
		Long(packBlockPos(3, 64, 0)).
		Byte(1).
		VarInt(304).
		Build()
	if err := handlePlayerAction(finish, p, w, mgr); err != nil {
		t.Fatal(err)
	}
	if got := w.GetBlock(3, 64, 0); !got.IsAir() {
		t.Fatalf("survival target = %q, want air after FINISH_DIGGING", got.ResourceLocation())
	}
	if got := p.Inventory[player.HotbarStart]; got.ItemID != "minecraft:dirt" || got.Count != 1 {
		t.Fatalf("survival drop = %+v, want one dirt", got)
	}
}

func TestBreakingLowerDoublePlantRemovesUpperHalfAndDropsFlower(t *testing.T) {
	p := player.New([16]byte{}, "gardener", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()
	lower := coreworld.Block{Namespace: "minecraft", Name: "peony", Properties: map[string]string{"half": "lower"}}
	upper := coreworld.Block{Namespace: "minecraft", Name: "peony", Properties: map[string]string{"half": "upper"}}
	w.SetBlock(5, 64, 0, lower)
	w.SetBlock(5, 65, 0, upper)

	start := protocol.NewBuilder(packetIDPlayerAction).
		VarInt(actionStatusStartDigging).
		Long(packBlockPos(5, 64, 0)).
		Byte(1).
		VarInt(305).
		Build()
	if err := handlePlayerAction(start, p, w, mgr); err != nil {
		t.Fatal(err)
	}
	for y := 64; y <= 65; y++ {
		if got := w.GetBlock(5, y, 0); !got.IsAir() {
			t.Fatalf("plant half y=%d = %q, want air", y, got.ResourceLocation())
		}
	}
	if got := p.Inventory[player.HotbarStart]; got.ItemID != "minecraft:peony" || got.Count != 1 {
		t.Fatalf("flower drop = %+v, want one peony", got)
	}
}

func TestBreakingUpperDoublePlantRemovesLowerHalf(t *testing.T) {
	p := player.New([16]byte{}, "gardener", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()
	lower := coreworld.Block{Namespace: "minecraft", Name: "lilac", Properties: map[string]string{"half": "lower"}}
	upper := coreworld.Block{Namespace: "minecraft", Name: "lilac", Properties: map[string]string{"half": "upper"}}
	w.SetBlock(5, 64, 0, lower)
	w.SetBlock(5, 65, 0, upper)

	start := protocol.NewBuilder(packetIDPlayerAction).
		VarInt(actionStatusStartDigging).
		Long(packBlockPos(5, 65, 0)).
		Byte(1).
		VarInt(306).
		Build()
	if err := handlePlayerAction(start, p, w, mgr); err != nil {
		t.Fatal(err)
	}
	for y := 64; y <= 65; y++ {
		if got := w.GetBlock(5, y, 0); !got.IsAir() {
			t.Fatalf("plant half y=%d = %q, want air", y, got.ResourceLocation())
		}
	}
}

func TestSurvivalGrassBreaksOnStartDigging(t *testing.T) {
	p := player.New([16]byte{}, "gardener", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()
	w.SetBlock(6, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "short_grass"})

	start := protocol.NewBuilder(packetIDPlayerAction).
		VarInt(actionStatusStartDigging).
		Long(packBlockPos(6, 64, 0)).
		Byte(1).
		VarInt(306).
		Build()
	if err := handlePlayerAction(start, p, w, mgr); err != nil {
		t.Fatal(err)
	}
	if got := w.GetBlock(6, 64, 0); !got.IsAir() {
		t.Fatalf("grass = %q, want air after START_DIGGING", got.ResourceLocation())
	}
	if got := p.Inventory[player.HotbarStart]; got.ItemID != "minecraft:wheat_seeds" || got.Count != 1 {
		t.Fatalf("grass drop = %+v, want one wheat seed", got)
	}
}

func TestHoeTillsAndSeedsPlant(t *testing.T) {
	p := player.New([16]byte{}, "farmer", player.ClientEditionJava)
	p.GameMode = player.GameModeSurvival
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:iron_hoe", Count: 1}
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	mgr := session.NewManager()
	w.SetBlock(8, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "dirt"})

	till := protocol.NewBuilder(packetIDUseItemOn).
		VarInt(0).Long(packBlockPos(8, 64, 0)).VarInt(1).
		Float(0.5).Float(1).Float(0.5).Bool(false).Bool(false).VarInt(400).Build()
	if err := handleUseItemOn(till, p, w, mgr, nil, nil); err != nil {
		t.Fatal(err)
	}
	farmland := w.GetBlock(8, 64, 0)
	if farmland.ResourceLocation() != "minecraft:farmland" || farmland.Properties["moisture"] != "0" {
		t.Fatalf("tilled block = %s, want dry farmland", farmland.Key())
	}

	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:wheat_seeds", Count: 2}
	plant := protocol.NewBuilder(packetIDUseItemOn).
		VarInt(0).Long(packBlockPos(8, 64, 0)).VarInt(1).
		Float(0.5).Float(1).Float(0.5).Bool(false).Bool(false).VarInt(401).Build()
	if err := handleUseItemOn(plant, p, w, mgr, nil, nil); err != nil {
		t.Fatal(err)
	}
	crop := w.GetBlock(8, 65, 0)
	if crop.ResourceLocation() != "minecraft:wheat" || crop.Properties["age"] != "0" {
		t.Fatalf("planted crop = %s, want age-0 wheat", crop.Key())
	}
	if got := p.Inventory[player.HotbarStart].Count; got != 1 {
		t.Fatalf("seed count = %d, want 1", got)
	}
}
