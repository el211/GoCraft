package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	"GoCraft/internal/debuglog"
)

const playerDataVersion = 1

type playerDataStore struct {
	directory string
	mu        sync.Mutex
}

type persistedPlayerData struct {
	Version       int                                    `json:"version"`
	Username      string                                 `json:"username"`
	Position      spatial.Vec3                           `json:"position"`
	Rotation      spatial.Rotation                       `json:"rotation"`
	GameMode      player.GameMode                        `json:"game_mode"`
	Health        float32                                `json:"health"`
	Dead          bool                                   `json:"dead,omitempty"`
	Food          int32                                  `json:"food"`
	Saturation    float32                                `json:"saturation"`
	Exhaustion    float32                                `json:"exhaustion"`
	Absorption    float32                                `json:"absorption,omitempty"`
	StatusEffects []player.StatusEffect                  `json:"status_effects,omitempty"`
	Experience    int32                                  `json:"experience"`
	Tags          []string                               `json:"tags,omitempty"`
	Inventory     [player.InventorySize]player.ItemStack `json:"inventory"`
	EnderChest    [27]player.ItemStack                   `json:"ender_chest"`
	HeldSlot      int                                    `json:"held_slot"`
	SpawnPoint    spatial.BlockPos                       `json:"spawn_point"`
	HasSpawnPoint bool                                   `json:"has_spawn_point"`
	SpawnIsAnchor bool                                   `json:"spawn_is_anchor,omitempty"`
	Dimension     int32                                  `json:"dimension"`
}

func persistedPlayerWasDead(data persistedPlayerData) bool {
	// Player data written before the Dead field existed recorded a death as
	// health zero. Preserve compatibility instead of reviving at one health.
	return data.Dead || data.Health <= 0
}

func newPlayerDataStore(directory string) *playerDataStore {
	return &playerDataStore{directory: directory}
}

func (store *playerDataStore) path(uuid [16]byte) string {
	return filepath.Join(store.directory, formatPlayerUUID(uuid)+".json")
}

