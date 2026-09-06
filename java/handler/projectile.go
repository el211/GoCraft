package handler

import (
	"encoding/binary"
	"math"
	"time"

	corentity "GoCraft/core/entity"
	"GoCraft/core/itemregistry"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/session"
)

const windChargeCooldown = 500 * time.Millisecond

// UseThrowable handles the instant-use vanilla throwable family shared by
// Java and Bedrock. It returns false for items that are not throwables.
func UseThrowable(p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32) bool {
	if p == nil || w == nil || mgr == nil || nextEntityID == nil || p.Dead || p.GameMode == player.GameModeSpectator {
		return false
	}
	stack := p.HeldItem()
	projectileType := corentity.EntityType("")
	speed := 1.5
	sound := ""
	switch stack.ItemID {
	case "minecraft:snowball":
		projectileType, sound = corentity.TypeSnowball, "minecraft:entity.snowball.throw"
	case "minecraft:egg":
		projectileType, sound = corentity.TypeEgg, "minecraft:entity.egg.throw"
	case "minecraft:ender_pearl":
		projectileType, sound = corentity.TypeEnderPearl, "minecraft:entity.ender_pearl.throw"
	case "minecraft:experience_bottle":
		projectileType, speed, sound = corentity.TypeExperienceBottle, 0.7, "minecraft:entity.experience_bottle.throw"
	case "minecraft:splash_potion", "minecraft:lingering_potion":
		projectileType, speed, sound = corentity.TypePotion, 0.5, "minecraft:entity.splash_potion.throw"
	default:
		return false
	}

	id := nextEntityID()
	var uuid [16]byte
	binary.BigEndian.PutUint32(uuid[:4], uint32(id))
	copy(uuid[4:], p.UUID[:12])
	yaw := float64(p.Rotation.Yaw) * math.Pi / 180
	pitchDegrees := float64(p.Rotation.Pitch)
	if projectileType == corentity.TypeExperienceBottle || projectileType == corentity.TypePotion {
		pitchDegrees += 20
	}
	pitch := pitchDegrees * math.Pi / 180
	cosPitch := math.Cos(pitch)
	projectile := corentity.New(id, uuid, projectileType, p.Position.X, p.Position.Y+1.52, p.Position.Z)
	projectile.OwnerEntityID = p.EntityID
	if projectileType == corentity.TypePotion {
		projectile.ProjectileItem = stack
		projectile.ProjectileItem.Count = 1
	}
	projectile.VX = -math.Sin(yaw) * cosPitch * speed
	projectile.VY = -math.Sin(pitch) * speed
	projectile.VZ = math.Cos(yaw) * cosPitch * speed
	projectile.Yaw, projectile.Pitch = p.Rotation.Yaw, float32(pitchDegrees)
	w.Entities.Add(projectile)
	BroadcastSpawnMobInDimension(projectile, mgr, p.Dimension)
	BroadcastSoundAtDimension(mgr, p.Dimension, sound, soundCategoryPlayers,
		projectile.Position.X, projectile.Position.Y, projectile.Position.Z, 0.5, 1)
	if p.GameMode != player.GameModeCreative {
		slot := player.HotbarStart + p.HeldSlot
		p.Inventory[slot].Count--
		normalizeStack(&p.Inventory[slot])
		p.ContainerStateID++
		if conn != nil {
			_ = SyncPlayerInventory(conn, p)
		}
	}
	return true
}

// UseWindCharge spawns the shared wind-charge projectile for either edition.
// The caller supplies the edition's live connection (nil for Bedrock).
func UseWindCharge(p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32) bool {
	if p == nil || w == nil || mgr == nil || nextEntityID == nil || p.Dead || p.GameMode == player.GameModeSpectator ||
		p.HeldItem().ItemID != "minecraft:wind_charge" {
		return false
	}
	now := time.Now()
	if !p.LastWindChargeUse.IsZero() && now.Sub(p.LastWindChargeUse) < windChargeCooldown {
		return true
	}
	p.LastWindChargeUse = now
	id := nextEntityID()
	var uuid [16]byte
	binary.BigEndian.PutUint32(uuid[:4], uint32(id))
	copy(uuid[4:], p.UUID[:12])
	yaw := float64(p.Rotation.Yaw) * math.Pi / 180
	pitch := float64(p.Rotation.Pitch) * math.Pi / 180
	cosPitch := math.Cos(pitch)
	projectile := corentity.New(id, uuid, corentity.TypeWindCharge, p.Position.X, p.Position.Y+1.52, p.Position.Z)
	projectile.OwnerEntityID = p.EntityID
	projectile.VX = -math.Sin(yaw) * cosPitch * 1.5
	projectile.VY = -math.Sin(pitch) * 1.5
	projectile.VZ = math.Cos(yaw) * cosPitch * 1.5
	projectile.Yaw, projectile.Pitch = p.Rotation.Yaw, p.Rotation.Pitch
	w.Entities.Add(projectile)
	BroadcastSpawnMobInDimension(projectile, mgr, p.Dimension)
	BroadcastSoundAtDimension(mgr, p.Dimension, "minecraft:entity.wind_charge.throw", soundCategoryPlayers,
		projectile.Position.X, projectile.Position.Y, projectile.Position.Z, 0.5, 1)
	if p.GameMode == player.GameModeSurvival {
		slot := player.HotbarStart + p.HeldSlot
		p.Inventory[slot].Count--
		normalizeStack(&p.Inventory[slot])
		if conn != nil {
			_ = SyncPlayerInventory(conn, p)
		}
	}
	return true
}

