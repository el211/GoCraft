// Package entity defines the edition-agnostic entity model used by the
// GoCraft game core.  Java- or Bedrock-specific identifiers (numeric entity
// type registry IDs, metadata formats) must live in the respective adapter
// packages; this package uses only canonical resource-location strings.
package entity

import (
	"GoCraft/core/player"
	"GoCraft/core/spatial"
)

// EntityType is the canonical resource location for a Minecraft entity type,
// e.g. "minecraft:cow".
type EntityType string

// VillagerVariant and VillagerProfession are canonical core values. Edition
// adapters translate them to their own registry IDs at the protocol boundary.
type VillagerVariant string
type VillagerProfession string

const (
	VillagerVariantDesert  VillagerVariant = "minecraft:desert"
	VillagerVariantJungle  VillagerVariant = "minecraft:jungle"
	VillagerVariantPlains  VillagerVariant = "minecraft:plains"
	VillagerVariantSavanna VillagerVariant = "minecraft:savanna"
	VillagerVariantSnow    VillagerVariant = "minecraft:snow"
	VillagerVariantSwamp   VillagerVariant = "minecraft:swamp"
	VillagerVariantTaiga   VillagerVariant = "minecraft:taiga"
)

const (
	VillagerProfessionNone          VillagerProfession = "minecraft:none"
	VillagerProfessionArmorer       VillagerProfession = "minecraft:armorer"
	VillagerProfessionButcher       VillagerProfession = "minecraft:butcher"
	VillagerProfessionCartographer  VillagerProfession = "minecraft:cartographer"
	VillagerProfessionCleric        VillagerProfession = "minecraft:cleric"
	VillagerProfessionFarmer        VillagerProfession = "minecraft:farmer"
	VillagerProfessionFisherman     VillagerProfession = "minecraft:fisherman"
	VillagerProfessionFletcher      VillagerProfession = "minecraft:fletcher"
	VillagerProfessionLeatherworker VillagerProfession = "minecraft:leatherworker"
	VillagerProfessionLibrarian     VillagerProfession = "minecraft:librarian"
	VillagerProfessionMason         VillagerProfession = "minecraft:mason"
	VillagerProfessionNitwit        VillagerProfession = "minecraft:nitwit"
	VillagerProfessionShepherd      VillagerProfession = "minecraft:shepherd"
	VillagerProfessionToolsmith     VillagerProfession = "minecraft:toolsmith"
	VillagerProfessionWeaponsmith   VillagerProfession = "minecraft:weaponsmith"
)

