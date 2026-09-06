package world

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"GoCraft/core/entity"
	"GoCraft/core/spatial"
	"GoCraft/internal/debuglog"
)

// World holds all chunks that have been loaded or generated, and delegates
// chunk creation to a Generator for positions not yet in cache.
//
// If a Storage is provided, World tries to load chunks from it before
// generating, and marks modified chunks dirty for flushing on Close.
//
// All public methods are safe for concurrent use.  Note that chunk content
// (Section.Set) is not protected by the world mutex; concurrent modification
// of the same chunk column from multiple goroutines is a known M8 limitation
// that will be addressed by per-chunk locking in a later milestone.
type World struct {
	mu              sync.RWMutex
	chunks          map[[2]int32]*Chunk
	inflight        map[[2]int32]chan struct{}
	generator       Generator
	storage         Storage // nil = no persistence
	dirty           map[[2]int32]struct{}
	flushing        map[[2]int32]struct{}
	lastAccess      map[[2]int32]uint64
	accessClock     uint64
	maxCachedChunks int

	blockObserverMu       sync.RWMutex
	blockObserver         func(BlockChange)
	blockEntityObserverMu sync.RWMutex
	blockEntityObserver   func(BlockEntity)
	vibrationMu           sync.Mutex
	vibrations            map[[3]int]struct{}

	pregenQueue  chan [2]int32
	pregenQueued map[[2]int32]struct{}
	pregenStop   chan struct{}
	pregenWG     sync.WaitGroup
	closeOnce    sync.Once

	// Entities holds all non-player entities (mobs, dropped items, etc.).
	// It is initialised by New and safe for concurrent use.
	Entities *entity.Manager

	// Village entity spawning
	villagersEnabled bool
	spawnedVillages  map[[2]int]struct{}
	spawnedEntities  []*entity.Entity
	spawnedMu        sync.Mutex
	nextVillagerID   atomic.Int32

	// Network handlers enqueue combat here; the entity tick goroutine drains
	// and applies it so health retains a single writer.
	damageMu      sync.Mutex
	pendingDamage map[int32]EntityDamage

	// Container block entities share canonical contents across Java sessions.
	containerMu sync.RWMutex

	// BlockPhysics schedules falling-block and fluid-spread block ticks.
	BlockPhysics *BlockPhysics

	// Redstone propagates redstone signal through the world.
	Redstone *RedstoneEngine

	// worldTime is the current time-of-day (0–23999) published by the server
	// tick goroutine once per second so handler code can read it atomically.
	worldTime     atomic.Int64
	physicsTime   atomic.Int64
	lavaTickDelay atomic.Int64
	ultrawarm     atomic.Bool

	// requestTimeSkip is set by the bed-sleep handler and drained by the
	// server tick goroutine which then advances worldAge to the next morning.
	requestTimeSkip atomic.Bool

	// Loaded village POIs allow saved/imported villages to receive residents
	// even when they were not placed by the current generator revision.
	villagePOIMu         sync.Mutex
	villageBeds          map[spatial.BlockPos]struct{}
	villageDoors         map[spatial.BlockPos]Block
	villageJobs          map[spatial.BlockPos]entity.VillagerProfession
	spawnedVillageHomes  map[spatial.BlockPos]struct{}
	spawnedVillageGuards map[[2]int]struct{}
}

// ChunkIfLoaded returns a cached chunk without loading or generating terrain.
// Network-side derived data such as light updates use this to avoid expanding
// the world merely because a mutation occurred near a chunk border.
func (w *World) ChunkIfLoaded(cx, cz int32) (*Chunk, bool) {
	if w == nil {
		return nil, false
	}
	w.mu.RLock()
	chunk, ok := w.chunks[[2]int32{cx, cz}]
	w.mu.RUnlock()
	return chunk, ok
}

// SetBlockObserver installs an adapter-neutral notification invoked after each
// canonical block mutation. It lets secondary protocol adapters mirror Java,
// physics, redstone, and command changes without importing networking into core.
func (w *World) SetBlockObserver(observer func(BlockChange)) {
	w.blockObserverMu.Lock()
	w.blockObserver = observer
	w.blockObserverMu.Unlock()
}

func (w *World) notifyBlockObserver(x, y, z int, previous, block Block) {
	w.blockObserverMu.RLock()
	observer := w.blockObserver
	w.blockObserverMu.RUnlock()
	if observer != nil {
		observer(BlockChange{X: x, Y: y, Z: z, Previous: previous, Block: block})
	}
}

// SetBlockEntityObserver installs an adapter-neutral notification invoked after
// canonical block-entity data changes. The snapshot passed to the observer owns
// its Data and Items slices and may safely outlive the world mutation.
func (w *World) SetBlockEntityObserver(observer func(BlockEntity)) {
	w.blockEntityObserverMu.Lock()
	w.blockEntityObserver = observer
	w.blockEntityObserverMu.Unlock()
}

func (w *World) notifyBlockEntityObserver(entity BlockEntity) {
	w.blockEntityObserverMu.RLock()
	observer := w.blockEntityObserver
	w.blockEntityObserverMu.RUnlock()
	if observer == nil {
		return
	}
	entity.Data = append([]byte(nil), entity.Data...)
	entity.Items = append([]ContainerItem(nil), entity.Items...)
	observer(entity)
}

// EntityDamage is a simulation-thread damage event. Source coordinates are
// optional and allow passive AI to flee from an attacker without mutating an
// entity from a network goroutine.
type EntityDamage struct {
	Amount           float32
	SourceX, SourceZ float64
	HasSource        bool
	SourcePlayerUUID [16]byte
	HasPlayerSource  bool
}

// New creates an empty world that generates chunks with gen on demand.
// storage may be nil for a generation-only world with no persistence.
func New(gen Generator, storage Storage, villagersEnabled bool) *World {
	w := &World{
		chunks:               make(map[[2]int32]*Chunk),
		inflight:             make(map[[2]int32]chan struct{}),
		generator:            gen,
		storage:              storage,
		dirty:                make(map[[2]int32]struct{}),
		flushing:             make(map[[2]int32]struct{}),
		lastAccess:           make(map[[2]int32]uint64),
		maxCachedChunks:      768,
		pregenQueue:          make(chan [2]int32, 4096),
		pregenQueued:         make(map[[2]int32]struct{}),
		pregenStop:           make(chan struct{}),
		Entities:             entity.NewManager(),
		villagersEnabled:     villagersEnabled,
		spawnedVillages:      make(map[[2]int]struct{}),
		pendingDamage:        make(map[int32]EntityDamage),
		vibrations:           make(map[[3]int]struct{}),
		villageBeds:          make(map[spatial.BlockPos]struct{}),
		villageDoors:         make(map[spatial.BlockPos]Block),
		villageJobs:          make(map[spatial.BlockPos]entity.VillagerProfession),
		spawnedVillageHomes:  make(map[spatial.BlockPos]struct{}),
		spawnedVillageGuards: make(map[[2]int]struct{}),
		BlockPhysics:         newBlockPhysics(),
	}
	w.Redstone = newRedstoneEngine(w)
	w.lavaTickDelay.Store(30)
	const workers = 2
	w.pregenWG.Add(workers)
	for i := 0; i < workers; i++ {
		go w.pregenerationWorker()
	}
	return w
}

