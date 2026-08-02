package handler

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
)

// operatorRecord deliberately follows the useful portion of vanilla's
// ops.json shape so server owners can inspect and edit it easily.
type operatorRecord struct {
	Name string
}

var operatorRegistry = struct {
	sync.RWMutex
	path  string
	names map[string]operatorRecord
}{names: make(map[string]operatorRecord)}

// ConfigureOperators loads persisted operators and merges names explicitly
// configured in server.yml. The configured list is useful for bootstrapping a
// new server before /op can be run in game.
func ConfigureOperators(path string, configured []string) error {
	operatorRegistry.Lock()
	defer operatorRegistry.Unlock()

	operatorRegistry.path = path
	operatorRegistry.names = make(map[string]operatorRecord)
	var loadErr error
	data, err := os.ReadFile(path)
	if err == nil {
		var records []map[string]any
		if err = json.Unmarshal(data, &records); err != nil {
			loadErr = err
		} else {
			for _, record := range records {
				name, _ := record[`name`].(string)
				if name == `` {
					name, _ = record[`Name`].(string)
				}
				addOperatorLocked(name)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		loadErr = err
	}
	for _, name := range configured {
		addOperatorLocked(name)
	}
	return loadErr
}

// IsOperatorName reports whether a name is present in the operator registry.
func IsOperatorName(name string) bool {
	operatorRegistry.RLock()
	_, ok := operatorRegistry.names[strings.ToLower(strings.TrimSpace(name))]
	operatorRegistry.RUnlock()
	return ok
}

// OperatorCount returns the number of persisted operators.
func OperatorCount() int {
	operatorRegistry.RLock()
	count := len(operatorRegistry.names)
	operatorRegistry.RUnlock()
	return count
}

// SetOperator promotes a name and persists the updated ops.json file.
func SetOperator(name string) error {
	operatorRegistry.Lock()
	defer operatorRegistry.Unlock()
	if !addOperatorLocked(name) {
		return nil
	}
	return saveOperatorsLocked()
}

func addOperatorLocked(name string) bool {
	name = strings.TrimSpace(name)
	if name == `` {
		return false
	}
	key := strings.ToLower(name)
	if _, exists := operatorRegistry.names[key]; exists {
		return false
	}
	operatorRegistry.names[key] = operatorRecord{Name: name}
	return true
}

func saveOperatorsLocked() error {
	if operatorRegistry.path == `` {
		return nil
	}
	records := make([]operatorRecord, 0, len(operatorRegistry.names))
	for _, record := range operatorRegistry.names {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return strings.ToLower(records[i].Name) < strings.ToLower(records[j].Name)
	})
	persisted := make([]map[string]any, 0, len(records))
	for _, record := range records {
		persisted = append(persisted, map[string]any{
			`name`:                record.Name,
			`level`:               4,
			`bypassesPlayerLimit`: false,
		})
	}
	data, err := json.MarshalIndent(persisted, ``, `  `)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(operatorRegistry.path, data, 0o644)
}
