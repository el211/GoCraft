package handler

import (
	"encoding/binary"
	"math"
	"time"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/session"
)

func releaseRangedItem(p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32) {
	itemID := p.UsingItemID
	started := p.UsingItemSince
	p.UsingItemID = ""
	p.UsingItemSince = time.Time{}
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
	case "minecraft:crossbow":
		if ticks < 25 || !consumeArrow(p) {
			return
		}
		speed, damage = 3.15, 9
		sound = "minecraft:item.crossbow.shoot"
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

func consumeArrow(p *player.Player) bool {
	if p.GameMode == player.GameModeCreative {
		return true
	}
	for index := range p.Inventory {
		if p.Inventory[index].ItemID != "minecraft:arrow" &&
			p.Inventory[index].ItemID != "minecraft:spectral_arrow" {
			continue
		}
		p.Inventory[index].Count--
		normalizeStack(&p.Inventory[index])
		return true
	}
	return false
}
