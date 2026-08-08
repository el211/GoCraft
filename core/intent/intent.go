// Package intent defines the message types that network adapter goroutines
// (Java, Bedrock) post to the core simulation loop, and the Bus that routes
// them.
//
// # Architecture
//
//	Java handler goroutine  ──┐
//	                          ├──> intent.Bus ──> core simulation tick
//	Bedrock handler goroutine ──┘
//
// Handlers must NOT mutate core/player or core/world state directly.
// They post an Intent; the simulation/tick goroutine drains the Bus and applies
// each intent, then publishes immutable outbound events to session writers.
//
// # Priority and backpressure
//
// The Bus uses three independent paths, matched to each intent's priority:
//
//   - Lifecycle (Join, Disconnect): blocking Post; never silently dropped.
//     Lifecycle events are rare, so occasional blocking is acceptable.
//     The caller should always combine Post with a context timeout.
//
//   - Moves: coalesced per player via sync.Map. Each UpdateMove call replaces
//     the previous pending position; the tick only sees the newest position.
//     High-frequency movement never fills a channel or blocks a goroutine.
//
//   - Gameplay (Chat, Teleport): non-blocking TryPost; drops are counted and
//     logged via slog. A full gameplay channel indicates an overloaded tick
//     loop, not a programming error.
package intent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
)

// ── Intent types ──────────────────────────────────────────────────────────────

// LifecycleIntent is the sealed interface for Join and Disconnect intents.
// External packages can hold and type-switch on LifecycleIntent values but
// cannot implement the interface, keeping the set of lifecycle events closed.
type LifecycleIntent interface{ isLifecycle() }

// GameplayIntent is the sealed interface for Chat and Teleport intents.
type GameplayIntent interface{ isGameplay() }

// JoinResult is the simulation's synchronous response to a JoinIntent.
type JoinResult struct {
	EntityID int32
	Position spatial.Vec3
	// Err is non-nil if the join was rejected (e.g. duplicate UUID).
	Err error
}

// JoinIntent requests that a player enter the world.
//
// Done must be a buffered channel with capacity ≥ 1. The simulation sends
// exactly one JoinResult to Done and never blocks, even if the caller has
// already timed out. Callers must always drain Done (use select with context
// cancellation or a timeout).
//
// TrustedIdentity must be false when the session is unauthenticated (Bedrock
// online_mode=false or Java online_mode=false). The simulation must never
// grant authority (operator status, XUID-based lookup) to untrusted identities.
type JoinIntent struct {
	PlayerUUID      [16]byte
	Username        string
	Edition         string // "java" or "bedrock"
	TrustedIdentity bool
	Done            chan<- JoinResult // buffered(1); never nil
}

// DisconnectIntent notifies the simulation that a player's connection closed.
// The simulation removes the player from the world and broadcasts despawn to
// remaining sessions.
type DisconnectIntent struct {
	PlayerUUID [16]byte
	Reason     string
}

// MoveIntent carries a player's latest position update. High-frequency; the
// Bus coalesces pending moves per player so the tick always sees only the
// newest position, regardless of how many packets arrived between ticks.
type MoveIntent struct {
	PlayerUUID [16]byte
	Position   spatial.Vec3
	Rotation   spatial.Rotation
	OnGround   bool
}

// ChatIntent carries a player chat message. Posted to the gameplay channel;
// the tick broadcasts it to all active sessions.
type ChatIntent struct {
	PlayerUUID  [16]byte
	DisplayName string // pre-resolved display name for broadcast formatting
	Message     string
}

// TeleportIntent requests a server-authoritative teleport. The simulation
// moves the player and triggers chunk streaming as needed.
type TeleportIntent struct {
	PlayerUUID [16]byte
	X, Y, Z    float64
}

// BlockInteractIntent carries a Bedrock block break/place prediction to the
// simulation thread. Action uses protocol-neutral values declared below.
type BlockInteractIntent struct {
	PlayerUUID [16]byte
	Action     uint8
	Position   spatial.BlockPos
	Face       int32
	HotbarSlot int32
}

// ConsumeFoodIntent requests consumption of the currently selected Bedrock
// hotbar item after the client completes its use-item animation.
type ConsumeFoodIntent struct {
	PlayerUUID [16]byte
	HotbarSlot int32
}

