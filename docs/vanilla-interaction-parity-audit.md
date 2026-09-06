# Dragonfly gameplay and interaction parity audit

Audit date: 2026-09-02. GoCraft baseline: `missing-items` at `00685b7`.
Progress update: 2026-09-06 (batch 3: bamboo, cave vines, jukebox, name tags, goat horn, spyglass, sign text; batch 4: sheep shearing, cow milking, Bedrock jukebox/name tags/goat horn/spyglass, ordinary vine spread; batch 5: shulker box, mooshroom stew, sheep regrowth, lectern, chiseled bookshelf; batch 6: glow ink sac/dye on signs, tripwire disarm, snow golem shearing, Bedrock collar/wool/sheared metadata).
Reference implementation: Dragonfly v0.11.0 from the resolved Go module source.
Protocol targets: Java Edition 1.21.4 and Bedrock Edition 1.26.45.

## Progress since the audit (2026-09-03)

The structural groundwork and the first tranche of `Missing` items are done.
Rows updated below are marked **Implemented (new)**; each is backed by a
cross-edition test. Completed so far:

- **Canonical item components.** `player.ItemStack` now carries an extensible
  component codec preserved through Java codecs, Bedrock NBT, anvil/inventory
  paths, dropped items, and world objects (resolves finding 1).
- **Canonical status-effect engine.** Effects are tracked, ticked, expire, apply
  periodic damage/heal, modify defensive damage, persist, and sync to both
  clients; foods, potions, totems, milk/honey cures, and a command bridge all
  route through it (resolves finding 2).
- **Potions.** Drinkable, splash, and lingering potions work on both editions:
  outcomes, bottle remainders, thrown payloads, distance-scaled effects,
  lingering area-effect clouds with radius decay and reapplication, plus Bedrock
  variant round-tripping and the native Java `potion_contents` component.
- **Milk & honey.** Milk drinking clears effects; honey cures poison.
- **Suspicious stew.** Flower-selected effect stored and applied on both editions.
- **Nine Bedrock-only interactions ported to Java** (TNT ignition, note-block
  tuning, respawn-anchor charging, composter lifecycle, flower-pot in/out, candle
  cake, beehive harvest, pumpkin carving, honeycomb waxing) plus hoe hanging
  roots — all now routed through shared canonical operations.
- **Java drop & offhand player actions** (Player Action statuses 3/4 drop, 6 swap).

Batch 2 (random-tick block growth, `core/world`, all deterministically tested):

- **Sugar cane & cactus growth** to a three-block height via the age counter;
  cactus refuses to grow beside a solid block.
- **Kelp** column growth up through source water, leaving kelp_plant bodies.
- **Twisting & weeping vine** growth into air (upward / downward).
- **Dry-sponge water absorption** on placement (BFS, distance 6, ≤64 blocks,
  waterlogged draining, wet-sponge conversion).
- **Coral death** — live coral dies to its dead variant 60 ticks after losing
  water contact, scheduled through the block-physics engine.

## Completed in batch 3 (2026-09-06)

- **Bamboo growth.** Sapling converts to trunk on random tick (~1-in-3). Tip grows upward with a position-seeded target height (12–16 blocks). Leaf transitions: top="small", second="large", rest="none". Tested: sapling conversion, upward growth, leaf transitions, height cap.
- **Cave vine growth.** Tip (cave_vines age 0..25) grows downward into air, leaving cave_vines_plant body. Berries grow independently (~1-in-11 per tick). Harvest interaction (HarvestCaveVineBerries) strips berries and returns glow_berries. Tested: downward growth, berry growth, harvest.
- **Jukebox record insert and eject.** Right-clicking a jukebox with a music disc sets has_record=true, stores the disc in the block entity slot 0, and plays the disc sound (category: record). Right-clicking a loaded jukebox ejects the disc into the player's inventory or drops it, plays the stop sound. All 19 vanilla disc variants mapped. GetBlockEntity added to World API. Tested: insert, reject-non-disc, reject-full, eject, empty-eject guard, slot read.
- **Name tags.** DisplayName and CustomNameVisible fields added to core/entity.Entity. Applying a name tag in main hand to any entity sets the custom name and broadcasts metadata (index 2: Optional Text Component, index 3: Boolean). DisplayName() helper added to ItemStack reading the minecraft:custom_name component. Consumes one name tag in survival.
- **Goat horn.** Right-clicking plays the instrument sound (8 vanilla sounds mapped via minecraft:instrument component) with a 7-second cooldown. Sound broadcast at range 64. LastGoatHornUse field added to Player.
- **Spyglass.** Right-clicking sets UsingItemID="minecraft:spyglass" so other players see the use animation. Stop is handled by the existing use-item release path.
- **Sign text editing.** minecraft:sign_update serverbound packet (ID 52) registered and dispatched. Lines validated (≤15 chars each), encoded as network-NBT front_text/back_text compound with messages list, stored via SetBlockEntity, and broadcast to all dimension viewers via BroadcastBlockEntityDataInDimension. openSignEditor (packetIDOpenSignEditor = 0x36) registered for future placement path.

