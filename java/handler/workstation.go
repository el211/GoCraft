package handler

import (
	"GoCraft/core/itemregistry"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
)

const workstationContainerID int32 = 1

func openWorkstation(p *player.Player, conn *network.ClientConn, w *coreworld.World, pos spatial.BlockPos, kind string) error {
	if p == nil || !IsWorkstation(kind) {
		return nil
	}
	if p.OpenContainerKind == "minecraft:crafting_table" {
		returnCraftingGrid(p)
	} else if IsWorkstation(p.OpenContainerKind) {
		returnWorkstationItems(p)
	}
	p.OpenContainerID = workstationContainerID
	p.OpenContainerKind = kind
	p.OpenContainerPos = pos
	p.OpenContainerPartnerPos = spatial.BlockPos{}
	p.OpenContainerHasPartner = false
	p.WorkstationSelection = 0
	p.ContainerSlots = make([]player.ItemStack, WorkstationSlotCount(kind))
	// Brewing stands store their slots persistently in the world as a block
	// entity. Load those items now so they appear in the opened screen.
	if kind == "minecraft:brewing_stand" && w != nil {
		for _, ci := range w.ContainerItems(int(pos.X), int(pos.Y), int(pos.Z)) {
			if ci.Slot >= 0 && ci.Slot < len(p.ContainerSlots) {
				p.ContainerSlots[ci.Slot] = ci.Stack()
			}
		}
	}
	UpdateWorkstationResult(kind, p.ContainerSlots, p.WorkstationSelection)
	p.ContainerStateID++
	if conn == nil {
		return nil
	}
	if err := sendOpenScreen(conn, workstationContainerID, containerMenuType(kind), containerTitle(kind)); err != nil {
		return err
	}
	return sendChestContainerContent(conn, p)
}

func handleWorkstationClick(p *player.Player, slot int, button byte, mode int32) {
	if p == nil || !IsWorkstation(p.OpenContainerKind) {
		return
	}
	UpdateWorkstationResult(p.OpenContainerKind, p.ContainerSlots, p.WorkstationSelection)
	output := WorkstationOutputIndex(p.OpenContainerKind)
	switch mode {
	case 0:
		if slot == output {
			takeWorkstationOutputToCursor(p)
		} else {
			clickWorkstationSlot(p, slot, button)
		}
	case 1:
		if slot == output {
			shiftWorkstationOutput(p)
		} else {
			shiftWorkstationSlot(p, slot)
		}
	}
	UpdateWorkstationResult(p.OpenContainerKind, p.ContainerSlots, p.WorkstationSelection)
	p.ContainerStateID++
}

func handleWorkstationButtonClick(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn) error {
	r := pkt.Reader()
	windowID, err := protocol.ReadVarInt(r)
	if err != nil {
		return err
	}
	buttonID, err := protocol.ReadVarInt(r)
	if err != nil {
		return err
	}
	if r.Len() != 0 || p == nil || windowID != workstationContainerID ||
		p.OpenContainerID != windowID || p.OpenContainerKind != "minecraft:stonecutter" || buttonID < 0 {
		return nil
	}
	p.WorkstationSelection = int(buttonID)
	UpdateWorkstationResult(p.OpenContainerKind, p.ContainerSlots, p.WorkstationSelection)
	p.ContainerStateID++
	if conn == nil {
		return nil
	}
	return sendChestContainerContent(conn, p)
}

func takeWorkstationOutputToCursor(p *player.Player) {
	output := WorkstationOutputIndex(p.OpenContainerKind)
	if output < 0 || output >= len(p.ContainerSlots) {
		return
	}
	preview := p.ContainerSlots[output]
	if preview.IsEmpty() || (!p.CarriedItem.IsEmpty() &&
		(p.CarriedItem.ItemID != preview.ItemID || p.CarriedItem.Damage != preview.Damage ||
			p.CarriedItem.Count+preview.Count > player.MaxStackSize(preview.ItemID))) {
		return
	}
	result, xp, ok := TakeWorkstationResult(p.OpenContainerKind, p.ContainerSlots, p.WorkstationSelection)
	if !ok {
		return
	}
	p.PendingWorkstationXP += xp
	if p.CarriedItem.IsEmpty() {
		p.CarriedItem = result
	} else {
		p.CarriedItem.Count += result.Count
	}
}

