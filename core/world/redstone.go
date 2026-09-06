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
	Changes        []BlockChange // visual block state changes to broadcast
	PoweredLoads   [][3]int      // positions of loads that just became powered (TNT, pistons)
	UnpoweredLoads [][3]int      // positions of loads whose input just fell to zero
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
	queue := positions
	processed := 0

	for len(queue) > 0 && processed < 65536 {
		pos := queue[0]
		queue = queue[1:]
		processed++

		x, y, z := pos[0], pos[1], pos[2]
		block := re.world.GetBlock(x, y, z)
		name := block.ResourceLocation()

		newPower := re.computePower(x, y, z, name, block)

		re.mu.Lock()
		oldPower := re.power[pos]
		if newPower != oldPower {
			wasUnpowered := oldPower == 0
			wasPowered := oldPower > 0
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
			if wasPowered && newPower == 0 && IsRedstoneLoad(name) {
				result.UnpoweredLoads = append(result.UnpoweredLoads, pos)
			}
			for _, nb := range neighbors6(x, y, z) {
				queue = append(queue, nb)
			}
		} else {
			re.mu.Unlock()
			// Freshly loaded blocks may have a visual state that does not match
			// the engine's zero-value power cache. Reconcile state even when the
			// numeric signal itself did not transition.
			if (name == "minecraft:redstone_torch" || name == "minecraft:redstone_wall_torch") &&
				block.Properties["lit"] != boolStr(newPower > 0) {
				if change, ok := re.applyPowerState(x, y, z, name, block, newPower > 0); ok {
					result.Changes = append(result.Changes, change)
					for _, nb := range neighbors6(x, y, z) {
						queue = append(queue, nb)
					}
				}
			}
			if name == "minecraft:repeater" && block.Properties["locked"] != boolStr(re.repeaterLocked(x, y, z, block)) {
				if change, ok := re.applyPowerState(x, y, z, name, block, newPower > 0); ok {
					result.Changes = append(result.Changes, change)
				}
			}
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
		// A torch observes power delivered to its attachment block, including
		// power conducted into that block by dust. Exclude the torch itself so
		// its output cannot feed back through its own support.
		if re.powerReceivedExcluding(ax, ay, az, [3]int{x, y, z}) > 0 {
			return 0 // inverted
		}
		return 15

	case "minecraft:lever":
		if block.Properties["powered"] == "true" {
			return 15
		}
		return 0

	case "minecraft:daylight_detector", "minecraft:inverted_daylight_detector", "minecraft:target",
		"minecraft:detector_rail", "minecraft:lightning_rod", "minecraft:tripwire_hook", "minecraft:lectern",
		"minecraft:observer":
		return re.powerFromSource(x, y, z, block)

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
				p := re.powerFromSourceToward(nb[0], nb[1], nb[2], nbBlock, [3]int{x, y, z})
				if p > best {
					best = p
				}
			} else if IsRedstoneConductor(nbName) {
				p := re.powerFromConductorToward(nb[0], nb[1], nb[2], nbBlock, [3]int{x, y, z})
				if p > best {
					best = p
				}
			} else if isRedstonePowerConductor(nbBlock) {
				p := re.strongPowerThroughBlock(nb[0], nb[1], nb[2], [3]int{x, y, z})
				if p > best {
					best = p
				}
			}
		}
		// Dust also follows one-block steps. It may climb a solid neighbor when
		// the space above the current dust is clear, or descend beside a
		// non-solid neighbor, matching Pumpkin's calculate_power scan.
		for _, offset := range [4][2]int{{0, -1}, {0, 1}, {-1, 0}, {1, 0}} {
			nx, nz := x+offset[0], z+offset[1]
			neighbor := re.world.GetBlock(nx, y, nz)
			wireY := y - 1
			if isRedstoneSolidBlock(neighbor) {
				if !isRedstoneSolidBlock(re.world.GetBlock(x, y+1, z)) {
					wireY = y + 1
				} else {
					continue
				}
			}
			wire := re.world.GetBlock(nx, wireY, nz)
			if wire.ResourceLocation() == "minecraft:redstone_wire" {
				if power := re.PowerAt(nx, wireY, nz) - 1; power > best {
					best = power
				}
			}
		}
		return best

	case "minecraft:repeater":
		// Repeater outputs 15 if its input side has power > 0.
		if re.repeaterLocked(x, y, z, block) && block.Properties["powered"] == "true" {
			return 15
		}
		if re.repeaterLocked(x, y, z, block) {
			return 0
		}
		// Check input direction.
		dx, dz := redstoneFacingOffset(block.Properties["facing"])
		ix, iy, iz := x+dx, y, z+dz
		input := re.world.GetBlock(ix, iy, iz)
		if re.powerFromSourceToward(ix, iy, iz, input, [3]int{x, y, z}) > 0 || re.PowerAt(ix, iy, iz) > 0 {
			return 15
		}
		return 0

	case "minecraft:comparator":
		dx, dz := redstoneFacingOffset(block.Properties["facing"])
		inputX, inputZ := x+dx, z+dz
		main := re.inputPowerAt(inputX, y, inputZ, [3]int{x, y, z})
		if analog := re.analogOutputAt(inputX, y, inputZ); analog > main {
			main = analog
		}
		if main < 15 && isRedstoneSolidBlock(re.world.GetBlock(inputX, y, inputZ)) {
			if analog := re.analogOutputAt(inputX+dx, y, inputZ+dz); analog > main {
				main = analog
			}
		}
		left := re.inputPowerAt(x-dz, y, z+dx, [3]int{x, y, z})
		right := re.inputPowerAt(x+dz, y, z-dx, [3]int{x, y, z})
		side := left
		if right > side {
			side = right
		}
		if block.Properties["mode"] == "subtract" {
			if main > side {
				return main - side
			}
			return 0
		}
		if main >= side {
			return main
		}
		return 0

	case "minecraft:powered_rail", "minecraft:activator_rail":
		if re.railNetworkPowered(x, y, z, name) {
			return 15
		}
		return 0

	default:
		best := 0
		currentIsFullConductor := isRedstonePowerConductor(block) && !IsRedstoneConductor(name)
		for _, nb := range neighbors6(x, y, z) {
			nbBlock := re.world.GetBlock(nb[0], nb[1], nb[2])
			nbName := nbBlock.ResourceLocation()
			if IsRedstoneSource(nbName) {
				if (nbName == "minecraft:redstone_torch" || nbName == "minecraft:redstone_wall_torch") &&
					redstoneTorchAttachment(nb[0], nb[1], nb[2], nbBlock) == [3]int{x, y, z} {
					continue
				}
				p := re.powerFromSourceToward(nb[0], nb[1], nb[2], nbBlock, [3]int{x, y, z})
				if p > best {
					best = p
				}
			} else if IsRedstoneConductor(nbName) {
				if p := re.powerFromConductorToward(nb[0], nb[1], nb[2], nbBlock, [3]int{x, y, z}); p > best {
					best = p
				}
			} else if !currentIsFullConductor && isRedstonePowerConductor(nbBlock) {
				if p := re.strongPowerThroughBlock(nb[0], nb[1], nb[2], [3]int{x, y, z}); p > best {
					best = p
				}
			}
		}
		return best
	}
}