const (
	BlockActionBreak uint8 = iota + 1
	BlockActionUse
)

// EntityInteractIntent requests an attack or use against a canonical entity ID.
type EntityInteractIntent struct {
	PlayerUUID [16]byte
	TargetID   int32
	Attack     bool
}

// RespawnIntent requests revival after the Bedrock death-screen button.
type RespawnIntent struct {
	PlayerUUID [16]byte
}

type WakeIntent struct {
	PlayerUUID [16]byte
}

type HotbarIntent struct {
	PlayerUUID [16]byte
	Slot       int32
}

const (
	PlayerStateSprinting uint8 = iota + 1
	PlayerStateFlying
)

// PlayerStateIntent carries a Bedrock movement-state transition that must be
// validated by the simulation before it changes canonical player state.
type PlayerStateIntent struct {
	PlayerUUID [16]byte
	State      uint8
	Enabled    bool
}

const InventoryCursorSlot int16 = -1

const (
	InventoryActionMove uint8 = iota + 1
	InventoryActionSwap
	InventoryActionDrop
	InventoryActionDestroy
	// InventoryActionCreativeGive spawns an item directly into Destination from
	// the creative pool (no source slot). Only valid in creative game mode.
	InventoryActionCreativeGive
)

// InventoryAction describes one protocol-neutral operation in an atomic
// Bedrock item-stack request. Slots are canonical Player.Inventory indices;
// InventoryCursorSlot addresses Player.CarriedItem.
type InventoryAction struct {
	Kind        uint8
	Source      int16
	Destination int16
	Count       int
	// Item is the item to give; only populated for InventoryActionCreativeGive.
	Item player.ItemStack
}

type InventoryResult struct {
	Accepted bool
}

type InventoryIntent struct {
	PlayerUUID [16]byte
	Actions    []InventoryAction
	Done       chan<- InventoryResult
}

// Implement sealed interfaces.
func (JoinIntent) isLifecycle()          {}
func (DisconnectIntent) isLifecycle()    {}
func (ChatIntent) isGameplay()           {}
func (TeleportIntent) isGameplay()       {}
func (BlockInteractIntent) isGameplay()  {}
func (ConsumeFoodIntent) isGameplay()    {}
func (EntityInteractIntent) isGameplay() {}
func (RespawnIntent) isGameplay()        {}
func (WakeIntent) isGameplay()           {}
func (HotbarIntent) isGameplay()         {}
func (PlayerStateIntent) isGameplay()    {}
func (InventoryIntent) isGameplay()      {}

// ── Bus ───────────────────────────────────────────────────────────────────────

// Bus is the intent routing hub shared by all adapter goroutines.
// Construct with NewBus; do not copy after first use.
type Bus struct {
	lifecycle       chan LifecycleIntent
	moves           sync.Map // map[[16]byte]MoveIntent, latest wins
	gameplay        chan GameplayIntent
	droppedGameplay atomic.Int64
}

// NewBus creates a Bus with the given channel capacities.
//
// Recommended defaults:
//   - lifecycleCapacity = 64   (Join/Disconnect are rare; 64 is ample headroom)
//   - gameplayCapacity  = 512  (Chat bursts are bounded per player per tick)
func NewBus(lifecycleCapacity, gameplayCapacity int) *Bus {
	return &Bus{
		lifecycle: make(chan LifecycleIntent, lifecycleCapacity),
		gameplay:  make(chan GameplayIntent, gameplayCapacity),
	}
}