func shiftWorkstationOutput(p *player.Player) {
	for {
		output := WorkstationOutputIndex(p.OpenContainerKind)
		if output < 0 || output >= len(p.ContainerSlots) || p.ContainerSlots[output].IsEmpty() {
			return
		}
		inventory := p.Inventory
		if !addStackToInventory(&inventory, p.ContainerSlots[output]) {
			return
		}
		_, xp, ok := TakeWorkstationResult(p.OpenContainerKind, p.ContainerSlots, p.WorkstationSelection)
		if !ok {
			return
		}
		p.PendingWorkstationXP += xp
		p.Inventory = inventory
	}
}

func clickWorkstationSlot(p *player.Player, containerSlot int, button byte) {
	if containerSlot == -999 {
		clickChestSlot(p, containerSlot, button)
		return
	}
	target, workstationIndex := workstationContainerSlot(p, containerSlot)
	outputIndex := WorkstationOutputIndex(p.OpenContainerKind)
	if target == nil || (outputIndex >= 0 && workstationIndex == outputIndex) {
		return
	}
	if button == 0 {
		switch {
		case p.CarriedItem.IsEmpty():
			p.CarriedItem, *target = *target, player.ItemStack{}
		case target.IsEmpty() && canPlaceWorkstationSlot(p.OpenContainerKind, workstationIndex, p.CarriedItem):
			*target, p.CarriedItem = p.CarriedItem, player.ItemStack{}
		case target.SameItem(p.CarriedItem) &&
			canPlaceWorkstationSlot(p.OpenContainerKind, workstationIndex, p.CarriedItem) && target.Count < player.MaxStackSize(target.ItemID):
			add := minInt(player.MaxStackSize(target.ItemID)-target.Count, p.CarriedItem.Count)
			target.Count += add
			p.CarriedItem.Count -= add
			normalizeStack(&p.CarriedItem)
		case canPlaceWorkstationSlot(p.OpenContainerKind, workstationIndex, p.CarriedItem):
			p.CarriedItem, *target = *target, p.CarriedItem
		}
		return
	}
	if p.CarriedItem.IsEmpty() {
		if target.IsEmpty() {
			return
		}
		take := (target.Count + 1) / 2
		p.CarriedItem = *target
		p.CarriedItem.Count = take
		target.Count -= take
		normalizeStack(target)
	} else if target.IsEmpty() && canPlaceWorkstationSlot(p.OpenContainerKind, workstationIndex, p.CarriedItem) {
		*target = p.CarriedItem
		target.Count = 1
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
	} else if target.SameItem(p.CarriedItem) &&
		canPlaceWorkstationSlot(p.OpenContainerKind, workstationIndex, p.CarriedItem) && target.Count < player.MaxStackSize(target.ItemID) {
		target.Count++
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
	}
}

