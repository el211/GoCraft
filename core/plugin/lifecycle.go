package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"GoCraft/core/dispatch"
)

// Registry owns runtimes and loaded plugin instances for one server.
type Registry struct {
	mu          sync.RWMutex
	bus         *Bus
	commands    *dispatch.Registry
	host        Host
	provisioner Provisioner
	runtimes    map[string]Runtime
	instances   map[string]Instance
	loadOrder   []Instance
	started     []Runtime
	eventTypes  *EventTypes
}

func NewRegistry(ctx context.Context, budget time.Duration, host Host, provisioner Provisioner) *Registry {
	if host == nil {
		host = NewMutationQueue()
	}
	return &Registry{
		bus: newBus(ctx, budget, host), commands: dispatch.NewRegistry(), host: host, provisioner: provisioner,
		runtimes: make(map[string]Runtime), instances: make(map[string]Instance),
	}
}

func (r *Registry) Bus() *Bus { return r.bus }

func (r *Registry) Commands() *dispatch.Registry { return r.commands }

func (r *Registry) RegisterRuntime(runtime Runtime) error {
	if runtime == nil {
		return fmt.Errorf("plugin: nil runtime")
	}
	name := strings.TrimSpace(runtime.Name())
	if name == "" {
		return fmt.Errorf("plugin: runtime has no name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runtimes[name]; exists {
		return fmt.Errorf("plugin: runtime %s is already registered", name)
	}
	r.runtimes[name] = runtime
	return nil
}

func (r *Registry) Runtime(name string) (Runtime, bool) {
	r.mu.RLock()
	runtime, ok := r.runtimes[name]
	r.mu.RUnlock()
	return runtime, ok
}

func (r *Registry) Instance(pluginID string) (Instance, bool) {
	r.mu.RLock()
	instance, ok := r.instances[pluginID]
	r.mu.RUnlock()
	return instance, ok
}
