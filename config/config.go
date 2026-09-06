// Package config handles loading and saving server configuration from YAML.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// BedrockConfig holds configuration for the Bedrock Edition UDP listener.
type BedrockConfig struct {
	// Enabled controls whether the Bedrock listener is started at all.
	// Set to false to run a Java-only server.
	Enabled bool `yaml:"enabled"`

	// Address is the UDP listen address for RakNet connections.
	// GoCraft defaults to the deployment's requested Bedrock UDP port 19106.
	Address string `yaml:"address"`

	// OnlineMode requires connecting players to authenticate with Xbox Live.
	// Disable only for LAN testing; unauthenticated XUIDs are NOT treated as
	// trusted global identities.
	OnlineMode bool `yaml:"online_mode"`
}

const (
	WorldStorageDisk   = "disk"
	WorldStorageMemory = "memory"
)

// CombatConfig controls Java combat timing and knockback. Disabling the attack
// cooldown with the default values provides a legacy 1.8-style feel.
type CombatConfig struct {
	AttackCooldown      bool    `yaml:"attack_cooldown"`
	KnockbackHorizontal float64 `yaml:"knockback_horizontal"`
	KnockbackVertical   float64 `yaml:"knockback_vertical"`
}

// ItemTooltipConfig controls the extra information GoCraft adds to item
// tooltips. Vanilla attributes can be hidden independently because a legacy
// no-cooldown attack-speed override would otherwise appear as an ugly 1024.
type ItemTooltipConfig struct {
	ShowDurability        bool `yaml:"show_durability"`
	ShowAttributes        bool `yaml:"show_attributes"`
	HideVanillaAttributes bool `yaml:"hide_vanilla_attributes"`
}

type ClearLagTargets struct {
	DroppedItems   bool `yaml:"dropped_items"`
	ExperienceOrbs bool `yaml:"experience_orbs"`
	Projectiles    bool `yaml:"projectiles"`
	PrimedTNT      bool `yaml:"primed_tnt"`
	FallingBlocks  bool `yaml:"falling_blocks"`
	Boats          bool `yaml:"boats"`
	PassiveMobs    bool `yaml:"passive_mobs"`
	HostileMobs    bool `yaml:"hostile_mobs"`
}

type ClearLagConfig struct {
	Enabled                 bool            `yaml:"enabled"`
	IntervalSeconds         int             `yaml:"interval_seconds"`
	MinimumEntityAgeSeconds int             `yaml:"minimum_entity_age_seconds"`
	WarningSeconds          []int           `yaml:"warning_seconds"`
	WarningMessage          string          `yaml:"warning_message"`
	CompleteMessage         string          `yaml:"complete_message"`
	Remove                  ClearLagTargets `yaml:"remove"`
}

// WhitelistConfig controls the shared Java/Bedrock player whitelist. Names are
// compared case-insensitively and persisted to whitelist.json at runtime.
type WhitelistConfig struct {
	Enabled bool     `yaml:"enabled"`
	Players []string `yaml:"players"`
}

