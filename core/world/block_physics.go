package world

import (
	"strconv"
	"strings"
	"sync"
)

// blockUpdateKind identifies which physics rule to apply when an update fires.
type blockUpdateKind uint8

const (
	UpdateFall      blockUpdateKind = iota // gravity-affected block may need to fall
	UpdateFluid                            // fluid block may need to spread
	UpdateLeafDecay                        // leaf block may decay if too far from log
	UpdateFire                             // fire block may spread or burn out
	UpdateIce                              // ice block may melt to water
)

// PendingBlockUpdate is one scheduled block-tick entry.
type PendingBlockUpdate struct {
	X, Y, Z int
	Kind     blockUpdateKind
	DueTick  int64 // world-age tick when this update fires
}

// BlockPhysics is the scheduled-block-tick engine.
// It deduplicates redundant updates at the same position so a single block
// change cannot queue hundreds of duplicate work items.
type BlockPhysics struct {
	mu      sync.Mutex
	pending map[[3]int]PendingBlockUpdate
}

func newBlockPhysics() *BlockPhysics {
	return &BlockPhysics{pending: make(map[[3]int]PendingBlockUpdate)}
}

func (bp *BlockPhysics) schedule(x, y, z int, kind blockUpdateKind, worldAge, delayTicks int64) {
	key := [3]int{x, y, z}
	due := worldAge + delayTicks
	bp.mu.Lock()
	if existing, ok := bp.pending[key]; !ok || due < existing.DueTick {
		bp.pending[key] = PendingBlockUpdate{X: x, Y: y, Z: z, Kind: kind, DueTick: due}
	}
	bp.mu.Unlock()
}

// ScheduleFall queues a gravity check for (x, y, z) to fire in delayTicks ticks.
func (bp *BlockPhysics) ScheduleFall(x, y, z int, worldAge, delayTicks int64) {
	bp.schedule(x, y, z, UpdateFall, worldAge, delayTicks)
}

// ScheduleFluid queues a fluid-spread check for (x, y, z).
func (bp *BlockPhysics) ScheduleFluid(x, y, z int, worldAge, delayTicks int64) {
	bp.schedule(x, y, z, UpdateFluid, worldAge, delayTicks)
}

// ScheduleLeafDecay queues a leaf decay check for (x, y, z).
func (bp *BlockPhysics) ScheduleLeafDecay(x, y, z int, worldAge, delayTicks int64) {
	bp.schedule(x, y, z, UpdateLeafDecay, worldAge, delayTicks)
}

// ScheduleFire queues a fire-spread tick for (x, y, z).
func (bp *BlockPhysics) ScheduleFire(x, y, z int, worldAge, delayTicks int64) {
	bp.schedule(x, y, z, UpdateFire, worldAge, delayTicks)
}

// ScheduleIce queues an ice-melt check for (x, y, z).
func (bp *BlockPhysics) ScheduleIce(x, y, z int, worldAge, delayTicks int64) {
	bp.schedule(x, y, z, UpdateIce, worldAge, delayTicks)
}

// DrainDue removes and returns all updates whose DueTick <= worldAge.
func (bp *BlockPhysics) DrainDue(worldAge int64) []PendingBlockUpdate {
	bp.mu.Lock()
	var due []PendingBlockUpdate
	for key, u := range bp.pending {
		if u.DueTick <= worldAge {
			due = append(due, u)
			delete(bp.pending, key)
		}
	}
	bp.mu.Unlock()
	return due
}

// ─── Gravity ─────────────────────────────────────────────────────────────────

// IsGravityBlock reports whether name is a block that falls when unsupported.
func IsGravityBlock(name string) bool {
	switch name {
	case "minecraft:sand", "minecraft:red_sand", "minecraft:gravel",
		"minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil",
		"minecraft:suspicious_sand", "minecraft:suspicious_gravel",
		"minecraft:pointed_dripstone", "minecraft:dragon_egg":
		return true
	}
	return strings.HasSuffix(name, "_concrete_powder")
}

