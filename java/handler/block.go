package handler

// Block interaction handling for Milestone 8.
//
// Receives C→S Player Action (digging) and Use Item On (block placement)
// packets, mutates the canonical core/world, and broadcasts Block Update to
// every connected player.
//
// The packet layouts and IDs below target Minecraft Java 1.21.4,
// protocol 769.

import (
	"fmt"
	"log/slog"
	"strings"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
	javaworld "GoCraft/java/world"
)

// digBreaksBlock reports whether a Player Action should mutate the world.
// Creative breaks immediately on START_DIGGING. Survival normally waits for
// FINISH_DIGGING, but zero-hardness vegetation completes on START_DIGGING.
// Adventure and spectator cannot mutate blocks.
func digBreaksBlock(status int32, mode player.GameMode, blockName string) bool {
	switch mode {
	case player.GameModeCreative:
		return status == actionStatusStartDigging
	case player.GameModeSurvival:
		return status == actionStatusFinishDigging ||
			(status == actionStatusStartDigging && survivalInstantBreakBlock(blockName))
	default:
		return false
	}
}

func survivalInstantBreakBlock(blockName string) bool {
	switch blockName {
	case "minecraft:short_grass", "minecraft:grass", "minecraft:fern",
		"minecraft:tall_grass", "minecraft:large_fern", "minecraft:dead_bush",
		"minecraft:dandelion", "minecraft:poppy", "minecraft:allium",
		"minecraft:azure_bluet", "minecraft:red_tulip", "minecraft:orange_tulip",
		"minecraft:white_tulip", "minecraft:pink_tulip", "minecraft:oxeye_daisy",
		"minecraft:cornflower", "minecraft:lily_of_the_valley", "minecraft:blue_orchid",
		"minecraft:sunflower", "minecraft:lilac", "minecraft:rose_bush", "minecraft:peony",
		"minecraft:wither_rose", "minecraft:torchflower", "minecraft:brown_mushroom",
		"minecraft:red_mushroom", "minecraft:wheat", "minecraft:carrots",
		"minecraft:potatoes", "minecraft:beetroots", "minecraft:nether_wart":
		return true
	default:
		return false
	}
}

// Player Action status codes (field "status" in C→S Player Action).
const (
	actionStatusStartDigging  = 0 // block targeted — instant break in creative
	actionStatusCancelDigging = 1 // player looked away / right-clicked before break
	actionStatusFinishDigging = 2 // break animation completed (survival)
)

// ── Dispatch ──────────────────────────────────────────────────────────────────

// handleBlockPacket dispatches an incoming block-interaction packet.
// Called from the play loop for packets that need the world and session manager.
func handleBlockPacket(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn) error {
	switch pkt.ID {
	case packetIDPlayerAction:
		return handlePlayerAction(pkt, p, w, mgr)
	case packetIDUseItemOn:
		return handleUseItemOn(pkt, p, w, mgr, conn)
	}
	return nil
}

// ── C→S handlers ─────────────────────────────────────────────────────────────