func formatPlayerUUID(uuid [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

func snapshotPlayerData(p *player.Player) persistedPlayerData {
	health, food, saturation, dead := p.HealthSnapshot()
	_, _, exhaustion := p.HungerSnapshot()
	_, experience, _ := p.ExperienceSnapshot()
	return persistedPlayerData{
		Version: playerDataVersion, Username: p.Username,
		Position: p.Position, Rotation: p.Rotation, GameMode: p.GameMode,
		Health: health, Dead: dead, Food: food, Saturation: saturation, Exhaustion: exhaustion, Experience: experience, Tags: p.Tags(),
		Absorption: p.AbsorptionSnapshot(), StatusEffects: p.StatusEffectsSnapshot(),
		Inventory: p.Inventory, EnderChest: p.EnderChestInventory, HeldSlot: p.HeldSlot,
		SpawnPoint: p.SpawnPoint, HasSpawnPoint: p.HasSpawnPoint, SpawnIsAnchor: p.SpawnIsAnchor,
		Dimension: p.Dimension,
	}
}

func (store *playerDataStore) save(uuid [16]byte, data persistedPlayerData) error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := os.MkdirAll(store.directory, 0o755); err != nil {
		return fmt.Errorf("create playerdata directory: %w", err)
	}
	temporary, err := os.CreateTemp(store.directory, ".player-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary player data: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("encode player data: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync player data: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close player data: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path(uuid)); err != nil {
		return fmt.Errorf("replace player data: %w", err)
	}
	keepTemporary = false
	return nil
}

func (store *playerDataStore) load(uuid [16]byte) (persistedPlayerData, bool, error) {
	if store == nil {
		return persistedPlayerData{}, false, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	file, err := os.Open(store.path(uuid))
	if errors.Is(err, os.ErrNotExist) {
		return persistedPlayerData{}, false, nil
	}
	if err != nil {
		return persistedPlayerData{}, false, fmt.Errorf("open player data: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 2<<20))
	decoder.DisallowUnknownFields()
	var data persistedPlayerData
	if err := decoder.Decode(&data); err != nil {
		return persistedPlayerData{}, false, fmt.Errorf("decode player data: %w", err)
	}
	if data.Version != playerDataVersion {
		return persistedPlayerData{}, false, fmt.Errorf("unsupported player data version %d", data.Version)
	}
	return data, true, nil
}

func applyPersistedPlayerData(p *player.Player, data persistedPlayerData) error {
	if !finitePosition(data.Position) || data.Position.Y < -2048 || data.Position.Y > 2048 {
		return fmt.Errorf("invalid saved position %+v", data.Position)
	}
	if data.HeldSlot < 0 || data.HeldSlot > 8 {
		return fmt.Errorf("invalid held slot %d", data.HeldSlot)
	}
	if data.GameMode > player.GameModeSpectator {
		return fmt.Errorf("invalid game mode %d", data.GameMode)
	}
	if data.Dimension < 0 || data.Dimension > 2 {
		return fmt.Errorf("invalid dimension %d", data.Dimension)
	}
	for slot, stack := range data.Inventory {
		if stack.Count < 0 || stack.Count > 127 || stack.Damage < 0 ||
			(!stack.IsEmpty() && !strings.Contains(stack.ItemID, ":")) {
			return fmt.Errorf("invalid inventory stack in slot %d: %+v", slot, stack)
		}
		if stack.IsEmpty() {
			data.Inventory[slot] = player.ItemStack{}
		}
	}
	for slot, stack := range data.EnderChest {
		if stack.Count < 0 || stack.Count > 127 || stack.Damage < 0 ||
			(!stack.IsEmpty() && !strings.Contains(stack.ItemID, ":")) {
			return fmt.Errorf("invalid ender chest stack in slot %d: %+v", slot, stack)
		}
		if stack.IsEmpty() {
			data.EnderChest[slot] = player.ItemStack{}
		}
	}
	p.Position = data.Position
	p.Rotation = data.Rotation
	p.GameMode = data.GameMode
	if persistedPlayerWasDead(data) {
		p.Health = 0
		p.Dead = true
	} else {
		p.Health = min(max(data.Health, 1), p.MaxHealth)
		p.Dead = false
	}
	p.Food = min(max(data.Food, 0), 20)
	p.Saturation = min(max(data.Saturation, 0), 20)
	p.Exhaustion = min(max(data.Exhaustion, 0), 4)
	p.StatusEffects = nil
	p.Absorption = 0
	for _, effect := range data.StatusEffects {
		if _, ok := p.AddStatusEffect(effect); !ok {
			return fmt.Errorf("invalid saved status effect %+v", effect)
		}
	}
	p.Absorption = min(max(data.Absorption, 0), p.Absorption)
	p.SetTotalExperience(data.Experience)
	for _, tag := range data.Tags {
		if strings.TrimSpace(tag) == "" || len(tag) > 1024 {
			return fmt.Errorf("invalid player tag %q", tag)
		}
		p.AddTag(tag)
	}
	p.Inventory = data.Inventory
	p.EnderChestInventory = data.EnderChest
	p.HeldSlot = data.HeldSlot
	p.SpawnPoint = data.SpawnPoint
	p.HasSpawnPoint = data.HasSpawnPoint
	p.SpawnIsAnchor = data.SpawnIsAnchor
	p.Dimension = data.Dimension
	return nil
}

func finitePosition(position spatial.Vec3) bool {
	return !math.IsNaN(position.X) && !math.IsInf(position.X, 0) &&
		!math.IsNaN(position.Y) && !math.IsInf(position.Y, 0) &&
		!math.IsNaN(position.Z) && !math.IsInf(position.Z, 0)
}

func (s *Server) loadPlayerData(p *player.Player) {
	if s == nil || s.playerStore == nil || p == nil {
		return
	}
	data, found, err := s.playerStore.load(p.UUID)
	if err != nil {
		slog.Warn("could not load player data; using spawn defaults", "uuid", p.UUID, "err", err)
		return
	}
	if !found {
		return
	}
	if err := applyPersistedPlayerData(p, data); err != nil {
		slog.Warn("ignored invalid player data", "uuid", p.UUID, "err", err)
		return
	}
	debuglog.Info(debuglog.WorldLoading, "loaded player data", "uuid", p.UUID, "position", p.Position)
}

func (s *Server) savePlayerData(p *player.Player) {
	if s == nil || s.playerStore == nil || p == nil {
		return
	}
	if err := s.playerStore.save(p.UUID, snapshotPlayerData(p)); err != nil {
		slog.Warn("could not save player data", "uuid", p.UUID, "err", err)
	}
}

func (s *Server) saveAllPlayerData() {
	if s == nil || s.playerStore == nil || s.game == nil {
		return
	}
	s.game.OnlinePlayers(func(p *player.Player) {
		s.savePlayerData(p)
	})
}
