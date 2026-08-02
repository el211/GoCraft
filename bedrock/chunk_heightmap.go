package bedrock

import (
	coreworld "GoCraft/core/world"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// subChunkHeightMap builds the per-column heights Bedrock uses to calculate
// lighting for a requested sub-chunk.
func subChunkHeightMap(chunk *coreworld.Chunk, subY int32) (byte, []int8) {
	baseY := int(subY) * coreworld.SectionSize
	topY := baseY + coreworld.SectionSize - 1
	data := make([]int8, coreworld.SectionSize*coreworld.SectionSize)
	allAbove, allBelow := true, true

	for x := 0; x < coreworld.SectionSize; x++ {
		for z := 0; z < coreworld.SectionSize; z++ {
			height := chunk.HighestBlockY(x, z)
			index := z*coreworld.SectionSize + x
			switch {
			case height > topY:
				data[index] = coreworld.SectionSize
				allBelow = false
			case height < baseY:
				data[index] = -1
				allAbove = false
			default:
				data[index] = int8(height - baseY)
				allAbove = false
				allBelow = false
			}
		}
	}

	if allAbove {
		return protocol.HeightMapDataTooHigh, nil
	}
	if allBelow {
		return protocol.HeightMapDataTooLow, nil
	}
	return protocol.HeightMapDataHasData, data
}
