package world

import (
	"fmt"
	"strings"
	"sync"

	coreworld "GoCraft/core/world"
	"GoCraft/internal/protocoldata"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
)

// packetIDLevelChunkWithLight is the server-to-client Play packet ID for
// "Level Chunk With Light", resolved from the embedded protocol data at init.
var packetIDLevelChunkWithLight = protocoldata.MustCB("play", "minecraft:level_chunk_with_light")

// Sender converts canonical chunks into Java chunk packets. Heightmaps are
// derived from each chunk's actual block columns.
type Sender struct {
	// SurfaceY is retained for source compatibility with older callers.
	// It is deprecated and no longer used during chunk encoding.
	SurfaceY int
}

// DefaultSender encodes canonical chunks for Java Edition.
var DefaultSender = &Sender{SurfaceY: 63}

var skyLightPagePool = sync.Pool{New: func() any { return new([2048]byte) }}

func acquireSkyLightPage() []byte {
	page := skyLightPagePool.Get().(*[2048]byte)
	clear(page[:])
	return page[:]
}

func releaseSkyLightPage(page []byte) {
	if len(page) == 2048 && cap(page) == 2048 {
		skyLightPagePool.Put((*[2048]byte)(page))
	}
}

func releaseSkyLightPages(pages [][]byte) {
	for _, page := range pages {
		releaseSkyLightPage(page)
	}
}

// SendChunk encodes c and sends it to conn as a Level Chunk With Light packet.
//
// Packet layout (1.21.4):
//
//	Int        Chunk X
//	Int        Chunk Z
//	NBT        Heightmaps (MOTION_BLOCKING + WORLD_SURFACE long arrays)
//	ByteArray  Data (24 encoded sections)
//	VarInt     Block entity count, followed by encoded block entities
//	-- Light update fields --
//	BitSet     Sky Light Mask (only sections containing sky light)
//	BitSet     Block Light Mask (empty)
//	BitSet     Empty Sky Light Mask (sections with zero sky light)
//	BitSet     Empty Block Light Mask (all 26 sections)
//	VarInt     Sky Light array count
//	ByteArray  One 2048-byte nibble array per non-empty sky section
//	VarInt     Block Light array count (0)
func (s *Sender) SendChunk(conn *network.ClientConn, c *coreworld.Chunk) error {
	heightmapNBT := EncodeChunkHeightmaps(c)
	chunkBuffer := acquireChunkBuffer()
	defer releaseChunkBuffer(chunkBuffer)
	encodeChunkTo(chunkBuffer, c)
	skyMask, emptySkyMask, skyArrays := buildSkyLight(c)
	defer releaseSkyLightPages(skyArrays)

	type encodedBlockEntity struct {
		entity coreworld.BlockEntity
		typeID int32
	}
	blockEntities := make([]encodedBlockEntity, 0, len(c.BlockEntities))
	for _, entity := range c.BlockEntities {
		if typeID, ok := BlockEntityTypeID(entity.Type); ok && len(entity.Data) > 0 {
			blockEntities = append(blockEntities, encodedBlockEntity{entity: entity, typeID: typeID})
		}
	}

	const allSectionsMask = int64((int64(1) << 26) - 1)
	b := protocol.NewBuilder(packetIDLevelChunkWithLight).
		Int(c.X).
		Int(c.Z).
		Bytes(heightmapNBT).
		ByteArray(chunkBuffer.Bytes()).
		VarInt(int32(len(blockEntities)))
	for _, encoded := range blockEntities {
		entity := encoded.entity
		packedXZ := byte((entity.X&15)<<4 | (entity.Z & 15))
		b.Byte(packedXZ).Short(int16(entity.Y)).VarInt(encoded.typeID).Bytes(entity.Data)
	}
	b.VarInt(1).Long(skyMask).
		VarInt(0). // block light mask
		VarInt(1).Long(emptySkyMask).
		VarInt(1).Long(allSectionsMask). // empty block light mask
		VarInt(int32(len(skyArrays)))
	for _, light := range skyArrays {
		b.ByteArray(light)
	}
	b.VarInt(0) // no block-light arrays
	return conn.WritePacket(b.Build())
}

// buildSkyLight returns protocol masks and 2048-byte nibble arrays for the 24
// world sections plus the two boundary light sections. Terrain and caves below
// the highest opaque block receive zero sky light; open sky receives level 15.
const chunkSkyLightCells = coreworld.SectionCount * coreworld.SectionSize * coreworld.SectionSize * coreworld.SectionSize

var (
	skyLightLevelPool = sync.Pool{New: func() any { return new([chunkSkyLightCells]byte) }}
	skyLightQueuePool = sync.Pool{New: func() any {
		queue := make([]int, 0, chunkSkyLightCells/8)
		return &queue
	}}
)

