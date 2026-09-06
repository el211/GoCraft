package server

import (
	"math"
	"math/rand"

	corentity "GoCraft/core/entity"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/java/handler"
)

func (s *Server) applyVehicleMove(move intent.VehicleMoveIntent) {
	p := s.game.GetPlayer(move.PlayerUUID)
	if p == nil || p.VehicleEntityID == 0 || p.Dead {
		return
	}
	vehicle, ok := s.world.Entities.Get(p.VehicleEntityID)
	if !ok || !corentity.IsRideableVehicle(vehicle.Type) || vehicle.RiderEntityID != p.EntityID {
		p.VehicleEntityID = 0
		return
	}
	if math.Abs(move.Position.X-vehicle.Position.X) > 3 ||
		math.Abs(move.Position.Y-vehicle.Position.Y) > 3 || math.Abs(move.Position.Z-vehicle.Position.Z) > 3 {
		return
	}
	vehicle.Position = move.Position
	vehicle.Yaw = move.Yaw
	vehicle.OnGround = move.OnGround
	p.Position = move.Position
	handler.BroadcastEntityPosition(vehicle, s.sessions)
}

func (s *Server) interactionRNG() *rand.Rand {
	if s.spawnRNG == nil {
		s.spawnRNG = rand.New(rand.NewSource(1))
	}
	return s.spawnRNG
}

func (s *Server) consumeAnimalItem(p *player.Player, replacement string) bool {
	if p == nil || p.GameMode == player.GameModeCreative {
		return true
	}
	slotIndex := player.HotbarStart + p.HeldSlot
	if slotIndex < 0 || slotIndex >= len(p.Inventory) || p.Inventory[slotIndex].IsEmpty() {
		return false
	}
	p.Inventory[slotIndex].Count--
	if p.Inventory[slotIndex].Count <= 0 {
		p.Inventory[slotIndex] = player.ItemStack{}
	}
	if replacement != "" {
		_ = p.GiveItem(player.ItemStack{ItemID: replacement, Count: 1})
	}
	p.ContainerStateID++
	return true
}

func (s *Server) syncPlayerInventory(p *player.Player) {
	if p == nil || p.Edition != player.ClientEditionJava || s.sessions == nil {
		return
	}
	handler.SendPlayerInventory(p, s.sessions)
}

func animalOwnedBy(e *corentity.Entity, p *player.Player) bool {
	return e != nil && p != nil && e.HasTameOwner && e.TameOwnerUUID == p.UUID
}

func isHorseFamily(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeHorse, corentity.TypeDonkey, corentity.TypeMule,
		corentity.TypeSkeletonHorse, corentity.TypeZombieHorse,
		corentity.TypeLlama, corentity.TypeTraderLlama:
		return true
	default:
		return false
	}
}

func saddleApplicable(t corentity.EntityType) bool {
	switch t {
	case corentity.TypeCamel, corentity.TypeDonkey, corentity.TypeHorse, corentity.TypeMule,
		corentity.TypePig, corentity.TypeSkeletonHorse, corentity.TypeStrider,
		corentity.TypeZombieHorse:
		return true
	default:
		return false
	}
}

func (s *Server) setAnimalOwner(e *corentity.Entity, p *player.Player) {
	e.Tamed = true
	e.HasTameOwner = true
	e.TameOwnerUUID = p.UUID
	e.TameOwnerEntityID = p.EntityID
}

func (s *Server) broadcastAnimalState(e *corentity.Entity) {
	if e == nil {
		return
	}
	handler.BroadcastMobMetadata(e, s.sessions)
}

func (s *Server) broadcastAnimalEvent(e *corentity.Entity, javaStatus byte, bedrockEvent byte) {
	if e == nil {
		return
	}
	if javaStatus != 0 {
		handler.BroadcastEntityStatus(e.EntityID, javaStatus, s.sessions)
	}
	if s.bedrockListener != nil && bedrockEvent != 0 {
		s.bedrockListener.BroadcastActorEvent(e.EntityID, bedrockEvent)
	}
}