// All known entity types (complete list for Minecraft 1.21.4 / protocol 769).
const (
	// ── Passive mobs ─────────────────────────────────────────────────────────
	TypeAllay            EntityType = "minecraft:allay"
	TypeArmadillo        EntityType = "minecraft:armadillo"
	TypeAxolotl          EntityType = "minecraft:axolotl"
	TypeBat              EntityType = "minecraft:bat"
	TypeCamel            EntityType = "minecraft:camel"
	TypeCat              EntityType = "minecraft:cat"
	TypeChicken          EntityType = "minecraft:chicken"
	TypeCod              EntityType = "minecraft:cod"
	TypeCow              EntityType = "minecraft:cow"
	TypeDonkey           EntityType = "minecraft:donkey"
	TypeFox              EntityType = "minecraft:fox"
	TypeFrog             EntityType = "minecraft:frog"
	TypeGlowSquid        EntityType = "minecraft:glow_squid"
	TypeGoat             EntityType = "minecraft:goat"
	TypeHorse            EntityType = "minecraft:horse"
	TypeMooshroom        EntityType = "minecraft:mooshroom"
	TypeMule             EntityType = "minecraft:mule"
	TypeOcelot           EntityType = "minecraft:ocelot"
	TypePanda            EntityType = "minecraft:panda"
	TypeParrot           EntityType = "minecraft:parrot"
	TypePig              EntityType = "minecraft:pig"
	TypePufferfish       EntityType = "minecraft:pufferfish"
	TypeRabbit           EntityType = "minecraft:rabbit"
	TypeSalmon           EntityType = "minecraft:salmon"
	TypeSheep            EntityType = "minecraft:sheep"
	TypeSkeletonHorse    EntityType = "minecraft:skeleton_horse"
	TypeSniffer          EntityType = "minecraft:sniffer"
	TypeSquid            EntityType = "minecraft:squid"
	TypeTadpole          EntityType = "minecraft:tadpole"
	TypeTropicalFish     EntityType = "minecraft:tropical_fish"
	TypeTurtle           EntityType = "minecraft:turtle"
	TypeVillager         EntityType = "minecraft:villager"
	TypeFallingBlock     EntityType = "minecraft:falling_block"
	TypePrimedTNT        EntityType = "minecraft:tnt"
	TypeItem             EntityType = "minecraft:item"
	TypeExperienceOrb    EntityType = "minecraft:experience_orb"
	TypeArrow            EntityType = "minecraft:arrow"
	TypeSpectralArrow    EntityType = "minecraft:spectral_arrow"
	TypeTrident          EntityType = "minecraft:trident"
	TypeWindCharge       EntityType = "minecraft:wind_charge"
	TypeSnowball         EntityType = "minecraft:snowball"
	TypeEgg              EntityType = "minecraft:egg"
	TypeEnderPearl       EntityType = "minecraft:ender_pearl"
	TypeExperienceBottle EntityType = "minecraft:experience_bottle"
	TypePotion           EntityType = "minecraft:potion"
	TypeAreaEffectCloud  EntityType = "minecraft:area_effect_cloud"
	TypeSmallFireball    EntityType = "minecraft:small_fireball"
	TypeFireball         EntityType = "minecraft:fireball"
	TypeEyeOfEnder       EntityType = "minecraft:eye_of_ender"
	TypeFireworkRocket   EntityType = "minecraft:firework_rocket"
	TypeWanderingTrader  EntityType = "minecraft:wandering_trader"

	// ── Boats ────────────────────────────────────────────────────────────────
	TypeOakBoat      EntityType = "minecraft:oak_boat"
	TypeSpruceBoat   EntityType = "minecraft:spruce_boat"
	TypeBirchBoat    EntityType = "minecraft:birch_boat"
	TypeJungleBoat   EntityType = "minecraft:jungle_boat"
	TypeAcaciaBoat   EntityType = "minecraft:acacia_boat"
	TypeDarkOakBoat  EntityType = "minecraft:dark_oak_boat"
	TypeMangroveBoat EntityType = "minecraft:mangrove_boat"
	TypeCherryBoat   EntityType = "minecraft:cherry_boat"
	TypeBambooRaft   EntityType = "minecraft:bamboo_raft"

	TypeOakChestBoat      EntityType = "minecraft:oak_chest_boat"
	TypeSpruceChestBoat   EntityType = "minecraft:spruce_chest_boat"
	TypeBirchChestBoat    EntityType = "minecraft:birch_chest_boat"
	TypeJungleChestBoat   EntityType = "minecraft:jungle_chest_boat"
	TypeAcaciaChestBoat   EntityType = "minecraft:acacia_chest_boat"
	TypeDarkOakChestBoat  EntityType = "minecraft:dark_oak_chest_boat"
	TypeMangroveChestBoat EntityType = "minecraft:mangrove_chest_boat"
	TypeCherryChestBoat   EntityType = "minecraft:cherry_chest_boat"
	TypeBambooChestRaft   EntityType = "minecraft:bamboo_chest_raft"
	TypeZombieHorse       EntityType = "minecraft:zombie_horse"
	TypeMinecart          EntityType = "minecraft:minecart"
	TypeChestMinecart     EntityType = "minecraft:chest_minecart"
	TypeFurnaceMinecart   EntityType = "minecraft:furnace_minecart"
	TypeTNTMinecart       EntityType = "minecraft:tnt_minecart"
	TypeHopperMinecart    EntityType = "minecraft:hopper_minecart"
	TypeSpawnerMinecart   EntityType = "minecraft:spawner_minecart"
	TypeCommandMinecart   EntityType = "minecraft:command_block_minecart"

	// ── Neutral / tameable mobs ───────────────────────────────────────────────
	TypeBee             EntityType = "minecraft:bee"
	TypeDolphin         EntityType = "minecraft:dolphin"
	TypeIronGolem       EntityType = "minecraft:iron_golem"
	TypeLlama           EntityType = "minecraft:llama"
	TypePolarBear       EntityType = "minecraft:polar_bear"
	TypeSnowGolem       EntityType = "minecraft:snow_golem"
	TypeStrider         EntityType = "minecraft:strider"
	TypeTraderLlama     EntityType = "minecraft:trader_llama"
	TypeWolf            EntityType = "minecraft:wolf"
	TypeZombifiedPiglin EntityType = "minecraft:zombified_piglin"

	// ── Hostile mobs ─────────────────────────────────────────────────────────
	TypeBlaze          EntityType = "minecraft:blaze"
	TypeBogged         EntityType = "minecraft:bogged"
	TypeBreeze         EntityType = "minecraft:breeze"
	TypeCaveSpider     EntityType = "minecraft:cave_spider"
	TypeCreaker        EntityType = "minecraft:creaking"
	TypeCreeper        EntityType = "minecraft:creeper"
	TypeDrowned        EntityType = "minecraft:drowned"
	TypeElderGuardian  EntityType = "minecraft:elder_guardian"
	TypeEnderman       EntityType = "minecraft:enderman"
	TypeEndermite      EntityType = "minecraft:endermite"
	TypeEvoker         EntityType = "minecraft:evoker"
	TypeGhast          EntityType = "minecraft:ghast"
	TypeGuardian       EntityType = "minecraft:guardian"
	TypeHoglin         EntityType = "minecraft:hoglin"
	TypeHusk           EntityType = "minecraft:husk"
	TypeIllusioner     EntityType = "minecraft:illusioner"
	TypeMagmaCube      EntityType = "minecraft:magma_cube"
	TypePhantom        EntityType = "minecraft:phantom"
	TypePiglin         EntityType = "minecraft:piglin"
	TypePiglinBrute    EntityType = "minecraft:piglin_brute"
	TypePillager       EntityType = "minecraft:pillager"
	TypeRavager        EntityType = "minecraft:ravager"
	TypeShulker        EntityType = "minecraft:shulker"
	TypeSilverfish     EntityType = "minecraft:silverfish"
	TypeSkeleton       EntityType = "minecraft:skeleton"
	TypeSlime          EntityType = "minecraft:slime"
	TypeSpider         EntityType = "minecraft:spider"
	TypeStray          EntityType = "minecraft:stray"
	TypeVex            EntityType = "minecraft:vex"
	TypeVindicator     EntityType = "minecraft:vindicator"
	TypeWarden         EntityType = "minecraft:warden"
	TypeWitch          EntityType = "minecraft:witch"
	TypeWither         EntityType = "minecraft:wither"
	TypeWitherSkeleton EntityType = "minecraft:wither_skeleton"
	TypeZoglin         EntityType = "minecraft:zoglin"
	TypeZombie         EntityType = "minecraft:zombie"
	TypeZombieVillager EntityType = "minecraft:zombie_villager"
)

