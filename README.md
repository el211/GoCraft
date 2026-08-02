<p align="center">
  <img src="gocraftpng.png" alt="GoCraft — Minecraft server rewritten in Go" width="100%">
</p>

<h1 align="center">GoCraft</h1>

<p align="center">
  A native-Go Minecraft server built from scratch around an edition-agnostic core.
</p>

> [!WARNING]
> GoCraft is early experimental software. It is **not production-ready** and should not be exposed as a public server. Expect breaking changes and data-model changes during development.

## Overview

GoCraft is a native Go implementation of a Minecraft server written from scratch. It is built around a protocol-independent game core with edition-specific network adapters at the boundary. It is not a Paper fork, does not use the JVM, and is not a drop-in replacement for an existing server.

A vanilla Minecraft: Java Edition 1.21.4 client can connect, authenticate, complete configuration, and enter a persistent seed-driven world. Players can move, chat, run slash commands, break and place blocks using items from their inventory, and see other players and passive mobs in real time. World changes are saved to Anvil region files and reloaded on restart.

## Compatibility

| Client | Current status |
| --- | --- |
| Minecraft: Java Edition 1.21.4 | Active development target |
| Java protocol 769 | Implemented |
| Other Java Edition versions | Not supported |
| Minecraft: Bedrock Edition 1.26.30 / protocol 1001 | Beta native adapter with canonical-world and Java/Bedrock cross-play support |

Changing `version_name` or `protocol_version` in `server.yml` changes the advertised status metadata only; it does not add protocol compatibility.

## Implemented

- Native Go entry point and executable
- TCP listener, per-connection goroutines, and graceful process shutdown
- Minecraft packet framing, VarInt/VarLong encoding, UUIDs, and common wire types
- Handshake routing to status or login state
- Server-list status response with MOTD, version, and player limits
- Ping/pong latency exchange
- Offline-mode login with deterministic offline UUIDs
- Online-mode authentication through the Mojang session server
- RSA key exchange and AES-128-CFB8 encrypted connections
- Java configuration state:
  - Known-packs negotiation via a `registry.Provider` interface (`VanillaProvider` uses the vanilla 1.21.4 shortcut; future providers can send full registry data for custom content or Bedrock translation)
  - `minecraft:brand` plugin message, vanilla feature flags, and configuration completion
- Entry into the Java play state:
  - Login, abilities, default spawn, tab-list entry, position sync, center-chunk marker
  - Teleport confirmation
  - Periodic keep-alive requests and response validation
- **Canonical world layer** (`core/world`):
  - Edition-agnostic `Block` type with `Namespace`, `Name`, and `Properties` — no Java or Bedrock IDs in the core
  - Palette-based `Section` and `Chunk` types; 24 sections per column (Y=−64 to 319)
  - Deterministic seeded terrain with oceans, mountains, cliffs, climate biomes, caves, ores, and vegetation
  - Concurrent in-memory chunk cache with on-demand generation, configurable background pregeneration, and bounded reusable chunk/light encoding buffers
  - `Storage` interface with explicit `disk` or `memory` mode; disk mode uses Anvil region files, NBT, zlib compression, atomic saves, dirty-chunk tracking, lazy loading, and periodic autosaves
  - Architecture test that fails at compile time if any `core/` package imports `java/`
- **Java chunk encoding** (`java/world`):
  - Data-driven Java 1.21.4 global block state ID registry
  - `Block → Java state ID` lookup at the adapter boundary — the core never touches Java IDs
  - Network-NBT heightmap encoding (root compound without name, 1.20.2+ format)
  - `PalettedContainer` encoder: indirect palette, ≥4 bits/entry, no-overflow packing
  - Level Chunk With Light packets with height-aware masks, pooled lateral skylight propagation beneath roofs, and block entities
  - Center-first chunk batches, configurable view distance, unload hysteresis, and background pregeneration
- **Multiplayer** (`java/handler`):
  - Player spawn and despawn packets broadcast to all other sessions
  - Position and head-rotation broadcast on every movement packet
  - Lock-free broadcast via session snapshot (no lock held during socket writes)