## Completed in batch 4 (2026-09-06)

- **Sheep shearing.** Holding shears and interacting with an unsheared, adult sheep drops 1–3 wool of the sheep's colour, marks Sheared=true, broadcasts metadata (index 17: flags byte with colour bits 0-3 and sheared bit 4), and damages the shears. WoolColor and Sheared fields added to core/entity.Entity.
- **Cow/mooshroom milking.** Holding a bucket and interacting with an adult cow or mooshroom replaces the bucket with a milk bucket via consumeAnimalItem (works on both editions via the shared interactAnimal path).
- **Jukebox (Bedrock).** Holding a music disc and interacting with a jukebox inserts it (sets has_record=true, stores in slot 0). Interacting with a loaded jukebox ejects the disc (clears block entity, gives disc to player). Mirrors the Java implementation.
- **Name tags (Bedrock).** Right-clicking an entity with a named name tag on Bedrock sets DisplayName and CustomNameVisible and broadcasts metadata to all Java viewers.
- **Goat horn (Bedrock).** applyBedrockStartUseItem now triggers the instrument sound with the 7-second cooldown, matching the Java path.
- **Spyglass (Bedrock).** applyBedrockStartUseItem sets UsingItemID/UsingItemSince, matching the Java use-state path.
- **Ordinary vine spread.** minecraft:vine participates in the random-tick crop scan. 25% gate; 50% chance to spread downward (copies face set); horizontal spread picks a random face direction and places a new vine with supported faces. Tests: downward spread, horizontal spread.

## Completed in batch 5 (2026-09-06)

- **Shulker box content preservation.** Breaking a shulker box in survival packs its container items into the `minecraft:container` component of the dropped item (both Java and Bedrock). Contents no longer spill; the box can be re-placed with its inventory intact.
- **Mooshroom bowl/stew.** Right-clicking an adult mooshroom with an empty bowl gives mushroom stew, consuming the bowl. Shared via interactAnimal (both editions).
- **Sheep wool regrowth.** Shearing sets WoolRegrowTicks=300; the lifecycle tick decrements it and broadcasts metadata when it reaches zero, restoring the Sheared=false state (approx. 15 s).
- **Lectern book insert/remove.** Right-clicking a lectern with a written_book or writable_book places it (has_book=true, powered=true). Right-clicking without a book ejects it into the inventory. Book stored in block entity slot 0; contents drop on break. Both Java and Bedrock.
- **Chiseled bookshelf.** Six-slot book storage with cursor-position targeting (3×2 grid based on facing direction). Insert: holding book + empty slot. Eject: clicking occupied slot. slot_X_occupied block state updated. Books drop on break. Both Java and Bedrock.

## Completed in batch 6 (2026-09-06)

- **Glow ink sac and dye on signs.** Right-clicking a sign with a glow ink sac makes the front-face text glow (`has_glowing_text=1` in sign NBT). Right-clicking with a dye sets the text colour (`color` TAG_String). Ink sac removes glow. All 16 vanilla dye colours mapped to vanilla sign colour names. State preserved across sign-text edits via `SignState` (lines + glowing + color per face) stored on `BlockEntity`. Both Java and Bedrock.
- **Tripwire disarm.** Right-clicking `minecraft:tripwire` with shears while `disarmed=false` sets `disarmed=true`, drops one string when not powered, and damages the shears. Both Java and Bedrock.
- **Snow golem pumpkin removal.** Right-clicking a snow golem with shears when `HasPumpkin=true` removes the pumpkin face, drops a carved_pumpkin, damages shears, and broadcasts metadata (Java index 17 flags bit 0x10; Bedrock `EntityDataFlagSheared`). Snow golems now initialise with `HasPumpkin=true`.
- **Bedrock sheep wool color and sheared metadata.** `bedrockEntityMetadata` now encodes `EntityDataKeyColorIndex` from `WoolColor` and sets `EntityDataFlagSheared` from `Sheared`. Change detection added to the entity-view diff.
- **Bedrock collar color for wolf/cat.** `EntityDataKeyColorIndex` is set from `CollarColor` when the entity is tamed and the color is non-empty. Change detection added.

## Completed in batch 7 (2026-09-06)

- **Dragon egg teleport.** Right-clicking or beginning to punch the dragon egg in non-creative mode now teleports it to a random replaceable position (±7 x/z, ±1 y, up to 1 000 attempts). Creative players can break it normally. Both Java and Bedrock.
- **Sponge nether instant drying.** Wet sponge placed in the Nether (ultrawarm world) instantly converts to a dry sponge. `World.SetUltrawarm(true)` enables the flag; the nether world sets it on startup.
- **Cauldron entity fire extinguish.** A burning mob standing inside a `minecraft:water_cauldron` block now has its `FireTicks` cleared and the cauldron's water level decremented (level 1 → empty cauldron). Checked every entity tick for any entity with `FireTicks > 0`.
- **Bone meal on bamboo, kelp, cave vines, nether vines, and sea pickles.** `ApplyBoneMeal` now handles `minecraft:bamboo_sapling` (converts to segment), `minecraft:bamboo` tip (grows one block up), `minecraft:kelp` tip (grows one block up), `minecraft:cave_vines` tip (grows one block down), `minecraft:twisting_vines`/`minecraft:weeping_vines` tip (grows one block in direction), and `minecraft:sea_pickle` (increments pickles 1→4 when waterlogged). Both Java and Bedrock via shared `w.ApplyBoneMeal`.
- **Grindstone enchantment removal and XP refund.** Non-curse enchantments are stripped from all grindstone output items. Curse enchantments (binding, vanishing) are preserved. XP is awarded to the Java player when they take the output (≈ 8 XP per enchantment level). Bedrock inventory action path also strips enchantments via shared `grindstoneOperation`.

