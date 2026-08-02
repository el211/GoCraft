package handler

import (
	"strings"

	corentity "GoCraft/core/entity"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

const (
	soundCategoryBlocks  int32 = 4
	soundCategoryNeutral int32 = 6
	soundCategoryPlayers int32 = 7
)

// soundHolderID encodes a registry reference in the 1.21.4 Holder format.
// Zero selects an inline sound definition, so registry IDs are offset by one.
func soundHolderID(name string) int32 {
	id := javaworld.SoundEventID(name)
	if id < 0 {
		return -1
	}
	return id + 1
}

func buildEntitySound(name string, category, entityID int32, volume, pitch float32) *protocol.Packet {
	holderID := soundHolderID(name)
	if holderID < 0 {
		return nil
	}
	return protocol.NewBuilder(packetIDSoundEntity).
		VarInt(holderID).
		VarInt(category).
		VarInt(entityID).
		Float(volume).
		Float(pitch).
		Long(0).
		Build()
}

func buildSoundAt(name string, category int32, x, y, z float64, volume, pitch float32) *protocol.Packet {
	holderID := soundHolderID(name)
	if holderID < 0 {
		return nil
	}
	return protocol.NewBuilder(packetIDSound).
		VarInt(holderID).
		VarInt(category).
		Int(int32(x * 8)).
		Int(int32(y * 8)).
		Int(int32(z * 8)).
		Float(volume).
		Float(pitch).
		Long(0).
		Build()
}

// SoundCategoryHostile is the sound category for hostile mobs.
const SoundCategoryHostile int32 = 5

// SoundCategoryPlayers is the sound category for player weapon effects.
const SoundCategoryPlayers int32 = soundCategoryPlayers

// BroadcastSoundAt plays a positioned sound event to all connected players.
func BroadcastSoundAt(mgr *session.Manager, name string, category int32, x, y, z float64, volume, pitch float32) {
	broadcastSoundAt(mgr, name, category, x, y, z, volume, pitch)
}

func broadcastSoundAt(mgr *session.Manager, name string, category int32, x, y, z float64, volume, pitch float32) {
	pkt := buildSoundAt(name, category, x, y, z, volume, pitch)
	if pkt == nil {
		return
	}
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

func hurtSound(e *corentity.Entity) string {
	name := strings.TrimPrefix(string(e.Type), "minecraft:")
	switch name {
	case "mooshroom":
		name = "cow"
	case "donkey", "mule", "skeleton_horse", "zombie_horse":
		name = "horse"
	case "trader_llama":
		name = "llama"
	case "snow_golem":
		name = "snow_golem"
	case "iron_golem":
		name = "iron_golem"
	case "wandering_trader":
		name = "wandering_trader"
	}
	sound := "minecraft:entity." + name + ".hurt"
	if javaworld.SoundEventID(sound) >= 0 {
		return sound
	}
	return "minecraft:entity.generic.hurt"
}

func blockBreakSound(blockName string) string {
	switch {
	case strings.Contains(blockName, "_ore"),
		strings.Contains(blockName, "stone"),
		strings.Contains(blockName, "deepslate"),
		strings.Contains(blockName, "andesite"),
		strings.Contains(blockName, "diorite"),
		strings.Contains(blockName, "granite"):
		return "minecraft:block.stone.break"
	case strings.Contains(blockName, "glass"):
		return "minecraft:block.glass.break"
	case strings.Contains(blockName, "planks"),
		strings.Contains(blockName, "log"),
		strings.Contains(blockName, "wood"),
		strings.Contains(blockName, "door"):
		return "minecraft:block.wood.break"
	case strings.Contains(blockName, "sand"):
		return "minecraft:block.sand.break"
	case strings.Contains(blockName, "gravel"):
		return "minecraft:block.gravel.break"
	case strings.Contains(blockName, "grass"),
		strings.Contains(blockName, "dirt"),
		strings.Contains(blockName, "flower"),
		strings.Contains(blockName, "fern"),
		strings.Contains(blockName, "bush"):
		return "minecraft:block.grass.break"
	default:
		return "minecraft:block.stone.break"
	}
}

func rewardsExperience(blockName string) bool {
	switch blockName {
	case "minecraft:coal_ore", "minecraft:deepslate_coal_ore",
		"minecraft:diamond_ore", "minecraft:deepslate_diamond_ore",
		"minecraft:emerald_ore", "minecraft:deepslate_emerald_ore",
		"minecraft:lapis_ore", "minecraft:deepslate_lapis_ore",
		"minecraft:redstone_ore", "minecraft:deepslate_redstone_ore",
		"minecraft:nether_quartz_ore", "minecraft:nether_gold_ore":
		return true
	}
	return false
}
