package server

import (
	"math"

	corentity "GoCraft/core/entity"
	"GoCraft/core/itemregistry"
	"GoCraft/core/navigation"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
)

const (
	pumpkinNavigatorRepathTicks       = 15
	pumpkinNavigatorRepathSpreadTicks = 15
)

// navigateMob owns the MOVE control for one tick. It mirrors Pumpkin's
// NavigatorGoal lifecycle: recompute when the destination changes or the
// cooldown expires, advance walk nodes as the mob reaches them, and stop when
// no valid loaded-chunk path exists.
func (s *Server) navigateMob(e *corentity.Entity, ai *mobAI, destination spatial.Vec3, speed float64) bool {
	if s.world == nil {
		return false
	}
	goal := spatial.BlockPos{
		X: int32(math.Floor(destination.X)),
		Y: int32(math.Floor(destination.Y)),
		Z: int32(math.Floor(destination.Z)),
	}
	if ai.repathTick > 0 {
		ai.repathTick--
	}
	// Keep following the current path until its refresh window expires. A
	// moving target may change block every tick; immediately recalculating for
	// every change made all nearby mobs run A* together. Offset refreshes by
	// entity ID so herds and hostile groups do not create a 15-tick CPU spike.
	if !ai.hasPathGoal || ai.repathTick <= 0 {
		path, _ := navigation.FindPath(s.world, e.Position, destination, 4096)
		ai.path = path
		ai.pathIndex = 0
		ai.pathGoal = goal
		ai.hasPathGoal = true
		phase := int(uint32(e.EntityID) % pumpkinNavigatorRepathSpreadTicks)
		ai.repathTick = pumpkinNavigatorRepathTicks + phase
	}

	for ai.pathIndex < len(ai.path) {
		waypoint := ai.path[ai.pathIndex]
		dx, dz := waypoint.X-e.Position.X, waypoint.Z-e.Position.Z
		if dx*dx+dz*dz > 0.35*0.35 || math.Abs(waypoint.Y-e.Position.Y) > 1.1 {
			break
		}
		ai.pathIndex++
	}
	if ai.pathIndex >= len(ai.path) {
		e.VX, e.VZ = 0, 0
		return false
	}

	waypoint := ai.path[ai.pathIndex]
	dx, dz := waypoint.X-e.Position.X, waypoint.Z-e.Position.Z
	distance := math.Hypot(dx, dz)
	if distance < 0.001 {
		e.VX, e.VZ = 0, 0
		return true
	}
	e.VX, e.VZ = dx/distance*speed, dz/distance*speed
	e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
	if waypoint.Y > e.Position.Y+0.6 && distance < 1 && e.OnGround {
		e.VY = 0.42
	}
	return true
}

func clearMobNavigation(e *corentity.Entity, ai *mobAI) {
	ai.path = nil
	ai.pathIndex = 0
	ai.hasPathGoal = false
	ai.repathTick = 0
	e.VX, e.VZ = 0, 0
}

func pumpkinMovementSpeed(t corentity.EntityType, modifier float64) float64 {
	settings, ok := pumpkinEntitySpawnSettingsByType[string(t)]
	if !ok || settings.movementSpeed <= 0 {
		return 0.1 * modifier
	}
	// Pumpkin feeds the attribute through LivingEntity travel acceleration;
	// GoCraft stores final horizontal velocity, whose equivalent scale is 0.5.
	return settings.movementSpeed * modifier * 0.5
}

