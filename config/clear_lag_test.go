package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultsEnableSurvivalAndKeepClearLagOptIn(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "server.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultGameMode != "survival" {
		t.Fatalf("default game mode = %q, want survival", cfg.DefaultGameMode)
	}
	if cfg.ClearLag.Enabled {
		t.Fatal("clear lag should be disabled by default")
	}
	if cfg.ClearLag.IntervalSeconds != 300 || !cfg.ClearLag.Remove.DroppedItems || cfg.ClearLag.Remove.HostileMobs {
		t.Fatalf("unexpected clear lag defaults: %+v", cfg.ClearLag)
	}
}