// EmitVibration queues a game event that nearby sculk sensors may detect on
// the simulation tick. Coordinates identify the sound/event source block.
func (w *World) EmitVibration(x, y, z int) {
	if w == nil {
		return
	}
	w.vibrationMu.Lock()
	w.vibrations[[3]int{x, y, z}] = struct{}{}
	w.vibrationMu.Unlock()
}

// DrainVibrations returns the deduplicated game events queued since the last
// simulation tick.
func (w *World) DrainVibrations() [][3]int {
	if w == nil {
		return nil
	}
	w.vibrationMu.Lock()
	events := make([][3]int, 0, len(w.vibrations))
	for event := range w.vibrations {
		events = append(events, event)
	}
	clear(w.vibrations)
	w.vibrationMu.Unlock()
	return events
}

// Chunk returns the chunk at (x, z).  Lookup order:
//  1. In-memory cache — O(1), no I/O.
//  2. Storage backend (if present) — reads from disk.
//  3. Generator — creates a fresh chunk procedurally.
//
// Callers may retain the returned chunk pointer, but a later lookup can reload
// an evicted clean chunk as a new instance.
func (w *World) Chunk(x, z int32) *Chunk {
	key := [2]int32{x, z}

	// Fast path: already cached. If a background worker is already generating
	// the same chunk, wait for it instead of doing the expensive work twice.
	// A clean chunk may be evicted between the producer closing ready and a
	// waiter reacquiring the mutex. Retry that lookup instead of returning a nil
	// chunk to callers, all of which rely on Chunk's non-nil contract.
	var generatedReady chan struct{}
	for {
		w.mu.Lock()
		if c, ok := w.chunks[key]; ok {
			w.touchChunkLocked(key)
			w.mu.Unlock()
			return c
		}
		if ready, ok := w.inflight[key]; ok {
			w.mu.Unlock()
			<-ready
			w.mu.Lock()
			c, ok := w.chunks[key]
			if ok {
				w.touchChunkLocked(key)
			}
			w.mu.Unlock()
			if ok && c != nil {
				return c
			}
			continue
		}
		generatedReady = make(chan struct{})
		w.inflight[key] = generatedReady
		w.mu.Unlock()
		break
	}

	// Try loading from storage before generating.
	var loaded *Chunk
	if w.storage != nil {
		c, err := w.storage.LoadChunk(x, z)
		if err != nil {
			slog.Warn("world: failed to load chunk from storage",
				"x", x, "z", z, "err", err)
		} else if c != nil {
			loaded = c
		}
	}

	// Fall back to the generator if storage had nothing. Freshly generated
	// disk-backed chunks are dirty until their first Anvil save.
	generated := loaded == nil
	if generated {
		loaded = w.generator.Generate(x, z)
	}
	if loaded == nil {
		// Keep malformed custom generators from violating Chunk's non-nil contract.
		slog.Error("world: generator returned nil chunk", "x", x, "z", z)
		loaded = &Chunk{X: x, Z: z}
	}

	// Free per-section palette dedup maps after loading/generating is done.
	// They are lazily rebuilt the first time a Set() call modifies the section.
	// This recovers ~5-15 KB per surface section with many distinct block states.
	for _, sec := range loaded.Sections {
		if sec != nil {
			sec.Finalize()
		}
	}

	w.mu.Lock()
	w.chunks[key] = loaded
	w.touchChunkLocked(key)
	if generated && w.storage != nil {
		w.dirty[key] = struct{}{}
	}
	delete(w.inflight, key)
	close(generatedReady)
	w.trimChunksLocked()
	w.mu.Unlock()

	if w.villagersEnabled {
		if og, ok := w.generator.(*OverworldGenerator); ok {
			for _, vc := range og.VillageCentersNear(x, z) {
				vkey := [2]int{vc.WorldX, vc.WorldZ}
				w.spawnedMu.Lock()
				_, already := w.spawnedVillages[vkey]
				if !already {
					w.spawnedVillages[vkey] = struct{}{}
				}
				w.spawnedMu.Unlock()
				if already {
					continue
				}
				residents := og.VillageResidents(vc)
				for i, resident := range residents {
					id := w.nextVillagerID.Add(1) + 10_000_000
					var uuid [16]byte
					binary.BigEndian.PutUint64(uuid[:8], uint64(vc.WorldX)*0x9e3779b185ebca87^uint64(vc.WorldZ)*0xc2b2ae3d27d4eb4f)
					binary.BigEndian.PutUint64(uuid[8:], uint64(i+1)*0x6c62272e07bb0142^villageSalt)
					v := entity.New(int32(id), uuid, entity.TypeVillager,
						float64(resident.Spawn.X)+0.5, float64(resident.Spawn.Y), float64(resident.Spawn.Z)+0.5)
					v.VillagerVariant = villagerVariantForBiome(vc.Biome)
					v.VillagerProfession = resident.Profession
					v.VillagerLevel = 1
					v.HasVillageHome = true
					v.VillageHome = resident.Home
					v.VillageCenter = resident.Center
					v.VillageBed = resident.Bed
					v.VillageWorkstation = resident.Workstation
					v.HasVillageWorkstation = resident.Profession != entity.VillagerProfessionNone && resident.Workstation != (spatial.BlockPos{})
					v.OnGround = true
					w.Entities.Add(v)
					w.spawnedMu.Lock()
					w.spawnedEntities = append(w.spawnedEntities, v)
					w.spawnedMu.Unlock()
				}

				// Every generated village receives one iron golem guard homed to
				// its center, so the passive movement loop keeps it on patrol.
				guardID := w.nextVillagerID.Add(1) + 10_000_000
				var guardUUID [16]byte
				binary.BigEndian.PutUint64(guardUUID[:8], uint64(vc.WorldX)*0x9e3779b185ebca87^uint64(vc.WorldZ)*0xc2b2ae3d27d4eb4f)
				binary.BigEndian.PutUint64(guardUUID[8:], 0x69726f6e676f6c65^villageSalt)
				guardX, guardZ := vc.WorldX+4, vc.WorldZ
				guardY := og.SurfaceHeight(guardX, guardZ) + 1
				guard := entity.New(guardID, guardUUID, entity.TypeIronGolem, float64(guardX)+0.5, float64(guardY), float64(guardZ)+0.5)
				guard.HasVillageHome = true
				guard.VillageHome = spatial.BlockPos{X: int32(vc.WorldX), Y: int32(og.SurfaceHeight(vc.WorldX, vc.WorldZ) + 1), Z: int32(vc.WorldZ)}
				guard.VillageCenter = guard.VillageHome
				guard.OnGround = true
				w.Entities.Add(guard)
				w.spawnedMu.Lock()
				w.spawnedEntities = append(w.spawnedEntities, guard)
				w.spawnedMu.Unlock()
				debuglog.Info(debuglog.MobSpawning, "generated village population spawned", "centerX", vc.WorldX, "centerZ", vc.WorldZ, "villagers", len(residents), "ironGolems", 1)
			}
		}
	}
	w.discoverLoadedVillagePopulation(loaded)
	return loaded
}

