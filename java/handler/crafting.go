package handler

import (
	"bytes"
	"fmt"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

const craftingTableContainerID int32 = 1

func openCraftingTable(p *player.Player, conn *network.ClientConn) error {
	returnCraftingGrid(p)
	p.OpenContainerID = craftingTableContainerID
	p.OpenContainerKind = "minecraft:crafting_table"
	p.ContainerStateID++
	p.CraftingResult = player.ItemStack{}
	if err := sendOpenScreen(conn, craftingTableContainerID, containerMenuType("minecraft:crafting_table"), "Crafting"); err != nil {
		return err
	}
	return sendCraftingContainerContent(conn, p)
}

func sendCraftingContainerContent(conn *network.ClientConn, p *player.Player) error {
	b := protocol.NewBuilder(packetIDSetContainerContent).
		VarInt(craftingTableContainerID).
		VarInt(p.ContainerStateID).
		VarInt(46)
	encodeSlot(b, p.CraftingResult)
	for i := range p.CraftingGrid {
		encodeSlot(b, p.CraftingGrid[i])
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

func placeCraftingRecipe(p *player.Player, conn *network.ClientConn, windowID int32, recipe recipeDisplay, makeAll bool) error {
	if windowID != craftingTableContainerID || p.OpenContainerID != craftingTableContainerID || p.OpenContainerKind != "minecraft:crafting_table" {
		_ = sendSystemMessage(conn, "Open a crafting table before placing this recipe.")
		return nil
	}
	if recipe.station != "minecraft:crafting_table" || (recipe.kind != recipeDisplayShaped && recipe.kind != recipeDisplayShapeless) {
		_ = sendSystemMessage(conn, "That recipe uses "+recipe.station+", not a crafting table.")
		return nil
	}

	inventory := p.Inventory
	for i := range p.CraftingGrid {
		if !addStackToInventory(&inventory, p.CraftingGrid[i]) {
			_ = sendSystemMessage(conn, "Not enough inventory space to replace the crafting grid.")
			return nil
		}
	}
	var grid [9]player.ItemStack
	template, err := craftingTemplate(recipe)
	if err != nil {
		return err
	}

	placed := 0
	for placed < 64 {
		nextInventory, nextGrid := inventory, grid
		if !placeRecipeOnce(&nextInventory, &nextGrid, template) {
			break
		}
		inventory, grid = nextInventory, nextGrid
		placed++
		if !makeAll {
			break
		}
	}
	if placed == 0 {
		_ = sendSystemMessage(conn, "Missing ingredients for "+recipe.name+".")
		return sendCraftingContainerContent(conn, p)
	}

	p.Inventory = inventory
	p.CraftingGrid = grid
	p.CraftingResult = recipe.result.stack
	p.ContainerStateID++
	return sendCraftingContainerContent(conn, p)
}

func craftingTemplate(recipe recipeDisplay) ([9]recipeSlotDisplay, error) {
	var template [9]recipeSlotDisplay
	if recipe.kind == recipeDisplayShapeless {
		if len(recipe.ingredients) > len(template) {
			return template, fmt.Errorf("recipe %s has %d shapeless ingredients", recipe.name, len(recipe.ingredients))
		}
		copy(template[:], recipe.ingredients)
		return template, nil
	}
	if recipe.width > 3 || recipe.height > 3 {
		return template, fmt.Errorf("recipe %s is %dx%d and does not fit a crafting table", recipe.name, recipe.width, recipe.height)
	}
	for y := int32(0); y < recipe.height; y++ {
		for x := int32(0); x < recipe.width; x++ {
			template[y*3+x] = recipe.ingredients[y*recipe.width+x]
		}
	}
	return template, nil
}

func placeRecipeOnce(inventory *[player.InventorySize]player.ItemStack, grid *[9]player.ItemStack, template [9]recipeSlotDisplay) bool {
	for gridIndex, ingredient := range template {
		if ingredient.kind == slotDisplayEmpty {
			continue
		}
		inventoryIndex := findMatchingInventorySlot(inventory, ingredient)
		if inventoryIndex < 0 {
			return false
		}
		chosen := inventory[inventoryIndex].ItemID
		if !grid[gridIndex].IsEmpty() && (grid[gridIndex].ItemID != chosen || grid[gridIndex].Count >= 64) {
			return false
		}
		inventory[inventoryIndex].Count--
		if inventory[inventoryIndex].Count == 0 {
			inventory[inventoryIndex] = player.ItemStack{}
		}
		if grid[gridIndex].IsEmpty() {
			grid[gridIndex] = player.ItemStack{ItemID: chosen, Count: 1}
		} else {
			grid[gridIndex].Count++
		}
	}
	return true
}

func findMatchingInventorySlot(inventory *[player.InventorySize]player.ItemStack, ingredient recipeSlotDisplay) int {
	for i := player.HotbarStart; i < player.HotbarStart+9; i++ {
		if !inventory[i].IsEmpty() && ingredient.matches(inventory[i].ItemID) {
			return i
		}
	}
	for i := 9; i < player.HotbarStart; i++ {
		if !inventory[i].IsEmpty() && ingredient.matches(inventory[i].ItemID) {
			return i
		}
	}
	if !inventory[45].IsEmpty() && ingredient.matches(inventory[45].ItemID) {
		return 45
	}
	return -1
}

func addStackToInventory(inventory *[player.InventorySize]player.ItemStack, item player.ItemStack) bool {
	if item.IsEmpty() {
		return true
	}
	remaining := item.Count
	for _, bounds := range [][2]int{{player.HotbarStart, player.HotbarStart + 9}, {9, player.HotbarStart}, {45, 46}} {
		for i := bounds[0]; i < bounds[1] && remaining > 0; i++ {
			if inventory[i].ItemID != item.ItemID || inventory[i].Count >= 64 {
				continue
			}
			add := minInt(64-inventory[i].Count, remaining)
			inventory[i].Count += add
			remaining -= add
		}
	}
	for _, bounds := range [][2]int{{player.HotbarStart, player.HotbarStart + 9}, {9, player.HotbarStart}, {45, 46}} {
		for i := bounds[0]; i < bounds[1] && remaining > 0; i++ {
			if !inventory[i].IsEmpty() {
				continue
			}
			add := minInt(64, remaining)
			inventory[i] = player.ItemStack{ItemID: item.ItemID, Count: add}
			remaining -= add
		}
	}
	return remaining == 0
}

func handleContainerPacket(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn, w *coreworld.World) error {
	switch pkt.ID {
	case packetIDContainerClick:
		return handleContainerClick(pkt, p, conn, w)
	case packetIDContainerClose:
		return handleContainerClose(pkt, p, conn, w)
	}
	return nil
}

func handleContainerClick(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn, w *coreworld.World) error {
	r := pkt.Reader()
	windowID, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("container click: reading ID: %w", err)
	}
	if _, err := protocol.ReadVarInt(r); err != nil {
		return fmt.Errorf("container click: reading state: %w", err)
	}
	slot, err := protocol.ReadShort(r)
	if err != nil {
		return fmt.Errorf("container click: reading slot: %w", err)
	}
	button, err := protocol.ReadByte(r)
	if err != nil {
		return fmt.Errorf("container click: reading button: %w", err)
	}
	mode, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("container click: reading mode: %w", err)
	}
	changed, err := protocol.ReadVarInt(r)
	if err != nil || changed < 0 || changed > 128 {
		return fmt.Errorf("container click: invalid changed slot count %d: %w", changed, err)
	}
	for i := int32(0); i < changed; i++ {
		if _, err := protocol.ReadShort(r); err != nil {
			return fmt.Errorf("container click: reading changed slot: %w", err)
		}
		if _, err := readPlainSlot(r); err != nil {
			return fmt.Errorf("container click: reading changed item: %w", err)
		}
	}
	if _, err := readPlainSlot(r); err != nil {
		return fmt.Errorf("container click: reading cursor: %w", err)
	}
	if r.Len() != 0 {
		return fmt.Errorf("container click: %d trailing bytes", r.Len())
	}

	if windowID == chestContainerID && p.OpenContainerID == windowID && p.OpenContainerKind == "minecraft:chest" {
		handleChestClick(p, w, int(slot), button, mode)
		return sendChestContainerContent(conn, p)
	}
	if windowID != craftingTableContainerID || p.OpenContainerID != windowID || p.OpenContainerKind != "minecraft:crafting_table" {
		return nil
	}
	if mode == 1 && slot == 0 {
		if !p.CraftingResult.IsEmpty() && p.GiveItem(p.CraftingResult) {
			consumeCraftingIngredients(p)
		}
	} else if mode == 0 && slot == 0 {
		takeCraftingResult(p)
	} else if mode == 1 {
		shiftCraftingSlot(p, int(slot))
	} else if mode == 0 {
		clickCraftingSlot(p, int(slot), button)
	}
	p.CraftingResult = findCraftingResult(p.CraftingGrid)
	p.ContainerStateID++
	return sendCraftingContainerContent(conn, p)
}

func handleContainerClose(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn, w *coreworld.World) error {
	r := pkt.Reader()
	windowID, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("container close: reading ID: %w", err)
	}
	if windowID == chestContainerID && p.OpenContainerID == windowID && p.OpenContainerKind == "minecraft:chest" {
		persistChestContents(p, w)
		p.OpenContainerID = 0
		p.OpenContainerKind = ""
		p.OpenContainerPos = spatial.BlockPos{}
		p.ContainerSlots = nil
		p.ContainerStateID++
		return sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	if windowID == craftingTableContainerID && p.OpenContainerID == windowID {
		returnCraftingGrid(p)
		p.OpenContainerID = 0
		p.OpenContainerKind = ""
		p.ContainerStateID++
		return sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	return nil
}

func readPlainSlot(r *bytes.Reader) (player.ItemStack, error) {
	count, err := protocol.ReadVarInt(r)
	if err != nil {
		return player.ItemStack{}, err
	}
	if count <= 0 {
		return player.ItemStack{}, nil
	}
	itemID, err := protocol.ReadVarInt(r)
	if err != nil {
		return player.ItemStack{}, err
	}
	added, err := protocol.ReadVarInt(r)
	if err != nil {
		return player.ItemStack{}, err
	}
	if added != 0 {
		return player.ItemStack{}, fmt.Errorf("component-bearing client slots are not supported")
	}
	removed, err := protocol.ReadVarInt(r)
	if err != nil {
		return player.ItemStack{}, err
	}
	for i := int32(0); i < removed; i++ {
		if _, err := protocol.ReadVarInt(r); err != nil {
			return player.ItemStack{}, err
		}
	}
	name := javaworld.ItemName(itemID)
	if name == "" {
		return player.ItemStack{}, fmt.Errorf("unknown item ID %d", itemID)
	}
	return player.ItemStack{ItemID: name, Count: int(count)}, nil
}

func clickCraftingSlot(p *player.Player, containerSlot int, button byte) {
	if containerSlot == -999 {
		if button == 0 {
			p.CarriedItem = player.ItemStack{}
		} else if !p.CarriedItem.IsEmpty() {
			p.CarriedItem.Count--
			normalizeStack(&p.CarriedItem)
		}
		return
	}
	target := craftingContainerSlot(p, containerSlot)
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

func shiftCraftingSlot(p *player.Player, containerSlot int) {
	target := craftingContainerSlot(p, containerSlot)
	if target == nil || target.IsEmpty() || containerSlot >= 10 {
		return
	}
	inventory := p.Inventory
	if addStackToInventory(&inventory, *target) {
		p.Inventory = inventory
		*target = player.ItemStack{}
	}
}

func takeCraftingResult(p *player.Player) {
	if p.CraftingResult.IsEmpty() {
		return
	}
	if !p.CarriedItem.IsEmpty() && p.CarriedItem.ItemID != p.CraftingResult.ItemID {
		return
	}
	if p.CarriedItem.Count+p.CraftingResult.Count > 64 {
		return
	}
	if p.CarriedItem.IsEmpty() {
		p.CarriedItem = p.CraftingResult
	} else {
		p.CarriedItem.Count += p.CraftingResult.Count
	}
	consumeCraftingIngredients(p)
}

func consumeCraftingIngredients(p *player.Player) {
	for i := range p.CraftingGrid {
		if p.CraftingGrid[i].IsEmpty() {
			continue
		}
		p.CraftingGrid[i].Count--
		normalizeStack(&p.CraftingGrid[i])
	}
}

func returnCraftingGrid(p *player.Player) {
	inventory := p.Inventory
	for i := range p.CraftingGrid {
		if addStackToInventory(&inventory, p.CraftingGrid[i]) {
			p.CraftingGrid[i] = player.ItemStack{}
		}
	}
	p.Inventory = inventory
	p.CraftingResult = player.ItemStack{}
}

func craftingContainerSlot(p *player.Player, containerSlot int) *player.ItemStack {
	switch {
	case containerSlot >= 1 && containerSlot <= 9:
		return &p.CraftingGrid[containerSlot-1]
	case containerSlot >= 10 && containerSlot <= 36:
		return &p.Inventory[containerSlot-1]
	case containerSlot >= 37 && containerSlot <= 45:
		return &p.Inventory[player.HotbarStart+containerSlot-37]
	default:
		return nil
	}
}

func findCraftingResult(grid [9]player.ItemStack) player.ItemStack {
	for _, recipe := range javaRecipeDisplays {
		if recipe.station != "minecraft:crafting_table" {
			continue
		}
		if recipe.kind == recipeDisplayShaped && shapedGridMatches(recipe, grid) {
			return recipe.result.stack
		}
		if recipe.kind == recipeDisplayShapeless && shapelessGridMatches(recipe, grid) {
			return recipe.result.stack
		}
	}
	return player.ItemStack{}
}

func shapedGridMatches(recipe recipeDisplay, grid [9]player.ItemStack) bool {
	for offsetY := int32(0); offsetY <= 3-recipe.height; offsetY++ {
		for offsetX := int32(0); offsetX <= 3-recipe.width; offsetX++ {
			for _, mirrored := range []bool{false, true} {
				matched := true
				for gy := int32(0); gy < 3 && matched; gy++ {
					for gx := int32(0); gx < 3; gx++ {
						slot := grid[gy*3+gx]
						inside := gx >= offsetX && gx < offsetX+recipe.width && gy >= offsetY && gy < offsetY+recipe.height
						if !inside {
							if !slot.IsEmpty() {
								matched = false
								break
							}
							continue
						}
						rx := gx - offsetX
						if mirrored {
							rx = recipe.width - 1 - rx
						}
						ingredient := recipe.ingredients[(gy-offsetY)*recipe.width+rx]
						if ingredient.kind == slotDisplayEmpty {
							if !slot.IsEmpty() {
								matched = false
								break
							}
						} else if slot.IsEmpty() || !ingredient.matches(slot.ItemID) {
							matched = false
							break
						}
					}
				}
				if matched {
					return true
				}
			}
		}
	}
	return false
}

func shapelessGridMatches(recipe recipeDisplay, grid [9]player.ItemStack) bool {
	items := make([]string, 0, 9)
	for _, slot := range grid {
		if !slot.IsEmpty() {
			items = append(items, slot.ItemID)
		}
	}
	if len(items) != len(recipe.ingredients) {
		return false
	}
	used := make([]bool, len(items))
	var match func(int) bool
	match = func(index int) bool {
		if index == len(recipe.ingredients) {
			return true
		}
		for i, item := range items {
			if used[i] || !recipe.ingredients[index].matches(item) {
				continue
			}
			used[i] = true
			if match(index + 1) {
				return true
			}
			used[i] = false
		}
		return false
	}
	return match(0)
}

func normalizeStack(stack *player.ItemStack) {
	if stack.Count <= 0 {
		*stack = player.ItemStack{}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