// PostJoin submits a JoinIntent and blocks until the intent is queued or ctx
// is cancelled.  Returns ctx.Err() if the context expires before the intent
// can be queued.
//
// Because JoinIntents are rare (one per player login) and must not be silently
// dropped, blocking is intentional.  Callers must always supply a context with
// a deadline to avoid blocking forever when the tick goroutine is stalled:
//
//	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
//	defer cancel()
//	if err := bus.PostJoin(ctx, intent); err != nil { … }
func (b *Bus) PostJoin(ctx context.Context, i JoinIntent) error {
	select {
	case b.lifecycle <- i:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PostDisconnect submits a DisconnectIntent and blocks until queued or ctx is
// cancelled.  Returns ctx.Err() on cancellation.
//
// Disconnect intents must not be silently dropped — a missed disconnect leaves
// a ghost player in the game core.  Callers should pass the server context
// (which has a very long or no deadline) to avoid spurious drops, and reserve
// short deadlines for tests.
func (b *Bus) PostDisconnect(ctx context.Context, i DisconnectIntent) error {
	select {
	case b.lifecycle <- i:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMove stores the latest position for a player. If a move for this
// player is already pending, it is atomically replaced (coalesced) without
// waking the tick goroutine or allocating a channel send.
//
// This is safe to call from multiple goroutines for the same player UUID; the
// last write wins.
func (b *Bus) UpdateMove(i MoveIntent) {
	b.moves.Store(i.PlayerUUID, i)
}

// PostChat submits a chat message. Returns false if the gameplay channel is
// full (message dropped). Drops are counted and logged via slog.
func (b *Bus) PostChat(i ChatIntent) bool {
	return b.tryGameplay(i)
}

// PostTeleport submits a server-authoritative teleport. Returns false if the
// gameplay channel is full.
func (b *Bus) PostTeleport(i TeleportIntent) bool {
	return b.tryGameplay(i)
}

func (b *Bus) PostBlockInteract(i BlockInteractIntent) bool   { return b.tryGameplay(i) }
func (b *Bus) PostConsumeFood(i ConsumeFoodIntent) bool       { return b.tryGameplay(i) }
func (b *Bus) PostEntityInteract(i EntityInteractIntent) bool { return b.tryGameplay(i) }
func (b *Bus) PostRespawn(i RespawnIntent) bool               { return b.tryGameplay(i) }
func (b *Bus) PostWake(i WakeIntent) bool                     { return b.tryGameplay(i) }
func (b *Bus) PostHotbar(i HotbarIntent) bool                 { return b.tryGameplay(i) }
func (b *Bus) PostPlayerState(i PlayerStateIntent) bool       { return b.tryGameplay(i) }
func (b *Bus) PostInventory(i InventoryIntent) bool           { return b.tryGameplay(i) }

func (b *Bus) tryGameplay(i GameplayIntent) bool {
	select {
	case b.gameplay <- i:
		return true
	default:
		n := b.droppedGameplay.Add(1)
		// Log on first drop and every 100th thereafter to avoid log spam.
		if n == 1 || n%100 == 0 {
			slog.Warn("intent.Bus: gameplay channel full; intent dropped",
				"dropped_total", n)
		}
		return false
	}
}

// ── Drain ─────────────────────────────────────────────────────────────────────

// DrainResult holds all intents collected for one simulation tick.
type DrainResult struct {
	// Lifecycle contains all pending JoinIntent and DisconnectIntent values,
	// in FIFO order.  Type-switch on the concrete types to handle each kind.
	Lifecycle []LifecycleIntent
	// Moves contains the latest pending MoveIntent for each player. At most
	// one entry per player UUID; intermediate positions are discarded.
	Moves []MoveIntent
	// Gameplay contains all pending ChatIntent and TeleportIntent values, in
	// approximate FIFO order.
	Gameplay []GameplayIntent
}

// Drain collects all pending intents without blocking.
// Call exactly once per simulation tick, from the tick goroutine only.
//
// The returned slices are freshly allocated and safe to hold across ticks.
func (b *Bus) Drain() DrainResult {
	var result DrainResult

	// Lifecycle: drain entire buffered channel non-blockingly.
	n := len(b.lifecycle)
	result.Lifecycle = make([]LifecycleIntent, 0, n)
	for range n {
		result.Lifecycle = append(result.Lifecycle, <-b.lifecycle)
	}

	// Moves: swap the sync.Map for a fresh snapshot.
	// Delete-then-collect is safe: new moves posted after this point are
	// picked up on the next tick, never lost.
	b.moves.Range(func(key, value any) bool {
		b.moves.Delete(key)
		result.Moves = append(result.Moves, value.(MoveIntent))
		return true
	})

	// Gameplay: drain entire buffered channel non-blockingly.
	n = len(b.gameplay)
	result.Gameplay = make([]GameplayIntent, 0, n)
	for range n {
		result.Gameplay = append(result.Gameplay, <-b.gameplay)
	}

	return result
}
