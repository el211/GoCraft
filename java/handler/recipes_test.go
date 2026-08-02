package handler

import (
	"bytes"
	"testing"

	"GoCraft/java/protocol"
)

func TestRecipeBookContainsCompleteJava1214FixedCatalog(t *testing.T) {
	if err := validateRecipeDisplays(); err != nil {
		t.Fatalf("validate recipes: %v", err)
	}
	if javaRecipeSourceCount != 1370 {
		t.Fatalf("source recipe count = %d, want 1370", javaRecipeSourceCount)
	}
	if len(javaRecipeDisplays) != 1358 || len(javaRecipeDynamicRecipes) != 12 {
		t.Fatalf("catalog counts fixed=%d dynamic=%d, want 1358/12", len(javaRecipeDisplays), len(javaRecipeDynamicRecipes))
	}

	stationCounts := make(map[string]int)
	names := make(map[string]struct{}, len(javaRecipeDisplays))
	for _, recipe := range javaRecipeDisplays {
		stationCounts[recipe.station]++
		if _, duplicate := names[recipe.name]; duplicate {
			t.Fatalf("duplicate synchronized recipe %s", recipe.name)
		}
		names[recipe.name] = struct{}{}
	}
	wantStations := map[string]int{
		"minecraft:crafting_table": 964,
		"minecraft:furnace":        71,
		"minecraft:blast_furnace":  24,
		"minecraft:smoker":         9,
		"minecraft:campfire":       9,
		"minecraft:stonecutter":    254,
		"minecraft:smithing_table": 27,
	}
	for station, want := range wantStations {
		if got := stationCounts[station]; got != want {
			t.Errorf("%s recipe count = %d, want %d", station, got, want)
		}
	}
	for _, required := range []string{
		"minecraft:crafting_table", "minecraft:netherite_sword_smithing",
		"minecraft:gold_ingot_from_blasting_deepslate_gold_ore",
		"minecraft:andesite_slab_from_andesite_stonecutting",
	} {
		if _, ok := names[required]; !ok {
			t.Errorf("missing recipe %s", required)
		}
	}

	packet := buildRecipeBookAdd()
	if packet.ID != packetIDRecipeBookAdd {
		t.Fatalf("packet ID = 0x%x, want 0x%x", packet.ID, packetIDRecipeBookAdd)
	}
	reader := bytes.NewReader(packet.Data)
	count, err := protocol.ReadVarInt(reader)
	if err != nil {
		t.Fatal(err)
	}
	if int(count) != len(javaRecipeDisplays) {
		t.Fatalf("entry count = %d, want %d", count, len(javaRecipeDisplays))
	}
	for index, want := range javaRecipeDisplays {
		displayID := mustReadRecipeVarInt(t, reader)
		if displayID != int32(index) {
			t.Fatalf("entry %d display ID = %d", index, displayID)
		}
		kind := mustReadRecipeVarInt(t, reader)
		if kind != want.kind {
			t.Fatalf("entry %d kind = %d, want %d", index, kind, want.kind)
		}
		skipRecipeDisplayPayload(t, reader, kind)
		_ = mustReadRecipeVarInt(t, reader)
		if category := mustReadRecipeVarInt(t, reader); category != want.category {
			t.Fatalf("entry %d category = %d, want %d", index, category, want.category)
		}
		if requirements, err := protocol.ReadBool(reader); err != nil || requirements {
			t.Fatalf("entry %d requirements = %v, err=%v", index, requirements, err)
		}
		if _, err := protocol.ReadByte(reader); err != nil {
			t.Fatalf("entry %d flags: %v", index, err)
		}
	}
	if replace, err := protocol.ReadBool(reader); err != nil || !replace {
		t.Fatalf("replace = %v, err=%v", replace, err)
	}
	if reader.Len() != 0 {
		t.Fatalf("recipe packet has %d trailing bytes", reader.Len())
	}
}