## Still left to do (as of batch 7)

Nothing below is claimed complete. Ordered roughly by the original
implementation order; the largest remaining structural gaps first.

1. **Charged/stateful use items (finding 4).** Canonical two-hand use state,
   charged-crossbow stack, gliding flag + glide physics. Then Bedrock
   bow/crossbow/trident/shield use paths (all currently Java-only or missing) and
   Elytra rocket boost.
2. **Remaining stateful block entities.** Banner patterns, item/glow item frames.
3. **Entity interactions.** Leads & leash knots, horse/llama equipment UI, chest
   boats & storage minecarts, armour stands.
4. **Remaining random-tick / neighbour behaviour (order 7).** Cocoa jungle-log
   survival + bone meal, full fluid flow vectors / finite levels, fire flammability nuances.
5. **Workstation operations.** Beacon effects, enchanting offers, grindstone disenchant,
   smithing trims, loom patterns, cartography scaling — screens open but the defining
   operation is absent.
6. **Combat & survival depth (order 8).** Tipped/spectral arrows, and the remaining
   exhaustion/hazard sources.
7. **Items still missing outright.** Fishing rod, brush, writable/written book editing,
   bundles, and maps.

Findings 3–5 (unified use dispatch, richer hand/use state, mining-time
validation) remain only partially addressed.

This is a static code audit of behaviour, not a registry-presence check. An item
is not considered implemented merely because it appears in the Java data pack,
Bedrock creative catalogue, a recipe, or a trade. The audit follows the action
from adapter input through canonical state mutation, persistence, and feedback
to both editions.

Dragonfly v0.11.0 has 59 item files and 118 block files with explicit use,
activation, entity-inside, neighbour, random-tick, scheduled-tick, or redstone
behaviour. Dragonfly is a Bedrock-first comparison point rather than a complete
specification. Java 1.21.4 behaviour not represented by Dragonfly is included
where GoCraft's declared Java target still requires it.

## Status legend

| Status | Meaning |
| --- | --- |
| Implemented | The inspected path performs the main server-authoritative behaviour on this adapter. Edge cases may remain. |
| Partial | The action starts or renders, but important state, validation, effects, persistence, or feedback is absent. |
| Missing | No gameplay path was found; registry or UI presence alone does not count. |
| Adapter gap | One edition has a gameplay path and the other does not. |
| N/A | The feature is outside the Java 1.21.4 target because of Bedrock/Dragonfly version skew. |

## Highest-impact findings

1. **Resolved (2026-09-03).** `player.ItemStack` now has an extensible component
   codec. Potion contents and stew effects have canonical storage and round-trip
   through Java codecs, Bedrock NBT, anvil/inventory paths, dropped items, and
   world objects. Remaining component types (book pages, map data, lodestone
   targets, banner patterns, dyed colour, armour trims, goat-horn instrument,
   crossbow charge, bundle/shulker contents, custom names, lore) still need their
   own encoders but now have a codec to hang on.
2. **Resolved (2026-09-03).** GoCraft has a canonical timed status-effect engine.
   Effects are tracked, ticked, and expired; they apply periodic damage/heal,
   modify defensive damage, persist, and sync to both clients. Foods, potions,
   totems, milk/honey cures, and a command bridge route through it.
3. Java and Bedrock still dispatch many interactions through separate hardcoded
   switches. This has already produced two large adapter-only sets: several
   block/item actions exist only on Bedrock, while ranged weapons and shields
   exist only on Java.
4. Stateful use is stored as one player-wide `UsingItemID`, timestamp, and
   hotbar index. It cannot model both hands, a charged crossbow stack, potion
   payloads, spyglass state, horn cooldown, or a persistent gliding state.
5. Block breaking trusts the client's completion event. Bedrock calculates a
   Dragonfly break duration for crack feedback, but neither edition validates
   elapsed mining time, tool speed, haste/fatigue, being underwater, or being
   airborne before accepting completion.

These structural gaps should be fixed before adding many isolated switch cases;
otherwise stateful items will appear to work and then lose their data during an
inventory move, save, cross-edition sync, or restart.

## Item and item-use audit

### Food, drink, and effects

