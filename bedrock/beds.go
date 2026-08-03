package bedrock

import (
	"bytes"
	"strings"

	coreworld "GoCraft/core/world"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

type bedBlockActor struct {
	X, Y, Z int32
	Colour  uint8
}

// encodeBedBlockActors produces the consecutive Network Little Endian NBT
// compounds that Bedrock requires at the end of a successful SubChunk entry.
// Sending BlockActorData later is insufficient for the initial chunk load:
// the client creates the bed renderer while decoding this inline payload.
func encodeBedBlockActors(chunk *coreworld.Chunk, subY int32) ([]byte, error) {
	var payload bytes.Buffer
	encoder := nbt.NewEncoderWithEncoding(&payload, nbt.NetworkLittleEndian)
	for _, actor := range bedBlockActors(chunk, subY) {
		if err := encoder.Encode(map[string]any{
			"id": "Bed", "x": actor.X, "y": actor.Y, "z": actor.Z,
			"color": actor.Colour,
		}); err != nil {
			return nil, err
		}
	}
	return payload.Bytes(), nil
}

// bedBlockActors derives Bedrock block actors from canonical Java-style bed
// states. Both halves receive data, matching generated and imported worlds.
func bedBlockActors(chunk *coreworld.Chunk, subY int32) []bedBlockActor {
	if chunk == nil {
		return nil
	}
	sectionIndex := int(subY) - coreworld.WorldMinY/coreworld.SectionSize
	if sectionIndex < 0 || sectionIndex >= coreworld.SectionCount {
		return nil
	}
	section := chunk.Sections[sectionIndex]
	if section == nil || section.NonAir == 0 {
		return nil
	}
	actors := make([]bedBlockActor, 0, 4)
	for y := 0; y < coreworld.SectionSize; y++ {
		for z := 0; z < coreworld.SectionSize; z++ {
			for x := 0; x < coreworld.SectionSize; x++ {
				block := section.At(x, y, z)
				if !strings.HasSuffix(block.ResourceLocation(), "_bed") {
					continue
				}
				actors = append(actors, bedBlockActor{
					X:      int32(int(chunk.X)*coreworld.SectionSize + x),
					Y:      subY*coreworld.SectionSize + int32(y),
					Z:      int32(int(chunk.Z)*coreworld.SectionSize + z),
					Colour: bedrockBedColour(block.Name),
				})
			}
		}
	}
	return actors
}

func bedrockBedColour(name string) uint8 {
	colours := map[string]uint8{
		"white": 0, "orange": 1, "magenta": 2, "light_blue": 3,
		"yellow": 4, "lime": 5, "pink": 6, "gray": 7,
		"light_gray": 8, "cyan": 9, "purple": 10, "blue": 11,
		"brown": 12, "green": 13, "red": 14, "black": 15,
	}
	return colours[strings.TrimSuffix(name, "_bed")]
}
