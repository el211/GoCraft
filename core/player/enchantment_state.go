package player

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type EnchantmentLevel struct {
	ID    string
	Level int
}

func DecodeEnchantments(encoded string) ([]EnchantmentLevel, error) {
	if encoded == "" {
		return nil, nil
	}
	values := make([]EnchantmentLevel, 0, strings.Count(encoded, ";")+1)
	for _, entry := range strings.Split(encoded, ";") {
		parts := strings.SplitN(entry, "=", 2)
		level, err := strconv.Atoi(parts[len(parts)-1])
		if len(parts) != 2 || parts[0] == "" || !strings.Contains(parts[0], ":") || err != nil || level < 1 || level > 255 {
			return nil, fmt.Errorf("invalid enchantment component %q", entry)
		}
		values = append(values, EnchantmentLevel{ID: parts[0], Level: level})
	}
	return values, nil
}

// EncodeEnchantments serialises a list of enchantment levels into the compact
// "id=level;…" string used by ItemStack.Enchantments. The second return value
// is always nil and exists only so callers can use the multi-return form.
func EncodeEnchantments(values []EnchantmentLevel) (string, error) {
	return encodeEnchantments(values), nil
}

func encodeEnchantments(values []EnchantmentLevel) string {
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = value.ID + "=" + strconv.Itoa(value.Level)
	}
	return strings.Join(parts, ";")
}

func (s ItemStack) EnchantmentLevels() []EnchantmentLevel {
	values, _ := DecodeEnchantments(s.Enchantments)
	return values
}

func (s ItemStack) EnchantmentLevel(id string) int {
	if !strings.Contains(id, ":") {
		id = "minecraft:" + id
	}
	for _, value := range s.EnchantmentLevels() {
		if value.ID == id {
			return value.Level
		}
	}
	return 0
}

// Enchant mirrors Pumpkin: levels are capped at 255 and an existing level is
// only replaced when the requested level is higher.
func (s *ItemStack) Enchant(id string, level int) bool {
	if s == nil || s.IsEmpty() || level <= 0 {
		return false
	}
	if !strings.Contains(id, ":") {
		id = "minecraft:" + id
	}
	level = min(level, 255)
	values := s.EnchantmentLevels()
	for index, value := range values {
		if value.ID == id {
			if value.Level >= level {
				return false
			}
			values[index].Level = level
			s.Enchantments = encodeEnchantments(values)
			return true
		}
	}
	values = append(values, EnchantmentLevel{ID: id, Level: level})
	s.Enchantments = encodeEnchantments(values)
	return true
}
