package goplugin

import (
	"context"
	"errors"
	"fmt"

	"GoCraft/core/plugin"
	"GoCraft/runtime/link"
)

func (r *Runtime) Load(ctx context.Context, bundle plugin.Bundle) (plugin.Instance, error) {
	if !r.isStarted() {
		return nil, fmt.Errorf("go runtime: not started")
	}
	executable, cleanup, err := r.extract(bundle)
	if err != nil {
		return nil, err
	}
	supervisor := r.newSupervisor(bundle.Manifest.ID, executable)
	if err := supervisor.Start(ctx); err != nil {
		cleanup()
		return nil, err
	}
	events := make([]string, 0, len(bundle.Manifest.Subscriptions))
	for _, subscription := range bundle.Manifest.Subscriptions {
		events = append(events, subscription.Event)
	}
	_, err = supervisor.Load(ctx, link.LoadRequest{
		ID:            bundle.Manifest.ID,
		BundlePath:    bundle.Path,
		Entry:         bundle.Manifest.Entry,
		DataDirectory: bundle.DataDirectory,
		CommandTree:   bundle.Manifest.CommandTree,
		Events:        events,
		EventTypes:    bundle.EventTypes,
	})
	if err != nil {
		stopErr := supervisor.Stop(ctx)
		cleanup()
		return nil, errors.Join(err, stopErr)
	}
	instance := &Instance{
		runtime: r, manifest: bundle.Manifest,
		supervisor: supervisor, cleanup: cleanup,
	}
	if err := r.add(instance); err != nil {
		stopErr := supervisor.Stop(ctx)
		cleanup()
		return nil, errors.Join(err, stopErr)
	}
	return instance, nil
}

func (r *Runtime) isStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}
