// Package entity defines the edition-agnostic entity model used by the
// GoCraft game core.  Java- or Bedrock-specific identifiers (numeric entity
// type registry IDs, metadata formats) must live in the respective adapter
// packages; this package uses only canonical resource-location strings.
package entity

import "GoCraft/core/spatial"

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
	TypeAllay           EntityType = "minecraft:allay"
	TypeArmadillo       EntityType = "minecraft:armadillo"
	TypeAxolotl         EntityType = "minecraft:axolotl"
	TypeBat             EntityType = "minecraft:bat"
	TypeCamel           EntityType = "minecraft:camel"
	TypeCat             EntityType = "minecraft:cat"
	TypeChicken         EntityType = "minecraft:chicken"
	TypeCod             EntityType = "minecraft:cod"
	TypeCow             EntityType = "minecraft:cow"
	TypeDonkey          EntityType = "minecraft:donkey"
	TypeFox             EntityType = "minecraft:fox"
	TypeFrog            EntityType = "minecraft:frog"
	TypeGlowSquid       EntityType = "minecraft:glow_squid"
	TypeGoat            EntityType = "minecraft:goat"
	TypeHorse           EntityType = "minecraft:horse"
	TypeMooshroom       EntityType = "minecraft:mooshroom"
	TypeMule            EntityType = "minecraft:mule"
	TypeOcelot          EntityType = "minecraft:ocelot"
	TypePanda           EntityType = "minecraft:panda"
	TypeParrot          EntityType = "minecraft:parrot"
	TypePig             EntityType = "minecraft:pig"
	TypePufferfish      EntityType = "minecraft:pufferfish"
	TypeRabbit          EntityType = "minecraft:rabbit"
	TypeSalmon          EntityType = "minecraft:salmon"
	TypeSheep           EntityType = "minecraft:sheep"
	TypeSkeletonHorse   EntityType = "minecraft:skeleton_horse"
	TypeSniffer         EntityType = "minecraft:sniffer"
	TypeSquid           EntityType = "minecraft:squid"
	TypeTadpole         EntityType = "minecraft:tadpole"
	TypeTropicalFish    EntityType = "minecraft:tropical_fish"
	TypeTurtle          EntityType = "minecraft:turtle"
	TypeVillager        EntityType = "minecraft:villager"
	TypeFallingBlock    EntityType = "minecraft:falling_block"
	TypePrimedTNT       EntityType = "minecraft:tnt"
	TypeWanderingTrader EntityType = "minecraft:wandering_trader"

	// ── Boats ────────────────────────────────────────────────────────────────
	TypeOakBoat       EntityType = "minecraft:oak_boat"
	TypeSpruceBoat    EntityType = "minecraft:spruce_boat"
	TypeBirchBoat     EntityType = "minecraft:birch_boat"
	TypeJungleBoat    EntityType = "minecraft:jungle_boat"
	TypeAcaciaBoat    EntityType = "minecraft:acacia_boat"
	TypeDarkOakBoat   EntityType = "minecraft:dark_oak_boat"
	TypeMangroveBoat  EntityType = "minecraft:mangrove_boat"
	TypeCherryBoat    EntityType = "minecraft:cherry_boat"
	TypeBambooRaft    EntityType = "minecraft:bamboo_raft"

	TypeOakChestBoat      EntityType = "minecraft:oak_chest_boat"
	TypeSpruceChestBoat   EntityType = "minecraft:spruce_chest_boat"
	TypeBirchChestBoat    EntityType = "minecraft:birch_chest_boat"
	TypeJungleChestBoat   EntityType = "minecraft:jungle_chest_boat"
	TypeAcaciaChestBoat   EntityType = "minecraft:acacia_chest_boat"
	TypeDarkOakChestBoat  EntityType = "minecraft:dark_oak_chest_boat"
	TypeMangroveChestBoat EntityType = "minecraft:mangrove_chest_boat"
	TypeCherryChestBoat   EntityType = "minecraft:cherry_chest_boat"
	TypeBambooChestRaft   EntityType = "minecraft:bamboo_chest_raft"
	TypeZombieHorse     EntityType = "minecraft:zombie_horse"

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

	// Generated village ownership. HasVillageHome distinguishes assigned
	// positions from zero-value coordinates used by summoned villagers.
	HasVillageHome     bool
	VillageHome        spatial.BlockPos
	VillageCenter      spatial.BlockPos
	VillageBed         spatial.BlockPos
	VillageWorkstation spatial.BlockPos
	Sleeping           bool

	// FallingBlock fields — only used when Type == TypeFallingBlock.
	// FallingBlockStateID is the Java global block-state ID sent in the Spawn
	// Entity data field so the client renders the correct block during the fall.
	// FallingBlockName is the resource location placed when the entity lands.
	FallingBlockStateID int32
	FallingBlockName    string

	// PrimedTNT fields — only used when Type == TypePrimedTNT.
	// FuseTicks counts down from 80 to 0; at 0 the entity explodes.
	FuseTicks int32

	// Boat / vehicle fields.
	// RiderEntityID is the entity ID of the passenger currently riding this
	// entity, or 0 if unoccupied. Only one rider is tracked (vanilla boats
	// allow two but we implement one for simplicity).
	RiderEntityID int32

	// IsBaby marks an ageable entity (villager) as a child.
	// BabyAgeTicks counts upward from 0; the tick goroutine grows the villager
	// up once BabyAgeTicks reaches BabyGrowUpTicks.
	IsBaby      bool
	BabyAgeTicks int32
	// Spatial state — written only by the entity tick goroutine.
	Position   spatial.Vec3
	VX, VY, VZ float64 // velocity in blocks/tick
	Yaw, Pitch float32
	OnGround   bool

	// Health
	Health    float32
	MaxHealth float32
	Dead      bool
}

