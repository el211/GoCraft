package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"GoCraft/config"
	"GoCraft/core/player"
	coreplugin "GoCraft/core/plugin"
	"GoCraft/runtime/goplugin"
	"GoCraft/runtime/jvm"
	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
)

// pluginShutdownTimeout bounds how long unloading may take in total, matching
// the window LoadAll gives its own rollback path.
const pluginShutdownTimeout = 5 * time.Second

// pluginTickRate is the simulation rate published to every runtime in WELCOME,
// matching the 50ms ticker in Run. A runtime cannot enforce anything with it;
// it is what lets a plugin convert ticks to seconds without guessing.
const pluginTickRate = 20

// registerPluginRuntimes makes the language backends available to the registry.
//
// Registering one costs nothing. It is constructed, not started: no process is
// spawned, no JDK is looked for and nothing is downloaded until Preflight finds
// a scanned manifest that asks for it by name. That is what keeps "a server
// with no Java plugin never touches Java" true rather than aspirational, and it
// is why this runs unconditionally rather than behind a config switch.
func (s *Server) registerPluginRuntimes(cfg *config.Config) error {
	java := cfg.Plugins.Runtimes.JVM
	if err := s.pluginRegistry.RegisterRuntime(jvm.New(jvm.Config{
		JavaPath:     java.JavaPath,
		PreferSystem: java.PreferSystem,
		JarPath:      java.JarPath,
		TickRate:     pluginTickRate,
		EventBudget:  time.Duration(cfg.Plugins.EventBudgetMillis) * time.Millisecond,
		OnRespawn:    s.replayJoins,
		OnEmit:       s.pluginRegistry.EmitFrom,
	})); err != nil {
		return err
	}
	return s.pluginRegistry.RegisterRuntime(goplugin.New(goplugin.Config{
		TickRate:    pluginTickRate,
		EventBudget: time.Duration(cfg.Plugins.EventBudgetMillis) * time.Millisecond,
		OnEmit:      s.pluginRegistry.EmitFrom,
	}))
}

// replayJoins tells plugins that just came back who is already here.
//
// A runtime that died and was restarted has plugins with empty memory and no
// idea anyone is online: they never saw those players connect. §13 calls the
// fix synthetic player.join events, and this is it — the host makes up what
// they missed, because it is the only thing that knows.
//
// Only the restored plugins receive them. A Lua plugin, or a Java one in
// another runtime, never went away and saw the real joins; sending them again
// would have it count arrivals that never happened.
//
// It runs on the runtime's respawn goroutine rather than the tick. player.join
// is observational, so nothing waits on it and there is no tick to hold.
func (s *Server) replayJoins(restored []string) {
	if len(restored) == 0 || s.game == nil || s.plugins == nil {
		return
	}
	replayed := 0
	s.game.OnlinePlayers(func(online *player.Player) {
		s.plugins.EmitPlayerJoinTo(restored, online)
		replayed++
	})
	slog.Info("plugins: replayed joins to a restarted runtime",
		"plugins", len(restored), "players", replayed)
}

// loadPlugins scans the bundle directory, provisions every runtime the scanned
// manifests require, and loads the plugins. It runs before any listener opens:
// a server that accepts players while a spawn-protection plugin is still
// loading is worse than a server that takes longer to boot.
//
// A missing or empty directory is not an error. A failing bundle is: LoadAll
// already rolls back the plugins it started, so refusing to boot leaves the
// admin with a clear failure instead of a silently incomplete server.
func (s *Server) loadPlugins(ctx context.Context) error {
	if !s.cfg.Plugins.Enabled {
		return nil
	}
	bundles, err := coreplugin.ScanBundles(s.cfg.Plugins.Directory)
	if err != nil {
		return fmt.Errorf("scan plugins: %w", err)
	}
	if len(bundles) == 0 {
		slog.Info("plugins: none found", "directory", s.cfg.Plugins.Directory)
		return nil
	}
	if err := s.pluginRegistry.Preflight(ctx, bundles); err != nil {
		return err
	}
	if err := s.pluginRegistry.LoadAll(ctx, bundles); err != nil {
		return err
	}
	slog.Info("plugins: loaded", "count", len(bundles), "directory", s.cfg.Plugins.Directory)
	return nil
}

// unloadPlugins stops every plugin and runtime in reverse load order. It builds
// its own context: Run's is already cancelled by the time shutdown reaches this
// point, and a runtime still needs a bounded window to flush and exit.
//
// Failures are logged rather than returned. The server is going down either
// way, and the world flush that follows matters more than a runtime that
// refused to exit cleanly.
func (s *Server) unloadPlugins() {
	if s.pluginRegistry == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginShutdownTimeout)
	defer cancel()
	if err := s.pluginRegistry.Stop(ctx); err != nil {
		slog.Warn("plugins: unclean shutdown", "err", err)
	}
}

// pluginEffectMessage is the only host call a plugin can make today: send text
// to the player an event was about.
// The string lives in core/plugin, because the command path has to agree
// with the queue on what a reply is called.
const pluginEffectMessage = coreplugin.EffectMessage

