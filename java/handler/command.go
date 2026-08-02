package handler

// Command dispatcher for Milestone 12.
//
// Dispatcher maps slash-command names to CommandFunc handlers and executes
// them with a CommandContext that bundles every resource a handler might need.
// It is created once at server start, has commands registered via Register,
// and is passed through HandlePlay → playLoop → handleChatPacket so any
// C→S packet that looks like a command reaches it.

import (
	"fmt"
	"strings"
	"sync"

	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/session"
)

// CommandContext carries every resource a command handler might need.
// The Conn field is the issuing player's connection; use it for per-player
// feedback.  Manager gives access to all online sessions for commands that
// affect other players (e.g. /kick, /tp <player>).
type CommandContext struct {
	Player  *player.Player
	Conn    *network.ClientConn
	Args    []string // tokens after the command name, split on whitespace
	World   *coreworld.World
	Manager *session.Manager

	// TeleportTo moves the player to (x, y, z), sends Synchronize Player
	// Position, updates the center-chunk anchor, and streams the destination
	// chunks — all before returning.  Commands that reposition the player
	// (e.g. /tp) must call this instead of mutating Player.Position directly
	// so the client's chunk view is kept in sync.
	TeleportTo func(x, y, z float64) error

	// NextEntityID allocates an ID shared with players and naturally spawned
	// mobs. It is supplied by the dispatcher for commands such as /summon.
	NextEntityID func() int32

	// FindPlayer resolves an online player across both Java and Bedrock
	// adapters. Commands that only need canonical player state should prefer it
	// over Manager, which contains Java network sessions only.
	FindPlayer func(name string) *player.Player

	// Reply sends command feedback to the issuing edition. SyncAbilities asks
	// that edition adapter to publish changed flight/permission state.
	Reply         func(text string) error
	SyncAbilities func(*player.Player)
}

// CommandFunc is the handler signature for a built-in server command.
// A non-nil return value is formatted and sent to the issuing player as a
// system message; it is NOT logged as a server error.
type CommandFunc func(ctx CommandContext) error

type registeredCommand struct {
	fn           CommandFunc
	operatorOnly bool
}

// Dispatcher maps command names (lower-case) to their implementations.
// All methods are safe for concurrent use.
type Dispatcher struct {
	mu           sync.RWMutex
	cmds         map[string]registeredCommand
	nextEntityID func() int32
	findPlayer   func(string) *player.Player
}

// NewDispatcher returns an empty, ready-to-use Dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{cmds: make(map[string]registeredCommand)}
}

// Register adds fn under the given name.  name is lowercased before storage
// and matched case-insensitively at dispatch time.
func (d *Dispatcher) Register(name string, fn CommandFunc) {
	d.mu.Lock()
	d.cmds[strings.ToLower(name)] = registeredCommand{fn: fn}
	d.mu.Unlock()
}

// RegisterOperator adds a command that may only be used by server operators.
func (d *Dispatcher) RegisterOperator(name string, fn CommandFunc) {
	d.mu.Lock()
	d.cmds[strings.ToLower(name)] = registeredCommand{fn: fn, operatorOnly: true}
	d.mu.Unlock()
}

// RequireOperator upgrades already-registered commands to operator-only.
func (d *Dispatcher) RequireOperator(names ...string) {
	d.mu.Lock()
	for _, name := range names {
		key := strings.ToLower(name)
		command, ok := d.cmds[key]
		if ok {
			command.operatorOnly = true
			d.cmds[key] = command
		}
	}
	d.mu.Unlock()
}

// SetEntityIDAllocator installs the game-wide allocator used by entity-spawning
// commands. The allocator may be configured once during server startup.
func (d *Dispatcher) SetEntityIDAllocator(allocate func() int32) {
	d.mu.Lock()
	d.nextEntityID = allocate
	d.mu.Unlock()
}

// SetPlayerFinder installs the edition-neutral online-player lookup used by
// administrative commands such as /op and /god.
func (d *Dispatcher) SetPlayerFinder(find func(string) *player.Player) {
	d.mu.Lock()
	d.findPlayer = find
	d.mu.Unlock()
}

// Dispatch parses input (with or without a leading '/'), resolves the command
// name, fills ctx.Args with the remaining tokens, and calls the registered
// handler.
//
// Unknown commands and handler errors are reported to ctx.Conn as a system
// message.  Dispatch itself never returns an error to the caller because
// command failures are player-facing, not server-fatal.
func (d *Dispatcher) Dispatch(input string, ctx CommandContext) {
	input = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), "/"))
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}

	name := strings.ToLower(parts[0])
	ctx.Args = parts[1:]

	d.mu.RLock()
	command, ok := d.cmds[name]
	allocateEntityID := d.nextEntityID
	findPlayer := d.findPlayer
	d.mu.RUnlock()
	ctx.NextEntityID = allocateEntityID
	ctx.FindPlayer = findPlayer

	if !ok {
		if ctx.Reply != nil {
			_ = ctx.Reply(fmt.Sprintf(`Unknown command: /%s`, name))
			return
		}
		_ = sendSystemMessage(ctx.Conn, fmt.Sprintf("Unknown command: /%s", name))
		return
	}
	if command.operatorOnly && (ctx.Player == nil || !ctx.Player.Operator) {
		_ = sendCommandMessage(ctx, `You do not have permission to use this command`)
		return
	}
	if err := command.fn(ctx); err != nil {
		if ctx.Reply != nil {
			_ = ctx.Reply(`Error: ` + err.Error())
			return
		}
		_ = sendSystemMessage(ctx.Conn, "Error: "+err.Error())
	}
}