// tickPassiveIdleGoals implements the common Pumpkin passive goal stack after
// higher-priority swim, escape-danger, sleeping, and breeding goals: Tempt,
// WanderAround, LookAtEntity, then RandomLookAround.
func (s *Server) tickPassiveIdleGoals(e *corentity.Entity, ai *mobAI) {
	if target := s.closestTemptingPlayer(e, 10); target != nil {
		ai.hasWanderGoal = false
		dx, dz := target.Position.X-e.Position.X, target.Position.Z-e.Position.Z
		e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
		if dx*dx+dz*dz <= 2.5*2.5 {
			clearMobNavigation(e, ai)
			return
		}
		s.navigateMob(e, ai, target.Position, pumpkinMovementSpeed(e.Type, 1.25))
		return
	}

	if ai.hasWanderGoal {
		if s.navigateMob(e, ai, ai.wanderTarget, pumpkinMovementSpeed(e.Type, 1.0)) {
			return
		}
		ai.hasWanderGoal = false
		clearMobNavigation(e, ai)
	}

	if ai.lookTick > 0 {
		ai.lookTick--
		e.VX, e.VZ = 0, 0
		e.Yaw = float32(math.Atan2(-(ai.lookX-e.Position.X), ai.lookZ-e.Position.Z) * 180 / math.Pi)
		return
	}

	// Pumpkin checks non-every-tick goals every second selector tick. A 1/600
	// roll here therefore matches WanderAroundGoal's to_goal_ticks(120) chance.
	ai.wanderTick--
	if ai.wanderTick <= 0 {
		ai.wanderTick = 2
		if ai.rng.Intn(60) == 0 {
			ai.wanderTarget = spatial.Vec3{
				X: e.Position.X + ai.rng.Float64()*20 - 10,
				Y: e.Position.Y + ai.rng.Float64()*14 - 7,
				Z: e.Position.Z + ai.rng.Float64()*20 - 10,
			}
			ai.hasWanderGoal = true
			if s.navigateMob(e, ai, ai.wanderTarget, pumpkinMovementSpeed(e.Type, 1.0)) {
				return
			}
			ai.hasWanderGoal = false
		}
	}

	if ai.rng.Float64() < 0.02 {
		if target := s.closestVisiblePlayer(e, 6); target != nil {
			ai.lookX, ai.lookZ = target.Position.X, target.Position.Z
		} else {
			angle := ai.rng.Float64() * 2 * math.Pi
			ai.lookX, ai.lookZ = e.Position.X+math.Cos(angle), e.Position.Z+math.Sin(angle)
		}
		ai.lookTick = 20 + ai.rng.Intn(20)
	}
	e.VX, e.VZ = 0, 0
}

func (s *Server) closestTemptingPlayer(e *corentity.Entity, maximumDistance float64) *player.Player {
	if s.game == nil {
		return nil
	}
	var closest *player.Player
	closestDistance := maximumDistance * maximumDistance
	s.game.OnlinePlayers(func(candidate *player.Player) {
		if candidate.Dimension != s.simulationDimension || candidate.Dead || candidate.GameMode == player.GameModeSpectator || !isTemptItem(e.Type, candidate.HeldItem().ItemID) {
			return
		}
		dx, dy, dz := candidate.Position.X-e.Position.X, candidate.Position.Y-e.Position.Y, candidate.Position.Z-e.Position.Z
		distance := dx*dx + dy*dy + dz*dz
		if distance < closestDistance && s.mobHasLineOfSight(e, candidate.Position, 1.62) {
			closest, closestDistance = candidate, distance
		}
	})
	return closest
}

func (s *Server) closestVisiblePlayer(e *corentity.Entity, maximumDistance float64) *player.Player {
	if s.game == nil {
		return nil
	}
	var closest *player.Player
	closestDistance := maximumDistance * maximumDistance
	s.game.OnlinePlayers(func(candidate *player.Player) {
		if candidate.Dimension != s.simulationDimension || candidate.Dead || candidate.GameMode == player.GameModeSpectator {
			return
		}
		dx, dy, dz := candidate.Position.X-e.Position.X, candidate.Position.Y-e.Position.Y, candidate.Position.Z-e.Position.Z
		distance := dx*dx + dy*dy + dz*dz
		if distance < closestDistance && s.mobHasLineOfSight(e, candidate.Position, 1.62) {
			closest, closestDistance = candidate, distance
		}
	})
	return closest
}

func isTemptItem(entityType corentity.EntityType, item string) bool {
	animal := ""
	switch entityType {
	case corentity.TypeCow, corentity.TypeMooshroom:
		animal = "cow"
	case corentity.TypeSheep:
		animal = "sheep"
	case corentity.TypeGoat:
		animal = "goat"
	case corentity.TypePig:
		animal = "pig"
	case corentity.TypeChicken:
		animal = "chicken"
	case corentity.TypeRabbit:
		animal = "rabbit"
	}
	return animal != "" && itemregistry.HasTag(item, "minecraft:"+animal+"_food")
}

