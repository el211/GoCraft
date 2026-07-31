package anvil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	coreworld "GoCraft/core/world"
)

func TestStorageRegionRoundTrip(t *testing.T) {
	worldDir := t.TempDir()
	storage, err := NewStorage(worldDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	chunk := &coreworld.Chunk{X: 33, Z: -1}
	sectionIndex := (64 - coreworld.WorldMinY) / coreworld.SectionSize
	section := coreworld.NewSection()
	log := coreworld.Block{Namespace: "minecraft", Name: "oak_log", Properties: map[string]string{"axis": "x"}}
	furnace := coreworld.Block{Namespace: "minecraft", Name: "furnace", Properties: map[string]string{"facing": "north", "lit": "false"}}
	section.Set(1, 0, 1, log)
	section.Set(2, 0, 3, furnace)
	section.SetUniformBiome("minecraft:plains")
	section.SetBiomeCell(1, 0, 1, "minecraft:desert")
	section.SetBiomeCell(2, 1, 3, "minecraft:badlands")
	chunk.Sections[sectionIndex] = section
	chunk.BlockEntities = []coreworld.BlockEntity{{
		X:     int(chunk.X)*16 + 2,
		Y:     64,
		Z:     int(chunk.Z)*16 + 3,
		Type:  "minecraft:furnace",
		Data:  testBlockEntityPayload(),
		Items: []coreworld.ContainerItem{{Slot: 2, ItemID: "minecraft:iron_ingot", Count: 17}},
	}}

	neighbor := &coreworld.Chunk{X: 34, Z: -1}
	neighborSection := coreworld.NewSection()
	neighborSection.Set(0, 0, 0, coreworld.Block{Namespace: "minecraft", Name: "diamond_block"})
	neighbor.Sections[sectionIndex] = neighborSection

	if err := storage.SaveChunk(chunk); err != nil {
		t.Fatalf("SaveChunk: %v", err)
	}
	if err := storage.SaveChunk(neighbor); err != nil {
		t.Fatalf("SaveChunk neighbor: %v", err)
	}
	if err := storage.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	regionPath := filepath.Join(worldDir, "region", "r.1.-1.mca")
	info, err := os.Stat(regionPath)
	if err != nil {
		t.Fatalf("region file: %v", err)
	}
	if info.Size() < 3*4096 {
		t.Fatalf("region file size=%d, want header plus chunk sectors", info.Size())
	}

	loadedStorage, err := NewStorage(worldDir)
	if err != nil {
		t.Fatalf("NewStorage reload: %v", err)
	}
	loaded, err := loadedStorage.LoadChunk(chunk.X, chunk.Z)
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadChunk returned nil")
	}
	loadedSection := loaded.Sections[sectionIndex]
	if got := loadedSection.At(1, 0, 1); !got.Equal(log) {
		t.Fatalf("property block=%s, want %s", got.Key(), log.Key())
	}
	if got := loadedSection.BiomeAtCell(1, 0, 1); got != "minecraft:desert" {
		t.Fatalf("biome cell=%q, want minecraft:desert", got)
	}
	if got := loadedSection.BiomeAtCell(2, 1, 3); got != "minecraft:badlands" {
		t.Fatalf("biome cell=%q, want minecraft:badlands", got)
	}
	if len(loaded.BlockEntities) != 1 {
		t.Fatalf("block entities=%d, want 1", len(loaded.BlockEntities))
	}
	if got := loaded.BlockEntities[0]; got.Type != "minecraft:furnace" || got.X != chunk.BlockEntities[0].X || got.Y != 64 || got.Z != chunk.BlockEntities[0].Z || !bytes.Equal(got.Data, chunk.BlockEntities[0].Data) {
		t.Fatalf("block entity differs after disk round trip: %+v", got)
	}
	if got := loaded.BlockEntities[0].Items; len(got) != 1 || got[0].Slot != 2 || got[0].ItemID != "minecraft:iron_ingot" || got[0].Count != 17 {
		t.Fatalf("container items differ after disk round trip: %+v", got)
	}

	root, err := loadChunkFromRegion(worldDir, chunk.X, chunk.Z)
	if err != nil {
		t.Fatalf("raw reload: %v", err)
	}
	if root["Heightmaps"].Get("WORLD_SURFACE").typ != tagLongArr || len(root["Heightmaps"].Get("WORLD_SURFACE").LongArray()) == 0 {
		t.Fatal("saved chunk has no WORLD_SURFACE heightmap")
	}
	if got := len(root["block_entities"].List()); got != 1 {
		t.Fatalf("saved block_entities=%d, want 1", got)
	}

	loadedSection.Set(3, 0, 3, coreworld.Block{Namespace: "minecraft", Name: "glass"})
	if err := loadedStorage.SaveChunk(loaded); err != nil {
		t.Fatalf("resave loaded chunk: %v", err)
	}
	if err := loadedStorage.Flush(); err != nil {
		t.Fatalf("reflush loaded chunk: %v", err)
	}
	preservedNeighbor, err := loadedStorage.LoadChunk(neighbor.X, neighbor.Z)
	if err != nil {
		t.Fatalf("reload untouched neighbor: %v", err)
	}
	if preservedNeighbor == nil || preservedNeighbor.Sections[sectionIndex].At(0, 0, 0).ResourceLocation() != "minecraft:diamond_block" {
		t.Fatal("rewriting one region slot did not preserve its neighbor")
	}
}

func testBlockEntityPayload() []byte {
	var buffer bytes.Buffer
	wByte(&buffer, byte(tagCompound))
	writeCompoundPayload(&buffer, map[string]Tag{
		"BurnTime":   {typ: tagShort, shortV: 42},
		"CustomName": {typ: tagString, strV: `{"text":"Test Furnace"}`},
	})
	return buffer.Bytes()
}

func TestWorldFlushPersistsGeneratedChunk(t *testing.T) {
	worldDir := t.TempDir()
	storage, err := NewStorage(worldDir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(worldDir, "region")); err != nil || !info.IsDir() {
		t.Fatalf("region directory was not created immediately: info=%v err=%v", info, err)
	}
	world := coreworld.New(&coreworld.FlatGenerator{}, storage, false)
	defer world.Close()
	world.Chunk(0, 0)
	if err := world.Flush(); err != nil {
		t.Fatalf("World.Flush: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worldDir, "region", "r.0.0.mca")); err != nil {
		t.Fatalf("autosave did not create region file: %v", err)
	}
}