// DrainSpawnedEntities returns entities created by chunk generation since the
// previous drain. Adapters use this edition-neutral event queue to announce
// newly generated villagers exactly once.
func (w *World) DrainSpawnedEntities() []*entity.Entity {
	w.spawnedMu.Lock()
	spawned := append([]*entity.Entity(nil), w.spawnedEntities...)
	w.spawnedEntities = nil
	w.spawnedMu.Unlock()
	return spawned
}

func villagerVariantForBiome(biome string) entity.VillagerVariant {
	switch biome {
	case "minecraft:desert":
		return entity.VillagerVariantDesert
	case "minecraft:savanna":
		return entity.VillagerVariantSavanna
	case "minecraft:taiga":
		return entity.VillagerVariantTaiga
	case "minecraft:snowy_plains":
		return entity.VillagerVariantSnow
	default:
		return entity.VillagerVariantPlains
	}
}

func villagerProfessionForIndex(index int) entity.VillagerProfession {
	professions := [...]entity.VillagerProfession{
		entity.VillagerProfessionFarmer,
		entity.VillagerProfessionLibrarian,
		entity.VillagerProfessionFletcher,
		entity.VillagerProfessionToolsmith,
		entity.VillagerProfessionArmorer,
	}
	return professions[index%len(professions)]
}

// SetMaxCachedChunks sets the maximum number of clean chunks retained in RAM.
// Dirty chunks are never evicted before they have been persisted.
func (w *World) SetMaxCachedChunks(limit int) {
	if limit < 128 {
		limit = 128
	}
	w.mu.Lock()
	w.maxCachedChunks = limit
	w.trimChunksLocked()
	w.mu.Unlock()
}

func (w *World) touchChunkLocked(key [2]int32) {
	w.accessClock++
	w.lastAccess[key] = w.accessClock
}

func (w *World) trimChunksLocked() {
	for len(w.chunks) > w.maxCachedChunks {
		var oldestKey [2]int32
		oldestAccess := ^uint64(0)
		found := false
		for key := range w.chunks {
			if _, dirty := w.dirty[key]; dirty {
				continue
			}
			if _, flushing := w.flushing[key]; flushing {
				continue
			}
			if access := w.lastAccess[key]; !found || access < oldestAccess {
				oldestKey, oldestAccess, found = key, access, true
			}
		}
		if !found {
			return
		}
		delete(w.chunks, oldestKey)
		delete(w.lastAccess, oldestKey)
	}
}

// QueueEntityDamage schedules damage for the entity tick goroutine. It returns
// false if the entity does not exist or amount is invalid. Dead entities are ignored by the tick.
func (w *World) QueueEntityDamage(entityID int32, amount float32) bool {
	return w.queueEntityDamage(entityID, amount, 0, 0, false, [16]byte{}, false)
}

// QueueEntityDamageFrom queues damage with the horizontal position of its
// source, allowing passive mobs to panic away from the attacker.
func (w *World) QueueEntityDamageFrom(entityID int32, amount float32, sourceX, sourceZ float64) bool {
	return w.queueEntityDamage(entityID, amount, sourceX, sourceZ, true, [16]byte{}, false)
}

// QueueEntityImpactFrom records a zero-health-damage hit with a source. It is
// used by projectiles such as snowballs that still trigger the vanilla hurt
// reaction and knockback without reducing ordinary entity health.
func (w *World) QueueEntityImpactFrom(entityID int32, sourceX, sourceZ float64) bool {
	if _, ok := w.Entities.Get(entityID); !ok {
		return false
	}
	w.damageMu.Lock()
	w.pendingDamage[entityID] = EntityDamage{Amount: 0, SourceX: sourceX, SourceZ: sourceZ, HasSource: true}
	w.damageMu.Unlock()
	return true
}

// QueueEntityDamageFromPlayer queues player-caused damage. The UUID is kept in
// the edition-neutral event so Java and Bedrock kills award identical XP.
func (w *World) QueueEntityDamageFromPlayer(entityID int32, amount float32, sourceX, sourceZ float64, playerUUID [16]byte) bool {
	return w.queueEntityDamage(entityID, amount, sourceX, sourceZ, true, playerUUID, true)
}

func (w *World) queueEntityDamage(entityID int32, amount float32, sourceX, sourceZ float64, hasSource bool, playerUUID [16]byte, hasPlayerSource bool) bool {
	if amount <= 0 {
		return false
	}
	if _, ok := w.Entities.Get(entityID); !ok {
		return false
	}
	w.damageMu.Lock()
	event := w.pendingDamage[entityID]
	event.Amount += amount
	if hasSource {
		event.SourceX, event.SourceZ, event.HasSource = sourceX, sourceZ, true
	}
	if hasPlayerSource {
		event.SourcePlayerUUID, event.HasPlayerSource = playerUUID, true
	}
	w.pendingDamage[entityID] = event
	w.damageMu.Unlock()
	return true
}

// DrainEntityDamage transfers all queued damage to the simulation tick.
func (w *World) DrainEntityDamage() map[int32]EntityDamage {
	w.damageMu.Lock()
	pending := w.pendingDamage
	w.pendingDamage = make(map[int32]EntityDamage)
	w.damageMu.Unlock()
	return pending
}

// QueuePregeneration schedules every chunk in the square around (cx, cz),
// centre-out. Calls are cheap and duplicate coordinates are coalesced. The
// workers populate the normal in-memory cache, so a foreground player reaching
// a queued chunk does not wait for terrain generation.
func (w *World) QueuePregeneration(cx, cz, radius int32) int {
	if radius < 0 {
		return 0
	}
	queued := 0
	for ring := int32(0); ring <= radius; ring++ {
		for dx := -ring; dx <= ring; dx++ {
			for dz := -ring; dz <= ring; dz++ {
				if ring > 0 && abs32World(dx) != ring && abs32World(dz) != ring {
					continue
				}
				key := [2]int32{cx + dx, cz + dz}
				w.mu.Lock()
				_, loaded := w.chunks[key]
				_, loading := w.inflight[key]
				_, pending := w.pregenQueued[key]
				if loaded || loading || pending {
					w.mu.Unlock()
					continue
				}
				w.pregenQueued[key] = struct{}{}
				select {
				case w.pregenQueue <- key:
					queued++
				default:
					delete(w.pregenQueued, key)
				}
				w.mu.Unlock()
			}
		}
	}
	return queued
}

func (w *World) pregenerationWorker() {
	defer w.pregenWG.Done()
	for {
		select {
		case <-w.pregenStop:
			return
		case key := <-w.pregenQueue:
			w.Chunk(key[0], key[1])
			w.mu.Lock()
			delete(w.pregenQueued, key)
			w.mu.Unlock()
		}
	}
}

