package world

const (
	// SectionSize is the number of blocks along each axis of a chunk section.
	SectionSize = 16

	// WorldMinY is the minimum world Y coordinate in 1.18+ (post height expansion).
	WorldMinY = -64
	// WorldMaxY is the maximum world Y coordinate (inclusive) in 1.18+.
	WorldMaxY = 319

	// SectionCount is the number of 16-block-tall sections per chunk column.
	// (WorldMaxY - WorldMinY + 1) / SectionSize = 384 / 16 = 24
	SectionCount = (WorldMaxY - WorldMinY + 1) / SectionSize
)

// Chunk is the canonical, edition-agnostic chunk: a 16×(24×16)×16 column of blocks.
//
// The Java adapter (java/world) converts this to the Level Chunk With Light packet.
// A future Bedrock adapter will convert it to a Sub Chunk packet independently.
// Neither the field types nor the method signatures must reference Java or Bedrock.
type Chunk struct {
	// X and Z are chunk coordinates (block position divided by 16).
	X, Z int32
	// Sections holds one Section per 16-block-tall slice, bottom first.
	// A nil entry represents an all-air section (saves memory for empty columns).
	Sections [SectionCount]*Section

	// BlockEntities contains position, type, and opaque NBT payload for blocks
	// such as chests, furnaces, signs, and spawners. Edition adapters may ignore
	// payloads they do not understand while preserving them on disk.
	BlockEntities []BlockEntity
}

// BlockEntity is a canonical positioned block entity. Data contains an
// anonymous network-NBT compound when loaded from Java Anvil storage.
type BlockEntity struct {
	X, Y, Z int
	Type    string
	Data    []byte
	Items   []ContainerItem
}

// ContainerItem is an edition-independent item stored in a block container.
// Slot is the zero-based index inside the container (0-26 for a single chest).
type ContainerItem struct {
	Slot   int
	ItemID string
	Count  int
}

// HighestBlockY returns the highest non-air block in the local x/z column.
// WorldMinY-1 is returned for a completely empty column.
func (c *Chunk) HighestBlockY(x, z int) int {
	if x < 0 || x >= SectionSize || z < 0 || z >= SectionSize {
		return WorldMinY - 1
	}
	for sectionIndex := SectionCount - 1; sectionIndex >= 0; sectionIndex-- {
		section := c.Sections[sectionIndex]
		if section == nil || section.NonAir == 0 {
			continue
		}
		for localY := SectionSize - 1; localY >= 0; localY-- {
			if !section.At(x, localY, z).IsAir() {
				return SectionMinY(sectionIndex) + localY
			}
		}
	}
	return WorldMinY - 1
}

// SectionMinY returns the absolute minimum Y coordinate of section index i.
func SectionMinY(i int) int { return WorldMinY + i*SectionSize }

// Section is one 16×16×16 cube of blocks within a chunk.
//
// Internally it uses a palette so that only distinct Block values that actually
// appear are stored; every grid position holds a compact palette index.
// Index 0 in the palette is always the Air block.
//
// Palette deduplication uses Block.Key() as the map key, which sorts properties
// alphabetically so two Block values with the same properties in different
// insertion order are guaranteed to map to the same palette entry.
//
// The Java encoder (java/world/encoder.go) reads BlockPalette() and BlockData()
// to build the PalettedContainer wire format.  It maps each canonical Block to a
// Java global state ID via its own registry — the core never touches Java IDs.
// A future Bedrock encoder will do the same with Bedrock runtime IDs.
type Section struct {
	blockPalette []Block           // canonical Block values; [0] = Air
	paletteIndex map[string]uint16 // Block.Key() → palette index, for O(1) dedup
	blockData    [4096]uint16      // palette index per position: y*256 + z*16 + x
	NonAir       int16             // count of non-air blocks (required for Java wire format)

	// Biome is retained as the representative/first biome for compatibility with
	// older adapters. The palette below stores the real 4x4x4 biome cells used by
	// modern Java chunks.
	Biome        string
	biomePalette []string
	biomeIndex   map[string]uint16
	biomeData    [64]uint16
}