// applyPluginEffects performs what plugins asked for, on the tick.
//
// Writes never originate from a plugin. A handler runs on another thread — in
// another process, for the JVM — and touching world state from there would race
// the simulation reading it. So a verdict carries requests, the queue holds
// them, and this applies them where every other mutation happens.
//
// It runs at the top of the tick, before intents, so an effect produced by an
// event during the previous tick lands before the world moves again.
func (s *Server) applyPluginEffects() {
	if s.pluginEffects == nil {
		return
	}
	// The callback never returns an error, so Drain always empties the queue.
	// Stopping at the first failure would let one plugin's malformed effect
	// hold back every effect queued behind it — the same reason a failing
	// subscriber does not cancel the others.
	_, _ = s.pluginEffects.Drain(s.applyPluginEffect)
}

// applyPluginEffect performs one host call.
//
// An unknown type is logged, not fatal. A plugin built against a newer ABI may
// ask for something this server has never heard of, and refusing to boot over
// it would be worse than ignoring one message.
func (s *Server) applyPluginEffect(call abi.HostCall) error {
	switch call.Type {
	case pluginEffectMessage:
		if err := s.applyPluginMessage(call); err != nil {
			slog.Warn("plugins: effect not applied", "type", call.Type, "err", err)
		}
	default:
		slog.Warn("plugins: unknown effect", "type", call.Type)
	}
	// Never fatal: see applyPluginEffects.
	return nil
}

// applyPluginMessage delivers one message, to whichever edition the target is
// on.
//
// The recipient travels with the effect as the same PlayerRef the event
// carried, so the host reads back exactly what it wrote and the two cannot
// disagree about who acted.
func (s *Server) applyPluginMessage(call abi.HostCall) error {
	if len(call.Fields) != 2 {
		return fmt.Errorf("plugins: %s carries %d fields, want a player and a message",
			call.Type, len(call.Fields))
	}
	uuid, ok := coreplugin.PlayerUUIDFrom(call.Fields[0])
	if !ok {
		// An event with no acting player — a block broken by a piston. Nobody
		// to tell, and not a failure.
		return nil
	}
	message, ok := coreplugin.TextFrom(call.Fields[1])
	if !ok {
		return fmt.Errorf("plugins: %s carries no message", call.Type)
	}
	target := s.game.GetPlayer(uuid)
	if target == nil {
		// They left between the event and the tick that applies it. Common
		// enough on a join message, and nothing to report.
		return nil
	}
	return s.sendPlayerMessage(target, message)
}

// maximumJoinAnnounceTicks bounds how long a join waits for its player to
// become reachable — five seconds at 20 TPS.
//
// A player who never becomes reachable disconnected during login, which happens
// often enough. Past this the join is dropped: announcing an arrival to plugins
// after the player has gone would be worse than not announcing it.
const maximumJoinAnnounceTicks = 100

// announceJoinWhenReachable queues a join for the tick to announce.
//
// It is not emitted where the player is created, which is the obvious place and
// the wrong one: at that moment neither adapter has a session for them yet, so
// a plugin greeting the arrival would have its message dropped for want of
// anyone to send it to. That is not a delivery bug — the event fired before
// the player existed as far as the network is concerned.
//
// Waiting for reachability also keeps this edition-agnostic. The condition is
// the same one delivery tests, so the two cannot disagree, and a future adapter
// needs no new emission point.
func (s *Server) announceJoinWhenReachable(p *player.Player) {
	if p == nil || s.plugins == nil {
		return
	}
	s.pendingJoinsMu.Lock()
	defer s.pendingJoinsMu.Unlock()
	if s.pendingJoins == nil {
		s.pendingJoins = make(map[[16]byte]int)
	}
	s.pendingJoins[p.UUID] = 0
}

// announceReachableJoins emits player.join for everyone who has become
// reachable since the last tick.
func (s *Server) announceReachableJoins() {
	s.pendingJoinsMu.Lock()
	if len(s.pendingJoins) == 0 {
		s.pendingJoinsMu.Unlock()
		return
	}
	var ready []*player.Player
	for uuid, waited := range s.pendingJoins {
		target := s.game.GetPlayer(uuid)
		if target == nil {
			// Gone before they arrived.
			delete(s.pendingJoins, uuid)
			continue
		}
		if s.playerReachable(target) {
			delete(s.pendingJoins, uuid)
			ready = append(ready, target)
			continue
		}
		if waited >= maximumJoinAnnounceTicks {
			delete(s.pendingJoins, uuid)
			slog.Warn("plugins: join not announced, player never became reachable",
				"player", target.Username)
			continue
		}
		s.pendingJoins[uuid] = waited + 1
	}
	s.pendingJoinsMu.Unlock()

	// Emitted outside the lock: the bus hands the event to every subscriber,
	// and holding a server lock while plugin code runs is how a slow handler
	// becomes a stalled join queue.
	for _, target := range ready {
		s.plugins.EmitPlayerJoin(target)
	}
}

// playerReachable reports whether something written to this player would
// actually be sent. It is the condition sendPlayerMessage tests, named so the
// two cannot drift.
func (s *Server) playerReachable(target *player.Player) bool {
	if target == nil {
		return false
	}
	if target.Edition == player.ClientEditionJava {
		_, ok := s.sessions.Get(target.UUID)
		return ok
	}
	return s.bedrockListener != nil && s.bedrockListener.HasSession(target.UUID)
}
