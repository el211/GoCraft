package world

import "testing"

func TestLeverWirePowersAndUnpowersLamp(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "lever", Properties: map[string]string{"powered": "true"}})
	w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(2, 64, 0, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})

	w.Redstone.FlushUpdates()
	if got := w.GetBlock(1, 64, 0).Properties["power"]; got != "15" {
		t.Fatalf("wire power = %q, want 15", got)
	}
	if got := w.GetBlock(2, 64, 0).Properties["lit"]; got != "true" {
		t.Fatalf("lamp lit = %q, want true", got)
	}

	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "lever", Properties: map[string]string{"powered": "false"}})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(1, 64, 0).Properties["power"]; got != "0" {
		t.Fatalf("wire power after lever off = %q, want 0", got)
	}
	if got := w.GetBlock(2, 64, 0).Properties["lit"]; got != "false" {
		t.Fatalf("lamp lit after lever off = %q, want false", got)
	}
}

func TestEveryButtonAndPressurePlateIsRecognisedAsSource(t *testing.T) {
	for _, name := range []string{
		"minecraft:polished_blackstone_button", "minecraft:pale_oak_button",
		"minecraft:birch_pressure_plate", "minecraft:heavy_weighted_pressure_plate",
	} {
		if !IsRedstoneSource(name) {
			t.Errorf("%s was not recognised as a redstone source", name)
		}
	}
}

func TestAnalogAndTriggeredSourcesEmitTheirState(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]string
		want  int
	}{
		{"daylight_detector", map[string]string{"power": "9"}, 9},
		{"target", map[string]string{"power": "12"}, 12},
		{"detector_rail", map[string]string{"powered": "true"}, 15},
		{"tripwire_hook", map[string]string{"attached": "true", "powered": "true"}, 15},
	}
	for _, test := range tests {
		w := New(&FlatGenerator{}, nil, false)
		w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: test.name, Properties: test.props})
		w.Redstone.FlushUpdates()
		if got := w.Redstone.PowerAt(0, 64, 0); got != test.want {
			t.Errorf("%s power = %d, want %d", test.name, got, test.want)
		}
		w.Close()
	}
}

func TestRedstoneTorchInvertsPowerConductedThroughSupport(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "lever", Properties: map[string]string{"powered": "true"}})
	w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(2, 64, 0, Block{Namespace: "minecraft", Name: "stone"})
	w.SetBlock(2, 65, 0, Block{Namespace: "minecraft", Name: "redstone_torch", Properties: map[string]string{"lit": "true"}})
	w.SetBlock(3, 65, 0, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})

	w.Redstone.FlushUpdates()
	if got := w.GetBlock(2, 65, 0).Properties["lit"]; got != "false" {
		t.Fatalf("powered support left torch lit = %q", got)
	}
	if got := w.GetBlock(3, 65, 0).Properties["lit"]; got != "false" {
		t.Fatalf("inverted torch powered lamp = %q (wire=%d support=%d torch=%d lamp=%d)", got,
			w.Redstone.PowerAt(1, 64, 0), w.Redstone.PowerAt(2, 64, 0),
			w.Redstone.PowerAt(2, 65, 0), w.Redstone.PowerAt(3, 65, 0))
	}

	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "lever", Properties: map[string]string{"powered": "false"}})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(2, 65, 0).Properties["lit"]; got != "true" {
		t.Fatalf("unpowered support left torch unlit = %q", got)
	}
	if got := w.GetBlock(3, 65, 0).Properties["lit"]; got != "true" {
		t.Fatalf("torch did not activate lamp = %q", got)
	}
}

func TestRepeaterReadsRearAndPowersOnlyItsFront(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, -1, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "repeater", Properties: map[string]string{
		"facing": "north", "delay": "1", "locked": "false", "powered": "false",
	}})
	w.SetBlock(0, 64, 1, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})
	w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})

	w.Redstone.FlushUpdates()
	if got := w.GetBlock(0, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("repeater powered = %q, want true", got)
	}
	if got := w.GetBlock(0, 64, 1).Properties["lit"]; got != "true" {
		t.Fatalf("front lamp lit = %q, want true", got)
	}
	if got := w.GetBlock(1, 64, 0).Properties["lit"]; got != "false" {
		t.Fatalf("side lamp lit = %q, want false", got)
	}
}