// interactAnimal applies feeding/taming/saddling/mounting on the simulation
// thread. It returns true when the player's held inventory may have changed.
func (s *Server) interactAnimal(p *player.Player, e *corentity.Entity) bool {
	if p == nil || e == nil || e.Dead || p.Dead || p.GameMode == player.GameModeSpectator {
		return false
	}
	held := p.HeldItem()
	item := held.ItemID

	// Apply a name tag: sets the entity's custom name and consumes the tag.
	if item == "minecraft:name_tag" {
		name := held.DisplayName()
		if name != "" {
			if !s.consumeAnimalItem(p, "") {
				return false
			}
			e.DisplayName = name
			e.CustomNameVisible = true
			s.broadcastAnimalState(e)
			return true
		}
	}

	// Shear a sheep: drops 1-3 wool and marks the sheep as sheared.
	if item == "minecraft:shears" && e.Type == corentity.TypeSheep && !e.Sheared && !e.IsBaby {
		woolColor := e.WoolColor
		if woolColor == "" {
			woolColor = "white"
		}
		woolItem := "minecraft:" + woolColor + "_wool"
		woolCount := 1 + s.interactionRNG().Intn(3)
		drop := player.ItemStack{ItemID: woolItem, Count: woolCount}
		if !p.GiveItem(drop) {
			s.newDroppedItemForPlayer(p, drop, e.Position, 0)
		}
		e.Sheared = true
		e.WoolRegrowTicks = 300 // ~15 s at 20 tps; regrow on next grass-eat opportunity
		s.broadcastAnimalState(e)
		s.damageBedrockHeldItem(p, 1)
		return true
	}

	// Shear a mooshroom: drops 5 mushrooms and converts to a plain cow.
	if item == "minecraft:shears" && e.Type == corentity.TypeMooshroom && !e.IsBaby {
		mushroom := "minecraft:red_mushroom"
		if e.WoolColor == "brown" {
			mushroom = "minecraft:brown_mushroom"
		}
		drop := player.ItemStack{ItemID: mushroom, Count: 5}
		if !p.GiveItem(drop) {
			s.newDroppedItemForPlayer(p, drop, e.Position, 0)
		}
		// Convert mooshroom to a plain cow.
		world := s.worldForPlayer(p)
		world.Entities.Remove(e.EntityID)
		handler.BroadcastRemoveEntity(e.EntityID, s.sessions)
		if s.game != nil {
			cow := corentity.New(s.game.NextEntityID(), newRandomUUID(), corentity.TypeCow,
				e.Position.X, e.Position.Y, e.Position.Z)
			cow.Health = e.Health
			cow.MaxHealth = e.MaxHealth
			cow.IsBaby = e.IsBaby
			cow.OnGround = e.OnGround
			world.Entities.Add(cow)
			handler.BroadcastSpawnMob(cow, s.sessions)
		}
		s.damageBedrockHeldItem(p, 1)
		return true
	}

	// Dye a sheep: applies the dye colour and consumes one dye.
	if e.Type == corentity.TypeSheep {
		if color := sheepDyeColor(item); color != "" {
			if !s.consumeAnimalItem(p, "") {
				return false
			}
			e.WoolColor = color
			s.broadcastAnimalState(e)
			return true
		}
	}

	// Capture aquatic mobs with a water bucket.
	if item == "minecraft:water_bucket" && !e.IsBaby {
		if bucket := fishBucketForType(e.Type); bucket != "" {
			if !s.consumeAnimalItem(p, bucket) {
				return false
			}
			// Remove the entity from the world.
			world := s.worldForPlayer(p)
			world.Entities.Remove(e.EntityID)
			handler.BroadcastRemoveEntity(e.EntityID, s.sessions)
			return true
		}
	}

	// Milk a cow or mooshroom: replaces one bucket in hand with a milk bucket.
	if item == "minecraft:bucket" && !e.IsBaby &&
		(e.Type == corentity.TypeCow || e.Type == corentity.TypeMooshroom) {
		s.consumeAnimalItem(p, "minecraft:milk_bucket")
		return true
	}

	// Bowl on mooshroom → mushroom stew.
	if item == "minecraft:bowl" && !e.IsBaby && e.Type == corentity.TypeMooshroom {
		s.consumeAnimalItem(p, "minecraft:mushroom_stew")
		return true
	}

	if item == "minecraft:saddle" && saddleApplicable(e.Type) && !e.Saddled && !e.IsBaby {
		if isHorseFamily(e.Type) && !e.Tamed {
			return false
		}
		if !s.consumeAnimalItem(p, "") {
			return false
		}
		e.Saddled = true
		s.broadcastAnimalState(e)
		return true
	}

	effect := corentity.FoodEffect(e.Type, item, e.Tamed)
	if effect.Accepted {
		if effect.Poisons {
			if !s.consumeAnimalItem(p, "") {
				return false
			}
			// Pumpkin applies poison for 900 ticks and then lethal maximum damage.
			e.PoisonTicks = 900
			return true
		}
		if effect.Taming && !e.Tamed {
			if !s.consumeAnimalItem(p, "") {
				return false
			}
			chance := 3
			if e.Type == corentity.TypeParrot {
				chance = 10
			}
			if s.interactionRNG().Intn(chance) == 0 {
				if e.Type == corentity.TypeOcelot {
					e.Trusting = true
					s.broadcastAnimalState(e)
					s.broadcastAnimalEvent(e, 7, 7)
					return true
				}
				s.setAnimalOwner(e, p)
				e.Sitting = e.Type == corentity.TypeWolf || e.Type == corentity.TypeCat || e.Type == corentity.TypeParrot
				s.broadcastAnimalState(e)
				s.broadcastAnimalEvent(e, 7, 7) // Java/Bedrock taming succeeded.
			} else {
				s.broadcastAnimalEvent(e, 6, 6) // Java/Bedrock taming failed.
			}
			return true
		}

		acted := false
		if e.IsBaby {
			growth := effect.GrowthTicks
			if growth <= 0 {
				growth = e.RemainingBabyTicks() / 10
				if growth < 1 {
					growth = 1
				}
			}
			e.BabyAgeTicks += growth
			if e.BabyAgeTicks >= corentity.BabyGrowUpTicks {
				e.BabyAgeTicks = corentity.BabyGrowUpTicks
			}
			acted = true
		} else if effect.Breeding && e.BreedingCooldownTicks == 0 && e.LoveTicks == 0 {
			if (e.Type != corentity.TypeWolf && e.Type != corentity.TypeCat) || animalOwnedBy(e, p) {
				e.LoveTicks = corentity.LoveDurationTicks
				e.LoveCauseUUID = p.UUID
				e.HasLoveCause = true
				e.BreedingMateEntityID = 0
				e.BreedingProgressTicks = 0
				acted = true
			}
		}
		if effect.Heal > 0 && e.Health < e.MaxHealth {
			e.Heal(effect.Heal)
			acted = true
		}
		if effect.TemperIncrease > 0 && e.Temper < 100 {
			e.Temper += effect.TemperIncrease
			if e.Temper > 100 {
				e.Temper = 100
			}
			acted = true
		}
		if acted {
			replacement := ""
			if item == "minecraft:tropical_fish_bucket" {
				replacement = "minecraft:water_bucket"
			}
			if !s.consumeAnimalItem(p, replacement) {
				return false
			}
			s.broadcastAnimalState(e)
			if e.LoveTicks > 0 {
				s.broadcastAnimalEvent(e, 18, 26) // In-love hearts.
			} else if s.bedrockListener != nil {
				s.bedrockListener.BroadcastActorEvent(e.EntityID, 57) // Feed.
			}
			return true
		}
	}

	if e.Tamed && animalOwnedBy(e, p) {
		switch e.Type {
		case corentity.TypeWolf, corentity.TypeCat, corentity.TypeParrot:
			e.Sitting = !e.Sitting
			s.broadcastAnimalState(e)
			return false
		}
	}

	if isHorseFamily(e.Type) && !e.Tamed && !e.IsBaby {
		// Vanilla compares a random roll with temper and adds five temper after a
		// failed ride. The actual buck animation is represented by the failure event.
		if int32(s.interactionRNG().Intn(100)) < e.Temper {
			s.setAnimalOwner(e, p)
			s.broadcastAnimalState(e)
			s.broadcastAnimalEvent(e, 7, 7)
		} else {
			e.Temper += 5
			if e.Temper > 100 {
				e.Temper = 100
			}
			s.broadcastAnimalEvent(e, 6, 6)
		}
		s.mountPlayer(p, e)
		return false
	}

	if corentity.IsAnimalVehicle(e.Type) && !e.IsBaby {
		if (e.Type == corentity.TypePig || e.Type == corentity.TypeStrider) && !e.Saddled {
			return false
		}
		if isHorseFamily(e.Type) && !e.Tamed {
			return false
		}
		s.mountPlayer(p, e)
	}
	return false
}

