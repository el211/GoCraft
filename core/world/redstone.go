package world

import (
	"strings"
	"sync"
)

// RedstoneEngine propagates redstone signal through the world.
//
// Design (simplified but correct for basic circuits):
//   - Power sources emit a signal strength 0-15.
//   - Redstone wire (dust) carries power, losing 1 per block of distance.
//   - Redstone torches emit 15 when NOT on a powered block (inverter).
//   - Redstone blocks always emit 15.
//   - Loads (pistons, lamps, TNT, dispensers) activate when power > 0.
//
// The engine is called from the tick goroutine via FlushUpdates, which drains
// the dirty set and propagates any changes. Block state writes (powered flags,
// dust power level) are applied directly through the World pointer.
type RedstoneEngine struct {
	mu    sync.Mutex
	world *World
	dirty map[[3]int]struct{} // positions needing re-evaluation
	// power holds the computed strong-power at each position (0-15).
	// Positions not in the map have power 0.
	power map[[3]int]int
}

// newRedstoneEngine creates an engine wired to w.
func newRedstoneEngine(w *World) *RedstoneEngine {
	return &RedstoneEngine{
		world: w,
		dirty: make(map[[3]int]struct{}),
		power: make(map[[3]int]int),
	}
}

// NotifyChange tells the engine that a block at (x,y,z) changed and that its
// power level (and that of its immediate neighbours) should be re-evaluated.
func (re *RedstoneEngine) NotifyChange(x, y, z int) {
	re.mu.Lock()
	for _, d := range neighbors6(x, y, z) {
		re.dirty[d] = struct{}{}
	}
	re.dirty[[3]int{x, y, z}] = struct{}{}
	re.mu.Unlock()
}

// PowerAt returns the current computed power level at (x,y,z).
func (re *RedstoneEngine) PowerAt(x, y, z int) int {
	re.mu.Lock()
	p := re.power[[3]int{x, y, z}]
	re.mu.Unlock()
	return p
}

// RedstoneResult holds the output of FlushUpdates.
type RedstoneResult struct {
	Changes      []BlockChange // visual block state changes to broadcast
	PoweredLoads [][3]int      // positions of loads that just became powered (TNT, pistons)
}

// FlushUpdates drains the dirty set, propagates power changes, and returns a
// RedstoneResult describing visual changes and newly powered loads.
// Must be called from the tick goroutine only.
func (re *RedstoneEngine) FlushUpdates() RedstoneResult {
	re.mu.Lock()
	if len(re.dirty) == 0 {
		re.mu.Unlock()
		return RedstoneResult{}
	}
	// Drain dirty set into a local slice.
	positions := make([][3]int, 0, len(re.dirty))
	for pos := range re.dirty {
		positions = append(positions, pos)
	}
	re.dirty = make(map[[3]int]struct{})
	re.mu.Unlock()

	// BFS propagation: iterate until stable (no more changes).
	var result RedstoneResult
	visited := make(map[[3]int]struct{})
	queue := positions

	for len(queue) > 0 {
		pos := queue[0]
		queue = queue[1:]
		if _, seen := visited[pos]; seen {
			continue
		}
		visited[pos] = struct{}{}

		x, y, z := pos[0], pos[1], pos[2]
		block := re.world.GetBlock(x, y, z)
		name := block.ResourceLocation()

		newPower := re.computePower(x, y, z, name, block)

		re.mu.Lock()
		oldPower := re.power[pos]
		if newPower != oldPower {
			wasUnpowered := oldPower == 0
			if newPower == 0 {
				delete(re.power, pos)
			} else {
				re.power[pos] = newPower
			}
			re.mu.Unlock()

			// Power changed — propagate to neighbors and apply visual state.
			change, ok := re.applyPowerState(x, y, z, name, block, newPower > 0)
			if ok {
				result.Changes = append(result.Changes, change)
			}
			// Track loads that just became powered (0 → >0).
			if wasUnpowered && newPower > 0 && IsRedstoneLoad(name) {
				result.PoweredLoads = append(result.PoweredLoads, pos)
			}
			for _, nb := range neighbors6(x, y, z) {
				if _, seen := visited[nb]; !seen {
					queue = append(queue, nb)
				}
			}
		} else {
			re.mu.Unlock()
		}
	}
	return result
}