// JavaResourcePackConfig controls the resource pack pushed to Java clients.
// Java and Bedrock use different pack formats — configure each separately.
type JavaResourcePackConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`    // HTTPS URL to the .zip resource pack
	Hash    string `yaml:"hash"`   // SHA-1 hex of the zip (for client caching)
	Forced  bool   `yaml:"forced"` // kick the player if they decline
	Prompt  string `yaml:"prompt"` // MiniMessage text shown before accept dialog
}

// BedrockResourcePackConfig controls the resource packs pushed to Bedrock clients.
// Paths may point to .mcpack, .zip, or .mcaddon files.
// .mcaddon files are automatically unpacked — each contained resource pack and
// behavior pack sub-folder is loaded and sent to connecting clients.
type BedrockResourcePackConfig struct {
	Enabled bool     `yaml:"enabled"`
	Paths   []string `yaml:"paths"`  // local paths to .mcpack, .zip, or .mcaddon files
	Forced  bool     `yaml:"forced"` // kick the player if they decline
}

// ResourcePackConfig bundles resource pack settings for both editions.
type ResourcePackConfig struct {
	Java    JavaResourcePackConfig    `yaml:"java"`
	Bedrock BedrockResourcePackConfig `yaml:"bedrock"`
}

// CustomItemsConfig controls GoCraft's cross-edition custom item system.
// Place item packs in sub-directories under PacksDir (default: "packs/").
// Each pack directory must contain an items.yml and a textures/ folder.
type CustomItemsConfig struct {
	Enabled bool `yaml:"enabled"`

	// PacksDir is the directory that contains individual pack sub-directories.
	PacksDir string `yaml:"packs_dir"`

	// Java controls how the auto-generated Java resource pack is delivered.
	Java struct {
		// ServePort is the port the embedded HTTP server listens on.
		// Java clients download the generated pack from this port.
		ServePort int `yaml:"serve_port"`

		// PublicHost is the hostname or IP address that Java clients use to
		// reach this server (e.g. your server's public IP or domain).
		// Leave empty for local/LAN testing only.
		PublicHost string `yaml:"public_host"`
	} `yaml:"java"`
}

// PermissionEditorConfig controls the bytebin-based permission editor.
// The server uploads permission data to bytebin (outbound only — no listening
// port needed), and the editor runs as a static GitHub Pages site.
type PermissionEditorConfig struct {
	Enabled    bool   `yaml:"enabled"`
	EditorURL  string `yaml:"editor_url"`
	BytebinURL string `yaml:"bytebin_url"`
}

// PluginsConfig controls the plugin subsystem. Bundles are scanned and loaded
// before the listeners open, so a slow or failing plugin delays the port rather
// than letting players join a partially configured server.
type PluginsConfig struct {
	// Enabled controls whether the plugins directory is scanned at all.
	Enabled bool `yaml:"enabled"`

	// Directory holds the .gcpkg bundles to load, relative to the working
	// directory. A missing directory is not an error.
	Directory string `yaml:"directory"`

	// EventBudgetMillis bounds how long one cancellable event may spend across
	// all of its subscribers before the host stops waiting for verdicts.
	EventBudgetMillis int `yaml:"event_budget_ms"`

	// ColdEventGraceMillis is added to that budget the first time a subscriber
	// sees an event type.
	//
	// The first dispatch into a runtime that has never run a plugin's handler
	// costs milliseconds where every one after it costs microseconds: classes
	// are initialised and code runs interpreted before anything compiles it. The
	// host warms what it owns before opening its listeners; what is left is the
	// author's code, which a warm-up must not run against values nobody sent.
	//
	// Without this the healthiest server logs one deadline exceeded per restart,
	// and a fail_closed event refuses the first action after every boot. Spent
	// once per subscriber per event type, so it cannot become a way to hold the
	// tick. Zero turns it off.
	ColdEventGraceMillis int `yaml:"cold_event_grace_ms"`

	// Runtimes configures the language backends. Nothing here is consulted
	// unless an installed plugin declares that runtime: a server with no Java
	// plugin never looks for java and never provisions anything.
	Runtimes RuntimesConfig `yaml:"runtimes"`
}

// RuntimesConfig holds one section per language backend, keyed by the name a
// plugin manifest writes in its runtime field.
type RuntimesConfig struct {
	JVM JVMRuntimeConfig `yaml:"jvm"`
}

// JVMRuntimeConfig configures the Java backend.
//
// The host parses this, never the JVM: a runtime that read its own YAML would
// leave the host unable to report a bad value with a file and a line, and would
// not help Lua or Go plugins at all.
type JVMRuntimeConfig struct {
	// PreferSystem keeps an installed JDK that meets the baseline instead of
	// downloading a second copy. Turning it off is an admin asking for the
	// pinned build specifically, so detection is then skipped rather than used
	// as a fallback.
	PreferSystem bool `yaml:"prefer_system"`

	// JavaPath forces one java binary and outranks JAVA_HOME and PATH.
	JavaPath string `yaml:"java_path"`

	// JarPath runs a runtime jar from disk instead of the embedded one. It is
	// how a developer works against a local gocraft-java checkout, and the only
	// way to run Java plugins in a build that carries no embedded jar.
	JarPath string `yaml:"jar_path"`
}

// DebugConfig controls verbose diagnostic log categories. All switches default
// to false so routine console and latest.log output remain concise.
type DebugConfig struct {
	StartupRegistry      bool `yaml:"startup_registry"`
	EnvironmentOverrides bool `yaml:"environment_overrides"`
	WorldLoading         bool `yaml:"world_loading"`
	MobSpawning          bool `yaml:"mob_spawning"`
	Autosaves            bool `yaml:"autosaves"`
	EntityEvents         bool `yaml:"entity_events"`
	EntityTickOverruns   bool `yaml:"entity_tick_overruns"`
	BedrockCatalogues    bool `yaml:"bedrock_catalogues"`
	BedrockLogin         bool `yaml:"bedrock_login"`
	BedrockChunks        bool `yaml:"bedrock_chunks"`
	BedrockInventory     bool `yaml:"bedrock_inventory"`
	Profiling            bool `yaml:"profiling"`
}

// MobSpawningConfig controls the natural-mob caps for one complete 17x17
// chunk simulation area. Smaller loaded areas scale these values down, and 0
// disables natural spawning for that category.
type MobSpawningConfig struct {
	Hostile                   int `yaml:"hostile"`
	Passive                   int `yaml:"passive"`
	Ambient                   int `yaml:"ambient"`
	Axolotls                  int `yaml:"axolotls"`
	UndergroundWaterCreatures int `yaml:"underground_water_creatures"`
	WaterCreatures            int `yaml:"water_creatures"`
	WaterAmbient              int `yaml:"water_ambient"`
}

// Config holds all server configuration values.
type Config struct {
	// JavaEnabled controls whether the Java Edition TCP listener is started.
	// Set to false to run a Bedrock-only server.
	JavaEnabled bool `yaml:"java_enabled"`

	// Network (Java Edition)
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// Identity
	MOTD            string `yaml:"motd"`
	ServerIcon      string `yaml:"server_icon"`
	MaxPlayers      int    `yaml:"max_players"`
	VersionName     string `yaml:"version_name"`
	ProtocolVersion int    `yaml:"protocol_version"`

	// Behaviour (Java Edition)
	OnlineMode bool `yaml:"online_mode"`

	// Villagers controls whether villager entities are spawned in villages.
	// Set to false to disable NPC spawning (structures still generate).
	Villagers bool `yaml:"villagers"`

	// WorldStorage selects "disk" (Anvil persistence plus an in-memory cache)
	// or "memory" (no disk reads or writes).
	WorldStorage string `yaml:"world_storage"`

	// WorldDir is the Anvil world folder used when WorldStorage is "disk".
	WorldDir string `yaml:"world_dir"`

	// WorldSeed controls deterministic overworld terrain generation for chunks
	// absent from Anvil storage. The same seed always produces the same terrain.
	WorldSeed int64 `yaml:"world_seed"`

	// ViewDistance is the radius, in chunks, sent to Java clients. PreGenerateRadius
	// warms a larger square in the background so movement rarely waits on worldgen.
	ViewDistance      int `yaml:"view_distance"`
	PreGenerateRadius int `yaml:"pregenerate_radius"`

	// MaxCachedChunks bounds clean chunks retained in RAM. Dirty memory-mode
	// chunks remain pinned so unsaved changes are never silently discarded.
	MaxCachedChunks int `yaml:"max_cached_chunks"`

	// Difficulty controls hostile-mob behaviour and spawning.
	// Valid values: peaceful, easy, normal, hard.  Default: normal.
	// On peaceful no hostile mobs are spawned and existing ones are removed.
	Difficulty string `yaml:"difficulty"`

	DefaultGameMode string            `yaml:"default_gamemode"`
	MobSpawning     MobSpawningConfig `yaml:"mob_spawning"`

	// Operators bootstraps named operators. Runtime /op changes are persisted
	// separately in ops.json, matching the vanilla server convention.
	Operators        []string
	Whitelist        WhitelistConfig        `yaml:"whitelist"`
	ResourcePack     ResourcePackConfig     `yaml:"resource_pack"`
	CustomItems      CustomItemsConfig      `yaml:"custom_items"`
	PermissionEditor PermissionEditorConfig `yaml:"permission_editor"`
	Plugins          PluginsConfig          `yaml:"plugins"`
	Debug            DebugConfig            `yaml:"debug"`

	// Combat timing and knockback settings.
	Combat CombatConfig `yaml:"combat"`

	ItemTooltips ItemTooltipConfig `yaml:"item_tooltips"`

	ClearLag ClearLagConfig `yaml:"clear_lag"`

	// Bedrock Edition UDP listener settings.
	Bedrock BedrockConfig `yaml:"bedrock"`
}

// defaults returns a Config populated with sane out-of-the-box values.
func defaults() *Config {
	return &Config{
		JavaEnabled:       true,
		Host:              "0.0.0.0",
		Port:              25565,
		MOTD:              "A GoCraft Server",
		ServerIcon:        "server-icon.png",
		MaxPlayers:        20,
		VersionName:       "1.21.4",
		ProtocolVersion:   769, // Minecraft Java Edition 1.21.4
		OnlineMode:        false,
		Villagers:         true,
		WorldStorage:      WorldStorageDisk,
		WorldDir:          "world",
		ViewDistance:      8,
		PreGenerateRadius: 8,
		MaxCachedChunks:   256,
		Difficulty:        "normal",
		DefaultGameMode:   "survival",
		MobSpawning: MobSpawningConfig{
			Hostile:                   35,
			Passive:                   16,
			Ambient:                   15,
			Axolotls:                  5,
			UndergroundWaterCreatures: 5,
			WaterCreatures:            5,
			WaterAmbient:              20,
		},
		Whitelist: WhitelistConfig{Enabled: false, Players: []string{}},
		CustomItems: func() CustomItemsConfig {
			var ci CustomItemsConfig
			ci.Enabled = true
			ci.PacksDir = "packs"
			ci.Java.ServePort = 8080
			return ci
		}(),
		PermissionEditor: PermissionEditorConfig{
			Enabled:    true,
			EditorURL:  "https://el211.github.io/GoCraft/editor",
			BytebinURL: "https://bytebin.lucko.me",
		},
		Plugins: PluginsConfig{
			Enabled:              true,
			Directory:            "plugins",
			EventBudgetMillis:    2,
			ColdEventGraceMillis: 20,
			Runtimes: RuntimesConfig{
				JVM: JVMRuntimeConfig{PreferSystem: true},
			},
		},
		Combat: CombatConfig{
			AttackCooldown:      false,
			KnockbackHorizontal: 0.4,
			KnockbackVertical:   0.4,
		},
		ItemTooltips: ItemTooltipConfig{
			ShowDurability:        true,
			ShowAttributes:        true,
			HideVanillaAttributes: true,
		},
		ClearLag: ClearLagConfig{
			Enabled:                 false,
			IntervalSeconds:         300,
			MinimumEntityAgeSeconds: 30,
			WarningSeconds:          []int{60, 30, 10, 5, 4, 3, 2, 1},
			WarningMessage:          "[ClearLag] Removing old entities in {seconds}s",
			CompleteMessage:         "[ClearLag] Removed {count} old entities",
			Remove: ClearLagTargets{
				DroppedItems:   true,
				ExperienceOrbs: true,
				Projectiles:    true,
				PrimedTNT:      true,
				FallingBlocks:  true,
			},
		},
		Bedrock: BedrockConfig{
			Enabled:    false,
			Address:    "0.0.0.0:19106",
			OnlineMode: true,
		},
	}
}

// Load reads configuration from the YAML file at path.
// If the file does not exist, default values are written to path and returned.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err2 := save(path, cfg); err2 != nil {
			return nil, fmt.Errorf("config: writing defaults to %s: %w", path, err2)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: invalid values in %s: %w", path, err)
	}

	return cfg, nil
}

// Addr returns the "host:port" string suitable for net.Listen.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// validate returns an error if required fields are out of range.
func (c *Config) validate() error {
	if c.JavaEnabled {
		if c.Port < 1 || c.Port > 65535 {
			return fmt.Errorf("port %d is out of range 1-65535", c.Port)
		}
		if c.ProtocolVersion <= 0 {
			return errors.New("protocol_version must be > 0")
		}
	}
	c.WorldStorage = strings.ToLower(strings.TrimSpace(c.WorldStorage))
	switch c.WorldStorage {
	case WorldStorageDisk:
		if strings.TrimSpace(c.WorldDir) == "" {
			return errors.New("world_dir must not be empty when world_storage is disk")
		}
	case WorldStorageMemory:
	default:
		return fmt.Errorf("world_storage %q must be disk or memory", c.WorldStorage)
	}
	if c.MaxPlayers < 0 {
		return errors.New("max_players must be >= 0")
	}
	if c.ViewDistance < 2 || c.ViewDistance > 32 {
		return fmt.Errorf("view_distance %d is out of range 2-32", c.ViewDistance)
	}
	if c.PreGenerateRadius < c.ViewDistance || c.PreGenerateRadius > 64 {
		return fmt.Errorf("pregenerate_radius %d must be between view_distance (%d) and 64", c.PreGenerateRadius, c.ViewDistance)
	}
	if c.MaxCachedChunks < 128 || c.MaxCachedChunks > 65536 {
		return fmt.Errorf("max_cached_chunks %d must be between 128 and 65536", c.MaxCachedChunks)
	}
	c.Difficulty = strings.ToLower(strings.TrimSpace(c.Difficulty))
	switch c.Difficulty {
	case "peaceful", "easy", "normal", "hard":
	default:
		return fmt.Errorf("difficulty %q must be peaceful, easy, normal, or hard", c.Difficulty)
	}
	c.DefaultGameMode = strings.ToLower(strings.TrimSpace(c.DefaultGameMode))
	switch c.DefaultGameMode {
	case "survival", "creative", "adventure", "spectator":
	default:
		return fmt.Errorf("default_gamemode %q must be survival, creative, adventure, or spectator", c.DefaultGameMode)
	}
	for name, limit := range map[string]int{
		"hostile":                     c.MobSpawning.Hostile,
		"passive":                     c.MobSpawning.Passive,
		"ambient":                     c.MobSpawning.Ambient,
		"axolotls":                    c.MobSpawning.Axolotls,
		"underground_water_creatures": c.MobSpawning.UndergroundWaterCreatures,
		"water_creatures":             c.MobSpawning.WaterCreatures,
		"water_ambient":               c.MobSpawning.WaterAmbient,
	} {
		if limit < 0 || limit > 1000 {
			return fmt.Errorf("mob_spawning.%s %d must be between 0 and 1000", name, limit)
		}
	}
	if c.Combat.KnockbackHorizontal < 0 || c.Combat.KnockbackHorizontal > 4 {
		return fmt.Errorf("combat.knockback_horizontal %.3f must be between 0 and 4", c.Combat.KnockbackHorizontal)
	}
	if c.Combat.KnockbackVertical < 0 || c.Combat.KnockbackVertical > 4 {
		return fmt.Errorf("combat.knockback_vertical %.3f must be between 0 and 4", c.Combat.KnockbackVertical)
	}
	if c.ClearLag.IntervalSeconds < 10 {
		return errors.New("clear_lag.interval_seconds must be at least 10")
	}
	if c.ClearLag.MinimumEntityAgeSeconds < 0 {
		return errors.New("clear_lag.minimum_entity_age_seconds must be >= 0")
	}
	for _, seconds := range c.ClearLag.WarningSeconds {
		if seconds <= 0 || seconds >= c.ClearLag.IntervalSeconds {
			return fmt.Errorf("clear_lag warning %d must be between 1 and interval_seconds-1", seconds)
		}
	}
	if c.Bedrock.Enabled && c.Bedrock.Address == "" {
		return errors.New("bedrock.address must not be empty when bedrock is enabled")
	}
	if c.PermissionEditor.Enabled {
		if parsed, err := url.ParseRequestURI(c.PermissionEditor.EditorURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("permission_editor.editor_url %q must be a valid http/https URL", c.PermissionEditor.EditorURL)
		}
		if parsed, err := url.ParseRequestURI(c.PermissionEditor.BytebinURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("permission_editor.bytebin_url %q must be a valid http/https URL", c.PermissionEditor.BytebinURL)
		}
	}
	if c.Plugins.Enabled {
		c.Plugins.Directory = strings.TrimSpace(c.Plugins.Directory)
		if c.Plugins.Directory == "" {
			return errors.New("plugins.directory must not be empty when plugins are enabled")
		}
		if c.Plugins.EventBudgetMillis < 1 || c.Plugins.EventBudgetMillis > 1000 {
			return fmt.Errorf("plugins.event_budget_ms %d must be between 1 and 1000", c.Plugins.EventBudgetMillis)
		}
		if c.Plugins.ColdEventGraceMillis < 0 || c.Plugins.ColdEventGraceMillis > 1000 {
			return fmt.Errorf("plugins.cold_event_grace_ms %d must be between 0 and 1000",
				c.Plugins.ColdEventGraceMillis)
		}
	}
	return nil
}

// ApplyEnvOverrides reads well-known environment variables and overrides any
// YAML values that are present.  Call this after Load() and before passing
// Config to the server.  Validation is re-run after applying overrides so
// an invalid combination (e.g. GOCRAFT_JAVA_PORT=0) is caught early.
//
// Environment variables (all optional; empty = no override):
//
//	GOCRAFT_JAVA_HOST         Java TCP bind host          (default: 0.0.0.0)
//	GOCRAFT_JAVA_PORT         Java TCP port number        (default: 25565)
//	GOCRAFT_JAVA_ENABLED      "true"/"false"              (default: true)
//	GOCRAFT_ONLINE_MODE       Java auth required          (default: false)
//	GOCRAFT_MOTD              Server MOTD string
//	GOCRAFT_SERVER_ICON       Java server-list icon path
//	GOCRAFT_MAX_PLAYERS       Max concurrent players
//	GOCRAFT_WORLD_STORAGE     disk or memory              (default: disk)
//	GOCRAFT_WORLD_DIR         Anvil world directory path
//	GOCRAFT_WORLD_SEED        Signed 64-bit terrain seed
//	GOCRAFT_VIEW_DISTANCE     Java chunk view radius        (default: 8)
//	GOCRAFT_PREGENERATE_RADIUS Background generation radius (default: 12)
//	GOCRAFT_MAX_CACHED_CHUNKS Clean chunk cache limit       (default: 768)
//	GOCRAFT_DIFFICULTY        peaceful/easy/normal/hard    (default: normal)
//	GOCRAFT_WHITELIST_ENABLED "true"/"false"              (default: false)
//	GOCRAFT_BEDROCK_ENABLED            "true"/"false"              (default: false)
//	GOCRAFT_BEDROCK_ADDR               Bedrock UDP address         (default: 0.0.0.0:19106)
//	GOCRAFT_BEDROCK_ONLINE_MODE        Xbox Live auth required     (default: true)
//	GOCRAFT_PERMISSION_EDITOR_ENABLED  "true"/"false"              (default: true)
//	GOCRAFT_PERMISSION_EDITOR_URL      Editor GitHub Pages URL
//	GOCRAFT_PERMISSION_EDITOR_BYTEBIN  Bytebin base URL
func (c *Config) ApplyEnvOverrides() error {
	if v := os.Getenv("GOCRAFT_JAVA_HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("GOCRAFT_JAVA_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_JAVA_PORT %q: %w", v, err)
		}
		c.Port = n
	}
	if v := os.Getenv("GOCRAFT_JAVA_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_JAVA_ENABLED %q: %w", v, err)
		}
		c.JavaEnabled = b
	}
	if v := os.Getenv("GOCRAFT_ONLINE_MODE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_ONLINE_MODE %q: %w", v, err)
		}
		c.OnlineMode = b
	}
	if v := os.Getenv("GOCRAFT_MOTD"); v != "" {
		c.MOTD = v
	}
	if v := os.Getenv("GOCRAFT_SERVER_ICON"); v != "" {
		c.ServerIcon = v
	}
	if v := os.Getenv("GOCRAFT_MAX_PLAYERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_MAX_PLAYERS %q: %w", v, err)
		}
		c.MaxPlayers = n
	}
	if v := os.Getenv("GOCRAFT_WORLD_STORAGE"); v != "" {
		c.WorldStorage = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("GOCRAFT_WORLD_DIR"); v != "" {
		c.WorldDir = v
	}
	if v := os.Getenv("GOCRAFT_WORLD_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("GOCRAFT_WORLD_SEED %q: %w", v, err)
		}
		c.WorldSeed = n
	}
	if v := os.Getenv("GOCRAFT_VIEW_DISTANCE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_VIEW_DISTANCE %q: %w", v, err)
		}
		c.ViewDistance = n
	}
	if v := os.Getenv("GOCRAFT_PREGENERATE_RADIUS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_PREGENERATE_RADIUS %q: %w", v, err)
		}
		c.PreGenerateRadius = n
	}
	if v := os.Getenv("GOCRAFT_MAX_CACHED_CHUNKS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_MAX_CACHED_CHUNKS %q: %w", v, err)
		}
		c.MaxCachedChunks = n
	}
	if v := os.Getenv("GOCRAFT_DIFFICULTY"); v != "" {
		c.Difficulty = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("GOCRAFT_WHITELIST_ENABLED"); v != "" {
		value, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_WHITELIST_ENABLED %q: %w", v, err)
		}
		c.Whitelist.Enabled = value
	}
	if v := os.Getenv("GOCRAFT_ATTACK_COOLDOWN"); v != "" {
		value, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_ATTACK_COOLDOWN %q: %w", v, err)
		}
		c.Combat.AttackCooldown = value
	}
	if v := os.Getenv("GOCRAFT_KNOCKBACK_HORIZONTAL"); v != "" {
		value, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("GOCRAFT_KNOCKBACK_HORIZONTAL %q: %w", v, err)
		}
		c.Combat.KnockbackHorizontal = value
	}
	if v := os.Getenv("GOCRAFT_KNOCKBACK_VERTICAL"); v != "" {
		value, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("GOCRAFT_KNOCKBACK_VERTICAL %q: %w", v, err)
		}
		c.Combat.KnockbackVertical = value
	}
	if v := os.Getenv("GOCRAFT_BEDROCK_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_BEDROCK_ENABLED %q: %w", v, err)
		}
		c.Bedrock.Enabled = b
	}
	if v := os.Getenv("GOCRAFT_BEDROCK_ADDR"); v != "" {
		c.Bedrock.Address = v
	}
	if v := os.Getenv("GOCRAFT_BEDROCK_ONLINE_MODE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_BEDROCK_ONLINE_MODE %q: %w", v, err)
		}
		c.Bedrock.OnlineMode = b
	}
	if v := os.Getenv("GOCRAFT_PERMISSION_EDITOR_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOCRAFT_PERMISSION_EDITOR_ENABLED %q: %w", v, err)
		}
		c.PermissionEditor.Enabled = b
	}
	if v := os.Getenv("GOCRAFT_PERMISSION_EDITOR_URL"); v != "" {
		c.PermissionEditor.EditorURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("GOCRAFT_PERMISSION_EDITOR_BYTEBIN"); v != "" {
		c.PermissionEditor.BytebinURL = strings.TrimRight(v, "/")
	}

	if c.Debug.EnvironmentOverrides {
		logEnvOverrides()
	}

	// Re-validate after overrides — env vars can produce invalid combinations.
	return c.validate()
}

// logEnvOverrides logs which values were overridden by environment variables.
func logEnvOverrides() {
	vars := []struct{ key, val string }{
		{"GOCRAFT_JAVA_HOST", os.Getenv("GOCRAFT_JAVA_HOST")},
		{"GOCRAFT_JAVA_PORT", os.Getenv("GOCRAFT_JAVA_PORT")},
		{"GOCRAFT_JAVA_ENABLED", os.Getenv("GOCRAFT_JAVA_ENABLED")},
		{"GOCRAFT_ONLINE_MODE", os.Getenv("GOCRAFT_ONLINE_MODE")},
		{"GOCRAFT_MOTD", os.Getenv("GOCRAFT_MOTD")},
		{"GOCRAFT_SERVER_ICON", os.Getenv("GOCRAFT_SERVER_ICON")},
		{"GOCRAFT_MAX_PLAYERS", os.Getenv("GOCRAFT_MAX_PLAYERS")},
		{"GOCRAFT_WORLD_STORAGE", os.Getenv("GOCRAFT_WORLD_STORAGE")},
		{"GOCRAFT_WORLD_DIR", os.Getenv("GOCRAFT_WORLD_DIR")},
		{"GOCRAFT_WORLD_SEED", os.Getenv("GOCRAFT_WORLD_SEED")},
		{"GOCRAFT_VIEW_DISTANCE", os.Getenv("GOCRAFT_VIEW_DISTANCE")},
		{"GOCRAFT_PREGENERATE_RADIUS", os.Getenv("GOCRAFT_PREGENERATE_RADIUS")},
		{"GOCRAFT_MAX_CACHED_CHUNKS", os.Getenv("GOCRAFT_MAX_CACHED_CHUNKS")},
		{"GOCRAFT_DIFFICULTY", os.Getenv("GOCRAFT_DIFFICULTY")},
		{"GOCRAFT_WHITELIST_ENABLED", os.Getenv("GOCRAFT_WHITELIST_ENABLED")},
		{"GOCRAFT_ATTACK_COOLDOWN", os.Getenv("GOCRAFT_ATTACK_COOLDOWN")},
		{"GOCRAFT_KNOCKBACK_HORIZONTAL", os.Getenv("GOCRAFT_KNOCKBACK_HORIZONTAL")},
		{"GOCRAFT_KNOCKBACK_VERTICAL", os.Getenv("GOCRAFT_KNOCKBACK_VERTICAL")},
		{"GOCRAFT_BEDROCK_ENABLED", os.Getenv("GOCRAFT_BEDROCK_ENABLED")},
		{"GOCRAFT_BEDROCK_ADDR", os.Getenv("GOCRAFT_BEDROCK_ADDR")},
		{"GOCRAFT_BEDROCK_ONLINE_MODE", os.Getenv("GOCRAFT_BEDROCK_ONLINE_MODE")},
		{"GOCRAFT_PERMISSION_EDITOR_ENABLED", os.Getenv("GOCRAFT_PERMISSION_EDITOR_ENABLED")},
		{"GOCRAFT_PERMISSION_EDITOR_URL", os.Getenv("GOCRAFT_PERMISSION_EDITOR_URL")},
		{"GOCRAFT_PERMISSION_EDITOR_BYTEBIN", os.Getenv("GOCRAFT_PERMISSION_EDITOR_BYTEBIN")},
	}
	for _, v := range vars {
		if v.val != "" {
			slog.Info("config: env override applied", "var", v.key, "value", v.val)
		}
	}
}

// save marshals cfg to YAML and writes it to path, creating parent directories.
func save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