func (s *Server) tickHostileIdleGoals(e *corentity.Entity, ai *mobAI) {
	if ai.hasWanderGoal {
		modifier := 1.0
		if e.Type == corentity.TypeCreeper {
			modifier = 0.8
		}
		if s.navigateMob(e, ai, ai.wanderTarget, pumpkinMovementSpeed(e.Type, modifier)) {
			return
		}
		ai.hasWanderGoal = false
		clearMobNavigation(e, ai)
	}
	if ai.lookTick > 0 {
		ai.lookTick--
		e.VX, e.VZ = 0, 0
		e.Yaw = float32(math.Atan2(-(ai.lookX-e.Position.X), ai.lookZ-e.Position.Z) * 180 / math.Pi)
		return
	}
	ai.wanderTick--
	if ai.wanderTick <= 0 {
		ai.wanderTick = 2
		if ai.rng.Intn(60) == 0 {
			ai.wanderTarget = spatial.Vec3{
				X: e.Position.X + ai.rng.Float64()*20 - 10,
				Y: e.Position.Y + ai.rng.Float64()*14 - 7,
				Z: e.Position.Z + ai.rng.Float64()*20 - 10,
			}
			ai.hasWanderGoal = true
			return
		}
	}
	if ai.rng.Float64() < 0.02 {
		if target := s.closestVisiblePlayer(e, 8); target != nil {
			ai.lookX, ai.lookZ = target.Position.X, target.Position.Z
		} else {
			angle := ai.rng.Float64() * 2 * math.Pi
			ai.lookX, ai.lookZ = e.Position.X+math.Cos(angle), e.Position.Z+math.Sin(angle)
		}
		ai.lookTick = 20 + ai.rng.Intn(20)
	}
	e.VX, e.VZ = 0, 0
}

func isAquaticMob(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeAxolotl, corentity.TypeCod, corentity.TypeDolphin,
		corentity.TypeGlowSquid, corentity.TypePufferfish, corentity.TypeSalmon,
		corentity.TypeSquid, corentity.TypeTadpole, corentity.TypeTropicalFish,
		corentity.TypeGuardian, corentity.TypeElderGuardian:
		return true
	}
	return false
}

func (s *Server) entityInWater(e *corentity.Entity) bool {
	if s.world == nil {
		return false
	}
	x, y, z := int(math.Floor(e.Position.X)), int(math.Floor(e.Position.Y)), int(math.Floor(e.Position.Z))
	cx, cz := spatialChunkCoords(x, z)
	return s.world.IsChunkLoaded(cx, cz) && s.world.GetBlock(x, y, z).ResourceLocation() == "minecraft:water"
}

func (s *Server) tickAquaticMobAI(e *corentity.Entity, ai *mobAI) {
	if !s.entityInWater(e) {
		if e.OnGround && ai.rng.Intn(20) == 0 {
			e.VY = 0.3
			e.VX = (ai.rng.Float64() - 0.5) * 0.2
			e.VZ = (ai.rng.Float64() - 0.5) * 0.2
		}
		return
	}
	validTarget := ai.hasWanderGoal && s.waterAt(ai.wanderTarget)
	if !validTarget || ai.wanderTick <= 0 {
		ai.hasWanderGoal = false
		for attempt := 0; attempt < 10; attempt++ {
			candidate := spatial.Vec3{
				X: e.Position.X + ai.rng.Float64()*16 - 8,
				Y: e.Position.Y + ai.rng.Float64()*8 - 4,
				Z: e.Position.Z + ai.rng.Float64()*16 - 8,
			}
			if s.waterAt(candidate) {
				ai.wanderTarget = candidate
				ai.hasWanderGoal = true
				break
			}
		}
		ai.wanderTick = 40 + ai.rng.Intn(80)
	} else {
		ai.wanderTick--
	}
	if !ai.hasWanderGoal {
		e.VX, e.VZ = 0, 0
		return
	}
	dx := ai.wanderTarget.X - e.Position.X
	dy := ai.wanderTarget.Y - e.Position.Y
	dz := ai.wanderTarget.Z - e.Position.Z
	distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if distance < 0.5 {
		ai.hasWanderGoal = false
		e.VX, e.VY, e.VZ = 0, 0, 0
		return
	}
	speed := pumpkinMovementSpeed(e.Type, 1.0)
	if speed < 0.04 {
		speed = 0.04
	}
	e.VX, e.VY, e.VZ = dx/distance*speed, dy/distance*speed, dz/distance*speed
	e.Yaw = float32(math.Atan2(-dx, dz) * 180 / math.Pi)
}

func (s *Server) waterAt(position spatial.Vec3) bool {
	if s.world == nil {
		return false
	}
	x, y, z := int(math.Floor(position.X)), int(math.Floor(position.Y)), int(math.Floor(position.Z))
	cx, cz := spatialChunkCoords(x, z)
	return s.world.IsChunkLoaded(cx, cz) && s.world.GetBlock(x, y, z).ResourceLocation() == "minecraft:water"
}

func spatialChunkCoords(x, z int) (int32, int32) {
	return int32(math.Floor(float64(x) / 16)), int32(math.Floor(float64(z) / 16))
}
