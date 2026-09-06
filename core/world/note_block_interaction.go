package world

import "strconv"

const NoteBlockPitchCount = 25

// TuneNoteBlock advances a note block by one semitone, wraps after F sharp,
// and sets the instrument from the block below.
func TuneNoteBlock(block, below Block) (Block, bool) {
	if block.ResourceLocation() != "minecraft:note_block" {
		return Block{}, false
	}
	note, err := strconv.Atoi(block.Properties["note"])
	if err != nil || note < 0 || note >= NoteBlockPitchCount {
		note = 0
	}
	updated := copyInteractionBlock(block)
	updated.Properties["note"] = strconv.Itoa((note + 1) % NoteBlockPitchCount)
	updated.Properties["instrument"] = NoteBlockInstrument(below)
	return updated, true
}

// NoteBlockInstrument returns the vanilla instrument name for the block placed
// directly below a note block. Matches the Java 1.21.4 instrument selection.
func NoteBlockInstrument(below Block) string {
	name := below.ResourceLocation()
	switch {
	case name == "minecraft:clay":
		return "flute"
	case name == "minecraft:gold_block":
		return "bell"
	case name == "minecraft:packed_ice":
		return "chime"
	case name == "minecraft:bone_block":
		return "xylophone"
	case name == "minecraft:iron_block":
		return "iron_xylophone"
	case name == "minecraft:soul_sand":
		return "cow_bell"
	case name == "minecraft:pumpkin" || name == "minecraft:carved_pumpkin" ||
		name == "minecraft:jack_o_lantern":
		return "didgeridoo"
	case name == "minecraft:emerald_block":
		return "bit"
	case name == "minecraft:hay_block":
		return "banjo"
	case name == "minecraft:glowstone":
		return "pling"
	case isWoolBlock(name):
		return "guitar"
	case isSandOrGravel(name):
		return "snare"
	case isGlassVariant(name):
		return "hat"
	case isLogOrWood(name):
		return "bass"
	case isStoneVariant(name):
		return "basedrum"
	default:
		return "harp"
	}
}

func isWoolBlock(name string) bool {
	switch name {
	case "minecraft:white_wool", "minecraft:orange_wool", "minecraft:magenta_wool",
		"minecraft:light_blue_wool", "minecraft:yellow_wool", "minecraft:lime_wool",
		"minecraft:pink_wool", "minecraft:gray_wool", "minecraft:light_gray_wool",
		"minecraft:cyan_wool", "minecraft:purple_wool", "minecraft:blue_wool",
		"minecraft:brown_wool", "minecraft:green_wool", "minecraft:red_wool",
		"minecraft:black_wool":
		return true
	}
	return false
}

func isSandOrGravel(name string) bool {
	switch name {
	case "minecraft:sand", "minecraft:red_sand", "minecraft:gravel",
		"minecraft:white_concrete_powder", "minecraft:orange_concrete_powder",
		"minecraft:magenta_concrete_powder", "minecraft:light_blue_concrete_powder",
		"minecraft:yellow_concrete_powder", "minecraft:lime_concrete_powder",
		"minecraft:pink_concrete_powder", "minecraft:gray_concrete_powder",
		"minecraft:light_gray_concrete_powder", "minecraft:cyan_concrete_powder",
		"minecraft:purple_concrete_powder", "minecraft:blue_concrete_powder",
		"minecraft:brown_concrete_powder", "minecraft:green_concrete_powder",
		"minecraft:red_concrete_powder", "minecraft:black_concrete_powder":
		return true
	}
	return false
}

func isGlassVariant(name string) bool {
	switch name {
	case "minecraft:glass", "minecraft:tinted_glass", "minecraft:sea_lantern",
		"minecraft:beacon",
		"minecraft:glass_pane", "minecraft:iron_bars",
		"minecraft:white_stained_glass", "minecraft:orange_stained_glass",
		"minecraft:magenta_stained_glass", "minecraft:light_blue_stained_glass",
		"minecraft:yellow_stained_glass", "minecraft:lime_stained_glass",
		"minecraft:pink_stained_glass", "minecraft:gray_stained_glass",
		"minecraft:light_gray_stained_glass", "minecraft:cyan_stained_glass",
		"minecraft:purple_stained_glass", "minecraft:blue_stained_glass",
		"minecraft:brown_stained_glass", "minecraft:green_stained_glass",
		"minecraft:red_stained_glass", "minecraft:black_stained_glass",
		"minecraft:white_stained_glass_pane", "minecraft:orange_stained_glass_pane",
		"minecraft:magenta_stained_glass_pane", "minecraft:light_blue_stained_glass_pane",
		"minecraft:yellow_stained_glass_pane", "minecraft:lime_stained_glass_pane",
		"minecraft:pink_stained_glass_pane", "minecraft:gray_stained_glass_pane",
		"minecraft:light_gray_stained_glass_pane", "minecraft:cyan_stained_glass_pane",
		"minecraft:purple_stained_glass_pane", "minecraft:blue_stained_glass_pane",
		"minecraft:brown_stained_glass_pane", "minecraft:green_stained_glass_pane",
		"minecraft:red_stained_glass_pane", "minecraft:black_stained_glass_pane":
		return true
	}
	return false
}

func isLogOrWood(name string) bool {
	switch {
	case len(name) > 10 && (name[len(name)-4:] == "_log" || name[len(name)-5:] == "_wood" ||
		name[len(name)-8:] == "_hyphae" || name[len(name)-5:] == "_stem"):
		return true
	case name == "minecraft:bamboo_block" || name == "minecraft:bamboo_mosaic" ||
		name == "minecraft:mushroom_stem":
		return true
	}
	return false
}

func isStoneVariant(name string) bool {
	switch name {
	case "minecraft:stone", "minecraft:granite", "minecraft:polished_granite",
		"minecraft:diorite", "minecraft:polished_diorite",
		"minecraft:andesite", "minecraft:polished_andesite",
		"minecraft:blackstone", "minecraft:polished_blackstone",
		"minecraft:cobblestone", "minecraft:mossy_cobblestone",
		"minecraft:cobbled_deepslate", "minecraft:deepslate",
		"minecraft:polished_deepslate", "minecraft:calcite",
		"minecraft:tuff", "minecraft:basalt", "minecraft:smooth_basalt",
		"minecraft:netherrack", "minecraft:nether_bricks",
		"minecraft:obsidian", "minecraft:crying_obsidian",
		"minecraft:end_stone", "minecraft:sandstone",
		"minecraft:red_sandstone", "minecraft:prismarine":
		return true
	}
	return false
}