func TestComparatorSubtractsSideSignal(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, -1, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.SetBlock(3, 64, 0, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.SetBlock(2, 64, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "comparator", Properties: map[string]string{
		"facing": "north", "mode": "subtract", "powered": "false",
	}})

	w.Redstone.FlushUpdates()
	if got := w.Redstone.PowerAt(0, 64, 0); got != 1 {
		t.Fatalf("comparator output = %d, want 1", got)
	}
	if got := w.GetBlock(0, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("comparator powered = %q, want true", got)
	}
}

func TestRedstoneUpdatesMechanismStates(t *testing.T) {
	tests := []struct {
		name, property, want string
		properties           map[string]string
	}{
		{"powered_rail", "powered", "true", map[string]string{"shape": "east_west", "powered": "false"}},
		{"activator_rail", "powered", "true", map[string]string{"shape": "east_west", "powered": "false"}},
		{"hopper", "enabled", "false", map[string]string{"facing": "down", "enabled": "true"}},
		{"oak_fence_gate", "open", "true", map[string]string{"facing": "north", "open": "false", "powered": "false"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := New(&FlatGenerator{}, nil, false)
			defer w.Close()
			w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "redstone_block"})
			w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: test.name, Properties: test.properties})
			w.Redstone.FlushUpdates()
			if got := w.GetBlock(1, 64, 0).Properties[test.property]; got != test.want {
				t.Fatalf("%s = %q, want %q", test.property, got, test.want)
			}
		})
	}
}

func TestObserverPowersOnlyAwayFromObservedFace(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "observer", Properties: map[string]string{"facing": "east", "powered": "true"}})
	w.SetBlock(-1, 64, 0, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})
	w.SetBlock(0, 64, 1, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(-1, 64, 0).Properties["lit"]; got != "true" {
		t.Fatalf("lamp behind observer lit = %q, want true", got)
	}
	if got := w.GetBlock(0, 64, 1).Properties["lit"]; got != "false" {
		t.Fatalf("lamp beside observer lit = %q, want false", got)
	}
}

func TestRedstonePowerTravelsUpAndDownSteps(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "lever", Properties: map[string]string{"powered": "true"}})
	w.SetBlock(1, 64, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(2, 64, 0, Block{Namespace: "minecraft", Name: "stone"})
	w.SetBlock(2, 65, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(3, 64, 0, Block{Namespace: "minecraft", Name: "stone"})
	w.SetBlock(3, 65, 0, Block{Namespace: "minecraft", Name: "redstone_wire", Properties: map[string]string{"power": "0"}})
	w.SetBlock(4, 65, 0, Block{Namespace: "minecraft", Name: "redstone_lamp", Properties: map[string]string{"lit": "false"}})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(2, 65, 0).Properties["power"]; got != "14" {
		t.Fatalf("upper step power = %q, want 14", got)
	}
	if got := w.GetBlock(4, 65, 0).Properties["lit"]; got != "true" {
		t.Fatalf("lamp after step lit = %q, want true", got)
	}
}

func TestRepeaterSideSignalLocksCurrentState(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	repeater := func(facing string) Block {
		return Block{Namespace: "minecraft", Name: "repeater", Properties: map[string]string{
			"facing": facing, "delay": "1", "locked": "false", "powered": "false",
		}}
	}
	w.SetBlock(0, 64, 0, repeater("north"))
	w.SetBlock(0, 64, -1, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.Redstone.FlushUpdates()
	w.SetBlock(1, 64, 0, repeater("east"))
	w.SetBlock(2, 64, 0, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(0, 64, 0).Properties["locked"]; got != "true" {
		t.Fatalf("locked = %q, want true", got)
	}
	w.SetBlock(0, 64, -1, Air)
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(0, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("locked repeater powered = %q, want retained true", got)
	}
}

func TestPoweredRailCarriesPowerForEightRails(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	for x := 0; x < 9; x++ {
		w.SetBlock(x, 64, 0, Block{Namespace: "minecraft", Name: "powered_rail", Properties: map[string]string{
			"shape": "east_west", "powered": "false",
		}})
	}
	w.SetBlock(0, 65, 0, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(7, 64, 0).Properties["powered"]; got != "true" {
		t.Fatalf("eighth rail powered = %q, want true", got)
	}
	if got := w.GetBlock(8, 64, 0).Properties["powered"]; got != "false" {
		t.Fatalf("ninth rail powered = %q, want false", got)
	}
}

func TestComparatorReadsContainerFullness(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	w.SetBlock(0, 64, -1, Block{Namespace: "minecraft", Name: "chest"})
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "comparator", Properties: map[string]string{
		"facing": "north", "mode": "compare", "powered": "false",
	}})
	w.Redstone.FlushUpdates()
	if got := w.Redstone.PowerAt(0, 64, 0); got != 0 {
		t.Fatalf("empty chest output = %d, want 0", got)
	}
	w.SetContainerItems(0, 64, -1, "minecraft:chest", []ContainerItem{{Slot: 0, ItemID: "minecraft:stone", Count: 64}})
	w.Redstone.FlushUpdates()
	if got := w.Redstone.PowerAt(0, 64, 0); got != 1 {
		t.Fatalf("one full chest slot output = %d, want 1", got)
	}
}

func TestCopperBulbTogglesOnlyOnRisingEdge(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	bulb := Block{Namespace: "minecraft", Name: "waxed_oxidized_copper_bulb", Properties: map[string]string{
		"lit": "false", "powered": "false",
	}}
	w.SetBlock(1, 64, 0, bulb)
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "redstone_block"})
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(1, 64, 0); got.Properties["lit"] != "true" || got.Properties["powered"] != "true" {
		t.Fatalf("powered bulb = %s", got.Key())
	}
	w.SetBlock(0, 64, 0, Air)
	w.Redstone.FlushUpdates()
	if got := w.GetBlock(1, 64, 0); got.Properties["lit"] != "true" || got.Properties["powered"] != "false" {
		t.Fatalf("unpowered bulb = %s", got.Key())
	}
}

