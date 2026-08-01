// Package player defines the edition-agnostic Player model used by the
// GoCraft game core. Java- or Bedrock-specific fields (packet IDs, metadata
// formats, registry indices) must live in the respective adapter packages.
package player

import (
	"time"

	"GoCraft/core/spatial"
)

// ClientEdition identifies which protocol edition the player is using.
type ClientEdition uint8

const (
	// ClientEditionJava is the Minecraft: Java Edition protocol.
	ClientEditionJava ClientEdition = iota
	// ClientEditionBedrock is the Minecraft: Bedrock Edition protocol.
	ClientEditionBedrock
)

// GameMode is the in-game play mode, identical across editions.
type GameMode uint8

const (
	GameModeSurvival  GameMode = 0
	GameModeCreative  GameMode = 1
	GameModeAdventure GameMode = 2
	GameModeSpectator GameMode = 3
)

// Player is the canonical server-side player representation.
// It is intentionally free of any Java- or Bedrock-specific types.
//
// Network-level concerns (packet sending, connection state, encryption) are
// owned by the edition adapter, which holds a *Player and updates it as
// packets arrive.
type Player struct {
	// UUID is the player's unique identifier (edition-agnostic).
	UUID [16]byte
	// Username is the player's display name.
	Username string
	// Edition indicates which protocol the player is connecting over.
	Edition ClientEdition

	// Position is the player's current world position.
	Position spatial.Vec3
	// Rotation holds the player's look direction.
	Rotation spatial.Rotation
	// OnGround reports whether the player last reported being on the ground.
	OnGround bool

	// GameMode is the current game mode.
	GameMode GameMode

	// Flying and movement-speed settings are protocol-independent player
	// preferences controlled by the built-in flight and speed commands.
	AllowFlying bool
	Flying      bool
	FlySpeed    float32
	WalkSpeed   float32

	// AttackCooldown selects modern timed attacks when true. LastAttack is
	// canonical combat timing state shared by every protocol adapter.
	AttackCooldown bool
	LastAttack     time.Time

	// EntityID is the server-assigned entity ID used in packets.
	// It is assigned by the game core when the player joins.
	EntityID int32

	// VehicleEntityID is the entity ID of the vehicle the player is currently
	// riding, or 0 if the player is on foot.
	VehicleEntityID int32

	// SpawnPoint is the player's individual respawn position (set by sleeping in
	// a bed).  HasSpawnPoint is false until the player has slept in a bed, in
	// which case the world spawn is used on death.
	SpawnPoint    spatial.BlockPos
	HasSpawnPoint bool

	// Sleeping is true while the player is in the sleep-request state (they
	// right-clicked a bed at night).  The server tick checks this to decide
	// when all players are sleeping and the night can be skipped.
	Sleeping bool

	// Inventory holds the player's item slots.
	// See the InventorySize / HotbarStart constants for the slot layout.
	Inventory [InventorySize]ItemStack

	// HeldSlot is the currently selected hotbar slot (0–8).
	HeldSlot int

	// Open-container state is edition-independent inventory state. The Java
	// adapter maps these slots to the protocol-specific crafting-table layout.
	OpenContainerID          int32
	OpenContainerKind        string
	OpenContainerPos         spatial.BlockPos // right-half pos (slots 0-26) or sole chest
	OpenContainerPartnerPos  spatial.BlockPos // left-half pos (slots 27-53); zero if single
	OpenContainerHasPartner  bool
	ContainerStateID         int32
	ContainerSlots           []ItemStack
	CraftingGrid      [9]ItemStack
	CraftingResult    ItemStack
	CarriedItem       ItemStack
}

// HeldItem returns the ItemStack in the currently selected hotbar slot.
func (p *Player) HeldItem() ItemStack {
	return p.Inventory[HotbarStart+p.HeldSlot]
}

// GiveItem adds item to the first available inventory slot, merging into an
// existing partial stack when possible.  Returns true if all items were placed,
// false if the inventory was full.
func (p *Player) GiveItem(item ItemStack) bool {
	if item.IsEmpty() {
		return true
	}
	remaining := item.Count
	ranges := [][2]int{{HotbarStart, InventorySize}, {9, HotbarStart}}

	// Refuse atomically when the inventory cannot hold the whole request.
	capacity := 0
	for _, inventoryRange := range ranges {
		for i := inventoryRange[0]; i < inventoryRange[1]; i++ {
			slot := p.Inventory[i]
			switch {
			case slot.IsEmpty():
				capacity += 64
			case slot.ItemID == item.ItemID && slot.Count < 64:
				capacity += 64 - slot.Count
			}
		}
	}
	if capacity < remaining {
		return false
	}

	// Merge into compatible partial stacks first.
	for _, inventoryRange := range ranges {
		for i := inventoryRange[0]; i < inventoryRange[1] && remaining > 0; i++ {
			slot := &p.Inventory[i]
			if slot.ItemID != item.ItemID || slot.Count >= 64 {
				continue
			}
			room := 64 - slot.Count
			add := remaining
			if add > room {
				add = room
			}
			slot.Count += add
			remaining -= add
		}
	}
	// Then use empty slots, splitting across stacks when necessary.
	for _, inventoryRange := range ranges {
		for i := inventoryRange[0]; i < inventoryRange[1] && remaining > 0; i++ {
			slot := &p.Inventory[i]
			if !slot.IsEmpty() {
				continue
			}
			add := remaining
			if add > 64 {
				add = 64
			}
			*slot = ItemStack{ItemID: item.ItemID, Count: add}
			remaining -= add
		}
	}
	return remaining == 0
}

// New creates a Player with sensible defaults.
// Game mode defaults to Creative so that block interaction works out-of-the-box
// for testing.  A config-driven game-mode option will be added in a later milestone.
func New(uuid [16]byte, username string, edition ClientEdition) *Player {
	return &Player{
		UUID:      uuid,
		Username:  username,
		Edition:   edition,
		Position:  spatial.DefaultSpawnPos,
		GameMode:  GameModeCreative,
		FlySpeed:  0.05,
		WalkSpeed: 0.1,
	}
}