- **Chat** (`java/handler`):
  - Player chat messages broadcast to all connected players
  - `/`-prefixed input dispatched through the built-in command system
  - 256-character message length limit with client notification
- **Block interaction and containers** (`java/handler`):
  - Creative instant-break on START_DIGGING; survival completes solid-block breaks on FINISH_DIGGING, while zero-hardness plants break immediately (server-authoritative hardness/tool timing is still incomplete)
  - Held-item block placement replaces grass, flowers, and other replaceable plants, refuses to overwrite solid blocks, consumes the stack in survival, and validates world Y bounds
  - Hoes till supported dirt variants; wheat, carrot, potato, beetroot, and nether-wart crops can be planted and grow without a light requirement
  - Block Update broadcast, Acknowledge Block Change sequence echo, material-aware break sounds, and experience-ore pickup feedback (experience-orb entities are not implemented yet)
  - The complete Java 1.21.4 recipe source set is embedded: 1,370 definitions, including 1,358 fixed recipe displays across crafting, furnaces, smokers, campfires, stonecutters, and smithing; 12 dynamic special recipes remain non-executable
  - Crafting tables support recipe-book automatic placement plus basic grid, result, cursor, and shift-click movement; advanced click modes and ingredient remainders are still incomplete
  - Single chests provide 27 usable slots with server-authoritative clicks and Anvil block-entity persistence in disk mode; double chests are not implemented
  - Furnace, blast-furnace, smoker, and other station menus and recipes are advertised, but timed processing and most non-crafting station inventory logic are not implemented yet
- **Inventory and items** (`core/player`, `java/world`, `java/handler`):
  - `ItemStack{ItemID, Count}` in `core/player` — no Java IDs in the core
  - 46-slot player inventory with hotbar slot tracking; `HeldItem()` accessor
  - Creative Mode Set Item handling: maps numeric item registry IDs to canonical resource locations
  - Set Held Item tracking for hotbar scroll; initial inventory sent on join
- **Data-driven registries** (`internal/gamedata`, `java/world`):
  - Block-state, item, entity-type, biome, sound-event, and mob-effect protocol IDs loaded from JSON at init time
  - `internal/gamedata` package embeds `java/1.21.4/blocks.json`, `java/1.21.4/registries.json`, and `java/1.21.4/recipes.json` via `//go:embed`; files ship inside the binary — no external data directory required at runtime
  - Registry JSON follows Minecraft data-generator output; the embedded recipe catalog is generated from the official Java 1.21.4 server data pack and rebuilt with the binary
  - Property-keyed block state lookup (`"minecraft:grass_block[snowy=true]"` → ID) extracted from states arrays; key sort matches `core/world.Block.Key()` so no Java IDs leak into the core
  - Block, item, and entity-type hardcoded Go maps removed; maps populated by `registry.go` init function with structured logging of entry counts
- Protocol-independent player, spatial, and online-player registry types
- YAML configuration with defaults and validation (`world_storage: disk|memory`, `world_dir`, and deterministic `world_seed`)
- Structured logging through Go's `log/slog`
- Automated tests for authentication, cryptography, packet framing, recipes, crafting and chest containers, Anvil persistence, world generation, entity behavior, commands, configuration, and architecture isolation

- **Entity system** (`core/entity`, `java/handler`, `server/`):
  - Canonical `Entity` struct with position, velocity, health, dead flag, and concurrency ownership comment
  - `entity.Manager`: thread-safe Add / Remove / Get / Snapshot
  - ~40 `EntityType` string constants (resource location format)
  - 20 TPS tick loop with gravity, horizontal drag, interior-aware ground detection, queued entity damage, dead-entity cleanup, and tick-drift warnings
  - Non-blocking broadcast: packets built synchronously inside the tick goroutine (sole writer), dispatched to a goroutine for I/O so slow clients cannot stall the simulation
  - Java entity encoding: Spawn Entity packet, Teleport Entity packet, Remove Entities packet
  - Five passive mobs spawn near the configured world spawn; a capped experimental surface-animal spawn loop runs near online players; left-click attacks queue damage for the simulation tick, broadcast hurt sounds, animation, configurable knockback, and despawn packets, and make surviving passive mobs jump and flee from the attacker. Attack cooldown can be disabled for a legacy-style feel; player-versus-player combat is not implemented
  - `game.Game.NextEntityID()` shared atomic counter ensures player and mob IDs never collide