| Item or family | Java | Bedrock | Missing or incomplete behaviour |
| --- | --- | --- | --- |
| Ordinary registry-backed foods | Implemented | Implemented | Hunger, saturation, use duration, full-hunger checks, stack consumption, and bowl/bottle remainders are shared. |
| Rotten flesh, raw chicken, spider eye, poisonous potato, pufferfish | Implemented (new) | Implemented (new) | Food effects are now stored and ticked authoritatively via the canonical status-effect engine on both the timed-use and legacy consume paths. Edge cases (probability rolls, exact durations per item) still deserve conformance tests. |
| Golden apple and enchanted golden apple | Implemented (new) | Implemented (new) | Regeneration, absorption, resistance, and fire resistance are now canonical effects applied through the engine on both editions. |
| Honey bottle | Implemented (new) | Implemented (new) | Nutrition, bottle remainder, and canonical poison cure all work. |
| Suspicious stew | Implemented (new) | Implemented (new) | Flower-selected effect is stored as a canonical component and applied on eat; Bedrock variant metadata round-trips through item packets and the creative catalogue. |
| Drinkable potions | Implemented (new) | Implemented (new) | Accepted by the consumable dispatcher; potion outcome resolved and applied, bottle remainder produced, instant heal/damage handled. |
| Splash potions | Implemented (new) | Implemented (new) | Players throw them on both editions; impacts apply distance-scaled effects to nearby players with the preserved payload. Colour particles and thrown metadata are sent. |
| Lingering potions | Implemented (new) | Implemented (new) | Player throw path, canonical area-effect cloud entity with radius decay, reapplication delay, and quarter-duration effects, ticked in every dimension. Tipped-arrow interaction is still open. |
| Milk bucket | Implemented (new) | Implemented (new) | Drink use, bucket remainder, and effect clearing all work. |

### Weapons, use-state, and utility items

| Item or family | Java | Bedrock | Missing or incomplete behaviour |
| --- | --- | --- | --- |
| Snowball, egg, ender pearl, experience bottle, wind charge | Implemented | Implemented | Shared projectiles include motion and impact behaviour. Enchantments and several edition-specific particles/sounds still need coverage. |
| Bow | Partial | Missing | Java draws and fires arrows, but Power, Punch, Flame, Infinity, spectral/tipped payloads, pickup rules, and per-shot critical state are incomplete. Bedrock has no bow use-state path. |
| Crossbow | Partial | Missing | Java has a two-step load/fire flow, but charge is a player-wide boolean and is lost on slot change. Quick Charge, Multishot, Piercing, charged projectile persistence, offhand priority, and firework ammunition are missing. Bedrock has no use path. |
| Trident | Partial | Missing | Java throws a basic projectile. Loyalty, Riptide, Channeling, Impaling, pickup/return ownership, wet checks, and per-stack state are missing. Bedrock has no use path. |
| Shield | Partial | Missing | Java main-hand blocking exists after a fixed delay. Offhand use cannot be started, and axe disable, durability loss, projectile/explosion rules, cooldown, and full feedback are incomplete. Bedrock has no shield use-state path. |
| Firework rocket | Partial | Partial | Ground launch, preserved rocket explosion data, ticking, explosion, and damage work on both adapters. Elytra boost and crossbow-fired rockets are missing. |
| Firework star | Partial | Partial | Some crafting/component decoding exists, but a firework-star component is not preserved as an item stack and all crafting transformations are not round-trippable. |
| Elytra | Partial | Partial | Equipping is possible. Java accepts the start-fall-flying action only by resetting fall distance; no canonical gliding flag or glide physics exists. Bedrock glide input is not modelled, and rocket boost is missing on both. |
| Totem of undying | Partial | Partial | Canonical death prevention and consumption run for both editions, and the granted survival effects are now stored authoritatively through the status-effect engine. Bedrock still does not receive the totem animation/effect packets through the totem path. |
| Goat horn | Implemented (new) | Implemented (new) | Both editions play the instrument sound (8 vanilla variants via component) with a 7 s cooldown. |
| Spyglass | Implemented (new) | Implemented (new) | Both editions set UsingItemID so other players see the use animation. |
| Fishing rod | Missing | Missing | No hook entity, cast/reel state, bobber physics, hooked entity/item handling, durability, or fishing loot. |
| Brush | Missing | Missing | No brushing progress, cooldown, suspicious sand/gravel dust states, block entity, archaeology loot, or durability. |
| Carrot on a stick / warped fungus on a stick | Missing | Missing | Pig/strider steering boost and durability are absent even though riding exists. |
| Compass / recovery compass | Partial | Partial | Static item identity exists. Lodestone target, dimension, tracking flag, last-death target, and canonical compass state are absent. |
| Empty map / filled map | Partial | Partial | Empty map converts to a filled-map item and cartography accepts it, but map allocation, scale, lock, dimension, colours, decorations, exploration-map target, and live map packets are absent. |
| Writable and written books | Missing | Missing | Pages, title, author, generation, editing/signing packets, validation, copying, and open-book interaction are absent. |
| Music discs | Missing | Missing | Items exist, but insertion/ejection and playback require the missing jukebox behaviour. |
| Dyes, ink sacs, glow ink sacs | Partial | Partial | Sheep dyeing, sign text colour, glowing sign text (glow ink sac), and collar colour (wolf/cat) now work (batch 6). Loom pattern preservation, cauldron leather/banner washing, and banner pattern application are still absent. |
| Shears | Partial | Partial | Carving pumpkins, harvesting beehives, sheep shearing (batch 4), mooshroom conversion, snow-golem pumpkin removal, and tripwire disarm (batch 6) now work on both editions. Complete sounds/durability rules still need verification. |
| Glass bottle | Partial | Partial | Bedrock fills from a full beehive. Java does not. Water-source and water-cauldron filling, dragon-breath collection, incremental cauldron levels, and complete remainders are missing. |
| Water/lava/powder-snow buckets | Partial | Partial | Basic source and full-cauldron transfer works. Fish, axolotl, tadpole, and entity capture/release; waterlogging/unwaterlogging; ultrawarm evaporation; and full dispenser/cauldron cases are incomplete. |
| Spawn eggs | Partial | Partial | A dispenser can spawn a generic entity. Player block/entity use, baby spawning, mob-specific NBT/state, placement validation, and unsupported-entity rejection are absent. |
| Armour stand item | Missing | Missing | No placeable armour-stand entity, pose, equipment interaction, damage rules, or drops. |
| Item frame / glow item frame | Missing | Missing | No placement, support validation, contained stack, rotation, map mode, comparator signal, punch removal, or glow behaviour. |
| Bundle | Missing | Missing | No contents/weight component or insert/extract interaction. |
| Shulker-box items | Partial | Partial | Placed storage works, but breaking spills contents and drops a separate empty box. Vanilla must keep the inventory inside the dropped shulker item. |