// Entity is the canonical server-side representation of any non-player entity.
//
// # Concurrency ownership
//
// The entity tick goroutine in server.Server is the sole writer of the spatial
// and health fields (Position, VX/VY/VZ, OnGround, Health, Dead).  All other
// goroutines (player handlers, commands) must treat these fields as read-only
// unless they hold an external lock or go through a dedicated mutation API.
//
// The current design is safe because:
//   - Only the tick goroutine calls Damage/Heal and integrates velocity.
//   - Broadcast goroutines only read from already-built protocol.Packet bytes,
//     never from entity fields directly (see server.tickEntities).
//
// Per-entity locking will be added when combat or concurrent mutations are
// introduced.
type Entity struct {
	// Identity
	EntityID int32
	UUID     [16]byte
	Type     EntityType

	// Villager identity. Zero values are treated as plains/none/level 1 by
	// edition adapters and are ignored for non-villager entities.
	VillagerVariant    VillagerVariant
	VillagerProfession VillagerProfession
	VillagerLevel      int32
	// VillagerExperience locks the profession once the first trade is made,
	// matching vanilla's rule that traded villagers do not become unemployed
	// when their workstation disappears. VillagerHasTraded is kept explicit so
	// zero-XP custom trades can lock a profession too.
	VillagerExperience int32
	VillagerHasTraded  bool
	// NaturalSpawned distinguishes mobs created by the runtime mob spawner from
	// generated village residents, commands, and player-created entities. It is
	// used for distance despawning in the absence of chunk-scoped entity storage.
	NaturalSpawned bool

	// Generated village ownership. HasVillageHome distinguishes assigned
	// positions from zero-value coordinates used by summoned villagers.
	HasVillageHome        bool
	VillageHome           spatial.BlockPos
	VillageCenter         spatial.BlockPos
	VillageBed            spatial.BlockPos
	VillageWorkstation    spatial.BlockPos
	HasVillageWorkstation bool
	Sleeping              bool

	// FallingBlock fields — only used when Type == TypeFallingBlock.
	// FallingBlockStateID is the Java global block-state ID sent in the Spawn
	// Entity data field so the client renders the correct block during the fall.
	// FallingBlockName is the resource location placed when the entity lands.
	FallingBlockStateID int32
	FallingBlockName    string

	// DisplayName is the custom name set by a name tag. "" means no custom name.
	// CustomNameVisible controls whether the name floats above the entity.
	DisplayName       string
	CustomNameVisible bool

	// Sheep fields — only used when Type == TypeSheep.
	// Sheared tracks whether the wool has been harvested. WoolColor is the
	// canonical dye name ("white", "black", etc.); "" defaults to "white".
	// WoolRegrowTicks counts down to wool regrowth after shearing.
	Sheared        bool
	WoolColor      string
	WoolRegrowTicks int32

	// Enderman fields - only used when type == TypeEnderman
	// EndermanCarriedBlock is the canonical resource location of the block
	// an enderman is holding, or "" when empty. Adapters resolve their own IDs.
	EndermanCarriedBlock string

	// PrimedTNT fields — only used when Type == TypePrimedTNT.
	// FuseTicks counts down from 80 to 0; at 0 the entity explodes.
	FuseTicks                                               int32
	MinecartDetectorX, MinecartDetectorY, MinecartDetectorZ int
	MinecartOnDetector                                      bool

	// Vehicle fields. RiderEntityID is the controlling/front passenger and
	// SecondRiderEntityID is the rear passenger used by boats and camels.
	// Passenger mutations are owned by the simulation tick.
	RiderEntityID       int32
	SecondRiderEntityID int32

	// Projectile fields.
	OwnerEntityID    int32
	ProjectileDamage float32
	ProjectileItem   player.ItemStack
	EyeTarget        spatial.Vec3
	HasEyeTarget     bool
	EyeSurvives      bool
	// Firework fields hold the canonical component and server-side lifetime.
	FireworkData      player.FireworkData
	FireworkLifeTicks int32
	FireworkLifetime  int32
	// Area-effect cloud fields hold lingering-potion lifecycle state.
	CloudRadius             float64
	CloudRadiusGrowth       float64
	CloudRadiusOnUse        float64
	CloudDurationTicks      int64
	CloudReapplicationDelay int64
	CloudTargets            map[int32]int64

	// Dropped-item fields. These are used only when Type == TypeItem and are
	// encoded as the ItemEntity's tracked ItemStack at metadata index 8.
	ItemID             string
	ItemCount          int
	ItemDamage         int
	ItemEnchantments   string
	ItemPotDecorations [4]string
	ItemHasFireworks   bool
	ItemFireworks      player.FireworkData
	ItemComponents     string
	// ExperienceAmount is the number of points carried by an experience orb.
	// ExperienceKillerUUID records the player whose damage caused a living
	// entity's death so the simulation can apply player-kill XP rewards.
	ExperienceAmount     int32
	ExperienceKillerUUID [16]byte
	HasExperienceKiller  bool

	// Age and animal interaction state. BabyAgeTicks counts upward from zero
	// until BabyGrowUpTicks. Love/cooldown values count down once per tick.
	IsBaby                bool
	BabyAgeTicks          int32
	LoveTicks             int32
	BreedingCooldownTicks int32
	BreedingMateEntityID  int32
	BreedingProgressTicks int32
	LoveCauseUUID         [16]byte
	HasLoveCause          bool

	// Tameable/rideable animal state. Ownership is UUID-authoritative so it
	// survives reconnects; TameOwnerEntityID is a runtime synchronization hint.
	Tamed             bool
	TameOwnerUUID     [16]byte
	HasTameOwner      bool
	TameOwnerEntityID int32
	Sitting           bool
	Trusting          bool
	Saddled           bool
	Temper            int32
	PoisonTicks       int32
	// Pufferfish state is 0 (small), 1 (half-puffed), or 2 (fully puffed).
	PufferState        int32
	PufferInflateTicks int
	PufferDeflateTicks int
	// MainHandItemID is the canonical item visibly equipped by a mob. Player
	// equipment remains in player.Player.Inventory.
	MainHandItemID string
	// FireTicks is the remaining time the entity is rendered and damaged as
	// burning. It is owned by the simulation tick.
	FireTicks int
	// UsingItem drives living-entity hand-use metadata, including the bow draw
	// animation used by skeletons.
	UsingItem bool
	// Spatial state — written only by the entity tick goroutine.
	Position   spatial.Vec3
	VX, VY, VZ float64 // velocity in blocks/tick
	Yaw, Pitch float32
	OnGround   bool
	AgeTicks   int64

	// Health
	Health    float32
	MaxHealth float32
	Dead      bool
	// DeathTicks keeps a dead living entity present long enough for the
	// vanilla 20-tick death animation before it is removed from clients.
	DeathTicks int
}

