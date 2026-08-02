package bedrock

import (
	"testing"

	coreworld "GoCraft/core/world"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

func TestSubChunkHeightMapMixedColumns(t *testing.T) {
	chunk := &coreworld.Chunk{}
	setChunkBlock(chunk, 0, 64, 0)

	mapType, data := subChunkHeightMap(chunk, 4)
	if mapType != protocol.HeightMapDataHasData {
		t.Fatalf("height map type = %d, want has data", mapType)
	}
	if len(data) != 256 {
		t.Fatalf("height map length = %d, want 256", len(data))
	}
	if data[0] != 0 {
		t.Fatalf("height at 0,0 = %d, want 0", data[0])
	}
	if data[1] != -1 {
		t.Fatalf("height at 1,0 = %d, want -1", data[1])
	}
}

func TestSubChunkHeightMapAllAbove(t *testing.T) {
	chunk := &coreworld.Chunk{}
	for x := 0; x < 16; x++ {
		for z := 0; z < 16; z++ {
			setChunkBlock(chunk, x, 80, z)
		}
	}

	mapType, data := subChunkHeightMap(chunk, 4)
	if mapType != protocol.HeightMapDataTooHigh || data != nil {
		t.Fatalf("height map = (%d, %v), want too high with no data", mapType, data)
	}
}

func TestSubChunkHeightMapAllBelow(t *testing.T) {
	mapType, data := subChunkHeightMap(&coreworld.Chunk{}, 4)
	if mapType != protocol.HeightMapDataTooLow || data != nil {
		t.Fatalf("height map = (%d, %v), want too low with no data", mapType, data)
	}
}

func setChunkBlock(chunk *coreworld.Chunk, x, y, z int) {
	sectionIndex := (y - coreworld.WorldMinY) / coreworld.SectionSize
	if chunk.Sections[sectionIndex] == nil {
		chunk.Sections[sectionIndex] = coreworld.NewSection()
	}
	chunk.Sections[sectionIndex].Set(x, y&15, z, coreworld.Block{Namespace: "minecraft", Name: "stone"})
}
