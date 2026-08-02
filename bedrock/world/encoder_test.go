package world

import (
	"encoding/binary"
	"testing"

	coreworld "GoCraft/core/world"
)

import dfworld "github.com/df-mc/dragonfly/server/world"

func TestCanonicalSubChunkUsesVersionNineAndAbsoluteY(t *testing.T) {
	section := coreworld.NewSection()
	section.Set(1, 2, 3, coreworld.Block{Namespace: "minecraft", Name: "stone"})

	payload, err := NewEncoder().EncodeSubChunk(section, -3)
	if err != nil {
		t.Fatalf("EncodeSubChunk: %v", err)
	}
	if len(payload) < 4 {
		t.Fatalf("payload too short: %d", len(payload))
	}
	if payload[0] != 9 || payload[1] != 1 || int8(payload[2]) != -3 {
		t.Fatalf("header = [%d %d %d], want [9 1 -3]", payload[0], payload[1], int8(payload[2]))
	}
	if payload[3]&1 == 0 {
		t.Fatal("block palette is not marked as a network palette")
	}
}

func TestCanonicalAirSectionUsesAllAirResult(t *testing.T) {
	payload, err := NewEncoder().EncodeSubChunk(nil, 0)
	if err != nil {
		t.Fatalf("EncodeSubChunk: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("air payload length = %d, want 0", len(payload))
	}
}

func TestKnownBlocksResolveToDifferentNetworkHashes(t *testing.T) {
	encoder := NewEncoder()
	air := encoder.BlockNetworkID(coreworld.Air)
	stone := encoder.BlockNetworkID(coreworld.Block{Namespace: "minecraft", Name: "stone"})
	if air == stone {
		t.Fatalf("air and stone resolved to the same network ID %d", air)
	}
}

func TestCanonicalSubChunkReordersBlocksForBedrock(t *testing.T) {
	section := coreworld.NewSection()
	section.Set(1, 2, 3, coreworld.Block{Namespace: "minecraft", Name: "stone"})

	payload, err := NewEncoder().EncodeSubChunk(section, 0)
	if err != nil {
		t.Fatalf("EncodeSubChunk: %v", err)
	}
	if bits := payload[3] >> 1; bits != 1 {
		t.Fatalf("bits per block = %d, want 1", bits)
	}

	// Bedrock's flat index is X*256 + Z*16 + Y. The canonical section's
	// Y*256 + Z*16 + X index must not leak directly onto the wire.
	bedrockIndex := 1*256 + 3*16 + 2
	canonicalIndex := 2*256 + 3*16 + 1
	if got := packedPaletteIndex(payload[4:], bedrockIndex, 1); got != 1 {
		t.Fatalf("palette index at Bedrock position = %d, want 1", got)
	}
	if got := packedPaletteIndex(payload[4:], canonicalIndex, 1); got != 0 {
		t.Fatalf("palette index at unconverted canonical position = %d, want 0", got)
	}
}

func packedPaletteIndex(words []byte, index, bits int) uint32 {
	valuesPerWord := 32 / bits
	wordIndex := index / valuesPerWord
	bitOffset := (index % valuesPerWord) * bits
	word := binary.LittleEndian.Uint32(words[wordIndex*4:])
	return (word >> bitOffset) & (1<<bits - 1)
}

func TestBlockNetworkIDsBelongToAdvertisedPalette(t *testing.T) {
	encoder := NewEncoder()
	air := encoder.BlockNetworkID(coreworld.Air)
	stone := encoder.BlockNetworkID(coreworld.Block{Namespace: `minecraft`, Name: `stone`})
	registry := dfworld.DefaultBlockRegistry
	registry.Finalize()
	airName, _, ok := registry.RuntimeIDToState(air)
	if !ok || airName != `minecraft:air` {
		t.Fatalf(`air runtime ID %d maps to %q, ok=%v`, air, airName, ok)
	}
	stoneName, _, ok := registry.RuntimeIDToState(stone)
	if !ok || stoneName != `minecraft:stone` {
		t.Fatalf(`stone runtime ID %d maps to %q, ok=%v`, stone, stoneName, ok)
	}
}