// SetDroppedItem copies a complete stack into this entity's dropped-item
// state. Callers should use this instead of assigning individual fields.
func (e *Entity) SetDroppedItem(stack player.ItemStack) {
	if e == nil {
		return
	}
	e.ItemID, e.ItemCount, e.ItemDamage = stack.ItemID, stack.Count, stack.Damage
	e.ItemEnchantments = stack.Enchantments
	e.ItemPotDecorations = stack.PotDecorations
	e.ItemHasFireworks, e.ItemFireworks = stack.HasFireworks, stack.Fireworks
	e.ItemComponents = stack.Components
}

// DroppedItem reconstructs the complete canonical stack carried by an item
// entity.
func (e *Entity) DroppedItem() player.ItemStack {
	if e == nil {
		return player.ItemStack{}
	}
	return player.ItemStack{
		ItemID: e.ItemID, Count: e.ItemCount, Damage: e.ItemDamage,
		Enchantments: e.ItemEnchantments, PotDecorations: e.ItemPotDecorations,
		HasFireworks: e.ItemHasFireworks, Fireworks: e.ItemFireworks,
		Components: e.ItemComponents,
	}
}

// CanTradeAsVillager reports whether a villager is old enough and has a
// profession that may expose merchant offers. Unemployed villagers and
// nitwits use the vanilla/Pumpkin unhappy interaction instead.
func (e *Entity) CanTradeAsVillager() bool {
	if e == nil || e.Type != TypeVillager || e.IsBaby {
		return false
	}
	switch e.VillagerProfession {
	case "", VillagerProfessionNone, VillagerProfessionNitwit:
		return false
	default:
		return true
	}
}

