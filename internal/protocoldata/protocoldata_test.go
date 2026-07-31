package protocoldata_test

// Validation tests for the embedded protocol-data packet tables.
//
// Each test targets a distinct invariant:
//
//   TestVersionMetadata        — _gocraft_version present and matches expected
//   TestScopeMetadataPresent   — _scope is present and non-empty
//   TestPacketNameFormat        — all names start with "minecraft:" and contain no whitespace
//   TestBothDirectionsPresent   — every state file has both clientbound and serverbound maps
//   TestNoDuplicateIDs          — no two names share a numeric ID in the same direction
//   TestIDRange                 — all IDs within [0, maxPacketID]
//   TestReferencedPacketsResolve— every packet used by GoCraft resolves through MustCB/MustSB
//
// Running `go test ./internal/protocoldata/...` catches all of the above.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"GoCraft/internal/protocoldata"
)

// maxPacketID is the largest packet ID this test will accept.
// In Java Edition 1.21.4 no play-state direction exceeds ~130 packets,
// so IDs ≥ 256 always indicate a data-entry error.
const maxPacketID = 255

// expectedVersion must match the _gocraft_version field in every JSON file.
const expectedVersion = "1.21.4"

// stateFileRaw is used for test-level introspection — we re-parse the embedded
// JSON so we can iterate every entry individually.
type stateFileRaw struct {
	GoCraftVersion string           `json:"_gocraft_version"`
	Scope          string           `json:"_scope"`
	Clientbound    map[string]int32 `json:"clientbound"`
	Serverbound    map[string]int32 `json:"serverbound"`
}

// allStates lists every protocol state that has an embedded JSON file.
var allStates = []string{"play", "configuration", "login", "status", "handshake"}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestVersionMetadata verifies that every file declares the expected version
// string, catching accidental use of a wrong-version data file.
func TestVersionMetadata(t *testing.T) {
	for _, state := range allStates {
		state := state
		t.Run(state, func(t *testing.T) {
			sf := mustParseStateFile(t, state)
			if sf.GoCraftVersion == "" {
				t.Errorf("state %q: missing _gocraft_version key", state)
			} else if sf.GoCraftVersion != expectedVersion {
				t.Errorf("state %q: _gocraft_version is %q, want %q",
					state, sf.GoCraftVersion, expectedVersion)
			}
		})
	}
}

// TestScopeMetadataPresent ensures every file carries a non-empty _scope key
// so it is unambiguous that the tables are partial, not complete protocol specs.
func TestScopeMetadataPresent(t *testing.T) {
	for _, state := range allStates {
		state := state
		t.Run(state, func(t *testing.T) {
			sf := mustParseStateFile(t, state)
			if sf.Scope == "" {
				t.Errorf("state %q: missing or empty _scope — add a human-readable coverage note", state)
			}
		})
	}
}

// TestPacketNameFormat checks that every semantic packet name:
//   - starts with "minecraft:" (namespace required)
//   - contains no ASCII whitespace (a common copy-paste mistake)
func TestPacketNameFormat(t *testing.T) {
	const prefix = "minecraft:"
	for _, state := range allStates {
		state := state
		t.Run(state, func(t *testing.T) {
			sf := mustParseStateFile(t, state)
			checkNameFormat(t, state, "clientbound", sf.Clientbound, prefix)
			checkNameFormat(t, state, "serverbound", sf.Serverbound, prefix)
		})
	}
}

// TestBothDirectionsPresent verifies that every state file has both
// "clientbound" and "serverbound" top-level maps, even if one is empty
// (e.g. handshake has no clientbound packets).  A missing key would silently
// leave one direction unresolvable.
func TestBothDirectionsPresent(t *testing.T) {
	for _, state := range allStates {
		state := state
		t.Run(state, func(t *testing.T) {
			sf := mustParseStateFile(t, state)
			if sf.Clientbound == nil {
				t.Errorf("state %q: missing \"clientbound\" map (use {} for empty)", state)
			}
			if sf.Serverbound == nil {
				t.Errorf("state %q: missing \"serverbound\" map (use {} for empty)", state)
			}
		})
	}
}

