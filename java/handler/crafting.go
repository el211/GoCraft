package handler

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"GoCraft/core/intent"
	"GoCraft/core/itemregistry"
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
	return -1
}

func addStackToInventory(inventory *[player.InventorySize]player.ItemStack, item player.ItemStack) bool {
	if item.IsEmpty() {
		return true
	}
	remaining := item.Count
	stackLimit := player.MaxStackSize(item.ItemID)
	for _, bounds := range [][2]int{{player.HotbarStart, player.HotbarStart + 9}, {9, player.HotbarStart}} {
		for i := bounds[0]; i < bounds[1] && remaining > 0; i++ {
			if !inventory[i].SameItem(item) || inventory[i].Count >= stackLimit {
				continue
			}
			add := minInt(stackLimit-inventory[i].Count, remaining)
			inventory[i].Count += add
			remaining -= add
		}
	}
	for _, bounds := range [][2]int{{player.HotbarStart, player.HotbarStart + 9}, {9, player.HotbarStart}} {
		for i := bounds[0]; i < bounds[1] && remaining > 0; i++ {
			if !inventory[i].IsEmpty() {
				continue
			}
			add := minInt(stackLimit, remaining)
			inventory[i] = item
			inventory[i].Count = add
			remaining -= add
		}
	}
	return remaining == 0
}

func handleContainerPacket(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn, w *coreworld.World, bus *intent.Bus) error {
	switch pkt.ID {
	case packetIDContainerClick:
		return handleContainerClick(pkt, p, conn, w, bus)
	case packetIDContainerClose:
		return handleContainerClose(pkt, p, conn, w)
	case packetIDContainerButtonClick:
		if p.OpenContainerKind == "minecraft:crafter" {
			r := pkt.Reader()
			if _, err := protocol.ReadVarInt(r); err != nil { // windowID
				return err
			}
			buttonID, err := protocol.ReadVarInt(r)
			if err != nil {
				return err
			}
			return handleCrafterButtonClick(p, conn, w, buttonID)
		}
		return handleWorkstationButtonClick(pkt, p, conn)
	}
	return nil
}