func acquireSkyLightLevels() []byte {
	levels := skyLightLevelPool.Get().(*[chunkSkyLightCells]byte)
	clear(levels[:])
	return levels[:]
}

func releaseSkyLightLevels(levels []byte) {
	if len(levels) == chunkSkyLightCells && cap(levels) == chunkSkyLightCells {
		skyLightLevelPool.Put((*[chunkSkyLightCells]byte)(levels))
	}
}

func chunkBlockAt(c *coreworld.Chunk, x, worldY, z int) coreworld.Block {
	sectionIndex := (worldY - coreworld.WorldMinY) / coreworld.SectionSize
	if sectionIndex < 0 || sectionIndex >= coreworld.SectionCount {
		return coreworld.Air
	}
	return c.Sections[sectionIndex].At(x, (worldY-coreworld.WorldMinY)&15, z)
}

func skyCellIndex(x, worldY, z int) int {
	return (worldY-coreworld.WorldMinY)*256 + z*16 + x
}

// computeSkyLightLevels first fills directly sky-exposed transparent cells with
// level 15, then performs a bounded flood through transparent cells. This
// models the lateral daylight that vanilla uses beneath roof overhangs and
// prevents doors, crops, and interiors from rendering pitch black.
func computeSkyLightLevels(c *coreworld.Chunk) []byte {
	levels := acquireSkyLightLevels()

	for z := 0; z < 16; z++ {
		for x := 0; x < 16; x++ {
			skyOpen := true
			for worldY := coreworld.WorldMaxY; worldY >= coreworld.WorldMinY; worldY-- {
				if !isSkyTransparent(chunkBlockAt(c, x, worldY, z).ResourceLocation()) {
					skyOpen = false
					continue
				}
				if skyOpen {
					levels[skyCellIndex(x, worldY, z)] = 15
				}
			}
		}
	}

	queuePtr := skyLightQueuePool.Get().(*[]int)
	queue := (*queuePtr)[:0]
	defer func() {
		clear(queue)
		*queuePtr = queue[:0]
		skyLightQueuePool.Put(queuePtr)
	}()

	// Seed only the dark cells bordering direct daylight. Enqueuing every
	// directly lit sky cell would create unnecessary work in empty sections.
	for worldY := coreworld.WorldMinY; worldY <= coreworld.WorldMaxY; worldY++ {
		for z := 0; z < 16; z++ {
			for x := 0; x < 16; x++ {
				index := skyCellIndex(x, worldY, z)
				if levels[index] != 0 || !isSkyTransparent(chunkBlockAt(c, x, worldY, z).ResourceLocation()) {
					continue
				}
				directNeighbor := false
				for _, delta := range [][3]int{{-1, 0, 0}, {1, 0, 0}, {0, -1, 0}, {0, 1, 0}, {0, 0, -1}, {0, 0, 1}} {
					nx, ny, nz := x+delta[0], worldY+delta[1], z+delta[2]
					if nx < 0 || nx >= 16 || nz < 0 || nz >= 16 || ny < coreworld.WorldMinY || ny > coreworld.WorldMaxY {
						continue
					}
					if levels[skyCellIndex(nx, ny, nz)] == 15 {
						directNeighbor = true
						break
					}
				}
				if directNeighbor {
					levels[index] = 14
					queue = append(queue, index)
				}
			}
		}
	}

	for head := 0; head < len(queue); head++ {
		index := queue[head]
		level := levels[index]
		if level <= 1 {
			continue
		}
		yOffset := index / 256
		rem := index % 256
		z := rem / 16
		x := rem % 16
		worldY := coreworld.WorldMinY + yOffset
		nextLevel := level - 1
		for _, delta := range [][3]int{{-1, 0, 0}, {1, 0, 0}, {0, -1, 0}, {0, 1, 0}, {0, 0, -1}, {0, 0, 1}} {
			nx, ny, nz := x+delta[0], worldY+delta[1], z+delta[2]
			if nx < 0 || nx >= 16 || nz < 0 || nz >= 16 || ny < coreworld.WorldMinY || ny > coreworld.WorldMaxY {
				continue
			}
			neighbor := skyCellIndex(nx, ny, nz)
			if levels[neighbor] >= nextLevel ||
				!isSkyTransparent(chunkBlockAt(c, nx, ny, nz).ResourceLocation()) {
				continue
			}
			levels[neighbor] = nextLevel
			queue = append(queue, neighbor)
		}
	}
	return levels
}

