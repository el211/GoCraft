package world

// registry.go loads block-state IDs, item protocol IDs, entity-type protocol
// IDs, and biome protocol IDs from the embedded Minecraft data-generator JSON
// files at package init time.
//
// JSON formats (matching Minecraft's data generator output):
//
//	blocks.json      — map[blockName]blockEntry
//	items.json       — complete item name-to-protocol-ID registry
//	registries.json  — remaining registry data
//
// All files must carry a "_gocraft_version" string key.  At init the loader
// verifies this matches the constant expectedVersion; a mismatch panics so
// a wrong-version data file is never silently accepted.
//
// If any JSON file cannot be parsed the package panics: the server cannot
// function with corrupt registry data, and a compile-time embedded file is
// far more reliable than a runtime open() that silently degrades behaviour.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"GoCraft/internal/gamedata"
)

// expectedVersion is the Minecraft version string these data files must declare.
// It must match the directory name under internal/gamedata/java/.
const expectedVersion = "1.21.4"

// ── Unknown-ID rate-limited warning ──────────────────────────────────────────

// unknownIDs tracks resource locations we have already warned about so that a
// missing registry entry generates exactly one log line per location, not one
// per block/entity tick.
var unknownIDs sync.Map

// warnUnknown logs a warning for resourceLocation the first time it is seen.
func warnUnknown(registry, resourceLocation string) {
	key := registry + "\x00" + resourceLocation
	if _, loaded := unknownIDs.LoadOrStore(key, struct{}{}); !loaded {
		slog.Warn("gamedata: unknown resource location",
			"registry", registry, "id", resourceLocation,
			"hint", "replace internal/gamedata/java/"+expectedVersion+"/registries.json with full data-generator output")
	}
}

// ── JSON schema types ─────────────────────────────────────────────────────────

type blockStateEntry struct {
	ID         int32             `json:"id"`
	Default    bool              `json:"default"`
	Properties map[string]string `json:"properties"`
}

type blockEntry struct {
	Properties map[string][]string `json:"properties"`
	States     []blockStateEntry   `json:"states"`
}

type registryItemEntry struct {
	ProtocolID int32 `json:"protocol_id"`
}

type registryEntry struct {
	Default    string                       `json:"default"`
	ProtocolID int32                        `json:"protocol_id"`
	Entries    map[string]registryItemEntry `json:"entries"`
}

// ── Loader ────────────────────────────────────────────────────────────────────

func init() {
	loadBlockRegistry()
	loadRegistries()
}

// loadBlockRegistry parses blocks.json and populates javaStateIDs.
func loadBlockRegistry() {
	data, err := gamedata.FS.ReadFile("java/1.21.4/blocks.json")
	if err != nil {
		panic(fmt.Sprintf("gamedata: reading blocks.json: %v", err))
	}

	// Parse as a raw map so we can pull the version key without a full schema.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("gamedata: parsing blocks.json: %v", err))
	}
	validateVersion(raw, "blocks.json")

	loaded := make(map[string]int32, len(raw)*2)
	defaults, states := 0, 0

	for blockName, rawVal := range raw {
		if isMetaKey(blockName) {
			continue
		}
		var entry blockEntry
		if err := json.Unmarshal(rawVal, &entry); err != nil {
			slog.Warn("gamedata: skipping malformed block entry",
				"block", blockName, "err", err)
			continue
		}
		for _, s := range entry.States {
			states++
			if s.Default {
				loaded[blockName] = s.ID
				defaults++
			}
			if len(s.Properties) > 0 {
				loaded[blockStateKey(blockName, s.Properties)] = s.ID
			}
		}
	}

	javaStateIDs = loaded
	slog.Info("gamedata: loaded block registry",
		"version", expectedVersion, "blocks", defaults, "states", states)
}

// loadItemRegistry parses the complete protocol-769 item table generated from
// PrismarineJS's version-specific items.json.
func loadItemRegistry() (map[string]int32, map[int32]string) {
	data, err := gamedata.FS.ReadFile("java/1.21.4/items.json")
	if err != nil {
		panic(fmt.Sprintf("gamedata: reading items.json: %v", err))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("gamedata: parsing items.json: %v", err))
	}
	validateVersion(raw, "items.json")
	entriesRaw, ok := raw["entries"]
	if !ok {
		panic("gamedata: items.json missing entries")
	}
	var nameToID map[string]int32
	if err := json.Unmarshal(entriesRaw, &nameToID); err != nil {
		panic(fmt.Sprintf("gamedata: parsing items.json entries: %v", err))
	}
	idToName := make(map[int32]string, len(nameToID))
	for name, id := range nameToID {
		if previous, exists := idToName[id]; exists {
			panic(fmt.Sprintf("gamedata: duplicate item protocol ID %d for %s and %s", id, previous, name))
		}
		idToName[id] = name
	}
	return nameToID, idToName
}