// handlePlayerAction handles C→S Player Action.
//
// Wire layout (1.21.4):
//
//	VarInt    status   (0=start, 1=cancel, 2=finish digging)
//	Long      location (packed block position: X«38 | Z«12 | Y)
//	Byte      face     (0=−Y, 1=+Y, 2=−Z, 3=+Z, 4=−X, 5=+X)
//	VarInt    sequence (monotonic counter; echoed in Acknowledge Block Change)
func handlePlayerAction(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager) error {
	r := pkt.Reader()

	status, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("player action: reading status: %w", err)
	}
	bx, by, bz, err := protocol.ReadPosition(r)
	if err != nil {
		return fmt.Errorf("player action: reading position: %w", err)
	}
	if _, err := protocol.ReadByte(r); err != nil { // face — unused in M8
		return fmt.Errorf("player action: reading face: %w", err)
	}
	seq, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("player action: reading sequence: %w", err)
	}

	// Reject out-of-bounds Y before touching the world.
	if int(by) < coreworld.WorldMinY || int(by) > coreworld.WorldMaxY {
		slog.Warn("player action: Y out of bounds", "player", p.Username, "y", by)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}

	broken := w.GetBlock(int(bx), int(by), int(bz))
	if !broken.IsAir() && digBreaksBlock(status, p.GameMode, broken.ResourceLocation()) {
		slog.Info("block break", "player", p.Username,
			"x", bx, "y", by, "z", bz,
			"block", broken.ResourceLocation(),
			"mode", p.GameMode, "status", status)
		applyBlockChange(int(bx), int(by), int(bz), coreworld.Air, w, mgr)
		breakLinkedPlantHalf(int(bx), int(by), int(bz), broken, w, mgr)
		unlinkChestPartner(int(bx), int(by), int(bz), broken, w, mgr)
		broadcastSoundAt(mgr, blockBreakSound(broken.ResourceLocation()), soundCategoryBlocks,
			float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 0.8)

		// Give drop to player in survival/adventure mode.
		if p.GameMode != player.GameModeCreative && p.GameMode != player.GameModeSpectator {
			dropName, dropCount := blockDropItem(broken.ResourceLocation())
			if dropName != "" && dropCount > 0 {
				if p.GiveItem(player.ItemStack{ItemID: dropName, Count: dropCount}) {
					// Sync updated inventory to the client.
					sess, ok := mgr.Get(p.UUID)
					if ok {
						_ = sendSetContainerContent(sess.Conn, p, 1)
					}
					// GoCraft does not yet spawn experience-orb entities, but
					// experience-bearing ores still provide vanilla pickup feedback.
					if rewardsExperience(broken.ResourceLocation()) {
						broadcastSoundAt(mgr, "minecraft:entity.experience_orb.pickup", soundCategoryPlayers,
							float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 0.2, 1)
					}
				}
			}
		}
	}
	if status == actionStatusStartDigging || status == actionStatusCancelDigging {
		slog.Debug("block digging", "player", p.Username, "x", bx, "y", by, "z", bz,
			"mode", p.GameMode, "status", status)
	}

	// Always acknowledge so the client does not roll back its optimistic update.
	sendAcknowledgeBlockChange(mgr, p, seq)
	return nil
}

func breakLinkedPlantHalf(x, y, z int, broken coreworld.Block, w *coreworld.World, mgr *session.Manager) {
	switch broken.ResourceLocation() {
	case "minecraft:tall_grass", "minecraft:large_fern", "minecraft:sunflower",
		"minecraft:lilac", "minecraft:rose_bush", "minecraft:peony", "minecraft:pitcher_plant":
	default:
		return
	}
	half := broken.Properties["half"]
	otherY := y + 1
	wantHalf := "upper"
	if half == "upper" {
		otherY, wantHalf = y-1, "lower"
	} else if half != "lower" {
		return
	}
	other := w.GetBlock(x, otherY, z)
	if other.ResourceLocation() == broken.ResourceLocation() && other.Properties["half"] == wantHalf {
		applyBlockChange(x, otherY, z, coreworld.Air, w, mgr)
	}
}

// faceOffset maps a Use Item On face index to the (dx, dy, dz) offset of the
// block being placed relative to the targeted block.
//
//	0: −Y (bottom face → place below)
//	1: +Y (top face    → place above, most common)
//	2: −Z (north face)
//	3: +Z (south face)
//	4: −X (west face)
//	5: +X (east face)
var faceOffset = [6][3]int32{
	{0, -1, 0},
	{0, +1, 0},
	{0, 0, -1},
	{0, 0, +1},
	{-1, 0, 0},
	{+1, 0, 0},
}

