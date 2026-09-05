package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type namedRuntime struct {
	name    string
	runtime Runtime
}

// Preflight establishes what has to be true across every scanned bundle before
// any of them loads, then provisions only the runtimes those manifests require.
//
// The cheap checks run first and synchronously. Provisioning may download a
// JDK; a manifest that cannot be reconciled with the others should cost none of
// that, and an error arriving before the download starts is also the one an
// admin can read without scrolling.
func (r *Registry) Preflight(ctx context.Context, bundles []Bundle) error {
	eventTypes, err := newEventTypes(bundles)
	if err != nil {
		return err
	}
	if err := validateSubscriptions(bundles, eventTypes); err != nil {
		return err
	}
	r.mu.Lock()
	r.eventTypes = eventTypes
	r.mu.Unlock()
	runtimes, err := r.neededRuntimes(bundles)
	if err != nil {
		return err
	}
	errs := make([]error, len(runtimes))
	var wait sync.WaitGroup
	wait.Add(len(runtimes))
	for index, required := range runtimes {
		index, required := index, required
		go func() {
			defer wait.Done()
			if err := required.runtime.Provision(ctx, r.provisioner); err != nil {
				errs[index] = fmt.Errorf("provision runtime %s: %w", required.name, err)
			}
		}()
	}
	wait.Wait()
	return errors.Join(errs...)
}

func (r *Registry) neededRuntimes(bundles []Bundle) ([]namedRuntime, error) {
	names := make(map[string]struct{})
	for _, bundle := range bundles {
		names[bundle.Manifest.Runtime] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	r.mu.RLock()
	defer r.mu.RUnlock()
	runtimes := make([]namedRuntime, 0, len(ordered))
	for _, name := range ordered {
		runtime, ok := r.runtimes[name]
		if !ok {
			return nil, fmt.Errorf("plugin: runtime %s is not available in this build", name)
		}
		runtimes = append(runtimes, namedRuntime{name: name, runtime: runtime})
	}
	return runtimes, nil
}
