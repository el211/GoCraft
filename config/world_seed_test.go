package config

import (
	"strings"
	"testing"
)

func TestWorldSeedEnvironmentOverride(t *testing.T) {
	t.Setenv("GOCRAFT_WORLD_SEED", "-9223372036854775808")
	cfg := defaults()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if cfg.WorldSeed != -9223372036854775808 {
		t.Fatalf("WorldSeed = %d, want signed 64-bit override", cfg.WorldSeed)
	}
}

func TestWorldSeedEnvironmentRejectsInvalidValue(t *testing.T) {
	t.Setenv("GOCRAFT_WORLD_SEED", "not-a-seed")
	cfg := defaults()
	err := cfg.ApplyEnvOverrides()
	if err == nil || !strings.Contains(err.Error(), "GOCRAFT_WORLD_SEED") {
		t.Fatalf("ApplyEnvOverrides error = %v, want named seed error", err)
	}
}

func TestStreamingEnvironmentOverrides(t *testing.T) {
	t.Setenv("GOCRAFT_VIEW_DISTANCE", "10")
	t.Setenv("GOCRAFT_PREGENERATE_RADIUS", "16")
	cfg := defaults()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if cfg.ViewDistance != 10 || cfg.PreGenerateRadius != 16 {
		t.Fatalf("streaming distances=(%d,%d), want (10,16)", cfg.ViewDistance, cfg.PreGenerateRadius)
	}
}

func TestPregenerationRadiusCannotBeSmallerThanView(t *testing.T) {
	cfg := defaults()
	cfg.ViewDistance = 12
	cfg.PreGenerateRadius = 8
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "pregenerate_radius") {
		t.Fatalf("validate error=%v, want pregenerate_radius error", err)
	}
}

func TestWorldStorageDefaultsToDisk(t *testing.T) {
	cfg := defaults()
	if cfg.WorldStorage != WorldStorageDisk || cfg.WorldDir != "world" {
		t.Fatalf("storage defaults = (%q,%q), want (disk,world)", cfg.WorldStorage, cfg.WorldDir)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWorldStorageEnvironmentSelectsMemory(t *testing.T) {
	t.Setenv("GOCRAFT_WORLD_STORAGE", "MEMORY")
	cfg := defaults()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if cfg.WorldStorage != WorldStorageMemory {
		t.Fatalf("WorldStorage = %q, want memory", cfg.WorldStorage)
	}
}

func TestWorldStorageRejectsInvalidMode(t *testing.T) {
	cfg := defaults()
	cfg.WorldStorage = "cloud"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "world_storage") {
		t.Fatalf("validate error = %v, want world_storage error", err)
	}
}

func TestMaxCachedChunksEnvironmentOverride(t *testing.T) {
	t.Setenv("GOCRAFT_MAX_CACHED_CHUNKS", "512")
	cfg := defaults()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCachedChunks != 512 {
		t.Fatalf("MaxCachedChunks=%d, want 512", cfg.MaxCachedChunks)
	}
}

func TestMaxCachedChunksRejectsOutOfRange(t *testing.T) {
	cfg := defaults()
	cfg.MaxCachedChunks = 64
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "max_cached_chunks") {
		t.Fatalf("validate error=%v, want max_cached_chunks error", err)
	}
}

func TestCombatEnvironmentOverrides(t *testing.T) {
	t.Setenv("GOCRAFT_ATTACK_COOLDOWN", "true")
	t.Setenv("GOCRAFT_KNOCKBACK_HORIZONTAL", "0.55")
	t.Setenv("GOCRAFT_KNOCKBACK_VERTICAL", "0.42")
	cfg := defaults()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Combat.AttackCooldown || cfg.Combat.KnockbackHorizontal != 0.55 || cfg.Combat.KnockbackVertical != 0.42 {
		t.Fatalf("combat config = %+v", cfg.Combat)
	}
}

func TestCombatKnockbackRejectsUnsafeRange(t *testing.T) {
	cfg := defaults()
	cfg.Combat.KnockbackHorizontal = 4.1
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "knockback_horizontal") {
		t.Fatalf("validate error = %v, want knockback_horizontal range error", err)
	}
}