func shiftWorkstationSlot(p *player.Player, containerSlot int) {
	target, workstationIndex := workstationContainerSlot(p, containerSlot)
	outputIndex := WorkstationOutputIndex(p.OpenContainerKind)
	if target == nil || target.IsEmpty() || (outputIndex >= 0 && workstationIndex == outputIndex) {
		return
	}
	if workstationIndex >= 0 {
		inventory := p.Inventory
		if addStackToInventory(&inventory, *target) {
			p.Inventory = inventory
			*target = player.ItemStack{}
		}
		return
	}
	slots := append([]player.ItemStack(nil), p.ContainerSlots...)
	remaining := *target
	for index := range slots {
		if index == WorkstationOutputIndex(p.OpenContainerKind) || !canPlaceWorkstationSlot(p.OpenContainerKind, index, remaining) {
			continue
		}
		if slots[index].SameItem(remaining) && slots[index].Count < player.MaxStackSize(remaining.ItemID) {
			add := minInt(player.MaxStackSize(remaining.ItemID)-slots[index].Count, remaining.Count)
			slots[index].Count += add
			remaining.Count -= add
			if remaining.Count == 0 {
				break
			}
		}
	}
	for index := range slots {
		if remaining.Count == 0 {
			break
		}
		if index == WorkstationOutputIndex(p.OpenContainerKind) || !slots[index].IsEmpty() || !canPlaceWorkstationSlot(p.OpenContainerKind, index, remaining) {
			continue
		}
		add := minInt(player.MaxStackSize(remaining.ItemID), remaining.Count)
		slots[index] = remaining
		slots[index].Count = add
		remaining.Count -= add
	}
	if remaining.Count == 0 {
		p.ContainerSlots = slots
		*target = player.ItemStack{}
	}
}

// workstationContainerSlot returns a screen slot and its workstation index.
// Player inventory slots return an index of -1.
func workstationContainerSlot(p *player.Player, containerSlot int) (*player.ItemStack, int) {
	slotCount := len(p.ContainerSlots)
	switch {
	case containerSlot >= 0 && containerSlot < slotCount:
		return &p.ContainerSlots[containerSlot], containerSlot
	case containerSlot >= slotCount && containerSlot < slotCount+27:
		return &p.Inventory[9+containerSlot-slotCount], -1
	case containerSlot >= slotCount+27 && containerSlot < slotCount+36:
		return &p.Inventory[player.HotbarStart+containerSlot-(slotCount+27)], -1
	default:
		return nil, -2
	}
}

func canPlaceWorkstationSlot(kind string, index int, stack player.ItemStack) bool {
	if stack.IsEmpty() {
		return true
	}
	if index < 0 {
		return true
	}
	if index == WorkstationOutputIndex(kind) {
		return false
	}
	switch kind {
	case "minecraft:enchanting_table":
		return (index == 1 && stack.ItemID == "minecraft:lapis_lazuli") || index == 0
	case "minecraft:loom":
		return (index == 0 && itemregistry.HasTag(stack.ItemID, "minecraft:banners")) ||
			(index == 1 && itemregistry.HasTag(stack.ItemID, "gocraft:dyes")) ||
			(index == 2 && itemregistry.HasTag(stack.ItemID, "gocraft:banner_patterns"))
	case "minecraft:smithing_table":
		return smithingAccepts(index, stack.ItemID)
	case "minecraft:stonecutter":
		return index == 0
	case "minecraft:cartography_table":
		return (index == 0 && stack.ItemID == "minecraft:filled_map") ||
			(index == 1 && (stack.ItemID == "minecraft:paper" || stack.ItemID == "minecraft:map" || stack.ItemID == "minecraft:glass_pane"))
	case "minecraft:brewing_stand":
		if index <= 2 {
			return stack.ItemID == "minecraft:potion" || stack.ItemID == "minecraft:splash_potion" ||
				stack.ItemID == "minecraft:lingering_potion" || stack.ItemID == "minecraft:glass_bottle"
		}
		return (index == 4 && stack.ItemID == "minecraft:blaze_powder") || index == 3
	case "minecraft:beacon":
		switch stack.ItemID {
		case "minecraft:iron_ingot", "minecraft:gold_ingot", "minecraft:diamond", "minecraft:emerald", "minecraft:netherite_ingot":
			return true
		default:
			return false
		}
	default:
		return true
	}
}

func returnWorkstationItems(p *player.Player) {
	if p == nil || !IsWorkstation(p.OpenContainerKind) {
		return
	}
	output := WorkstationOutputIndex(p.OpenContainerKind)
	for index, stack := range p.ContainerSlots {
		if index == output || stack.IsEmpty() {
			continue
		}
		if p.GiveItem(stack) {
			p.ContainerSlots[index] = player.ItemStack{}
		}
	}
}