func abs32World(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// Seed returns the world generation seed, or 0 for non-OverworldGenerator worlds.
func (w *World) Seed() int64 {
	if g, ok := w.generator.(*OverworldGenerator); ok {
		return g.Seed()
	}
	return 0
}

// NearestVillage locates the closest village generated by GoCraft. Worlds
// backed by another generator report no result.
func (w *World) NearestVillage(x, z, maxDistance int) (VillageCenter, bool) {
	generator, ok := w.generator.(*OverworldGenerator)
	if !ok {
		return VillageCenter{}, false
	}
	return generator.NearestVillage(x, z, maxDistance)
}

// NearestStronghold locates the closest Pumpkin concentric-ring position.
func (w *World) NearestStronghold(x, z, maxChunks int) (int, int, bool) {
	generator, ok := w.generator.(*OverworldGenerator)
	if !ok {
		return 0, 0, false
	}
	return generator.NearestStronghold(x, z, maxChunks)
}

// NearestBiome locates a nearby sample of a GoCraft-generated biome. Worlds
// backed by another generator report no result.
func (w *World) NearestBiome(x, z int, target string, maxDistance int) (int, int, bool) {
	generator, ok := w.generator.(*OverworldGenerator)
	if !ok {
		return 0, 0, false
	}
	return generator.NearestBiome(x, z, target, maxDistance)
}

// BiomeAt returns the canonical biome resource location at a block position.
// Dimension generators can answer this without loading a chunk, which lets
// the natural spawner use biome-specific spawn tables without adding disk I/O
// to the tick goroutine. Generators without biome sampling fall back to plains.
func (w *World) BiomeAt(x, y, z int) string {
	generator, ok := w.generator.(interface {
		BiomeAt3D(x, y, z int) string
	})
	if !ok {
		return "minecraft:plains"
	}
	biome := generator.BiomeAt3D(x, y, z)
	if strings.Contains(biome, ":") {
		return biome
	}
	return "minecraft:" + biome
}

// IsChunkLoaded reports whether the chunk at chunk coordinates (cx, cz) is
// already in the in-memory cache.  The check is non-blocking and never
// triggers disk I/O or chunk generation.
func (w *World) IsChunkLoaded(cx, cz int32) bool {
	w.mu.RLock()
	_, ok := w.chunks[[2]int32{cx, cz}]
	w.mu.RUnlock()
	return ok
}

// ChunkCoordsFor converts absolute block coordinates to the chunk coordinate
// pair that contains them.
func ChunkCoordsFor(x, z int) (cx, cz int32) {
	return int32(math.Floor(float64(x) / SectionSize)),
		int32(math.Floor(float64(z) / SectionSize))
}

// SurfaceYIfLoaded returns the highest non-air block Y and true when the
// containing chunk is already cached, or (0, false) without triggering any
// disk I/O or generation when it is not.
func (w *World) SurfaceYIfLoaded(x, z int) (int, bool) {
	cx, cz := ChunkCoordsFor(x, z)
	if !w.IsChunkLoaded(cx, cz) {
		return 0, false
	}
	return w.SurfaceY(x, z), true
}

// CanEntityOccupyIfLoaded is the non-blocking variant of CanEntityOccupy. It
// returns (false, false) — meaning "blocked, but don't trust this answer" —
// when any of the sampled positions fall inside an unloaded chunk.  The caller
// should skip the move rather than trigger disk I/O on the tick goroutine.
func (w *World) CanEntityOccupyIfLoaded(x, y, z float64) (ok bool, loaded bool) {
	for _, sampleX := range [...]float64{x - 0.3, x + 0.3} {
		for _, sampleZ := range [...]float64{z - 0.3, z + 0.3} {
			cx, cz := ChunkCoordsFor(int(math.Floor(sampleX)), int(math.Floor(sampleZ)))
			if !w.IsChunkLoaded(cx, cz) {
				return false, false
			}
		}
	}
	return w.CanEntityOccupy(x, y, z), true
}

// SurfaceY returns the highest non-air block at absolute x/z, loading or
// generating the containing chunk when necessary.
func (w *World) SurfaceY(x, z int) int {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	localX := x - int(cx)*SectionSize
	localZ := z - int(cz)*SectionSize
	return w.Chunk(cx, cz).HighestBlockY(localX, localZ)
}

// GroundYAtOrBelow returns the highest non-air support block at or below maxY.
// Unlike SurfaceY, this can find an interior floor beneath a roof.
func (w *World) GroundYAtOrBelow(x, z, maxY int) int {
	if maxY > WorldMaxY {
		maxY = WorldMaxY
	}
	for y := maxY; y >= WorldMinY; y-- {
		block := w.GetBlock(x, y, z)
		if entitySupportBlock(block.ResourceLocation()) &&
			!entitySupportBlock(w.GetBlock(x, y+1, z).ResourceLocation()) &&
			!entitySupportBlock(w.GetBlock(x, y+2, z).ResourceLocation()) {
			return y
		}
	}
	return WorldMinY - 1
}

// CanEntityOccupy reports whether a standard passive-mob collision box can
// occupy the position without intersecting solid blocks.
func (w *World) CanEntityOccupy(x, y, z float64) bool {
	for _, sampleX := range [...]float64{x - 0.3, x + 0.3} {
		for _, sampleZ := range [...]float64{z - 0.3, z + 0.3} {
			for blockY := int(math.Floor(y)); blockY <= int(math.Floor(y))+1; blockY++ {
				if entitySupportBlock(w.GetBlock(int(math.Floor(sampleX)), blockY, int(math.Floor(sampleZ))).ResourceLocation()) {
					return false
				}
			}
		}
	}
	return true
}

func entitySupportBlock(name string) bool {
	switch name {
	case "", "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:water", "minecraft:bubble_column", "minecraft:lava", "minecraft:nether_portal", "minecraft:end_portal", "minecraft:end_gateway", "minecraft:short_grass",
		"minecraft:grass", "minecraft:tall_grass", "minecraft:fern",
		"minecraft:large_fern", "minecraft:wheat", "minecraft:carrots",
		"minecraft:potatoes", "minecraft:beetroots", "minecraft:dandelion",
		"minecraft:poppy", "minecraft:allium", "minecraft:azure_bluet",
		"minecraft:red_tulip", "minecraft:orange_tulip", "minecraft:white_tulip",
		"minecraft:pink_tulip", "minecraft:cornflower", "minecraft:oxeye_daisy",
		"minecraft:lily_of_the_valley", "minecraft:blue_orchid", "minecraft:lilac",
		"minecraft:rose_bush", "minecraft:peony", "minecraft:sunflower",
		"minecraft:wither_rose", "minecraft:torchflower", "minecraft:dead_bush",
		"minecraft:seagrass", "minecraft:tall_seagrass", "minecraft:vine",
		"minecraft:moss_carpet", "minecraft:brown_mushroom", "minecraft:red_mushroom",
		"minecraft:oak_sapling", "minecraft:spruce_sapling", "minecraft:birch_sapling",
		"minecraft:jungle_sapling", "minecraft:acacia_sapling", "minecraft:dark_oak_sapling",
		"minecraft:cherry_sapling", "minecraft:mangrove_propagule", "minecraft:fire":
		return false
	default:
		return true
	}
}

// IsEntitySupportBlock reports whether entities and projectiles collide with
// the block rather than passing through it.
func IsEntitySupportBlock(name string) bool {
	return entitySupportBlock(name)
}

// BlockChange describes a canonical world mutation that adapters can broadcast.
type BlockChange struct {
	X, Y, Z  int
	Previous Block
	Block    Block
}

// TickFarmland applies vanilla hydration: water within four horizontal blocks
// at the farmland level or one block above sets moisture to 7. Dry farmland
// loses one moisture level per update and eventually returns to dirt when it
// has no crop above it.
func (w *World) TickFarmland(tick int64, maxChanges int) []BlockChange {
	if maxChanges <= 0 {
		return nil
	}
	w.mu.RLock()
	chunks := make([]*Chunk, 0, len(w.chunks))
	for _, chunk := range w.chunks {
		chunks = append(chunks, chunk)
	}
	w.mu.RUnlock()

	changes := make([]BlockChange, 0, maxChanges)
	for _, chunk := range chunks {
		for sectionIndex, section := range chunk.Sections {
			if section == nil || section.NonAir == 0 {
				continue
			}
			palette := section.BlockPalette()
			hasFarmland := false
			for _, block := range palette {
				if block.ResourceLocation() == "minecraft:farmland" {
					hasFarmland = true
					break
				}
			}
			if !hasFarmland {
				continue
			}
			for index, paletteIndex := range section.BlockData() {
				if int(paletteIndex) >= len(palette) {
					continue
				}
				block := palette[paletteIndex]
				if block.ResourceLocation() != "minecraft:farmland" {
					continue
				}
				localX := index % SectionSize
				localZ := (index / SectionSize) % SectionSize
				localY := index / (SectionSize * SectionSize)
				x := int(chunk.X)*SectionSize + localX
				y := SectionMinY(sectionIndex) + localY
				z := int(chunk.Z)*SectionSize + localZ
				hash := uint64(int64(x)*0x9e3779b1) ^ uint64(int64(y)*0x85ebca77) ^
					uint64(int64(z)*0xc2b2ae3d) ^ uint64((tick/20)*0x27d4eb2d)
				if hash%5 != 0 {
					continue
				}
				moisture, _ := strconv.Atoi(block.Properties["moisture"])
				var replacement Block
				above, aboveLoaded := w.blockIfLoaded(x, y+1, z)
				if aboveLoaded && farmlandBlockedAbove(above) {
					replacement = Block{Namespace: "minecraft", Name: "dirt"}
				} else if w.hasFarmlandWater(x, y, z) {
					if moisture >= 7 {
						continue
					}
					replacement = copyWorldBlock(block)
					replacement.Properties["moisture"] = "7"
				} else if moisture > 0 {
					replacement = copyWorldBlock(block)
					replacement.Properties["moisture"] = strconv.Itoa(moisture - 1)
				} else if !isFarmlandCrop(w.GetBlock(x, y+1, z).ResourceLocation()) {
					replacement = Block{Namespace: "minecraft", Name: "dirt"}
				} else {
					continue
				}
				changes = append(changes, BlockChange{X: x, Y: y, Z: z, Block: replacement})
				if len(changes) >= maxChanges {
					break
				}
			}
			if len(changes) >= maxChanges {
				break
			}
		}
		if len(changes) >= maxChanges {
			break
		}
	}
	for _, change := range changes {
		w.SetBlock(change.X, change.Y, change.Z, change.Block)
	}
	return changes
}

func farmlandBlockedAbove(block Block) bool {
	if block.IsAir() || IsFluidBlock(block.ResourceLocation()) || isFarmlandCrop(block.ResourceLocation()) {
		return false
	}
	return IsEntitySupportBlock(block.ResourceLocation())
}

func (w *World) hasFarmlandWater(x, y, z int) bool {
	for dx := -4; dx <= 4; dx++ {
		for dz := -4; dz <= 4; dz++ {
			for dy := 0; dy <= 1; dy++ {
				wx, wz := x+dx, z+dz
				cx, cz := ChunkCoordsFor(wx, wz)
				if !w.IsChunkLoaded(cx, cz) {
					continue
				}
				water := w.GetBlock(wx, y+dy, wz)
				if water.ResourceLocation() == "minecraft:water" || water.Properties["waterlogged"] == "true" {
					return true
				}
			}
		}
	}
	return false
}

func isFarmlandCrop(name string) bool {
	switch name {
	case "minecraft:wheat", "minecraft:carrots", "minecraft:potatoes",
		"minecraft:beetroots", "minecraft:melon_stem", "minecraft:pumpkin_stem",
		"minecraft:attached_melon_stem", "minecraft:attached_pumpkin_stem",
		"minecraft:torchflower_crop", "minecraft:pitcher_crop":
		return true
	}
	return false
}

func copyWorldBlock(block Block) Block {
	properties := make(map[string]string, len(block.Properties))
	for key, value := range block.Properties {
		properties[key] = value
	}
	block.Properties = properties
	return block
}

// ContainerItems returns a snapshot of the canonical items stored at a block position.
// Missing containers and empty slots are represented by an empty slice.
func (w *World) ContainerItems(x, y, z int) []ContainerItem {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)

	w.containerMu.RLock()
	defer w.containerMu.RUnlock()
	for _, blockEntity := range c.BlockEntities {
		if blockEntity.X == x && blockEntity.Y == y && blockEntity.Z == z {
			items := make([]ContainerItem, len(blockEntity.Items))
			copy(items, blockEntity.Items)
			return items
		}
	}
	return nil
}