// UseEnderEye launches an eye toward the closest Pumpkin stronghold ring.
func UseEnderEye(p *player.Player, w *coreworld.World, mgr *session.Manager, nextEntityID func() int32) {
	if p == nil || w == nil || mgr == nil || nextEntityID == nil || p.Dead || p.GameMode == player.GameModeSpectator {
		return
	}
	targetX, targetZ, ok := w.NearestStronghold(int(math.Floor(p.Position.X)), int(math.Floor(p.Position.Z)), 100)
	if !ok {
		return
	}
	id := nextEntityID()
	var uuid [16]byte
	binary.BigEndian.PutUint32(uuid[:4], uint32(id))
	copy(uuid[4:], p.UUID[:12])
	yaw := float64(p.Rotation.Yaw) * math.Pi / 180
	pitch := float64(p.Rotation.Pitch) * math.Pi / 180
	cosPitch := math.Cos(pitch)
	const speed = 0.9
	eye := corentity.New(id, uuid, corentity.TypeEyeOfEnder, p.Position.X, p.Position.Y+1.62, p.Position.Z)
	eye.OwnerEntityID = p.EntityID
	dx, dz := float64(targetX)-eye.Position.X, float64(targetZ)-eye.Position.Z
	distance := math.Hypot(dx, dz)
	if distance > 12 {
		eye.EyeTarget = spatial.Vec3{
			X: eye.Position.X + dx/distance*12,
			Y: eye.Position.Y + 8,
			Z: eye.Position.Z + dz/distance*12,
		}
	} else {
		eye.EyeTarget = spatial.Vec3{X: float64(targetX), Z: float64(targetZ)}
	}
	eye.HasEyeTarget = true
	eye.EyeSurvives = uint32(id)*1103515245%5 != 0
	eye.VX = -math.Sin(yaw) * cosPitch * speed
	eye.VY = -math.Sin(pitch)*speed + 0.15 // slight upward arc like vanilla
	eye.VZ = math.Cos(yaw) * cosPitch * speed
	eye.Yaw, eye.Pitch = p.Rotation.Yaw, p.Rotation.Pitch
	w.Entities.Add(eye)
	BroadcastSpawnMobInDimension(eye, mgr, p.Dimension)
	BroadcastSoundAtDimension(mgr, p.Dimension, "minecraft:entity.ender_eye.launch", soundCategoryPlayers,
		eye.Position.X, eye.Position.Y, eye.Position.Z, 0.5, 1)
	if p.GameMode != player.GameModeCreative {
		slot := player.HotbarStart + p.HeldSlot
		p.Inventory[slot].Count--
		normalizeStack(&p.Inventory[slot])
	}
}