func closeWorkstation(p *player.Player, w *coreworld.World) {
	// Brewing stand items are persistent — save them before clearing slots.
	if p.OpenContainerKind == "minecraft:brewing_stand" && w != nil {
		items := make([]coreworld.ContainerItem, 0, len(p.ContainerSlots))
		for slot, stack := range p.ContainerSlots {
			if !stack.IsEmpty() {
				items = append(items, coreworld.ContainerItemFromStack(slot, stack))
			}
		}
		w.SetContainerItems(int(p.OpenContainerPos.X), int(p.OpenContainerPos.Y), int(p.OpenContainerPos.Z), p.OpenContainerKind, items)
	} else {
		returnWorkstationItems(p)
	}
	// Return carried item to inventory on close.
	if !p.CarriedItem.IsEmpty() {
		if p.GiveItem(p.CarriedItem) {
			p.CarriedItem = player.ItemStack{}
		}
	}
	p.OpenContainerID = 0
	p.OpenContainerKind = ""
	p.OpenContainerPos = spatial.BlockPos{}
	p.ContainerSlots = nil
	p.WorkstationSelection = 0
	p.ContainerStateID++
}

// handleAnvilRename processes the rename_item (C→S) packet sent by the client
// while typing a name in the anvil UI. It sets or clears the minecraft:custom_name
// component on slot 0, recomputes the output slot, and syncs the container.
func handleAnvilRename(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn) error {
	if p == nil {
		return nil
	}
	if p.OpenContainerKind != "minecraft:anvil" &&
		p.OpenContainerKind != "minecraft:chipped_anvil" &&
		p.OpenContainerKind != "minecraft:damaged_anvil" {
		return nil
	}
	r := pkt.Reader()
	name, err := protocol.ReadString(r)
	if err != nil {
		return err
	}
	if len(p.ContainerSlots) == 0 || p.ContainerSlots[0].IsEmpty() {
		return nil
	}
	slot0 := p.ContainerSlots[0]
	if name == "" {
		// Clear custom name.
		_ = slot0.SetComponent("minecraft:custom_name", nil)
	} else {
		_ = slot0.SetComponent("minecraft:custom_name", name)
	}
	p.ContainerSlots[0] = slot0
	UpdateWorkstationResult(p.OpenContainerKind, p.ContainerSlots, p.WorkstationSelection)
	p.ContainerStateID++
	if conn != nil {
		return sendChestContainerContent(conn, p)
	}
	return nil
}

// IsWorkstation reports whether kind uses GoCraft's transient workstation
// inventory. These inventories are owned by the player while the screen is
// open; unlike storage containers they are never written to a block entity.
func IsWorkstation(kind string) bool {
	switch kind {
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil",
		"minecraft:enchanting_table", "minecraft:grindstone", "minecraft:loom",
		"minecraft:smithing_table", "minecraft:stonecutter", "minecraft:brewing_stand",
		"minecraft:cartography_table", "minecraft:beacon":
		return true
	default:
		return false
	}
}

// WorkstationSlotCount is the number of workstation-owned slots in the Java
// and Bedrock vanilla layouts. Player inventory slots are appended by each
// protocol adapter.
func WorkstationSlotCount(kind string) int {
	switch kind {
	case "minecraft:enchanting_table":
		return 2
	case "minecraft:loom", "minecraft:smithing_table":
		return 4
	case "minecraft:brewing_stand":
		return 5
	case "minecraft:beacon":
		return 1
	case "minecraft:stonecutter":
		return 2
	default:
		if IsWorkstation(kind) {
			return 3
		}
		return 0
	}
}

// WorkstationOutputIndex returns the derived output slot, or -1 for screens
// whose slots are all persistent inputs/state.
func WorkstationOutputIndex(kind string) int {
	switch kind {
	case "minecraft:enchanting_table", "minecraft:brewing_stand", "minecraft:beacon":
		return -1
	case "minecraft:loom", "minecraft:smithing_table":
		return 3
	case "minecraft:stonecutter":
		return 1
	default:
		if IsWorkstation(kind) {
			return 2
		}
		return -1
	}
}

