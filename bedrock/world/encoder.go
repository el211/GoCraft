package world

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"

	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"

	coreworld "GoCraft/core/world"
)

// currentBlockVersion is stamped into every persistent palette NBT entry.
// This matches Dragonfly's CurrentBlockVersion constant (1.16.0.14 encoding).
// Bedrock clients apply blockupgrader.Upgrade when reading older state versions,
// so this value is safe to use regardless of which 1.x client connects.
const currentBlockVersion int32 = 18040335

// persistentBlockEntry is the NBT structure of one entry in a persistent sub-chunk palette.
// It uses the same field names as Dragonfly's blockEntry struct in server/world/chunk/encode.go.
type persistentBlockEntry struct {
	Name    string         `nbt:"name"`
	States  map[string]any `nbt:"states"`
	Version int32          `nbt:"version"`
}

// Encoder translates GoCraft's edition-neutral blocks into stable Bedrock
// network block hashes using the vanilla registry shipped with Dragonfly.
type Encoder struct {
	mu        sync.RWMutex
	byName    map[string][]bedrockState
	cache     map[string]uint32
	airHash   uint32
	stoneHash uint32
}

type bedrockState struct {
	name      string
	networkID uint32
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
		networkID, ok := registry.RuntimeIDToHash(rid)
		if !ok {
			continue
		}
		e.byName[name] = append(e.byName[name], bedrockState{
			name: name, networkID: networkID, props: props,
		})
	}
	e.airHash = e.resolve(coreworld.Air)
	e.stoneHash = e.resolve(coreworld.Block{Namespace: "minecraft", Name: "stone"})
	return e
}

// BlockNetworkID returns the stable network hash of a Bedrock block state.
// StartGame advertises UseBlockNetworkIDHashes, so every block ID sent to the
// client must use this value instead of a palette index. Hashes remain valid
// when the vanilla palette gains or reorders states between protocol versions.
func (e *Encoder) BlockNetworkID(block coreworld.Block) uint32 {
	key := block.Key()
	e.mu.RLock()
	if networkID, ok := e.cache[key]; ok {
		e.mu.RUnlock()
		return networkID
	}
	e.mu.RUnlock()

	networkID := e.resolve(block)
	e.mu.Lock()
	e.cache[key] = networkID
	e.mu.Unlock()
	return networkID
}

func (e *Encoder) resolve(block coreworld.Block) uint32 {
	block = bedrockVisualBlock(block)
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
		// the palette with a non-existent Bedrock network hash.
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
	return best.networkID
}

// resolveState returns the Bedrock block name and property map for a canonical block.
// Used when encoding persistent palette entries (NBT compounds) in SubChunk responses.
func (e *Encoder) resolveState(block coreworld.Block) (string, map[string]any) {
	block = bedrockVisualBlock(block)
	name := block.ResourceLocation()
	if block.IsAir() || name == "" {
		return "minecraft:air", map[string]any{}
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
		return "minecraft:air", map[string]any{}
	}

	wanted := translateProperties(block.Properties)
	best, bestScore := candidates[0], -1<<30
	for _, candidate := range candidates {
		score := stateScore(candidate.props, wanted)
		if score > bestScore {
			best, bestScore = candidate, score
		}
	}
	props := best.props
	if props == nil {
		props = map[string]any{}
	}
	return best.name, props
}