func (s *Server) mountPlayer(p *player.Player, vehicle *corentity.Entity) bool {
	if p == nil || vehicle == nil || p.VehicleEntityID != 0 || !corentity.IsRideableVehicle(vehicle.Type) {
		return false
	}
	if !vehicle.AddPassenger(p.EntityID) {
		return false
	}
	p.VehicleEntityID = vehicle.EntityID
	p.Position = vehicle.Position
	handler.BroadcastSetPassengers(vehicle.EntityID, vehicle.PassengerIDs(), s.sessions)
	return true
}

func (s *Server) dismountPlayer(p *player.Player) bool {
	if p == nil || p.VehicleEntityID == 0 {
		return false
	}
	vehicleID := p.VehicleEntityID
	p.VehicleEntityID = 0
	if vehicle, ok := s.world.Entities.Get(vehicleID); ok {
		vehicle.RemovePassenger(p.EntityID)
		p.Position.X = vehicle.Position.X + 1.5
		p.Position.Y = vehicle.Position.Y
		p.Position.Z = vehicle.Position.Z
		handler.BroadcastSetPassengers(vehicleID, vehicle.PassengerIDs(), s.sessions)
		if p.Edition == player.ClientEditionJava && s.sessions != nil {
			if current, found := s.sessions.Get(p.UUID); found && current.TeleportTo != nil {
				_ = current.TeleportTo(p.Position.X, p.Position.Y, p.Position.Z)
			}
		}
		return true
	}
	handler.BroadcastSetPassengers(vehicleID, nil, s.sessions)
	return true
}