// TestNoDuplicateIDs checks that no two semantic names map to the same numeric
// ID within the same state+direction, which would silently route packets to the
// wrong handler.
func TestNoDuplicateIDs(t *testing.T) {
	for _, state := range allStates {
		state := state
		t.Run(state, func(t *testing.T) {
			sf := mustParseStateFile(t, state)
			checkNoDuplicateIDs(t, state, "clientbound", sf.Clientbound)
			checkNoDuplicateIDs(t, state, "serverbound", sf.Serverbound)
		})
	}
}

// TestIDRange verifies every packet ID is within [0, maxPacketID].
func TestIDRange(t *testing.T) {
	for _, state := range allStates {
		state := state
		t.Run(state, func(t *testing.T) {
			sf := mustParseStateFile(t, state)
			checkIDRange(t, state, "clientbound", sf.Clientbound)
			checkIDRange(t, state, "serverbound", sf.Serverbound)
		})
	}
}

// TestReferencedPacketsResolve verifies that every packet name referenced by
// GoCraft's handler layer (packets.go) resolves through MustCB / MustSB.
// A missing JSON entry surfaces as a panic+FAIL here instead of only at server
// startup, making CI the gate rather than a production incident.
func TestReferencedPacketsResolve(t *testing.T) {
	type entry struct{ state, dir, name string }
	refs := []entry{
		// Handshake
		{"handshake", "serverbound", "minecraft:intention"},
		// Status
		{"status", "serverbound", "minecraft:status_request"},
		{"status", "clientbound", "minecraft:status_response"},
		{"status", "serverbound", "minecraft:ping_request"},
		{"status", "clientbound", "minecraft:pong_response"},
		// Login
		{"login", "serverbound", "minecraft:hello"},
		{"login", "serverbound", "minecraft:key"},
		{"login", "serverbound", "minecraft:login_acknowledged"},
		{"login", "clientbound", "minecraft:login_disconnect"},
		{"login", "clientbound", "minecraft:hello"},
		{"login", "clientbound", "minecraft:game_profile"},
		// Configuration
		{"configuration", "serverbound", "minecraft:client_information"},
		{"configuration", "serverbound", "minecraft:select_known_packs"},
		{"configuration", "serverbound", "minecraft:finish_configuration"},
		{"configuration", "clientbound", "minecraft:custom_payload"},
		{"configuration", "clientbound", "minecraft:finish_configuration"},
		{"configuration", "clientbound", "minecraft:registry_data"},
		{"configuration", "clientbound", "minecraft:update_enabled_features"},
		{"configuration", "clientbound", "minecraft:update_tags"},
		{"configuration", "clientbound", "minecraft:select_known_packs"},
		// Play — clientbound
		{"play", "clientbound", "minecraft:login"},
		{"play", "clientbound", "minecraft:player_abilities"},
		{"play", "clientbound", "minecraft:player_info_update"},
		{"play", "clientbound", "minecraft:player_info_remove"},
		{"play", "clientbound", "minecraft:player_position"},
		{"play", "clientbound", "minecraft:game_event"},
		{"play", "clientbound", "minecraft:set_chunk_cache_center"},
		{"play", "clientbound", "minecraft:set_default_spawn_position"},
		{"play", "clientbound", "minecraft:set_entity_data"},
		{"play", "clientbound", "minecraft:open_screen"},
		{"play", "clientbound", "minecraft:hurt_animation"},
		{"play", "clientbound", "minecraft:merchant_offers"},
		{"play", "clientbound", "minecraft:recipe_book_add"},
		{"play", "clientbound", "minecraft:set_entity_motion"},
		{"play", "clientbound", "minecraft:sound_entity"},
		{"play", "clientbound", "minecraft:sound"},
		{"play", "clientbound", "minecraft:update_mob_effect"},
		{"play", "clientbound", "minecraft:update_recipes"},
		{"play", "clientbound", "minecraft:keep_alive"},
		{"play", "clientbound", "minecraft:forget_level_chunk"},
		{"play", "clientbound", "minecraft:set_time"},
		{"play", "clientbound", "minecraft:system_chat"},
		{"play", "clientbound", "minecraft:spawn_entity"},
		{"play", "clientbound", "minecraft:remove_entities"},
		{"play", "clientbound", "minecraft:rotate_head"},
		{"play", "clientbound", "minecraft:teleport_entity"},
		{"play", "clientbound", "minecraft:block_update"},
		{"play", "clientbound", "minecraft:acknowledge_block_change"},
		{"play", "clientbound", "minecraft:set_container_content"},
		{"play", "clientbound", "minecraft:set_held_slot"},
		{"play", "clientbound", "minecraft:commands"},
		{"play", "clientbound", "minecraft:disconnect"},
		{"play", "clientbound", "minecraft:level_chunk_with_light"},
		// Play — serverbound
		{"play", "serverbound", "minecraft:accept_teleportation"},
		{"play", "serverbound", "minecraft:keep_alive"},
		{"play", "serverbound", "minecraft:move_player_pos"},
		{"play", "serverbound", "minecraft:move_player_pos_rot"},
		{"play", "serverbound", "minecraft:move_player_rot"},
		{"play", "serverbound", "minecraft:move_player_status_only"},
		{"play", "serverbound", "minecraft:player_abilities"},
		{"play", "serverbound", "minecraft:place_recipe"},
		{"play", "serverbound", "minecraft:chat_command"},
		{"play", "serverbound", "minecraft:chat"},
		{"play", "serverbound", "minecraft:interact"},
		{"play", "serverbound", "minecraft:player_action"},
		{"play", "serverbound", "minecraft:set_carried_item"},
		{"play", "serverbound", "minecraft:set_creative_mode_slot"},
		{"play", "serverbound", "minecraft:use_item_on"},
	}

	for _, r := range refs {
		r := r
		t.Run(fmt.Sprintf("%s/%s/%s", r.state, r.dir, r.name), func(t *testing.T) {
			var id int32
			var panicked bool
			func() {
				defer func() {
					if rv := recover(); rv != nil {
						panicked = true
						t.Errorf("panicked for %s/%s/%q: %v", r.state, r.dir, r.name, rv)
					}
				}()
				if r.dir == "clientbound" {
					id = protocoldata.MustCB(r.state, r.name)
				} else {
					id = protocoldata.MustSB(r.state, r.name)
				}
			}()
			if !panicked && (id < 0 || id > maxPacketID) {
				t.Errorf("%s/%s/%q: resolved ID %d outside valid range [0, %d]",
					r.state, r.dir, r.name, id, maxPacketID)
			}
		})
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// TestProtocol769PlayPacketIDs pins every implemented Play packet to the
// protocol-769 mapper in PrismarineJS minecraft-data data/pc/1.21.4/protocol.json.
// In particular, level_chunk_with_light is 0x28; 0x27 is keep_alive.
func TestProtocol769PlayPacketIDs(t *testing.T) {
	tests := []struct {
		direction string
		name      string
		want      int32
	}{
		{"clientbound", "minecraft:bundle_delimiter", 0x00},
		{"clientbound", "minecraft:spawn_entity", 0x01},
		{"clientbound", "minecraft:acknowledge_block_change", 0x05},
		{"clientbound", "minecraft:block_update", 0x09},
		{"clientbound", "minecraft:commands", 0x11},
		{"clientbound", "minecraft:set_container_content", 0x13},
		{"clientbound", "minecraft:disconnect", 0x1d},
		{"clientbound", "minecraft:forget_level_chunk", 0x22},
		{"clientbound", "minecraft:game_event", 0x23},
		{"clientbound", "minecraft:keep_alive", 0x27},
		{"clientbound", "minecraft:level_chunk_with_light", 0x28},
		{"clientbound", "minecraft:login", 0x2c},
		{"clientbound", "minecraft:hurt_animation", 0x25},
		{"clientbound", "minecraft:merchant_offers", 0x2e},
		{"clientbound", "minecraft:open_screen", 0x35},
		{"clientbound", "minecraft:open_sign_editor", 0x36},
		{"clientbound", "minecraft:player_abilities", 0x3a},
		{"clientbound", "minecraft:player_info_remove", 0x3f},
		{"clientbound", "minecraft:player_info_update", 0x40},
		{"clientbound", "minecraft:player_position", 0x42},
		{"clientbound", "minecraft:recipe_book_add", 0x44},
		{"clientbound", "minecraft:remove_entities", 0x47},
		{"clientbound", "minecraft:rotate_head", 0x4d},
		{"clientbound", "minecraft:set_chunk_cache_center", 0x58},
		{"clientbound", "minecraft:set_default_spawn_position", 0x5b},
		{"clientbound", "minecraft:set_entity_data", 0x5d},
		{"clientbound", "minecraft:set_entity_motion", 0x5f},
		{"clientbound", "minecraft:set_held_slot", 0x63},
		{"clientbound", "minecraft:set_time", 0x6b},
		{"clientbound", "minecraft:sound_entity", 0x6e},
		{"clientbound", "minecraft:sound", 0x6f},
		{"clientbound", "minecraft:system_chat", 0x73},
		{"clientbound", "minecraft:teleport_entity", 0x77},
		{"clientbound", "minecraft:update_mob_effect", 0x7d},
		{"clientbound", "minecraft:update_recipes", 0x7e},
		{"serverbound", "minecraft:accept_teleportation", 0x00},
		{"serverbound", "minecraft:chat_command", 0x05},
		{"serverbound", "minecraft:chat", 0x07},
		{"serverbound", "minecraft:interact", 0x18},
		{"serverbound", "minecraft:keep_alive", 0x1a},
		{"serverbound", "minecraft:move_player_pos", 0x1c},
		{"serverbound", "minecraft:move_player_pos_rot", 0x1d},
		{"serverbound", "minecraft:move_player_rot", 0x1e},
		{"serverbound", "minecraft:move_player_status_only", 0x1f},
		{"serverbound", "minecraft:place_recipe", 0x25},
		{"serverbound", "minecraft:player_abilities", 0x26},
		{"serverbound", "minecraft:player_action", 0x27},
		{"serverbound", "minecraft:set_carried_item", 0x33},
		{"serverbound", "minecraft:set_creative_mode_slot", 0x36},
		{"serverbound", "minecraft:use_item_on", 0x3c},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(fmt.Sprintf("%s/%s", tc.direction, tc.name), func(t *testing.T) {
			var got int32
			if tc.direction == "clientbound" {
				got = protocoldata.MustCB("play", tc.name)
			} else {
				got = protocoldata.MustSB("play", tc.name)
			}
			if got != tc.want {
				t.Fatalf("protocol 769 packet ID = 0x%02x, want 0x%02x", got, tc.want)
			}
		})
	}
}

func mustParseStateFile(t *testing.T, state string) stateFileRaw {
	t.Helper()
	path := fmt.Sprintf("java/1.21.4/%s.json", state)
	data, err := protocoldata.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read embedded file %q: %v", path, err)
	}
	var sf stateFileRaw
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("cannot parse %q: %v", path, err)
	}
	return sf
}

func checkNameFormat(t *testing.T, state, dir string, entries map[string]int32, prefix string) {
	t.Helper()
	for name := range entries {
		if !strings.HasPrefix(name, prefix) {
			t.Errorf("%s/%s: name %q must start with %q", state, dir, name, prefix)
		}
		if strings.ContainsAny(name, " \t\n\r") {
			t.Errorf("%s/%s: name %q contains whitespace", state, dir, name)
		}
	}
}

func checkNoDuplicateIDs(t *testing.T, state, dir string, entries map[string]int32) {
	t.Helper()
	seen := make(map[int32]string, len(entries))
	for name, id := range entries {
		if prev, exists := seen[id]; exists {
			t.Errorf("%s/%s: duplicate ID %d — %q and %q both map to it",
				state, dir, id, prev, name)
		}
		seen[id] = name
	}
}

func checkIDRange(t *testing.T, state, dir string, entries map[string]int32) {
	t.Helper()
	for name, id := range entries {
		if id < 0 || id > maxPacketID {
			t.Errorf("%s/%s/%q: ID %d outside valid range [0, %d]",
				state, dir, name, id, maxPacketID)
		}
	}
}