func (re *RedstoneEngine) analogOutputAt(x, y, z int) int {
	block := re.world.GetBlock(x, y, z)
	switch block.ResourceLocation() {
	case "minecraft:composter":
		return atoi(block.Properties["level"])
	case "minecraft:cake":
		return 14 - min(atoi(block.Properties["bites"]), 6)*2
	case "minecraft:respawn_anchor":
		charges := atoi(block.Properties["charges"])
		if charges > 0 {
			return charges*4 - 1
		}
		return 0
	case "minecraft:jukebox":
		if block.Properties["has_record"] == "true" {
			be := re.world.GetBlockEntity(x, y, z)
			return JukeboxComparatorSignal(JukeboxRecordItem(be))
		}
		return 0
	case "minecraft:chiseled_bookshelf":
		be := re.world.GetBlockEntity(x, y, z)
		return int(be.LastBookshelfSlot) // 0 = no interaction, 1-6 = last slot
	}
	slots := redstoneContainerSlots(block.ResourceLocation())
	if slots == 0 {
		return 0
	}
	fullness, occupied := 0.0, 0
	for _, item := range re.world.ContainerItems(x, y, z) {
		if item.Count <= 0 {
			continue
		}
		occupied++
		fullness += float64(min(item.Count, 64)) / 64
	}
	if occupied == 0 {
		return 0
	}
	return int(fullness/float64(slots)*14) + 1
}

