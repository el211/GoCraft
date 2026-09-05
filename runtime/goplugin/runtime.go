// Package goplugin runs compiled GoCraft plugins in isolated child processes.
package goplugin

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"GoCraft/core/plugin"
	"GoCraft/runtime/link"
	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
)

const (
	RuntimeName = "go"
	abiVersion  = 1
)

type Config struct {
	ExtractDirectory string
	SocketDirectory  string
	TickRate         uint32
	EventBudget      time.Duration
	StartTimeout     time.Duration
	Liveness         link.Liveness
	Stdout           io.Writer
	Stderr           io.Writer
	// OnEmit dispatches a plugin-defined event this runtime's plugin published.
	//
	// The host supplies it rather than this package building one, because
	// dispatching means reaching the registry and runtime/link must never see
	// core/plugin. The closure is built where both are already in scope: the
	// server.
	OnEmit func(ctx context.Context, emission abi.Emission) abi.EmissionResult
	// Spawn replaces process creation in tests.
	Spawn func(executable string) link.Spawn
}

type Runtime struct {
	config Config

	mu        sync.Mutex
	started   bool
	instances map[string]*Instance
}

func New(config Config) *Runtime {
	return &Runtime{config: config, instances: make(map[string]*Instance)}
}

func (*Runtime) Name() string { return RuntimeName }

// Provision is intentionally empty: a native plugin carries its own runtime.
func (*Runtime) Provision(context.Context, plugin.Provisioner) error { return nil }

func (r *Runtime) Start(context.Context, plugin.Host) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("go runtime: already started")
	}
	r.started = true
	return nil
}

func (r *Runtime) add(instance *Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return fmt.Errorf("go runtime: not started")
	}
	if _, exists := r.instances[instance.manifest.ID]; exists {
		return fmt.Errorf("go runtime: plugin %s is already loaded", instance.manifest.ID)
	}
	r.instances[instance.manifest.ID] = instance
	return nil
}

func (r *Runtime) remove(id string) {
	r.mu.Lock()
	delete(r.instances, id)
	r.mu.Unlock()
}