// SyncWorkstationToConn sends the current workstation slot contents to the
// player's open container. Used by the brewing stand ticker to push slot
// updates without reopening the screen.
func SyncWorkstationToConn(conn *network.ClientConn, p *player.Player) error {
	if conn == nil || p == nil {
		return nil
	}
	return sendChestContainerContent(conn, p)
}

// SyncBrewingContainer sends the two brewing stand progress properties to the
// player who currently has the block open. brewTime counts down 400→0,
// fuelAmount is the remaining brews from the last blaze powder (0-20).
func SyncBrewingContainer(conn *network.ClientConn, p *player.Player, brewTime, fuelAmount int) error {
	if conn == nil || p == nil || p.OpenContainerKind != "minecraft:brewing_stand" {
		return nil
	}
	for prop, value := range [2]int{brewTime, fuelAmount} {
		pkt := protocol.NewBuilder(packetIDSetContainerData).
			VarInt(workstationContainerID).Short(int16(prop)).Short(int16(value)).Build()
		if err := conn.WritePacket(pkt); err != nil {
			return err
		}
	}
	return nil
}

type workstationOperation struct {
	result  player.ItemStack
	consume []int
	xp      int32 // XP to award the player on output take (grindstone disenchant)
}

// UpdateWorkstationResult recomputes the server-authoritative preview. Client
// supplied output stacks are never trusted.
func UpdateWorkstationResult(kind string, slots []player.ItemStack, selection int) {
	output := WorkstationOutputIndex(kind)
	if output < 0 || output >= len(slots) {
		return
	}
	slots[output] = workstationOperationFor(kind, slots, selection).result
}

// TakeWorkstationResult consumes the exact inputs for the current preview and
// returns one indivisible result stack and any XP to award the player (e.g.
// from grindstone disenchanting). It is the only supported way to remove a
// non-empty workstation output.
func TakeWorkstationResult(kind string, slots []player.ItemStack, selection int) (player.ItemStack, int32, bool) {
	operation := workstationOperationFor(kind, slots, selection)
	if operation.result.IsEmpty() {
		return player.ItemStack{}, 0, false
	}
	for index, count := range operation.consume {
		if count <= 0 || index < 0 || index >= len(slots) || slots[index].Count < count {
			continue
		}
		slots[index].Count -= count
		normalizeStack(&slots[index])
	}
	UpdateWorkstationResult(kind, slots, selection)
	return operation.result, operation.xp, true
}

func workstationOperationFor(kind string, slots []player.ItemStack, selection int) workstationOperation {
	if len(slots) != WorkstationSlotCount(kind) {
		return workstationOperation{}
	}
	switch kind {
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil":
		return anvilOperation(slots)
	case "minecraft:grindstone":
		return grindstoneOperation(slots)
	case "minecraft:loom":
		return loomOperation(slots)
	case "minecraft:smithing_table":
		return smithingOperation(slots)
	case "minecraft:stonecutter":
		return stonecutterOperation(slots, selection)
	case "minecraft:cartography_table":
		return cartographyOperation(slots)
	default:
		return workstationOperation{}
	}
}

