// Package player defines the edition-agnostic Player model used by the
// GoCraft game core. Java- or Bedrock-specific fields (packet IDs, metadata
// formats, registry indices) must live in the respective adapter packages.
package player

import (
	"sync"
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
	healthMu sync.Mutex

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

	// Operator grants access to administrative slash commands. GodMode blocks
	// ordinary survival damage until an operator disables it with /ungod.
	Operator bool
	GodMode  bool

	// Flying and movement-speed settings are protocol-independent player
	// preferences controlled by the built-in flight and speed commands.
	AllowFlying bool
	Flying      bool
	FlySpeed    float32
	WalkSpeed   float32

	// AttackCooldown selects modern timed attacks when true. LastAttack is
	// canonical combat timing state shared by every protocol adapter.
	AttackCooldown      bool
	LastAttack          time.Time
	KnockbackHorizontal float64
	KnockbackVertical   float64

	// Survival state is authoritative on the server. Health is measured in
	// half-hearts (20 is the normal ten-heart maximum).
	Health            float32
	MaxHealth         float32
	Food              int32
	Saturation        float32
	Dead              bool
	LastDamageCause   string
	InvulnerableUntil time.Time
	// OnDeath is installed by the owning server and runs once when health first
	// reaches zero. It is used for edition-neutral world effects such as
	// dropping the survival inventory.
	OnDeath func(*Player)

	// FallDistance accumulates downward travel while airborne. Sprinting is
	// tracked from the client command packet and is used by legacy knockback.
	FallDistance          float64
	Sprinting             bool
	UsingItemID           string
	UsingItemSince        time.Time
	LastEnvironmentDamage time.Time
	UnderwaterSince       time.Time

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
	WorldSpawn    spatial.Vec3

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
	OpenContainerID         int32
	OpenContainerKind       string
	OpenContainerPos        spatial.BlockPos // right-half pos (slots 0-26) or sole chest
	OpenContainerPartnerPos spatial.BlockPos // left-half pos (slots 27-53); zero if single
	OpenContainerHasPartner bool
	ContainerStateID        int32
	ContainerSlots          []ItemStack
	CraftingGrid            [9]ItemStack
	CraftingResult          ItemStack
	CarriedItem             ItemStack
}

// HeldItem returns the ItemStack in the currently selected hotbar slot.
func (p *Player) HeldItem() ItemStack {
	return p.Inventory[HotbarStart+p.HeldSlot]
}

// ArmorPoints returns the armour HUD value from the four equipped slots.
func (p *Player) ArmorPoints() int {
	total := 0
	for i := 5; i <= 8; i++ {
		total += ArmorPoints(p.Inventory[i].ItemID)
	}
	return total
}

// ArmorToughness returns the total armour toughness contributed by equipped
// diamond and netherite armour.
func (p *Player) ArmorToughness() float32 {
	var total float32
	for i := 5; i <= 8; i++ {
		total += ArmorToughness(p.Inventory[i].ItemID)
	}
	return total
}

// KnockbackResistance returns the equipped armour's vanilla knockback
// resistance, capped to the attribute's normal range.
func (p *Player) KnockbackResistance() float32 {
	var total float32
	for i := 5; i <= 8; i++ {
		total += ArmorKnockbackResistance(p.Inventory[i].ItemID)
	}
	if total > 1 {
		return 1
	}
	return total
}

// ApplyDamage atomically subtracts health. It returns the new health and
// whether this hit caused death.
func (p *Player) ApplyDamage(amount float32, cause string) (health float32, died bool) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if amount <= 0 || p.Dead {
		return p.Health, false
	}
	p.Health -= amount
	if p.Health <= 0 {
		p.Health = 0
		p.Dead = true
		died = true
	}
	p.LastDamageCause = cause
	return p.Health, died
}

// HealthSnapshot returns a consistent copy of the client-visible survival
// state.
func (p *Player) HealthSnapshot() (health float32, food int32, saturation float32, dead bool) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return p.Health, p.Food, p.Saturation, p.Dead
}

// HealFull restores a living player to full health and hunger. Dead players
// must complete the normal respawn handshake before they can be healed.
func (p *Player) HealFull() bool {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if p.Dead {
		return false
	}
	if p.MaxHealth <= 0 {
		p.MaxHealth = 20
	}
	p.Health = p.MaxHealth
	p.Food = 20
	p.Saturation = 5
	p.LastDamageCause = ``
	p.LastEnvironmentDamage = time.Time{}
	p.UnderwaterSince = time.Time{}
	return true
}

// Revive restores a dead player to the standard full survival state.
func (p *Player) Revive() {
	p.healthMu.Lock()
	if p.MaxHealth <= 0 {
		p.MaxHealth = 20
	}
	p.Health = p.MaxHealth
	p.Food = 20
	p.Saturation = 5
	p.Dead = false
	p.LastDamageCause = ""
	p.FallDistance = 0
	p.OnGround = false
	p.Sleeping = false
	p.Sprinting = false
	p.Flying = false
	p.UsingItemID = ""
	p.UsingItemSince = time.Time{}
	p.LastAttack = time.Time{}
	p.LastEnvironmentDamage = time.Time{}
	p.UnderwaterSince = time.Time{}
	p.InvulnerableUntil = time.Now().Add(3 * time.Second)
	p.healthMu.Unlock()
}

// GiveItem adds item to the first available inventory slot, merging into an
// existing partial stack when possible.  Returns true if all items were placed,
// false if the inventory was full.
func (p *Player) GiveItem(item ItemStack) bool {
	if item.IsEmpty() {
		return true
	}
	remaining := item.Count
	stackLimit := MaxStackSize(item.ItemID)
	ranges := [][2]int{{HotbarStart, InventorySize}, {9, HotbarStart}}

	// Refuse atomically when the inventory cannot hold the whole request.
	capacity := 0
	for _, inventoryRange := range ranges {
		for i := inventoryRange[0]; i < inventoryRange[1]; i++ {
			slot := p.Inventory[i]
			switch {
			case slot.IsEmpty():
				capacity += stackLimit
			case slot.ItemID == item.ItemID && slot.Damage == item.Damage && slot.Count < stackLimit:
				capacity += stackLimit - slot.Count
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
			if slot.ItemID != item.ItemID || slot.Damage != item.Damage || slot.Count >= stackLimit {
				continue
			}
			room := stackLimit - slot.Count
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
			if add > stackLimit {
				add = stackLimit
			}
			*slot = ItemStack{ItemID: item.ItemID, Count: add, Damage: item.Damage}
			remaining -= add
		}
	}
	return remaining == 0
}

// New creates a Player with sensible defaults.
// Core-only callers get Creative for backwards compatibility; the server
// overrides this with default_gamemode from server.yml when players join.
func New(uuid [16]byte, username string, edition ClientEdition) *Player {
	return &Player{
		UUID:                uuid,
		Username:            username,
		Edition:             edition,
		Position:            spatial.DefaultSpawnPos,
		WorldSpawn:          spatial.DefaultSpawnPos,
		GameMode:            GameModeCreative,
		FlySpeed:            0.05,
		WalkSpeed:           0.1,
		Health:              20,
		MaxHealth:           20,
		Food:                20,
		Saturation:          5,
		KnockbackHorizontal: 0.4,
		KnockbackVertical:   0.4,
	}
}
