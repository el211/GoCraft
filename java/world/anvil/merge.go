package anvil

import (
	"bytes"
	"math/bits"
	"sort"

	coreworld "GoCraft/core/world"
)

// encodeChunkNBTWithBase updates canonical blocks/biomes while preserving all
// unknown NBT from a loaded Java chunk: structures, ticks, carving masks,
// blending data, heightmap variants, and block entities.
func encodeChunkNBTWithBase(c *coreworld.Chunk, base map[string]Tag) []byte {
	root := cloneCompound(base)
	root["DataVersion"] = Tag{typ: tagInt, intV: dataVersion}
	root["xPos"] = Tag{typ: tagInt, intV: c.X}
	root["yPos"] = Tag{typ: tagInt, intV: int32(sectionYBase)}
	root["zPos"] = Tag{typ: tagInt, intV: c.Z}
	root["Status"] = Tag{typ: tagString, strV: "minecraft:full"}

	byY := make(map[int8]Tag)
	for _, original := range root["sections"].List() {
		if original.typ == tagCompound {
			byY[original.Get("Y").Byte()] = cloneTag(original)
		}
	}
	for sectionIndex, section := range c.Sections {
		if section == nil {
			continue
		}
		sectionY := int8(sectionIndex + sectionYBase)
		entry, ok := byY[sectionY]
		if !ok || entry.typ != tagCompound {
			entry = Tag{typ: tagCompound, compound: make(map[string]Tag)}
		}
		entry.compound["Y"] = Tag{typ: tagByte, byteV: sectionY}
		entry.compound["block_states"] = blockStatesTag(section)
		entry.compound["biomes"] = biomesTag(section)
		byY[sectionY] = entry
	}
	sectionYs := make([]int, 0, len(byY))
	for sectionY := range byY {
		sectionYs = append(sectionYs, int(sectionY))
	}
	sort.Ints(sectionYs)
	sections := make([]Tag, 0, len(sectionYs))
	for _, sectionY := range sectionYs {
		sections = append(sections, byY[int8(sectionY)])
	}
	root["sections"] = Tag{typ: tagList, listElem: tagCompound, listV: sections}

	// Preserve exact Java heightmaps when block contents did not change. After a
	// block edit, update the two maps GoCraft consumes and retain every other map.
	originalChunk, decodeErr := chunkFromNBT(base, c.X, c.Z)
	if decodeErr != nil || originalChunk == nil || !chunksHaveSameBlocks(c, originalChunk) {
		heightmaps := root["Heightmaps"]
		if heightmaps.typ != tagCompound {
			heightmaps = Tag{typ: tagCompound, compound: make(map[string]Tag)}
		}
		packed := packChunkHeightmap(c)
		heightmaps.compound["WORLD_SURFACE"] = Tag{typ: tagLongArr, longsV: packed}
		heightmaps.compound["MOTION_BLOCKING"] = Tag{typ: tagLongArr, longsV: append([]int64(nil), packed...)}
		root["Heightmaps"] = heightmaps
	}

	// Block entities are reconstructed from their canonical position, type, and
	// opaque payload so edits and removals are reflected without losing NBT.
	root["block_entities"] = blockEntitiesTag(c.BlockEntities)

	var buffer bytes.Buffer
	WriteRootCompound(&buffer, root)
	return buffer.Bytes()
}

func blockStatesTag(section *coreworld.Section) Tag {
	palette := section.BlockPalette()
	paletteTags := make([]Tag, len(palette))
	for i, material := range palette {
		entry := map[string]Tag{"Name": {typ: tagString, strV: material.ResourceLocation()}}
		if len(material.Properties) > 0 {
			properties := make(map[string]Tag, len(material.Properties))
			for key, value := range material.Properties {
				properties[key] = Tag{typ: tagString, strV: value}
			}
			entry["Properties"] = Tag{typ: tagCompound, compound: properties}
		}
		paletteTags[i] = Tag{typ: tagCompound, compound: entry}
	}
	compound := map[string]Tag{
		"palette": {typ: tagList, listElem: tagCompound, listV: paletteTags},
	}
	if len(palette) > 1 {
		bitsPerEntry := max(4, bits.Len(uint(len(palette)-1)))
		entriesPerLong := 64 / bitsPerEntry
		data := section.BlockData()
		longs := make([]int64, (4096+entriesPerLong-1)/entriesPerLong)
		for index, value := range data {
			longs[index/entriesPerLong] |= int64(value) << ((index % entriesPerLong) * bitsPerEntry)
		}
		compound["data"] = Tag{typ: tagLongArr, longsV: longs}
	}
	return Tag{typ: tagCompound, compound: compound}
}