func releaseRangedItem(p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32) {
	if _, _, food := player.FoodValue(p.UsingItemID); food {
		if !TickJavaFoodUse(p, conn, mgr, time.Now()) {
			clearJavaFoodUse(p)
		}
		return
	}
	itemID := p.UsingItemID
	started := p.UsingItemSince
	p.UsingItemID = ""
	p.UsingItemSince = time.Time{}
	p.UsingItemSlot = -1
	if itemID == "" || started.IsZero() || nextEntityID == nil || w == nil || mgr == nil {
		return
	}
	ticks := int(time.Since(started) / (50 * time.Millisecond))
	projectileType := corentity.TypeArrow
	speed, damage := 0.0, float32(0)
	sound := "minecraft:entity.arrow.shoot"
	switch itemID {
	case "minecraft:bow":
		power := float64(ticks) / 20
		power = (power*power + power*2) / 3
		if power < 0.1 {
			return
		}
		if power > 1 {
			power = 1
		}
		if !consumeArrow(p) {
			return
		}
		speed = power * 3
		damage = float32(2 + power*4)
		// Power enchantment: each level adds 0.5 * (level + 1) extra damage.
		if lvl := p.Inventory[player.HotbarStart+p.HeldSlot].EnchantmentLevel("minecraft:power"); lvl > 0 {
			damage += float32(0.5 * float64(lvl+1))
		}
	case "minecraft:crossbow":
		// Vanilla crossbow: releasing after ≥25 ticks loads the crossbow but
		// does NOT fire. A second right-click fires the loaded arrow.
		if ticks < 25 || !consumeArrow(p) {
			return
		}
		p.CrossbowLoaded = true
		if mgr != nil {
			BroadcastSoundAt(mgr, "minecraft:item.crossbow.loading_end", soundCategoryPlayers,
				p.Position.X, p.Position.Y, p.Position.Z, 1, 1)
		}
		return
	case "minecraft:trident":
		if ticks < 10 {
			return
		}
		projectileType = corentity.TypeTrident
		speed, damage = 2.5, 8
		sound = "minecraft:item.trident.throw"
	default:
		return
	}

	id := nextEntityID()
	var uuid [16]byte
	binary.BigEndian.PutUint32(uuid[:4], uint32(id))
	copy(uuid[4:], p.UUID[:12])
	yaw := float64(p.Rotation.Yaw) * math.Pi / 180
	pitch := float64(p.Rotation.Pitch) * math.Pi / 180
	cosPitch := math.Cos(pitch)
	projectile := corentity.New(id, uuid, projectileType, p.Position.X, p.Position.Y+1.52, p.Position.Z)
	projectile.OwnerEntityID = p.EntityID
	projectile.ProjectileDamage = damage
	projectile.VX = -math.Sin(yaw) * cosPitch * speed
	projectile.VY = -math.Sin(pitch) * speed
	projectile.VZ = math.Cos(yaw) * cosPitch * speed
	projectile.Yaw = p.Rotation.Yaw
	projectile.Pitch = p.Rotation.Pitch
	w.Entities.Add(projectile)
	BroadcastSpawnMob(projectile, mgr)
	BroadcastSoundAt(mgr, sound, soundCategoryPlayers,
		projectile.Position.X, projectile.Position.Y, projectile.Position.Z, 1, 1)
	damageHeldItem(p, conn, 1)
}

// fireCrossbowLoaded fires the stored arrow from a loaded crossbow.
// Called when the player right-clicks with a crossbow that has CrossbowLoaded=true.
func fireCrossbowLoaded(p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32) {
	if p == nil || w == nil || mgr == nil || nextEntityID == nil || p.Dead || p.GameMode == player.GameModeSpectator {
		return
	}
	id := nextEntityID()
	var uuid [16]byte
	binary.BigEndian.PutUint32(uuid[:4], uint32(id))
	copy(uuid[4:], p.UUID[:12])
	yaw := float64(p.Rotation.Yaw) * math.Pi / 180
	pitch := float64(p.Rotation.Pitch) * math.Pi / 180
	cosPitch := math.Cos(pitch)
	const speed = 3.15
	const damage = float32(9)
	projectile := corentity.New(id, uuid, corentity.TypeArrow, p.Position.X, p.Position.Y+1.52, p.Position.Z)
	projectile.OwnerEntityID = p.EntityID
	projectile.ProjectileDamage = damage
	projectile.VX = -math.Sin(yaw) * cosPitch * speed
	projectile.VY = -math.Sin(pitch) * speed
	projectile.VZ = math.Cos(yaw) * cosPitch * speed
	projectile.Yaw = p.Rotation.Yaw
	projectile.Pitch = p.Rotation.Pitch
	w.Entities.Add(projectile)
	BroadcastSpawnMob(projectile, mgr)
	BroadcastSoundAt(mgr, "minecraft:item.crossbow.shoot", soundCategoryPlayers,
		projectile.Position.X, projectile.Position.Y, projectile.Position.Z, 1, 1)
	damageHeldItem(p, conn, 1)
}

func consumeArrow(p *player.Player) bool {
	if p.GameMode == player.GameModeCreative {
		return true
	}
	for index := range p.Inventory {
		if !itemregistry.HasTag(p.Inventory[index].ItemID, "minecraft:arrows") {
			continue
		}
		p.Inventory[index].Count--
		normalizeStack(&p.Inventory[index])
		return true
	}
	return false
}
