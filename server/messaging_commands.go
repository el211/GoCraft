package server

import (
	"fmt"
	"strings"

	"GoCraft/core/player"
	"GoCraft/java/handler"
)

func (s *Server) registerMessagingCommands() {
	s.cmds.Register("me", func(ctx handler.CommandContext) error {
		if ctx.Player == nil || len(ctx.Args) == 0 {
			return fmt.Errorf("usage: /me <action>")
		}
		s.broadcastMessage("* " + ctx.Player.Username + " " + strings.Join(ctx.Args, " "))
		return nil
	})
	s.cmds.RegisterOperator("say", func(ctx handler.CommandContext) error {
		if len(ctx.Args) == 0 {
			return fmt.Errorf("usage: /say <message>")
		}
		s.broadcastMessage("[Server] " + strings.Join(ctx.Args, " "))
		return nil
	})
	for _, name := range []string{"msg", "tell", "w"} {
		s.cmds.Register(name, s.commandPrivateMessage)
	}
}

func (s *Server) commandPrivateMessage(ctx handler.CommandContext) error {
	if ctx.Player == nil || len(ctx.Args) < 2 {
		return fmt.Errorf("usage: /msg <player> <message>")
	}
	target := s.findOnlinePlayer(ctx.Args[0])
	if target == nil {
		return fmt.Errorf("player not found: %s", ctx.Args[0])
	}
	message := strings.Join(ctx.Args[1:], " ")
	if err := s.sendPlayerMessage(target, "["+ctx.Player.Username+" -> you] "+message); err != nil {
		return err
	}
	return commandReply(ctx, "[you -> "+target.Username+"] "+message)
}

func (s *Server) findOnlinePlayer(name string) *player.Player {
	var found *player.Player
	if s.game != nil {
		s.game.OnlinePlayers(func(candidate *player.Player) {
			if found == nil && strings.EqualFold(candidate.Username, name) {
				found = candidate
			}
		})
	}
	return found
}

// sendPlayerMessage delivers text to one online player on either edition. It
// is installed on the dispatcher as the command-feedback bridge, so it is also
// how every built-in command answers.
//
// A nil target is the console, which has no session to write to and is not an
// error: the caller has already logged whatever it was going to say.
func (s *Server) sendPlayerMessage(target *player.Player, message string) error {
	if target == nil {
		return nil
	}
	if target.Edition == player.ClientEditionJava {
		if current, ok := s.sessions.Get(target.UUID); ok {
			return handler.SendSystemMessage(current.Conn, message)
		}
	} else if s.bedrockListener != nil {
		s.bedrockListener.SendMessage(target.UUID, message)
		return nil
	}
	return fmt.Errorf("player session is unavailable")
}

// sendPlayerLink sends a clickable component to a Java player and the plain URL
// to a Bedrock one.
//
// Degrading rather than dropping it is the point: the Bedrock client cannot
// render the component, and sending nothing is how the permission editor link
// became invisible to half the operators on the server.
func (s *Server) sendPlayerLink(target *player.Player, message, link string) error {
	if target == nil {
		return nil
	}
	if target.Edition == player.ClientEditionJava {
		if current, ok := s.sessions.Get(target.UUID); ok {
			return handler.SendLinkMessage(current.Conn, message, link)
		}
	}
	return s.sendPlayerMessage(target, message+" "+link)
}

// syncPlayerAbilities republishes a player's game mode, flight and speeds after
// a command changed them, through whichever adapter owns that player.
func (s *Server) syncPlayerAbilities(target *player.Player) {
	if target == nil {
		return
	}
	if target.Edition == player.ClientEditionJava {
		if current, ok := s.sessions.Get(target.UUID); ok {
			_ = handler.SyncPlayerState(current.Conn, target)
		}
		return
	}
	if s.bedrockListener != nil {
		s.bedrockListener.RefreshPlayerAbilities(target)
	}
}

// syncPlayerStatusEffect publishes command-applied effects through both
// protocol adapters while canonical state remains on the shared player.
func (s *Server) syncPlayerStatusEffect(target *player.Player, effect player.StatusEffect) {
	if target == nil {
		return
	}
	if s.sessions != nil {
		for _, viewer := range s.sessions.SnapshotAll() {
			handler.SendMobEffect(viewer.Conn, target, effect.ID, effect.Amplifier, effect.Duration)
		}
		if (effect.ID == "minecraft:glowing" || effect.ID == "minecraft:invisibility") && target.Edition == player.ClientEditionJava {
			handler.BroadcastPlayerSharedFlags(target.EntityID, handler.PlayerSharedFlags(target), s.sessions)
		}
	}
	if target.Edition == player.ClientEditionBedrock && s.bedrockListener != nil {
		effectType := bedrockEffectType(effect.ID)
		if effectType != 0 {
			s.bedrockListener.SendPlayerMobEffect(target, effectType, effect.Amplifier, effect.Duration)
		}
	}
}

func (s *Server) broadcastMessage(message string) {
	handler.BroadcastSystemMessage(s.sessions, message)
}