func skipRecipeDisplayPayload(t *testing.T, reader *bytes.Reader, kind int32) {
	t.Helper()
	switch kind {
	case recipeDisplayShapeless:
		count := mustReadRecipeVarInt(t, reader)
		for i := int32(0); i < count; i++ {
			skipSlotDisplay(t, reader)
		}
		skipSlotDisplay(t, reader)
		skipSlotDisplay(t, reader)
	case recipeDisplayShaped:
		_ = mustReadRecipeVarInt(t, reader)
		_ = mustReadRecipeVarInt(t, reader)
		count := mustReadRecipeVarInt(t, reader)
		for i := int32(0); i < count; i++ {
			skipSlotDisplay(t, reader)
		}
		skipSlotDisplay(t, reader)
		skipSlotDisplay(t, reader)
	case recipeDisplayFurnace:
		for i := 0; i < 4; i++ {
			skipSlotDisplay(t, reader)
		}
		_ = mustReadRecipeVarInt(t, reader)
		if _, err := protocol.ReadFloat(reader); err != nil {
			t.Fatal(err)
		}
	case recipeDisplayStonecutter:
		for i := 0; i < 3; i++ {
			skipSlotDisplay(t, reader)
		}
	case recipeDisplaySmithing:
		for i := 0; i < 5; i++ {
			skipSlotDisplay(t, reader)
		}
	default:
		t.Fatalf("unknown recipe display type %d", kind)
	}
}

func mustReadRecipeVarInt(t *testing.T, reader *bytes.Reader) int32 {
	t.Helper()
	value, err := protocol.ReadVarInt(reader)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func skipSlotDisplay(t *testing.T, reader *bytes.Reader) {
	t.Helper()
	displayType := mustReadRecipeVarInt(t, reader)
	switch displayType {
	case slotDisplayEmpty, slotDisplayAnyFuel:
		return
	case slotDisplayItem:
		_ = mustReadRecipeVarInt(t, reader)
	case slotDisplayStack:
		count := mustReadRecipeVarInt(t, reader)
		if count <= 0 {
			return
		}
		_ = mustReadRecipeVarInt(t, reader)
		added := mustReadRecipeVarInt(t, reader)
		removed := mustReadRecipeVarInt(t, reader)
		for i := int32(0); i < added; i++ {
			switch component := mustReadRecipeVarInt(t, reader); component {
			case 2, 3:
				_ = mustReadRecipeVarInt(t, reader)
			case 8:
				lines := mustReadRecipeVarInt(t, reader)
				for line := int32(0); line < lines; line++ {
					if err := skipNetworkNBT(reader); err != nil {
						t.Fatalf("skip lore component: %v", err)
					}
				}
			case 13:
				attributes := mustReadRecipeVarInt(t, reader)
				for attribute := int32(0); attribute < attributes; attribute++ {
					_ = mustReadRecipeVarInt(t, reader)
					if _, err := protocol.ReadString(reader); err != nil {
						t.Fatal(err)
					}
					if _, err := protocol.ReadDouble(reader); err != nil {
						t.Fatal(err)
					}
					_ = mustReadRecipeVarInt(t, reader)
					_ = mustReadRecipeVarInt(t, reader)
				}
				if _, err := protocol.ReadBool(reader); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unexpected component %d", component)
			}
		}
		for i := int32(0); i < removed; i++ {
			_ = mustReadRecipeVarInt(t, reader)
		}
	case slotDisplayTag:
		if _, err := protocol.ReadString(reader); err != nil {
			t.Fatal(err)
		}
	case slotDisplaySmithingTrim:
		for i := 0; i < 3; i++ {
			skipSlotDisplay(t, reader)
		}
	case slotDisplayWithRemainder:
		for i := 0; i < 2; i++ {
			skipSlotDisplay(t, reader)
		}
	case slotDisplayComposite:
		count := mustReadRecipeVarInt(t, reader)
		for i := int32(0); i < count; i++ {
			skipSlotDisplay(t, reader)
		}
	default:
		t.Fatalf("unsupported slot display type %d", displayType)
	}
}