### Tools and block-use items

| Item or family | Java | Bedrock | Missing or incomplete behaviour |
| --- | --- | --- | --- |
| Axes | Partial | Partial | Stripping, scraping oxidation, and removing wax are present. Complete sound/particle feedback and enchantment durability rules remain. |
| Hoes | Implemented (new) | Implemented (new) | Tilling and hanging-root drops from rooted dirt now share canonical hoe transformations on both editions. Durability enchantment rules still deserve verification. |
| Shovels | Partial | Partial | Dirt paths and campfire extinguishing work. Full flattenable set, sound parity, and durability/enchantment rules need verification. |
| Flint and steel / fire charge | Partial | Partial | Both adapters create fire, light candles/campfires, and ignite portals. Bedrock directly primes TNT; Java has no TNT target branch. Projectile/dispenser and feedback rules remain incomplete. |
| Honeycomb | Implemented (new) | Implemented (new) | Copper waxing now runs through a shared canonical operation on both editions. |
| Bone meal | Partial | Partial | Supported crops and saplings work. Grass-area features, flowers, moss, azalea, mangrove, underwater plants/coral, fungi/nylium, sea pickles, and particles are incomplete. |
| Ender eye | Implemented | Implemented | Stronghold launch and portal-frame insertion are present. Structure search and feedback still deserve runtime tests. |

## Block interaction audit

### Bedrock behaviour with no Java equivalent

**Resolved (2026-09-03).** All nine interactions below were moved into shared
canonical operations and given a Java path, so both editions now dispatch them
through the same code. Each is covered by a Java test.

| Interaction | Bedrock | Java |
| --- | --- | --- |
| Carve pumpkin with shears and drop seeds | Implemented | Implemented (new) |
| Harvest a full bee nest/hive with shears or bottle | Implemented | Implemented (new) |
| Wax copper with honeycomb | Implemented | Implemented (new) |
| Add a candle to a cake | Implemented | Implemented (new) |
| Put a plant into, or remove it from, a flower pot | Implemented | Implemented (new) |
| Add compostables, mature the composter, collect bone meal | Implemented (new) | Implemented (new) |
| Charge a respawn anchor with glowstone | Implemented (new) | Implemented (new) |
| Ignite TNT directly with flint and steel/fire charge | Implemented | Implemented (new) |
| Tune a note block | Implemented (new) | Implemented (new) |

The prior structural recommendation — move these into shared canonical
operations before adding Java calls — was followed for all nine.

### Missing or incomplete on both editions

