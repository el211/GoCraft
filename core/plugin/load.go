package plugin

import (
	"context"
	"errors"
	"fmt"
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
	if err := r.readyRuntimes(ctx, ordered); err != nil {
		return rollback(err, nil)
	}
	return nil
}
