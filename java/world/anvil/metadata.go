package anvil

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
)

// LevelMetadata contains the world identity and spawn fields GoCraft consumes
// from a Java Edition level.dat. Unknown NBT is intentionally left untouched.
type LevelMetadata struct {
	DataVersion   int32
	LevelName     string
	Seed          int64
	SpawnX        int32
	SpawnY        int32
	SpawnZ        int32
	GameType      int32
	Hardcore      bool
	AllowCommands bool
	VersionName   string
	// DayTime is the time-of-day tick stored in level.dat (0–23999).
	// Time is the total world age in ticks.
	DayTime int64
	Time    int64
}

// LoadLevelMetadata reads the gzip-compressed level.dat in worldDir.
func LoadLevelMetadata(worldDir string) (LevelMetadata, error) {
	file, err := os.Open(filepath.Join(worldDir, "level.dat"))
	if err != nil {
		return LevelMetadata{}, fmt.Errorf("anvil: opening level.dat: %w", err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return LevelMetadata{}, fmt.Errorf("anvil: level.dat gzip: %w", err)
	}
	root, err := ReadRootCompound(compressed)
	closeErr := compressed.Close()
	if err != nil {
		return LevelMetadata{}, fmt.Errorf("anvil: level.dat NBT: %w", err)
	}
	if closeErr != nil {
		return LevelMetadata{}, fmt.Errorf("anvil: closing level.dat gzip: %w", closeErr)
	}
	data := root["Data"]
	if data.typ != tagCompound {
		return LevelMetadata{}, fmt.Errorf("anvil: level.dat missing Data compound")
	}
	settings := data.Get("WorldGenSettings")
	version := data.Get("Version")
	return LevelMetadata{
		DataVersion:   data.Get("DataVersion").Int(),
		LevelName:     data.Get("LevelName").Str(),
		Seed:          settings.Get("seed").Long(),
		SpawnX:        data.Get("SpawnX").Int(),
		SpawnY:        data.Get("SpawnY").Int(),
		SpawnZ:        data.Get("SpawnZ").Int(),
		GameType:      data.Get("GameType").Int(),
		Hardcore:      data.Get("hardcore").Byte() != 0,
		AllowCommands: data.Get("allowCommands").Byte() != 0,
		VersionName:   version.Get("Name").Str(),
		DayTime:       data.Get("DayTime").Long(),
		Time:          data.Get("Time").Long(),
	}, nil
}