| Block or family | Status | Missing or incomplete behaviour |
| --- | --- | --- |
| Jukebox | Implemented (new) | Record insert (has_record state, slot 0 block entity, disc sound), eject (return disc, stop sound) wired on both Java and Bedrock. All 19 vanilla disc sound events mapped. Comparator output remains. |
| Lectern | Missing | Book insert/remove, pages, current page, UI, page-turn events, comparator output, redstone pulse, and persistence. It currently exists only as a generated village workstation/redstone name. |
| Chiseled bookshelf | Missing | Six-slot inventory, targeted slot selection, book insertion/removal, block state, comparator signal, vibration, and persistence. |
| Item/glow item frame | Missing | See item table; Dragonfly implements this as a stateful block. |
| Dragon egg | Implemented (new) | Right-click or dig-start teleports egg to random replaceable position (±7 x/z, ±1 y). Creative exception applied. Both Java and Bedrock. |
| Note block | Implemented (new) | Tuning now runs through a shared canonical operation on both editions. Instrument-from-block-below and canonical note sound/particle on rising edge still need coverage. |
| Signs and hanging signs | Partial | Partial | Placement, text editing, dye colour, and glow ink sac / ink sac now work on both editions (batch 6). SignState (lines + glowing + color) preserved across edits. Wax, click events, and Bedrock text editing remain. |
| Banners | Partial | Placement works. Pattern layers, loom output, shields carrying patterns, map markers, block-entity persistence, and wash-off in cauldrons are absent. |
| Beacon | Partial | Both adapters can open a one-slot screen. Pyramid level, beam obstruction/colour, payment validation, selected effects, periodic area application, and persistence are absent. |
| Enchanting table | Partial | The screen accepts item/lapis slots, but offers, bookshelf power, seed, XP/lapis cost, selection packet, and enchant application are absent. |
| Brewing stand | Partial | Persistent slots and generic hopper movement exist, but blaze fuel, brew timer, ingredient transformations, potion payloads, bottle properties, and slot-aware automation are absent. |
| Anvil | Partial | Basic material/same-item repair exists. Rename, enchant merging, prior-work penalty, XP cost, too-expensive rules, anvil damage, and output feedback are absent. |
| Grindstone | Partial | Basic combining/output exists. Non-curse enchantments are now stripped from both input items and the output, curses (binding, vanishing) are preserved, and XP is awarded to the player (≈ 8 XP per enchantment level). Sounds remain absent. |
| Smithing table | Partial | Upgrade item-ID transforms exist. Armour trims and trim material/pattern components cannot be produced or preserved. |
| Loom | Partial | It consumes a banner and dye but returns an unchanged banner because pattern data has no canonical representation. Selection, six-layer limit, and full pattern rules are absent. |
| Stonecutter | Partial | Recipe selection/output exists. Adapter selection parity and complete feedback/validation require tests. |
| Cartography table | Partial | It returns a generic filled map for paper, map, or glass pane. Scale, clone count, lock state, map identity, and data preservation are absent. |
| Respawn anchor | Partial | Charging now works on both editions through a shared canonical operation (Java added). Nether spawn assignment, charge use on respawn, comparator output, and explosion outside the Nether are still absent. |
| Bee nest / beehive | Partial | Bedrock can harvest honey. Bees, occupants, entry/exit, honey production, anger, smoke pacification, Silk Touch data, dripping, and Java harvest are absent. |
| Campfire | Partial | Placement, lighting/extinguishing, four stored cooking slots, cooking completion, and damage are present. Item rendering, per-slot progress persistence, smoke height, hay signal, bee pacification, projectile lighting, soul variants, and complete drop/waterlogging rules remain. |
| Candles / candle cakes / cake | Partial | Core stacking, eating, lighting, and extinguishing exist, but Java candle-cake creation, projectile lighting, cake collision details, particles, sounds, and complete waterlogging/support behaviour remain. |
| Cauldrons | Partial | Full bucket transfers work. Water cauldron now extinguishes burning mobs and decrements level. Bottles, incremental water/powder levels, dyed leather washing/dyeing, banner cleaning, shulker dyeing, precipitation, dripstone filling, and comparator details are still absent. |
| Sponge | Implemented (new) | Dry-sponge breadth-first water absorption (taxicab distance 6, up to 64 blocks), waterlogged draining, and wet-sponge conversion now run on placement through any adapter. Wet sponge placed in the Nether (ultrawarm) now instantly dries to a dry sponge. Absorption particles remain. |
| Coral and coral blocks | Implemented (new) | Live coral now schedules a death check 60 ticks after losing water contact and converts to its dead variant; waterlogged or water-adjacent coral survives. Bone-meal coral-block spreading remains. |
| Cactus | Implemented (new) | Random growth to a three-block height with the age counter, refusing to grow beside a solid block. Contact damage and support removal were already present. Item destruction and entity collision detail remain. |
| Sugar cane | Implemented (new) | Random growth to a three-block height through the age counter. Placement/support removal was already present. Water-adjacency survival is still enforced only by block physics. |
| Bamboo / bamboo sapling | Implemented (new) | Sapling conversion (~1-in-3 ticks), random upward growth, position-seeded height cap (12–16 blocks), and correct small/large/none leaf transitions are implemented. Bone meal now converts sapling to segment and advances the tip by one block. Break-from-below rules remain. |
| Kelp | Implemented (new) | Random column growth through water: the age-0..25 tip advances into source water above, leaving kelp_plant bodies behind. Bone meal now advances the tip one block (no random gate). Break-from-below rules remain. |
| Vines / cave vines / twisting and weeping vines | Partial | Twisting/weeping vines grow via the age-0..25 tip mechanic. Cave vines grow downward and grow berries; harvest wired. Ordinary vine now spreads downward and horizontally via random tick (batch 4). Bone meal now advances twisting/weeping/cave vine tips one block (batch 7). Climbing state and shears rules are still absent. |
| Cocoa | Partial | A bounded legacy growth tick exists. Correct jungle-log attachment/facing survival, placement interaction, and bone meal are absent. |
| Fire | Partial | Placement, scheduled spread/burnout, and contact damage exist. Fire immunity/effects, rain extinguishing, portal interaction details, gamerules, block-specific flammability, and complete soul-fire rules remain. |
| Fluids and waterlogging | Partial | Source placement, simple spread, collision checks, and lava/water hardening exist. Flow vectors, finite levels/source formation, waterlogged fluid ticking, ultrawarm evaporation, entity pushing, dripstone, and many replaceability rules are incomplete. |
| Boats and minecarts | Partial | Placement, mounting, basic movement, rails, powered/detector/activator effects, and TNT minecart fuse exist. Placement is too permissive, and collision, fluid physics, fall damage, passenger rules, chest/hopper/furnace inventories, furnace fuel, and complete cross-edition control are missing. |