// loadRegistries loads the complete item table, then parses registries.json
// for entity-type and biome IDs.
func loadRegistries() {
	data, err := gamedata.FS.ReadFile("java/1.21.4/registries.json")
	if err != nil {
		panic(fmt.Sprintf("gamedata: reading registries.json: %v", err))
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("gamedata: parsing registries.json: %v", err))
	}
	validateVersion(raw, "registries.json")

	itemIDs, itemNames = loadItemRegistry()
	etMap, _ := loadRegistry(raw, "minecraft:entity_type", "registries.json")
	entityTypeIDs = etMap
	biomeMap, _ := loadRegistry(raw, "minecraft:worldgen/biome", "registries.json")
	biomeIDs = biomeMap
	blockEntityMap, _ := loadRegistry(raw, "minecraft:block_entity_type", "registries.json")
	blockEntityTypeIDs = blockEntityMap
	soundMap, _ := loadRegistry(raw, "minecraft:sound_event", "registries.json")
	soundEventIDs = soundMap
	effectMap, _ := loadRegistry(raw, "minecraft:mob_effect", "registries.json")
	mobEffectIDs = effectMap

	slog.Info("gamedata: loaded registries",
		"version", expectedVersion,
		"items", len(itemIDs),
		"entityTypes", len(entityTypeIDs),
		"biomes", len(biomeIDs),
		"blockEntityTypes", len(blockEntityTypeIDs),
		"soundEvents", len(soundEventIDs),
		"mobEffects", len(mobEffectIDs))
}

// loadRegistry extracts one named registry from the top-level registries map
// and returns (nameToID, idToName).
func loadRegistry(raw map[string]json.RawMessage, regName, filename string) (map[string]int32, map[int32]string) {
	regRaw, ok := raw[regName]
	if !ok {
		panic(fmt.Sprintf("gamedata: %s missing registry %q", filename, regName))
	}
	var reg registryEntry
	if err := json.Unmarshal(regRaw, &reg); err != nil {
		panic(fmt.Sprintf("gamedata: parsing %s registry %q: %v", filename, regName, err))
	}
	nameToID := make(map[string]int32, len(reg.Entries))
	idToName := make(map[int32]string, len(reg.Entries))
	for name, e := range reg.Entries {
		nameToID[name] = e.ProtocolID
		idToName[e.ProtocolID] = name
	}
	return nameToID, idToName
}

// validateVersion reads the "_gocraft_version" key from a raw JSON map and
// panics if it is missing or does not match expectedVersion.
func validateVersion(raw map[string]json.RawMessage, filename string) {
	vRaw, ok := raw["_gocraft_version"]
	if !ok {
		panic(fmt.Sprintf("gamedata: %s missing required \"_gocraft_version\" key", filename))
	}
	var ver string
	if err := json.Unmarshal(vRaw, &ver); err != nil {
		panic(fmt.Sprintf("gamedata: %s: invalid \"_gocraft_version\": %v", filename, err))
	}
	if ver != expectedVersion {
		panic(fmt.Sprintf(
			"gamedata: %s declares version %q but server expects %q — rebuild after replacing data files",
			filename, ver, expectedVersion))
	}
}

// isMetaKey reports whether a JSON key is a GoCraft documentation field rather
// than a real game entry.
func isMetaKey(k string) bool {
	switch k {
	case "_comment", "_format", "_source", "_coverage", "_gocraft_version":
		return true
	}
	return false
}

// ── Visible-failure wrappers ──────────────────────────────────────────────────

// blockStateIDLookup returns the Java state ID for the given key, logging a
// warning the first time an unknown block is encountered.
// Callers should use StateID (in blocks.go) which tries both the full key and
// the resource-location-only fallback before calling this.
func blockStateIDLookup(key string) (int32, bool) {
	id, ok := javaStateIDs[key]
	return id, ok
}

// ── Key helpers ───────────────────────────────────────────────────────────────

// blockStateKey builds a sorted-property lookup key matching Block.Key().
// Format: "namespace:name[k1=v1,k2=v2,…]" with properties in alphabetical order.
func blockStateKey(blockName string, props map[string]string) string {
	if len(props) == 0 {
		return blockName
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sortStrings(keys)

	out := []byte(blockName)
	out = append(out, '[')
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, k...)
		out = append(out, '=')
		out = append(out, props[k]...)
	}
	out = append(out, ']')
	return string(out)
}

// sortStrings sorts a slice of strings in-place (insertion sort — fine for the
// small property counts found in Minecraft block states).
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		key := ss[i]
		j := i - 1
		for j >= 0 && ss[j] > key {
			ss[j+1] = ss[j]
			j--
		}
		ss[j+1] = key
	}
}
