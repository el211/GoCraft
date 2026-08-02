// Package world contains the edition-agnostic world model for GoCraft.
// Java- and Bedrock-specific packet structures must NOT appear here.
// The Java adapter (java/world) converts canonical types to wire format;
// a future Bedrock adapter will do the same independently.
package world

import (
	"sort"
	"strings"
)

// Block is the canonical, edition-agnostic representation of a placed block.
//
// Identity is defined by Namespace, Name, and Properties together:
//   - Two blocks with the same Namespace and Name but different Properties
//     are different block states (e.g. grass_block[snowy=false] vs [snowy=true]).
//   - A block with nil or empty Properties is the default state for that type.
//
// Edition-specific IDs are determined entirely at the adapter boundary:
//
//	Canonical Block
//	       │
//	┌──────┴──────┐
//	▼             ▼
//	Java global   Bedrock runtime
//	state ID      ID
//
// The core never stores, imports, or depends on any edition-specific ID.
// Only the encoder packages (java/world, future bedrock/world) know IDs.
type Block struct {
	// Namespace is the resource-location namespace; almost always "minecraft".
	Namespace string
	// Name is the block identifier without namespace, e.g. "stone", "grass_block".
	Name string
	// Properties holds block-state key/value pairs, e.g. {"facing": "north"}.
	// nil and empty map are both treated as the default state.
	Properties map[string]string
}

// Air is the canonical air block representing empty space.
var Air = Block{Namespace: "minecraft", Name: "air"}

// IsAir reports whether b represents empty space.
// The zero value (empty Block) and minecraft:air with no properties are both air.
func (b Block) IsAir() bool {
	return (b.Namespace == "" && b.Name == "") ||
		(b.Namespace == "minecraft" && b.Name == "air" && len(b.Properties) == 0)
}

// ResourceLocation returns the "namespace:name" identifier, e.g. "minecraft:stone".
// Properties are not included — use this for registry lookups that key on block type.
func (b Block) ResourceLocation() string {
	if b.Namespace == "" {
		return b.Name
	}
	return b.Namespace + ":" + b.Name
}

// DoublePlantPartnerY returns the vertical partner coordinate and expected
// half for vanilla two-block plants. It is shared by edition adapters so a
// break from either Java or Bedrock cannot leave a floating half behind.
func DoublePlantPartnerY(block Block, y int) (partnerY int, partnerHalf string, ok bool) {
	switch block.ResourceLocation() {
	case "minecraft:tall_grass", "minecraft:large_fern", "minecraft:sunflower",
		"minecraft:lilac", "minecraft:rose_bush", "minecraft:peony", "minecraft:pitcher_plant":
	default:
		return 0, "", false
	}
	switch block.Properties["half"] {
	case "lower":
		return y + 1, "upper", true
	case "upper":
		return y - 1, "lower", true
	default:
		return 0, "", false
	}
}

// Equal reports whether b and other represent exactly the same block state,
// including all properties. Map iteration order does not affect the result.
func (b Block) Equal(other Block) bool {
	if b.Namespace != other.Namespace || b.Name != other.Name {
		return false
	}
	if len(b.Properties) != len(other.Properties) {
		return false
	}
	for k, v := range b.Properties {
		if other.Properties[k] != v {
			return false
		}
	}
	return true
}

// Key returns the canonical string key for this block state, suitable for use
// in maps, hash tables, palette deduplication, and registry lookups.
//
// Format:
//
//	"namespace:name"                           — default state (no properties)
//	"namespace:name[prop1=val1,prop2=val2]"    — with properties sorted A→Z
//
// Two Block values that represent the same state always produce the same Key
// regardless of the order their Properties map was populated.
func (b Block) Key() string {
	rl := b.ResourceLocation()
	if len(b.Properties) == 0 {
		return rl
	}
	propKeys := make([]string, 0, len(b.Properties))
	for k := range b.Properties {
		propKeys = append(propKeys, k)
	}
	sort.Strings(propKeys)
	pairs := make([]string, len(propKeys))
	for i, k := range propKeys {
		pairs[i] = k + "=" + b.Properties[k]
	}
	return rl + "[" + strings.Join(pairs, ",") + "]"
}