func TestCauldronComparatorSignal(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]string
		want  int
	}{
		{"empty cauldron", nil, 0},
		{"water level 1", map[string]string{"level": "1"}, 1},
		{"water level 2", map[string]string{"level": "2"}, 2},
		{"water level 3", map[string]string{"level": "3"}, 3},
		{"lava cauldron", nil, 3},
		{"powder snow level 1", map[string]string{"level": "1"}, 1},
	}
	blockNames := []string{
		"cauldron", "water_cauldron", "water_cauldron", "water_cauldron",
		"lava_cauldron", "powder_snow_cauldron",
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := New(&FlatGenerator{}, nil, false)
			defer w.Close()
			w.SetBlock(0, 64, -1, Block{Namespace: "minecraft", Name: blockNames[i], Properties: tc.props})
			w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "comparator",
				Properties: map[string]string{"facing": "north", "mode": "compare", "powered": "false"}})
			w.Redstone.FlushUpdates()
			if got := w.Redstone.PowerAt(0, 64, 0); got != tc.want {
				t.Fatalf("%s comparator = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestChiseledBookshelfComparatorSignal(t *testing.T) {
	w := New(&FlatGenerator{}, nil, false)
	defer w.Close()
	bs := Block{Namespace: "minecraft", Name: "chiseled_bookshelf",
		Properties: map[string]string{
			"facing": "south", "slot_0_occupied": "false", "slot_1_occupied": "false",
			"slot_2_occupied": "false", "slot_3_occupied": "false",
			"slot_4_occupied": "false", "slot_5_occupied": "false",
		}}
	w.SetBlock(0, 64, -1, bs)
	w.SetBlock(0, 64, 0, Block{Namespace: "minecraft", Name: "comparator",
		Properties: map[string]string{"facing": "north", "mode": "compare", "powered": "false"}})

	// No interaction: output should be 0.
	w.Redstone.FlushUpdates()
	if got := w.Redstone.PowerAt(0, 64, 0); got != 0 {
		t.Fatalf("empty bookshelf comparator = %d, want 0", got)
	}

	// Simulate insert at slot 2 (1-based = 3).
	w.SetBookshelfLastSlot(0, 64, -1, 3)
	w.Redstone.FlushUpdates()
	if got := w.Redstone.PowerAt(0, 64, 0); got != 3 {
		t.Fatalf("bookshelf comparator after slot 3 insert = %d, want 3", got)
	}

	// Eject from slot 5 (1-based = 6).
	w.SetBookshelfLastSlot(0, 64, -1, 6)
	w.Redstone.FlushUpdates()
	if got := w.Redstone.PowerAt(0, 64, 0); got != 6 {
		t.Fatalf("bookshelf comparator after slot 6 eject = %d, want 6", got)
	}
}
