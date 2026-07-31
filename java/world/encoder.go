package world

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"

	coreworld "GoCraft/core/world"
)

const maxPooledChunkBufferCapacity = 2 << 20

var chunkBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func acquireChunkBuffer() *bytes.Buffer {
	buf := chunkBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func releaseChunkBuffer(buf *bytes.Buffer) {
	if buf.Cap() > maxPooledChunkBufferCapacity {
		return
	}
	buf.Reset()
	chunkBufferPool.Put(buf)
}

func copyBufferBytes(buf *bytes.Buffer) []byte {
	return append([]byte(nil), buf.Bytes()...)
}

// EncodeHeightmaps returns the raw bytes of the NBT compound that goes into
// the Heightmaps field of the Level Chunk With Light packet (1.21.4).
//
// Two heightmap arrays are always included:
//   - MOTION_BLOCKING: the topmost block that prevents entity movement.
//   - WORLD_SURFACE:   the topmost non-air block (same for a flat world).
//
// Both arrays are packed long arrays with 9 bits per entry and 37 longs,
// encoding 256 identical values (one per column in the 16×16 chunk).
func EncodeHeightmaps(surfaceY int) []byte {
	longs := packHeightmap(surfaceY)
	buf := acquireChunkBuffer()
	defer releaseChunkBuffer(buf)
	writeNetworkNBTCompound(buf, func(w io.Writer) {
		writeNBTLongArray(w, "MOTION_BLOCKING", longs)
		writeNBTLongArray(w, "WORLD_SURFACE", longs)
	})
	return copyBufferBytes(buf)
}

// EncodeChunkHeightmaps derives both required heightmaps from the actual block
// columns in c. This supports generated hills, oceans, and loaded Anvil chunks.
func EncodeChunkHeightmaps(c *coreworld.Chunk) []byte {
	var surfaceYs [256]int
	for z := 0; z < coreworld.SectionSize; z++ {
		for x := 0; x < coreworld.SectionSize; x++ {
			surfaceYs[z*coreworld.SectionSize+x] = c.HighestBlockY(x, z)
		}
	}
	longs := packHeightmapValues(surfaceYs)
	buf := acquireChunkBuffer()
	defer releaseChunkBuffer(buf)
	writeNetworkNBTCompound(buf, func(w io.Writer) {
		writeNBTLongArray(w, "MOTION_BLOCKING", longs)
		writeNBTLongArray(w, "WORLD_SURFACE", longs)
	})
	return copyBufferBytes(buf)
}

// EncodeChunk converts a canonical Chunk into the raw byte slice that is sent
// as the Data (VarInt-prefixed ByteArray) field of Level Chunk With Light.
//
// Layout: for each of the 24 sections, bottom-to-top:
//
//	Int16              non-air block count
//	PalettedContainer  block states (indirect palette, ≥4 bits, or single value)
//	PalettedContainer  biomes       (single value for Milestone 4)
func EncodeChunk(c *coreworld.Chunk) []byte {
	buf := acquireChunkBuffer()
	defer releaseChunkBuffer(buf)
	encodeChunkTo(buf, c)
	return copyBufferBytes(buf)
}

// encodeChunkTo writes a chunk into a caller-owned buffer. Callers may reuse
// the buffer only after the consumer has synchronously copied its bytes.
func encodeChunkTo(buf *bytes.Buffer, c *coreworld.Chunk) {
	for _, sec := range c.Sections {
		encodeSection(buf, sec)
	}
}

// encodeSection writes one 16×16×16 section into buf.
func encodeSection(buf *bytes.Buffer, sec *coreworld.Section) {
	if sec == nil {
		writeInt16BE(buf, 0)
		writeSingleValue(buf, 0)
		writeSingleValue(buf, BiomeID("minecraft:plains"))
		return
	}

	writeInt16BE(buf, sec.NonAir)
	if sec.NonAir == 0 {
		writeSingleValue(buf, 0)
	} else {
		writeBlockStates(buf, sec)
	}
	writeBiomeStates(buf, sec)
}

// writeBiomeStates encodes the section's real 4x4x4 biome container. Java
// permits an indirect palette up to 3 bits; larger palettes use 6-bit global
// biome IDs directly.
func writeBiomeStates(buf *bytes.Buffer, sec *coreworld.Section) {
	palette := sec.BiomePalette()
	data := sec.BiomeData()
	if len(palette) <= 1 {
		writeSingleValue(buf, BiomeID(palette[0]))
		return
	}

	bitsPerEntry := bitsNeeded(len(palette))
	if bitsPerEntry < 1 {
		bitsPerEntry = 1
	}
	values := make([]int32, 64)
	if bitsPerEntry <= 3 {
		buf.WriteByte(byte(bitsPerEntry))
		writeVarInt(buf, int32(len(palette)))
		for _, biome := range palette {
			writeVarInt(buf, BiomeID(biome))
		}
		for i, paletteIndex := range data {
			values[i] = int32(paletteIndex)
		}
	} else {
		bitsPerEntry = 6
		buf.WriteByte(byte(bitsPerEntry))
		for i, paletteIndex := range data {
			if int(paletteIndex) >= len(palette) {
				paletteIndex = 0
			}
			values[i] = BiomeID(palette[paletteIndex])
		}
	}

	entriesPerLong := 64 / bitsPerEntry
	dataLen := (64 + entriesPerLong - 1) / entriesPerLong
	writeVarInt(buf, int32(dataLen))
	var longBuf [8]byte
	for longIndex := 0; longIndex < dataLen; longIndex++ {
		var packed uint64
		base := longIndex * entriesPerLong
		for entry := 0; entry < entriesPerLong && base+entry < 64; entry++ {
			packed |= uint64(values[base+entry]) << (entry * bitsPerEntry)
		}
		binary.BigEndian.PutUint64(longBuf[:], packed)
		buf.Write(longBuf[:])
	}
}

// writeSingleValue encodes a single-value PalettedContainer:
//
//	Byte(0)        bits_per_entry = 0 → single value
//	VarInt(value)  the global state / biome ID
//	VarInt(0)      data array length = 0
func writeSingleValue(buf *bytes.Buffer, value int32) {
	buf.WriteByte(0)
	writeVarInt(buf, value)
	writeVarInt(buf, 0)
}

// writeBlockStates encodes a Section's blocks as an indirect PalettedContainer.
//
// Java requires at least 4 bits per entry for block states (indirect palette
// threshold is 9; direct palette uses exactly 15 bits per entry).
// We always use indirect for palettes ≤ 256 entries.
//
// Wire layout (indirect):
//
//	Byte         bits_per_entry (≥ 4)
//	VarInt       palette size
//	VarInt[]     palette entries (Java global state IDs)
//	VarInt       data array length (in longs)
//	Long[]       packed bit array (no overflow: entries stay within one long)
func writeBlockStates(buf *bytes.Buffer, sec *coreworld.Section) {
	palette := sec.BlockPalette()

	// Map each canonical Block to its Java 1.21.4 global state ID.
	// The StateID lookup is purely a Java-adapter concern; the core palette
	// holds canonical Block values with no knowledge of Java IDs.
	javaIDs := make([]int32, len(palette))
	for i, block := range palette {
		javaIDs[i] = StateID(block)
	}

	// Determine bits per entry: minimum 4 for Java block states.
	bitsPerEntry := bitsNeeded(len(javaIDs))
	if bitsPerEntry < 4 {
		bitsPerEntry = 4
	}
	// If bitsPerEntry ≥ 9 we would need the direct (global) format; for M4
	// palettes are tiny so this branch is unreachable.

	buf.WriteByte(byte(bitsPerEntry))

	// Palette
	writeVarInt(buf, int32(len(javaIDs)))
	for _, id := range javaIDs {
		writeVarInt(buf, id)
	}

	// Pack block data: no-overflow packing (Java 1.16+ format).
	entriesPerLong := 64 / bitsPerEntry
	dataLen := (4096 + entriesPerLong - 1) / entriesPerLong // ceil(4096 / epl)
	writeVarInt(buf, int32(dataLen))

	data := sec.BlockData()
	var longBuf [8]byte
	for longIdx := 0; longIdx < dataLen; longIdx++ {
		var packed int64
		base := longIdx * entriesPerLong
		for entry := 0; entry < entriesPerLong; entry++ {
			blockIdx := base + entry
			if blockIdx >= 4096 {
				break
			}
			packed |= int64(data[blockIdx]) << (entry * bitsPerEntry)
		}
		binary.BigEndian.PutUint64(longBuf[:], uint64(packed))
		buf.Write(longBuf[:])
	}
}

// bitsNeeded returns ceil(log2(n)), minimum 1.
func bitsNeeded(n int) int {
	if n <= 1 {
		return 1
	}
	bits := 0
	n--
	for n > 0 {
		n >>= 1
		bits++
	}
	return bits
}

// writeInt16BE writes a big-endian signed 16-bit integer.
func writeInt16BE(buf *bytes.Buffer, v int16) {
	buf.Write([]byte{byte(v >> 8), byte(v)})
}

// writeVarInt appends a VarInt to buf.
func writeVarInt(buf *bytes.Buffer, v int32) {
	uv := uint32(v)
	for {
		if uv < 0x80 {
			buf.WriteByte(byte(uv))
			return
		}
		buf.WriteByte(byte(uv&0x7F) | 0x80)
		uv >>= 7
	}
}