func handleContainerClick(pkt *protocol.Packet, p *player.Player, conn *network.ClientConn, w *coreworld.World, bus *intent.Bus) error {
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

	if windowID == 0 {
		before := p.ArmorPoints()
		if mode == 1 && slot == 0 {
			shiftPersonalCraftingResult(p)
		} else if mode == 0 && slot == 0 {
			takePersonalCraftingResult(p)
		} else if mode == 1 {
			shiftPlayerInventorySlot(p, int(slot))
		} else if mode == 0 {
			clickPlayerInventorySlot(p, int(slot), button)
		} else if mode == 5 {
			handleQuickCraft(p, int(slot), button, func(slot int) *player.ItemStack {
				if slot < 1 || slot >= player.InventorySize || !canPlaceInPlayerSlot(slot, p.CarriedItem) {
					return nil
				}
				return &p.Inventory[slot]
			})
		}
		updatePersonalCraftingResult(p)
		p.ContainerStateID++
		if err := sendSetContainerContent(conn, p, p.ContainerStateID); err != nil {
			return err
		}
		if p.ArmorPoints() != before {
			return sendArmorAttributes(conn, p)
		}
		return nil
	}

	if windowID == chestContainerID && p.OpenContainerID == windowID && isJavaStorageContainer(p.OpenContainerKind) {
		handleChestClick(p, w, int(slot), button, mode)
		return sendChestContainerContent(conn, p)
	}
	if windowID == furnaceContainerID && p.OpenContainerID == windowID && IsFurnaceContainer(p.OpenContainerKind) {
		handleFurnaceClick(p, w, int(slot), button, mode, bus)
		return sendFurnaceContainerContent(conn, p)
	}
	if windowID == workstationContainerID && p.OpenContainerID == windowID && IsWorkstation(p.OpenContainerKind) {
		handleWorkstationClick(p, int(slot), button, mode)
		if xp := p.PendingWorkstationXP; xp > 0 {
			p.PendingWorkstationXP = 0
			p.AddExperience(xp)
			_ = sendExperience(conn, p)
		}
		return sendChestContainerContent(conn, p)
	}
	if windowID != craftingTableContainerID || p.OpenContainerID != windowID || p.OpenContainerKind != "minecraft:crafting_table" {
		return nil
	}
	if mode == 1 && slot == 0 {
		shiftCraftingResult(p)
	} else if mode == 0 && slot == 0 {
		takeCraftingResult(p)
	} else if mode == 1 {
		shiftCraftingSlot(p, int(slot))
	} else if mode == 0 {
		clickCraftingSlot(p, int(slot), button)
	} else if mode == 5 {
		handleQuickCraft(p, int(slot), button, func(slot int) *player.ItemStack {
			return craftingContainerSlot(p, slot)
		})
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
	if windowID == chestContainerID && p.OpenContainerID == windowID && isJavaStorageContainer(p.OpenContainerKind) {
		persistStorageContents(p, w)
		p.OpenContainerID = 0
		p.OpenContainerKind = ""
		p.OpenContainerPos = spatial.BlockPos{}
		p.OpenContainerPartnerPos = spatial.BlockPos{}
		p.OpenContainerHasPartner = false
		p.ContainerSlots = nil
		p.ContainerStateID++
		return sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	if windowID == craftingTableContainerID && p.OpenContainerID == windowID && p.OpenContainerKind == "minecraft:crafting_table" {
		returnCraftingGrid(p)
		p.OpenContainerID = 0
		p.OpenContainerKind = ""
		p.ContainerStateID++
		return sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	if windowID == furnaceContainerID && p.OpenContainerID == windowID && IsFurnaceContainer(p.OpenContainerKind) {
		persistFurnaceContents(p, w)
		p.OpenContainerID = 0
		p.OpenContainerKind = ""
		p.OpenContainerPos = spatial.BlockPos{}
		p.ContainerSlots = nil
		p.ContainerStateID++
		return sendSetContainerContent(conn, p, p.ContainerStateID)
	}
	if windowID == workstationContainerID && p.OpenContainerID == windowID && IsWorkstation(p.OpenContainerKind) {
		closeWorkstation(p, w)
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
	removed, err := protocol.ReadVarInt(r)
	if err != nil {
		return player.ItemStack{}, err
	}
	damage := int32(0)
	enchantments := ""
	var potDecorations [4]string
	var fireworks player.FireworkData
	hasFireworks := false
	components := ""
	potionName := ""
	for i := int32(0); i < added; i++ {
		componentType, err := protocol.ReadVarInt(r)
		if err != nil {
			return player.ItemStack{}, err
		}
		switch componentType {
		case 0: // custom_data: preserve GoCraft's canonical extension object
			components, err = readGoCraftComponents(r)
			if err != nil {
				return player.ItemStack{}, fmt.Errorf("reading custom item data: %w", err)
			}
		case 2: // max_damage
			if _, err := protocol.ReadVarInt(r); err != nil {
				return player.ItemStack{}, err
			}
		case 3: // damage
			damage, err = protocol.ReadVarInt(r)
			if err != nil {
				return player.ItemStack{}, err
			}
		case 8: // lore: VarInt count followed by optional anonymous NBT
			lines, err := protocol.ReadVarInt(r)
			if err != nil || lines < 0 || lines > 256 {
				return player.ItemStack{}, fmt.Errorf("invalid lore line count %d: %w", lines, err)
			}
			for line := int32(0); line < lines; line++ {
				if err := skipNetworkNBT(r); err != nil {
					return player.ItemStack{}, fmt.Errorf("reading lore: %w", err)
				}
			}
		case 10: // enchantments: registry ID/level pairs
			length, readErr := protocol.ReadVarInt(r)
			if readErr != nil || length < 0 || length > 256 {
				return player.ItemStack{}, fmt.Errorf("invalid enchantment count %d: %w", length, readErr)
			}
			stack := player.ItemStack{ItemID: "minecraft:stone", Count: 1}
			for entry := int32(0); entry < length; entry++ {
				enchantmentID, idErr := protocol.ReadVarInt(r)
				level, levelErr := protocol.ReadVarInt(r)
				name := javaworld.EnchantmentName(enchantmentID)
				if idErr != nil || levelErr != nil || name == "" || level < 1 || level > 255 {
					return player.ItemStack{}, fmt.Errorf("invalid enchantment id=%d level=%d", enchantmentID, level)
				}
				stack.Enchant(name, int(level))
			}
			enchantments = stack.Enchantments
		case 61: // pot_decorations: array of item registry IDs
			length, readErr := protocol.ReadVarInt(r)
			if readErr != nil || length < 0 || length > 64 {
				return player.ItemStack{}, fmt.Errorf("invalid pot decoration count %d: %w", length, readErr)
			}
			for entry := int32(0); entry < length; entry++ {
				decorationID, idErr := protocol.ReadVarInt(r)
				if idErr != nil {
					return player.ItemStack{}, idErr
				}
				decoration := javaworld.ItemName(decorationID)
				if decoration == "" {
					return player.ItemStack{}, fmt.Errorf("unknown pot decoration item ID %d", decorationID)
				}
				if entry < int32(len(potDecorations)) {
					potDecorations[entry] = decoration
				}
			}
		case 13: // attribute modifiers, including the final showTooltip flag
			attributes, readErr := protocol.ReadVarInt(r)
			if readErr != nil || attributes < 0 || attributes > 256 {
				return player.ItemStack{}, fmt.Errorf("invalid attribute modifier count %d: %w", attributes, readErr)
			}
			for attribute := int32(0); attribute < attributes; attribute++ {
				if _, readErr = protocol.ReadVarInt(r); readErr != nil {
					return player.ItemStack{}, readErr
				}
				if _, readErr = protocol.ReadString(r); readErr != nil {
					return player.ItemStack{}, readErr
				}
				if _, readErr = protocol.ReadDouble(r); readErr != nil {
					return player.ItemStack{}, readErr
				}
				if _, readErr = protocol.ReadVarInt(r); readErr != nil {
					return player.ItemStack{}, readErr
				}
				if _, readErr = protocol.ReadVarInt(r); readErr != nil {
					return player.ItemStack{}, readErr
				}
			}
			if _, readErr = protocol.ReadBool(r); readErr != nil {
				return player.ItemStack{}, readErr
			}
		case 41: // potion_contents: optional potion, colour, custom effects
			hasPotion, readErr := protocol.ReadBool(r)
			if readErr != nil {
				return player.ItemStack{}, readErr
			}
			if hasPotion {
				potionRegistryID, idErr := protocol.ReadVarInt(r)
				if idErr != nil || javaworld.PotionName(potionRegistryID) == "" {
					return player.ItemStack{}, fmt.Errorf("invalid potion registry ID %d", potionRegistryID)
				}
				potionName = javaworld.PotionName(potionRegistryID)
			}
			hasColour, colourErr := protocol.ReadBool(r)
			if colourErr != nil {
				return player.ItemStack{}, colourErr
			}
			if hasColour {
				if _, colourErr = protocol.ReadInt(r); colourErr != nil {
					return player.ItemStack{}, colourErr
				}
			}
			customEffects, effectErr := protocol.ReadVarInt(r)
			if effectErr != nil || customEffects != 0 {
				return player.ItemStack{}, fmt.Errorf("unsupported custom potion effect count %d", customEffects)
			}
		case 56: // fireworks: flight duration and bounded explosion list
			flight, readErr := protocol.ReadVarInt(r)
			if readErr != nil || flight < 0 || flight > 255 {
				return player.ItemStack{}, fmt.Errorf("invalid firework flight %d: %w", flight, readErr)
			}
			length, readErr := protocol.ReadVarInt(r)
			if readErr != nil || length < 0 || length > player.MaxFireworkExplosions {
				return player.ItemStack{}, fmt.Errorf("invalid firework explosion count %d: %w", length, readErr)
			}
			fireworks.Flight = uint8(flight)
			fireworks.ExplosionCount = uint8(length)
			for explosionIndex := int32(0); explosionIndex < length; explosionIndex++ {
				explosion, readErr := readJavaFireworkExplosion(r)
				if readErr != nil {
					return player.ItemStack{}, readErr
				}
				fireworks.Explosions[explosionIndex] = explosion
			}
			hasFireworks = true
		default:
			return player.ItemStack{}, fmt.Errorf("unsupported item component %d", componentType)
		}
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
	if damage < 0 {
		damage = 0
	}
	stack := player.ItemStack{
		ItemID: name, Count: int(count), Damage: int(damage), Enchantments: enchantments, PotDecorations: potDecorations,
		HasFireworks: hasFireworks, Fireworks: fireworks,
	}
	if components != "" {
		if err := stack.SetComponents(components); err != nil {
			return player.ItemStack{}, fmt.Errorf("invalid canonical item components: %w", err)
		}
	}
	if potionName != "" {
		if existing, _ := player.PotionName(stack); existing == "" {
			if err := stack.SetComponent("potion_contents", map[string]string{"potion": potionName}); err != nil {
				return player.ItemStack{}, err
			}
		}
	}
	return stack, nil
}

func readJavaFireworkExplosion(r *bytes.Reader) (player.FireworkExplosion, error) {
	var explosion player.FireworkExplosion
	shape, err := protocol.ReadVarInt(r)
	if err != nil || shape < 0 || shape > 4 {
		return explosion, fmt.Errorf("invalid firework shape %d: %w", shape, err)
	}
	explosion.Shape = uint8(shape)
	colors, err := protocol.ReadVarInt(r)
	if err != nil || colors < 0 || colors > player.MaxFireworkColors {
		return explosion, fmt.Errorf("invalid firework color count %d: %w", colors, err)
	}
	explosion.ColorCount = uint8(colors)
	for index := int32(0); index < colors; index++ {
		color, readErr := protocol.ReadInt(r)
		if readErr != nil {
			return explosion, readErr
		}
		explosion.Colors[index] = color
	}
	fades, err := protocol.ReadVarInt(r)
	if err != nil || fades < 0 || fades > player.MaxFireworkColors {
		return explosion, fmt.Errorf("invalid firework fade count %d: %w", fades, err)
	}
	explosion.FadeColorCount = uint8(fades)
	for index := int32(0); index < fades; index++ {
		color, readErr := protocol.ReadInt(r)
		if readErr != nil {
			return explosion, readErr
		}
		explosion.FadeColors[index] = color
	}
	explosion.Trail, err = protocol.ReadBool(r)
	if err != nil {
		return explosion, err
	}
	explosion.Twinkle, err = protocol.ReadBool(r)
	return explosion, err
}

func skipNetworkNBT(r *bytes.Reader) error {
	tagType, err := r.ReadByte()
	if err != nil {
		return err
	}
	if tagType == 0 {
		return nil
	}
	return skipNBTPayload(r, tagType)
}

func skipNBTPayload(r *bytes.Reader, tagType byte) error {
	switch tagType {
	case 0:
		return nil
	case 1:
		return skipReaderBytes(r, 1)
	case 2:
		return skipReaderBytes(r, 2)
	case 3, 5:
		return skipReaderBytes(r, 4)
	case 4, 6:
		return skipReaderBytes(r, 8)
	case 7:
		n, err := readNBTLength(r)
		if err != nil {
			return err
		}
		return skipReaderBytes(r, n)
	case 8:
		return skipNBTString(r)
	case 9:
		elementType, err := r.ReadByte()
		if err != nil {
			return err
		}
		n, err := readNBTLength(r)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if err := skipNBTPayload(r, elementType); err != nil {
				return err
			}
		}
		return nil
	case 10:
		for {
			childType, err := r.ReadByte()
			if err != nil {
				return err
			}
			if childType == 0 {
				return nil
			}
			if err := skipNBTString(r); err != nil {
				return err
			}
			if err := skipNBTPayload(r, childType); err != nil {
				return err
			}
		}
	case 11:
		n, err := readNBTLength(r)
		if err != nil {
			return err
		}
		return skipReaderBytes(r, n*4)
	case 12:
		n, err := readNBTLength(r)
		if err != nil {
			return err
		}
		return skipReaderBytes(r, n*8)
	default:
		return fmt.Errorf("invalid NBT tag type %d", tagType)
	}
}

func readNBTLength(r *bytes.Reader) (int, error) {
	var raw [4]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return 0, err
	}
	n := int(int32(binary.BigEndian.Uint32(raw[:])))
	if n < 0 || n > r.Len() {
		return 0, fmt.Errorf("invalid NBT length %d", n)
	}
	return n, nil
}

func skipNBTString(r *bytes.Reader) error {
	_, err := readNBTStringValue(r)
	return err
}

func readNBTStringValue(r *bytes.Reader) (string, error) {
	var raw [2]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return "", err
	}
	length := int(binary.BigEndian.Uint16(raw[:]))
	if length > r.Len() {
		return "", io.ErrUnexpectedEOF
	}
	value := make([]byte, length)
	if _, err := io.ReadFull(r, value); err != nil {
		return "", err
	}
	return string(value), nil
}

func skipReaderBytes(r *bytes.Reader, n int) error {
	if n < 0 || n > r.Len() {
		return io.ErrUnexpectedEOF
	}
	_, err := r.Seek(int64(n), io.SeekCurrent)
	return err
}

func clickPlayerInventorySlot(p *player.Player, slot int, button byte) {
	if slot == -999 {
		if button == 0 {
			p.CarriedItem = player.ItemStack{}
		} else if !p.CarriedItem.IsEmpty() {
			p.CarriedItem.Count--
			normalizeStack(&p.CarriedItem)
		}
		return
	}
	// Slot zero is a derived crafting result and is handled separately. Slots
	// 1-4 are the player's 2x2 crafting input grid.
	if slot < 1 || slot >= player.InventorySize {
		return
	}
	target := &p.Inventory[slot]
	if button == 0 {
		switch {
		case p.CarriedItem.IsEmpty():
			p.CarriedItem, *target = *target, player.ItemStack{}
		case !canPlaceInPlayerSlot(slot, p.CarriedItem):
			return
		case target.IsEmpty():
			*target, p.CarriedItem = p.CarriedItem, player.ItemStack{}
		case target.SameItem(p.CarriedItem):
			limit := player.MaxStackSize(target.ItemID)
			add := minInt(limit-target.Count, p.CarriedItem.Count)
			if add > 0 {
				target.Count += add
				p.CarriedItem.Count -= add
				normalizeStack(&p.CarriedItem)
			}
		default:
			p.CarriedItem, *target = *target, p.CarriedItem
		}
		return
	}
	if button != 1 {
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
		return
	}
	if !canPlaceInPlayerSlot(slot, p.CarriedItem) {
		return
	}
	if target.IsEmpty() {
		*target = p.CarriedItem
		target.Count = 1
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
		return
	}
	if target.SameItem(p.CarriedItem) &&
		target.Count < player.MaxStackSize(target.ItemID) {
		target.Count++
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
	}
}

func shiftPlayerInventorySlot(p *player.Player, slot int) {
	if slot < 1 || slot >= player.InventorySize || p.Inventory[slot].IsEmpty() {
		return
	}
	item := p.Inventory[slot]
	if slot <= 4 {
		inventory := p.Inventory
		inventory[slot] = player.ItemStack{}
		if addStackToInventory(&inventory, item) {
			p.Inventory = inventory
		}
		return
	}
	if slot >= 9 {
		if armorSlot := armorInventorySlot(item.ItemID); armorSlot >= 5 && p.Inventory[armorSlot].IsEmpty() {
			p.Inventory[armorSlot] = item
			p.Inventory[slot] = player.ItemStack{}
			return
		}
	}
	if slot >= 5 && slot <= 8 {
		inventory := p.Inventory
		inventory[slot] = player.ItemStack{}
		if addStackToInventory(&inventory, item) {
			p.Inventory = inventory
		}
		return
	}
	// Vanilla shift-click alternates between main inventory and hotbar.
	start, end := player.HotbarStart, player.HotbarStart+9
	if slot >= player.HotbarStart && slot < player.HotbarStart+9 {
		start, end = 9, player.HotbarStart
	}
	for i := start; i < end; i++ {
		if p.Inventory[i].IsEmpty() {
			p.Inventory[i] = item
			p.Inventory[slot] = player.ItemStack{}
			return
		}
	}
}

func canPlaceInPlayerSlot(slot int, item player.ItemStack) bool {
	if slot < 5 || slot > 8 {
		return true
	}
	return armorInventorySlot(item.ItemID) == slot
}

// handleQuickCraft implements the three-packet drag sequence used by modern
// clients: start (0/4), add slot (1/5), and end (2/6). Left drag distributes
// the cursor evenly; right drag places one item in each selected slot.
func handleQuickCraft(p *player.Player, slot int, button byte, targetFor func(int) *player.ItemStack) {
	stage, dragButton := button%4, button/4
	switch stage {
	case 0:
		p.QuickCraftButton = dragButton
		p.QuickCraftSlots = p.QuickCraftSlots[:0]
	case 1:
		if p.CarriedItem.IsEmpty() || dragButton != p.QuickCraftButton || targetFor(slot) == nil {
			return
		}
		for _, existing := range p.QuickCraftSlots {
			if existing == slot {
				return
			}
		}
		p.QuickCraftSlots = append(p.QuickCraftSlots, slot)
	case 2:
		if dragButton != p.QuickCraftButton || p.CarriedItem.IsEmpty() || len(p.QuickCraftSlots) == 0 {
			p.QuickCraftSlots = p.QuickCraftSlots[:0]
			return
		}
		remainingSlots := len(p.QuickCraftSlots)
		for _, selected := range p.QuickCraftSlots {
			target := targetFor(selected)
			if target == nil || (!target.IsEmpty() && !target.SameItem(p.CarriedItem)) {
				remainingSlots--
				continue
			}
			amount := 1
			if dragButton == 0 {
				amount = p.CarriedItem.Count / remainingSlots
				if amount < 1 {
					amount = 1
				}
			}
			limit := player.MaxStackSize(p.CarriedItem.ItemID)
			space := limit
			if !target.IsEmpty() {
				space -= target.Count
			}
			amount = minInt(amount, minInt(space, p.CarriedItem.Count))
			if amount > 0 {
				if target.IsEmpty() {
					*target = p.CarriedItem
					target.Count = amount
				} else {
					target.Count += amount
				}
				p.CarriedItem.Count -= amount
				normalizeStack(&p.CarriedItem)
			}
			remainingSlots--
			if p.CarriedItem.IsEmpty() {
				break
			}
		}
		p.QuickCraftSlots = p.QuickCraftSlots[:0]
	}
}

func updatePersonalCraftingResult(p *player.Player) {
	grid := [4]player.ItemStack{
		p.Inventory[1], p.Inventory[2], p.Inventory[3], p.Inventory[4],
	}
	p.Inventory[0] = FindPersonalCraftingResult(grid)
}

func takePersonalCraftingResult(p *player.Player) {
	result := p.Inventory[0]
	if result.IsEmpty() || (!p.CarriedItem.IsEmpty() && !p.CarriedItem.SameItem(result)) {
		return
	}
	if p.CarriedItem.Count+result.Count > player.MaxStackSize(result.ItemID) {
		return
	}
	if p.CarriedItem.IsEmpty() {
		p.CarriedItem = result
	} else {
		p.CarriedItem.Count += result.Count
	}
	consumePersonalCraftingIngredients(p)
}

func shiftPersonalCraftingResult(p *player.Player) {
	for {
		result := p.Inventory[0]
		if result.IsEmpty() {
			return
		}
		inventory := p.Inventory
		if !addStackToInventory(&inventory, result) {
			return
		}
		p.Inventory = inventory
		consumePersonalCraftingIngredients(p)
		updatePersonalCraftingResult(p)
	}
}

func consumePersonalCraftingIngredients(p *player.Player) {
	for slot := 1; slot <= 4; slot++ {
		if p.Inventory[slot].IsEmpty() {
			continue
		}
		p.Inventory[slot].Count--
		normalizeStack(&p.Inventory[slot])
	}
}

func armorInventorySlot(itemID string) int {
	definition, ok := itemregistry.Lookup(itemID)
	if !ok || definition.Equipment == nil {
		return -1
	}
	switch definition.Equipment.Slot {
	case "head":
		return 5
	case "chest":
		return 6
	case "legs":
		return 7
	case "feet":
		return 8
	default:
		return -1
	}
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
		case target.SameItem(p.CarriedItem) && target.Count < 64:
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
		p.CarriedItem = *target
		p.CarriedItem.Count = take
		target.Count -= take
		normalizeStack(target)
	} else if target.IsEmpty() {
		*target = p.CarriedItem
		target.Count = 1
		p.CarriedItem.Count--
		normalizeStack(&p.CarriedItem)
	} else if target.SameItem(p.CarriedItem) && target.Count < 64 {
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
	if !p.CarriedItem.IsEmpty() && !p.CarriedItem.SameItem(p.CraftingResult) {
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

func shiftCraftingResult(p *player.Player) {
	for {
		result := findCraftingResult(p.CraftingGrid)
		if result.IsEmpty() {
			p.CraftingResult = player.ItemStack{}
			return
		}
		inventory := p.Inventory
		if !addStackToInventory(&inventory, result) {
			p.CraftingResult = result
			return
		}
		p.Inventory = inventory
		consumeCraftingIngredients(p)
	}
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
	return findCraftingResultIn(javaRecipeDisplays, grid)
}

func findCraftingResultIn(displays []recipeDisplay, grid [9]player.ItemStack) player.ItemStack {
	for _, recipe := range displays {
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

// FindPersonalCraftingResult resolves every vanilla shaped or shapeless recipe
// that fits the 2x2 crafting grid in the player's inventory. The recipe data
// and tag matching are shared with Java Edition, so wood variants, dyes and
// other tag-based recipes produce their corresponding vanilla result.
func FindPersonalCraftingResult(grid [4]player.ItemStack) player.ItemStack {
	var tableGrid [9]player.ItemStack
	tableGrid[0] = grid[0]
	tableGrid[1] = grid[1]
	tableGrid[3] = grid[2]
	tableGrid[4] = grid[3]
	return findCraftingResult(tableGrid)
}

// FindCraftingTableResult resolves a canonical 3x3 crafting-table grid.
func FindCraftingTableResult(grid [9]player.ItemStack) player.ItemStack {
	return findCraftingResult(grid)
}

// FindBedrockCraftingTableResult includes current Pumpkin Bedrock recipes that
// cannot be sent to GoCraft's Java 1.21.4 clients.
func FindBedrockCraftingTableResult(grid [9]player.ItemStack) player.ItemStack {
	if result := findCraftingResult(grid); !result.IsEmpty() {
		return result
	}
	return findCraftingResultIn(pumpkinBedrockRecipeDisplays, grid)
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
