package handler

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
)

type whitelistFile struct {
	Enabled bool     `json:"enabled"`
	Players []string `json:"players"`
}

var whitelistRegistry = struct {
	sync.RWMutex
	path    string
	enabled bool
	names   map[string]string
}{names: make(map[string]string)}

// ConfigureWhitelist loads the persisted shared whitelist and merges players
// bootstrapped through server.yml. A persisted enabled flag takes precedence.
func ConfigureWhitelist(path string, enabled bool, configured []string) error {
	whitelistRegistry.Lock()
	defer whitelistRegistry.Unlock()
	whitelistRegistry.path = path
	whitelistRegistry.enabled = enabled
	whitelistRegistry.names = make(map[string]string)
	var loadErr error
	data, err := os.ReadFile(path)
	if err == nil {
		var state whitelistFile
		if err = json.Unmarshal(data, &state); err != nil {
			loadErr = err
		} else {
			whitelistRegistry.enabled = state.Enabled
			for _, name := range state.Players {
				addWhitelistNameLocked(name)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		loadErr = err
	}
	for _, name := range configured {
		addWhitelistNameLocked(name)
	}
	return loadErr
}

func IsWhitelisted(name string) bool {
	whitelistRegistry.RLock()
	enabled := whitelistRegistry.enabled
	_, listed := whitelistRegistry.names[strings.ToLower(strings.TrimSpace(name))]
	whitelistRegistry.RUnlock()
	return !enabled || listed
}

func WhitelistEnabled() bool {
	whitelistRegistry.RLock()
	enabled := whitelistRegistry.enabled
	whitelistRegistry.RUnlock()
	return enabled
}

func SetWhitelistEnabled(enabled bool) error {
	whitelistRegistry.Lock()
	defer whitelistRegistry.Unlock()
	whitelistRegistry.enabled = enabled
	return saveWhitelistLocked()
}

func AddWhitelistedPlayer(name string) error {
	whitelistRegistry.Lock()
	defer whitelistRegistry.Unlock()
	if !addWhitelistNameLocked(name) {
		return nil
	}
	return saveWhitelistLocked()
}

func RemoveWhitelistedPlayer(name string) (bool, error) {
	whitelistRegistry.Lock()
	defer whitelistRegistry.Unlock()
	key := strings.ToLower(strings.TrimSpace(name))
	if _, ok := whitelistRegistry.names[key]; !ok {
		return false, nil
	}
	delete(whitelistRegistry.names, key)
	return true, saveWhitelistLocked()
}

func WhitelistedPlayers() []string {
	whitelistRegistry.RLock()
	players := make([]string, 0, len(whitelistRegistry.names))
	for _, name := range whitelistRegistry.names {
		players = append(players, name)
	}
	whitelistRegistry.RUnlock()
	sort.Slice(players, func(i, j int) bool { return strings.ToLower(players[i]) < strings.ToLower(players[j]) })
	return players
}

func addWhitelistNameLocked(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	key := strings.ToLower(name)
	if _, exists := whitelistRegistry.names[key]; exists {
		return false
	}
	whitelistRegistry.names[key] = name
	return true
}

func saveWhitelistLocked() error {
	if whitelistRegistry.path == "" {
		return nil
	}
	players := make([]string, 0, len(whitelistRegistry.names))
	for _, name := range whitelistRegistry.names {
		players = append(players, name)
	}
	sort.Slice(players, func(i, j int) bool { return strings.ToLower(players[i]) < strings.ToLower(players[j]) })
	data, err := json.MarshalIndent(whitelistFile{Enabled: whitelistRegistry.enabled, Players: players}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(whitelistRegistry.path, append(data, '\n'), 0o644)
}
