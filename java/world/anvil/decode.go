package anvil

import (
	"bytes"
	"fmt"
	"math/bits"
	"strings"

	coreworld "GoCraft/core/world"
)

// sectionYBase is the section Y offset such that sectionY = sIdx + sectionYBase.
// For WorldMinY = -64: sectionYBase = WorldMinY / SectionSize = -64/16 = -4.
const sectionYBase = coreworld.WorldMinY / coreworld.SectionSize

// chunkFromNBT converts a parsed Anvil chunk root compound into a canonical
// core/world.Chunk.
//
// Returns (nil, nil) when the chunk has not finished generating (Status is not
// "minecraft:full"), which the World layer treats as "not present" and falls
// back to the generator.
func chunkFromNBT(root map[string]Tag, cx, cz int32) (*coreworld.Chunk, error) {
	status := root["Status"].Str()
	// Accept both "minecraft:full" (1.18+) and the legacy bare "full".
	if status != "minecraft:full" && status != "full" {
		return nil, nil
	}

	chunk := &coreworld.Chunk{X: cx, Z: cz}

	sectionsTag := root["sections"]
	if sectionsTag.typ != tagList {
		// 1.17 and earlier stored sections under Level.Sections — not supported.
		return nil, fmt.Errorf("anvil: no 'sections' TAG_List in chunk (%d,%d)", cx, cz)
	}

	for _, secTag := range sectionsTag.List() {
		if secTag.typ != tagCompound {
			continue
		}
		c := secTag.compound

		// Y is TAG_Byte holding the section coordinate (not an absolute Y).
		sY := int(c["Y"].Byte())
		sIdx := sY - sectionYBase // convert section Y → array index
		if sIdx < 0 || sIdx >= coreworld.SectionCount {
			continue
		}

		bsTag := c["block_states"]
		if bsTag.typ != tagCompound {
			continue // section has no blocks (e.g. light-only section)
		}
		bs := bsTag.compound

		paletteTag := bs["palette"]
		if paletteTag.typ != tagList || len(paletteTag.listV) == 0 {
			continue
		}

		// Decode palette entries into canonical Blocks.
		palBlocks, err := decodePalette(paletteTag.listV)
		if err != nil {
			return nil, fmt.Errorf("anvil: chunk (%d,%d) section %d: %w", cx, cz, sIdx, err)
		}

		section := coreworld.NewSection()

		if len(palBlocks) == 1 {
			// Single-entry palette — every block in the section is this type.
			fillSection(section, palBlocks[0])
		} else {
			dataTag := bs["data"]
			if dataTag.typ != tagLongArr || len(dataTag.longsV) == 0 {
				// No data array despite multi-entry palette — treat as air.
				chunk.Sections[sIdx] = section
				continue
			}
			if err := decodeBlockData(section, palBlocks, dataTag.longsV); err != nil {
				return nil, fmt.Errorf("anvil: chunk (%d,%d) section %d: %w", cx, cz, sIdx, err)
			}
		}

		if err := decodeBiomeData(section, c["biomes"]); err != nil {
			return nil, fmt.Errorf("anvil: chunk (%d,%d) section %d biomes: %w", cx, cz, sIdx, err)
		}
		chunk.Sections[sIdx] = section
	}

	chunk.BlockEntities = decodeBlockEntities(root["block_entities"])
	return chunk, nil
}

func decodeBlockEntities(list Tag) []coreworld.BlockEntity {
	if list.typ != tagList {
		return nil
	}
	entities := make([]coreworld.BlockEntity, 0, len(list.listV))
	for _, entry := range list.listV {
		if entry.typ != tagCompound {
			continue
		}
		data := cloneCompound(entry.compound)
		entityType := data["id"].Str()
		x, y, z := int(data["x"].Int()), int(data["y"].Int()), int(data["z"].Int())
		items := decodeContainerItems(data["Items"])
		delete(data, "Items")
		delete(data, "id")
		delete(data, "x")
		delete(data, "y")
		delete(data, "z")
		var payload bytes.Buffer
		wByte(&payload, byte(tagCompound))
		writeCompoundPayload(&payload, data)
		entities = append(entities, coreworld.BlockEntity{X: x, Y: y, Z: z, Type: entityType, Data: payload.Bytes(), Items: items})
	}
	return entities
}

func decodeContainerItems(list Tag) []coreworld.ContainerItem {
	if list.typ != tagList {
		return nil
	}
	items := make([]coreworld.ContainerItem, 0, len(list.listV))
	for _, entry := range list.listV {
		if entry.typ != tagCompound {
			continue
		}
		slotTag := entry.compound["Slot"]
		slot := int(slotTag.Byte())
		if slotTag.typ == tagShort {
			slot = int(slotTag.Short())
		} else if slotTag.typ == tagInt {
			slot = int(slotTag.Int())
		}
		itemID := entry.compound["id"].Str()
		count := numericTagValue(entry.compound["count"])
		if count == 0 {
			count = numericTagValue(entry.compound["Count"])
		}
		if slot < 0 || itemID == "" || count <= 0 {
			continue
		}
		items = append(items, coreworld.ContainerItem{Slot: slot, ItemID: itemID, Count: count})
	}
	return items
}