- **Experimental villages and villagers**:
  - Newly generated houses include biome-specific roofs and doors, one bed per house, and profession workstations; farms use hydrated crops and composters
  - Generated villages receive multiple villagers plus an iron-golem guard; residents are assigned to a house, bed, workstation, and village center and roam within the village
  - Villagers return to their beds at night and expose profession-specific merchant offers on right-click
  - Trading inventory execution and vanilla-complete villager POI/pathfinding are not implemented yet; saved chunks created by older builds are not overwritten
- **Command system** (`java/handler`):
  - `Dispatcher` with `Register` / `Dispatch`; unknown commands and handler errors reported to the issuing player as chat messages
  - `CommandContext` with `Player`, `Conn`, `Args`, `World`, `Manager`, and `TeleportTo` closure
  - Commands packet (brigadier DAG, 0x11 S→C) sent on join for client-side tab completion
  - `/help` — list commands; `/list` — online player names and count
  - `/gamemode <mode>` — updates canonical `Player.GameMode`, sends Game Event reason 3 + Player Abilities + tab-list update broadcast
  - `/tp <x> <y> <z>` — teleports player, sends Synchronize Player Position, immediately streams destination chunks and unloads origin chunks via `TeleportTo` closure
  - `/tp <player>` — player-name teleport with the same immediate chunk-streaming behaviour
  - `/xyz` — reports precise position, block coordinates, and chunk coordinates
  - `/locate <village|biome>` — locates the nearest generated village or a supported generated biome and prints a ready-to-use `/tp` command; every target is tab-completable
  - `/summon <mob> [villager_profession]` — spawns a known 1.21.4 mob beside the player; villager professions are tab-completable, while mob AI remains experimental
  - `/version` and `/ver` — report `GoCraft 1.21.4`
  - `/give <player> <item|block> [count]` and `/get <item|block> [count]` — validate against the 1.21.4 item registry, add to the target/self inventory, and synchronize it
  - `/fly` — toggles authorized flight and the current flying state
  - `/potioneffect <player> <effect> <seconds>` — applies a registry-backed level-I effect with particles and HUD icon
  - `/walkspeed <value|reset>` and `/flyspeed <value|reset>` — update Player Abilities speed values (`/walkspeen` and `/flyyspeed` are accepted aliases)
  - `/kick <player> [reason]` — sends Disconnect (Play) with NBT-encoded reason, closes connection

### Not implemented

Some vanilla systems remain incomplete: advanced Bedrock crafting/container transactions, timed furnace/smoker/blast-furnace processing, complete entity AI/pathfinding, permissions, and every edition-specific sound/particle are not implemented. Java and Bedrock players do share the canonical world, players, mobs, chat, combat, drops, time, movement, equipment, health/death/respawn, and basic block/inventory interactions. Paper plugin compatibility is not supported.

## Architecture

```text
                         ┌──────────────────────────┐
Java Edition client ───▶ │ Java protocol adapter    │
                         │ java/network             │
                         │ java/handler             │
                         │ java/world  ─┐           │
                         │ java/registry│           │
                         └──────────────┼───────────┘
                                        │ canonical Block / Chunk
                         ┌──────────────▼───────────┐
                         │ GoCraft core             │
                         │ core/world               │  ← no Java or Bedrock imports
                         │ core/player              │
                         │ core/game                │
                         │ core/spatial             │
                         └──────────────┬───────────┘
                                        │ canonical Block / Chunk
                         ┌──────────────▼───────────┐
Bedrock client ─ ─ ─ ─ ▶ │ Native Bedrock adapter    │
                         │ bedrock/world + sync     │
                         └──────────────────────────┘
```

### Block identity

The canonical block type carries no edition-specific IDs:

