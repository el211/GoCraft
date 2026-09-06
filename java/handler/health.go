package handler

import (
	"fmt"
	"math"
	"time"

	"GoCraft/core/player"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
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

// SyncPlayerHealth refreshes hearts, food and saturation for a Java player
// after an edition-neutral server tick changes survival state.
func SyncPlayerHealth(conn *network.ClientConn, p *player.Player) error {
	return sendUpdateHealth(conn, p)
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
		String(dimensionName(p.Dimension)).
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

// sendMobEffect sends a single status effect to the target connection.
// amplifier is 0-based (0 = level I). durationTicks is in game ticks (20/s).
func sendMobEffect(conn *network.ClientConn, entityID int32, effectName string, amplifier, durationTicks int32) {
	if conn == nil {
		return
	}
	effectID := javaworld.MobEffectID(effectName)
	if effectID < 0 {
		return
	}
	pkt := protocol.NewBuilder(packetIDUpdateMobEffect).
		VarInt(entityID).
		VarInt(effectID).
		VarInt(amplifier).
		VarInt(durationTicks).
		Byte(0x06). // show particles and icon
		Build()
	_ = conn.WritePacket(pkt)
}

// tryConsumeTotem checks if the player holds a totem of undying in main or
// offhand. If found, it consumes the totem, resets health to 1, applies
// vanilla totem effects, and returns true (death is prevented).
// Per PumpkinMC living.rs:1853-1931 and vanilla EntityLiving.java.
func tryConsumeTotem(target *session.Session) bool {
	if target == nil || target.Player == nil {
		return false
	}
	p := target.Player

	// Check main hand then offhand for a totem of undying.
	heldSlot := player.HotbarStart + p.HeldSlot
	offSlot := player.OffhandSlot
	totemSlot := -1
	for _, s := range []int{heldSlot, offSlot} {
		if p.Inventory[s].ItemID == "minecraft:totem_of_undying" {
			totemSlot = s
			break
		}
	}
	if totemSlot < 0 {
		return false
	}

	// Consume the totem.
	p.Inventory[totemSlot].Count--
	normalizeStack(&p.Inventory[totemSlot])
	if target.Conn != nil {
		_ = SyncPlayerInventory(target.Conn, p)
	}

	// Restore to 1 HP and clear dead flag.
	p.RestoreFromTotem()
	_ = sendUpdateHealth(target.Conn, p)

	// Send entity status 35 = "totem of undying animation" to all viewers.
	// (EntityStatus ProtectedFromDeath = 35 in Java protocol.)
	pkt := protocol.NewBuilder(packetIDEntityEvent).
		Int(p.EntityID).
		Byte(35).
		Build()
	if target.Conn != nil {
		_ = target.Conn.WritePacket(pkt)
	}

	// Apply effects: Absorption II (100t), Regeneration II (900t), Fire Resistance I (800t).
	effects := []player.StatusEffect{
		{ID: "minecraft:absorption", Amplifier: 1, Duration: 100, ShowParticles: true, ShowIcon: true},
		{ID: "minecraft:regeneration", Amplifier: 1, Duration: 900, ShowParticles: true, ShowIcon: true},
		{ID: "minecraft:fire_resistance", Duration: 800, ShowParticles: true, ShowIcon: true},
	}
	for _, effect := range effects {
		stored, _ := p.AddStatusEffect(effect)
		sendMobEffect(target.Conn, p.EntityID, stored.ID, stored.Amplifier, stored.Duration)
	}
	return true
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
	damage = damage * (1 - reduction/25)
	// Protection enchantment: 4 EPF per level, capped at 20 total.
	epf := 0
	for i := 5; i <= 8; i++ {
		epf += p.Inventory[i].EnchantmentLevel("minecraft:protection") * 4
	}
	if epf > 20 {
		epf = 20
	}
	if epf > 0 {
		damage = damage * float32(25-epf) / 25
	}
	return damage
}

// DamagePlayer applies a normal survival hit using modern armour reduction.
func DamagePlayer(target *session.Session, rawDamage float32, cause string, mgr *session.Manager) bool {
	return damagePlayerFromPos(target, rawDamage, cause, mgr, 0, 0, true, false)
}

// DamagePlayerFromSource applies damage to a player with a known attacker position
// so the shield blocking direction check can determine if the hit is from the front.
func DamagePlayerFromSource(target *session.Session, rawDamage float32, cause string, mgr *session.Manager, srcX, srcZ float64) bool {
	return damagePlayerFromPos(target, rawDamage, cause, mgr, srcX, srcZ, false, false)
}

// DamagePlayerLegacy applies the pre-1.9 armour formula used by legacy PvP.
func DamagePlayerLegacy(target *session.Session, rawDamage float32, cause string, mgr *session.Manager) bool {
	return damagePlayerFromPos(target, rawDamage, cause, mgr, 0, 0, true, false)
}

// isShieldBypassDamage returns true for damage types that bypass a held shield.
func isShieldBypassDamage(cause string) bool {
	switch cause {
	case "fell out of the world", "hit the ground too hard", "drowned",
		"tried to swim in lava", "went up in flames", "was pricked to death",
		"froze to death", "starved to death":
		return true
	}
	return false
}

func damagePlayerFromPos(target *session.Session, rawDamage float32, cause string, mgr *session.Manager, srcX, srcZ float64, noSource, bypassInvulnerability bool) bool {
	if target == nil || target.Player == nil || rawDamage <= 0 {
		return false
	}
	p := target.Player

	// Shield blocking check (based on PumpkinMC living.rs:2448-2503).
	// If the player is using a shield and the attack is from the front, block it.
	if !bypassInvulnerability && !isShieldBypassDamage(cause) && !noSource {
		shieldInMain := p.Inventory[player.HotbarStart+p.HeldSlot].ItemID == "minecraft:shield"
		shieldInOff := p.Inventory[player.OffhandSlot].ItemID == "minecraft:shield"
		usingShield := (shieldInMain || shieldInOff) &&
			p.UsingItemID == "minecraft:shield" &&
			!p.UsingItemSince.IsZero() &&
			time.Since(p.UsingItemSince) >= 250*time.Millisecond

		if usingShield {
			// Check if the attack comes from the front hemisphere.
			// Player's look direction: yaw 0=south (+Z), 90=west (-X), 180=north (-Z), -90=east (+X).
			yawRad := float64(p.Rotation.Yaw) * math.Pi / 180
			lookX := -math.Sin(yawRad)
			lookZ := math.Cos(yawRad)
			dx, dz := p.Position.X-srcX, p.Position.Z-srcZ
			dist := math.Hypot(dx, dz)
			if dist > 0.001 {
				dx, dz = dx/dist, dz/dist
			}
			// Dot product < 0 means attack comes from in front.
			if lookX*dx+lookZ*dz < 0 {
				if mgr != nil {
					BroadcastSoundAt(mgr, "minecraft:item.shield.block", soundCategoryPlayers,
						p.Position.X, p.Position.Y, p.Position.Z, 1, 1)
				}
				return false // Damage fully blocked
			}
		}
	}

	return damagePlayer(target, rawDamage, cause, mgr, false, bypassInvulnerability, false)
}

// DamagePlayerMagic applies effect damage without armour reduction or wear.
func DamagePlayerMagic(target *session.Session, rawDamage float32, cause string, mgr *session.Manager) bool {
	return damagePlayer(target, rawDamage, cause, mgr, false, false, true)
}

func damagePlayer(target *session.Session, rawDamage float32, cause string, mgr *session.Manager, legacyArmor, bypassInvulnerability, bypassArmor bool) bool {
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
	if !bypassInvulnerability && !bypassArmor {
		damage = reducedDamage(p, rawDamage, legacyArmor)
	}
	if damage <= 0 {
		return false
	}
	_, died := p.ApplyDamage(damage, cause)

	if !bypassInvulnerability && !bypassArmor {
		armourWear := int(math.Floor(float64(rawDamage) / 4))
		if armourWear < 1 {
			armourWear = 1
		}
		DamagePlayerArmor(p, target.Conn, armourWear)
	}
	_ = sendUpdateHealth(target.Conn, p)
	if mgr != nil {
		sound := "minecraft:entity.player.hurt"
		broadcastSoundAt(mgr, sound, soundCategoryPlayers,
			target.Player.Position.X, target.Player.Position.Y, target.Player.Position.Z, 1.0, 1.0)
		BroadcastHurtAnimation(p.EntityID, p.Rotation.Yaw, mgr)
	}
	if died {
		// Sleeping is a tracked client pose, not just a canonical bool. Clear it
		// immediately when death wins so Java does not carry the bed pose through
		// Respawn, and Bedrock's next sync observes the same standing state.
		if p.Sleeping {
			p.Sleeping = false
			if mgr != nil {
				BroadcastPlayerWaking(p.EntityID, mgr)
			}
		}

		// Totem of undying: if the player holds one, prevent death.
		if tryConsumeTotem(target) {
			return true
		}
		if p.OnDeath != nil {
			p.OnDeath(p)
		}
		if mgr != nil {
			BroadcastDeathAnimation(p.EntityID, mgr)
			BroadcastSystemMessage(mgr, fmt.Sprintf("%v %v", target.Player.Username, cause))
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
	return damagePlayer(target, maxHealth+1, cause, mgr, false, true, true)
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