// New creates an entity of the given type at the given position with full health.
func New(id int32, uuid [16]byte, t EntityType, x, y, z float64) *Entity {
	maxHP := defaultMaxHealth(t)
	switch t {
	case TypeHorse, TypeDonkey, TypeMule, TypeLlama, TypeTraderLlama:
		roll := uint32(id)*1664525 + 1013904223
		maxHP = 15 + float32(roll%16)
	}
	e := &Entity{
		EntityID:  id,
		UUID:      uuid,
		Type:      t,
		Position:  spatial.Vec3{X: x, Y: y, Z: z},
		Health:    maxHP,
		MaxHealth: maxHP,
	}
	if t == TypeSkeletonHorse || t == TypeZombieHorse {
		e.Tamed = true
	}
	if t == TypeTNTMinecart {
		e.FuseTicks = -1
	}
	switch t {
	case TypeSkeleton, TypeStray, TypeBogged:
		e.MainHandItemID = "minecraft:bow"
	case TypePillager, TypePiglin:
		e.MainHandItemID = "minecraft:crossbow"
	case TypeVindicator:
		e.MainHandItemID = "minecraft:iron_axe"
	case TypeWitherSkeleton:
		e.MainHandItemID = "minecraft:stone_sword"
	}
	return e
}

// NewPrimedTNT creates an ignited TNT entity with the vanilla fuse and initial
// upward motion shared by block, redstone, and dispenser activation paths.
func NewPrimedTNT(id int32, uuid [16]byte, x, y, z float64) *Entity {
	tnt := New(id, uuid, TypePrimedTNT, x, y, z)
	tnt.FuseTicks = 80
	tnt.VY = 0.2
	return tnt
}