// GetBlockEntity returns a snapshot of the block entity at (x, y, z), or a
// zero-value BlockEntity when none is stored there.
func (w *World) GetBlockEntity(x, y, z int) BlockEntity {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)
	w.containerMu.RLock()
	defer w.containerMu.RUnlock()
	for _, entity := range c.BlockEntities {
		if entity.X == x && entity.Y == y && entity.Z == z {
			copy := entity
			copy.Items = append([]ContainerItem(nil), entity.Items...)
			copy.Data = append([]byte(nil), entity.Data...)
			return copy
		}
	}
	return BlockEntity{}
}

// LoadedBlockEntities returns a deep snapshot of block entities from chunks
// already resident in memory. Automation uses this to tick hoppers without
// generating or loading unrelated chunks.
func (w *World) LoadedBlockEntities() []BlockEntity {
	if w == nil {
		return nil
	}
	w.mu.RLock()
	chunks := make([]*Chunk, 0, len(w.chunks))
	for _, chunk := range w.chunks {
		chunks = append(chunks, chunk)
	}
	w.mu.RUnlock()

	w.containerMu.RLock()
	entities := make([]BlockEntity, 0)
	for _, chunk := range chunks {
		for _, entity := range chunk.BlockEntities {
			copyEntity := entity
			copyEntity.Data = append([]byte(nil), entity.Data...)
			copyEntity.Items = append([]ContainerItem(nil), entity.Items...)
			entities = append(entities, copyEntity)
		}
	}
	w.containerMu.RUnlock()
	return entities
}