```go
// core/world — shared by all adapters
type Block struct {
    Namespace  string            // "minecraft"
    Name       string            // "stone", "grass_block", …
    Properties map[string]string // {"snowy": "false"}, nil = default state
}
```

Edition-specific IDs are resolved entirely at the adapter boundary:

```
Canonical Block
       │
┌──────┴──────┐
▼             ▼
Java global   Bedrock runtime
state ID      ID (future)
```

This means only the encoder packages need to change when updating Java versions or adding Bedrock support; `core/` is untouched. The architecture test in `core/world/arch_test.go` enforces this by failing the build if any `core/` file imports `GoCraft/java`.

### Registry abstraction

Known-packs negotiation and registry delivery are behind a `registry.Provider` interface:

```go
type Provider interface {
    Packs() []Pack
    SendRegistries(conn *network.ClientConn) error
}
```

`VanillaProvider` uses the Known-Packs shortcut (zero registry packets for vanilla 1.21.4). A future `ExplicitProvider` will send full registry data for custom dimensions, custom biomes, additional Java versions, and Java-to-Bedrock ID translation.

- **GoCraft core (`core/`)** owns the edition-neutral game state: blocks, chunks, world, entities, players, inventories, and spatial types. It never imports `java/` or `bedrock/`.
- **Java adapter (`java/`)** reads from `core/` and produces native Java Edition packets: TCP framing, login auth, encryption, chunk encoding, and play-state management. It does not know Bedrock exists.
- **Bedrock adapter (`bedrock/`)** uses RakNet/UDP and optional Xbox authentication, translates canonical chunks to Bedrock block hashes, posts gameplay intents to the core, and synchronizes shared players/entities back to every Bedrock session.
- **Server layer (`server/`)** wires configuration, the core, and the active adapters into the executable.

## Development status

| Milestone | Status | Scope |
| --- | --- | --- |
| 1 — Handshake and status ping | Complete | Handshake, server-list response, ping/pong, YAML configuration |
| 2 — Login and authentication | Complete | Offline and online login, Mojang session verification, RSA and AES-CFB8 |
| 3 — Configuration and play-state entry | Complete | Known packs, feature flags, initial play packets, teleport confirmation, keep-alive |
| 4 — World layer and chunk streaming | Complete | Canonical Block/Chunk types, seeded terrain, Java chunk encoding, batched streaming and pregeneration |
| 5 — Movement and dynamic chunk streaming | Complete | Movement packet handling, posToChunk floor-division, per-boundary chunk load/unload |
| 6 — Multiplayer sync | Complete | Player spawn/despawn, position and head-rotation broadcast, lock-free session snapshot |
| 7 — Chat | Complete | Chat broadcast, `/` command prefix, 256-character length limit |
| 8 — Block interaction | Complete | Creative/survival break logic, block placement from held item, Block Update broadcast, Y-bounds guard |
| 9 — World persistence | Complete | Anvil region-file I/O, NBT read/write, atomic saves, dirty-chunk tracking, `Storage` interface |
| 10 — Inventory and items | Complete | ItemStack, 46-slot inventory, hotbar tracking, Creative Mode Set Item, placement from held item, occupied-block guard |
| 11 — Entity system | Complete | Canonical Entity type, entity registry, mob spawn/tick/despawn, health and damage, 20 TPS tick loop |
| 12 — Commands | Complete | Command dispatcher, Commands packet with typed/literal tab completion, navigation/summon, targeted give/get, flight/speeds, potion effects, administration, and version/help commands |
| 13 — Data-driven registries | Complete | Load block state IDs, item IDs, entity-type IDs, and biome IDs from versioned JSON (`blocks.json`, `items.json`, `registries.json`); embedded via go:embed; hardcoded Go maps replaced; unknown IDs warn once via sync.Map |
| 13.1 — Data-driven packet IDs | Complete | Semantic packet names (minecraft:login etc.) in versioned JSON; internal/protocoldata MustCB/MustSB panic at startup on missing names; all handler hex constants removed; validation test suite (7 distinct invariants); GitHub Actions CI on ubuntu-latest |
| Experimental gameplay extensions | In progress | Full recipe catalog, basic crafting execution, persistent single chests, farming, configurable legacy-style mob combat, village residents and guards |
| 14 — Bedrock adapter | Beta | RakNet/UDP, Xbox auth, canonical chunk encoding, shared simulation, inventory basics, and bidirectional Java/Bedrock visibility |
| 15 — Go plugin API | Future work | Event bus, command registration, scheduler, permission nodes; plugins are compiled Go packages |