// fishBucketForType returns the filled bucket item ID for capturable aquatic mobs, or "".
func fishBucketForType(t corentity.EntityType) string {
	switch t {
	case corentity.TypeCod:
		return "minecraft:cod_bucket"
	case corentity.TypeSalmon:
		return "minecraft:salmon_bucket"
	case corentity.TypePufferfish:
		return "minecraft:pufferfish_bucket"
	case corentity.TypeTropicalFish:
		return "minecraft:tropical_fish_bucket"
	case corentity.TypeAxolotl:
		return "minecraft:axolotl_bucket"
	case corentity.TypeTadpole:
		return "minecraft:tadpole_bucket"
	default:
		return ""
	}
}

// sheepDyeColor returns the canonical colour name if itemID is a dye, else "".
func sheepDyeColor(itemID string) string {
	const prefix = "minecraft:"
	const suffix = "_dye"
	if len(itemID) <= len(prefix)+len(suffix) {
		return ""
	}
	if itemID[:len(prefix)] != prefix {
		return ""
	}
	tail := itemID[len(prefix):]
	if len(tail) <= len(suffix) || tail[len(tail)-len(suffix):] != suffix {
		return ""
	}
	return tail[:len(tail)-len(suffix)]
}

func (s *Server) dismountEntityPassengers(vehicle *corentity.Entity) {
	if vehicle == nil || s.game == nil {
		return
	}
	for _, passengerID := range vehicle.PassengerIDs() {
		var passenger *player.Player
		s.game.OnlinePlayers(func(candidate *player.Player) {
			if candidate.EntityID == passengerID {
				passenger = candidate
			}
		})
		if passenger != nil {
			s.dismountPlayer(passenger)
		} else {
			vehicle.RemovePassenger(passengerID)
		}
	}
	handler.BroadcastSetPassengers(vehicle.EntityID, nil, s.sessions)
}
