package server

import (
	"math"
	"math/rand"

	corentity "GoCraft/core/entity"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"
)

// EndermanTeleportRadius is the max block radius for a damage-triggered teleport.
const EndermanTeleportRadius = 32

// tryEndermanTeleport mirrors Pumpkin's 64 random landing attempts. It only
// considers loaded chunks and rejects water, blocked headroom, and void floors.
func (s *Server) tryEndermanTeleport(enderman *corentity.Entity) bool {
	if s == nil || s.world == nil || enderman == nil {
		return false
	}
	seed := int64(enderman.EntityID)*6364136223846793005 + enderman.AgeTicks
	rng := rand.New(rand.NewSource(seed))
	for attempt := 0; attempt < 64; attempt++ {
		x := enderman.Position.X + (rng.Float64()-0.5)*64
		z := enderman.Position.Z + (rng.Float64()-0.5)*64
		feetY := int(math.Floor(enderman.Position.Y)) + rng.Intn(64) - 32
		cx, cz := coreworld.ChunkCoordsFor(int(math.Floor(x)), int(math.Floor(z)))
		if !s.world.IsChunkLoaded(cx, cz) {
			continue
		}
		for feetY > coreworld.WorldMinY+1 &&
			!coreworld.IsEntitySupportBlock(s.world.GetBlock(int(math.Floor(x)), feetY-1, int(math.Floor(z))).ResourceLocation()) {
			feetY--
		}
		if feetY <= coreworld.WorldMinY+1 || s.world.TouchesWater(x, float64(feetY), z) {
			continue
		}
		ok, loaded := s.world.CanEntityOccupyIfLoaded(x, float64(feetY), z)
		if !loaded || !ok || coreworld.IsEntitySupportBlock(
			s.world.GetBlock(int(math.Floor(x)), feetY+2, int(math.Floor(z))).ResourceLocation()) {
			continue
		}
		enderman.Position.X, enderman.Position.Y, enderman.Position.Z = x, float64(feetY), z
		enderman.VX, enderman.VY, enderman.VZ = 0, 0, 0
		return true
	}
	return false
}

// EndermanPickupBlocks lists block IDs an enderman can pick up. It mirrors the
// vanilla minecraft:enderman_holdable block tag.
var EndermanPickupBlocks = map[string]bool{
	"minecraft:grass_block":    true,
	"minecraft:dirt":           true,
	"minecraft:sand":           true,
	"minecraft:gravel":         true,
	"minecraft:brown_mushroom": true,
	"minecraft:red_mushroom":   true,
	"minecraft:cactus":         true,
	"minecraft:pumpkin":        true,
	"minecraft:melon":          true,
	"minecraft:mycelium":       true,
}

// Vanilla runs the enderman's pick-up and place goals as independent random
// checks every tick: roughly one attempt in 20 ticks to lift a block and one in
// 2000 to set it back down, so a carried block is held for a long while.
const (
	endermanPickupOneInTicks = 20
	endermanPlaceOneInTicks  = 2000
)

// tickEndermanBlockCarry implements the vanilla pick-up and place goals. An
// enderman holding nothing looks for a holdable block; one already carrying a
// block looks for somewhere to drop it.
//
// Vanilla also raycasts before lifting a block so an enderman cannot reach
// through a wall. mobHasLineOfSight is unsuitable here because it treats the
// destination block as an obstruction, which is always true when the
// destination is the block itself.
// TODO: add a block-aware raycast that stops short of the target.
func (s *Server) tickEndermanBlockCarry(e *corentity.Entity) {
	if e == nil || e.Type != corentity.TypeEnderman || e.Dead || s.world == nil {
		return
	}
	if e.EndermanCarriedBlock == "" {
		s.tryEndermanPickupBlock(e)
		return
	}
	s.tryEndermanPlaceBlock(e)
}