// TouchesWater reports whether the vertical player volume at the supplied
// feet position intersects water (including waterlogged blocks). Entering
// water resets vanilla fall distance immediately, before the player reaches
// the floor below the fluid.
func (w *World) TouchesWater(x, y, z float64) bool {
	if w == nil {
		return false
	}
	bx, bz := int(math.Floor(x)), int(math.Floor(z))
	minY := int(math.Floor(y))
	maxY := int(math.Floor(y + 1.799999))
	for by := minY; by <= maxY; by++ {
		block := w.GetBlock(bx, by, bz)
		if block.ResourceLocation() == "minecraft:water" || block.ResourceLocation() == "minecraft:bubble_column" || block.Properties["waterlogged"] == "true" {
			return true
		}
	}
	return false
}

// SetContainerItems stores canonical container contents in the owning chunk.
// The Anvil adapter serialises these entries into the block entity Items list.
func (w *World) SetContainerItems(x, y, z int, blockEntityType string, items []ContainerItem) {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)

	clean := make([]ContainerItem, 0, len(items))
	for _, item := range items {
		if item.Slot < 0 || item.ItemID == "" || item.Count <= 0 {
			continue
		}
		clean = append(clean, item)
	}

	w.containerMu.Lock()
	updated := false
	for i := range c.BlockEntities {
		entity := &c.BlockEntities[i]
		if entity.X != x || entity.Y != y || entity.Z != z {
			continue
		}
		entity.Type = blockEntityType
		entity.Items = append([]ContainerItem(nil), clean...)
		if len(entity.Data) < 2 {
			entity.Data = []byte{10, 0}
		}
		updated = true
		break
	}
	if !updated {
		c.BlockEntities = append(c.BlockEntities, BlockEntity{
			X: x, Y: y, Z: z, Type: blockEntityType, Data: []byte{10, 0},
			Items: append([]ContainerItem(nil), clean...),
		})
	}
	w.containerMu.Unlock()

	w.mu.Lock()
	key := [2]int32{cx, cz}
	w.chunks[key] = c
	w.touchChunkLocked(key)
	w.dirty[key] = struct{}{}
	w.mu.Unlock()
	w.Redstone.NotifyChange(x, y, z)
}

// SetBlockEntity stores or replaces the non-container block entity at a world
// position. Data must be an anonymous network-NBT compound. It is copied so
// callers cannot mutate persisted chunk state after this method returns.
func (w *World) SetBlockEntity(x, y, z int, blockEntityType string, data []byte) {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)

	w.containerMu.Lock()
	updated := false
	var snapshot BlockEntity
	for index := range c.BlockEntities {
		entity := &c.BlockEntities[index]
		if entity.X == x && entity.Y == y && entity.Z == z {
			entity.Type = blockEntityType
			entity.Data = append([]byte(nil), data...)
			snapshot = *entity
			updated = true
			break
		}
	}
	if !updated {
		snapshot = BlockEntity{
			X: x, Y: y, Z: z, Type: blockEntityType, Data: append([]byte(nil), data...),
		}
		c.BlockEntities = append(c.BlockEntities, snapshot)
	}
	w.containerMu.Unlock()

	w.mu.Lock()
	key := [2]int32{cx, cz}
	w.chunks[key] = c
	w.touchChunkLocked(key)
	w.dirty[key] = struct{}{}
	w.mu.Unlock()
	w.notifyBlockEntityObserver(snapshot)
}

// SignState bundles all sign-specific state that must survive text edits and
// item interactions (glow ink sac, dye).
type SignState struct {
	FrontLines   [4]string
	BackLines    [4]string
	FrontGlowing bool
	BackGlowing  bool
	FrontColor   string // "" = default ("black")
	BackColor    string // "" = default ("black")
}

// SetBlockEntitySign stores or replaces a sign block entity, persisting the
// sign state (lines, glowing, color) alongside the raw NBT data.
func (w *World) SetBlockEntitySign(x, y, z int, data []byte, state SignState) {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)

	w.containerMu.Lock()
	updated := false
	var snapshot BlockEntity
	for index := range c.BlockEntities {
		entity := &c.BlockEntities[index]
		if entity.X == x && entity.Y == y && entity.Z == z {
			entity.Type = "minecraft:sign"
			entity.Data = append([]byte(nil), data...)
			entity.SignFrontLines = state.FrontLines
			entity.SignBackLines = state.BackLines
			entity.SignFrontGlowing = state.FrontGlowing
			entity.SignBackGlowing = state.BackGlowing
			entity.SignFrontColor = state.FrontColor
			entity.SignBackColor = state.BackColor
			snapshot = *entity
			updated = true
			break
		}
	}
	if !updated {
		snapshot = BlockEntity{
			X: x, Y: y, Z: z, Type: "minecraft:sign",
			Data:             append([]byte(nil), data...),
			SignFrontLines:   state.FrontLines, SignBackLines: state.BackLines,
			SignFrontGlowing: state.FrontGlowing, SignBackGlowing: state.BackGlowing,
			SignFrontColor:   state.FrontColor, SignBackColor: state.BackColor,
		}
		c.BlockEntities = append(c.BlockEntities, snapshot)
	}
	w.containerMu.Unlock()

	w.mu.Lock()
	key := [2]int32{cx, cz}
	w.chunks[key] = c
	w.touchChunkLocked(key)
	w.dirty[key] = struct{}{}
	w.mu.Unlock()
	w.notifyBlockEntityObserver(snapshot)
}

// SetBlock places block at absolute world coordinates (x, y, z).
// The chunk is loaded or generated on demand.
// Out-of-bounds Y is silently ignored.
func (w *World) SetBlock(x, y, z int, block Block) {
	if y < WorldMinY || y > WorldMaxY {
		return
	}
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)

	relY := y - WorldMinY
	sIdx := relY / SectionSize
	lx := x - int(cx)*SectionSize
	ly := relY % SectionSize
	lz := z - int(cz)*SectionSize

	if c.Sections[sIdx] == nil {
		c.Sections[sIdx] = NewSection()
	}
	oldBlock := c.Sections[sIdx].At(lx, ly, lz)
	c.Sections[sIdx].Set(lx, ly, lz, block)
	if block.IsAir() {
		w.containerMu.Lock()
		if len(c.BlockEntities) > 0 {
			filtered := c.BlockEntities[:0]
			for _, entity := range c.BlockEntities {
				if entity.X == x && entity.Y == y && entity.Z == z {
					continue
				}
				filtered = append(filtered, entity)
			}
			c.BlockEntities = filtered
		}
		w.containerMu.Unlock()
	}

	// Dirty chunks are pinned until persisted (disk mode) or shutdown (memory
	// mode), so cache eviction can never silently discard player changes.
	w.mu.Lock()
	key := [2]int32{cx, cz}
	w.chunks[key] = c
	w.touchChunkLocked(key)
	w.dirty[key] = struct{}{}
	w.trimChunksLocked()
	w.mu.Unlock()

	// Schedule physics updates for affected neighbours.
	// worldAge 0 = "fire next tick" (drainDue uses <= comparison).
	w.scheduleBlockNeighborUpdates(x, y, z, oldBlock, block)
	w.notifyBlockObserver(x, y, z, oldBlock, block)
	if !oldBlock.Equal(block) {
		w.triggerObservers(x, y, z)
	}
	if IsRailBlock(oldBlock.ResourceLocation()) || IsRailBlock(block.ResourceLocation()) {
		w.UpdateRailShapesAround(x, y, z)
	}
	if block.ResourceLocation() == "minecraft:sponge" && oldBlock.ResourceLocation() != "minecraft:sponge" {
		w.absorbWaterAround(x, y, z)
	}
	if block.ResourceLocation() == "minecraft:wet_sponge" && w.ultrawarm.Load() {
		w.SetBlock(x, y, z, Block{Namespace: "minecraft", Name: "sponge"})
	}
}