// New creates an entity of the given type at the given position with full health.
func New(id int32, uuid [16]byte, t EntityType, x, y, z float64) *Entity {
	maxHP := defaultMaxHealth(t)
	return &Entity{
		EntityID:  id,
		UUID:      uuid,
		Type:      t,
		Position:  spatial.Vec3{X: x, Y: y, Z: z},
		Health:    maxHP,
		MaxHealth: maxHP,
	}
}

// defaultMaxHealth returns the base max health for all entity types.
// Unknown types default to 20 (10 hearts).
func defaultMaxHealth(t EntityType) float32 {
	switch t {
	// Passive — low health
	case TypeChicken, TypeCod, TypeSalmon, TypeTropicalFish, TypePufferfish:
		return 3
	case TypeRabbit, TypeTadpole:
		return 3
	case TypeBat:
		return 6
	case TypeParrot:
		return 6
	case TypeOcelot, TypeCat:
		return 10
	case TypeSheep, TypeDolphin:
		return 8
	case TypeCow, TypeMooshroom, TypePig, TypeDonkey, TypeMule,
		TypeGlowSquid, TypeSquid, TypeAllay, TypeArmadillo:
		return 10
	case TypeFox:
		return 10
	case TypeGoat:
		return 10
	case TypePanda:
		return 20
	case TypeLlama, TypeTraderLlama:
		return 22 // average of 15–30 range
	case TypeHorse, TypeSkeletonHorse, TypeZombieHorse:
		return 26 // average of 15–30 range
	case TypeCamel:
		return 32
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
	case TypeZombie, TypeSkeleton, TypeCreeper, TypeEnderman,
		TypeSpider, TypeHusk, TypeDrowned, TypePhantom, TypeVex,
		TypePiglin, TypePillager, TypeVindicator, TypeWitch, TypeBlaze,
		TypeStray, TypeBogged, TypeZombieVillager, TypeZoglin:
		return 20
	case TypeCaveSpider:
		return 12
	case TypeSilverfish, TypeSlime, TypeEndermite:
		return 8
	case TypeGuardian:
		return 30
	case TypeElderGuardian:
		return 80
	case TypeGhast:
		return 10
	case TypeMagmaCube:
		return 16
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

	// Boats
	case TypeOakBoat, TypeSpruceBoat, TypeBirchBoat, TypeJungleBoat,
		TypeAcaciaBoat, TypeDarkOakBoat, TypeMangroveBoat, TypeCherryBoat,
		TypeBambooRaft, TypeOakChestBoat, TypeSpruceChestBoat, TypeBirchChestBoat,
		TypeJungleChestBoat, TypeAcaciaChestBoat, TypeDarkOakChestBoat,
		TypeMangroveChestBoat, TypeCherryChestBoat, TypeBambooChestRaft:
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
