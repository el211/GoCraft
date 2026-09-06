// Package player defines the edition-agnostic Player model used by the
// GoCraft game core. Java- or Bedrock-specific fields (packet IDs, metadata
// formats, registry indices) must live in the respective adapter packages.
package player

import (
	"sync"
	"sync/atomic"
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
	MaxAirSupply               = 300
)

// Player is the canonical server-side player representation.
// It is intentionally free of any Java- or Bedrock-specific types.
//
// Network-level concerns (packet sending, connection state, encryption) are
// owned by the edition adapter, which holds a *Player and updates it as
// packets arrive.
type Player struct {
	healthMu     sync.Mutex
	experienceMu sync.Mutex
	tagsMu       sync.RWMutex
	activityUnix atomic.Int64

	// UUID is the player's unique identifier (edition-agnostic).
	UUID [16]byte
	// Username is the player's display name.
	Username string
	// Edition indicates which protocol the player is connecting over.
	Edition       ClientEdition
	RemoteAddress string

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
	Health               float32
	MaxHealth            float32
	Food                 int32
	Saturation           float32
	Exhaustion           float32
	Absorption           float32
	StatusEffects        []StatusEffect
	ExperienceLevel      int32
	ExperienceTotal      int32
	ExperienceProgress   float32
	experiencePickupTick int64
	// PendingWorkstationXP accumulates XP from grindstone disenchanting that
	// has not yet been broadcast; crafting.go drains and syncs it.
	PendingWorkstationXP int32
	tags                 map[string]struct{}
	Dead                 bool
	LastDamageCause      string
	InvulnerableUntil    time.Time
	// OnDeath is installed by the owning server and runs once when health first
	// reaches zero. It is used for edition-neutral world effects such as
	// dropping the survival inventory.
	OnDeath func(*Player)

	// FallDistance accumulates downward travel while airborne. Sprinting is
	// tracked from the client command packet and is used by legacy knockback.
	FallDistance   float64
	Sprinting      bool
	Sneaking       bool
	UsingItemID    string
	UsingItemSince time.Time
	// UsingItemSlot is the zero-based hotbar slot captured when a Bedrock item
	// use begins. It prevents changing slots mid-animation from consuming a
	// different stack. -1 means no active hand.
	UsingItemSlot         int
	LastEnvironmentDamage time.Time
	UnderwaterSince       time.Time
	AirSupply             int32
	DrowningTicks         int32
	LastVibrationPosition spatial.Vec3
	HasVibrationPosition  bool
	LastWindChargeUse time.Time
	LastGoatHornUse   time.Time
	// LastAttackerEntityID is the entity ID of the last mob that dealt damage to
	// this player. Used by tamed wolves to select a retaliation target.
	// Reset to 0 when the player respawns or the wolf loses the target.
	LastAttackerEntityID int32
	// LastAttackedEntityID is the entity ID of the last entity this player hit.
	// Used by tamed wolves (OwnerHurtTarget) to assist during player combat.
	LastAttackedEntityID int32

	// CrossbowLoaded is true when the player has completed a full crossbow draw
	// (≥25 ticks). Vanilla crossbow requires two actions: draw to load, then
	// right-click again to fire. Cleared on fire or on slot change.
	CrossbowLoaded bool

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
	Raining       bool
	Thundering    bool
	// Dimension uses Bedrock's vanilla IDs: 0 overworld, 1 Nether, 2 End.
	Dimension int32
	// PortalCooldownUntil prevents a player standing inside a portal from
	// immediately bouncing back before they can leave the destination portal.
	PortalCooldownUntil time.Time

	// Sleeping is true while the player is in the sleep-request state (they
	// right-clicked a bed at night).  The server tick checks this to decide
	// when all players are sleeping and the night can be skipped.
	Sleeping bool

	// Inventory holds the player's item slots.
	// See the InventorySize / HotbarStart constants for the slot layout.
	Inventory [InventorySize]ItemStack
	// EnderChestInventory is the player's private, persistent 27-slot storage.
	// It is shared by every Ender Chest but never stored in a world block entity.
	EnderChestInventory [27]ItemStack

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
	// WorkstationSelection is the zero-based recipe selected in a workstation
	// such as a stonecutter. It is transient UI state and is reset whenever a
	// new workstation is opened.
	WorkstationSelection int
	CraftingGrid         [9]ItemStack
	CraftingResult       ItemStack
	CarriedItem          ItemStack
	// CrafterDisabledSlots is a 9-bit bitmask (bit N = slot N is disabled)
	// for the currently open crafter block. Persisted to the block entity.
	CrafterDisabledSlots uint16
	QuickCraftButton     byte
	QuickCraftSlots      []int
}