// defaultMaxHealth returns the base max health for all entity types.
// Unknown types default to 20 (10 hearts).
func defaultMaxHealth(t EntityType) float32 {
	switch t {
	// Passive — low health
	case TypeCod, TypeSalmon, TypeTropicalFish, TypePufferfish:
		return 3
	case TypeRabbit:
		return 3
	case TypeChicken:
		return 4
	case TypeBat, TypeParrot, TypeTadpole:
		return 6
	case TypeOcelot, TypeCat:
		return 10
	case TypeSheep:
		return 8
	case TypeCow, TypeMooshroom, TypePig, TypeGlowSquid, TypeSquid,
		TypeDolphin, TypeFrog:
		return 10
	case TypeFox, TypeGoat:
		return 10
	case TypeArmadillo:
		return 12
	case TypeAllay:
		return 20
	case TypePanda:
		return 20
	case TypeLlama, TypeTraderLlama:
		return 22 // average of 15–30 range
	case TypeHorse, TypeDonkey, TypeMule:
		return 26 // average of 15–30 range
	case TypeCamel:
		return 32
	case TypeSkeletonHorse, TypeZombieHorse:
		return 15
	case TypeSniffer:
		return 14
	case TypeTurtle:
		return 30
	case TypeBee:
		return 10
	case TypeAxolotl:
		return 14
	case TypeWanderingTrader:
		return 20

	// Neutral
	case TypeWolf:
		return 8
	case TypePolarBear:
		return 30
	case TypeStrider:
		return 20
	case TypeZombifiedPiglin:
		return 20

	// Hostile
	case TypeZombie, TypeSkeleton, TypeCreeper, TypeHusk, TypeDrowned,
		TypePhantom, TypeBlaze, TypeStray, TypeZombieVillager:
		return 20
	case TypeEnderman, TypeZoglin:
		return 40
	case TypeSpider, TypeBogged, TypePiglin:
		return 16
	case TypeVex:
		return 14
	case TypePillager, TypeVindicator:
		return 24
	case TypeWitch:
		return 26
	case TypeCaveSpider:
		return 12
	case TypeSilverfish, TypeEndermite:
		return 8
	case TypeSlime, TypeMagmaCube:
		return 1
	case TypeGuardian:
		return 30
	case TypeElderGuardian:
		return 80
	case TypeGhast:
		return 10
	case TypeRavager:
		return 100
	case TypeHoglin:
		return 40
	case TypePiglinBrute:
		return 50
	case TypeShulker:
		return 30
	case TypeBreeze:
		return 30
	case TypeCreaker:
		return 1 // effectively invulnerable; simplified
	case TypeEvoker:
		return 24
	case TypeIllusioner:
		return 32
	case TypeWarden:
		return 500
	case TypeWither:
		return 300
	case TypeWitherSkeleton:
		return 20

	// Iron/Snow Golem
	case TypeIronGolem:
		return 100
	case TypeSnowGolem:
		return 4

	// Villager
	case TypeVillager:
		return 20
	case TypeItem, TypeExperienceOrb, TypeArrow, TypeSpectralArrow, TypeTrident, TypeWindCharge, TypeFireworkRocket:
		return 1

	// Boats
	case TypeOakBoat, TypeSpruceBoat, TypeBirchBoat, TypeJungleBoat,
		TypeAcaciaBoat, TypeDarkOakBoat, TypeMangroveBoat, TypeCherryBoat,
		TypeBambooRaft, TypeOakChestBoat, TypeSpruceChestBoat, TypeBirchChestBoat,
		TypeJungleChestBoat, TypeAcaciaChestBoat, TypeDarkOakChestBoat,
		TypeMangroveChestBoat, TypeCherryChestBoat, TypeBambooChestRaft,
		TypeMinecart, TypeChestMinecart, TypeFurnaceMinecart, TypeTNTMinecart,
		TypeHopperMinecart, TypeSpawnerMinecart, TypeCommandMinecart:
		return 40

	default:
		return 20
	}
}