// ─── Fluids ──────────────────────────────────────────────────────────────────

// IsFluidBlock reports whether name is a fluid that spreads (water or lava).
func IsFluidBlock(name string) bool {
	return name == "minecraft:water" || name == "minecraft:lava"
}

// IsFluidPassable reports whether a block can be displaced by a fluid.
func IsFluidPassable(name string) bool {
	switch name {
	case "", "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:water", "minecraft:lava",
		"minecraft:short_grass", "minecraft:grass", "minecraft:tall_grass",
		"minecraft:fern", "minecraft:large_fern",
		"minecraft:dandelion", "minecraft:poppy", "minecraft:blue_orchid",
		"minecraft:allium", "minecraft:azure_bluet", "minecraft:red_tulip",
		"minecraft:orange_tulip", "minecraft:white_tulip", "minecraft:pink_tulip",
		"minecraft:oxeye_daisy", "minecraft:cornflower", "minecraft:lily_of_the_valley",
		"minecraft:wheat", "minecraft:carrots", "minecraft:potatoes",
		"minecraft:beetroots", "minecraft:sugar_cane":
		return true
	}
	return false
}

// IsSolidLandingSurface reports whether a falling block can land on name.
func IsSolidLandingSurface(name string) bool {
	if name == "" || name == "minecraft:air" || name == "minecraft:cave_air" ||
		name == "minecraft:void_air" {
		return false
	}
	if IsFluidBlock(name) || IsFluidPassable(name) {
		return false
	}
	return true
}

