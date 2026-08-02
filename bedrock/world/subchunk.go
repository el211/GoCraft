// Package world provides Bedrock Edition world encoding utilities for GoCraft.
//
// This file contains common Bedrock sub-chunk and biome-storage primitives.
// encoder.go performs canonical GoCraft block-to-Bedrock state translation;
// these helpers remain useful for empty sections and initial chunk payloads.
package world

import (
	"bytes"
	"encoding/binary"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// Overworld sub-chunk layout for Bedrock 1.18+ (Y range −64..320):
//
//	Sub-chunk 0 → Y = −64 to −49
//	Sub-chunk 8 → Y =   64 to  79   ← ground level for flat world
//	Sub-chunk 23 → Y = 304 to 319
const (
	groundSubChunkIndex = int32(8) // contains Y=64..79
	spawnY              = float32(65)

	// overworldSubChunkCount is the number of sub-chunks in the 1.18+ overworld
	// (Y range −64 to 319 = 384 blocks = 24 sub-chunks).
	overworldSubChunkCount = 24

	// biomePlains is the Bedrock runtime biome ID for the plains biome.
	// Biome IDs are fixed numeric constants defined by the game; plains = 1.
	biomePlains = uint32(1)
)

// SpawnY returns the Y coordinate players should spawn at in the flat world.
func SpawnY() float32 { return spawnY }

// GroundSubChunkIndex returns the sub-chunk index that contains the surface.
func GroundSubChunkIndex() int32 { return groundSubChunkIndex }

// blockState is the NBT structure of a Bedrock block palette entry.
type blockState struct {
	Name   string         `nbt:"name"`
	States map[string]any `nbt:"states"`
}

// EncodeAirSubChunk returns a valid version-8 sub-chunk payload that contains
// only air.  Uses a single-value (bitsPerBlock=0) persistent palette to
// minimise payload size.
//
// Format:
//
//	0x08           version 8
//	0x01           bitsPerBlock=0 | persistentFlag=1  → (0<<1)|1 = 1
//	LE int32 = 1   palette count
//	NBT compound   minecraft:air
func EncodeAirSubChunk() ([]byte, error) {
	airNBT, err := marshalBlockState("minecraft:air", nil)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteByte(0x08) // version
	buf.WriteByte(0x01) // bitsPerBlock=0, persistent
	writeInt32LE(&buf, 1)
	buf.Write(airNBT)
	return buf.Bytes(), nil
}

// EncodeGroundSubChunk returns a valid version-8 sub-chunk payload for the
// ground level (sub-chunk index 8, Y=64..79 in the overworld).
//
// Block layout:
//   - Y=0 (global Y=64): all 256 blocks = minecraft:stone
//   - Y=1..15 (global Y=65..79): all blocks = air
//
// Format:
//
//	0x08                version 8
//	0x03                bitsPerBlock=1 | persistentFlag=1 → (1<<1)|1 = 3
//	128 LE uint32s      packed block indices (1 bit each, 32 per word)
//	  [8 × 0xFFFFFFFF] Y=0 blocks (stone, palette index 1)
//	  [120 × 0x00000000] Y=1..15 (air, palette index 0)
//	LE int32 = 2        palette count
//	NBT compound        minecraft:air   (index 0)
//	NBT compound        minecraft:stone (index 1)
func EncodeGroundSubChunk() ([]byte, error) {
	airNBT, err := marshalBlockState("minecraft:air", nil)
	if err != nil {
		return nil, err
	}
	stoneNBT, err := marshalBlockState("minecraft:stone", nil)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteByte(0x08) // version 8
	buf.WriteByte(0x03) // bitsPerBlock=1, persistent

	// 128 × LE uint32 (1 bit per block, 4096 blocks total, 32 per uint32).
	// Block index order within a sub-chunk: idx = x | (z << 4) | (y << 8)
	// Y=0: indices 0..255 → first 8 words → all ones (palette index 1 = stone)
	// Y=1..15: indices 256..4095 → remaining 120 words → all zeros (air)
	word := make([]byte, 4)
	for i := range 128 {
		var w uint32
		if i < 8 {
			w = 0xFFFF_FFFF // Y=0 layer: stone
		}
		binary.LittleEndian.PutUint32(word, w)
		buf.Write(word)
	}

	writeInt32LE(&buf, 2) // 2 palette entries
	buf.Write(airNBT)
	buf.Write(stoneNBT)
	return buf.Bytes(), nil
}

// EncodeLevelChunkPayload returns the RawPayload for a LevelChunk packet
// sent with SubChunkCount = SubChunkRequestModeLimitless (CacheEnabled=false).
//
// In this mode sub-chunk block data is NOT included — sub-chunks are requested
// individually via SubChunkRequest packets.  The payload contains only:
//
//  1. 3D biome palette storage for each overworld sub-chunk
//     (24 × single-value plains storage, 9 bytes each = 216 bytes)
//  2. Border block count varint (0x00 — no border blocks)
//
// Biome palette storage format (runtime, not persistent):
//
//	byte:    (bitsPerBlock << 1) | 0   →  0x00 for single-value
//	uint32:  palette count = 1
//	uint32:  biome runtime ID (1 = plains)
func EncodeLevelChunkPayload() []byte {
	var buf bytes.Buffer
	buf.Grow(overworldSubChunkCount*2 + 1)

	// One biome palette entry per sub-chunk: all plains.
	entry := makeSingleValueBiomeStorage(biomePlains)
	for range overworldSubChunkCount {
		buf.Write(entry)
	}

	buf.WriteByte(0x00) // border block count varint (0 = none)
	return buf.Bytes()
}

// makeSingleValueBiomeStorage encodes a single-value runtime palette storage
// (bitsPerBlock=0) for the given biome ID.  The returned slice is 9 bytes.
func makeSingleValueBiomeStorage(biomeID uint32) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x01) // bitsPerBlock=0, network palette
	_ = protocol.WriteVarint32(&buf, int32(biomeID))
	return buf.Bytes()
}

// marshalBlockState serialises a Bedrock block state to Network Little Endian
// NBT format, as required by the persistent sub-chunk palette.
// states may be nil for blocks with no properties.
func marshalBlockState(name string, states map[string]any) ([]byte, error) {
	if states == nil {
		states = map[string]any{}
	}
	return nbt.MarshalEncoding(blockState{Name: name, States: states}, nbt.NetworkLittleEndian)
}

func writeInt32LE(buf *bytes.Buffer, v int32) {
	b := [4]byte{}
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	buf.Write(b[:])
}