func redstoneContainerSlots(name string) int {
	switch {
	case name == "minecraft:hopper", name == "minecraft:brewing_stand":
		return 5
	case name == "minecraft:dispenser", name == "minecraft:dropper", name == "minecraft:crafter":
		return 9
	case name == "minecraft:furnace" || name == "minecraft:smoker" || name == "minecraft:blast_furnace":
		return 3
	case name == "minecraft:decorated_pot":
		return 1
	case name == "minecraft:chest" || name == "minecraft:trapped_chest" || name == "minecraft:barrel" || strings.HasSuffix(name, "_shulker_box"):
		return 27
	default:
		return 0
	}
}

func (re *RedstoneEngine) railNetworkPowered(x, y, z int, railName string) bool {
	type node struct{ x, y, z, distance int }
	queue := []node{{x: x, y: y, z: z}}
	visited := map[[3]int]struct{}{{x, y, z}: {}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, neighbor := range neighbors6(current.x, current.y, current.z) {
			block := re.world.GetBlock(neighbor[0], neighbor[1], neighbor[2])
			if re.powerFromSourceToward(neighbor[0], neighbor[1], neighbor[2], block,
				[3]int{current.x, current.y, current.z}) > 0 {
				return true
			}
			if IsRedstoneConductor(block.ResourceLocation()) &&
				re.powerFromConductorToward(neighbor[0], neighbor[1], neighbor[2], block,
					[3]int{current.x, current.y, current.z}) > 0 {
				return true
			}
		}
		if current.distance >= 7 {
			continue
		}
		for _, offset := range [4][2]int{{0, -1}, {0, 1}, {-1, 0}, {1, 0}} {
			for _, dy := range []int{0, 1, -1} {
				next := node{x: current.x + offset[0], y: current.y + dy, z: current.z + offset[1], distance: current.distance + 1}
				key := [3]int{next.x, next.y, next.z}
				if _, seen := visited[key]; seen || re.world.GetBlock(next.x, next.y, next.z).ResourceLocation() != railName {
					continue
				}
				visited[key] = struct{}{}
				queue = append(queue, next)
				break
			}
		}
	}
	return false
}

func (re *RedstoneEngine) repeaterLocked(x, y, z int, block Block) bool {
	dx, dz := redstoneFacingOffset(block.Properties["facing"])
	for _, side := range [2][2]int{{-dz, dx}, {dz, -dx}} {
		sx, sz := x+side[0], z+side[1]
		source := re.world.GetBlock(sx, y, sz)
		if source.ResourceLocation() != "minecraft:repeater" && source.ResourceLocation() != "minecraft:comparator" {
			continue
		}
		if re.powerFromConductorToward(sx, y, sz, source, [3]int{x, y, z}) > 0 {
			return true
		}
	}
	return false
}

func isRedstoneSolidBlock(block Block) bool {
	name := block.ResourceLocation()
	return !block.IsAir() && !IsFluidBlock(name) && name != "minecraft:redstone_wire" &&
		!strings.Contains(name, "torch") && !strings.Contains(name, "rail") &&
		!strings.HasSuffix(name, "_button") && !strings.HasSuffix(name, "_pressure_plate")
}

func redstoneTorchAttachment(x, y, z int, block Block) [3]int {
	attachment := [3]int{x, y - 1, z}
	if block.ResourceLocation() != "minecraft:redstone_wall_torch" {
		return attachment
	}
	switch block.Properties["facing"] {
	case "north":
		return [3]int{x, y, z + 1}
	case "south":
		return [3]int{x, y, z - 1}
	case "east":
		return [3]int{x - 1, y, z}
	case "west":
		return [3]int{x + 1, y, z}
	default:
		return attachment
	}
}