// bedrockVisualBlock applies edition-specific visual fallbacks without
// changing the canonical block stored in the world. Current Bedrock clients
// render Java's imported two-block tall_grass pair as two complete plants
// stacked on top of each other. A single short_grass at the lower position is
// the closest equivalent that renders consistently; the upper half is hidden.
func bedrockVisualBlock(block coreworld.Block) coreworld.Block {
	if block.ResourceLocation() != "minecraft:tall_grass" {
		return block
	}
	if block.Properties["half"] == "upper" {
		return coreworld.Air
	}
	return coreworld.Block{Namespace: "minecraft", Name: "short_grass"}
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

// EncodeSubChunk encodes one canonical section using Bedrock sub-chunk v9
// with a persistent (NBT) palette. Persistent palette entries contain the
// Bedrock block name and property map rather than version-specific runtime IDs,
// making the encoding valid regardless of which Bedrock protocol version the
// client uses. subY is the absolute sub-chunk coordinate (-4..19 in the overworld).
func (e *Encoder) EncodeSubChunk(section *coreworld.Section, subY int32) ([]byte, error) {
	if section == nil || section.NonAir == 0 {
		return nil, nil
	}
	palette := section.BlockPalette()
	data := section.BlockData()
	bits := paletteBits(len(palette))

	var buf bytes.Buffer
	buf.WriteByte(9) // sub-chunk version
	buf.WriteByte(1) // one block storage layer
	buf.WriteByte(byte(int8(subY)))
	buf.WriteByte(bits << 1) // persistent palette: flag bit 0 is clear

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
		// Persistent palette: count is LE uint32 (not varint).
		writeUint32LE(&buf, uint32(len(palette)))
	}

	// Write each palette entry as an NBT compound (LittleEndian encoding).
	// This matches Dragonfly's BlockPaletteEncoding.encode / diskEncoding path.
	for _, block := range palette {
		name, props := e.resolveState(block)
		entry := persistentBlockEntry{Name: name, States: props, Version: currentBlockVersion}
		entryBytes, err := nbt.MarshalEncoding(entry, nbt.LittleEndian)
		if err != nil {
			return nil, err
		}
		buf.Write(entryBytes)
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

// EncodeV9AirSubChunk returns a minimal version-9 persistent-palette sub-chunk
// containing only air.  subY is the absolute Bedrock sub-chunk Y coordinate
// (range −4 to 19 in the overworld).
//
// Format (matches EncodeSubChunk for bitsPerBlock=0):
//
//	byte(9)         sub-chunk version
//	byte(1)         one block storage layer
//	byte(subY)      absolute sub-chunk Y (signed)
//	byte(0)         bitsPerBlock=0 | persistent flag (bit0=0) → 0<<1 = 0x00
//	NBT compound    minecraft:air with currentBlockVersion
func EncodeV9AirSubChunk(subY int8) ([]byte, error) {
	entry := persistentBlockEntry{
		Name:    "minecraft:air",
		States:  map[string]any{},
		Version: currentBlockVersion,
	}
	entryBytes, err := nbt.MarshalEncoding(entry, nbt.LittleEndian)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte(9)          // version 9
	buf.WriteByte(1)          // 1 layer
	buf.WriteByte(byte(subY)) // sub-chunk Y (signed byte)
	buf.WriteByte(0)          // bitsPerBlock=0, persistent (bit0 clear)
	// bitsPerBlock=0: no word data, no palette count — just the single entry
	buf.Write(entryBytes)
	return buf.Bytes(), nil
}

// EncodeFullChunkPayload builds the complete RawPayload for a LevelChunk packet
// sent with SubChunkCount = SectionCount (24).  Block data is included directly
// so the client does not need to send SubChunkRequest packets.
//
// Sub-chunks use the V9 network format (flag bit0=1, network hashes as zigzag
// varints) — matching Dragonfly's NetworkEncoding for inline LevelChunk payloads.
//
// Payload layout:
//  1. 24 sub-chunk blobs (V9 network-hash palette), one per section
//  2. 24 × single-value plains biome storage (network palette)
//  3. Border block count varint (0x00)
func (e *Encoder) EncodeFullChunkPayload(chunk *coreworld.Chunk) ([]byte, error) {
	var buf bytes.Buffer
	for i := 0; i < coreworld.SectionCount; i++ {
		subY := int8(i - 4) // section 0 → subY=-4, section 4 → subY=0, etc.
		var sectionBytes []byte
		if chunk != nil && chunk.Sections[i] != nil && chunk.Sections[i].NonAir > 0 {
			sectionBytes = e.encodeNetworkSubChunk(chunk.Sections[i], subY)
		}
		if len(sectionBytes) == 0 {
			sectionBytes = e.encodeNetworkAirSubChunk(subY)
		}
		buf.Write(sectionBytes)
	}
	// Biome data: one single-value plains entry per sub-chunk.
	plains := makeSingleValueBiomeStorage(biomePlains)
	for range overworldSubChunkCount {
		buf.Write(plains)
	}
	buf.WriteByte(0x00) // border block count varint (0 = none)
	return buf.Bytes(), nil
}

// encodeNetworkSubChunk encodes one section using V9 network format (network hashes
// as zigzag varints, flag bit0=1).  This is the format Bedrock expects inside a
// LevelChunk RawPayload when SubChunkCount equals the actual section count.
func (e *Encoder) encodeNetworkSubChunk(section *coreworld.Section, subY int8) []byte {
	palette := section.BlockPalette()
	data := section.BlockData()
	bits := paletteBits(len(palette))

	var buf bytes.Buffer
	buf.WriteByte(9)           // sub-chunk version 9
	buf.WriteByte(1)           // one block storage layer
	buf.WriteByte(byte(subY))  // absolute sub-chunk Y (signed)
	buf.WriteByte(bits<<1 | 1) // network palette flag: bit0 = 1

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
				// Bedrock X-Z-Y vs canonical Y-Z-X
				x := bedrockIndex >> 8
				z := (bedrockIndex >> 4) & 15
				y := bedrockIndex & 15
				canonicalIndex := y*256 + z*16 + x
				word |= uint32(data[canonicalIndex]) << (valueIndex * int(bits))
			}
			writeUint32LE(&buf, word)
		}
		// Network palette: count as zigzag varint32
		_ = protocol.WriteVarint32(&buf, int32(len(palette)))
	}
	// Write each palette entry as its stable Bedrock network hash (zigzag varint32).
	for _, block := range palette {
		_ = protocol.WriteVarint32(&buf, int32(e.BlockNetworkID(block)))
	}
	return buf.Bytes()
}

// encodeNetworkAirSubChunk returns a minimal V9 network-format sub-chunk
// containing only air.  Used for nil/empty sections inside EncodeFullChunkPayload.
func (e *Encoder) encodeNetworkAirSubChunk(subY int8) []byte {
	var buf bytes.Buffer
	buf.WriteByte(9)          // version 9
	buf.WriteByte(1)          // 1 layer
	buf.WriteByte(byte(subY)) // sub-chunk Y
	buf.WriteByte(0x01)       // bitsPerBlock=0, network flag = 1
	// bitsPerBlock=0: no count, single entry as a network hash.
	_ = protocol.WriteVarint32(&buf, int32(e.airHash))
	return buf.Bytes()
}