// NewSection returns an all-air section with a single-entry palette containing Air.
func NewSection() *Section {
	airKey := Air.Key()
	return &Section{
		blockPalette: []Block{Air},
		paletteIndex: map[string]uint16{airKey: 0},
		Biome:        "minecraft:plains",
		biomePalette: []string{"minecraft:plains"},
		biomeIndex:   map[string]uint16{"minecraft:plains": 0},
	}
}

// At returns the canonical Block at section-local coordinates (0–15 each).
// Returns Air for nil sections.
func (s *Section) At(x, y, z int) Block {
	if s == nil || len(s.blockPalette) == 0 {
		return Air
	}
	return s.blockPalette[s.blockData[y*256+z*16+x]]
}

// Set places a block at section-local coordinates.
// Uses Block.Key() for deduplication so blocks with identical properties are
// always treated as the same palette entry regardless of map insertion order.
func (s *Section) Set(x, y, z int, block Block) {
	gridIdx := y*256 + z*16 + x
	key := block.Key()

	if i, ok := s.paletteIndex[key]; ok {
		old := s.blockData[gridIdx]
		s.blockData[gridIdx] = i
		s.updateNonAir(old, i)
		return
	}

	// New block value — append to palette and index.
	newIdx := uint16(len(s.blockPalette))
	s.blockPalette = append(s.blockPalette, block)
	s.paletteIndex[key] = newIdx
	old := s.blockData[gridIdx]
	s.blockData[gridIdx] = newIdx
	s.updateNonAir(old, newIdx)
}

// updateNonAir adjusts the NonAir counter after a palette index change.
// Palette index 0 is always air.
func (s *Section) updateNonAir(old, new uint16) {
	wasAir := old == 0
	isAir := new == 0
	if wasAir && !isAir {
		s.NonAir++
	} else if !wasAir && isAir {
		s.NonAir--
	}
}

// BlockPalette returns the canonical Block palette for this section.
// The Java encoder maps each Block to a Java global state ID via its own registry.
// A future Bedrock encoder will map to Bedrock runtime IDs independently.
func (s *Section) BlockPalette() []Block { return s.blockPalette }

// BlockData returns the raw palette-index array (position: y*256+z*16+x).
// Each value is an index into the slice returned by BlockPalette.
func (s *Section) BlockData() [4096]uint16 { return s.blockData }

// SetUniformBiome fills all 4x4x4 biome cells with one resource location.
func (s *Section) SetUniformBiome(biome string) {
	if biome == "" {
		biome = "minecraft:plains"
	}
	s.Biome = biome
	s.biomePalette = []string{biome}
	s.biomeIndex = map[string]uint16{biome: 0}
	s.biomeData = [64]uint16{}
}

// SetBiomeCell sets one Java biome quart-cell (coordinates 0-3 on each axis).
func (s *Section) SetBiomeCell(x, y, z int, biome string) {
	if x < 0 || x >= 4 || y < 0 || y >= 4 || z < 0 || z >= 4 {
		return
	}
	if biome == "" {
		biome = "minecraft:plains"
	}
	if s.biomeIndex == nil {
		s.SetUniformBiome(s.Biome)
	}
	index, ok := s.biomeIndex[biome]
	if !ok {
		index = uint16(len(s.biomePalette))
		s.biomePalette = append(s.biomePalette, biome)
		s.biomeIndex[biome] = index
	}
	s.biomeData[y*16+z*4+x] = index
	if len(s.biomePalette) > 0 {
		s.Biome = s.biomePalette[0]
	}
}

// BiomeAtCell returns a biome quart-cell, defaulting to plains.
func (s *Section) BiomeAtCell(x, y, z int) string {
	if s == nil || x < 0 || x >= 4 || y < 0 || y >= 4 || z < 0 || z >= 4 || len(s.biomePalette) == 0 {
		return "minecraft:plains"
	}
	index := int(s.biomeData[y*16+z*4+x])
	if index < 0 || index >= len(s.biomePalette) {
		return s.biomePalette[0]
	}
	return s.biomePalette[index]
}

// BiomePalette and BiomeData expose a read-only snapshot to edition adapters.
func (s *Section) BiomePalette() []string {
	if s == nil || len(s.biomePalette) == 0 {
		return []string{"minecraft:plains"}
	}
	return s.biomePalette
}

func (s *Section) BiomeData() [64]uint16 {
	if s == nil {
		return [64]uint16{}
	}
	return s.biomeData
}