// containerMenuType maps a block resource location to its minecraft:menu
// protocol ID when right-clicking opens a container UI.
// Returns -1 if the block is not an interactive container.
func containerMenuType(blockName string) int32 {
	switch blockName {
	case "minecraft:crafting_table":
		return 12 // minecraft:crafting
	case "minecraft:furnace", "minecraft:lit_furnace":
		return 14 // minecraft:furnace
	case "minecraft:blast_furnace", "minecraft:lit_blast_furnace":
		return 10 // minecraft:blast_furnace
	case "minecraft:smoker", "minecraft:lit_smoker":
		return 22 // minecraft:smoker
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil":
		return 8 // minecraft:anvil
	case "minecraft:enchanting_table":
		return 13 // minecraft:enchantment
	case "minecraft:grindstone":
		return 15 // minecraft:grindstone
	case "minecraft:loom":
		return 18 // minecraft:loom
	case "minecraft:smithing_table":
		return 21 // minecraft:smithing
	case "minecraft:stonecutter":
		return 24 // minecraft:stonecutter
	case "minecraft:brewing_stand":
		return 11 // minecraft:brewing_stand
	case "minecraft:cartography_table":
		return 23 // minecraft:cartography_table
	case "minecraft:beacon":
		return 9 // minecraft:beacon
	case "minecraft:chest", "minecraft:trapped_chest", "minecraft:barrel",
		"minecraft:ender_chest":
		return 2 // minecraft:generic_9x3
	case "minecraft:hopper":
		return 16 // minecraft:hopper
	case "minecraft:dispenser", "minecraft:dropper":
		return 6 // minecraft:generic_3x3
	case "minecraft:shulker_box",
		"minecraft:white_shulker_box", "minecraft:orange_shulker_box",
		"minecraft:magenta_shulker_box", "minecraft:light_blue_shulker_box",
		"minecraft:yellow_shulker_box", "minecraft:lime_shulker_box",
		"minecraft:pink_shulker_box", "minecraft:gray_shulker_box",
		"minecraft:light_gray_shulker_box", "minecraft:cyan_shulker_box",
		"minecraft:purple_shulker_box", "minecraft:blue_shulker_box",
		"minecraft:brown_shulker_box", "minecraft:green_shulker_box",
		"minecraft:red_shulker_box", "minecraft:black_shulker_box":
		return 20 // minecraft:shulker_box
	}
	return -1
}

// blockDropItem returns the canonical item name and count that should drop when
// the block with the given resource location is broken without silk touch.
// Returns ("", 0) if the block drops nothing.
func blockDropItem(blockName string) (string, int) {
	switch blockName {
	// Stone-family: drop cobblestone
	case "minecraft:stone":
		return "minecraft:cobblestone", 1
	case "minecraft:infested_stone":
		return "", 0 // silverfish block — no drop

	// Grass / dirt variants
	case "minecraft:grass_block", "minecraft:mycelium", "minecraft:podzol":
		return "minecraft:dirt", 1
	case "minecraft:farmland", "minecraft:dirt_path":
		return "minecraft:dirt", 1

	// Coal ore
	case "minecraft:coal_ore", "minecraft:deepslate_coal_ore":
		return "minecraft:coal", 1

	// Iron ore
	case "minecraft:iron_ore", "minecraft:deepslate_iron_ore":
		return "minecraft:raw_iron", 1

	// Gold ore
	case "minecraft:gold_ore", "minecraft:deepslate_gold_ore":
		return "minecraft:raw_gold", 1
	case "minecraft:nether_gold_ore":
		return "minecraft:gold_nugget", 4

	// Diamond ore
	case "minecraft:diamond_ore", "minecraft:deepslate_diamond_ore":
		return "minecraft:diamond", 1

	// Emerald ore
	case "minecraft:emerald_ore", "minecraft:deepslate_emerald_ore":
		return "minecraft:emerald", 1

	// Lapis ore
	case "minecraft:lapis_ore", "minecraft:deepslate_lapis_ore":
		return "minecraft:lapis_lazuli", 4

	// Redstone ore
	case "minecraft:redstone_ore", "minecraft:deepslate_redstone_ore",
		"minecraft:lit_redstone_ore", "minecraft:lit_deepslate_redstone_ore":
		return "minecraft:redstone", 4

	// Copper ore
	case "minecraft:copper_ore", "minecraft:deepslate_copper_ore":
		return "minecraft:raw_copper", 2

	// Nether quartz ore
	case "minecraft:nether_quartz_ore":
		return "minecraft:quartz", 1

	// Ancient debris (drops itself)
	case "minecraft:ancient_debris":
		return "minecraft:ancient_debris", 1

	// Nether wart
	case "minecraft:nether_wart":
		return "minecraft:nether_wart", 2

	// Gravel (simplification: always drops gravel, not flint)
	case "minecraft:gravel":
		return "minecraft:gravel", 1

	// Clay
	case "minecraft:clay":
		return "minecraft:clay_ball", 4

	// Glowstone
	case "minecraft:glowstone":
		return "minecraft:glowstone_dust", 2

	// Sea lantern
	case "minecraft:sea_lantern":
		return "minecraft:prismarine_crystals", 2

	// Leaves — drop nothing without silk touch (simplification: no sapling chance)
	case "minecraft:oak_leaves", "minecraft:birch_leaves", "minecraft:spruce_leaves",
		"minecraft:jungle_leaves", "minecraft:acacia_leaves", "minecraft:dark_oak_leaves",
		"minecraft:cherry_leaves", "minecraft:azalea_leaves", "minecraft:flowering_azalea_leaves",
		"minecraft:mangrove_leaves":
		return "", 0

	// Grass simplification: always yields one seed (vanilla uses a chance).
	case "minecraft:short_grass", "minecraft:grass", "minecraft:fern",
		"minecraft:tall_grass", "minecraft:large_fern":
		return "minecraft:wheat_seeds", 1

	// Flowers drop themselves.
	case "minecraft:dandelion", "minecraft:poppy", "minecraft:allium",
		"minecraft:azure_bluet", "minecraft:red_tulip", "minecraft:orange_tulip",
		"minecraft:white_tulip", "minecraft:pink_tulip", "minecraft:oxeye_daisy",
		"minecraft:cornflower", "minecraft:lily_of_the_valley", "minecraft:blue_orchid",
		"minecraft:sunflower", "minecraft:lilac", "minecraft:rose_bush", "minecraft:peony",
		"minecraft:wither_rose", "minecraft:torchflower":
		return blockName, 1

	// Plants that currently have no survival drop implementation.
	case "minecraft:dead_bush", "minecraft:seagrass", "minecraft:tall_seagrass",
		"minecraft:vine", "minecraft:moss_carpet",
		"minecraft:brown_mushroom", "minecraft:red_mushroom":
		return "", 0

	// Air — nothing
	case "", "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:water", "minecraft:lava":
		return "", 0

	// Everything else drops itself
	default:
		return blockName, 1
	}
}

