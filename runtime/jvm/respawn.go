package jvm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"GoCraft/runtime/link"
)

// Respawn tunes what happens when the JVM dies while the server is running.
//
// It dies for two reasons worth surviving: it crashed, or it stopped answering
// pings and was killed for it. Either way the server is still up with players
// on it, and leaving every Java plugin dead until the next restart is a worse
// outcome than bringing them back.
type Respawn struct {
	// Attempts bounds consecutive respawns before the runtime gives up and
	// stays down. Zero takes the default of 3.
	//
	// A bound rather than a retry loop: a JVM that dies on startup dies on
	// every startup, and a server spinning up a doomed process forever is
	// harder to diagnose than one that says it stopped trying.
	Attempts int

	// Backoff is the wait before the first attempt, doubled each time. Zero
	// takes one second.
	Backoff time.Duration

	// StableAfter is how long a respawned runtime has to survive before it
	// counts as recovered. Zero takes one minute.
	//
	// Without it the attempt counter is useless: a JVM that starts, greets and
	// dies a moment later makes every respawn look successful, so the count
	// resets each round and the server flaps forever. What matters is not
	// whether the process started but whether it stayed.
	StableAfter time.Duration

	// Disabled leaves a dead runtime dead. For an admin who would rather see
	// the plugins stop than have them restart with empty state.
	Disabled bool
}

func (r Respawn) resolve() (attempts int, backoff, stable time.Duration) {
	attempts, backoff, stable = r.Attempts, r.Backoff, r.StableAfter
	if attempts <= 0 {
		attempts = 3
	}
	if backoff <= 0 {
		backoff = time.Second
	}
	if stable <= 0 {
		stable = time.Minute
	}
	return attempts, backoff, stable
}

// watch brings the runtime back when it dies on its own.
//
// It never provisions. If the JVM dies and the download cache was purged in the
// meantime, fetching 45 MB while players are connected is not an improvement
// over leaving the runtime down until the next boot — §17 is explicit, and this
// is the one place that rule is enforceable.
func (r *Runtime) watch() {
	attempts, backoff, stable := r.config.Respawn.resolve()
	// Failures are counted within a flapping window, not over the life of the
	// server. A runtime that ran for a day and then crashed once is not two
	// thirds of the way to being given up on.
	failure := 0
	startedAt := time.Now()
	for {
		supervisor, err := r.running()
		if err != nil {
			return
		}
		select {
		case <-supervisor.Failed():
		case <-r.done:
			return
		}
		if r.deliberate() {
			return
		}

		cause := supervisor.Err()
		if r.config.Respawn.Disabled {
			slog.Error("jvm: the runtime died and respawn is disabled",
				"err", cause, "plugins", len(r.remembered()))
			r.clearSupervisor()
			return
		}

		// Surviving long enough is what counts as recovered — not the fact that
		// a process started. A JVM that greets and dies a moment later would
		// otherwise reset this every round and flap forever.
		if lived := time.Since(startedAt); lived >= stable {
			failure = 0
		}
		failure++
		if failure > attempts {
			slog.Error("jvm: giving up on the runtime",
				"attempts", attempts, "err", cause,
				"hint", "a JVM that dies on startup dies on every startup; the server "+
					"keeps running without its Java plugins until the next restart")
			r.clearSupervisor()
			return
		}

		wait := backoff << (failure - 1)
		slog.Warn("jvm: the runtime died, bringing it back",
			"err", cause, "attempt", failure, "of", attempts, "in", wait)
		if !r.sleep(wait) {
			return
		}
		if err := r.respawn(); err != nil {
			if r.deliberate() {
				return
			}
			slog.Error("jvm: respawn failed", "attempt", failure, "err", err)
			continue
		}
		startedAt = time.Now()
	}
}

// respawn starts a new JVM and reloads everything that was in the old one.
//
// The plugins come back with empty memory. That is documented at the top of the
// plugin API and bears repeating here, because this is the code that makes it
// true: anything a plugin kept in a field is gone, and a plugin that stored
// player data in a HashMap has just lost it.
func (r *Runtime) respawn() error {
	if r.deliberate() {
		return fmt.Errorf("jvm: runtime is stopping")
	}
	java := r.Java()
	if java == "" {
		return fmt.Errorf("jvm: no JDK; respawn does not provision")
	}
	jar, err := r.ensureJar()
	if err != nil {
		return err
	}

	// Its own context. Boot's is long gone by the time a runtime dies mid-game.
	ctx, cancel := context.WithTimeout(context.Background(), r.config.StartTimeout)
	defer cancel()

	supervisor := link.NewSupervisor(r.linkConfig(java, jar), r.config.Liveness)

	if err := supervisor.Start(ctx); err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = supervisor.Stop(ctx)
		}
	}()

	// In the order they were loaded the first time, because load order comes
	// from the dependency graph and a plugin may rely on an earlier one.
	restored := make([]string, 0, len(r.remembered()))
	for _, bundle := range r.remembered() {
		if _, err := supervisor.Load(ctx, link.LoadRequest{
			ID: bundle.id, BundlePath: bundle.path,
			Entry: bundle.entry, DataDirectory: bundle.data,
			CommandTree: bundle.commandTree, Events: bundle.events,
			EventTypes: bundle.eventTypes,
		}); err != nil {
			// One plugin refusing to come back must not keep the others down.
			// It was running a moment ago, so this is worth reporting loudly
			// and carrying on.
			slog.Error("jvm: plugin did not survive the respawn",
				"plugin", bundle.id, "err", err)
			continue
		}
		restored = append(restored, bundle.id)
	}
	if err := supervisor.Ready(); err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return fmt.Errorf("jvm: runtime stopped during respawn")
	}
	r.supervisor = supervisor
	installed = true
	r.mu.Unlock()

	slog.Info("jvm: runtime back up", "plugins", len(restored))
	if r.config.OnRespawn != nil {
		// The host replays what the plugins missed — §13's synthetic joins for
		// everyone already online. Only the host knows who that is.
		r.config.OnRespawn(restored)
	}
	return nil
}

func (r *Runtime) remembered() []loadedBundle {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]loadedBundle(nil), r.loaded...)
}

func (r *Runtime) deliberate() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stopping
}

func (r *Runtime) clearSupervisor() {
	r.mu.Lock()
	r.supervisor = nil
	r.mu.Unlock()
}

func (r *Runtime) sleep(wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.done:
		// Shutdown must not wait out a backoff that has become pointless.
		return false
	}
}
