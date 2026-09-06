package plugin

import (
	"context"

	"GoCraft/core/dispatch"
	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/command"

	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// Host is the only mutation path exposed to a plugin runtime.
type Host interface {
	Enqueue(call abi.HostCall) error
}

// Artifact describes a pinned runtime or library download.
type Artifact struct {
	URL    string
	SHA256 string
	Size   int64
	Strip  int
	Bin    string
}

// Provisioner supplies content-addressed artifacts during boot preflight.
type Provisioner interface {
	Cached(key string) (path string, ok bool)
	Fetch(ctx context.Context, key string, artifact Artifact) (string, error)
}

// Runtime hosts all plugins using one language backend.
type Runtime interface {
	Name() string
	Provision(ctx context.Context, provisioner Provisioner) error
	Start(ctx context.Context, host Host) error
	Load(ctx context.Context, bundle Bundle) (Instance, error)
	Stop(ctx context.Context) error
}

// Instance is one loaded plugin, regardless of its runtime.
type Instance interface {
	Manifest() gcpkg.Manifest
	Dispatch(ctx context.Context, event *abi.Event) (abi.Verdict, error)
	Unload(ctx context.Context) error
}

// Warmer is an Instance whose runtime pays a cold-start cost worth removing
// before the first real event reaches it.
//
// Optional, because it is not a universal property. A Go plugin is a compiled
// binary with nothing to load, link or interpret; a JVM measures 7.9 ms on the
// first event of a type against a shared budget of 2 ms, from class loading,
// protobuf coders built on first use and bytecode nobody has compiled yet.
//
// The registry calls this once per declared subscription, after the plugin is
// loaded and before the runtimes are told READY — a window the host already
// waits through without a budget. What the runtime does with it is its own
// business, except for the one thing it must not do: run a handler.
type Warmer interface {
	Warm(ctx context.Context, event string) error
}

// CommandInstance is implemented by runtimes that loaded a command tree.
//
// It answers with a result rather than an error for the same reason Dispatch
// answers with a verdict: what a handler asked the world to do is queued here,
// not by the runtime, so a runtime stays a forward and every effect in the
// system passes through one queue.
type CommandInstance interface {
	InvokeCommand(context.Context, command.ExecID, dispatch.Sender, dispatch.Values) (abi.CommandResult, error)
}

// ReadyRuntime is implemented by runtimes that have to be told the load phase
// is over.
//
// An out-of-process runtime cannot work it out for itself: it is sent plugins
// one at a time and never learns which one was last, so without this it would
// either go live too early or wait forever. An in-process runtime has no phase
// to end and does not implement it — which is why this is a separate interface
// rather than a method on Runtime that half the backends would leave empty.
//
// It runs after every plugin is up and before any listener opens.
type ReadyRuntime interface {
	Ready(ctx context.Context) error
}