func anvilOperation(slots []player.ItemStack) workstationOperation {
	left, right := slots[0], slots[1]
	if left.IsEmpty() {
		return workstationOperation{}
	}
	// Rename-only: right slot empty, item has a custom name pending.
	if right.IsEmpty() {
		name := left.DisplayName()
		if name == "" {
			return workstationOperation{}
		}
		result := left
		result.Count = 1
		return workstationOperation{result: result, consume: []int{1, 0}}
	}
	name := left.DisplayName()

	// Enchanted book: transfer enchantments to the left item.
	if right.ItemID == "minecraft:enchanted_book" && player.MaxDurability(left.ItemID) > 0 {
		result := left
		result.Count = 1
		if name != "" {
			_ = result.SetComponent("minecraft:custom_name", name)
		}
		if mergeEnchantments(&result, right) {
			return workstationOperation{result: result, consume: []int{1, 1}}
		}
		return workstationOperation{}
	}

	if player.MaxDurability(left.ItemID) == 0 {
		return workstationOperation{}
	}
	if left.ItemID == right.ItemID {
		result := repairedStack(left, right, 12)
		// Carry left's enchantments and name into the repaired result, then merge right's.
		result.Enchantments = left.Enchantments
		result.Components = left.Components
		mergeEnchantments(&result, right)
		if name != "" {
			_ = result.SetComponent("minecraft:custom_name", name)
		}
		return workstationOperation{result: result, consume: []int{1, 1}}
	}
	if !itemregistry.RepairsWith(left.ItemID, right.ItemID) {
		return workstationOperation{}
	}
	result := left
	result.Count = 1
	result.Damage -= max(1, player.MaxDurability(left.ItemID)/4)
	if result.Damage < 0 {
		result.Damage = 0
	}
	if name != "" {
		_ = result.SetComponent("minecraft:custom_name", name)
	}
	return workstationOperation{result: result, consume: []int{1, 1}}
}

// mergeEnchantments applies compatible enchantments from src to dst. It uses
// vanilla merging rules: same level → combined to level+1 (capped at MaxLevel);
// src level higher → src level wins; dst level higher → dst wins (no change).
// Returns true if at least one enchantment was added or upgraded.
func mergeEnchantments(dst *player.ItemStack, src player.ItemStack) bool {
	srcLevels := src.EnchantmentLevels()
	if len(srcLevels) == 0 {
		return false
	}
	changed := false
	for _, el := range srcLevels {
		ench, ok := player.EnchantmentByID(el.ID)
		if !ok {
			continue
		}
		// Check compatibility: skip enchantments that conflict with existing ones.
		if !ench.CompatibleWith(*dst) && dst.EnchantmentLevel(el.ID) == 0 {
			continue
		}
		// Check the enchantment is valid for this item type (unless it's a book destination).
		if dst.ItemID != "minecraft:enchanted_book" && !ench.Supports(dst.ItemID) {
			continue
		}
		existing := dst.EnchantmentLevel(el.ID)
		var newLevel int
		if existing == el.Level {
			newLevel = min(el.Level+1, ench.MaxLevel)
		} else if el.Level > existing {
			newLevel = el.Level
		} else {
			continue // dst already has higher level
		}
		if dst.Enchant(el.ID, newLevel) {
			changed = true
		}
	}
	return changed
}

func grindstoneOperation(slots []player.ItemStack) workstationOperation {
	first, second := slots[0], slots[1]
	if first.IsEmpty() && second.IsEmpty() {
		return workstationOperation{}
	}
	if first.IsEmpty() || second.IsEmpty() {
		single, index := first, 0
		if single.IsEmpty() {
			single, index = second, 1
		}
		stripped, xp := grindstoneStrip(single)
		stripped.Count = 1
		consume := make([]int, 2)
		consume[index] = 1
		return workstationOperation{result: stripped, consume: consume, xp: xp}
	}
	if first.ItemID != second.ItemID || player.MaxDurability(first.ItemID) == 0 {
		return workstationOperation{}
	}
	repaired := repairedStack(first, second, 5)
	xp := grindstoneXP(first) + grindstoneXP(second)
	repaired.Enchantments = grindstoneKeepCurses(first, second)
	return workstationOperation{result: repaired, consume: []int{1, 1}, xp: xp}
}

// grindstoneStrip removes all non-curse enchantments from a stack, returning
// the stripped stack and the XP value of the removed enchantments.
func grindstoneStrip(s player.ItemStack) (player.ItemStack, int32) {
	xp := grindstoneXP(s)
	curses := grindstoneKeepCurses(s)
	result := s
	result.Enchantments = curses
	return result, xp
}

