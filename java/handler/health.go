package handler

import (
	"fmt"
	"math"
	"time"

	"GoCraft/core/player"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

// buildUpdateHealth synchronizes the hearts, hunger, and saturation HUD.
func buildUpdateHealth(p *player.Player) *protocol.Packet {
	health, food, saturation, _ := p.HealthSnapshot()
	return protocol.NewBuilder(packetIDUpdateHealth).
		Float(health).
		VarInt(food).
		Float(saturation).
		Build()
}

func sendUpdateHealth(conn *network.ClientConn, p *player.Player) error {
	if conn == nil {
		return nil
	}
	return conn.WritePacket(buildUpdateHealth(p))
}

func buildDeathCombatEvent(p *player.Player, message string) *protocol.Packet {
	return protocol.NewBuilder(packetIDDeathCombatEvent).
		VarInt(p.EntityID).
		Bytes(nbtTextComponent(message)).
		Build()
}

// buildRespawn returns the protocol-769 Respawn packet. The SpawnInfo layout
// mirrors the initial Login packet but contains no dimension list.
func buildRespawn(p *player.Player, dimensionTypeID int32, hashedSeed int64) *protocol.Packet {
	return protocol.NewBuilder(packetIDRespawn).
		VarInt(dimensionTypeID).
		String(overworldDimensionName).
		Long(hashedSeed).
		Byte(byte(p.GameMode)).
		Byte(byte(p.GameMode)).
		Bool(false). // debug
		Bool(false). // flat
		Bool(false). // no last-death location
		VarInt(0).   // portal cooldown
		VarInt(63).  // sea level
		Byte(0).     // do not preserve attributes or entity metadata
		Build()
}

// reducedDamage applies vanilla's current armour/toughness formula. PvP can
// select the legacy formula to match the 1.7.10 armour model.
func reducedDamage(p *player.Player, damage float32, legacyArmor bool) float32 {
	if damage <= 0 {
		return 0
	}
	armour := float32(p.ArmorPoints())
	if legacyArmor {
		return damage * (25 - armour) / 25
	}
	toughness := p.ArmorToughness()
	reduction := float32(math.Min(20, math.Max(float64(armour/5), float64(armour-damage/(2+toughness/4)))))
	return damage * (1 - reduction/25)
}

// DamagePlayer applies a normal survival hit using modern armour reduction.
func DamagePlayer(target *session.Session, rawDamage float32, cause string, mgr *session.Manager) bool {
	return damagePlayer(target, rawDamage, cause, mgr, false, false)
}

// DamagePlayerLegacy applies the pre-1.9 armour formula used by legacy PvP.
func DamagePlayerLegacy(target *session.Session, rawDamage float32, cause string, mgr *session.Manager) bool {
	return damagePlayer(target, rawDamage, cause, mgr, true, false)
}

func damagePlayer(target *session.Session, rawDamage float32, cause string, mgr *session.Manager, legacyArmor, bypassInvulnerability bool) bool {
	if target == nil || target.Player == nil || rawDamage <= 0 {
		return false
	}
	p := target.Player
	if !bypassInvulnerability && p.GodMode {
		return false
	}
	if !bypassInvulnerability && (p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator) {
		return false
	}
	if !bypassInvulnerability && time.Now().Before(p.InvulnerableUntil) {
		return false
	}
	_, _, _, alreadyDead := p.HealthSnapshot()
	if alreadyDead {
		return false
	}

	damage := rawDamage
	if !bypassInvulnerability {
		damage = reducedDamage(p, rawDamage, legacyArmor)
	}
	if damage <= 0 {
		return false
	}
	_, died := p.ApplyDamage(damage, cause)

	if !bypassInvulnerability {
		armourWear := int(math.Floor(float64(rawDamage) / 4))
		if armourWear < 1 {
			armourWear = 1
		}
		DamagePlayerArmor(p, target.Conn, armourWear)
	}
	_ = sendUpdateHealth(target.Conn, p)
	if mgr != nil {
		BroadcastHurtAnimation(p.EntityID, p.Rotation.Yaw, mgr)
	}
	if died {
		if p.OnDeath != nil {
			p.OnDeath(p)
		}
		if mgr != nil {
			BroadcastDeathAnimation(p.EntityID, mgr)
		}
		message := fmt.Sprintf("%s %s", p.Username, cause)
		if target.Conn != nil {
			_ = target.Conn.WritePacket(buildDeathCombatEvent(p, message))
		}
		if mgr != nil {
			BroadcastSystemMessage(mgr, message)
		}
	}
	return true
}

// KillPlayer kills even creative/spectator players, as vanilla /kill does.
func KillPlayer(target *session.Session, cause string, mgr *session.Manager) bool {
	if target == nil || target.Player == nil {
		return false
	}
	maxHealth := target.Player.MaxHealth
	if maxHealth <= 0 {
		maxHealth = 20
	}
	return damagePlayer(target, maxHealth+1, cause, mgr, false, true)
}

// SendLegacyKnockback applies the configured 1.7-style impulse, reduced by
// netherite armour's knockback resistance.
func SendLegacyKnockback(target *session.Session, sourceX, sourceZ, horizontal, vertical float64) {
	if target == nil || target.Player == nil || target.Conn == nil {
		return
	}
	dx := target.Player.Position.X - sourceX
	dz := target.Player.Position.Z - sourceZ
	distance := math.Hypot(dx, dz)
	if distance < 0.0001 {
		dx, dz, distance = 0, 1, 1
	}
	resistance := 1 - float64(target.Player.KnockbackResistance())
	SendPlayerKnockback(target.Conn, target.Player.EntityID,
		dx/distance*horizontal*resistance, vertical*resistance, dz/distance*horizontal*resistance)
}
