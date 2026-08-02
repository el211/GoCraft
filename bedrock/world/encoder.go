package world

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"

	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/protocol"

	coreworld "GoCraft/core/world"
)

// Encoder translates GoCraft's edition-neutral blocks into the vanilla
// Bedrock block-state registry shipped with Dragonfly. Runtime IDs come from
// the exact palette used by the active gophertunnel protocol revision.
type Encoder struct {
	mu        sync.RWMutex
	byName    map[string][]bedrockState
	cache     map[string]uint32
	airHash   uint32
	stoneHash uint32
}

type bedrockState struct {
	runtimeID uint32
	props     map[string]any
}

// NewEncoder prepares the vanilla Bedrock state lookup. Dragonfly embeds the
// block state palette for the same gophertunnel protocol family used here.
func NewEncoder() *Encoder {
	registry := dfworld.DefaultBlockRegistry
	registry.Finalize()

	e := &Encoder{
		byName: make(map[string][]bedrockState),
		cache:  make(map[string]uint32),
	}
	for rid := uint32(0); rid < uint32(registry.BlockCount()); rid++ {
		name, props, ok := registry.RuntimeIDToState(rid)
		if !ok {
			continue
		}
		e.byName[name] = append(e.byName[name], bedrockState{runtimeID: rid, props: props})
	}
	e.airHash = e.resolve(coreworld.Air)
	e.stoneHash = e.resolve(coreworld.Block{Namespace: "minecraft", Name: "stone"})
	return e
}

// BlockNetworkID returns the runtime ID from the palette advertised by StartGame.
func (e *Encoder) BlockNetworkID(block coreworld.Block) uint32 {
	key := block.Key()
	e.mu.RLock()
	if runtimeID, ok := e.cache[key]; ok {
		e.mu.RUnlock()
		return runtimeID
	}
	e.mu.RUnlock()

	runtimeID := e.resolve(block)
	e.mu.Lock()
	e.cache[key] = runtimeID
	e.mu.Unlock()
	return runtimeID
}

func (e *Encoder) resolve(block coreworld.Block) uint32 {
	name := block.ResourceLocation()
	if block.IsAir() || name == "" {
		name = "minecraft:air"
	}
	candidates := e.byName[name]
	if len(candidates) == 0 {
		for _, alternate := range alternateBlockNames(name) {
			if candidates = e.byName[alternate]; len(candidates) != 0 {
				break
			}
		}
	}
	if len(candidates) == 0 {
		// Unknown Java-only states are rendered as air rather than corrupting
		// the palette with a non-existent Bedrock runtime ID.
		if e.airHash != 0 {
			return e.airHash
		}
		return 0
	}

	wanted := translateProperties(block.Properties)
	best, bestScore := candidates[0], -1<<30
	for _, candidate := range candidates {
		score := stateScore(candidate.props, wanted)
		if score > bestScore {
			best, bestScore = candidate, score
		}
	}
	return best.runtimeID
}

func alternateBlockNames(name string) []string {
	if strings.HasSuffix(name, `_bed`) {
		return []string{`minecraft:bed`}
	}
	switch name {
	case `minecraft:oak_door`:
		return []string{`minecraft:wooden_door`}
	case "minecraft:lily_pad":
		return []string{"minecraft:waterlily"}
	case "minecraft:sugar_cane":
		return []string{"minecraft:reeds"}
	case "minecraft:cobweb":
		return []string{"minecraft:web"}
	case "minecraft:dirt_path":
		return []string{"minecraft:grass_path"}
	case "minecraft:short_grass":
		return []string{"minecraft:tallgrass"}
	}
	if strings.HasSuffix(name, "_wall_sign") {
		return []string{strings.TrimSuffix(name, "_wall_sign") + "_wall_sign"}
	}
	if strings.HasSuffix(name, "_sign") {
		return []string{strings.TrimSuffix(name, "_sign") + "_standing_sign"}
	}
	return nil
}