// grindstoneXP returns the XP refund for stripping enchantments from a stack.
// Each enchantment level contributes ~8 XP (a rough vanilla approximation).
func grindstoneXP(s player.ItemStack) int32 {
	var total int32
	for _, e := range s.EnchantmentLevels() {
		if e.ID == "minecraft:binding_curse" || e.ID == "minecraft:vanishing_curse" {
			continue
		}
		total += int32(e.Level) * 8
	}
	if total < 0 {
		total = 0
	}
	return total
}

// grindstoneKeepCurses encodes only the curse enchantments from one or two
// stacks for the grindstone output.
func grindstoneKeepCurses(stacks ...player.ItemStack) string {
	curses := []player.EnchantmentLevel{}
	for _, s := range stacks {
		for _, e := range s.EnchantmentLevels() {
			if e.ID != "minecraft:binding_curse" && e.ID != "minecraft:vanishing_curse" {
				continue
			}
			found := false
			for i, c := range curses {
				if c.ID == e.ID {
					if e.Level > c.Level {
						curses[i].Level = e.Level
					}
					found = true
					break
				}
			}
			if !found {
				curses = append(curses, e)
			}
		}
	}
	if len(curses) == 0 {
		return ""
	}
	encoded, _ := player.EncodeEnchantments(curses)
	return encoded
}

func repairedStack(first, second player.ItemStack, bonusPercent int) player.ItemStack {
	maximum := player.MaxDurability(first.ItemID)
	remaining := first.RemainingDurability() + second.RemainingDurability() + maximum*bonusPercent/100
	if remaining > maximum {
		remaining = maximum
	}
	return player.ItemStack{ItemID: first.ItemID, Count: 1, Damage: maximum - remaining}
}

func smithingOperation(slots []player.ItemStack) workstationOperation {
	resultID, ok := smithingTransform(slots[0].ItemID, slots[1].ItemID, slots[2].ItemID)
	if !ok {
		return workstationOperation{}
	}
	result := slots[1]
	result.ItemID = resultID
	result.Count = 1
	return workstationOperation{result: result, consume: []int{1, 1, 1}}
}

func loomOperation(slots []player.ItemStack) workstationOperation {
	if !itemregistry.HasTag(slots[0].ItemID, "minecraft:banners") ||
		!itemregistry.HasTag(slots[1].ItemID, "gocraft:dyes") {
		return workstationOperation{}
	}
	result := slots[0]
	result.Count = 1
	consume := []int{1, 1, 0}
	if !slots[2].IsEmpty() {
		consume[2] = 1
	}
	return workstationOperation{result: result, consume: consume}
}

func stonecutterOperation(slots []player.ItemStack, selection int) workstationOperation {
	input := slots[0]
	if input.IsEmpty() {
		return workstationOperation{}
	}
	matching := make([]recipeDisplay, 0, 16)
	for _, recipe := range javaRecipeDisplays {
		if recipe.kind != recipeDisplayStonecutter || len(recipe.ingredients) != 1 || !recipe.ingredients[0].matches(input.ItemID) {
			continue
		}
		matching = append(matching, recipe)
	}
	if selection < 0 || selection >= len(matching) {
		selection = 0
	}
	if len(matching) == 0 || matching[selection].result.stack.IsEmpty() {
		return workstationOperation{}
	}
	return workstationOperation{result: matching[selection].result.stack, consume: []int{1}}
}

func cartographyOperation(slots []player.ItemStack) workstationOperation {
	if slots[0].ItemID != "minecraft:filled_map" || slots[1].IsEmpty() {
		return workstationOperation{}
	}
	switch slots[1].ItemID {
	case "minecraft:paper", "minecraft:map", "minecraft:glass_pane":
		// Map scale, duplication and lock state live in data components. Keeping
		// the canonical map item here makes the operation safe until ItemStack
		// gains those components; the client can still use and close the table.
		return workstationOperation{
			result:  player.ItemStack{ItemID: "minecraft:filled_map", Count: 1, Damage: slots[0].Damage},
			consume: []int{1, 1},
		}
	default:
		return workstationOperation{}
	}
}