func biomesTag(section *coreworld.Section) Tag {
	palette := section.BiomePalette()
	paletteTags := make([]Tag, len(palette))
	for i, biome := range palette {
		paletteTags[i] = Tag{typ: tagString, strV: biome}
	}
	compound := map[string]Tag{
		"palette": {typ: tagList, listElem: tagString, listV: paletteTags},
	}
	if len(palette) > 1 {
		bitsPerEntry := max(1, bits.Len(uint(len(palette)-1)))
		entriesPerLong := 64 / bitsPerEntry
		data := section.BiomeData()
		longs := make([]int64, (64+entriesPerLong-1)/entriesPerLong)
		for index, value := range data {
			longs[index/entriesPerLong] |= int64(value) << ((index % entriesPerLong) * bitsPerEntry)
		}
		compound["data"] = Tag{typ: tagLongArr, longsV: longs}
	}
	return Tag{typ: tagCompound, compound: compound}
}

func packChunkHeightmap(c *coreworld.Chunk) []int64 {
	const bitsPerEntry = 9
	const entriesPerLong = 64 / bitsPerEntry
	longs := make([]int64, (256+entriesPerLong-1)/entriesPerLong)
	for z := 0; z < 16; z++ {
		for x := 0; x < 16; x++ {
			index := z*16 + x
			value := int64(c.HighestBlockY(x, z)+1) - coreworld.WorldMinY
			if value < 0 {
				value = 0
			}
			longs[index/entriesPerLong] |= value << ((index % entriesPerLong) * bitsPerEntry)
		}
	}
	return longs
}

func chunkBlockAt(c *coreworld.Chunk, x, y, z int) coreworld.Block {
	if y < coreworld.WorldMinY || y > coreworld.WorldMaxY {
		return coreworld.Air
	}
	section := c.Sections[(y-coreworld.WorldMinY)/16]
	if section == nil {
		return coreworld.Air
	}
	return section.At(x, (y-coreworld.WorldMinY)%16, z)
}
func blockEntitiesTag(entities []coreworld.BlockEntity) Tag {
	entries := make([]Tag, 0, len(entities))
	for _, entity := range entities {
		if entity.Type == "" || len(entity.Data) < 2 || tagType(entity.Data[0]) != tagCompound {
			continue
		}
		reader := bytes.NewReader(entity.Data[1:])
		payload, err := readPayload(reader, tagCompound)
		if err != nil || reader.Len() != 0 || payload.typ != tagCompound {
			continue
		}
		compound := cloneCompound(payload.compound)
		compound["id"] = Tag{typ: tagString, strV: entity.Type}
		compound["x"] = Tag{typ: tagInt, intV: int32(entity.X)}
		compound["y"] = Tag{typ: tagInt, intV: int32(entity.Y)}
		compound["z"] = Tag{typ: tagInt, intV: int32(entity.Z)}
		if entity.Items != nil {
			compound["Items"] = containerItemsTag(entity.Items)
		}
		entries = append(entries, Tag{typ: tagCompound, compound: compound})
	}
	return Tag{typ: tagList, listElem: tagCompound, listV: entries}
}
func containerItemsTag(items []coreworld.ContainerItem) Tag {
	entries := make([]Tag, 0, len(items))
	for _, item := range items {
		if item.Slot < 0 || item.Slot > 255 || item.ItemID == "" || item.Count <= 0 {
			continue
		}
		entries = append(entries, Tag{typ: tagCompound, compound: map[string]Tag{
			"Slot":  {typ: tagByte, byteV: int8(item.Slot)},
			"id":    {typ: tagString, strV: item.ItemID},
			"count": {typ: tagInt, intV: int32(item.Count)},
		}})
	}
	return Tag{typ: tagList, listElem: tagCompound, listV: entries}
}

func chunksHaveSameBlocks(first, second *coreworld.Chunk) bool {
	for sectionIndex := 0; sectionIndex < coreworld.SectionCount; sectionIndex++ {
		firstSection, secondSection := first.Sections[sectionIndex], second.Sections[sectionIndex]
		for y := 0; y < coreworld.SectionSize; y++ {
			for z := 0; z < coreworld.SectionSize; z++ {
				for x := 0; x < coreworld.SectionSize; x++ {
					firstBlock, secondBlock := coreworld.Air, coreworld.Air
					if firstSection != nil {
						firstBlock = firstSection.At(x, y, z)
					}
					if secondSection != nil {
						secondBlock = secondSection.At(x, y, z)
					}
					if !firstBlock.Equal(secondBlock) {
						return false
					}
				}
			}
		}
	}
	return true
}
