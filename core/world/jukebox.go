package world

// Jukebox canonical operations: inserting a music disc sets the has_record
// block property and stores the disc in the block entity; ejecting removes it.

// musicDiscSignal maps each music disc to its vanilla comparator output (1–15).
// Signal 0 means no disc; empty jukebox outputs 0.
var musicDiscSignal = map[string]int{
	"minecraft:music_disc_13":               1,
	"minecraft:music_disc_cat":              2,
	"minecraft:music_disc_blocks":           3,
	"minecraft:music_disc_chirp":            4,
	"minecraft:music_disc_far":              5,
	"minecraft:music_disc_mall":             6,
	"minecraft:music_disc_mellohi":          7,
	"minecraft:music_disc_stal":             8,
	"minecraft:music_disc_strad":            9,
	"minecraft:music_disc_ward":             10,
	"minecraft:music_disc_11":               11,
	"minecraft:music_disc_wait":             12,
	"minecraft:music_disc_otherside":        14,
	"minecraft:music_disc_relic":            14,
	"minecraft:music_disc_5":               15,
	"minecraft:music_disc_pigstep":         13,
	"minecraft:music_disc_precipice":       13,
	"minecraft:music_disc_creator":         13,
	"minecraft:music_disc_creator_music_box": 11,
}

// JukeboxComparatorSignal returns the comparator output for a jukebox: the
// disc-specific signal strength (1–15) when playing, or 0 when empty.
func JukeboxComparatorSignal(discItemID string) int {
	return musicDiscSignal[discItemID]
}

// musicDiscItems is the set of valid music disc item IDs an enderman can place
// into a jukebox. The map value is the sound event to play.
var musicDiscItems = map[string]string{
	"minecraft:music_disc_13":          "minecraft:music.disc.13",
	"minecraft:music_disc_cat":         "minecraft:music.disc.cat",
	"minecraft:music_disc_blocks":      "minecraft:music.disc.blocks",
	"minecraft:music_disc_chirp":       "minecraft:music.disc.chirp",
	"minecraft:music_disc_far":         "minecraft:music.disc.far",
	"minecraft:music_disc_mall":        "minecraft:music.disc.mall",
	"minecraft:music_disc_mellohi":     "minecraft:music.disc.mellohi",
	"minecraft:music_disc_stal":        "minecraft:music.disc.stal",
	"minecraft:music_disc_strad":       "minecraft:music.disc.strad",
	"minecraft:music_disc_ward":        "minecraft:music.disc.ward",
	"minecraft:music_disc_11":          "minecraft:music.disc.11",
	"minecraft:music_disc_wait":        "minecraft:music.disc.wait",
	"minecraft:music_disc_otherside":   "minecraft:music.disc.otherside",
	"minecraft:music_disc_relic":       "minecraft:music.disc.relic",
	"minecraft:music_disc_5":           "minecraft:music.disc.5",
	"minecraft:music_disc_pigstep":     "minecraft:music.disc.pigstep",
	"minecraft:music_disc_precipice":   "minecraft:music.disc.precipice",
	"minecraft:music_disc_creator":     "minecraft:music.disc.creator",
	"minecraft:music_disc_creator_music_box": "minecraft:music.disc.creator_music_box",
}

// IsMusicDisc reports whether the item ID is a valid music disc.
func IsMusicDisc(itemID string) bool {
	_, ok := musicDiscItems[itemID]
	return ok
}

// MusicDiscSound returns the sound event for a music disc, or "" if not a disc.
func MusicDiscSound(itemID string) string {
	return musicDiscItems[itemID]
}

// InsertJukeboxRecord inserts a music disc into an empty jukebox. Returns the
// updated block (has_record=true) and true if the block is a jukebox and the
// item is a music disc.
func InsertJukeboxRecord(block Block, itemID string) (Block, bool) {
	if block.ResourceLocation() != "minecraft:jukebox" {
		return block, false
	}
	if block.Properties["has_record"] == "true" {
		return block, false
	}
	if !IsMusicDisc(itemID) {
		return block, false
	}
	updated := copyWorldBlock(block)
	updated.Properties["has_record"] = "true"
	return updated, true
}

// EjectJukeboxRecord removes any disc from a jukebox. Returns the stored disc
// item ID, the updated block (has_record=false), and true when a disc was present.
func EjectJukeboxRecord(block Block, storedRecord string) (itemID string, updated Block, ok bool) {
	if block.ResourceLocation() != "minecraft:jukebox" {
		return "", block, false
	}
	if block.Properties["has_record"] != "true" || storedRecord == "" {
		return "", block, false
	}
	cleared := copyWorldBlock(block)
	cleared.Properties["has_record"] = "false"
	return storedRecord, cleared, true
}

// JukeboxRecordItem returns the record stored in a block entity's Items slot 0,
// or "" when the jukebox is empty.
func JukeboxRecordItem(entity BlockEntity) string {
	for _, item := range entity.Items {
		if item.Slot == 0 && item.ItemID != "" {
			return item.ItemID
		}
	}
	return ""
}