func (re *RedstoneEngine) powerReceivedExcluding(x, y, z int, excluded [3]int) int {
	best := 0
	for _, nb := range neighbors6(x, y, z) {
		if nb == excluded {
			continue
		}
		block := re.world.GetBlock(nb[0], nb[1], nb[2])
		if power := re.powerFromSource(nb[0], nb[1], nb[2], block); power > best {
			best = power
		}
		if IsRedstoneConductor(block.ResourceLocation()) {
			if power := re.PowerAt(nb[0], nb[1], nb[2]); power > best {
				best = power
			}
		}
	}
	return best
}

func (re *RedstoneEngine) inputPowerAt(x, y, z int, target [3]int) int {
	block := re.world.GetBlock(x, y, z)
	power := re.powerFromSourceToward(x, y, z, block, target)
	if IsRedstoneConductor(block.ResourceLocation()) {
		if conductor := re.powerFromConductorToward(x, y, z, block, target); conductor > power {
			power = conductor
		}
	} else if isRedstonePowerConductor(block) {
		if conductor := re.strongPowerThroughBlock(x, y, z, target); conductor > power {
			power = conductor
		}
	}
	return power
}

func (re *RedstoneEngine) strongPowerThroughBlock(x, y, z int, excluded [3]int) int {
	best := 0
	for _, nb := range neighbors6(x, y, z) {
		if nb == excluded {
			continue
		}
		source := re.world.GetBlock(nb[0], nb[1], nb[2])
		if p := re.powerFromSourceToward(nb[0], nb[1], nb[2], source, [3]int{x, y, z}); p > best {
			best = p
		}
		// Dust strongly powers the full block directly underneath it. Side
		// dust remains a weak input and must not be reflected into another dust.
		if source.ResourceLocation() == "minecraft:redstone_wire" && nb[1] == y+1 {
			if p := re.PowerAt(nb[0], nb[1], nb[2]); p > best {
				best = p
			}
		}
	}
	return best
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
	case "minecraft:comparator":
		return re.PowerAt(x, y, z)
	case "minecraft:observer":
		if block.Properties["powered"] == "true" {
			return 15
		}
	case "minecraft:daylight_detector", "minecraft:inverted_daylight_detector", "minecraft:target":
		return atoi(block.Properties["power"])
	case "minecraft:detector_rail", "minecraft:lightning_rod", "minecraft:lectern":
		if block.Properties["powered"] == "true" {
			return 15
		}
	case "minecraft:tripwire_hook":
		if block.Properties["attached"] == "true" && block.Properties["powered"] == "true" {
			return 15
		}
	case "minecraft:sculk_sensor", "minecraft:calibrated_sculk_sensor":
		if block.Properties["sculk_sensor_phase"] == "active" {
			return atoi(block.Properties["power"])
		}
	default:
		if strings.HasSuffix(name, "_button") && block.Properties["powered"] == "true" {
			return 15
		}
		if strings.HasSuffix(name, "_pressure_plate") {
			if block.Properties["powered"] == "true" {
				return 15
			}
			if power := atoi(block.Properties["power"]); power > 0 {
				return power
			}
		}
	}
	return 0
}

func (re *RedstoneEngine) powerFromSourceToward(x, y, z int, block Block, target [3]int) int {
	name := block.ResourceLocation()
	if name == "minecraft:repeater" || name == "minecraft:comparator" {
		dx, dz := redstoneFacingOffset(block.Properties["facing"])
		if target != [3]int{x - dx, y, z - dz} {
			return 0
		}
	}
	if name == "minecraft:observer" {
		dx, dy, dz := pistonOffset(block.Properties["facing"])
		if target != [3]int{x - dx, y - dy, z - dz} {
			return 0
		}
	}
	if (name == "minecraft:redstone_torch" || name == "minecraft:redstone_wall_torch") &&
		redstoneTorchAttachment(x, y, z, block) == target {
		return 0
	}
	return re.powerFromSource(x, y, z, block)
}

func (re *RedstoneEngine) powerFromConductorToward(x, y, z int, block Block, target [3]int) int {
	name := block.ResourceLocation()
	if name == "minecraft:repeater" || name == "minecraft:comparator" {
		dx, dz := redstoneFacingOffset(block.Properties["facing"])
		if target != [3]int{x - dx, y, z - dz} {
			return 0
		}
	}
	return re.PowerAt(x, y, z)
}

