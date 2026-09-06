package server

import (
	"GoCraft/core/player"
	"GoCraft/java/handler"
	"GoCraft/java/session"
)

// tickPlayerStatusEffects advances the shared effect state and applies its
// gameplay consequences through the normal health and hunger paths.
func (s *Server) tickPlayerStatusEffects() {
	if s.game == nil {
		return
	}
	s.game.OnlinePlayers(func(p *player.Player) {
		tick := p.TickStatusEffects()
		target := &session.Session{Player: p}
		if p.Edition == player.ClientEditionJava && s.sessions != nil {
			if current, ok := s.sessions.Get(p.UUID); ok && current != nil {
				target = current
			}
		}

		healthChanged := tick.Heal > 0 && p.Heal(tick.Heal)
		if tick.Exhaustion > 0 {
			p.AddExhaustion(tick.Exhaustion)
			healthChanged = true
		}
		if tick.Damage > 0 {
			cause := "was poisoned"
			if tick.CanKill {
				cause = "withered away"
			}
			handler.DamagePlayerMagic(target, tick.Damage, cause, s.sessions)
		}
		if healthChanged && p.Edition == player.ClientEditionJava {
			_ = handler.SyncPlayerHealth(target.Conn, p)
		}

		glowingExpired := false
		for _, expired := range tick.Expired {
			if p.Edition == player.ClientEditionJava {
				handler.RemoveMobEffect(target.Conn, p, expired.ID)
			} else if s.bedrockListener != nil {
				s.bedrockListener.RemovePlayerMobEffect(p, bedrockEffectType(expired.ID))
			}
			if expired.ID == "minecraft:glowing" {
				glowingExpired = true
			}
		}
		if glowingExpired && p.Edition == player.ClientEditionJava && s.sessions != nil {
			handler.BroadcastPlayerSharedFlags(p.EntityID, handler.PlayerSharedFlags(p), s.sessions)
		}
	})
}
