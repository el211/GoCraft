package server

import (
	"strings"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"
)

func isBedrockGenericContainer(blockID string) bool {
	switch blockID {
	case "minecraft:chest", "minecraft:trapped_chest", "minecraft:barrel", "minecraft:ender_chest",
		"minecraft:hopper", "minecraft:dispenser", "minecraft:dropper", "minecraft:crafter":
		return true
	default:
		return blockID == "minecraft:shulker_box" || strings.HasSuffix(blockID, "_shulker_box")
	}
}

func bedrockGenericContainerSize(blockID string) int {
	switch blockID {
	case "minecraft:hopper":
		return 5
	case "minecraft:dispenser", "minecraft:dropper":
		return 9
	case "minecraft:crafter":
		return 9
	default:
		return 27
	}
}

func isBedrockWorkstation(blockID string) bool {
	return handler.IsWorkstation(blockID)
}

func bedrockWorkstationSlotCount(blockID string) int {
	return handler.WorkstationSlotCount(blockID)
}

func (s *Server) openBedrockWorkstation(p *player.Player, pos spatial.BlockPos, blockID string) {
	if p == nil || !isBedrockWorkstation(blockID) {
		return
	}
	p.OpenContainerID = 1
	p.OpenContainerKind = blockID
	p.OpenContainerPos = pos
	p.OpenContainerPartnerPos = spatial.BlockPos{}
	p.OpenContainerHasPartner = false
	p.ContainerSlots = make([]player.ItemStack, bedrockWorkstationSlotCount(blockID))
	p.WorkstationSelection = 0
	// Brewing stands persist items in the block entity; load them now.
	if blockID == "minecraft:brewing_stand" {
		w := s.worldForPlayer(p)
		for _, ci := range w.ContainerItems(int(pos.X), int(pos.Y), int(pos.Z)) {
			if ci.Slot >= 0 && ci.Slot < len(p.ContainerSlots) {
				p.ContainerSlots[ci.Slot] = ci.Stack()
			}
		}
	}
	handler.UpdateWorkstationResult(blockID, p.ContainerSlots, p.WorkstationSelection)
}

func (s *Server) returnBedrockWorkstationItems(p *player.Player) {
	if p == nil || !isBedrockWorkstation(p.OpenContainerKind) {
		return
	}
	// Brewing stand items are persistent — save them back to block entity.
	if p.OpenContainerKind == "minecraft:brewing_stand" {
		items := make([]coreworld.ContainerItem, 0, len(p.ContainerSlots))
		for slot, stack := range p.ContainerSlots {
			if !stack.IsEmpty() {
				items = append(items, coreworld.ContainerItemFromStack(slot, stack))
			}
		}
		w := s.worldForPlayer(p)
		w.SetContainerItems(int(p.OpenContainerPos.X), int(p.OpenContainerPos.Y), int(p.OpenContainerPos.Z), p.OpenContainerKind, items)
		return
	}
	for index, stack := range p.ContainerSlots {
		if stack.IsEmpty() || bedrockWorkstationOutputIndex(p.OpenContainerKind) == index {
			continue
		}
		if !p.GiveItem(stack) {
			if dropped := s.newDroppedItemForPlayer(p, stack, p.Position, index); dropped != nil && p.Dimension == dimensionOverworld {
				handler.BroadcastSpawnMob(dropped, s.sessions)
			}
		}
	}
}

func bedrockWorkstationOutputIndex(blockID string) int {
	return handler.WorkstationOutputIndex(blockID)
}

func (s *Server) openBedrockGenericContainer(p *player.Player, pos spatial.BlockPos, blockID string) {
	if p == nil || s == nil || s.worldForPlayer(p) == nil {
		return
	}
	dimensionWorld := s.worldForPlayer(p)
	if blockID == "minecraft:chest" || blockID == "minecraft:trapped_chest" {
		handler.LoadChestContainerState(p, dimensionWorld, pos)
		return
	}
	p.OpenContainerID = 1
	p.OpenContainerKind = blockID
	p.OpenContainerPos = pos
	p.OpenContainerPartnerPos = spatial.BlockPos{}
	p.OpenContainerHasPartner = false
	p.ContainerSlots = make([]player.ItemStack, bedrockGenericContainerSize(blockID))
	if blockID == "minecraft:ender_chest" {
		copy(p.ContainerSlots, p.EnderChestInventory[:])
		return
	}
	for _, item := range dimensionWorld.ContainerItems(int(pos.X), int(pos.Y), int(pos.Z)) {
		if item.Slot < 0 || item.Slot >= len(p.ContainerSlots) || item.ItemID == "" || item.Count <= 0 {
			continue
		}
		p.ContainerSlots[item.Slot] = item.Stack()
	}
}

func (s *Server) persistBedrockGenericContainer(p *player.Player) {
	if p == nil || s == nil || s.worldForPlayer(p) == nil || !isBedrockGenericContainer(p.OpenContainerKind) {
		return
	}
	if p.OpenContainerKind == "minecraft:chest" || p.OpenContainerKind == "minecraft:trapped_chest" {
		handler.PersistChestContents(p, s.worldForPlayer(p))
		return
	}
	if p.OpenContainerKind == "minecraft:ender_chest" {
		clear(p.EnderChestInventory[:])
		copy(p.EnderChestInventory[:], p.ContainerSlots)
		return
	}
	items := make([]coreworld.ContainerItem, 0, len(p.ContainerSlots))
	for slot, stack := range p.ContainerSlots {
		if stack.IsEmpty() {
			continue
		}
		items = append(items, coreworld.ContainerItemFromStack(slot, stack))
	}
	pos := p.OpenContainerPos
	s.worldForPlayer(p).SetContainerItems(int(pos.X), int(pos.Y), int(pos.Z), p.OpenContainerKind, items)
}
