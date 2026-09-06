package server

import (
	"math"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	"GoCraft/java/handler"
	"GoCraft/java/session"
)

const splashPotionRadius = 4.0

func (s *Server) applySplashPotion(projectileItem player.ItemStack, position spatial.Vec3) {
	s.applySplashPotionScaled(projectileItem, position, 1)
}

func (s *Server) applySplashPotionScaled(projectileItem player.ItemStack, position spatial.Vec3, durationMultiplier float64) {
	outcome, ok := player.PotionOutcomeFor(projectileItem)
	if !ok || s.game == nil {
		return
	}
	for _, target := range s.allPlayerSessions() {
		p := target.Player
		if p == nil || p.Dead || p.Dimension != s.simulationDimension {
			continue
		}
		dx := p.Position.X - position.X
		dy := p.Position.Y + 0.9 - position.Y
		dz := p.Position.Z - position.Z
		distance := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if distance >= splashPotionRadius {
			continue
		}
		scale := 1 - distance/splashPotionRadius
		s.applyPotionOutcome(target, outcome, scale, scale*durationMultiplier)
	}
}

func (s *Server) applyPotionOutcome(target *session.Session, outcome player.PotionOutcome, potencyScale, durationScale float64) {
	p := target.Player
	healthChanged := outcome.Heal > 0 && p.Heal(outcome.Heal*float32(potencyScale))
	if outcome.Damage > 0 {
		handler.DamagePlayerMagic(target, outcome.Damage*float32(potencyScale), "was killed by magic", s.sessions)
	}
	glowingChanged := false
	for _, effect := range outcome.Effects {
		effect.Duration = int32(float64(effect.Duration) * durationScale)
		if effect.Duration < 20 {
			continue
		}
		stored, changed := p.AddStatusEffect(effect)
		if !changed {
			continue
		}
		if p.Edition == player.ClientEditionJava {
			handler.SendMobEffect(target.Conn, p, stored.ID, stored.Amplifier, stored.Duration)
			if stored.ID == "minecraft:glowing" || stored.ID == "minecraft:invisibility" {
				glowingChanged = true
			}
		} else if effectType := bedrockEffectType(stored.ID); effectType != 0 && s.bedrockListener != nil {
			s.bedrockListener.SendPlayerMobEffect(p, effectType, stored.Amplifier, stored.Duration)
		}
	}
	if glowingChanged && s.sessions != nil {
		handler.BroadcastPlayerSharedFlags(p.EntityID, handler.PlayerSharedFlags(p), s.sessions)
	}
	if healthChanged && p.Edition == player.ClientEditionJava {
		_ = handler.SyncPlayerHealth(target.Conn, p)
	}
}