// Damage reduces the entity's health by amount.  If health reaches zero the
// entity is marked Dead.  Damage to an already-dead entity is a no-op.
func (e *Entity) Damage(amount float32) {
	if e.Dead || amount <= 0 {
		return
	}
	e.Health -= amount
	if e.Health <= 0 {
		e.Health = 0
		e.Dead = true
	}
}

// Heal restores health by amount, capped at MaxHealth.  Reviving a dead
// entity is not supported here; callers that want to respawn entities should
// create a new one.
func (e *Entity) Heal(amount float32) {
	if amount <= 0 {
		return
	}
	e.Health += amount
	if e.Health > e.MaxHealth {
		e.Health = e.MaxHealth
	}
}

// IsAlive reports whether the entity has not yet died.
func (e *Entity) IsAlive() bool { return !e.Dead }

// IsBoat reports whether this entity is any boat or raft variant.
func IsBoat(t EntityType) bool {
	switch t {
	case TypeOakBoat, TypeSpruceBoat, TypeBirchBoat, TypeJungleBoat,
		TypeAcaciaBoat, TypeDarkOakBoat, TypeMangroveBoat, TypeCherryBoat,
		TypeBambooRaft, TypeOakChestBoat, TypeSpruceChestBoat, TypeBirchChestBoat,
		TypeJungleChestBoat, TypeAcaciaChestBoat, TypeDarkOakChestBoat,
		TypeMangroveChestBoat, TypeCherryChestBoat, TypeBambooChestRaft:
		return true
	}
	return false
}

func IsMinecart(t EntityType) bool {
	switch t {
	case TypeMinecart, TypeChestMinecart, TypeFurnaceMinecart, TypeTNTMinecart,
		TypeHopperMinecart, TypeSpawnerMinecart, TypeCommandMinecart:
		return true
	}
	return false
}

func IsProjectile(t EntityType) bool {
	switch t {
	case TypeArrow, TypeSpectralArrow, TypeTrident, TypeWindCharge,
		TypeSnowball, TypeEgg, TypeEnderPearl, TypeExperienceBottle,
		TypePotion, TypeSmallFireball, TypeFireball, TypeEyeOfEnder, TypeFireworkRocket:
		return true
	}
	return false
}
