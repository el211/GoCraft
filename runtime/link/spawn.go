package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"time"

	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	wire "github.com/GoCraft-MC/gocraft-abi/abi/v1/wire"
	"github.com/GoCraft-MC/gocraft-abi/ipc"
)

// Spawn builds the command that starts a runtime for a given socket path.
//
// This is the whole of what ipc does not know. runtime/jvm returns a java
// invocation, a future runtime/python returns an interpreter — nothing in this
// package learns what either of them is. The caller also owns the command's
// Stdout and Stderr, because where a runtime's own logs go is its business.
type Spawn func(socket string) *exec.Cmd

// Config describes one runtime process.
type Config struct {
	// Runtime names the backend. It appears in the socket file name and in
	// errors, so keep it short: it spends part of the 107 byte path budget.
	Runtime string

	// Directory holds the socket. Short paths only — see SocketPath.
	Directory string

	// ABI the host speaks. A runtime announcing anything else is refused rather
	// than negotiated with: a runtime that guesses is worse than one that stops.
	ABI uint32

	TickRate    uint32
	EventBudget time.Duration

	// StartTimeout bounds the whole startup: the child connecting back, and its
	// HELLO arriving. A runtime that starts and then says nothing must not hold
	// the boot open, because the listeners wait on it.
	StartTimeout time.Duration

	Spawn Spawn

	// Handler receives envelopes that answer no request. It runs on the read
	// goroutine and must not block.
	Handler func(*wire.Envelope)

	// OnEmit dispatches a plugin-defined event the runtime published, and
	// answers what the subscribers did to it.
	//
	// It runs on its own goroutine, never on the read loop: dispatching reaches
	// subscribers in this very runtime, and every reply from them has to be
	// read off the socket the loop would be blocked on. ctx is the supervisor's
	// own, so an emission outlives the request that caused it only as long as
	// the process does.
	//
	// Nil means this host has nothing to dispatch into, which a plugin learns
	// as an error in EMITTED rather than as a dead runtime.
	OnEmit func(ctx context.Context, emission abi.Emission) abi.EmissionResult
}

const defaultStartTimeout = 30 * time.Second

// Child is one running runtime process and the connection to it.
type Child struct {
	conn    *Conn
	command *exec.Cmd
	socket  string

	// Version is what the runtime called itself in HELLO, for logs only.
	Version string

	exited   chan struct{}
	exitOnce sync.Once
	exitErr  error
}

// Conn returns the multiplexed connection to the runtime.
func (c *Child) Conn() *Conn { return c.conn }

// Exited is closed once the process has been reaped.
func (c *Child) Exited() <-chan struct{} { return c.exited }

// ExitError reports how the process ended, or nil while it is still running.
func (c *Child) ExitError() error {
	select {
	case <-c.exited:
		return c.exitErr
	default:
		return nil
	}
}

// Start spawns a runtime and completes the handshake, returning only once the
// runtime is ready to be sent work.
//
// The order matters: the socket is opened before the child is spawned, so the
// child never races a listener that does not exist yet.
func Start(ctx context.Context, config Config) (*Child, error) {
	if config.Spawn == nil {
		return nil, fmt.Errorf("ipc: %s: no spawn function", config.Runtime)
	}
	timeout := config.StartTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}

	socket, err := SocketPath(config.Directory, config.Runtime)
	if err != nil {
		return nil, err
	}
	listener, err := Listen(socket)
	if err != nil {
		return nil, err
	}
	// Accepting once is enough: one runtime, one connection. Closing the
	// listener also removes the socket file, so nothing else can connect to a
	// runtime that is already serving this host.
	defer listener.Close()

	command := config.Spawn(socket)
	if command == nil {
		return nil, fmt.Errorf("ipc: %s: spawn returned no command", config.Runtime)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("ipc: %s: start: %w", config.Runtime, err)
	}

	child, err := accept(ctx, config, listener, command, timeout)
	if err != nil {
		kill(command)
		return nil, err
	}
	child.socket = socket
	return child, nil
}

func accept(ctx context.Context, config Config, listener net.Listener, command *exec.Cmd, timeout time.Duration) (*Child, error) {
	deadline := time.Now().Add(timeout)
	if configured, ok := ctx.Deadline(); ok && configured.Before(deadline) {
		deadline = configured
	}
	if unix, ok := listener.(*net.UnixListener); ok {
		unix.SetDeadline(deadline)
	}
	stream, err := listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("ipc: %s did not connect back within %s: %w", config.Runtime, timeout, err)
	}

	// The same deadline covers HELLO. A runtime that connects and then says
	// nothing is as broken as one that never connects, and equally able to hold
	// the boot open forever.
	stream.SetDeadline(deadline)
	codec := ipc.NewCodec(stream)

	hello, err := codec.Receive()
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("ipc: %s sent no usable handshake: %w", config.Runtime, err)
	}
	greeting := hello.GetHello()
	if greeting == nil {
		stream.Close()
		return nil, fmt.Errorf("ipc: %s opened with %T instead of HELLO", config.Runtime, hello.GetBody())
	}
	if greeting.GetAbi() != config.ABI {
		stream.Close()
		return nil, fmt.Errorf("ipc: %s speaks ABI %d, host speaks %d", config.Runtime, greeting.GetAbi(), config.ABI)
	}

	welcome := &wire.Envelope{Seq: hello.GetSeq(), Body: &wire.Envelope_Welcome{Welcome: &wire.Welcome{
		Abi:           config.ABI,
		TickRate:      config.TickRate,
		EventBudgetMs: uint32(config.EventBudget / time.Millisecond),
	}}}
	if err := codec.Send(welcome); err != nil {
		stream.Close()
		return nil, fmt.Errorf("ipc: %s: send welcome: %w", config.Runtime, err)
	}

	// The handshake is over, so the deadline goes with it: from here the wait is
	// bounded per request by its own context, not by one deadline for the life
	// of the connection.
	stream.SetDeadline(time.Time{})

	child := &Child{
		conn:    NewConn(codec, config.Handler),
		command: command,
		Version: greeting.GetRuntime(),
		exited:  make(chan struct{}),
	}
	go child.reap()
	return child, nil
}

func (c *Child) reap() {
	err := c.command.Wait()
	c.exitOnce.Do(func() {
		c.exitErr = err
		close(c.exited)
	})
}

// Stop asks the runtime to leave, then makes sure it did.
//
// SHUTDOWN is a request, not a guarantee: a runtime stuck in a plugin's unload
// handler will never act on it. The kill below is what bounds shutdown, so the
// server does not hang on a plugin that misbehaves on its way out.
func (c *Child) Stop(ctx context.Context) error {
	sendErr := c.conn.Send(&wire.Envelope{Body: &wire.Envelope_Shutdown{Shutdown: &wire.Shutdown{}}})

	select {
	case <-c.exited:
	case <-ctx.Done():
		kill(c.command)
		<-c.exited
	}
	c.conn.Close()

	if exitErr := c.ExitError(); exitErr != nil {
		var status *exec.ExitError
		if errors.As(exitErr, &status) {
			// A runtime that was killed, or that exited non-zero after being
			// asked to leave, is not a failure worth propagating: it was going
			// away either way.
			return nil
		}
		return exitErr
	}
	return sendErr
}

func kill(command *exec.Cmd) {
	if command.Process != nil {
		command.Process.Kill()
	}
}
