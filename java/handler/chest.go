package handler

import (
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
)

const (
	chestContainerID int32 = 1
	chestSlotCount         = 27
)

func openChest(p *player.Player, conn *network.ClientConn, w *coreworld.World, pos spatial.BlockPos) error {
	if p.OpenContainerKind == "minecraft:crafting_table" {
		returnCraftingGrid(p)
	}
	p.OpenContainerID = chestContainerID
	p.OpenContainerKind = "minecraft:chest"
	p.OpenContainerPos = pos
	p.ContainerSlots = make([]player.ItemStack, chestSlotCount)
	for _, item := range w.ContainerItems(int(pos.X), int(pos.Y), int(pos.Z)) {
		if item.Slot < 0 || item.Slot >= len(p.ContainerSlots) || item.ItemID == "" || item.Count <= 0 {
			continue
		}
		p.ContainerSlots[item.Slot] = player.ItemStack{ItemID: item.ItemID, Count: item.Count}
	}
	p.ContainerStateID++
	persistChestContents(p, w)
	if err := sendOpenScreen(conn, chestContainerID, containerMenuType("minecraft:chest"), "Chest"); err != nil {
		return err
	}
	return sendChestContainerContent(conn, p)
}

func sendChestContainerContent(conn *network.ClientConn, p *player.Player) error {
	b := protocol.NewBuilder(packetIDSetContainerContent).
		VarInt(chestContainerID).
		VarInt(p.ContainerStateID).
		VarInt(chestSlotCount + 36)
	for i := 0; i < chestSlotCount; i++ {
		if i < len(p.ContainerSlots) {
			encodeSlot(b, p.ContainerSlots[i])
		} else {
			encodeSlot(b, player.ItemStack{})
		}
	}
	for i := 9; i < player.HotbarStart; i++ {
		encodeSlot(b, p.Inventory[i])
	}
	for i := player.HotbarStart; i < player.HotbarStart+9; i++ {
		encodeSlot(b, p.Inventory[i])
	}
	encodeSlot(b, p.CarriedItem)
	return conn.WritePacket(b.Build())
}

func handleChestClick(p *player.Player, w *coreworld.World, slot int, button byte, mode int32) {
	switch mode {
	case 0:
		clickChestSlot(p, slot, button)
	case 1:
		shiftChestSlot(p, slot)
	}
	p.ContainerStateID++
	persistChestContents(p, w)
}

func clickChestSlot(p *player.Player, containerSlot int, button byte) {
	if containerSlot == -999 {
		if button == 0 {
			p.CarriedItem = player.ItemStack{}
		} else if !p.CarriedItem.IsEmpty() {
			p.CarriedItem.Count--
			normalizeStack(&p.CarriedItem)
		}
		return
	}
	target := chestContainerSlot(p, containerSlot)
	if target == nil {
		return
	}
	if button == 0 {
		switch {
		case p.CarriedItem.IsEmpty():
			p.CarriedItem, *target = *target, player.ItemStack{}
		case target.IsEmpty():
			*target, p.CarriedItem = p.CarriedItem, player.ItemStack{}
		case target.ItemID == p.CarriedItem.ItemID && target.Count < 64:
			add := minInt(64-target.Count, p.CarriedItem.Count)
			target.Count += add
			p.CarriedItem.Count -= add
			normalizeStack(&p.CarriedItem)
		default:
			p.CarriedItem, *target = *target, p.CarriedItem
		}
		return
	}
	if p.CarriedItem.IsEmpty() {
		if target.IsEmpty() {
			return
		}
		take := (target.Count + 1) / 2
		p.CarriedItem = player.ItemStack{ItemID: target.ItemID, Count: take}
		target.Count -= take
		normalizeStack(target)
	} else if target.IsEmpty() {
		*target = player.ItemStack{ItemID: p.CarriedItem.ItemID, Count: 1}
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
	} else if target.ItemID == p.CarriedItem.ItemID && target.Count < 64 {
		target.Count++
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
	}
}

func shiftChestSlot(p *player.Player, containerSlot int) {
	target := chestContainerSlot(p, containerSlot)
	if target == nil || target.IsEmpty() {
		return
	}
	if containerSlot < chestSlotCount {
		inventory := p.Inventory
		if addStackToInventory(&inventory, *target) {
			p.Inventory = inventory
			*target = player.ItemStack{}
		}
		return
	}
	slots := append([]player.ItemStack(nil), p.ContainerSlots...)
	if addStackToContainer(slots, *target) {
		p.ContainerSlots = slots
		*target = player.ItemStack{}
	}
}

func addStackToContainer(slots []player.ItemStack, item player.ItemStack) bool {
	if item.IsEmpty() {
		return true
	}
	capacity := 0
	for _, slot := range slots {
		switch {
		case slot.IsEmpty():
			capacity += 64
		case slot.ItemID == item.ItemID && slot.Count < 64:
			capacity += 64 - slot.Count
		}
	}
	if capacity < item.Count {
		return false
	}
	remaining := item.Count
	for i := range slots {
		if slots[i].ItemID != item.ItemID || slots[i].Count >= 64 {
			continue
		}
		add := minInt(64-slots[i].Count, remaining)
		slots[i].Count += add
		remaining -= add
		if remaining == 0 {
			return true
		}
	}
	for i := range slots {
		if !slots[i].IsEmpty() {
			continue
		}
		add := minInt(64, remaining)
		slots[i] = player.ItemStack{ItemID: item.ItemID, Count: add}
		remaining -= add
		if remaining == 0 {
			return true
		}
	}
	return remaining == 0
}

func chestContainerSlot(p *player.Player, containerSlot int) *player.ItemStack {
	switch {
	case containerSlot >= 0 && containerSlot < chestSlotCount:
		if containerSlot >= len(p.ContainerSlots) {
			return nil
		}
		return &p.ContainerSlots[containerSlot]
	case containerSlot >= chestSlotCount && containerSlot < chestSlotCount+27:
		return &p.Inventory[9+containerSlot-chestSlotCount]
	case containerSlot >= chestSlotCount+27 && containerSlot < chestSlotCount+36:
		return &p.Inventory[player.HotbarStart+containerSlot-(chestSlotCount+27)]
	default:
		return nil
	}
}

func persistChestContents(p *player.Player, w *coreworld.World) {
	items := make([]coreworld.ContainerItem, 0, len(p.ContainerSlots))
	for slot, stack := range p.ContainerSlots {
		if stack.IsEmpty() {
			continue
		}
		items = append(items, coreworld.ContainerItem{Slot: slot, ItemID: stack.ItemID, Count: stack.Count})
	}
	pos := p.OpenContainerPos
	w.SetContainerItems(int(pos.X), int(pos.Y), int(pos.Z), "minecraft:chest", items)
}