func (w *World) triggerObservers(x, y, z int) {
	for _, pos := range neighbors6(x, y, z) {
		observer := w.GetBlock(pos[0], pos[1], pos[2])
		if observer.ResourceLocation() != "minecraft:observer" || observer.Properties["powered"] == "true" {
			continue
		}
		dx, dy, dz := pistonOffset(observer.Properties["facing"])
		if [3]int{pos[0] + dx, pos[1] + dy, pos[2] + dz} != [3]int{x, y, z} {
			continue
		}
		// Vanilla observers schedule detection two game ticks after the watched
		// block changes. The scheduled tick starts the two-tick output pulse.
		w.BlockPhysics.ScheduleObserver(pos[0], pos[1], pos[2], w.PhysicsTime(), 2)
	}
}

// setBlockNoPhysics writes a block directly without scheduling physics updates.
// Used by the redstone engine and other internal systems to avoid feedback loops.
func (w *World) setBlockNoPhysics(x, y, z int, block Block) {
	if y < WorldMinY || y > WorldMaxY {
		return
	}
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)
	relY := y - WorldMinY
	sIdx := relY / SectionSize
	lx := x - int(cx)*SectionSize
	ly := relY % SectionSize
	lz := z - int(cz)*SectionSize
	if c.Sections[sIdx] == nil {
		c.Sections[sIdx] = NewSection()
	}
	oldBlock := c.Sections[sIdx].At(lx, ly, lz)
	c.Sections[sIdx].Set(lx, ly, lz, block)
	w.mu.Lock()
	key := [2]int32{cx, cz}
	w.chunks[key] = c
	w.touchChunkLocked(key)
	w.dirty[key] = struct{}{}
	w.trimChunksLocked()
	w.mu.Unlock()
	w.notifyBlockObserver(x, y, z, oldBlock, block)
	if !oldBlock.Equal(block) {
		w.triggerObservers(x, y, z)
	}
}

// scheduleBlockNeighborUpdates schedules physics ticks when a block is placed
// or removed.  worldAge is unknown here so we schedule at due=0 (fires on the
// very next drain regardless of current age).
func (w *World) scheduleBlockNeighborUpdates(x, y, z int, old, placed Block) {
	age := w.PhysicsTime()
	oldName := old.ResourceLocation()
	placedName := placed.ResourceLocation()

	// 1. If we just removed a block (placed = air), the gravity block directly
	//    above might now be unsupported.
	if placed.IsAir() {
		above := w.GetBlock(x, y+1, z)
		aboveName := above.ResourceLocation()
		if IsGravityBlock(aboveName) {
			w.BlockPhysics.ScheduleFall(x, y+1, z, age, 2)
		}
		// Adjacent fluid blocks may now be able to spread into the new air.
		for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nb := w.GetBlock(x+d[0], y, z+d[1])
			if IsFluidBlock(nb.ResourceLocation()) {
				w.BlockPhysics.ScheduleFluid(x+d[0], y, z+d[1], age, w.fluidTickDelay(nb.ResourceLocation()))
			}
		}
		// Fluid from above may fall in.
		above2 := w.GetBlock(x, y+1, z)
		if IsFluidBlock(above2.ResourceLocation()) {
			w.BlockPhysics.ScheduleFluid(x, y+1, z, age, 1)
		}
	}

	// 2. If we placed a gravity block, check immediately whether it has support.
	if IsGravityBlock(placedName) {
		w.BlockPhysics.ScheduleFall(x, y, z, age, 2)
	}

	// 3. If we placed a fluid, schedule spreading.
	if IsFluidBlock(placedName) {
		w.BlockPhysics.ScheduleFluid(x, y, z, age, w.fluidTickDelay(placedName))
		for _, pos := range neighbors6(x, y, z) {
			if IsFluidBlock(w.GetBlock(pos[0], pos[1], pos[2]).ResourceLocation()) {
				w.BlockPhysics.ScheduleFluid(pos[0], pos[1], pos[2], age, 1)
			}
		}
	}

	// 4. Only removing/replacing a log can disconnect leaves. Previously every
	// broken plant triggered a 13x13x13 scan, creating needless chunk loads and
	// making cache-eviction races common while a player crossed flower fields.
	if IsLogBlock(oldName) && !IsLogBlock(placedName) {
		w.scheduleNearbyLeafDecay(x, y, z, age)
	}

	// 5. If fire was placed, schedule fire spread.
	if IsFireBlock(placedName) {
		w.BlockPhysics.ScheduleFire(x, y, z, age, 20)
	}

	// 6. If ice was placed, schedule potential melt.
	if IsIceBlock(placedName) {
		w.BlockPhysics.ScheduleIce(x, y, z, age, IceMeltDelay)
	}

	// 6b. Live coral schedules a death check when placed; removing a neighbouring
	// block (for example draining water) rechecks any adjacent coral.
	if IsLiveCoral(placedName) {
		w.BlockPhysics.ScheduleCoralDeath(x, y, z, age, coralDeathDelay)
	}
	if placed.IsAir() {
		for _, pos := range neighbors6(x, y, z) {
			if IsLiveCoral(w.GetBlock(pos[0], pos[1], pos[2]).ResourceLocation()) {
				w.BlockPhysics.ScheduleCoralDeath(pos[0], pos[1], pos[2], age, coralDeathDelay)
			}
		}
	}

	// 7. Notify redstone engine of any change near redstone components.
	if IsRedstoneConductor(placedName) || IsRedstoneSource(placedName) || IsRedstoneLoad(placedName) ||
		IsRedstoneConductor(oldName) || IsRedstoneSource(oldName) || IsRedstoneLoad(oldName) ||
		isRedstonePowerConductor(placed) || isRedstonePowerConductor(old) || placed.IsAir() {
		w.Redstone.NotifyChange(x, y, z)
	}
}