Detailed records for completed milestones are kept in [`logs/`](logs/).

## Requirements

- [Go](https://go.dev/) 1.24 or newer, as declared by `go.mod`
- Git, when cloning the repository
- A Minecraft: Java Edition 1.21.4 client for manual connection testing
- Network access to Mojang's session service when `online_mode: true`

No Java runtime is required.

## Clone, test, and build

### Windows PowerShell

```powershell
git clone https://github.com/el211/GoCraft.git
Set-Location GoCraft
go mod download
go test ./...
go build -o gocraft.exe .
```

### Linux / macOS

```bash
git clone https://github.com/el211/GoCraft.git
cd GoCraft
go mod download
go test ./...
go build -o gocraft .
```

## Configure

GoCraft reads `server.yml` from its working directory. The repository includes:

```yaml
java_enabled: true
host: 0.0.0.0
port: 25565
motd: A GoCraft Server
max_players: 20
version_name: 1.21.4
protocol_version: 769
online_mode: false
villagers: true
combat:
  attack_cooldown: false
  knockback_horizontal: 0.4
  knockback_vertical: 0.4
world_storage: disk
world_dir: world
world_seed: 0
view_distance: 8
pregenerate_radius: 12
max_cached_chunks: 768
bedrock:
  enabled: false
  address: 0.0.0.0:19106
  online_mode: true
```

| Setting | Meaning |
| --- | --- |
| `java_enabled` | Enables the Java Edition TCP listener |
| `host` | Address on which the Java TCP listener binds |
| `port` | Java Edition server port |
| `motd` | Text shown in the multiplayer server list |
| `max_players` | Advertised player limit |
| `version_name` | Advertised version name; currently `1.21.4` |
| `protocol_version` | Advertised protocol number; currently `769` |
| `online_mode` | Enables Mojang session authentication and encrypted login |
| `villagers` | Spawns generated village residents and iron-golem guards when true |
| `combat.attack_cooldown` | Enables modern timed attacks; false exposes legacy-style rapid attacks |
| `combat.knockback_horizontal` | Horizontal mob knockback strength (`0`-`4`) |
| `combat.knockback_vertical` | Vertical mob knockback strength (`0`-`4`) |
| `world_storage` | `disk` for persistent Anvil storage (default), or `memory` for an ephemeral world |
| `world_dir` | Anvil world folder used when `world_storage: disk` |
| `world_seed` | Signed 64-bit seed for deterministic overworld terrain |
| `view_distance` | Java chunk radius sent to each client (`2`-`32`) |
| `pregenerate_radius` | Larger background cache radius (`view_distance`-`64`) |
| `max_cached_chunks` | Maximum clean chunks retained in RAM (`128`-`65536`); default `768` |
| `bedrock.*` | Enables/configures the Bedrock UDP listener, Xbox authentication, and shared-world Java/Bedrock play; `address` may use any available UDP port, including `19106` |

Disk mode creates `world_dir/region` at startup, keeps a bounded hot-chunk cache in RAM, commits generated and modified chunks to Anvil region files every 30 seconds, and flushes again on clean shutdown. Memory mode performs no disk reads or writes, so its world changes are lost on restart. Go may retain freed heap pages for reuse, so panel-reported RAM does not always fall immediately even when cached chunks are evicted.

Changing `world_seed` and village-generation improvements affect newly generated chunks only. Use a new `world_dir` when changing seeds to avoid terrain seams. The built-in generator creates continents, climate biomes, oceans, beaches, mountains, caves, ores, and biome vegetation. It is an original generator and is not block-for-block seed-compatible with Mojang's noise router. For exact Java terrain, point `world_dir` at a world generated by vanilla/Paper 1.21.4; GoCraft reads its `level.dat`, full biome palettes, block states, and Anvil chunks.

If `server.yml` is absent, GoCraft creates it with defaults. Offline mode does not verify player identities; use it only in a trusted development environment.

## Run

Run the server from the repository root so it can find `server.yml`.

```powershell
# Windows
.\gocraft.exe
```

```bash
# Linux / macOS
./gocraft
```

Stop the server with <kbd>Ctrl</kbd>+<kbd>C</kbd>. The default listener is `0.0.0.0:25565`; connect a Java Edition 1.21.4 client to `localhost:25565` when testing locally.

## Project structure

```text
GoCraft/
├── bedrock/
│   ├── listener.go            # Experimental RakNet/login and limited Bedrock play loop
│   └── world/                 # Canonical block-hash and sub-chunk encoder
├── config/
│   └── config.go              # YAML loading, defaults, and validation
├── core/
│   ├── entity/
│   │   ├── entity.go          # Canonical Entity type, EntityType constants, Damage/Heal helpers
│   │   └── manager.go         # Thread-safe entity registry (Add/Remove/Get/Snapshot)
│   ├── game/game.go           # Edition-neutral player registry and shared entity ID counter
│   ├── player/
│   │   ├── player.go          # Canonical player model (position, inventory, game mode)
│   │   └── item.go            # ItemStack, InventorySize, HotbarStart constants
│   ├── spatial/spatial.go     # Position and rotation types
│   └── world/
│       ├── block.go           # Block{Namespace, Name, Properties} — no edition IDs
│       ├── chunk.go           # Sections, chunks, block entities, and canonical container items
│       ├── generator.go       # Generator interface and seeded OverworldGenerator
│       ├── storage.go         # Storage interface for chunk persistence
│       ├── world.go           # Concurrent world cache with dirty-chunk tracking
│       └── arch_test.go       # Fails build if core/ imports java/
├── java/
│   ├── auth/                  # Login crypto, UUIDs, Mojang sessions
│   ├── handler/               # Login/play handlers, commands, recipes, crafting, chests, blocks, inventory, entities
│   ├── network/               # TCP listener and client connections
│   ├── protocol/              # Framing, packets, VarInts, wire types
│   ├── registry/              # Provider interface + VanillaProvider
│   └── world/
│       ├── anvil/             # Anvil region-file I/O, NBT read/write, atomic saves
│       ├── blocks.go          # StateID() accessor (map populated from JSON at init)
│       ├── entity_types.go    # EntityTypeID() accessor (map populated from JSON at init)
│       ├── items.go           # ItemID/ItemName accessors + block-placement helpers
│       ├── registry.go        # JSON loader; populates block, item, entity-type maps at init
│       ├── chunk.go           # Java chunk encoder (PalettedContainer, heightmaps, light)
│       └── sender.go          # Chunk burst sender
├── internal/
│   ├── gamedata/
│   │   ├── embed.go           # go:embed declaration for embedded JSON data
│   │   └── java/1.21.4/
│   │       ├── blocks.json    # Block state IDs (Minecraft data-generator format)
│   │       ├── registries.json# Item, entity-type, biome, sound, and effect protocol IDs
│   │       └── recipes.json   # Embedded Java 1.21.4 recipe definitions
│   └── protocoldata/
│       ├── protocoldata.go    # MustCB/MustSB packet ID resolver; panics on unknown names
│       ├── protocoldata_test.go # 7-invariant validation test suite
│       └── java/1.21.4/       # Packet ID JSON files (play, configuration, login, status, handshake)
├── .github/workflows/ci.yml   # GitHub Actions: build + vet + test on ubuntu-latest
├── logs/                      # Milestone development records
├── server/
│   └── server.go              # Core and Java adapter orchestration
├── go.mod
├── go.sum
├── gocraftpng.png             # README banner
├── main.go                    # Executable entry point
└── server.yml                 # Runtime configuration
```

## Plugin API plans

A Go-native plugin API is planned, but **no plugin system is implemented today**. The intended direction includes events, scheduling, commands, permissions, and extension points built for GoCraft's own core. Paper, Bukkit, and Spigot plugin compatibility is not supported and should not be assumed.

## Bedrock and cross-play

GoCraft has a native Bedrock listener backed by the same canonical simulation as Java Edition. It indexes the current Bedrock block registry, encodes requested canonical chunks, and synchronizes Java and Bedrock players, mobs, dropped items, boats, projectiles, equipment, chat, block changes, time, health, death, respawn, sleeping, and basic inventories/interactions in both directions.

### What "adapter" means here

GoCraft is **not** a protocol translator like Geyser. The Bedrock adapter does not consume Java packets and re-encode them for Bedrock clients. Instead, both adapters independently read from the same canonical game state in `core/` and produce their own native wire format:

```
                    ┌─────────────────────────────┐
                    │         core/               │
                    │  World · Entity · Inventory │  ← no Java, no Bedrock
                    └────────────┬────────────────┘
                                 │ canonical state
               ┌─────────────────┴──────────────────┐
               ▼                                    ▼
   ┌───────────────────────┐          ┌───────────────────────┐
   │     java/ adapter     │          │   bedrock/ adapter    │
   │  native Java packets  │          │  native Bedrock packets│
   │  Java state IDs       │          │  Bedrock runtime IDs  │
   └───────────────────────┘          └───────────────────────┘
         Java client                       Bedrock client
```

Both clients now observe the same `core/world.World`, with each adapter converting canonical state into its own protocol independently. The Bedrock implementation is still beta: complex crafting/container stack requests, the full creative catalogue, exact biome palettes, and complete parity for every vanilla UI, sound, particle, and specialised entity behaviour remain future work.

## Contributing

GoCraft is still establishing its protocol and core boundaries. Before submitting a change:

1. Open an issue or discussion for large features or architecture changes.
2. Keep edition-independent code in `core/` and protocol-specific behavior in its adapter.
3. Never store edition-specific IDs (Java state IDs, Bedrock runtime IDs) in `core/` types.
4. Do not claim compatibility without a test or reproducible client trace.
5. Add or update tests for protocol encoding, authentication, and state transitions.
6. Run `go fmt ./...`, `go test ./...`, and `go build ./...`.
7. Keep pull requests focused and document any protocol version assumptions.

Please avoid adding generated binaries, credentials, player data, or private server logs.

## License

**Copyright © 2026 Oreo Studios — All Rights Reserved**

GoCraft is a proprietary software project developed and maintained by **Oreo Studios**.

This repository is intentionally public so the community can follow the development of the project. **Public visibility does not grant permission to copy, redistribute, modify, commercialize, or create derivative works from the source code.**

Unless you have received **prior written permission** from Oreo Studios, you may **not**:

* Copy substantial portions of the source code.
* Redistribute or publish modified versions.
* Create commercial or non-commercial derivatives.
* Rebrand or represent GoCraft as your own project.
* Use GoCraft code in another server software.

If you are interested in contributing to GoCraft or collaborating with Oreo Studios, we would love to hear from you. Please open an issue or contact us before starting any work.

**Company Information**

**Oreo Studios — Web & Game Development Studio**

SIREN: **993 823 459**
SIRET: **993 823 459 00017**
APE Code: **62.01Z**
Entrepreneur individuel — France

Website: https://oreostudios.fr

Minecraft is a trademark of Microsoft. GoCraft is an independent project and is not affiliated with or endorsed by Mojang Studios or Microsoft.

## Credits and acknowledgements

- The [Go project](https://go.dev/) and its standard library
- [`gopkg.in/yaml.v3`](https://pkg.go.dev/gopkg.in/yaml.v3) for YAML configuration
- Mojang Studios and Microsoft for Minecraft and its Java Edition protocol ecosystem
- Nukkit, Dragonfly, and gophertunnel, referenced in the repository's future Bedrock planning notes

GoCraft is an independent, unofficial project and is not affiliated with or endorsed by Mojang Studios or Microsoft. Minecraft is a trademark of Microsoft.