// FluidLevel parses the "level" property of a water or lava block.
// Returns -1 for non-fluid blocks.
func FluidLevel(b Block) int {
	if !IsFluidBlock(b.ResourceLocation()) {
		return -1
	}
	s := b.Properties["level"]
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

// MakeFluid returns a fluid block (water or lava) with the given level.
func MakeFluid(name string, level int) Block {
	return Block{
		Namespace: "minecraft",
		Name:      strings.TrimPrefix(name, "minecraft:"),
		Properties: map[string]string{
			"level": strconv.Itoa(level),
		},
	}
}

// ─── Concrete powder ─────────────────────────────────────────────────────────

// IsConcretePowder reports whether name is a concrete powder block.
func IsConcretePowder(name string) bool {
	return strings.HasSuffix(name, "_concrete_powder")
}

// ConcreteName returns the hardened concrete block name for a concrete powder name.
// e.g. "minecraft:white_concrete_powder" → "minecraft:white_concrete"
func ConcreteName(powderName string) string {
	return strings.Replace(powderName, "_concrete_powder", "_concrete", 1)
}

// ─── Leaves ──────────────────────────────────────────────────────────────────

// IsLeafBlock reports whether name is a leaves block.
func IsLeafBlock(name string) bool {
	return strings.HasSuffix(name, "_leaves")
}

// IsLogBlock reports whether name is a log or wood block (counts as log for leaf decay).
func IsLogBlock(name string) bool {
	return strings.HasSuffix(name, "_log") ||
		strings.HasSuffix(name, "_wood") ||
		strings.HasSuffix(name, "_stem") ||
		strings.HasSuffix(name, "_hyphae") ||
		name == "minecraft:mushroom_stem"
}

// LeafDecayRadius is the maximum Manhattan-distance a leaf can be from a log
// before it decays. Vanilla uses 6 (distance property 1-6, 7 = decaying).
const LeafDecayRadius = 6

// ─── Fire ────────────────────────────────────────────────────────────────────

// IsFireBlock reports whether name is a fire block.
func IsFireBlock(name string) bool {
	return name == "minecraft:fire" || name == "minecraft:soul_fire"
}

// IsFlammable reports whether a block can catch fire.
func IsFlammable(name string) bool {
	return FlammabilityScore(name) > 0
}

// FlammabilityScore returns how easily this block catches fire (1-100).
// Higher = catches fire more easily. 0 = not flammable.
func FlammabilityScore(name string) int {
	switch name {
	// Very flammable (wool, carpet, leaves, TNT)
	case "minecraft:tnt":
		return 100
	case "minecraft:bookshelf":
		return 30
	case "minecraft:hay_block":
		return 60
	case "minecraft:target":
		return 15
	// Leaves
	default:
		if strings.HasSuffix(name, "_leaves") {
			return 30
		}
		// Wood planks, slabs, stairs
		if strings.HasSuffix(name, "_planks") ||
			strings.HasSuffix(name, "_slab") && strings.Contains(name, "wood") ||
			strings.HasSuffix(name, "_stairs") && strings.Contains(name, "wood") ||
			strings.HasSuffix(name, "_fence") ||
			strings.HasSuffix(name, "_fence_gate") ||
			strings.HasSuffix(name, "_log") ||
			strings.HasSuffix(name, "_wood") {
			return 20
		}
		// Wool and carpet
		if strings.HasSuffix(name, "_wool") || strings.HasSuffix(name, "_carpet") {
			return 30
		}
		// Bamboo
		if strings.Contains(name, "bamboo") {
			return 20
		}
	}
	return 0
}

// FireBurnTicks is how many ticks fire can persist before burning out on its own.
// On netherrack, fire is permanent.
const FireBurnTicks = int64(100)

// IsNetherrack reports whether fire on this block is permanent.
func IsNetherrack(name string) bool {
	return name == "minecraft:netherrack" || name == "minecraft:soul_sand" || name == "minecraft:soul_soil"
}

// ─── Ice and snow ────────────────────────────────────────────────────────────

// IsIceBlock reports whether name is an ice block that can melt.
func IsIceBlock(name string) bool {
	return name == "minecraft:ice" || name == "minecraft:frosted_ice"
}

// IsPackedIce reports whether name is packed/blue ice (does not melt).
func IsPackedIce(name string) bool {
	return name == "minecraft:packed_ice" || name == "minecraft:blue_ice"
}

// IceMeltDelay is the number of ticks before ice melts when exposed to light/warmth.
const IceMeltDelay = int64(200)

// IsSnowBlock reports whether name is a snow layer or snow block.
func IsSnowBlock(name string) bool {
	return name == "minecraft:snow" || name == "minecraft:snow_block"
}

// ─── Redstone ────────────────────────────────────────────────────────────────

// IsRedstoneConductor reports whether this block transmits redstone power.
func IsRedstoneConductor(name string) bool {
	switch name {
	case "minecraft:redstone_wire",
		"minecraft:repeater", "minecraft:comparator",
		"minecraft:redstone_block":
		return true
	}
	return false
}

// IsRedstoneSource reports whether this block can be a power source.
func IsRedstoneSource(name string) bool {
	switch name {
	case "minecraft:lever", "minecraft:redstone_torch",
		"minecraft:redstone_wall_torch", "minecraft:redstone_block",
		"minecraft:stone_button", "minecraft:oak_button",
		"minecraft:spruce_button", "minecraft:birch_button",
		"minecraft:jungle_button", "minecraft:acacia_button",
		"minecraft:dark_oak_button", "minecraft:mangrove_button",
		"minecraft:cherry_button", "minecraft:bamboo_button",
		"minecraft:crimson_button", "minecraft:warped_button",
		"minecraft:stone_pressure_plate", "minecraft:oak_pressure_plate",
		"minecraft:daylight_detector":
		return true
	}
	return false
}

// IsRedstoneLoad reports whether this block reacts to redstone power.
func IsRedstoneLoad(name string) bool {
	switch name {
	case "minecraft:piston", "minecraft:sticky_piston",
		"minecraft:dispenser", "minecraft:dropper",
		"minecraft:note_block", "minecraft:redstone_lamp",
		"minecraft:tnt", "minecraft:iron_trapdoor",
		"minecraft:iron_door":
		return true
	}
	return false
}