// handleUseItemOn handles C→S Use Item On (block placement).
//
// Wire layout (1.21.4):
//
//	VarInt    hand         (0=main hand, 1=off hand)
//	Long      location     (packed block position of the targeted block)
//	VarInt    face         (0=−Y, 1=+Y, 2=−Z, 3=+Z, 4=−X, 5=+X)
//	Float     cursor_x/y/z (hit position within the target face, 0.0–1.0)
//	Bool      inside_block (player head is inside a block)
//	Bool      world_border_hit
//	VarInt    sequence
func handleUseItemOn(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn) error {
	r := pkt.Reader()

	hand, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("use item on: reading hand: %w", err)
	}
	bx, by, bz, err := protocol.ReadPosition(r)
	if err != nil {
		return fmt.Errorf("use item on: reading position: %w", err)
	}
	face, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("use item on: reading face: %w", err)
	}
	if _, err := protocol.ReadFloat(r); err != nil { // cursor_x
		return fmt.Errorf("use item on: reading cursor_x: %w", err)
	}
	if _, err := protocol.ReadFloat(r); err != nil { // cursor_y
		return fmt.Errorf("use item on: reading cursor_y: %w", err)
	}
	if _, err := protocol.ReadFloat(r); err != nil { // cursor_z
		return fmt.Errorf("use item on: reading cursor_z: %w", err)
	}
	if _, err := protocol.ReadBool(r); err != nil { // inside_block
		return fmt.Errorf("use item on: reading inside_block: %w", err)
	}
	if _, err := protocol.ReadBool(r); err != nil { // world_border_hit
		return fmt.Errorf("use item on: reading world_border_hit: %w", err)
	}
	seq, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("use item on: reading sequence: %w", err)
	}

	// Tool and seed interactions run before generic block/container handling.
	targetBlock := w.GetBlock(int(bx), int(by), int(bz))
	if hand == 0 && useHoeOrPlant(int(bx), int(by), int(bz), face, targetBlock, p, w, mgr) {
		sendAcknowledgeBlockChange(mgr, p, seq)
		p.ContainerStateID++
		if sess, ok := mgr.Get(p.UUID); ok {
			_ = sendSetContainerContent(sess.Conn, p, p.ContainerStateID)
		}
		return nil
	}

	// Container blocks: right-clicking opens a UI instead of placing a block.
	// (Sneaking to bypass is not yet tracked; always open the container.)
	if toggleDoor(int(bx), int(by), int(bz), targetBlock, w, mgr) {
		sound := "minecraft:block.wooden_door.open"
		if targetBlock.Properties["open"] == "true" {
			sound = "minecraft:block.wooden_door.close"
		}
		broadcastSoundAt(mgr, sound, soundCategoryBlocks,
			float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
		slog.Info("door toggled", "player", p.Username,
			"x", bx, "y", by, "z", bz, "block", targetBlock.ResourceLocation())
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if menuType := containerMenuType(targetBlock.ResourceLocation()); menuType >= 0 {
		title := containerTitle(targetBlock.ResourceLocation())
		slog.Info("container opened", "player", p.Username, "block", targetBlock.ResourceLocation())
		sendAcknowledgeBlockChange(mgr, p, seq)
		if targetBlock.ResourceLocation() == "minecraft:crafting_table" {
			return openCraftingTable(p, conn)
		}
		if targetBlock.ResourceLocation() == "minecraft:chest" ||
			targetBlock.ResourceLocation() == "minecraft:trapped_chest" {
			return openChest(p, conn, w, spatial.BlockPos{X: bx, Y: by, Z: bz})
		}
		return sendOpenScreen(conn, 1, menuType, title)
	}

	// Resolve the held block before choosing whether a replaceable target is
	// overwritten or the adjacent face receives the placement.
	held := p.HeldItem()
	if held.IsEmpty() || !javaworld.IsPlaceableAsBlock(held.ItemID) ||
		p.GameMode == player.GameModeAdventure || p.GameMode == player.GameModeSpectator {
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}

	px, py, pz := int(bx), int(by), int(bz)
	if !placementReplaceable(targetBlock.ResourceLocation()) {
		if face < 0 || int(face) >= len(faceOffset) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		off := faceOffset[face]
		px, py, pz = int(bx+off[0]), int(by+off[1]), int(bz+off[2])
	}

	if py < coreworld.WorldMinY || py > coreworld.WorldMaxY {
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}

	existing := w.GetBlock(px, py, pz)
	if !existing.IsAir() && !placementReplaceable(existing.ResourceLocation()) {
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !existing.IsAir() {
		breakLinkedPlantHalf(px, py, pz, existing, w, mgr)
	}

	block := javaworld.ItemIDToBlock(held.ItemID)
	slog.Info("block place", "player", p.Username,
		"block", block.ResourceLocation(), "x", px, "y", py, "z", pz)
	switch block.ResourceLocation() {
	case "minecraft:chest", "minecraft:trapped_chest":
		placeChestBlock(p, px, py, pz, block.ResourceLocation(), w, mgr)
		w.SetContainerItems(px, py, pz, block.ResourceLocation(), nil)
	default:
		applyBlockChange(px, py, pz, block, w, mgr)
	}
	if p.GameMode == player.GameModeSurvival {
		slot := player.HotbarStart + p.HeldSlot
		p.Inventory[slot].Count--
		normalizeStack(&p.Inventory[slot])
		p.ContainerStateID++
		if sess, ok := mgr.Get(p.UUID); ok {
			_ = sendSetContainerContent(sess.Conn, p, p.ContainerStateID)
		}
	}
	sendAcknowledgeBlockChange(mgr, p, seq)
	return nil
}

func placementReplaceable(blockName string) bool {
	switch blockName {
	case "", "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:short_grass", "minecraft:grass", "minecraft:fern",
		"minecraft:tall_grass", "minecraft:large_fern", "minecraft:dead_bush",
		"minecraft:dandelion", "minecraft:poppy", "minecraft:allium",
		"minecraft:azure_bluet", "minecraft:red_tulip", "minecraft:orange_tulip",
		"minecraft:white_tulip", "minecraft:pink_tulip", "minecraft:oxeye_daisy",
		"minecraft:cornflower", "minecraft:lily_of_the_valley", "minecraft:blue_orchid",
		"minecraft:sunflower", "minecraft:lilac", "minecraft:rose_bush", "minecraft:peony",
		"minecraft:wither_rose", "minecraft:torchflower", "minecraft:brown_mushroom",
		"minecraft:red_mushroom", "minecraft:snow", "minecraft:vine", "minecraft:fire":
		return true
	default:
		return false
	}
}

func useHoeOrPlant(x, y, z int, face int32, target coreworld.Block, p *player.Player, w *coreworld.World, mgr *session.Manager) bool {
	held := p.HeldItem()
	if held.IsEmpty() || face == 0 {
		return false
	}
	if isHoe(held.ItemID) {
		above := w.GetBlock(x, y+1, z)
		if !above.IsAir() {
			return false
		}
		var replacement coreworld.Block
		switch target.ResourceLocation() {
		case "minecraft:grass_block", "minecraft:dirt", "minecraft:dirt_path":
			replacement = coreworld.Block{Namespace: "minecraft", Name: "farmland", Properties: map[string]string{"moisture": "0"}}
		case "minecraft:coarse_dirt", "minecraft:rooted_dirt":
			replacement = coreworld.Block{Namespace: "minecraft", Name: "dirt"}
		default:
			return false
		}
		applyBlockChange(x, y, z, replacement, w, mgr)
		broadcastSoundAt(mgr, "minecraft:item.hoe.till", soundCategoryBlocks, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
		return true
	}

	if face != 1 || !w.GetBlock(x, y+1, z).IsAir() {
		return false
	}
	var crop coreworld.Block
	switch {
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:wheat_seeds":
		crop = coreworld.Block{Namespace: "minecraft", Name: "wheat", Properties: map[string]string{"age": "0"}}
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:carrot":
		crop = coreworld.Block{Namespace: "minecraft", Name: "carrots", Properties: map[string]string{"age": "0"}}
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:potato":
		crop = coreworld.Block{Namespace: "minecraft", Name: "potatoes", Properties: map[string]string{"age": "0"}}
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:beetroot_seeds":
		crop = coreworld.Block{Namespace: "minecraft", Name: "beetroots", Properties: map[string]string{"age": "0"}}
	case target.ResourceLocation() == "minecraft:soul_sand" && held.ItemID == "minecraft:nether_wart":
		crop = coreworld.Block{Namespace: "minecraft", Name: "nether_wart", Properties: map[string]string{"age": "0"}}
	default:
		return false
	}
	applyBlockChange(x, y+1, z, crop, w, mgr)
	broadcastSoundAt(mgr, "minecraft:item.crop.plant", soundCategoryBlocks, float64(x)+0.5, float64(y)+1, float64(z)+0.5, 1, 1)
	if p.GameMode != player.GameModeCreative {
		slot := player.HotbarStart + p.HeldSlot
		p.Inventory[slot].Count--
		if p.Inventory[slot].Count <= 0 {
			p.Inventory[slot] = player.ItemStack{}
		}
	}
	return true
}

func isHoe(item string) bool {
	switch item {
	case "minecraft:wooden_hoe", "minecraft:stone_hoe", "minecraft:iron_hoe",
		"minecraft:golden_hoe", "minecraft:diamond_hoe", "minecraft:netherite_hoe":
		return true
	default:
		return false
	}
}

// toggleDoor toggles both halves of a non-iron door and broadcasts the two
// resulting block states. Iron doors intentionally remain redstone-only.
func toggleDoor(x, y, z int, door coreworld.Block, w *coreworld.World, mgr *session.Manager) bool {
	if door.Namespace != "minecraft" || door.Name == "iron_door" ||
		!strings.HasSuffix(door.Name, "_door") {
		return false
	}
	open, ok := door.Properties["open"]
	if !ok {
		return false
	}
	nextOpen := "true"
	if open == "true" {
		nextOpen = "false"
	}
	toggled := copyBlockProperties(door)
	toggled.Properties["open"] = nextOpen
	applyBlockChange(x, y, z, toggled, w, mgr)

	otherY := y + 1
	if door.Properties["half"] == "upper" {
		otherY = y - 1
	}
	other := w.GetBlock(x, otherY, z)
	if other.ResourceLocation() == door.ResourceLocation() {
		otherToggled := copyBlockProperties(other)
		if _, exists := otherToggled.Properties["open"]; exists {
			otherToggled.Properties["open"] = nextOpen
			applyBlockChange(x, otherY, z, otherToggled, w, mgr)
		}
	}
	return true
}

func copyBlockProperties(block coreworld.Block) coreworld.Block {
	properties := make(map[string]string, len(block.Properties))
	for key, value := range block.Properties {
		properties[key] = value
	}
	block.Properties = properties
	return block
}

// ── World mutation + broadcast ────────────────────────────────────────────────

// applyBlockChange sets the block at (x, y, z) in the canonical world and
// broadcasts a Block Update packet to every connected player.
func applyBlockChange(x, y, z int, block coreworld.Block, w *coreworld.World, mgr *session.Manager) {
	w.SetBlock(x, y, z, block)
	stateID := javaworld.StateID(block)
	pkt := buildBlockUpdate(x, y, z, stateID)
	for _, s := range mgr.SnapshotAll() {
		_ = s.Conn.WritePacket(pkt)
	}
}

// BroadcastBlockChange sends an already-applied canonical mutation to all Java clients.
func BroadcastBlockChange(change coreworld.BlockChange, mgr *session.Manager) {
	pkt := buildBlockUpdate(change.X, change.Y, change.Z, javaworld.StateID(change.Block))
	for _, current := range mgr.SnapshotAll() {
		_ = current.Conn.WritePacket(pkt)
	}
}

// sendAcknowledgeBlockChange sends an Acknowledge Block Change packet to the
// session identified by p.UUID.
func sendAcknowledgeBlockChange(mgr *session.Manager, p *player.Player, seq int32) {
	sess, ok := mgr.Get(p.UUID)
	if !ok {
		return
	}
	_ = sess.Conn.WritePacket(buildAcknowledgeBlockChange(seq))
}

// ── Packet builders ───────────────────────────────────────────────────────────

// buildBlockUpdate constructs a Block Update (S→C) packet.
//
// Wire layout (1.21.4):
//
//	Long    location (packed: X«38 | Z«12 | Y)
//	VarInt  block_state_id
func buildBlockUpdate(x, y, z int, stateID int32) *protocol.Packet {
	return protocol.NewBuilder(packetIDBlockUpdate).
		Long(packBlockPos(x, y, z)).
		VarInt(stateID).
		Build()
}

// buildAcknowledgeBlockChange constructs an Acknowledge Block Change (S→C) packet.
//
// Wire layout (1.21.4):
//
//	VarInt  sequence_id
func buildAcknowledgeBlockChange(seq int32) *protocol.Packet {
	return protocol.NewBuilder(packetIDAcknowledgeBlockChange).
		VarInt(seq).
		Build()
}

// packBlockPos encodes absolute block coordinates as the Minecraft 64-bit
// packed Position: X(26 bits) | Z(26 bits) | Y(12 bits).
func packBlockPos(x, y, z int) int64 {
	return ((int64(x) & 0x3FFFFFF) << 38) |
		((int64(z) & 0x3FFFFFF) << 12) |
		(int64(y) & 0xFFF)
}

// containerTitle returns a human-readable title for a container block.
func containerTitle(blockName string) string {
	switch blockName {
	case "minecraft:crafting_table":
		return "Crafting"
	case "minecraft:furnace", "minecraft:lit_furnace":
		return "Furnace"
	case "minecraft:blast_furnace", "minecraft:lit_blast_furnace":
		return "Blast Furnace"
	case "minecraft:smoker", "minecraft:lit_smoker":
		return "Smoker"
	case "minecraft:anvil", "minecraft:chipped_anvil", "minecraft:damaged_anvil":
		return "Repair & Name"
	case "minecraft:enchanting_table":
		return "Enchant"
	case "minecraft:grindstone":
		return "Repair & Disenchant"
	case "minecraft:loom":
		return "Loom"
	case "minecraft:smithing_table":
		return "Upgrade Gear"
	case "minecraft:stonecutter":
		return "Stonecutter"
	case "minecraft:brewing_stand":
		return "Brewing Stand"
	case "minecraft:cartography_table":
		return "Cartography Table"
	case "minecraft:beacon":
		return "Beacon"
	case "minecraft:chest", "minecraft:trapped_chest", "minecraft:barrel":
		return "Chest"
	case "minecraft:ender_chest":
		return "Ender Chest"
	case "minecraft:hopper":
		return "Hopper"
	case "minecraft:dispenser":
		return "Dispenser"
	case "minecraft:dropper":
		return "Dropper"
	default:
		return "Container"
	}
}