### Broadly implemented but still needs conformance tests

The following families have a meaningful shared implementation and are not the
first missing-feature targets: beds and respawn points, ordinary chests/barrels
and per-player ender chests, furnaces/smokers/blast furnaces, crafting grids,
doors/trapdoors/fence gates, levers/buttons/pressure plates, repeaters,
comparators, daylight detectors, redstone wire/torches/lamps, pistons,
observers, sculk sensors, rails, hoppers, droppers, dispensers, crafters,
decorated pots, crop/stem growth, sapling trees, falling blocks, portals,
explosions, and ordinary attachment placement. Their remaining edge cases
should be covered by differential tests rather than being labelled complete.

## Entity interaction audit

GoCraft has a useful shared base for feeding, baby growth, breeding, taming,
sitting, saddling, mounting, villagers, boats, minecarts, and basic attacks.
The following interaction families are still missing or incomplete on both
adapters unless noted:

| Interaction | Status | Gap |
| --- | --- | --- |
| Sheep shearing and dyeing | Implemented (new) | Shearing, dyeing, wool regrowth, mooshroom conversion, snow-golem pumpkin removal, and Bedrock wool-color/sheared metadata all implemented. |
| Cow/mooshroom milking | Implemented (new) | Bucket → milk bucket via shared interactAnimal path on both editions. Mooshroom mushroom-stew bowl, suspicious stew, and mooshroom→cow conversion remain. |
| Fish/axolotl/tadpole bucket capture | Missing | No capture/release data, bucket replacement, variant preservation, or water placement. |
| Leads and leash knots | Missing | No leash ownership, fence knot entity, distance physics, drop, or detach interaction. |
| Name tags | Implemented (new) | Applying a name tag on both editions sets DisplayName and CustomNameVisible; metadata broadcast to all viewers. DisplayName() helper reads minecraft:custom_name component. Anvil naming prerequisite and persistence across restart remain. |
| Horse/donkey/llama equipment | Partial | Saddling/mounting exists. Inventory UI, armour, carpet, chest attachment, storage, jump charge, temper/buck details, and equipment persistence are absent. |
| Wolf/cat/parrot ownership | Partial | Taming and sit toggle exist. Collar dye, follow/teleport rules, owner defence breadth, shoulder parrots, gifts, and complete metadata are absent. |
| Turtle/frog/sniffer/armadillo special actions | Partial | Generic food/breeding tables exist, but egg laying, scute/drop timing, frogspawn, sniff/dig, brushing, rolling, and species-specific goals are absent. |
| Villager trading | Partial | Trading UI/catalogue exists. Gossip, restocking rules, demand, curing discounts, reputation, wandering trader details, and full profession workflows are incomplete. |
| Chest boats and storage minecarts | Missing | Entity types can spawn, but there is no portable container state or open/transfer interaction. |
| Armour stands | Missing | No entity implementation or interactions. |

## Player actions, combat, and survival audit