// HeldItem returns the ItemStack in the currently selected hotbar slot.
func (p *Player) HeldItem() ItemStack {
	return p.Inventory[HotbarStart+p.HeldSlot]
}

// RecordMovementVibration reports a footstep-sized movement event at most once
// per block travelled. Sneaking suppresses it, matching sculk vibration rules.
func (p *Player) RecordMovementVibration() bool {
	if p == nil || p.Sneaking {
		return false
	}
	if p.HasVibrationPosition && p.Position.Distance(p.LastVibrationPosition) < 1 {
		return false
	}
	p.LastVibrationPosition = p.Position
	p.HasVibrationPosition = true
	return true
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
	if isFireDamageCause(cause) && p.hasStatusEffectLocked("minecraft:fire_resistance") {
		return p.Health, false
	}
	amount = p.resistedDamageLocked(amount, cause)
	absorbed := min(amount, p.Absorption)
	p.Absorption -= absorbed
	amount -= absorbed
	if amount <= 0 {
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

// HungerSnapshot returns the three Bedrock hunger attributes atomically.
func (p *Player) HungerSnapshot() (food int32, saturation, exhaustion float32) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return p.Food, p.Saturation, p.Exhaustion
}

// TickBreathing advances Pumpkin's air supply state by one server tick.
func (p *Player) TickBreathing(underwater bool) (air int32, changed, drown bool) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if p.Dead || p.GameMode == GameModeCreative || p.GameMode == GameModeSpectator {
		changed = p.AirSupply != MaxAirSupply
		p.AirSupply, p.DrowningTicks = MaxAirSupply, 0
		return p.AirSupply, changed, false
	}
	if underwater {
		if p.AirSupply > 0 {
			p.AirSupply--
			changed = true
		}
		if p.AirSupply == 0 {
			p.DrowningTicks++
			if p.DrowningTicks >= 20 {
				p.DrowningTicks = 0
				drown = true
			}
		}
		return p.AirSupply, changed, drown
	}
	p.DrowningTicks = 0
	if p.AirSupply < MaxAirSupply {
		p.AirSupply = min(MaxAirSupply, p.AirSupply+4)
		changed = true
	}
	return p.AirSupply, changed, false
}

func (p *Player) AirSupplySnapshot() int32 {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return p.AirSupply
}

// AddExhaustion applies vanilla's exhaustion rollover. Each four exhaustion
// points consumes saturation first, then one food point.
func (p *Player) AddExhaustion(amount float32) {
	if amount <= 0 {
		return
	}
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if p.Dead {
		return
	}
	p.Exhaustion += amount
	for p.Exhaustion >= 4 {
		p.Exhaustion -= 4
		if p.Saturation > 0 {
			p.Saturation--
			if p.Saturation < 0 {
				p.Saturation = 0
			}
		} else if p.Food > 0 {
			p.Food--
		}
	}
}

// ConsumeFood fills hunger and saturation. It returns false when the player
// is full and the item should not be consumed.
func (p *Player) ConsumeFood(nutrition int32, saturationModifier float32) bool {
	return p.ConsumeFoodAllowFull(nutrition, saturationModifier, false)
}

