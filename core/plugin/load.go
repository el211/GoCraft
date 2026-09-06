package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// LoadAll starts required runtimes and loads validated bundles deterministically.
func (r *Registry) LoadAll(ctx context.Context, bundles []Bundle) error {
	ordered := append([]Bundle(nil), bundles...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Manifest.ID < ordered[j].Manifest.ID
	})
	seen := make(map[string]struct{}, len(ordered))
	for _, bundle := range ordered {
		if err := gcpkg.ValidateManifest(bundle.Manifest); err != nil {
			return err
		}
		if _, duplicate := seen[bundle.Manifest.ID]; duplicate {
			return fmt.Errorf("plugin: duplicate bundle id %s", bundle.Manifest.ID)
		}
		seen[bundle.Manifest.ID] = struct{}{}
	}
	if err := r.registerBundleCommands(ordered); err != nil {
		return err
	}
	if err := r.startRuntimes(ctx, ordered); err != nil {
		r.revokeBundleCommands(ordered)
		return err
	}
	rollback := func(cause error, discard Instance) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unloadErr error
		if discard != nil {
			unloadErr = discard.Unload(stopCtx)
		}
		r.revokeBundleCommands(ordered)
		return errors.Join(cause, unloadErr, r.Stop(stopCtx))
	}
	for _, bundle := range ordered {
		if err := ctx.Err(); err != nil {
			return rollback(err, nil)
		}
		prepared, err := prepareBundleData(bundle)
		if err != nil {
			return rollback(err, nil)
		}
		bundle = prepared
		// The ids come from preflight, which saw every manifest. A caller that
		// skipped it gets an empty table and a plugin that can emit nothing,
		// rather than ids assigned from whatever this one bundle happens to
		// declare — which would differ from the ones its subscribers hold.
		bundle.EventTypes = r.EventTypes().bindingsFor(bundle.Manifest)
		runtime, _ := r.Runtime(bundle.Manifest.Runtime)
		instance, err := runtime.Load(ctx, bundle)
		if err != nil {
			return rollback(fmt.Errorf("load plugin %s: %w", bundle.Manifest.ID, err), nil)
		}
		actual := instance.Manifest()
		if actual.ID != bundle.Manifest.ID {
			mismatch := fmt.Errorf("plugin %s loaded as %q", bundle.Manifest.ID, actual.ID)
			return rollback(mismatch, instance)
		}
		if bundle.Commands != nil {
			if _, ok := instance.(CommandInstance); !ok {
				unsupported := fmt.Errorf("plugin %s runtime does not support commands", actual.ID)
				return rollback(unsupported, instance)
			}
		}
		if err := r.bus.Attach(instance); err != nil {
			return rollback(err, instance)
		}
		r.mu.Lock()
		r.instances[actual.ID] = instance
		r.loadOrder = append(r.loadOrder, instance)
		r.mu.Unlock()
	}
	r.warmInstances(ctx, ordered)
	if err := r.readyRuntimes(ctx, ordered); err != nil {
		return rollback(err, nil)
	}
	return nil
}

// warmInstances runs each subscription's dispatch path before any of them can
// fire for real.
//
// After every plugin is loaded rather than after each one, so a runtime hosting
// several warms with all of them present — and before READY, which is the last
// moment the host is still waiting on the runtimes without a budget.
//
// Nothing here can fail the boot. A runtime that will not warm is a runtime
// whose first event is slow, which is what every runtime was before this
// existed; refusing to start a server over it would be trading a warning for an
// outage.
func (r *Registry) warmInstances(ctx context.Context, bundles []Bundle) {
	for _, bundle := range bundles {
		r.mu.RLock()
		instance := r.instances[bundle.Manifest.ID]
		r.mu.RUnlock()
		warmer, ok := instance.(Warmer)
		if !ok {
			continue
		}
		for _, subscription := range bundle.Manifest.Subscriptions {
			if err := ctx.Err(); err != nil {
				return
			}
			if err := warmer.Warm(ctx, subscription.Event); err != nil {
				slog.Warn("plugin event warm-up failed", "plugin", bundle.Manifest.ID,
					"event", subscription.Event, "err", err)
			}
		}
	}
}