| Action or system | Java | Bedrock | Gap |
| --- | --- | --- | --- |
| Movement, sprint, sneak, flight permission | Implemented | Implemented | Swimming, crawling, gliding, pose transitions, collision validation, and speed-effect integration are incomplete. |
| Drop one stack / drop one item | Implemented (new) | Implemented | Java now handles Player Action statuses 3 and 4; Bedrock inventory transactions already supported drops. |
| Swap main hand and offhand | Implemented (new) | Partial | Java now handles Player Action status 6. Bedrock inventory swaps exist, but use actions still assume a hotbar item. |
| Offhand use | Missing | Missing | Java acknowledges and ignores non-main-hand `Use Item`; Bedrock use intents carry only a hotbar slot. Shields, food, rockets, and utility items therefore cannot use normal offhand semantics. |
| Melee attack | Partial | Partial | Basic damage, range, cooldown gate, armour/toughness, knockback, mace fall bonus, and durability exist. Critical hits, sweeping attacks, attack-speed-per-item timing, fire aspect, damage/knockback enchantments, shield disable, statistics, and exhaustion are missing. |
| Armour and defensive enchantments | Partial | Partial | Base armour/toughness/knockback resistance work. Protection families, Feather Falling, Thorns, Respiration, Aqua Affinity, Soul Speed, Swift Sneak, Frost Walker, and equipment-triggered effects are not applied. |
| Projectile combat | Partial | Partial | Shared collision/damage exists. Tipped/spectral effects, arrow embedding/pickup, criticals, bow/crossbow/trident enchantments, owner rules, and shield interaction are incomplete. |
| Status effects | Implemented (new) | Implemented (new) | Canonical effect collection with a tick/expiry engine, periodic damage/heal, defensive-damage modification, cure, persistence, and cross-edition sync. Full attribute-modifier breadth and immunity rules still need conformance tests. |
| Hunger, regeneration, starvation | Partial | Partial | Core hunger/exhaustion and natural regeneration/starvation exist. Activity costs, difficulty rules, status-effect interaction, peaceful behaviour, and all exhaustion sources are incomplete. |
| Fall, fire, lava, cactus, berry, void, drowning | Partial | Partial | Main damage paths exist. Effect/enchantment mitigation, fire ticks, freezing/powder snow, suffocation, cramming, border damage, lightning, and many block hazards are absent or simplified. |
| Sleep and respawn | Partial | Partial | Beds set spawn and sleep at night. Occupancy, monsters nearby, dimension explosions, obstruction, sleep percentage/gamerules, anchor respawn, charge use, and exact wake placement remain. |

## Container and automation audit

- Ender chests are correctly backed by each player's `EnderChestInventory` on
  both adapters; the previous generic-world-container concern no longer applies.
- Ordinary chests, double chests, barrels, furnaces, hoppers, dispensers,
  droppers, crafters, and placed shulker boxes have canonical storage and basic
  adapter UIs.
- Shulker boxes use the wrong break contract: contents spill into the world and
  the dropped box cannot retain them.
- Hopper insertion filtering is specialised only for furnaces. Brewing stands
  and other sided inventories accept items too generically, and hopper
  minecarts/chest minecarts do not expose storage.
- Dispensers support a useful subset: basic projectiles, TNT, water/lava
  buckets, bone meal, flint and steel, and spawn eggs. Missing families include
  armour/equipment, boats/minecarts, shears, glass bottles, honeycomb, skulls,
  pumpkins, shulker placement, and correct potion/arrow payloads.
- Enchanting, brewing, beacon, loom, cartography, and smithing screens may open
  successfully even when their defining operation is absent. UI-open success
  must not be used as a parity signal.

## Bedrock/Java version-skew exclusions

Dragonfly v0.11.0 contains newer Bedrock content such as copper golem statues,
copper lanterns/torches/chains/bars, additional copper oxidation families, and
other items not present in Java 1.21.4. Those features are **N/A for Java
parity**, not Java bugs. They still require a separate Bedrock 1.26.45 audit for
placement, activation, oxidation, waxing, loot, recipes, entities, and network
states. GoCraft should not expose them to Java 1.21.4 unless it intentionally
defines them as custom content with a resource pack.

## Implementation order

1. Add extensible canonical item components and update inventory equality,
   persistence, Java component codecs, Bedrock NBT codecs, containers, dropped
   items, and recipes together.
2. Add a canonical player status-effect engine. Route foods, potions, beacons,
   totems, mobs, commands, milk, and adapter packets through it.
3. Replace the edition-specific block-use switches with shared item-use and
   block-activation operations; first port the nine confirmed Bedrock-only
   interactions to Java.
4. Add canonical hand/use state, charged-crossbow state, and gliding. Then wire
   Bedrock bow/crossbow/trident/shield use and Elytra rockets.
5. Implement the stateful block entities: jukebox, lectern, chiseled bookshelf,
   signs, banners, item frames, and portable shulker contents.
6. Implement missing entity interactions: buckets/milking, sheep, leads, name
   tags, animal equipment, and vehicle inventories.
7. Add random-tick/neighbor behaviours for sponge, coral, cactus, sugar cane,
   bamboo, kelp, and vines.
8. Add adapter conformance tests that run the same canonical interaction
   scenario through Java and Bedrock inputs and compare world, player,
   inventory, entity, sound, particle, and persistence results.

## Inspected source areas

- GoCraft canonical state: `core/player`, `core/entity`, `core/world`,
  `core/itemregistry`, `core/blockloot`, and `core/intent`.
- GoCraft Java actions: `java/handler/block.go`, `inventory.go`,
  `projectile.go`, `health.go`, `boat.go`, `trade.go`, `chest.go`,
  `workstation.go`, `crafting.go`, and `play.go`.
- GoCraft Bedrock actions: `bedrock/listener.go`, `bedrock/sync.go`,
  `server/bedrock_actions.go`, `bedrock_container.go`, `server.go`,
  `container_automation.go`, `animal_interaction.go`, and `firework.go`.
- Dragonfly reference: `server/item`, `server/block`, item NBT/component types,
  and their activation/use interfaces in the v0.11.0 module source.

This document is a code-derived backlog. Each `Missing` or `Partial` row should
become a focused implementation issue and a cross-edition test before it is
changed to `Implemented`.
