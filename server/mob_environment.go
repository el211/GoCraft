package server

import (
	"math"
	"strconv"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"
)

const daylightBurnDurationTicks = 8 * 20

func burnsInDaylight(entityType corentity.EntityType) bool {
	switch entityType {
	case corentity.TypeZombie, corentity.TypeZombieVillager, corentity.TypeZombieHorse,
		corentity.TypeDrowned, corentity.TypeSkeleton, corentity.TypeStray,
		corentity.TypeBogged, corentity.TypeWitherSkeleton, corentity.TypePhantom:
		return true
	default:
		return false
	}
}

// tickEndermanWater applies 1 damage per tick when an enderman touches water
// or rain, matching PumpkinMC enderman.rs mob_tick water/rain check.
func (s *Server) tickEndermanWater(entity *corentity.Entity, hurtEntities *[]*corentity.Entity) {
	if entity == nil || entity.Type != corentity.TypeEnderman || s.world == nil {
		return
	}
	if s.entityInWater(entity) {
		entity.Damage(1)
		*hurtEntities = append(*hurtEntities, entity)
		roll := uint64(entity.EntityID)*0x9e3779b97f4a7c15 ^ uint64(s.worldAge)*0xbf58476d1ce4e5b9
		if roll%10 != 0 {
			oldPosition := entity.Position
			if s.tryEndermanTeleport(entity) {
				handler.BroadcastEntityPosition(entity, s.sessions)
				handler.BroadcastSoundAt(s.sessions, "minecraft:entity.enderman.teleport", handler.SoundCategoryHostile,
					oldPosition.X, oldPosition.Y, oldPosition.Z, 1, 1)
				handler.BroadcastSoundAt(s.sessions, "minecraft:entity.enderman.teleport", handler.SoundCategoryHostile,
					entity.Position.X, entity.Position.Y, entity.Position.Z, 1, 1)
			}
		}
	}
}

func (s *Server) tickMobSunlight(entity *corentity.Entity, hurtEntities *[]*corentity.Entity) {
	if entity == nil || s.world == nil {
		return
	}
	s.tickEndermanWater(entity, hurtEntities)
	// Water cauldron extinguishes fire for any burning entity.
	if entity.FireTicks > 0 {
		s.tryExtinguishInCauldron(entity)
	}
	if s.simulationDimension != dimensionOverworld || !burnsInDaylight(entity.Type) {
		return
	}
	wasBurning := entity.FireTicks > 0
	if s.mobInDirectDaylight(entity) && entity.FireTicks <= 0 {
		entity.FireTicks = daylightBurnDurationTicks
	}
	if s.entityInWater(entity) {
		entity.FireTicks = 0
	} else if entity.FireTicks > 0 {
		entity.FireTicks--
		if entity.FireTicks > 0 && entity.FireTicks%20 == 0 {
			entity.Damage(1)
			*hurtEntities = append(*hurtEntities, entity)
		}
	}
	if wasBurning != (entity.FireTicks > 0) {
		handler.BroadcastMobFireState(entity, s.sessions)
	}
}

// tryExtinguishInCauldron clears FireTicks and reduces the water cauldron
// level when a burning entity stands inside a water cauldron.
func (s *Server) tryExtinguishInCauldron(entity *corentity.Entity) {
	x := int(math.Floor(entity.Position.X))
	y := int(math.Floor(entity.Position.Y))
	z := int(math.Floor(entity.Position.Z))
	block := s.world.GetBlock(x, y, z)
	if block.ResourceLocation() != "minecraft:water_cauldron" {
		return
	}
	level, _ := strconv.Atoi(block.Properties["level"])
	if level <= 0 {
		return
	}
	entity.FireTicks = 0
	level--
	var replacement coreworld.Block
	if level == 0 {
		replacement = coreworld.Block{Namespace: "minecraft", Name: "cauldron"}
	} else {
		replacement = coreworld.Block{Namespace: "minecraft", Name: "water_cauldron",
			Properties: map[string]string{"level": strconv.Itoa(level)}}
	}
	s.world.SetBlock(x, y, z, replacement)
}

func (s *Server) mobInDirectDaylight(entity *corentity.Entity) bool {
	timeOfDay := s.worldAge % 24000
	if timeOfDay < 0 {
		timeOfDay += 24000
	}
	if timeOfDay >= 12542 && timeOfDay <= 23460 {
		return false
	}
	x := int(math.Floor(entity.Position.X))
	z := int(math.Floor(entity.Position.Z))
	headY := int(math.Floor(entity.Position.Y + 1.62))
	surfaceY, loaded := s.world.SurfaceYIfLoaded(x, z)
	return loaded && surfaceY <= headY
}

// mobHasLineOfSight samples the segment between eye positions. It is used at
// the moment damage or a ranged attack is produced, preventing entities on
// opposite sides of a solid wall from hitting one another.
func (s *Server) mobHasLineOfSight(attacker *corentity.Entity, target spatial.Vec3, targetEyeHeight float64) bool {
	if attacker == nil || s.world == nil {
		return false
	}
	start := spatial.Vec3{X: attacker.Position.X, Y: attacker.Position.Y + 1.4, Z: attacker.Position.Z}
	end := spatial.Vec3{X: target.X, Y: target.Y + targetEyeHeight, Z: target.Z}
	dx, dy, dz := end.X-start.X, end.Y-start.Y, end.Z-start.Z
	distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
	steps := int(math.Ceil(distance * 5))
	if steps < 2 {
		return true
	}
	for step := 1; step < steps; step++ {
		t := float64(step) / float64(steps)
		block := s.world.GetBlock(
			int(math.Floor(start.X+dx*t)),
			int(math.Floor(start.Y+dy*t)),
			int(math.Floor(start.Z+dz*t)),
		)
		if coreworld.IsEntitySupportBlock(block.ResourceLocation()) {
			return false
		}
	}
	return true
}

