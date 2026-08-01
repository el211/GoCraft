package server

import (
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const seedFileName = "seed.txt"

// resolveWorldSeed determines the definitive world seed to use.
//
// Rules (in priority order):
//  1. If configuredSeed != 0, use it as-is (explicit override wins).
//  2. If worldDir is non-empty and seed.txt exists there, load the persisted seed.
//  3. Otherwise generate a random seed, and if worldDir is non-empty write it to
//     seed.txt so subsequent restarts use the same world.
//
// A seed of 0 is treated as "not set" — it is never written to seed.txt.
// This mirrors vanilla Minecraft's behaviour where an empty seed field means random.
func resolveWorldSeed(configuredSeed int64, worldDir string) int64 {
	// Explicit non-zero seed from config or level.dat — use it directly.
	if configuredSeed != 0 {
		return configuredSeed
	}

	seedFile := ""
	if worldDir != "" {
		seedFile = filepath.Join(worldDir, seedFileName)
	}

	// Try loading a previously persisted seed.
	if seedFile != "" {
		if data, err := os.ReadFile(seedFile); err == nil {
			s := strings.TrimSpace(string(data))
			if parsed, err := strconv.ParseInt(s, 10, 64); err == nil && parsed != 0 {
				slog.Info("server: loaded world seed from seed.txt", "seed", parsed)
				return parsed
			}
		}
	}

	// Generate a new random seed.
	seed := rand.New(rand.NewSource(time.Now().UnixNano())).Int63()
	slog.Info("server: generated random world seed", "seed", seed)

	// Persist it so the world stays consistent across restarts.
	if seedFile != "" {
		if err := os.MkdirAll(worldDir, 0o755); err == nil {
			_ = os.WriteFile(seedFile, []byte(strconv.FormatInt(seed, 10)+"\n"), 0o644)
			slog.Info("server: saved world seed to seed.txt", "path", seedFile)
		}
	}

	return seed
}