func redstoneFacingOffset(facing string) (int, int) {
	switch facing {
	case "south":
		return 0, 1
	case "east":
		return 1, 0
	case "west":
		return -1, 0
	default:
		return 0, -1
	}
}

// applyPowerState updates the visual/functional block state when power changes.
// Returns (BlockChange, true) if the block visually changed.
func (re *RedstoneEngine) applyPowerState(x, y, z int, name string, block Block, powered bool) (BlockChange, bool) {
	if name == "minecraft:trapdoor" || strings.HasSuffix(name, "_trapdoor") {
		newBlock := block
		newBlock.Properties = make(map[string]string, len(block.Properties)+2)
		for key, value := range block.Properties {
			newBlock.Properties[key] = value
		}
		newBlock.Properties["open"] = boolStr(powered)
		newBlock.Properties["powered"] = boolStr(powered)
		if block.Properties["open"] == newBlock.Properties["open"] && block.Properties["powered"] == newBlock.Properties["powered"] {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true
	}
	if strings.HasSuffix(name, "_door") || strings.HasSuffix(name, "_fence_gate") {
		newBlock := redstoneBlockWith(block, "powered", boolStr(powered), "open", boolStr(powered))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		if strings.HasSuffix(name, "_door") {
			otherY := y + 1
			if block.Properties["half"] == "upper" {
				otherY = y - 1
			}
			other := re.world.GetBlock(x, otherY, z)
			if other.ResourceLocation() == name {
				re.world.setBlockNoPhysics(x, otherY, z,
					redstoneBlockWith(other, "powered", boolStr(powered), "open", boolStr(powered)))
			}
		}
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true
	}
	if strings.HasSuffix(name, "_copper_bulb") {
		lit := block.Properties["lit"] == "true"
		if powered && block.Properties["powered"] != "true" {
			lit = !lit
		}
		newBlock := redstoneBlockWith(block, "powered", boolStr(powered), "lit", boolStr(lit))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true
	}
	switch name {
	case "minecraft:bell":
		newBlock := redstoneBlockWith(block, "powered", boolStr(powered))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

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
		if block.Properties["lit"] == boolStr(powered) {
			return BlockChange{}, false
		}
		newBlock := block
		newBlock.Properties = make(map[string]string, len(block.Properties)+1)
		for key, value := range block.Properties {
			newBlock.Properties[key] = value
		}
		newBlock.Properties["lit"] = boolStr(powered)
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:repeater":
		newBlock := Block{
			Namespace: "minecraft",
			Name:      "repeater",
			Properties: map[string]string{
				"delay":   orDefault(block.Properties["delay"], "1"),
				"facing":  orDefault(block.Properties["facing"], "north"),
				"locked":  boolStr(re.repeaterLocked(x, y, z, block)),
				"powered": boolStr(powered),
			},
		}
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:comparator":
		newBlock := block
		newBlock.Properties = make(map[string]string, len(block.Properties)+1)
		for key, value := range block.Properties {
			newBlock.Properties[key] = value
		}
		newBlock.Properties["powered"] = boolStr(powered)
		if block.Properties["powered"] == newBlock.Properties["powered"] {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:powered_rail", "minecraft:activator_rail":
		newBlock := redstoneBlockWith(block, "powered", boolStr(powered))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:hopper":
		newBlock := redstoneBlockWith(block, "enabled", boolStr(!powered))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:note_block":
		newBlock := redstoneBlockWith(block, "powered", boolStr(powered))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true

	case "minecraft:dispenser", "minecraft:dropper", "minecraft:crafter":
		newBlock := redstoneBlockWith(block, "triggered", boolStr(powered))
		if block.Equal(newBlock) {
			return BlockChange{}, false
		}
		re.world.setBlockNoPhysics(x, y, z, newBlock)
		return BlockChange{X: x, Y: y, Z: z, Block: newBlock}, true
	}
	return BlockChange{}, false
}

func redstoneBlockWith(block Block, properties ...string) Block {
	updated := block
	updated.Properties = make(map[string]string, len(block.Properties)+len(properties)/2)
	for key, value := range block.Properties {
		updated.Properties[key] = value
	}
	for index := 0; index+1 < len(properties); index += 2 {
		updated.Properties[properties[index]] = properties[index+1]
	}
	return updated
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

func atoi(value string) int {
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