func numericTagValue(tag Tag) int {
	switch tag.typ {
	case tagByte:
		return int(tag.Byte())
	case tagShort:
		return int(tag.Short())
	case tagInt:
		return int(tag.Int())
	default:
		return 0
	}
}

// decodeBiomeData reads the modern 4x4x4 biome paletted container stored in
// each section. Anvil uses compact, non-crossing longs just like block states,
// but biome palettes have a one-bit minimum and exactly 64 entries.
func decodeBiomeData(section *coreworld.Section, biomesTag Tag) error {
	if biomesTag.typ != tagCompound {
		return nil
	}
	paletteTag := biomesTag.compound["palette"]
	if paletteTag.typ != tagList || len(paletteTag.listV) == 0 {
		return nil
	}
	palette := make([]string, len(paletteTag.listV))
	for i, entry := range paletteTag.listV {
		if entry.typ != tagString || entry.Str() == "" {
			return fmt.Errorf("palette entry %d is not a biome string", i)
		}
		palette[i] = entry.Str()
	}
	section.SetUniformBiome(palette[0])
	if len(palette) == 1 {
		return nil
	}
	dataTag := biomesTag.compound["data"]
	if dataTag.typ != tagLongArr || len(dataTag.longsV) == 0 {
		return fmt.Errorf("multi-entry palette has no data")
	}
	bitsPerEntry := max(1, bits.Len(uint(len(palette)-1)))
	entriesPerLong := 64 / bitsPerEntry
	mask := int64((1 << bitsPerEntry) - 1)
	for cell := 0; cell < 64; cell++ {
		longIndex := cell / entriesPerLong
		if longIndex >= len(dataTag.longsV) {
			return fmt.Errorf("data ended at cell %d", cell)
		}
		bitOffset := (cell % entriesPerLong) * bitsPerEntry
		paletteIndex := int((dataTag.longsV[longIndex] >> bitOffset) & mask)
		if paletteIndex < 0 || paletteIndex >= len(palette) {
			return fmt.Errorf("palette index %d out of range %d", paletteIndex, len(palette))
		}
		x, z, y := cell&3, (cell>>2)&3, cell>>4
		section.SetBiomeCell(x, y, z, palette[paletteIndex])
	}
	return nil
}

// decodePalette converts a TAG_List of TAG_Compound block-state descriptors
// into a slice of canonical Blocks.
func decodePalette(list []Tag) ([]coreworld.Block, error) {
	blocks := make([]coreworld.Block, len(list))
	for i, entry := range list {
		if entry.typ != tagCompound {
			return nil, fmt.Errorf("palette entry %d is not TAG_Compound", i)
		}
		nameTag := entry.compound["Name"]
		if nameTag.typ == tagEnd {
			return nil, fmt.Errorf("palette entry %d missing 'Name'", i)
		}
		fullName := nameTag.Str() // e.g. "minecraft:stone"

		ns, name, found := strings.Cut(fullName, ":")
		if !found {
			// No colon — treat the whole string as the name under "minecraft".
			ns, name = "minecraft", fullName
		}

		var props map[string]string
		propsTag := entry.compound["Properties"]
		if propsTag.typ == tagCompound && len(propsTag.compound) > 0 {
			props = make(map[string]string, len(propsTag.compound))
			for k, v := range propsTag.compound {
				props[k] = v.Str()
			}
		}

		blocks[i] = coreworld.Block{
			Namespace:  ns,
			Name:       name,
			Properties: props,
		}
	}
	return blocks, nil
}

// fillSection sets every block in section to the given block.
// Only called for single-entry palettes; skips air to avoid needless work.
func fillSection(section *coreworld.Section, block coreworld.Block) {
	if block.IsAir() {
		return // section starts all-air; nothing to do
	}
	for y := 0; y < coreworld.SectionSize; y++ {
		for z := 0; z < coreworld.SectionSize; z++ {
			for x := 0; x < coreworld.SectionSize; x++ {
				section.Set(x, y, z, block)
			}
		}
	}
}

// decodeBlockData unpacks the Anvil 1.16+ packed long array into section blocks.
//
// Packing format (post-1.16): each long holds floor(64/bitsPerEntry) entries;
// entries do NOT span across longs.
//
// blockIdx = y*256 + z*16 + x  (same order as Section.blockData)
func decodeBlockData(section *coreworld.Section, palette []coreworld.Block, data []int64) error {
	bitsPerEntry := max(4, bits.Len(uint(len(palette)-1)))
	entriesPerLong := 64 / bitsPerEntry
	mask := int64((1 << bitsPerEntry) - 1)

	for blockIdx := 0; blockIdx < 4096; blockIdx++ {
		longIdx := blockIdx / entriesPerLong
		bitOff := (blockIdx % entriesPerLong) * bitsPerEntry
		if longIdx >= len(data) {
			break
		}
		paletteIdx := int((data[longIdx] >> bitOff) & mask)
		if paletteIdx >= len(palette) {
			return fmt.Errorf("palette index %d out of range (palette size %d)", paletteIdx, len(palette))
		}
		blk := palette[paletteIdx]
		if blk.IsAir() {
			continue // section defaults to air; skip for speed
		}
		x := blockIdx & 15
		z := (blockIdx >> 4) & 15
		y := blockIdx >> 8
		section.Set(x, y, z, blk)
	}
	return nil
}

// max returns the larger of a and b.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
