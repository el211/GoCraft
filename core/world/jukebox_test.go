package world

import "testing"

func jukeboxBlock(hasRecord bool) Block {
	val := "false"
	if hasRecord {
		val = "true"
	}
	return Block{Namespace: "minecraft", Name: "jukebox",
		Properties: map[string]string{"has_record": val}}
}

func TestInsertJukeboxRecord(t *testing.T) {
	empty := jukeboxBlock(false)
	updated, ok := InsertJukeboxRecord(empty, "minecraft:music_disc_cat")
	if !ok {
		t.Fatal("InsertJukeboxRecord returned false for valid disc")
	}
	if updated.Properties["has_record"] != "true" {
		t.Fatalf("has_record = %q after insert, want true", updated.Properties["has_record"])
	}
}

func TestInsertJukeboxRecordRejectsNonDisc(t *testing.T) {
	empty := jukeboxBlock(false)
	_, ok := InsertJukeboxRecord(empty, "minecraft:dirt")
	if ok {
		t.Fatal("InsertJukeboxRecord accepted non-disc item")
	}
}

func TestInsertJukeboxRecordRejectsFull(t *testing.T) {
	full := jukeboxBlock(true)
	_, ok := InsertJukeboxRecord(full, "minecraft:music_disc_cat")
	if ok {
		t.Fatal("InsertJukeboxRecord accepted insert into full jukebox")
	}
}

func TestEjectJukeboxRecord(t *testing.T) {
	full := jukeboxBlock(true)
	itemID, cleared, ok := EjectJukeboxRecord(full, "minecraft:music_disc_cat")
	if !ok {
		t.Fatal("EjectJukeboxRecord returned false for full jukebox")
	}
	if itemID != "minecraft:music_disc_cat" {
		t.Fatalf("ejected item = %q, want minecraft:music_disc_cat", itemID)
	}
	if cleared.Properties["has_record"] != "false" {
		t.Fatalf("has_record = %q after eject, want false", cleared.Properties["has_record"])
	}
}

func TestEjectJukeboxRecordRejectsEmpty(t *testing.T) {
	empty := jukeboxBlock(false)
	_, _, ok := EjectJukeboxRecord(empty, "")
	if ok {
		t.Fatal("EjectJukeboxRecord returned true for empty jukebox")
	}
}

func TestJukeboxRecordItem(t *testing.T) {
	entity := BlockEntity{
		Items: []ContainerItem{{Slot: 0, ItemID: "minecraft:music_disc_13", Count: 1}},
	}
	if got := JukeboxRecordItem(entity); got != "minecraft:music_disc_13" {
		t.Fatalf("JukeboxRecordItem = %q, want minecraft:music_disc_13", got)
	}
	if got := JukeboxRecordItem(BlockEntity{}); got != "" {
		t.Fatalf("JukeboxRecordItem on empty entity = %q, want empty", got)
	}
}