// buildSkyLight returns protocol masks and 2048-byte nibble arrays for the 24
// world sections plus the two boundary light sections.
func buildSkyLight(c *coreworld.Chunk) (skyMask, emptyMask int64, arrays [][]byte) {
	levels := computeSkyLightLevels(c)
	defer releaseSkyLightLevels(levels)

	// Bottom boundary section has no sky light.
	emptyMask |= 1
	for sectionIndex := 0; sectionIndex < coreworld.SectionCount; sectionIndex++ {
		light := acquireSkyLightPage()
		nonZero := false
		for localIndex := 0; localIndex < 4096; localIndex++ {
			level := levels[sectionIndex*4096+localIndex]
			if level == 0 {
				continue
			}
			byteIndex := localIndex >> 1
			if localIndex&1 == 0 {
				light[byteIndex] |= level
			} else {
				light[byteIndex] |= level << 4
			}
			nonZero = true
		}
		bit := int64(1) << (sectionIndex + 1)
		if nonZero {
			skyMask |= bit
			arrays = append(arrays, light)
		} else {
			emptyMask |= bit
			releaseSkyLightPage(light)
		}
	}

	// Top boundary section is full daylight.
	full := acquireSkyLightPage()
	for i := range full {
		full[i] = 0xff
	}
	skyMask |= int64(1) << 25
	arrays = append(arrays, full)
	return skyMask, emptyMask, arrays
}
func isSkyTransparent(resourceLocation string) bool {
	switch resourceLocation {
	case "", "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:water", "minecraft:glass", "minecraft:ice",
		// Leaves
		"minecraft:oak_leaves", "minecraft:birch_leaves", "minecraft:spruce_leaves",
		"minecraft:acacia_leaves", "minecraft:jungle_leaves",
		"minecraft:dark_oak_leaves", "minecraft:cherry_leaves",
		"minecraft:azalea_leaves", "minecraft:flowering_azalea_leaves",
		"minecraft:mangrove_leaves",
		// Short plants / ground cover (let sky light pass through)
		"minecraft:short_grass", "minecraft:grass", "minecraft:fern",
		"minecraft:dead_bush", "minecraft:seagrass",
		// Tall plants (both halves)
		"minecraft:tall_grass", "minecraft:large_fern",
		"minecraft:tall_seagrass",
		// Flowers
		"minecraft:dandelion", "minecraft:poppy", "minecraft:allium",
		"minecraft:azure_bluet", "minecraft:red_tulip", "minecraft:orange_tulip",
		"minecraft:white_tulip", "minecraft:pink_tulip", "minecraft:oxeye_daisy",
		"minecraft:cornflower", "minecraft:lily_of_the_valley", "minecraft:blue_orchid",
		"minecraft:wither_rose", "minecraft:torchflower", "minecraft:pitcher_plant",
		// Double-tall flowers
		"minecraft:sunflower", "minecraft:lilac", "minecraft:rose_bush", "minecraft:peony",
		// Other transparent / non-full-block
		"minecraft:sugar_cane", "minecraft:bamboo", "minecraft:lily_pad",
		"minecraft:brown_mushroom", "minecraft:red_mushroom",
		"minecraft:vine", "minecraft:moss_carpet",
		"minecraft:torch", "minecraft:wall_torch",
		"minecraft:ladder", "minecraft:rail",
		"minecraft:powered_rail", "minecraft:detector_rail", "minecraft:activator_rail":
		return true
	}
	// These families do not fill their block cube, so daylight can propagate
	// through them. Treating them as opaque makes doors, crops and roof stairs
	// render almost black even in direct daylight.
	for _, suffix := range []string{
		"_bed", "_button", "_carpet", "_door", "_fence", "_fence_gate",
		"_pane", "_pressure_plate", "_sapling", "_sign", "_slab", "_stairs",
		"_trapdoor", "_wall", "_wall_sign",
	} {
		if strings.HasSuffix(resourceLocation, suffix) {
			return true
		}
	}
	switch resourceLocation {
	case "minecraft:wheat", "minecraft:carrots", "minecraft:potatoes",
		"minecraft:beetroots", "minecraft:nether_wart", "minecraft:cocoa",
		"minecraft:sweet_berry_bush", "minecraft:torchflower_crop",
		"minecraft:pitcher_crop":
		return true
	}
	return false
}

// SendChunksAround sends all chunks in a square of radius r centred on chunk
// (cx, cz).  Chunks are fetched from w (generating them if necessary) and
// sent in spiral order — centre first, then outward — so the player sees their
// immediate surroundings first.
func (s *Sender) SendChunksAround(conn *network.ClientConn, w *coreworld.World, cx, cz, r int32) error {
	// Send centre chunk first, then spiral outward.
	for dx := -r; dx <= r; dx++ {
		for dz := -r; dz <= r; dz++ {
			c := w.Chunk(cx+int32(dx), cz+int32(dz))
			if err := s.SendChunk(conn, c); err != nil {
				return fmt.Errorf("sending chunk (%d,%d): %w", cx+dx, cz+dz, err)
			}
		}
	}
	return nil
}
