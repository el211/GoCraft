package world

// Registry-backed Java protocol IDs used outside terrain encoding.
var (
	soundEventIDs map[string]int32
	mobEffectIDs  map[string]int32
)

// SoundEventID returns the protocol ID of a 1.21.4 sound event, or -1 when the
// embedded registry does not contain name.
func SoundEventID(name string) int32 {
	id, ok := soundEventIDs[name]
	if !ok {
		return -1
	}
	return id
}

// MobEffectID returns the protocol ID of a 1.21.4 mob effect, or -1 when the
// embedded registry does not contain name.
func MobEffectID(name string) int32 {
	id, ok := mobEffectIDs[name]
	if !ok {
		return -1
	}
	return id
}

// MobEffectNames returns sorted, namespace-free names suitable for command
// literals and tab completion.
func MobEffectNames() []string {
	names := make([]string, 0, len(mobEffectIDs))
	for name := range mobEffectIDs {
		if len(name) > len("minecraft:") && name[:len("minecraft:")] == "minecraft:" {
			name = name[len("minecraft:"):]
		}
		names = append(names, name)
	}
	sortStrings(names)
	return names
}