func translateProperties(properties map[string]string) map[string]any {
	out := make(map[string]any, len(properties)*2)
	for key, raw := range properties {
		value := propertyValue(raw)
		out[key] = value
		switch key {
		case "axis":
			out["pillar_axis"] = raw
		case "facing":
			out["minecraft:cardinal_direction"] = raw
			if direction, ok := cardinalDirection(raw); ok {
				out["direction"] = direction
			}
			if direction, ok := stairDirection(raw); ok {
				out["weirdo_direction"] = direction
			}
		case "part":
			out["head_piece_bit"] = boolByte(raw == "head")
		case "occupied":
			out["occupied_bit"] = boolByte(raw == "true")
		case "open":
			out["open_bit"] = boolByte(raw == "true")
		case "powered":
			out["powered_bit"] = boolByte(raw == "true")
		case "half":
			out["upper_block_bit"] = boolByte(raw == "upper")
			out["vertical_half"] = raw
		case "hinge":
			out["door_hinge_bit"] = boolByte(raw == "right")
		case "type":
			out["top_slot_bit"] = boolByte(raw == "top")
			if raw == "top" {
				out["vertical_half"] = "top"
			} else if raw == "bottom" {
				out["vertical_half"] = "bottom"
			}
		case "age":
			out["growth"] = intProperty(raw)
		case "moisture":
			out["moisturized_amount"] = intProperty(raw)
		case "level":
			out["liquid_depth"] = intProperty(raw)
		case "snowy":
			out["covered_bit"] = boolByte(raw == "true")
		case "persistent":
			out["persistent_bit"] = boolByte(raw == "true")
		}
	}
	return out
}

func propertyValue(raw string) any {
	if raw == "true" {
		return uint8(1)
	}
	if raw == "false" {
		return uint8(0)
	}
	if n, err := strconv.ParseInt(raw, 10, 32); err == nil {
		return int32(n)
	}
	return raw
}

func intProperty(raw string) int32 {
	n, _ := strconv.ParseInt(raw, 10, 32)
	return int32(n)
}

func boolByte(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}

func cardinalDirection(value string) (int32, bool) {
	switch value {
	case "south":
		return 0, true
	case "west":
		return 1, true
	case "north":
		return 2, true
	case "east":
		return 3, true
	default:
		return 0, false
	}
}

func stairDirection(value string) (int32, bool) {
	switch value {
	case "east":
		return 0, true
	case "west":
		return 1, true
	case "south":
		return 2, true
	case "north":
		return 3, true
	default:
		return 0, false
	}
}

func stateScore(candidate, wanted map[string]any) int {
	score := -len(candidate)
	for key, value := range wanted {
		candidateValue, ok := candidate[key]
		if !ok {
			continue
		}
		if fmt.Sprint(candidateValue) == fmt.Sprint(value) {
			score += 12
		} else {
			score -= 4
		}
	}
	return score
}

// EncodeSubChunk encodes one canonical section using Bedrock sub-chunk v9.
// subY is the absolute sub-chunk coordinate (-4..19 in the overworld).
func (e *Encoder) EncodeSubChunk(section *coreworld.Section, subY int32) ([]byte, error) {
	if section == nil || section.NonAir == 0 {
		return nil, nil
	}
	palette := section.BlockPalette()
	data := section.BlockData()
	bits := paletteBits(len(palette))

	var buf bytes.Buffer
	buf.WriteByte(9) // current Bedrock sub-chunk format
	buf.WriteByte(1) // one block storage layer
	buf.WriteByte(byte(int8(subY)))
	buf.WriteByte(bits<<1 | 1) // network palette

	if bits != 0 {
		valuesPerWord := 32 / int(bits)
		wordCount := (4096 + valuesPerWord - 1) / valuesPerWord
		for wordIndex := 0; wordIndex < wordCount; wordIndex++ {
			var word uint32
			for valueIndex := 0; valueIndex < valuesPerWord; valueIndex++ {
				bedrockIndex := wordIndex*valuesPerWord + valueIndex
				if bedrockIndex >= len(data) {
					break
				}
				// Bedrock stores blocks in X-Z-Y order, whereas the canonical
				// section uses Y-Z-X order for Java compatibility.
				x := bedrockIndex >> 8
				z := (bedrockIndex >> 4) & 15
				y := bedrockIndex & 15
				canonicalIndex := y*256 + z*16 + x
				word |= uint32(data[canonicalIndex]) << (valueIndex * int(bits))
			}
			writeUint32LE(&buf, word)
		}
		if err := protocol.WriteVarint32(&buf, int32(len(palette))); err != nil {
			return nil, err
		}
	}
	for _, block := range palette {
		if err := protocol.WriteVarint32(&buf, int32(e.BlockNetworkID(block))); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func paletteBits(entries int) byte {
	switch {
	case entries <= 1:
		return 0
	case entries <= 2:
		return 1
	case entries <= 4:
		return 2
	case entries <= 8:
		return 3
	case entries <= 16:
		return 4
	case entries <= 32:
		return 5
	case entries <= 64:
		return 6
	case entries <= 256:
		return 8
	default:
		return 16
	}
}

func writeUint32LE(buf *bytes.Buffer, value uint32) {
	buf.WriteByte(byte(value))
	buf.WriteByte(byte(value >> 8))
	buf.WriteByte(byte(value >> 16))
	buf.WriteByte(byte(value >> 24))
}
