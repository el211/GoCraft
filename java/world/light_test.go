package world

import (
	"math/bits"
	"testing"

	coreworld "GoCraft/core/world"
)

func TestBuildSkyLightDarkensUndergroundAndLightsSky(t *testing.T) {
	chunk := (&coreworld.FlatGenerator{}).Generate(0, 0)
	skyMask, emptyMask, arrays := buildSkyLight(chunk)
	const all = uint64((1 << 26) - 1)
	if uint64(skyMask|emptyMask) != all || skyMask&emptyMask != 0 {
		t.Fatalf("sky masks overlap or omit sections: sky=%026b empty=%026b", skyMask, emptyMask)
	}
	if got := bits.OnesCount64(uint64(skyMask)); got != len(arrays) {
		t.Fatalf("sky arrays=%d, mask bits=%d", len(arrays), got)
	}
	// Y=0 is world section 4, represented by light-mask bit 5.
	if emptyMask&(int64(1)<<5) == 0 {
		t.Fatalf("underground section is not marked empty: %026b", emptyMask)
	}
	// Y=80 is section 9, represented by bit 10, and must see the sky.
	if skyMask&(int64(1)<<10) == 0 {
		t.Fatalf("above-ground section has no sky data: %026b", skyMask)
	}
	if len(arrays) >= 26 {
		t.Fatalf("sent %d sky arrays, want underground zero sections omitted", len(arrays))
	}
}

func TestVillageBlocksDoNotOccludeSkyLight(t *testing.T) {
	for _, block := range []string{
		"minecraft:oak_door",
		"minecraft:acacia_fence",
		"minecraft:glass_pane",
		"minecraft:oak_stairs",
		"minecraft:red_bed",
		"minecraft:wheat",
	} {
		if !isSkyTransparent(block) {
			t.Errorf("isSkyTransparent(%q) = false, want true", block)
		}
	}
}

func TestSkyLightPropagatesLaterallyUnderVillageRoof(t *testing.T) {
	chunk := &coreworld.Chunk{}
	sectionIndex := (64 - coreworld.WorldMinY) / coreworld.SectionSize
	section := coreworld.NewSection()
	chunk.Sections[sectionIndex] = section

	stone := coreworld.Block{Namespace: "minecraft", Name: "stone"}
	door := coreworld.Block{Namespace: "minecraft", Name: "oak_door"}
	// A five-by-five opaque roof blocks direct vertical skylight over the door,
	// while its open sides allow daylight to propagate laterally.
	for z := 5; z <= 9; z++ {
		for x := 5; x <= 9; x++ {
			section.Set(x, 2, z, stone) // world Y 66
		}
	}
	section.Set(7, 0, 7, door) // world Y 64

	levels := computeSkyLightLevels(chunk)
	defer releaseSkyLightLevels(levels)
	got := levels[skyCellIndex(7, 64, 7)]
	if got == 0 {
		t.Fatal("door beneath an open-sided roof received zero lateral skylight")
	}
	if got >= 15 {
		t.Fatalf("door beneath roof light = %d, want attenuated lateral light", got)
	}
}