// computePower returns the power level this block should have given its neighbours.
func (re *RedstoneEngine) computePower(x, y, z int, name string, block Block) int {
	switch name {
	case "minecraft:redstone_block":
		return 15

	case "minecraft:redstone_torch", "minecraft:redstone_wall_torch":
		// Torch emits 15 unless the block it sits on is powered.
		// Determine attachment block direction from Properties.
		facing := block.Properties["facing"]
		ax, ay, az := x, y-1, z // default: sitting on block below
		switch facing {
		case "north":
			ax, ay, az = x, y, z+1
		case "south":
			ax, ay, az = x, y, z-1
		case "east":
			ax, ay, az = x-1, y, z
		case "west":
			ax, ay, az = x+1, y, z
		}
		if re.strongPowerOf(ax, ay, az) > 0 {
			return 0 // inverted
		}
		return 15

	case "minecraft:lever":
		if block.Properties["powered"] == "true" {
			return 15
		}
		return 0

	case "minecraft:redstone_wire":
		// Dust gets max(neighbor_power - 1) from all 6 faces.
		best := 0
		for _, nb := range neighbors6(x, y, z) {
			nbBlock := re.world.GetBlock(nb[0], nb[1], nb[2])
			nbName := nbBlock.ResourceLocation()
			if nbName == "minecraft:redstone_wire" {
				p := re.PowerAt(nb[0], nb[1], nb[2])
				if p-1 > best {
					best = p - 1
				}
			} else if IsRedstoneSource(nbName) || nbName == "minecraft:redstone_block" {
				p := re.powerFromSource(nb[0], nb[1], nb[2], nbBlock)
				if p > best {
					best = p
				}
			}
		}
		return best

	case "minecraft:repeater":
		// Repeater outputs 15 if its input side has power > 0.
		if block.Properties["powered"] == "true" {
			return 15
		}
		// Check input direction.
		facing := block.Properties["facing"]
		ix, iy, iz := x, y, z+1 // default facing south, input from north
		switch facing {
		case "north":
			ix, iy, iz = x, y, z-1
		case "east":
			ix, iy, iz = x-1, y, z
		case "west":
			ix, iy, iz = x+1, y, z
		}
		if re.strongPowerOf(ix, iy, iz) > 0 || re.PowerAt(ix, iy, iz) > 0 {
			return 15
		}
		return 0

	default:
		// Solid blocks carry strong power if directly adjacent to a source.
		best := 0
		for _, nb := range neighbors6(x, y, z) {
			nbBlock := re.world.GetBlock(nb[0], nb[1], nb[2])
			nbName := nbBlock.ResourceLocation()
			if IsRedstoneSource(nbName) {
				p := re.powerFromSource(nb[0], nb[1], nb[2], nbBlock)
				if p > best {
					best = p
				}
			}
		}
		return best
	}
}

// strongPowerOf returns the strong power level emitted by (x,y,z).
// Strong power is power emitted directly by a source, not forwarded by dust.
func (re *RedstoneEngine) strongPowerOf(x, y, z int) int {
	block := re.world.GetBlock(x, y, z)
	return re.powerFromSource(x, y, z, block)
}

// powerFromSource returns power emitted by block if it is a source, else 0.
func (re *RedstoneEngine) powerFromSource(x, y, z int, block Block) int {
	name := block.ResourceLocation()
	switch name {
	case "minecraft:redstone_block":
		return 15
	case "minecraft:redstone_torch", "minecraft:redstone_wall_torch":
		return re.computePower(x, y, z, name, block)
	case "minecraft:lever":
		if block.Properties["powered"] == "true" {
			return 15
		}
	case "minecraft:repeater":
		if block.Properties["powered"] == "true" {
			return 15
		}
	default:
		if strings.HasSuffix(name, "_button") && block.Properties["powered"] == "true" {
			return 15
		}
		if strings.HasSuffix(name, "_pressure_plate") && block.Properties["powered"] == "true" {
			return 15
		}
	}
	return 0
}

// applyPowerState updates the visual/functional block state when power changes.
// Returns (BlockChange, true) if the block visually changed.
func (re *RedstoneEngine) applyPowerState(x, y, z int, name string, block Block, powered bool) (BlockChange, bool) {
	switch name {
	case "minecraft:redstone_lamp":
		var newName string
		if powered {
			newName = "minecraft:redstone_lamp" // stays same name, "lit" property
		}
		newBlock := Block{
			Namespace: "minecraft",
			Name:      "redstone_lamp",
			Properties: map[string]string{
				"lit": boolStr(powered),
			},
		}
		_ = newName
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:redstone_wire":
		// Update power property on dust.
		p := 0
		if powered {
			p = re.power[[3]int{x, y, z}]
		}
		newBlock := Block{
			Namespace: "minecraft",
			Name:      "redstone_wire",
			Properties: map[string]string{
				"power": itoa(p),
				"north": block.Properties["north"],
				"south": block.Properties["south"],
				"east":  block.Properties["east"],
				"west":  block.Properties["west"],
			},
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:redstone_torch", "minecraft:redstone_wall_torch":
		// Torch lit/unlit visual.
		newBlock := block
		if powered { // powered = torch IS emitting (not inverted)
			newBlock.Name = strings.TrimPrefix(name, "minecraft:")
		} else {
			// unlit variant: not in vanilla as a separate block, skip visual change
			return BlockChange{}, false
		}
		return BlockChange{}, false

	case "minecraft:repeater":
		newBlock := Block{
			Namespace: "minecraft",
			Name:      "repeater",
			Properties: map[string]string{
				"delay":   orDefault(block.Properties["delay"], "1"),
				"facing":  orDefault(block.Properties["facing"], "north"),
				"locked":  orDefault(block.Properties["locked"], "false"),
				"powered": boolStr(powered),
			},
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true
	}
	return BlockChange{}, false
}

// neighbors6 returns the 6 face-adjacent positions of (x,y,z).
func neighbors6(x, y, z int) [6][3]int {
	return [6][3]int{
		{x + 1, y, z}, {x - 1, y, z},
		{x, y + 1, z}, {x, y - 1, z},
		{x, y, z + 1}, {x, y, z - 1},
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