// scheduleNearbyLeafDecay schedules decay checks for all leaf blocks within
// LeafDecayRadius of (x, y, z). Called when a log is removed.
func (w *World) scheduleNearbyLeafDecay(x, y, z int, age int64) {
	r := LeafDecayRadius
	for dx := -r; dx <= r; dx++ {
		for dy := -r; dy <= r; dy++ {
			for dz := -r; dz <= r; dz++ {
				nx, ny, nz := x+dx, y+dy, z+dz
				nb := w.GetBlock(nx, ny, nz)
				if IsLeafBlock(nb.ResourceLocation()) {
					// Stagger by Manhattan distance so outer leaves decay last.
					delay := int64(abs(dx)+abs(dy)+abs(dz)) + 5
					w.BlockPhysics.ScheduleLeafDecay(nx, ny, nz, age, delay)
				}
			}
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// GetBlock returns the canonical Block at absolute world coordinates.
// Returns Air for out-of-bounds Y or sections that have never been written to.
func (w *World) GetBlock(x, y, z int) Block {
	if y < WorldMinY || y > WorldMaxY {
		return Air
	}
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)

	relY := y - WorldMinY
	sIdx := relY / SectionSize
	lx := x - int(cx)*SectionSize
	ly := relY % SectionSize
	lz := z - int(cz)*SectionSize

	if c.Sections[sIdx] == nil {
		return Air
	}
	return c.Sections[sIdx].At(lx, ly, lz)
}

// Flush writes all dirty (modified) chunks to storage and clears the dirty set.
// Does nothing if no storage backend was provided.
func (w *World) Flush() error {
	if w.storage == nil {
		w.mu.Lock()
		w.trimChunksLocked()
		w.mu.Unlock()
		return nil
	}

	// Swap dirty set under the lock so writers can continue marking new dirty
	// chunks while we flush.
	w.mu.Lock()
	dirty := w.dirty
	w.dirty = make(map[[2]int32]struct{})
	w.flushing = dirty
	w.mu.Unlock()

	for key := range dirty {
		w.mu.RLock()
		c, ok := w.chunks[key]
		w.mu.RUnlock()
		if !ok {
			continue
		}
		if err := w.storage.SaveChunk(c); err != nil {
			w.restoreDirty(dirty)
			return fmt.Errorf("world: saving chunk (%d,%d): %w", key[0], key[1], err)
		}
	}
	if err := w.storage.Flush(); err != nil {
		w.restoreDirty(dirty)
		return fmt.Errorf("world: flushing storage: %w", err)
	}
	w.mu.Lock()
	w.flushing = make(map[[2]int32]struct{})
	w.trimChunksLocked()
	loaded := len(w.chunks)
	w.mu.Unlock()
	if len(dirty) > 0 {
		debuglog.Info(debuglog.Autosaves, "world autosave complete", "chunks", len(dirty), "cachedChunks", loaded)
	}
	return nil
}

func (w *World) restoreDirty(dirty map[[2]int32]struct{}) {
	w.mu.Lock()
	w.flushing = make(map[[2]int32]struct{})
	for key := range dirty {
		w.dirty[key] = struct{}{}
	}
	w.mu.Unlock()
}

// Close flushes all dirty chunks and closes the storage backend.
// Should be called once on server shutdown.
func (w *World) Close() error {
	w.closeOnce.Do(func() {
		close(w.pregenStop)
		w.pregenWG.Wait()
	})
	if err := w.Flush(); err != nil {
		return err
	}
	if w.storage != nil {
		return w.storage.Close()
	}
	return nil
}

// LoadedCount returns the number of chunks currently held in memory.
func (w *World) LoadedCount() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.chunks)
}

// SetWorldTime publishes the current time-of-day tick (0–23999) so that
// handler code can read it without accessing the server struct.
func (w *World) SetWorldTime(t int64) { w.worldTime.Store(t) }

// WorldTime returns the last published time-of-day tick (0–23999).
func (w *World) WorldTime() int64 { return w.worldTime.Load() }

// SetPhysicsTime publishes the monotonic server tick used by block physics.
func (w *World) SetPhysicsTime(t int64) { w.physicsTime.Store(t) }

// PhysicsTime returns the last published monotonic server tick.
func (w *World) PhysicsTime() int64 { return w.physicsTime.Load() }

// SetLavaTickDelay configures the dimension-specific lava flow delay.
func (w *World) SetLavaTickDelay(delay int64) {
	if delay > 0 {
		w.lavaTickDelay.Store(delay)
	}
}

// SetUltrawarm marks this dimension as ultrawarm (Nether), causing wet sponges
// placed here to dry instantly.
func (w *World) SetUltrawarm(ultrawarm bool) {
	w.ultrawarm.Store(ultrawarm)
}

func (w *World) fluidTickDelay(name string) int64 {
	if name == "minecraft:lava" {
		return w.lavaTickDelay.Load()
	}
	return 5
}

// RequestTimeSkip is called by the sleep handler to ask the tick goroutine
// to advance worldAge to the next morning (time-of-day 0).
func (w *World) RequestTimeSkip() { w.requestTimeSkip.Store(true) }

// DrainTimeSkip returns true once and resets the flag if a time skip was
// requested since the last call. Called by the server tick goroutine.
func (w *World) DrainTimeSkip() bool { return w.requestTimeSkip.Swap(false) }

// DecoratedPotDecorations returns all four vanilla side decorations stored by
// the decorated-pot block entity. Missing entries represent bricks in vanilla.
func (w *World) DecoratedPotDecorations(x, y, z int) [4]string {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)
	w.containerMu.RLock()
	defer w.containerMu.RUnlock()
	for _, entity := range c.BlockEntities {
		if entity.X == x && entity.Y == y && entity.Z == z {
			return normalizeDecoratedPotDecorations(entity.PotDecorations)
		}
	}
	return normalizeDecoratedPotDecorations([4]string{})
}

// SetDecoratedPotDecorations updates only the decoration component and keeps
// the pot's stored item and opaque block-entity data intact.
func (w *World) SetDecoratedPotDecorations(x, y, z int, decorations [4]string) {
	cx := int32(math.Floor(float64(x) / SectionSize))
	cz := int32(math.Floor(float64(z) / SectionSize))
	c := w.Chunk(cx, cz)
	decorations = normalizeDecoratedPotDecorations(decorations)

	w.containerMu.Lock()
	updated := false
	var snapshot BlockEntity
	for index := range c.BlockEntities {
		entity := &c.BlockEntities[index]
		if entity.X != x || entity.Y != y || entity.Z != z {
			continue
		}
		if entity.Type == "" {
			entity.Type = "minecraft:decorated_pot"
		}
		if len(entity.Data) < 2 {
			entity.Data = []byte{10, 0}
		}
		entity.PotDecorations = decorations
		snapshot = *entity
		updated = true
		break
	}
	if !updated {
		snapshot = BlockEntity{
			X: x, Y: y, Z: z, Type: "minecraft:decorated_pot", Data: []byte{10, 0},
			PotDecorations: decorations,
		}
		c.BlockEntities = append(c.BlockEntities, snapshot)
	}
	w.containerMu.Unlock()

	w.mu.Lock()
	key := [2]int32{cx, cz}
	w.chunks[key] = c
	w.touchChunkLocked(key)
	w.dirty[key] = struct{}{}
	w.mu.Unlock()
	w.notifyBlockEntityObserver(snapshot)
}

func normalizeDecoratedPotDecorations(decorations [4]string) [4]string {
	for index := range decorations {
		if decorations[index] == "" {
			decorations[index] = "minecraft:brick"
		}
	}
	return decorations
}