// tryEndermanPickupBlock lifts a holdable block into the enderman's hands and
// leaves air behind.
func (s *Server) tryEndermanPickupBlock(e *corentity.Entity) {
	if !endermanBlockRoll(e.EntityID, s.worldAge, endermanPickupOneInTicks) {
		return
	}
	x, y, z := endermanSampleBlock(e)
	if !s.endermanBlockReachable(x, y, z) {
		return
	}
	name := s.world.GetBlock(x, y, z).ResourceLocation()
	if !EndermanPickupBlocks[name] {
		return
	}
	s.setEndermanBlock(x, y, z, coreworld.Air)
	e.EndermanCarriedBlock = name
	s.broadcastEndermanCarryState(e)
}

// tryEndermanPlaceBlock puts the carried block back into the world. The target
// must be empty and supported from below, matching vanilla's placement rule.
func (s *Server) tryEndermanPlaceBlock(e *corentity.Entity) {
	if !endermanBlockRoll(e.EntityID, s.worldAge, endermanPlaceOneInTicks) {
		return
	}
	x, y, z := endermanSampleBlock(e)
	if !s.endermanBlockReachable(x, y, z) || !s.endermanBlockReachable(x, y-1, z) {
		return
	}
	if !s.world.GetBlock(x, y, z).IsAir() {
		return
	}
	if !coreworld.IsEntitySupportBlock(s.world.GetBlock(x, y-1, z).ResourceLocation()) {
		return
	}
	s.setEndermanBlock(x, y, z, coreworld.BlockFromResourceLocation(e.EndermanCarriedBlock))
	e.EndermanCarriedBlock = ""
	s.broadcastEndermanCarryState(e)
}

// broadcastEndermanCarryState republishes entity metadata after the carried
// block changes. It is deliberately not called every tick: only the two carry
// transitions alter what the client renders.
//
// Block properties are not stored, so coreworld.BlockFromResourceLocation
// resolves a carried block to its default state; vanilla keeps the full state.
func (s *Server) broadcastEndermanCarryState(e *corentity.Entity) {
	handler.BroadcastMobMetadata(e, s.javaSessionsForDimension(s.simulationDimension))
}

// endermanBlockRoll reuses the deterministic roll from tickEndermanWater.
func endermanBlockRoll(entityID int32, worldAge int64, oneIn uint64) bool {
	roll := uint64(entityID)*0x9e3779b97f4a7c15 ^ uint64(worldAge)*0xbf58476d1ce4e5b9
	return roll%oneIn == 0
}

// endermanSampleBlock picks one candidate position per attempt, approximating
// vanilla's search box: two blocks horizontally and one vertically around the
// enderman's feet, which keeps the ground it stands on in range.
func endermanSampleBlock(e *corentity.Entity) (x, y, z int) {
	seed := int64(e.EntityID)*6364136223846793005 + e.AgeTicks
	rng := rand.New(rand.NewSource(seed))
	return int(math.Floor(e.Position.X)) + rng.Intn(5) - 2,
		int(math.Floor(e.Position.Y)) + rng.Intn(3) - 1,
		int(math.Floor(e.Position.Z)) + rng.Intn(5) - 2
}

// endermanBlockReachable rejects positions outside the build limits or inside a
// chunk that is not resident, mirroring the guards used by tryEndermanTeleport.
func (s *Server) endermanBlockReachable(x, y, z int) bool {
	if y < coreworld.WorldMinY || y > coreworld.WorldMaxY {
		return false
	}
	cx, cz := coreworld.ChunkCoordsFor(x, z)
	return s.world.IsChunkLoaded(cx, cz)
}

// setEndermanBlock writes a canonical block and mirrors it to both editions.
// Java sessions are filtered to the simulated dimension; the Bedrock listener
// only publishes Overworld changes.
func (s *Server) setEndermanBlock(x, y, z int, block coreworld.Block) {
	s.world.SetBlock(x, y, z, block)
	change := coreworld.BlockChange{X: x, Y: y, Z: z, Block: block}
	handler.BroadcastBlockChange(change, s.javaSessionsForDimension(s.simulationDimension))
	if s.bedrockListener != nil && s.simulationDimension == dimensionOverworld {
		s.bedrockListener.BroadcastBlockChange(change)
	}
}