// ConsumeFoodAllowFull fills hunger and saturation, optionally permitting an
// item such as a golden apple to be eaten while the hunger bar is already
// full. It returns whether the item use should complete and consume the item.
func (p *Player) ConsumeFoodAllowFull(nutrition int32, saturationModifier float32, allowFull bool) bool {
	if nutrition <= 0 {
		return false
	}
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if p.Dead || (p.Food >= 20 && !allowFull) {
		return false
	}
	p.Food += nutrition
	if p.Food > 20 {
		p.Food = 20
	}
	p.Saturation += float32(nutrition) * saturationModifier * 2
	if p.Saturation > float32(p.Food) {
		p.Saturation = float32(p.Food)
	}
	return true
}

// Heal restores a bounded amount of health to a living player.
func (p *Player) Heal(amount float32) bool {
	if amount <= 0 {
		return false
	}
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	if p.Dead || p.Health >= p.MaxHealth {
		return false
	}
	p.Health += amount
	if p.Health > p.MaxHealth {
		p.Health = p.MaxHealth
	}
	return true
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
	p.Exhaustion = 0
	p.AirSupply = MaxAirSupply
	p.DrowningTicks = 0
	p.LastDamageCause = ``
	p.LastEnvironmentDamage = time.Time{}
	p.UnderwaterSince = time.Time{}
	return true
}

// RestoreFromTotem is called when a totem of undying prevents death.
// The player's Dead flag is cleared, health set to 1, and invulnerability
// applied for 1 second so residual damage packets don't immediately re-kill.
func (p *Player) RestoreFromTotem() {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.Health = 1
	p.Dead = false
	p.LastDamageCause = ""
	p.InvulnerableUntil = time.Now().Add(1 * time.Second)
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
	p.Exhaustion = 0
	p.AirSupply = MaxAirSupply
	p.DrowningTicks = 0
	p.Dead = false
	p.LastDamageCause = ""
	p.FallDistance = 0
	p.OnGround = false
	p.Sleeping = false
	p.Sprinting = false
	p.Sneaking = false
	p.Flying = false
	p.UsingItemID = ""
	p.UsingItemSince = time.Time{}
	p.UsingItemSlot = -1
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
	// The normal storage inventory is exactly 36 slots: hotbar 36-44 and main
	// inventory 9-35. Slot 45 is offhand, not a tenth hotbar slot.
	ranges := [][2]int{{HotbarStart, HotbarStart + 9}, {9, HotbarStart}}

	// Refuse atomically when the inventory cannot hold the whole request.
	capacity := 0
	for _, inventoryRange := range ranges {
		for i := inventoryRange[0]; i < inventoryRange[1]; i++ {
			slot := p.Inventory[i]
			switch {
			case slot.IsEmpty():
				capacity += stackLimit
			case slot.SameItem(item) && slot.Count < stackLimit:
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
			if !slot.SameItem(item) || slot.Count >= stackLimit {
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
			*slot = item
			slot.Count = add
			remaining -= add
		}
	}
	return remaining == 0
}

// New creates a Player with sensible defaults.
// Core-only callers get Creative for backwards compatibility; the server
// overrides this with default_gamemode from server.yml when players join.
func New(uuid [16]byte, username string, edition ClientEdition) *Player {
	p := &Player{
		UUID:                uuid,
		Username:            username,
		Edition:             edition,
		Position:            spatial.DefaultSpawnPos,
		WorldSpawn:          spatial.DefaultSpawnPos,
		GameMode:            GameModeCreative,
		FlySpeed:            0.05,
		WalkSpeed:           0.1,
		tags:                make(map[string]struct{}),
		Health:              20,
		MaxHealth:           20,
		Food:                20,
		Saturation:          5,
		AirSupply:           MaxAirSupply,
		UsingItemSlot:       -1,
		KnockbackHorizontal: 0.4,
		KnockbackVertical:   0.4,
	}
	p.TouchActivity()
	return p
}

// TouchActivity records client traffic for the server idle timeout.
func (p *Player) TouchActivity() {
	if p != nil {
		p.activityUnix.Store(time.Now().UnixNano())
	}
}

// IdleFor reports the duration since the player's last client packet.
func (p *Player) IdleFor(now time.Time) time.Duration {
	last := p.activityUnix.Load()
	if last == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, last))
}
