package server

import (
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"
)

const (
	brewingSlotCount     = 5
	brewingBottleSlots   = 3
	brewingIngredientSlot = 3
	brewingFuelSlot      = 4
	brewingTicksPerBrew  = 400
	brewingFuelPerPowder = 20
)

type brewingState struct {
	BrewTime   int // counts down from 400 to 0 while active
	FuelAmount int // remaining brews from last blaze powder (0-20)
}

type brewingKey struct {
	Dimension int32
	Position  spatial.BlockPos
}

func loadBrewingSlots(w *coreworld.World, pos spatial.BlockPos) []player.ItemStack {
	slots := make([]player.ItemStack, brewingSlotCount)
	for _, item := range w.ContainerItems(int(pos.X), int(pos.Y), int(pos.Z)) {
		if item.Slot >= 0 && item.Slot < brewingSlotCount && item.ItemID != "" && item.Count > 0 {
			slots[item.Slot] = item.Stack()
		}
	}
	return slots
}

func persistBrewingSlots(w *coreworld.World, pos spatial.BlockPos, slots []player.ItemStack) {
	items := make([]coreworld.ContainerItem, 0, brewingSlotCount)
	for slot := 0; slot < brewingSlotCount && slot < len(slots); slot++ {
		stack := slots[slot]
		if !stack.IsEmpty() {
			items = append(items, coreworld.ContainerItemFromStack(slot, stack))
		}
	}
	w.SetContainerItems(int(pos.X), int(pos.Y), int(pos.Z), "minecraft:brewing_stand", items)
}

func (s *Server) brewingStateForDimension(dimension int32, pos spatial.BlockPos) *brewingState {
	if s.brewingStands == nil {
		s.brewingStands = make(map[brewingKey]*brewingState)
	}
	key := brewingKey{Dimension: dimension, Position: pos}
	state := s.brewingStands[key]
	if state == nil {
		state = &brewingState{}
		s.brewingStands[key] = state
	}
	return state
}

// canBrew returns true if the current slots have a valid brewing setup:
// at least one bottle slot can receive the ingredient's transformation,
// and there is fuel.
func canBrew(slots []player.ItemStack) bool {
	if len(slots) < brewingSlotCount {
		return false
	}
	ingredient := slots[brewingIngredientSlot]
	if ingredient.IsEmpty() {
		return false
	}
	for i := 0; i < brewingBottleSlots; i++ {
		bottle := slots[i]
		if bottle.IsEmpty() {
			continue
		}
		_, _, ok := coreworld.BrewingResult(bottle.ItemID, ingredient.ItemID, bottle.Components)
		if ok {
			return true
		}
	}
	return false
}

// applyBrew transforms the three bottle slots using the ingredient and returns
// whether any slot was actually changed.
func applyBrew(slots []player.ItemStack) bool {
	if len(slots) < brewingSlotCount {
		return false
	}
	ingredient := slots[brewingIngredientSlot]
	if ingredient.IsEmpty() {
		return false
	}
	changed := false
	for i := 0; i < brewingBottleSlots; i++ {
		bottle := slots[i]
		if bottle.IsEmpty() {
			continue
		}
		outItem, outPotion, ok := coreworld.BrewingResult(bottle.ItemID, ingredient.ItemID, bottle.Components)
		if !ok {
			continue
		}
		bottle.ItemID = outItem
		if outPotion != "" {
			_ = bottle.SetComponent("potion_contents", map[string]string{"potion": "minecraft:" + outPotion})
		}
		slots[i] = bottle
		changed = true
	}
	if changed {
		slots[brewingIngredientSlot].Count--
		if slots[brewingIngredientSlot].Count <= 0 {
			slots[brewingIngredientSlot] = player.ItemStack{}
		}
	}
	return changed
}

func (s *Server) tickBrewingStands() {
	// Register an in-progress state for any player who has a brewing stand open.
	s.game.OnlinePlayers(func(p *player.Player) {
		if p.OpenContainerKind == "minecraft:brewing_stand" {
			s.brewingStateForDimension(p.Dimension, p.OpenContainerPos)
		}
	})

	for key, state := range s.brewingStands {
		pos := key.Position
		dimensionWorld := s.worldForDimension(key.Dimension)
		block := dimensionWorld.GetBlock(int(pos.X), int(pos.Y), int(pos.Z))
		if block.ResourceLocation() != "minecraft:brewing_stand" {
			delete(s.brewingStands, key)
			continue
		}

		slots := loadBrewingSlots(dimensionWorld, pos)
		before := make([]player.ItemStack, brewingSlotCount)
		copy(before, slots)

		// Consume blaze powder into fuel.
		if state.FuelAmount == 0 && !slots[brewingFuelSlot].IsEmpty() &&
			slots[brewingFuelSlot].ItemID == "minecraft:blaze_powder" {
			state.FuelAmount = brewingFuelPerPowder
			slots[brewingFuelSlot].Count--
			if slots[brewingFuelSlot].Count <= 0 {
				slots[brewingFuelSlot] = player.ItemStack{}
			}
		}

		active := state.FuelAmount > 0 && canBrew(slots)

		if active {
			if state.BrewTime == 0 {
				state.BrewTime = brewingTicksPerBrew
			}
			state.BrewTime--
			if state.BrewTime == 0 {
				applyBrew(slots)
				state.FuelAmount--
			}
		} else {
			state.BrewTime = 0
		}

		slotsChanged := false
		for i := range before {
			if before[i] != slots[i] {
				slotsChanged = true
				break
			}
		}
		if slotsChanged {
			persistBrewingSlots(dimensionWorld, pos, slots)
		}

		s.game.OnlinePlayers(func(p *player.Player) {
			if p.Dimension != key.Dimension ||
				p.OpenContainerKind != "minecraft:brewing_stand" ||
				p.OpenContainerPos != pos {
				return
			}
			if slotsChanged {
				p.ContainerStateID++
				p.ContainerSlots = append(p.ContainerSlots[:0], slots...)
			}
			if p.Edition == player.ClientEditionBedrock && s.bedrockListener != nil {
				s.bedrockListener.SyncBrewingContainer(p, state.BrewTime, state.FuelAmount)
			}
			if p.Edition == player.ClientEditionJava {
				if sess, ok := s.sessions.Get(p.UUID); ok {
					if slotsChanged {
						_ = handler.SyncWorkstationToConn(sess.Conn, p)
					}
					_ = handler.SyncBrewingContainer(sess.Conn, p, state.BrewTime, state.FuelAmount)
				}
			}
		})
	}
}