// isPlayerStaringAtEnderman returns true if the player is looking directly at
// the enderman's eye region. Matches PumpkinMC enderman.rs:is_player_staring.
// The angular threshold is dot > 1 - 0.025/distance (vanilla cone test).
func isPlayerStaringAtEnderman(p *player.Player, e *corentity.Entity) bool {
	if p == nil || e == nil {
		return false
	}
	// Carved pumpkin helmet grants immunity to provoking endermen.
	// Slot 5 = helmet in the Java inventory layout (slots 5-8 = armor).
	if p.Inventory[5].ItemID == "minecraft:carved_pumpkin" {
		return false
	}
	const endermanEyeHeight = 2.55
	const playerEyeHeight = 1.62

	endermanEyeY := e.Position.Y + endermanEyeHeight
	playerEyeY := p.Position.Y + playerEyeHeight

	// Player look direction from yaw/pitch (same convention as velocity formula).
	yawRad := -float64(p.Rotation.Yaw) * math.Pi / 180
	pitchRad := float64(p.Rotation.Pitch) * math.Pi / 180
	cosPitch := math.Cos(pitchRad)
	lookX := math.Sin(yawRad) * cosPitch
	lookY := -math.Sin(pitchRad)
	lookZ := math.Cos(yawRad) * cosPitch

	dx := e.Position.X - p.Position.X
	dy := endermanEyeY - playerEyeY
	dz := e.Position.Z - p.Position.Z
	distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if distance < 0.1 {
		return false
	}

	dot := lookX*(dx/distance) + lookY*(dy/distance) + lookZ*(dz/distance)
	return dot > 1.0-0.025/distance
}

func isSkeletonArcher(entityType corentity.EntityType) bool {
	switch entityType {
	case corentity.TypeSkeleton, corentity.TypeStray, corentity.TypeBogged:
		return true
	default:
		return false
	}
}

func (s *Server) setMobUsingItem(entity *corentity.Entity, using bool) {
	if entity == nil || entity.UsingItem == using {
		return
	}
	entity.UsingItem = using
	handler.BroadcastMobUsingItemState(entity, s.sessions)
}

func (s *Server) shootMobArrow(shooter *corentity.Entity, target *player.Player) {
	if shooter == nil || target == nil || s.game == nil || s.world == nil {
		return
	}
	start := spatial.Vec3{X: shooter.Position.X, Y: shooter.Position.Y + 1.45, Z: shooter.Position.Z}
	dx := target.Position.X - start.X
	dz := target.Position.Z - start.Z
	horizontal := math.Hypot(dx, dz)
	dy := target.Position.Y + 0.9 - start.Y + horizontal*0.03
	distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if distance < 0.001 {
		return
	}
	const speed = 1.6
	arrow := corentity.New(s.game.NextEntityID(), newRandomUUID(), corentity.TypeArrow, start.X, start.Y, start.Z)
	arrow.OwnerEntityID = shooter.EntityID
	arrow.ProjectileDamage = 3
	arrow.VX, arrow.VY, arrow.VZ = dx/distance*speed, dy/distance*speed, dz/distance*speed
	s.world.Entities.Add(arrow)
	handler.BroadcastSpawnMob(arrow, s.sessions)
	handler.BroadcastSoundAt(s.sessions, "minecraft:entity.skeleton.shoot", handler.SoundCategoryHostile,
		shooter.Position.X, shooter.Position.Y+1, shooter.Position.Z, 1, 1)
}

func (s *Server) shootMobProjectile(shooter *corentity.Entity, target *player.Player, projectileType corentity.EntityType, speed float64, damage float32, sound string) {
	if shooter == nil || target == nil || s.game == nil || s.world == nil {
		return
	}
	start := spatial.Vec3{X: shooter.Position.X, Y: shooter.Position.Y + 1.35, Z: shooter.Position.Z}
	dx := target.Position.X - start.X
	dz := target.Position.Z - start.Z
	horizontal := math.Hypot(dx, dz)
	dropCompensation := 0.0
	if projectileType == corentity.TypePotion {
		dropCompensation = horizontal * 0.05
	}
	dy := target.Position.Y + 0.9 - start.Y + dropCompensation
	distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if distance < 0.001 {
		return
	}
	projectile := corentity.New(s.game.NextEntityID(), newRandomUUID(), projectileType, start.X, start.Y, start.Z)
	projectile.OwnerEntityID = shooter.EntityID
	projectile.ProjectileDamage = damage
	projectile.VX, projectile.VY, projectile.VZ = dx/distance*speed, dy/distance*speed, dz/distance*speed
	s.world.Entities.Add(projectile)
	handler.BroadcastSpawnMob(projectile, s.sessions)
	if sound != "" {
		handler.BroadcastSoundAt(s.sessions, sound, handler.SoundCategoryHostile,
			shooter.Position.X, shooter.Position.Y+1, shooter.Position.Z, 1, 1)
	}
}
