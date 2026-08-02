package world

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ── Air sub-chunk ─────────────────────────────────────────────────────────────

func TestAirSubChunkVersion(t *testing.T) {
	payload, err := EncodeAirSubChunk()
	if err != nil {
		t.Fatalf("EncodeAirSubChunk: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("empty payload")
	}
	if payload[0] != 0x08 {
		t.Errorf("version byte: got 0x%02x, want 0x08", payload[0])
	}
}

func TestAirSubChunkHeader(t *testing.T) {
	payload, err := EncodeAirSubChunk()
	if err != nil {
		t.Fatalf("EncodeAirSubChunk: %v", err)
	}
	// byte 1: (bitsPerBlock=0 << 1) | persistentFlag=1 = 0x01
	if payload[1] != 0x01 {
		t.Errorf("bitsPerBlock/persistent byte: got 0x%02x, want 0x01", payload[1])
	}
}

func TestAirSubChunkSingleValuePalette(t *testing.T) {
	payload, err := EncodeAirSubChunk()
	if err != nil {
		t.Fatalf("EncodeAirSubChunk: %v", err)
	}
	// After version(1) + header(1): LE int32 palette count = 1
	// No word array for bitsPerBlock=0.
	paletteCount := int32(binary.LittleEndian.Uint32(payload[2:6]))
	if paletteCount != 1 {
		t.Errorf("palette count: got %d, want 1", paletteCount)
	}
}

func TestAirSubChunkNBTContainsAir(t *testing.T) {
	payload, err := EncodeAirSubChunk()
	if err != nil {
		t.Fatalf("EncodeAirSubChunk: %v", err)
	}
	// The NBT compound should contain the string "minecraft:air".
	if !bytes.Contains(payload, []byte("minecraft:air")) {
		t.Error("air sub-chunk payload does not contain 'minecraft:air'")
	}
}

func TestAirSubChunkNoStoneOrOtherBlocks(t *testing.T) {
	payload, err := EncodeAirSubChunk()
	if err != nil {
		t.Fatalf("EncodeAirSubChunk: %v", err)
	}
	if bytes.Contains(payload, []byte("minecraft:stone")) {
		t.Error("air sub-chunk payload must not contain 'minecraft:stone'")
	}
}

func TestAirSubChunkMinimumSize(t *testing.T) {
	payload, err := EncodeAirSubChunk()
	if err != nil {
		t.Fatalf("EncodeAirSubChunk: %v", err)
	}
	// version(1) + bitsHeader(1) + paletteCount(4) + NBT(≥1)
	if len(payload) < 7 {
		t.Errorf("payload too short: %d bytes", len(payload))
	}
}

// ── Ground sub-chunk ──────────────────────────────────────────────────────────

func TestGroundSubChunkVersion(t *testing.T) {
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	if payload[0] != 0x08 {
		t.Errorf("version byte: got 0x%02x, want 0x08", payload[0])
	}
}

func TestGroundSubChunkBitsPerBlock(t *testing.T) {
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	// byte 1: (bitsPerBlock=1 << 1) | persistentFlag=1 = 0x03
	if payload[1] != 0x03 {
		t.Errorf("bitsPerBlock/persistent byte: got 0x%02x, want 0x03", payload[1])
	}
}

func TestGroundSubChunkWordArrayLength(t *testing.T) {
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	// version(1) + header(1) = 2 bytes before word array.
	// bitsPerBlock=1: ceil(4096 / 32) = 128 words × 4 bytes = 512 bytes.
	// So palette count starts at offset 2 + 512 = 514.
	if len(payload) < 514+4 {
		t.Fatalf("payload too short to contain word array + palette count: %d bytes", len(payload))
	}
	paletteCount := int32(binary.LittleEndian.Uint32(payload[514:518]))
	if paletteCount != 2 {
		t.Errorf("palette count: got %d, want 2", paletteCount)
	}
}

func TestGroundSubChunkY0StoneLayer(t *testing.T) {
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	// Y=0 blocks (x=0..15, z=0..15) occupy word indices 0..7.
	// block index = x | (z<<4) | (y<<8); for y=0: indices 0..255.
	// bitsPerBlock=1, 32 blocks/word → first 8 words cover the Y=0 layer.
	// All Y=0 blocks should be stone (palette index 1) → all word bits = 1 → 0xFFFFFFFF.
	for i := range 8 {
		off := 2 + i*4
		w := binary.LittleEndian.Uint32(payload[off : off+4])
		if w != 0xFFFF_FFFF {
			t.Errorf("word[%d] (Y=0 layer): got 0x%08x, want 0xffffffff", i, w)
		}
	}
}

func TestGroundSubChunkY1to15AirLayer(t *testing.T) {
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	// Words 8..127 cover Y=1..15: all air (palette index 0) → bits = 0.
	for i := 8; i < 128; i++ {
		off := 2 + i*4
		w := binary.LittleEndian.Uint32(payload[off : off+4])
		if w != 0 {
			t.Errorf("word[%d] (Y=1..15 layer): got 0x%08x, want 0x00000000", i, w)
		}
	}
}

func TestGroundSubChunkPaletteHasAirAndStone(t *testing.T) {
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	if !bytes.Contains(payload, []byte("minecraft:air")) {
		t.Error("ground sub-chunk payload does not contain 'minecraft:air'")
	}
	if !bytes.Contains(payload, []byte("minecraft:stone")) {
		t.Error("ground sub-chunk payload does not contain 'minecraft:stone'")
	}
}

func TestGroundSubChunkWordBoundaryAtY0End(t *testing.T) {
	// Verify the boundary between Y=0 (stone) and Y=1 (air) falls on word boundary 8.
	// Word 7 must be all 1s (stone) and word 8 must be all 0s (air).
	payload, err := EncodeGroundSubChunk()
	if err != nil {
		t.Fatalf("EncodeGroundSubChunk: %v", err)
	}
	word7 := binary.LittleEndian.Uint32(payload[2+7*4 : 2+7*4+4])
	word8 := binary.LittleEndian.Uint32(payload[2+8*4 : 2+8*4+4])
	if word7 != 0xFFFF_FFFF {
		t.Errorf("word[7] (last Y=0 word): got 0x%08x, want 0xffffffff", word7)
	}
	if word8 != 0 {
		t.Errorf("word[8] (first Y=1 word): got 0x%08x, want 0x00000000", word8)
	}
}

// ── Biome / LevelChunk payload ─────────────────────────────────────────────────

func TestLevelChunkPayloadLength(t *testing.T) {
	payload := EncodeLevelChunkPayload()
	// 24 sub-chunks × 9 bytes each + 1 byte border block varint = 217
	const want = overworldSubChunkCount*2 + 1
	if len(payload) != want {
		t.Errorf("LevelChunk payload length: got %d, want %d", len(payload), want)
	}
}

func TestLevelChunkPayloadBiomeHeader(t *testing.T) {
	payload := EncodeLevelChunkPayload()
	// Each biome storage entry starts with 0x00 (bitsPerBlock=0, runtime).
	for i := range overworldSubChunkCount {
		off := i * 2
		if payload[off] != 0x01 {
			t.Errorf("sub-chunk[%d] biome header: got 0x%02x, want 0x01", i, payload[off])
		}
	}
}

func TestLevelChunkPayloadBiomeIDs(t *testing.T) {
	payload := EncodeLevelChunkPayload()
	for i := range overworldSubChunkCount {
		off := i * 2
		if payload[off+1] != byte(biomePlains<<1) {
			t.Errorf("sub-chunk[%d] biome ID byte: got %d, want %d (plains)", i, payload[off+1], biomePlains<<1)
		}
	}
}

func TestLevelChunkPayloadBorderBlocksZero(t *testing.T) {
	payload := EncodeLevelChunkPayload()
	last := payload[len(payload)-1]
	if last != 0x00 {
		t.Errorf("border block varint: got 0x%02x, want 0x00", last)
	}
}

func TestLevelChunkPayloadAllSubChunksIdentical(t *testing.T) {
	payload := EncodeLevelChunkPayload()
	first := payload[0:2]
	for i := 1; i < overworldSubChunkCount; i++ {
		chunk := payload[i*2 : i*2+2]
		if !bytes.Equal(first, chunk) {
			t.Errorf("sub-chunk[%d] differs from sub-chunk[0]", i)
		}
	}
}

// ── NBT round-trip ────────────────────────────────────────────────────────────

func TestBlockStateNBTRoundTrip(t *testing.T) {
	for _, name := range []string{"minecraft:air", "minecraft:stone"} {
		b, err := marshalBlockState(name, nil)
		if err != nil {
			t.Errorf("marshalBlockState(%q): %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("marshalBlockState(%q): empty output", name)
		}
		// The serialised NBT must contain the block name as a UTF-8 string.
		if !bytes.Contains(b, []byte(name)) {
			t.Errorf("marshalBlockState(%q): name not found in output", name)
		}
	}
}

func TestBlockStateNBTHasStatesCompound(t *testing.T) {
	b, err := marshalBlockState("minecraft:stone", map[string]any{"stone_type": "stone"})
	if err != nil {
		t.Fatalf("marshalBlockState: %v", err)
	}
	if !bytes.Contains(b, []byte("stone_type")) {
		t.Error("NBT output does not contain state key 'stone_type'")
	}
}

// ── Constants ─────────────────────────────────────────────────────────────────

func TestGroundSubChunkIndexConstant(t *testing.T) {
	if GroundSubChunkIndex() != 8 {
		t.Errorf("GroundSubChunkIndex: got %d, want 8", GroundSubChunkIndex())
	}
}

func TestSpawnYConstant(t *testing.T) {
	if SpawnY() != 65 {
		t.Errorf("SpawnY: got %v, want 65", SpawnY())
	}
}
