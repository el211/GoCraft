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
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"strconv"
	"strings"

	"GoCraft/core/blockloot"
	corentity "GoCraft/core/entity"
	coreexperience "GoCraft/core/experience"
	coreintent "GoCraft/core/intent"
	"GoCraft/core/itemregistry"
	"GoCraft/core/player"
	coreplugin "GoCraft/core/plugin"
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
	actionStatusDropStack     = 3
	actionStatusDropItem      = 4
	actionStatusSwapOffhand   = 6
)

// ── Dispatch ──────────────────────────────────────────────────────────────────

// handleBlockPacket dispatches an incoming block-interaction packet.
// Called from the play loop for packets that need the world and session manager.
func handleBlockPacket(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32, plugins *coreplugin.Bus, intents *coreintent.Bus) error {
	switch pkt.ID {
	case packetIDPlayerAction:
		return handlePlayerActionWithContext(pkt, p, w, mgr, conn, nextEntityID, plugins)
	case packetIDUseItemOn:
		return handleUseItemOnWithIntents(pkt, p, w, mgr, conn, nextEntityID, intents)
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
	nextID := int32(0)
	return handlePlayerActionWithContext(pkt, p, w, mgr, nil, func() int32 {
		nextID++
		return nextID
	}, nil)
}

func handlePlayerActionWithContext(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32, plugins *coreplugin.Bus) error {
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
	if status == 5 { // RELEASE_USE_ITEM
		releaseRangedItem(p, w, mgr, conn, nextEntityID)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if status == actionStatusDropStack || status == actionStatusDropItem {
		dropJavaHeldItem(p, w, mgr, conn, nextEntityID, status == actionStatusDropStack)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if status == actionStatusSwapOffhand {
		swapJavaOffhand(p, conn)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}

	// Reject out-of-bounds Y before touching the world.
	if int(by) < coreworld.WorldMinY || int(by) > coreworld.WorldMaxY {
		slog.Warn("player action: Y out of bounds", "player", p.Username, "y", by)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}

	broken := w.GetBlock(int(bx), int(by), int(bz))
	// Dragon egg teleports on any hit in non-creative.
	if broken.ResourceLocation() == "minecraft:dragon_egg" && p.GameMode != player.GameModeCreative &&
		status == actionStatusStartDigging {
		dragonEggTeleport(int(bx), int(by), int(bz), w, mgr)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !broken.IsAir() && digBreaksBlock(status, p.GameMode, broken.ResourceLocation()) {
		heldSlot := player.HotbarStart + p.HeldSlot
		held := p.Inventory[heldSlot]
		position := spatial.BlockPos{X: bx, Y: by, Z: bz}
		if plugins != nil && !plugins.EmitBlockBreak(p, position, broken, held.ItemID) {
			BroadcastBlockChange(coreworld.BlockChange{X: int(bx), Y: int(by), Z: int(bz), Block: broken}, mgr)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		lootBlock := broken
		potDecorations := [4]string{}
		if broken.ResourceLocation() == "minecraft:decorated_pot" {
			potDecorations = w.DecoratedPotDecorations(int(bx), int(by), int(bz))
		}
		containerItems := w.ContainerItems(int(bx), int(by), int(bz))
		if broken.ResourceLocation() == "minecraft:decorated_pot" && held.EnchantmentLevel("minecraft:silk_touch") == 0 && blockloot.BreaksDecoratedPot(held.ItemID) {
			lootBlock = copyBlockProperties(broken)
			lootBlock.Properties["cracked"] = "true"
		}
		enchantments := make(map[string]int)
		for _, enchantment := range held.EnchantmentLevels() {
			enchantments[enchantment.ID] = enchantment.Level
		}
		lootContext := blockloot.Context{
			Block:          lootBlock,
			Tool:           held,
			Enchantments:   enchantments,
			PotDecorations: potDecorations,
			BlockAt: func(dx, dy, dz int) coreworld.Block {
				return w.GetBlock(int(bx)+dx, int(by)+dy, int(bz)+dz)
			},
		}
		drops := blockloot.Drops(lootContext)
		slog.Info("block break", "player", p.Username,
			"x", bx, "y", by, "z", bz,
			"block", broken.ResourceLocation(),
			"mode", p.GameMode, "status", status)
		applyBlockChange(int(bx), int(by), int(bz), coreworld.Air, w, mgr)
		breakLinkedPlantHalf(int(bx), int(by), int(bz), broken, w, mgr)
		breakLinkedBedHalf(int(bx), int(by), int(bz), broken, w, mgr)
		breakLinkedDoorHalf(int(bx), int(by), int(bz), broken, w, mgr)
		unlinkChestPartner(int(bx), int(by), int(bz), broken, w, mgr)
		breakUnsupportedBlocksAboveWithDrops(int(bx), int(by), int(bz), w, mgr, nextEntityID, p.Dimension)
		// Client which sent this event produce sounds on its own
		// Maybe we should broadcast some sounds only to other players?
		broadcastSoundAt(mgr, blockBreakSound(broken.ResourceLocation()), soundCategoryBlocks,
			float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 0.8)
		w.EmitVibration(int(bx), int(by), int(bz))

		// Spawn block and container drops in survival/adventure mode.
		inventoryChanged := false
		if p.GameMode != player.GameModeCreative && p.GameMode != player.GameModeSpectator {
			dropPosition := spatial.Vec3{X: float64(bx) + 0.5, Y: float64(by) + 0.5, Z: float64(bz) + 0.5}
			ordinal := 0
			if coreworld.IsShulkerBox(broken.ResourceLocation()) {
				// Replace the generic drop with one that carries the contents.
				for i2, drop := range drops {
					if drop.ItemID != "" {
						drops[i2] = coreworld.ShulkerBoxDropItem(drop.ItemID, containerItems)
					}
				}
			} else if isJavaStorageContainer(broken.ResourceLocation()) || broken.ResourceLocation() == "minecraft:decorated_pot" || IsFurnaceContainer(broken.ResourceLocation()) || broken.ResourceLocation() == "minecraft:jukebox" || broken.ResourceLocation() == "minecraft:lectern" || broken.ResourceLocation() == "minecraft:chiseled_bookshelf" {
				for _, item := range containerItems {
					if item.ItemID != "" && item.Count > 0 {
						spawnBlockDrop(w, nextEntityID, dropPosition, item.Stack(), ordinal, mgr, p.Dimension)
						ordinal++
					}
				}
			}

			for _, drop := range drops {
				spawnBlockDrop(w, nextEntityID, dropPosition, drop, ordinal, mgr, p.Dimension)
				ordinal++
			}
			for _, orb := range coreexperience.SpawnOrbs(w, nextEntityID,
				dropPosition,
				blockloot.Experience(lootContext)) {
				BroadcastSpawnMobInDimension(orb, mgr, p.Dimension)
			}
			if wear := player.BlockUseDamage(p.Inventory[heldSlot].ItemID); wear > 0 {
				p.Inventory[heldSlot].ApplyDamage(wear)
				inventoryChanged = true
			}
		}
		// Clear orphaned container data regardless of game mode.
		if isJavaStorageContainer(broken.ResourceLocation()) || broken.ResourceLocation() == "minecraft:decorated_pot" || IsFurnaceContainer(broken.ResourceLocation()) || broken.ResourceLocation() == "minecraft:jukebox" || broken.ResourceLocation() == "minecraft:lectern" {
			w.SetContainerItems(int(bx), int(by), int(bz), broken.ResourceLocation(), nil)
		}
		// Sync inventory once if the held tool was damaged.
		if inventoryChanged {
			if sess, ok := mgr.Get(p.UUID); ok {
				p.ContainerStateID++
				_ = sendSetContainerContent(sess.Conn, p, p.ContainerStateID)
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

func dropJavaHeldItem(p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32, entireStack bool) {
	if p == nil || w == nil || p.Dead || p.GameMode == player.GameModeSpectator || p.HeldSlot < 0 || p.HeldSlot >= 9 {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	stack := p.Inventory[slot]
	if stack.IsEmpty() {
		return
	}
	dropped := stack
	if !entireStack {
		dropped.Count = 1
	}
	p.Inventory[slot].Count -= dropped.Count
	normalizeStack(&p.Inventory[slot])
	clearJavaFoodUse(p)
	spawnBlockDrop(w, nextEntityID, p.Position, dropped, 0, mgr, p.Dimension)
	if conn != nil {
		_ = SyncPlayerInventory(conn, p)
	} else {
		p.ContainerStateID++
	}
}

func swapJavaOffhand(p *player.Player, conn *network.ClientConn) {
	if p == nil || p.Dead || p.GameMode == player.GameModeSpectator || p.HeldSlot < 0 || p.HeldSlot >= 9 {
		return
	}
	held := player.HotbarStart + p.HeldSlot
	p.Inventory[held], p.Inventory[player.OffhandSlot] = p.Inventory[player.OffhandSlot], p.Inventory[held]
	clearJavaFoodUse(p)
	if conn != nil {
		_ = SyncPlayerInventory(conn, p)
	} else {
		p.ContainerStateID++
	}
}

func spawnBlockDrop(w *coreworld.World, nextEntityID func() int32, position spatial.Vec3, stack player.ItemStack, ordinal int, mgr *session.Manager, dimension int32) {
	if w == nil || nextEntityID == nil || stack.IsEmpty() {
		return
	}
	id := nextEntityID()
	var entityUUID [16]byte
	if _, err := cryptorand.Read(entityUUID[:]); err != nil {
		for index := range entityUUID {
			entityUUID[index] = byte(uint32(id) >> (uint(index%4) * 8))
		}
	}
	entityUUID[6] = (entityUUID[6] & 0x0f) | 0x40
	entityUUID[8] = (entityUUID[8] & 0x3f) | 0x80
	dropped := corentity.New(id, entityUUID, corentity.TypeItem, position.X, position.Y+0.25, position.Z)
	dropped.SetDroppedItem(stack)
	angle := float64(id+int32(ordinal)*17) * 2.399963229728653
	dropped.VX, dropped.VY, dropped.VZ = math.Cos(angle)*0.1, 0.2, math.Sin(angle)*0.1
	w.Entities.Add(dropped)
	BroadcastSpawnMobInDimension(dropped, mgr, dimension)
}

func primeJavaTNT(x, y, z int, w *coreworld.World, mgr *session.Manager, nextEntityID func() int32, dimension int32) bool {
	if nextEntityID == nil || w.GetBlock(x, y, z).ResourceLocation() != "minecraft:tnt" {
		return false
	}
	applyBlockChange(x, y, z, coreworld.Air, w, mgr)
	id := nextEntityID()
	var uuid [16]byte
	if _, err := cryptorand.Read(uuid[:]); err != nil {
		uuid[0], uuid[1], uuid[2], uuid[3] = byte(id>>24), byte(id>>16), byte(id>>8), byte(id)
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	tnt := corentity.NewPrimedTNT(id, uuid, float64(x)+0.5, float64(y), float64(z)+0.5)
	w.Entities.Add(tnt)
	BroadcastSpawnMobInDimension(tnt, mgr, dimension)
	return true
}

func finishJavaIgniterUse(p *player.Player, itemID string) {
	if itemID != "minecraft:fire_charge" {
		return
	}
	if p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	p.Inventory[slot].Count--
	normalizeStack(&p.Inventory[slot])
}

func breakLinkedPlantHalf(x, y, z int, broken coreworld.Block, w *coreworld.World, mgr *session.Manager) {
	otherY, wantHalf, ok := coreworld.DoublePlantPartnerY(broken, y)
	if !ok {
		return
	}
	other := w.GetBlock(x, otherY, z)
	if other.ResourceLocation() == broken.ResourceLocation() && other.Properties["half"] == wantHalf {
		applyBlockChange(x, otherY, z, coreworld.Air, w, mgr)
	}
}

func breakUnsupportedBlocksAbove(x, y, z int, w *coreworld.World, mgr *session.Manager) {
	breakUnsupportedBlocksAboveWithDrops(x, y, z, w, mgr, nil, 0)
}

func breakUnsupportedBlocksAboveWithDrops(x, y, z int, w *coreworld.World, mgr *session.Manager, nextEntityID func() int32, dimension int32) {
	for updateIndex, update := range w.ApplyAttachmentSupportUpdatesAround(x, y, z) {
		if mgr != nil {
			BroadcastBlockChange(update.Change, mgr)
		}
		if !update.Removed {
			continue
		}
		dropPosition := spatial.Vec3{
			X: float64(update.Change.X) + 0.5,
			Y: float64(update.Change.Y) + 0.5,
			Z: float64(update.Change.Z) + 0.5,
		}
		for dropIndex, drop := range blockloot.Drops(blockloot.Context{Block: update.Previous}) {
			spawnBlockDrop(w, nextEntityID, dropPosition, drop, updateIndex*16+dropIndex, mgr, dimension)
		}
	}
	for _, change := range w.BreakUnsupportedCropsAbove(x, y, z) {
		if mgr != nil {
			BroadcastBlockChange(change, mgr)
		}
	}
	for _, change := range w.BreakUnsupportedCocoaAdjacentTo(x, y, z) {
		if mgr != nil {
			BroadcastBlockChange(change, mgr)
		}
	}
	for plantY := y + 1; plantY <= coreworld.WorldMaxY; plantY++ {
		plant := w.GetBlock(x, plantY, z)
		if !coreworld.RequiresGroundSupport(plant) || javaGroundSupportedBlockCanSurvive(plant, w.GetBlock(x, plantY-1, z)) {
			return
		}
		partnerY, partnerHalf, hasPartner := coreworld.DoublePlantPartnerY(plant, plantY)
		applyBlockChange(x, plantY, z, coreworld.Air, w, mgr)
		if hasPartner {
			partner := w.GetBlock(x, partnerY, z)
			if partner.ResourceLocation() == plant.ResourceLocation() && partner.Properties["half"] == partnerHalf {
				applyBlockChange(x, partnerY, z, coreworld.Air, w, mgr)
			}
		}
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
	case "minecraft:crafter":
		return 7 // minecraft:crafter_3x3
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
func handleUseItemOn(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32) error {
	return handleUseItemOnWithIntents(pkt, p, w, mgr, conn, nextEntityID, nil)
}

func handleUseItemOnWithIntents(pkt *protocol.Packet, p *player.Player, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn, nextEntityID func() int32, intents *coreintent.Bus) error {
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
	cursorX, err := protocol.ReadFloat(r)
	if err != nil {
		return fmt.Errorf("use item on: reading cursor_x: %w", err)
	}
	cursorY, err := protocol.ReadFloat(r)
	if err != nil {
		return fmt.Errorf("use item on: reading cursor_y: %w", err)
	}
	cursorZ, err := protocol.ReadFloat(r)
	if err != nil {
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
	held := p.HeldItem()
	if hand == 0 && held.ItemID == "minecraft:firework_rocket" &&
		p.GameMode != player.GameModeSpectator {
		if intents != nil {
			intents.PostFireworkUse(coreintent.FireworkUseIntent{
				PlayerUUID: p.UUID,
				HotbarSlot: int32(p.HeldSlot),
				Position: spatial.Vec3{
					X: float64(bx) + float64(cursorX),
					Y: float64(by) + float64(cursorY),
					Z: float64(bz) + float64(cursorZ),
				},
			})
		}
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if targetBlock.ResourceLocation() == "minecraft:sweet_berry_bush" &&
		(held.ItemID != "minecraft:bone_meal" || coreworld.CropAge(targetBlock) >= 3) {
		if count, changes, harvested := w.HarvestSweetBerryBush(int(bx), int(by), int(bz), rand.Uint64()); harvested {
			for _, change := range changes {
				if mgr != nil {
					BroadcastBlockChange(change, mgr)
				}
			}
			p.GiveItem(player.ItemStack{ItemID: "minecraft:sweet_berries", Count: count})
			p.ContainerStateID++
			if sess, ok := mgr.Get(p.UUID); ok {
				_ = sendSetContainerContent(sess.Conn, p, p.ContainerStateID)
			}
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	heldBefore := held.ItemID
	usedDamageableTool := isBlockUseTool(heldBefore)
	if hand == 0 && useToolOrPlant(int(bx), int(by), int(bz), face, targetBlock, p, w, mgr, nextEntityID) {
		sendAcknowledgeBlockChange(mgr, p, seq)
		if usedDamageableTool {
			damageHeldItem(p, conn, 1)
		} else {
			p.ContainerStateID++
			if sess, ok := mgr.Get(p.UUID); ok {
				_ = sendSetContainerContent(sess.Conn, p, p.ContainerStateID)
			}
		}
		return nil
	}
	if hand == 0 && held.ItemID == "minecraft:honeycomb" && p.GameMode != player.GameModeSpectator {
		if waxed, ok := coreworld.WaxCopper(targetBlock); ok {
			applyBlockChange(int(bx), int(by), int(bz), waxed, w, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
			}
			broadcastSoundAt(mgr, "minecraft:item.honeycomb.wax_on", soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && held.ItemID == "minecraft:shears" && p.GameMode != player.GameModeSpectator {
		if targetBlock.ResourceLocation() == "minecraft:tripwire" && targetBlock.Properties["disarmed"] != "true" {
			disarmed := copyBlockProperties(targetBlock)
			disarmed.Properties["disarmed"] = "true"
			applyBlockChange(int(bx), int(by), int(bz), disarmed, w, mgr)
			if targetBlock.Properties["powered"] != "true" {
				str := player.ItemStack{ItemID: "minecraft:string", Count: 1}
				if !p.GiveItem(str) {
					spawnBlockDrop(w, nextEntityID, p.Position, str, 0, mgr, p.Dimension)
				}
			}
			damageHeldItem(p, conn, 1)
			broadcastSoundAt(mgr, "minecraft:block.tripwire.detach", soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		if carved, ok := coreworld.CarvePumpkin(targetBlock, chestFacingFromYaw(p.Rotation.Yaw)); ok {
			applyBlockChange(int(bx), int(by), int(bz), carved, w, mgr)
			seeds := player.ItemStack{ItemID: "minecraft:pumpkin_seeds", Count: 4}
			if !p.GiveItem(seeds) {
				spawnBlockDrop(w, nextEntityID, p.Position, seeds, 0, mgr, p.Dimension)
			}
			if !damageHeldItem(p, conn, 1) {
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
			}
			broadcastSoundAt(mgr, "minecraft:block.pumpkin.carve", soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && p.GameMode != player.GameModeSpectator {
		if harvested, output, ok := coreworld.HarvestBeehive(targetBlock, held.ItemID); ok {
			applyBlockChange(int(bx), int(by), int(bz), harvested, w, mgr)
			sound := "minecraft:item.bottle.fill"
			if held.ItemID == "minecraft:shears" {
				sound = "minecraft:block.beehive.shear"
				if !p.GiveItem(output) {
					spawnBlockDrop(w, nextEntityID, p.Position, output, 0, mgr, p.Dimension)
				}
				if damageHeldItem(p, conn, 1) {
					output = player.ItemStack{}
				}
			} else {
				replaceJavaBucket(p, output.ItemID)
			}
			if !output.IsEmpty() {
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
			}
			broadcastSoundAt(mgr, sound, soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && p.GameMode != player.GameModeSpectator {
		if candleCake, ok := coreworld.AddCandleToCake(targetBlock, held.ItemID); ok {
			applyBlockChange(int(bx), int(by), int(bz), candleCake, w, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
			}
			broadcastSoundAt(mgr, "minecraft:block.cake.add_candle", soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && p.GameMode != player.GameModeSpectator && targetBlock.ResourceLocation() == "minecraft:flower_pot" {
		if potted, ok := coreworld.PottedBlock(held.ItemID); ok {
			applyBlockChange(int(bx), int(by), int(bz), potted, w, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			if conn != nil {
				_ = SyncPlayerInventory(conn, p)
			} else {
				p.ContainerStateID++
			}
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && p.GameMode != player.GameModeSpectator {
		if pottedItem, ok := coreworld.PottedItem(targetBlock); ok {
			if _, canPot := coreworld.PottedBlock(held.ItemID); canPot {
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
			applyBlockChange(int(bx), int(by), int(bz), coreworld.Block{Namespace: "minecraft", Name: "flower_pot"}, w, mgr)
			returned := player.ItemStack{ItemID: pottedItem, Count: 1}
			if !p.GiveItem(returned) {
				spawnBlockDrop(w, nextEntityID, p.Position, returned, 0, mgr, p.Dimension)
			}
			if conn != nil {
				_ = SyncPlayerInventory(conn, p)
			} else {
				p.ContainerStateID++
			}
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && p.GameMode != player.GameModeSpectator {
		if composted, consumed, schedule := coreworld.AddToComposter(targetBlock, held.ItemID, int(bx), int(by), int(bz), w.PhysicsTime()); consumed {
			applyBlockChange(int(bx), int(by), int(bz), composted, w, mgr)
			if schedule {
				w.BlockPhysics.ScheduleComposter(int(bx), int(by), int(bz), w.PhysicsTime(), 20)
			}
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			if conn != nil {
				_ = SyncPlayerInventory(conn, p)
			} else {
				p.ContainerStateID++
			}
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if hand == 0 && p.GameMode != player.GameModeSpectator {
		if charged, ok := coreworld.ChargeRespawnAnchor(targetBlock, held.ItemID); ok {
			applyBlockChange(int(bx), int(by), int(bz), charged, w, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
			}
			broadcastSoundAt(mgr, "minecraft:block.respawn_anchor.charge", soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}

	// Jukebox: insert a music disc or eject the current one.
	if hand == 0 && p.GameMode != player.GameModeSpectator &&
		targetBlock.ResourceLocation() == "minecraft:jukebox" {
		be := w.GetBlockEntity(int(bx), int(by), int(bz))
		stored := coreworld.JukeboxRecordItem(be)
		if stored != "" {
			// Eject current record.
			if ejected, cleared, ok := coreworld.EjectJukeboxRecord(targetBlock, stored); ok {
				applyBlockChange(int(bx), int(by), int(bz), cleared, w, mgr)
				w.SetContainerItems(int(bx), int(by), int(bz), "minecraft:jukebox", nil)
				drop := player.ItemStack{ItemID: ejected, Count: 1}
				if !p.GiveItem(drop) {
					spawnBlockDrop(w, nextEntityID, p.Position, drop, 0, mgr, p.Dimension)
				}
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
				broadcastSoundAt(mgr, "minecraft:block.jukebox.stop_record", soundCategoryBlocks,
					float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		} else if coreworld.IsMusicDisc(held.ItemID) {
			// Insert new record.
			if updated, ok := coreworld.InsertJukeboxRecord(targetBlock, held.ItemID); ok {
				applyBlockChange(int(bx), int(by), int(bz), updated, w, mgr)
				items := []coreworld.ContainerItem{{Slot: 0, ItemID: held.ItemID, Count: 1}}
				w.SetContainerItems(int(bx), int(by), int(bz), "minecraft:jukebox", items)
				if p.GameMode != player.GameModeCreative {
					slot := player.HotbarStart + p.HeldSlot
					p.Inventory[slot].Count--
					normalizeStack(&p.Inventory[slot])
					if conn != nil {
						_ = SyncPlayerInventory(conn, p)
					} else {
						p.ContainerStateID++
					}
				}
				broadcastSoundAt(mgr, coreworld.MusicDiscSound(held.ItemID), soundCategoryRecords,
					float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 4, 1)
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		}
	}

	// Chiseled bookshelf: targeted book insert/remove.
	if hand == 0 && p.GameMode != player.GameModeSpectator &&
		targetBlock.ResourceLocation() == "minecraft:chiseled_bookshelf" {
		facing := targetBlock.Properties["facing"]
		slot := coreworld.ChiseledBookshelfSlot(facing, float64(cursorX), float64(cursorY), float64(cursorZ))
		be := w.GetBlockEntity(int(bx), int(by), int(bz))
		slotProp := fmt.Sprintf("slot_%d_occupied", slot)
		if targetBlock.Properties[slotProp] == "true" {
			// Eject book from this slot.
			storedID := ""
			for _, ci := range be.Items {
				if ci.Slot == slot {
					storedID = ci.ItemID
					break
				}
			}
			if _, cleared, ok2 := coreworld.EjectBookshelfBook(targetBlock, slot, storedID); ok2 {
				applyBlockChange(int(bx), int(by), int(bz), cleared, w, mgr)
				newItems := make([]coreworld.ContainerItem, 0, 6)
				for _, ci := range be.Items {
					if ci.Slot != slot {
						newItems = append(newItems, ci)
					}
				}
				w.SetContainerItems(int(bx), int(by), int(bz), "minecraft:chiseled_bookshelf", newItems)
				w.SetBookshelfLastSlot(int(bx), int(by), int(bz), slot+1)
				drop := player.ItemStack{ItemID: storedID, Count: 1}
				if !p.GiveItem(drop) {
					spawnBlockDrop(w, nextEntityID, p.Position, drop, 0, mgr, p.Dimension)
				}
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		} else if coreworld.IsBookshelfBook(held.ItemID) {
			// Insert book into empty slot.
			if updated, ok2 := coreworld.InsertBookshelfBook(targetBlock, slot, held.ItemID); ok2 {
				applyBlockChange(int(bx), int(by), int(bz), updated, w, mgr)
				newItems := append(be.Items, coreworld.ContainerItem{Slot: slot, ItemID: held.ItemID, Count: 1})
				w.SetContainerItems(int(bx), int(by), int(bz), "minecraft:chiseled_bookshelf", newItems)
				w.SetBookshelfLastSlot(int(bx), int(by), int(bz), slot+1)
				if p.GameMode != player.GameModeCreative {
					heldInvSlot := player.HotbarStart + p.HeldSlot
					p.Inventory[heldInvSlot].Count--
					normalizeStack(&p.Inventory[heldInvSlot])
					if conn != nil {
						_ = SyncPlayerInventory(conn, p)
					} else {
						p.ContainerStateID++
					}
				}
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		}
	}

	// Lectern: place a book or eject the current one.
	if hand == 0 && p.GameMode != player.GameModeSpectator &&
		targetBlock.ResourceLocation() == "minecraft:lectern" {
		be := w.GetBlockEntity(int(bx), int(by), int(bz))
		stored := coreworld.LecternBook(be)
		if stored != "" && !coreworld.IsLecternBook(held.ItemID) {
			// Eject book (not holding a book).
			if _, cleared, ok := coreworld.EjectLecternBook(targetBlock, stored); ok {
				applyBlockChange(int(bx), int(by), int(bz), cleared, w, mgr)
				w.SetContainerItems(int(bx), int(by), int(bz), "minecraft:lectern", nil)
				drop := player.ItemStack{ItemID: stored, Count: 1}
				if !p.GiveItem(drop) {
					spawnBlockDrop(w, nextEntityID, p.Position, drop, 0, mgr, p.Dimension)
				}
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		} else if stored == "" && coreworld.IsLecternBook(held.ItemID) {
			// Place book.
			if updated, ok := coreworld.InsertLecternBook(targetBlock, held.ItemID); ok {
				applyBlockChange(int(bx), int(by), int(bz), updated, w, mgr)
				items := []coreworld.ContainerItem{{Slot: 0, ItemID: held.ItemID, Count: 1}}
				w.SetContainerItems(int(bx), int(by), int(bz), "minecraft:lectern", items)
				if p.GameMode != player.GameModeCreative {
					slot := player.HotbarStart + p.HeldSlot
					p.Inventory[slot].Count--
					normalizeStack(&p.Inventory[slot])
					if conn != nil {
						_ = SyncPlayerInventory(conn, p)
					} else {
						p.ContainerStateID++
					}
				}
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		}
	}

	// Sneaking with an item bypasses block activation so a block can be placed
	// against doors, containers, workstations, composters, and other UIs.
	bypassActivation := p.Sneaking && !held.IsEmpty()
	if !bypassActivation && p.GameMode != player.GameModeSpectator {
		if emptied, ready := coreworld.EmptyComposter(targetBlock); ready {
			applyBlockChange(int(bx), int(by), int(bz), emptied, w, mgr)
			boneMeal := player.ItemStack{ItemID: "minecraft:bone_meal", Count: 1}
			if !p.GiveItem(boneMeal) {
				spawnBlockDrop(w, nextEntityID, p.Position, boneMeal, 0, mgr, p.Dimension)
			}
			if conn != nil {
				_ = SyncPlayerInventory(conn, p)
			} else {
				p.ContainerStateID++
			}
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if !bypassActivation && p.GameMode != player.GameModeSpectator && targetBlock.ResourceLocation() == "minecraft:bell" {
		if _, valid := coreworld.BellRingDirection(targetBlock, face, cursorY); valid {
			if intents != nil {
				intents.PostBellRing(coreintent.BellRingIntent{
					PlayerUUID: p.UUID,
					Position:   spatial.BlockPos{X: bx, Y: by, Z: bz},
					Face:       face,
					HitY:       cursorY,
				})
			}
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}
	if !bypassActivation && toggleTrapdoor(int(bx), int(by), int(bz), targetBlock, w, mgr) {
		sound := "minecraft:block.wooden_trapdoor.open"
		if targetBlock.Properties["open"] == "true" {
			sound = "minecraft:block.wooden_trapdoor.close"
		}
		broadcastSoundAt(mgr, sound, soundCategoryBlocks,
			float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !bypassActivation && toggleDoor(int(bx), int(by), int(bz), targetBlock, w, mgr) {
		// Client which sent this event plays sound twice.
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
	// Bed right-click: sleeping interaction.
	if !bypassActivation && isBedBlock(targetBlock.ResourceLocation()) {
		handleBedInteract(p, int(bx), int(by), int(bz), w, conn, mgr)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}

	// Lever right-click: toggle powered state.
	if !bypassActivation && targetBlock.ResourceLocation() == "minecraft:lever" {
		toggled := copyBlockProperties(targetBlock)
		if toggled.Properties["powered"] == "true" {
			toggled.Properties["powered"] = "false"
		} else {
			toggled.Properties["powered"] = "true"
		}
		applyBlockChange(int(bx), int(by), int(bz), toggled, w, mgr)
		sound := "minecraft:block.lever.click"
		broadcastSoundAt(mgr, sound, soundCategoryBlocks,
			float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 0.3, 0.6)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !bypassActivation && strings.HasSuffix(targetBlock.ResourceLocation(), "_button") {
		if targetBlock.Properties["powered"] != "true" {
			pressed := copyBlockProperties(targetBlock)
			pressed.Properties["powered"] = "true"
			applyBlockChange(int(bx), int(by), int(bz), pressed, w, mgr)
			delay := int64(30)
			if targetBlock.ResourceLocation() == "minecraft:stone_button" ||
				targetBlock.ResourceLocation() == "minecraft:polished_blackstone_button" {
				delay = 20
			}
			w.BlockPhysics.ScheduleButton(int(bx), int(by), int(bz), w.PhysicsTime(), delay)
		}
		broadcastSoundAt(mgr, "minecraft:block.stone_button.click_on", soundCategoryBlocks,
			float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 0.3, 0.6)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !bypassActivation && targetBlock.ResourceLocation() == "minecraft:repeater" {
		updated := copyBlockProperties(targetBlock)
		delay, _ := strconv.Atoi(updated.Properties["delay"])
		updated.Properties["delay"] = strconv.Itoa(delay%4 + 1)
		applyBlockChange(int(bx), int(by), int(bz), updated, w, mgr)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !bypassActivation && targetBlock.ResourceLocation() == "minecraft:comparator" {
		updated := copyBlockProperties(targetBlock)
		if updated.Properties["mode"] == "subtract" {
			updated.Properties["mode"] = "compare"
		} else {
			updated.Properties["mode"] = "subtract"
		}
		applyBlockChange(int(bx), int(by), int(bz), updated, w, mgr)
		sendAcknowledgeBlockChange(mgr, p, seq)
		return nil
	}
	if !bypassActivation && targetBlock.ResourceLocation() == "minecraft:note_block" {
		blockBelow := w.GetBlock(int(bx), int(by)-1, int(bz))
		if tuned, ok := coreworld.TuneNoteBlock(targetBlock, blockBelow); ok {
			applyBlockChange(int(bx), int(by), int(bz), tuned, w, mgr)
			instrument := tuned.Properties["instrument"]
			if instrument == "" {
				instrument = "harp"
			}
			note, _ := strconv.Atoi(tuned.Properties["note"])
			pitch := float32(math.Pow(2, (float64(note)-12)/12))
			broadcastSoundAt(mgr, "minecraft:block.note_block."+instrument, soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 3, pitch)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}

	// Dragon egg right-click: teleport to random position (like PumpkinMC dragon_egg.rs).
	if !bypassActivation && targetBlock.ResourceLocation() == "minecraft:dragon_egg" && p.GameMode != player.GameModeCreative {
		if dragonEggTeleport(int(bx), int(by), int(bz), w, mgr) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		return nil
	}

	// Cake right-click: consume a slice (based on PumpkinMC cake.rs).
	if !bypassActivation && targetBlock.ResourceLocation() == "minecraft:cake" {
		if p.GameMode != player.GameModeSpectator {
			if eatCakeSlice(p, int(bx), int(by), int(bz), targetBlock, w, mgr, conn) {
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
		}
	}

	// Candle right-click: snuff lit candle, or add candle to unlit candle stack.
	if !bypassActivation && isCandleBlock(targetBlock.ResourceLocation()) {
		if targetBlock.Properties["lit"] == "true" {
			// Snuff the candle.
			snuffed := copyBlockProperties(targetBlock)
			snuffed.Properties["lit"] = "false"
			applyBlockChange(int(bx), int(by), int(bz), snuffed, w, mgr)
			broadcastSoundAt(mgr, "minecraft:block.candle.extinguish", soundCategoryBlocks,
				float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 1)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		// Add another candle (same color, up to 4).
		if !held.IsEmpty() && held.ItemID == candleItemForBlock(targetBlock.ResourceLocation()) {
			candles, _ := strconv.Atoi(targetBlock.Properties["candles"])
			if candles < 4 {
				added := copyBlockProperties(targetBlock)
				added.Properties["candles"] = strconv.Itoa(candles + 1)
				applyBlockChange(int(bx), int(by), int(bz), added, w, mgr)
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
		}
	}

	if menuType := containerMenuType(targetBlock.ResourceLocation()); !bypassActivation && menuType >= 0 {
		title := containerTitle(targetBlock.ResourceLocation())
		slog.Info("container opened", "player", p.Username, "block", targetBlock.ResourceLocation())
		sendAcknowledgeBlockChange(mgr, p, seq)
		if targetBlock.ResourceLocation() == "minecraft:crafting_table" {
			return openCraftingTable(p, conn)
		}
		if isJavaStorageContainer(targetBlock.ResourceLocation()) {
			return openStorageContainer(p, conn, w, spatial.BlockPos{X: bx, Y: by, Z: bz}, targetBlock.ResourceLocation())
		}
		if IsFurnaceContainer(targetBlock.ResourceLocation()) {
			return openFurnace(p, conn, w, spatial.BlockPos{X: bx, Y: by, Z: bz}, targetBlock.ResourceLocation())
		}
		if IsWorkstation(targetBlock.ResourceLocation()) {
			return openWorkstation(p, conn, w, spatial.BlockPos{X: bx, Y: by, Z: bz}, targetBlock.ResourceLocation())
		}
		return sendOpenScreen(conn, 1, menuType, title)
	}

	// Boat items: right-clicking water (or any surface) with a boat item spawns
	// a boat entity instead of placing a block.
	if !held.IsEmpty() && isBoatItem(held.ItemID) && nextEntityID != nil &&
		p.GameMode != player.GameModeAdventure && p.GameMode != player.GameModeSpectator {
		if boat := spawnBoatFromItem(held.ItemID, int(bx), int(by), int(bz), int(face), w, nextEntityID); boat != nil {
			boat.Yaw = p.Rotation.Yaw
			BroadcastSpawnMob(boat, mgr)
			consumePlacedBoat(p, conn)
			sendAcknowledgeBlockChange(mgr, p, seq)
			slog.Info("player placed boat", "player", p.Username, "type", boat.Type)
		} else {
			sendAcknowledgeBlockChange(mgr, p, seq)
		}
		return nil
	}
	if !held.IsEmpty() {
		if _, minecart := javaMinecartType(held.ItemID); minecart {
			placeJavaMinecart(p, conn, mgr, w, int(bx), int(by), int(bz), nextEntityID)
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	}

	// Resolve the held block before choosing whether a replaceable target is
	// overwritten or the adjacent face receives the placement.
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
	// Track if we are placing into water so we can waterlog the block.
	placingInWater := existing.ResourceLocation() == "minecraft:water"
	if !existing.IsAir() && existing.ResourceLocation() != "minecraft:water" && existing.ResourceLocation() != "minecraft:lava" {
		breakLinkedPlantHalf(px, py, pz, existing, w, mgr)
	}

	block := javaworld.ItemIDToBlock(held.ItemID)
	// Apply waterlogged property when placing into a water source.
	if placingInWater && blockSupportsWaterlogging(block.ResourceLocation()) {
		if block.Properties == nil {
			block.Properties = map[string]string{}
		}
		block.Properties["waterlogged"] = "true"
	}
	// Slab merging: when clicking a slab of the same block type at the matching
	// half, replace it with a double slab at the slab's position instead of
	// placing a new block at the adjacent position.
	if strings.HasSuffix(block.ResourceLocation(), "_slab") {
		targetSlab := w.GetBlock(int(bx), int(by), int(bz))
		if targetSlab.ResourceLocation() == block.ResourceLocation() {
			targetType := targetSlab.Properties["type"]
			canMerge := false
			switch face {
			case 1: // clicking top of target
				canMerge = targetType == "bottom"
			case 0: // clicking bottom of target
				canMerge = targetType == "top"
			default:
				// clicking a side: merge if cursor Y places us on the matching half
				if cursorY < 0.5 {
					canMerge = targetType == "bottom"
				} else {
					canMerge = targetType == "top"
				}
			}
			if canMerge {
				double := copyBlockProperties(targetSlab)
				double.Properties["type"] = "double"
				applyBlockChange(int(bx), int(by), int(bz), double, w, mgr)
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
		}
		// Place as top/bottom slab based on face and cursor position.
		slabType := "bottom"
		if face == 0 || (face >= 2 && cursorY >= 0.5) {
			slabType = "top"
		}
		block.Properties = map[string]string{"type": slabType, "waterlogged": "false"}
	}
	if block.ResourceLocation() == "minecraft:redstone_wire" {
		if !javaSupportsRedstoneComponent(w.GetBlock(px, py-1, pz)) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		block.Properties = redstoneWireConnections(px, py, pz, w)
	}
	broadcastSoundAt(mgr, blockBreakSound(block.ResourceLocation()), soundCategoryBlocks,
		float64(bx)+0.5, float64(by)+0.5, float64(bz)+0.5, 1, 0.8)
	slog.Info("block place", "player", p.Username,
		"block", block.ResourceLocation(), "x", px, "y", py, "z", pz)
	switch {
	case coreworld.IsAttachmentPlacementItem(block.ResourceLocation()):
		placed, _, ok := coreworld.AttachmentPlacementState(w, block, px, py, pz, face, javaAttachmentRotation(p.Rotation.Yaw), placingInWater)
		if !ok {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		applyBlockChange(px, py, pz, placed, w, mgr)
	case block.ResourceLocation() == "minecraft:chest" || block.ResourceLocation() == "minecraft:trapped_chest":
		placeChestBlock(p, px, py, pz, block.ResourceLocation(), w, mgr)
		w.SetContainerItems(px, py, pz, block.ResourceLocation(), nil)
	case IsFurnaceContainer(block.ResourceLocation()):
		block.Properties = map[string]string{"facing": chestFacingFromYaw(p.Rotation.Yaw), "lit": "false"}
		applyBlockChange(px, py, pz, block, w, mgr)
		w.SetContainerItems(px, py, pz, block.ResourceLocation(), nil)
	case isBedBlock(block.ResourceLocation()):
		if !placeBedBlock(p, px, py, pz, block.ResourceLocation(), w, mgr) {
			// No room for the head half — cancel placement entirely.
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	case isDoorBlock(block.ResourceLocation()):
		if !placeDoorBlock(p, px, py, pz, block.ResourceLocation(), cursorX, cursorZ, w, mgr) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	case isTrapdoorBlock(block.ResourceLocation()):
		placed, ok := javaTrapdoorPlacementState(block, face, cursorY, p.Rotation.Yaw, w, px, py, pz)
		if !ok {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		applyBlockChange(px, py, pz, placed, w, mgr)
	case strings.HasSuffix(block.ResourceLocation(), "_button"):
		placed, ok := javaButtonPlacementState(block, face, p.Rotation.Yaw, w, px, py, pz)
		if !ok {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		applyBlockChange(px, py, pz, placed, w, mgr)
	case block.ResourceLocation() == "minecraft:lever":
		// Lever uses the same face/facing layout as buttons.
		placed, ok := javaButtonPlacementState(block, face, p.Rotation.Yaw, w, px, py, pz)
		if !ok {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		placed.Properties["powered"] = "false"
		applyBlockChange(px, py, pz, placed, w, mgr)
	case block.ResourceLocation() == "minecraft:torch" || block.ResourceLocation() == "minecraft:soul_torch" ||
		block.ResourceLocation() == "minecraft:redstone_torch":
		if face == 1 && coreworld.IsSolidLandingSurface(w.GetBlock(px, py-1, pz).ResourceLocation()) {
			if block.ResourceLocation() == "minecraft:redstone_torch" {
				block.Properties = map[string]string{"lit": "true"}
			} else {
				block.Properties = nil
			}
			applyBlockChange(px, py, pz, block, w, mgr)
		} else if face >= 2 && face <= 5 {
			offset := faceOffset[face]
			support := w.GetBlock(px-int(offset[0]), py-int(offset[1]), pz-int(offset[2]))
			if !coreworld.IsSolidLandingSurface(support.ResourceLocation()) {
				sendAcknowledgeBlockChange(mgr, p, seq)
				return nil
			}
			block.Name = strings.Replace(block.Name, "_torch", "_wall_torch", 1)
			if block.Name == "torch" {
				block.Name = "wall_torch"
			}
			block.Properties = map[string]string{"facing": javaFacingForFace(face)}
			if strings.Contains(block.Name, "redstone") {
				block.Properties["lit"] = "true"
			}
			applyBlockChange(px, py, pz, block, w, mgr)
		} else {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
	case block.ResourceLocation() == "minecraft:chain" ||
		block.ResourceLocation() == "minecraft:end_rod" ||
		strings.HasSuffix(block.ResourceLocation(), "_log") ||
		strings.HasSuffix(block.ResourceLocation(), "_wood") ||
		strings.HasSuffix(block.ResourceLocation(), "_stem") ||
		strings.HasSuffix(block.ResourceLocation(), "_hyphae") ||
		block.ResourceLocation() == "minecraft:bamboo_block" ||
		strings.HasSuffix(block.ResourceLocation(), "_pillar") ||
		block.ResourceLocation() == "minecraft:basalt" ||
		block.ResourceLocation() == "minecraft:polished_basalt" ||
		block.ResourceLocation() == "minecraft:bone_block" ||
		block.ResourceLocation() == "minecraft:hay_block" ||
		block.ResourceLocation() == "minecraft:purpur_pillar" ||
		block.ResourceLocation() == "minecraft:quartz_pillar":
		// Axis-rotatable blocks: face determines orientation.
		axis := "y"
		switch face {
		case 2, 3: // north/south face → z axis
			axis = "z"
		case 4, 5: // west/east face → x axis
			axis = "x"
		}
		block.Properties = map[string]string{"axis": axis}
		applyBlockChange(px, py, pz, block, w, mgr)
	case block.ResourceLocation() == "minecraft:repeater":
		if !javaSupportsRedstoneComponent(w.GetBlock(px, py-1, pz)) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		// A diode's facing points toward its input (the placing player); its
		// output is on the opposite side.
		block.Properties = map[string]string{
			"delay": "1", "facing": chestFacingFromYaw(p.Rotation.Yaw),
			"locked": "false", "powered": "false",
		}
		applyBlockChange(px, py, pz, block, w, mgr)
	case block.ResourceLocation() == "minecraft:comparator":
		if !javaSupportsRedstoneComponent(w.GetBlock(px, py-1, pz)) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		block.Properties = map[string]string{
			"facing": chestFacingFromYaw(p.Rotation.Yaw),
			"mode":   "compare", "powered": "false",
		}
		applyBlockChange(px, py, pz, block, w, mgr)
	case strings.HasSuffix(block.ResourceLocation(), "_pressure_plate"):
		if !javaSupportsRedstoneComponent(w.GetBlock(px, py-1, pz)) {
			sendAcknowledgeBlockChange(mgr, p, seq)
			return nil
		}
		if block.ResourceLocation() == "minecraft:light_weighted_pressure_plate" ||
			block.ResourceLocation() == "minecraft:heavy_weighted_pressure_plate" {
			block.Properties = map[string]string{"power": "0"}
		} else {
			block.Properties = map[string]string{"powered": "false"}
		}
		applyBlockChange(px, py, pz, block, w, mgr)
		w.BlockPhysics.SchedulePressurePlate(px, py, pz, w.PhysicsTime(), 1)
	case isJavaStorageContainer(block.ResourceLocation()):
		switch block.ResourceLocation() {
		case "minecraft:hopper":
			block.Properties = map[string]string{"facing": javaHopperFacing(face), "enabled": "true"}
		case "minecraft:dispenser", "minecraft:dropper":
			block.Properties = map[string]string{"facing": chestFacingFromYaw(p.Rotation.Yaw), "triggered": "false"}
		case "minecraft:crafter":
			block.Properties = map[string]string{"orientation": "north_up", "crafting": "false", "triggered": "false"}
		default:
			if isShulkerBox(block.ResourceLocation()) {
				block.Properties = map[string]string{"facing": shulkerBoxFacing(face)}
			}
		}
		applyBlockChange(px, py, pz, block, w, mgr)
		w.SetContainerItems(px, py, pz, block.ResourceLocation(), nil)
	case block.ResourceLocation() == "minecraft:decorated_pot":
		block.Properties = map[string]string{
			"facing": chestFacingFromYaw(p.Rotation.Yaw), "cracked": "false",
			"waterlogged": strconv.FormatBool(placingInWater),
		}
		applyBlockChange(px, py, pz, block, w, mgr)
		w.SetContainerItems(px, py, pz, block.ResourceLocation(), nil)
		w.SetDecoratedPotDecorations(px, py, pz, held.NormalizedPotDecorations())
	case block.ResourceLocation() == "minecraft:grindstone":
		block.Properties = javaGrindstonePlacementState(face, p.Rotation.Yaw)
		applyBlockChange(px, py, pz, block, w, mgr)
	case block.ResourceLocation() == "minecraft:loom" ||
		block.ResourceLocation() == "minecraft:stonecutter" ||
		block.ResourceLocation() == "minecraft:cartography_table" ||
		block.ResourceLocation() == "minecraft:smithing_table":
		block.Properties = map[string]string{"facing": chestFacingFromYaw(p.Rotation.Yaw)}
		applyBlockChange(px, py, pz, block, w, mgr)
	default:
		applyBlockChange(px, py, pz, block, w, mgr)
	}
	if blockEntityType, ok := coreworld.PlacementBlockEntityType(block.ResourceLocation()); ok {
		w.SetBlockEntity(px, py, pz, blockEntityType, []byte{10, 0})
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

func javaFacingForFace(face int32) string {
	switch face {
	case 2:
		return "north"
	case 3:
		return "south"
	case 4:
		return "west"
	default:
		return "east"
	}
}

func javaHopperFacing(clickedFace int32) string {
	switch clickedFace {
	case 2:
		return "south"
	case 3:
		return "north"
	case 4:
		return "east"
	case 5:
		return "west"
	default:
		return "down"
	}
}

// shulkerBoxFacing returns the facing property for a shulker box based on the
// clicked face. The shulker box opens toward the face it was placed on.
func shulkerBoxFacing(face int32) string {
	switch face {
	case 0: // bottom face clicked → opens downward
		return "down"
	case 1: // top face clicked → opens upward (default)
		return "up"
	case 2:
		return "south"
	case 3:
		return "north"
	case 4:
		return "east"
	case 5:
		return "west"
	default:
		return "up"
	}
}

// PlacementReplaceable reports whether the named block can be overwritten by a
// placed block (air, flowers, grass, etc.).
func PlacementReplaceable(blockName string) bool { return placementReplaceable(blockName) }

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
		"minecraft:red_mushroom", "minecraft:snow", "minecraft:vine", "minecraft:fire",
		"minecraft:water", "minecraft:lava":
		return true
	default:
		return false
	}
}

// javaSupportsRedstoneComponent reports whether a block has a rigid upper
// face that can support dust, repeaters, and comparators. Using the old
// IsSolidLandingSurface check treated thin components (including another
// redstone wire) as solid, which allowed dust towers to be built in mid-air.
func javaSupportsRedstoneComponent(block coreworld.Block) bool {
	name := block.ResourceLocation()
	if name == "minecraft:hopper" {
		// Vanilla explicitly permits redstone components on hoppers despite the
		// hopper's non-cube collision shape.
		return true
	}
	if placementReplaceable(name) || coreworld.IsFluidBlock(name) || name == "" {
		return false
	}
	if strings.HasSuffix(name, "_slab") {
		return block.Properties["type"] == "top" || block.Properties["type"] == "double"
	}
	if strings.HasSuffix(name, "_stairs") {
		return block.Properties["half"] == "top"
	}
	if javaIsFence(name) || javaIsPane(name) || javaIsWall(name) ||
		strings.HasSuffix(name, "_fence_gate") || strings.HasSuffix(name, "_door") ||
		isTrapdoorBlock(name) || strings.HasSuffix(name, "_button") ||
		strings.HasSuffix(name, "_pressure_plate") || strings.HasSuffix(name, "_carpet") ||
		strings.HasSuffix(name, "_sign") || strings.HasSuffix(name, "_wall_sign") ||
		strings.HasSuffix(name, "_banner") || strings.HasSuffix(name, "_wall_banner") ||
		strings.Contains(name, "torch") || strings.Contains(name, "rail") ||
		strings.Contains(name, "glass") || strings.HasSuffix(name, "_leaves") {
		return false
	}
	switch name {
	case "minecraft:redstone_wire", "minecraft:repeater", "minecraft:comparator",
		"minecraft:lever", "minecraft:ladder", "minecraft:chain",
		"minecraft:lantern", "minecraft:soul_lantern", "minecraft:snow",
		"minecraft:cake", "minecraft:brewing_stand", "minecraft:flower_pot":
		return false
	}
	return true
}

func javaGroundSupportedBlockCanSurvive(block, support coreworld.Block) bool {
	name := block.ResourceLocation()
	switch name {
	case "minecraft:redstone_wire", "minecraft:repeater", "minecraft:comparator":
		return javaSupportsRedstoneComponent(support)
	case "minecraft:nether_wart":
		return support.ResourceLocation() == "minecraft:soul_sand"
	case "minecraft:wheat", "minecraft:carrots", "minecraft:potatoes", "minecraft:beetroots",
		"minecraft:pumpkin_stem", "minecraft:melon_stem", "minecraft:attached_pumpkin_stem",
		"minecraft:attached_melon_stem", "minecraft:torchflower_crop", "minecraft:pitcher_crop":
		return support.ResourceLocation() == "minecraft:farmland"
	default:
		return !support.IsAir() && !coreworld.IsFluidBlock(support.ResourceLocation())
	}
}

// blockSupportsWaterlogging returns true for blocks that have a waterlogged property.
func blockSupportsWaterlogging(blockName string) bool {
	switch blockName {
	case "minecraft:chest", "minecraft:trapped_chest", "minecraft:barrel",
		"minecraft:hopper", "minecraft:dispenser", "minecraft:dropper",
		"minecraft:stairs", "minecraft:slab", "minecraft:fence", "minecraft:fence_gate",
		"minecraft:wall", "minecraft:lantern", "minecraft:campfire",
		"minecraft:sea_pickle", "minecraft:coral", "minecraft:coral_fan",
		"minecraft:coral_block", "minecraft:kelp", "minecraft:seagrass":
		return true
	}
	// Many blocks support waterlogging by suffix
	for _, suffix := range []string{"_stairs", "_slab", "_fence", "_fence_gate", "_wall",
		"_trapdoor", "_door", "_sign", "_button", "_pressure_plate"} {
		if strings.HasSuffix(blockName, suffix) {
			return true
		}
	}
	return false
}

func useToolOrPlant(x, y, z int, face int32, target coreworld.Block, p *player.Player, w *coreworld.World, mgr *session.Manager, nextEntityID func() int32) bool {
	held := p.HeldItem()
	if held.IsEmpty() {
		return false
	}
	if (target.ResourceLocation() == "minecraft:campfire" || target.ResourceLocation() == "minecraft:soul_campfire") &&
		target.Properties["lit"] != "false" {
		if _, ok := FindCookingRecipe("minecraft:campfire", held.ItemID); ok {
			items := w.ContainerItems(x, y, z)
			usedSlots := make(map[int]bool, len(items))
			for _, item := range items {
				usedSlots[item.Slot] = true
			}
			for slot := 0; slot < 4; slot++ {
				if usedSlots[slot] {
					continue
				}
				cooking := held
				cooking.Count = 1
				items = append(items, coreworld.ContainerItemFromStack(slot, cooking))
				w.SetContainerItems(x, y, z, target.ResourceLocation(), items)
				if p.GameMode != player.GameModeCreative {
					inventorySlot := player.HotbarStart + p.HeldSlot
					p.Inventory[inventorySlot].Count--
					normalizeStack(&p.Inventory[inventorySlot])
				}
				broadcastSoundAt(mgr, "minecraft:block.campfire.crackle", soundCategoryBlocks,
					float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
				return true
			}
			return true
		}
	}
	if held.ItemID == "minecraft:bone_meal" {
		if target.ResourceLocation() == "minecraft:grass_block" && w.GetBlock(x, y+1, z).IsAir() {
			applyBlockChange(x, y+1, z, coreworld.Block{Namespace: "minecraft", Name: "short_grass"}, w, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			broadcastSoundAt(mgr, "minecraft:item.bone_meal.use", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
		if changes, used := w.ApplyBoneMeal(x, y, z, rand.Uint64()); used {
			for _, change := range changes {
				if mgr != nil {
					BroadcastBlockChange(change, mgr)
				}
			}
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			broadcastSoundAt(mgr, "minecraft:item.bone_meal.use", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
		if replacement, ok := javaBoneMealGrowth(target); ok {
			applyBlockChange(x, y, z, replacement, w, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			broadcastSoundAt(mgr, "minecraft:item.bone_meal.use", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
	}
	if !p.Sneaking && target.ResourceLocation() == "minecraft:decorated_pot" {
		items := w.ContainerItems(x, y, z)
		var stored player.ItemStack
		if len(items) > 0 {
			stored = items[0].Stack()
		}
		if !stored.IsEmpty() && (!stored.SameItem(held) || stored.Count >= player.MaxStackSize(stored.ItemID)) {
			return true
		}
		if stored.IsEmpty() {
			stored = held
			stored.Count = 1
		} else {
			stored.Count++
		}
		w.SetContainerItems(x, y, z, target.ResourceLocation(), []coreworld.ContainerItem{coreworld.ContainerItemFromStack(0, stored)})
		if p.GameMode != player.GameModeCreative {
			slot := player.HotbarStart + p.HeldSlot
			p.Inventory[slot].Count--
			normalizeStack(&p.Inventory[slot])
		}
		broadcastSoundAt(mgr, "minecraft:block.decorated_pot.insert", soundCategoryBlocks,
			float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
		return true
	}

	if held.ItemID == "minecraft:bucket" {
		var filled string
		switch {
		case (target.ResourceLocation() == "minecraft:water" || target.ResourceLocation() == "minecraft:lava") && coreworld.FluidLevel(target) == 0:
			filled = "minecraft:" + target.Name + "_bucket"
		case target.ResourceLocation() == "minecraft:powder_snow":
			filled = "minecraft:powder_snow_bucket"
		case target.ResourceLocation() == "minecraft:water_cauldron" && target.Properties["level"] == "3":
			filled = "minecraft:water_bucket"
		case target.ResourceLocation() == "minecraft:lava_cauldron":
			filled = "minecraft:lava_bucket"
		case target.ResourceLocation() == "minecraft:powder_snow_cauldron" && target.Properties["level"] == "3":
			filled = "minecraft:powder_snow_bucket"
		}
		if filled != "" {
			if strings.HasSuffix(target.ResourceLocation(), "_cauldron") {
				applyBlockChange(x, y, z, coreworld.Block{Namespace: "minecraft", Name: "cauldron"}, w, mgr)
			} else {
				applyBlockChange(x, y, z, coreworld.Air, w, mgr)
			}
			replaceJavaBucket(p, filled)
			sound := "minecraft:item.bucket.fill"
			if filled == "minecraft:lava_bucket" {
				sound = "minecraft:item.bucket.fill_lava"
			}
			broadcastSoundAt(mgr, sound, soundCategoryPlayers, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
	}

	if held.ItemID == "minecraft:glass_bottle" {
		var waterBottle player.ItemStack
		if err := waterBottle.SetComponent("potion_contents", map[string]string{"potion": "minecraft:water"}); err == nil {
			waterBottle.ItemID = "minecraft:potion"
			waterBottle.Count = 1
		}
		switch target.ResourceLocation() {
		case "minecraft:water":
			if coreworld.FluidLevel(target) == 0 {
				replaceJavaBucket(p, "")
				slot := player.HotbarStart + p.HeldSlot
				if p.GameMode != player.GameModeCreative {
					if p.Inventory[slot].Count <= 1 {
						p.Inventory[slot] = waterBottle
					} else {
						p.Inventory[slot].Count--
						p.GiveItem(waterBottle)
					}
				}
				broadcastSoundAt(mgr, "minecraft:item.bottle.fill", soundCategoryPlayers, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
				return true
			}
		case "minecraft:water_cauldron":
			level := 0
			if l := target.Properties["level"]; l != "" {
				level, _ = strconv.Atoi(l)
			}
			if level > 0 {
				if p.GameMode != player.GameModeCreative {
					slot := player.HotbarStart + p.HeldSlot
					if p.Inventory[slot].Count <= 1 {
						p.Inventory[slot] = waterBottle
					} else {
						p.Inventory[slot].Count--
						p.GiveItem(waterBottle)
					}
					newLevel := level - 1
					if newLevel <= 0 {
						applyBlockChange(x, y, z, coreworld.Block{Namespace: "minecraft", Name: "cauldron"}, w, mgr)
					} else {
						updated := copyBlockProperties(target)
						updated.Properties["level"] = strconv.Itoa(newLevel)
						applyBlockChange(x, y, z, updated, w, mgr)
					}
				}
				broadcastSoundAt(mgr, "minecraft:item.bottle.fill", soundCategoryPlayers, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
				return true
			}
		}
	}

	if entityType := fishBucketEntity(held.ItemID); entityType != "" && nextEntityID != nil {
		if face < 0 || int(face) >= len(faceOffset) {
			return false
		}
		offset := faceOffset[face]
		px, py, pz := x+int(offset[0]), y+int(offset[1]), z+int(offset[2])
		if py >= coreworld.WorldMinY && py <= coreworld.WorldMaxY && placementReplaceable(w.GetBlock(px, py, pz).ResourceLocation()) {
			applyBlockChange(px, py, pz, coreworld.MakeFluid("minecraft:water", 0), w, mgr)
			var uuid [16]byte
			id := nextEntityID()
			binary.BigEndian.PutUint32(uuid[:4], uint32(id))
			fish := corentity.New(id, uuid, entityType, float64(px)+0.5, float64(py)+0.5, float64(pz)+0.5)
			fish.OnGround = true
			w.Entities.Add(fish)
			BroadcastSpawnMobInDimension(fish, mgr, p.Dimension)
			replaceJavaBucket(p, "minecraft:bucket")
			broadcastSoundAt(mgr, "minecraft:item.bucket.empty_fish", soundCategoryPlayers,
				float64(px)+0.5, float64(py)+0.5, float64(pz)+0.5, 1, 1)
		}
		return true
	}

	if held.ItemID == "minecraft:water_bucket" || held.ItemID == "minecraft:lava_bucket" || held.ItemID == "minecraft:powder_snow_bucket" {
		if strings.HasSuffix(target.ResourceLocation(), "_cauldron") {
			return true
		}
		if target.ResourceLocation() == "minecraft:cauldron" {
			filled := coreworld.Block{Namespace: "minecraft", Name: "water_cauldron", Properties: map[string]string{"level": "3"}}
			switch held.ItemID {
			case "minecraft:lava_bucket":
				filled = coreworld.Block{Namespace: "minecraft", Name: "lava_cauldron"}
			case "minecraft:powder_snow_bucket":
				filled = coreworld.Block{Namespace: "minecraft", Name: "powder_snow_cauldron", Properties: map[string]string{"level": "3"}}
			}
			applyBlockChange(x, y, z, filled, w, mgr)
			replaceJavaBucket(p, "minecraft:bucket")
			return true
		}
		if face < 0 || int(face) >= len(faceOffset) {
			return false
		}
		offset := faceOffset[face]
		px, py, pz := x+int(offset[0]), y+int(offset[1]), z+int(offset[2])
		if py < coreworld.WorldMinY || py > coreworld.WorldMaxY || !placementReplaceable(w.GetBlock(px, py, pz).ResourceLocation()) {
			return true
		}
		placed := coreworld.Block{Namespace: "minecraft", Name: "powder_snow"}
		if held.ItemID == "minecraft:water_bucket" {
			placed = coreworld.MakeFluid("minecraft:water", 0)
		} else if held.ItemID == "minecraft:lava_bucket" {
			placed = coreworld.MakeFluid("minecraft:lava", 0)
		}
		applyBlockChange(px, py, pz, placed, w, mgr)
		replaceJavaBucket(p, "minecraft:bucket")
		sound := "minecraft:item.bucket.empty"
		if held.ItemID == "minecraft:lava_bucket" {
			sound = "minecraft:item.bucket.empty_lava"
		}
		broadcastSoundAt(mgr, sound, soundCategoryPlayers, float64(px)+0.5, float64(py)+0.5, float64(pz)+0.5, 1, 1)
		// Fluid interaction: lava meeting water → cobblestone/obsidian.
		if held.ItemID == "minecraft:lava_bucket" || held.ItemID == "minecraft:water_bucket" {
			checkFluidInteraction(px, py, pz, w, mgr)
		}
		return true
	}
	if isHoe(held.ItemID) {
		canMakeFarmland := face != 0 && w.GetBlock(x, y+1, z).IsAir()
		replacement, drop, ok := coreworld.UseHoe(target, canMakeFarmland)
		if !ok {
			return false
		}
		applyBlockChange(x, y, z, replacement, w, mgr)
		if p.GameMode != player.GameModeCreative && !drop.IsEmpty() && !p.GiveItem(drop) {
			spawnBlockDrop(w, nextEntityID, p.Position, drop, 0, mgr, p.Dimension)
		}
		broadcastSoundAt(mgr, "minecraft:item.hoe.till", soundCategoryBlocks, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
		return true
	}

	if isAxe(held.ItemID) {
		replacement, sound, ok := axeTransformation(target)
		if !ok {
			return false
		}
		applyBlockChange(x, y, z, replacement, w, mgr)
		broadcastSoundAt(mgr, sound, soundCategoryBlocks, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
		return true
	}

	if isShovel(held.ItemID) {
		if target.ResourceLocation() == "minecraft:campfire" || target.ResourceLocation() == "minecraft:soul_campfire" {
			if target.Properties["lit"] != "true" {
				return false
			}
			replacement := copyBlockProperties(target)
			replacement.Properties["lit"] = "false"
			applyBlockChange(x, y, z, replacement, w, mgr)
			broadcastSoundAt(mgr, "minecraft:block.fire.extinguish", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
		if !w.GetBlock(x, y+1, z).IsAir() {
			return false
		}
		switch target.ResourceLocation() {
		case "minecraft:grass_block", "minecraft:dirt", "minecraft:coarse_dirt",
			"minecraft:rooted_dirt", "minecraft:podzol", "minecraft:mycelium":
			applyBlockChange(x, y, z, coreworld.Block{Namespace: "minecraft", Name: "dirt_path"}, w, mgr)
			broadcastSoundAt(mgr, "minecraft:item.shovel.flatten", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
		return false
	}

	if isSignBlock(target.ResourceLocation()) {
		if held.ItemID == "minecraft:glow_ink_sac" || held.ItemID == "minecraft:ink_sac" {
			entity := w.GetBlockEntity(x, y, z)
			glowing := held.ItemID == "minecraft:glow_ink_sac"
			// Only change if this actually toggles something.
			if entity.SignFrontGlowing == glowing {
				return true
			}
			state := coreworld.SignState{
				FrontLines: entity.SignFrontLines, BackLines: entity.SignBackLines,
				FrontGlowing: glowing, BackGlowing: entity.SignBackGlowing,
				FrontColor: entity.SignFrontColor, BackColor: entity.SignBackColor,
			}
			data := buildSignNBTFromState(state)
			w.SetBlockEntitySign(x, y, z, data, state)
			BroadcastBlockEntityDataInDimension(w.GetBlockEntity(x, y, z), mgr, p.Dimension)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			sound := "minecraft:item.glow_ink_sac.use"
			if !glowing {
				sound = "minecraft:item.ink_sac.use"
			}
			broadcastSoundAt(mgr, sound, soundCategoryPlayers, float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
		if color := signDyeColor(held.ItemID); color != "" {
			entity := w.GetBlockEntity(x, y, z)
			if entity.SignFrontColor == color {
				return true
			}
			state := coreworld.SignState{
				FrontLines: entity.SignFrontLines, BackLines: entity.SignBackLines,
				FrontGlowing: entity.SignFrontGlowing, BackGlowing: entity.SignBackGlowing,
				FrontColor: color, BackColor: entity.SignBackColor,
			}
			data := buildSignNBTFromState(state)
			w.SetBlockEntitySign(x, y, z, data, state)
			BroadcastBlockEntityDataInDimension(w.GetBlockEntity(x, y, z), mgr, p.Dimension)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			broadcastSoundAt(mgr, "minecraft:item.dye.use", soundCategoryPlayers,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return true
		}
	}

	if held.ItemID == "minecraft:flint_and_steel" || held.ItemID == "minecraft:fire_charge" {
		if target.ResourceLocation() == "minecraft:tnt" &&
			primeJavaTNT(x, y, z, w, mgr, nextEntityID, p.Dimension) {
			broadcastSoundAt(mgr, "minecraft:entity.tnt.primed", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			finishJavaIgniterUse(p, held.ItemID)
			return true
		}
		if target.ResourceLocation() == "minecraft:obsidian" {
			if changes, ok := coreworld.NetherPortalInterior(w, x, y, z); ok {
				for _, change := range changes {
					applyBlockChange(change.X, change.Y, change.Z, change.Block, w, mgr)
				}
				broadcastSoundAt(mgr, "minecraft:item.flintandsteel.use", soundCategoryBlocks,
					float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
				finishJavaIgniterUse(p, held.ItemID)
				return true
			}
		}
		// Light campfire / soul campfire.
		if target.ResourceLocation() == "minecraft:campfire" || target.ResourceLocation() == "minecraft:soul_campfire" {
			if target.Properties["lit"] != "true" {
				lit := copyBlockProperties(target)
				lit.Properties["lit"] = "true"
				applyBlockChange(x, y, z, lit, w, mgr)
				broadcastSoundAt(mgr, "minecraft:item.flintandsteel.use", soundCategoryBlocks,
					float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
				finishJavaIgniterUse(p, held.ItemID)
				return true
			}
		}
		// Light candles / candle cake.
		if isCandleBlock(target.ResourceLocation()) {
			if target.Properties["lit"] != "true" {
				lit := copyBlockProperties(target)
				lit.Properties["lit"] = "true"
				applyBlockChange(x, y, z, lit, w, mgr)
				broadcastSoundAt(mgr, "minecraft:block.candle.ignite", soundCategoryBlocks,
					float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
				finishJavaIgniterUse(p, held.ItemID)
				return true
			}
		}
		if face < 0 || int(face) >= len(faceOffset) {
			return false
		}
		offset := faceOffset[face]
		fx, fy, fz := x+int(offset[0]), y+int(offset[1]), z+int(offset[2])
		if !placementReplaceable(w.GetBlock(fx, fy, fz).ResourceLocation()) {
			return false
		}
		applyBlockChange(fx, fy, fz, coreworld.Block{Namespace: "minecraft", Name: "fire"}, w, mgr)
		broadcastSoundAt(mgr, "minecraft:item.flintandsteel.use", soundCategoryBlocks,
			float64(fx)+0.5, float64(fy)+0.5, float64(fz)+0.5, 1, 1)
		finishJavaIgniterUse(p, held.ItemID)
		return true
	}

	if strings.HasSuffix(held.ItemID, "_spawn_egg") && nextEntityID != nil {
		entityType := corentity.EntityType("minecraft:" + strings.TrimSuffix(strings.TrimPrefix(held.ItemID, "minecraft:"), "_spawn_egg"))
		if corentity.DefaultMaxHealth(entityType) > 0 {
			if face < 0 || int(face) >= len(faceOffset) {
				return false
			}
			offset := faceOffset[face]
			sx, sy, sz := x+int(offset[0]), y+int(offset[1]), z+int(offset[2])
			id := nextEntityID()
			var uuid [16]byte
			binary.BigEndian.PutUint32(uuid[:4], uint32(id))
			e := corentity.New(id, uuid, entityType, float64(sx)+0.5, float64(sy)+0.5, float64(sz)+0.5)
			e.OnGround = true
			w.Entities.Add(e)
			BroadcastSpawnMobInDimension(e, mgr, p.Dimension)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
			}
			return true
		}
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
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:melon_seeds":
		crop = coreworld.Block{Namespace: "minecraft", Name: "melon_stem", Properties: map[string]string{"age": "0"}}
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:pumpkin_seeds":
		crop = coreworld.Block{Namespace: "minecraft", Name: "pumpkin_stem", Properties: map[string]string{"age": "0"}}
	case target.ResourceLocation() == "minecraft:farmland" && held.ItemID == "minecraft:torchflower_seeds":
		crop = coreworld.Block{Namespace: "minecraft", Name: "torchflower_crop", Properties: map[string]string{"age": "0"}}
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

func javaBoneMealGrowth(target coreworld.Block) (coreworld.Block, bool) {
	maxAge := -1
	switch target.ResourceLocation() {
	case "minecraft:cocoa":
		maxAge = 2
	case "minecraft:pitcher_crop":
		maxAge = 4
	}
	if maxAge >= 0 {
		age, _ := strconv.Atoi(target.Properties["age"])
		if age >= maxAge {
			return coreworld.Block{}, false
		}
		// Vanilla: advance by a random 2-5 stages, capped at maxAge.
		// (PumpkinMC crop/mod.rs: bonemeal_age_increase = random_range(2..=5))
		increase := 2 + rand.Intn(4) //nolint:gosec
		newAge := age + increase
		if newAge > maxAge {
			newAge = maxAge
		}
		replacement := copyBlockProperties(target)
		replacement.Properties["age"] = strconv.Itoa(newAge)
		return replacement, true
	}
	if strings.HasSuffix(target.ResourceLocation(), "_sapling") && target.ResourceLocation() != "minecraft:bamboo_sapling" {
		stage, _ := strconv.Atoi(target.Properties["stage"])
		if stage == 0 {
			replacement := copyBlockProperties(target)
			replacement.Properties["stage"] = "1"
			return replacement, true
		}
	}
	return coreworld.Block{}, false
}

func javaButtonPlacementState(block coreworld.Block, face int32, yaw float32, w *coreworld.World, x, y, z int) (coreworld.Block, bool) {
	if face < 0 || int(face) >= len(faceOffset) {
		return coreworld.Block{}, false
	}
	offset := faceOffset[face]
	support := w.GetBlock(x-int(offset[0]), y-int(offset[1]), z-int(offset[2]))
	if !coreworld.IsSolidLandingSurface(support.ResourceLocation()) {
		return coreworld.Block{}, false
	}
	buttonFace := "wall"
	facing := bedFacingFromYaw(yaw)
	switch face {
	case 0:
		buttonFace = "ceiling"
	case 1:
		buttonFace = "floor"
	case 2:
		facing = "north"
	case 3:
		facing = "south"
	case 4:
		facing = "west"
	case 5:
		facing = "east"
	}
	block.Properties = map[string]string{"face": buttonFace, "facing": facing, "powered": "false"}
	return block, true
}

// javaGrindstonePlacementState returns the block properties for a grindstone
// based on the clicked face and the player's yaw. Grindstone supports floor,
// wall, and ceiling mounting (like a button) but has no support requirement.
func javaGrindstonePlacementState(face int32, yaw float32) map[string]string {
	gFace := "wall"
	facing := bedFacingFromYaw(yaw)
	switch face {
	case 0:
		gFace = "ceiling"
	case 1:
		gFace = "floor"
	case 2:
		facing = "north"
	case 3:
		facing = "south"
	case 4:
		facing = "west"
	case 5:
		facing = "east"
	}
	return map[string]string{"face": gFace, "facing": facing}
}

// checkFluidInteraction checks if a newly placed fluid block at (x,y,z) should
// create cobblestone or obsidian by reacting with an adjacent opposite fluid.
// Based on PumpkinMC: lava (still) + water neighbor → obsidian; lava (flowing)
// + water neighbor → cobblestone.
func checkFluidInteraction(x, y, z int, w *coreworld.World, mgr *session.Manager) {
	placed := w.GetBlock(x, y, z)
	placedLoc := placed.ResourceLocation()
	isLava := placedLoc == "minecraft:lava"
	isWater := placedLoc == "minecraft:water"
	if !isLava && !isWater {
		return
	}
	// Neighbor offsets: N, S, E, W, Up, Down
	neighbors := [6][3]int{{0, 0, -1}, {0, 0, 1}, {1, 0, 0}, {-1, 0, 0}, {0, 1, 0}, {0, -1, 0}}
	for i, off := range neighbors {
		nx, ny, nz := x+off[0], y+off[1], z+off[2]
		neighbor := w.GetBlock(nx, ny, nz)
		nLoc := neighbor.ResourceLocation()
		if isLava && nLoc == "minecraft:water" {
			// Lava meets water → cobblestone or obsidian.
			// Still lava (level 0) + water = obsidian; flowing = cobblestone.
			level := coreworld.FluidLevel(placed)
			var result coreworld.Block
			if i == 5 {
				// Lava falling into water is resolved as stone by the fluid tick.
				continue
			}
			if level == 0 {
				result = coreworld.Block{Namespace: "minecraft", Name: "obsidian"}
			} else {
				result = coreworld.Block{Namespace: "minecraft", Name: "cobblestone"}
			}
			applyBlockChange(x, y, z, result, w, mgr)
			broadcastSoundAt(mgr, "minecraft:block.lava.extinguish", soundCategoryBlocks,
				float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
			return
		}
		if isWater && nLoc == "minecraft:lava" {
			if i == 4 {
				// Lava above this water falls into it and forms stone.
				continue
			}
			// Water meets lava → lava becomes cobblestone (or obsidian if still).
			level := coreworld.FluidLevel(neighbor)
			var result coreworld.Block
			if level == 0 {
				result = coreworld.Block{Namespace: "minecraft", Name: "obsidian"}
			} else {
				result = coreworld.Block{Namespace: "minecraft", Name: "cobblestone"}
			}
			applyBlockChange(nx, ny, nz, result, w, mgr)
			broadcastSoundAt(mgr, "minecraft:block.lava.extinguish", soundCategoryBlocks,
				float64(nx)+0.5, float64(ny)+0.5, float64(nz)+0.5, 1, 1)
		}
	}
}

// eatCakeSlice handles right-clicking a placed cake block.
// Based on PumpkinMC cake.rs: each bite restores 2 hunger and 0.4 saturation.
// The "bites" block property goes from 0 to 6; at bites=6 the block is removed.
// Returns true if a slice was consumed (or the attempt was made).
func eatCakeSlice(p *player.Player, x, y, z int, cake coreworld.Block, w *coreworld.World, mgr *session.Manager, conn *network.ClientConn) bool {
	bites := 0
	if s, ok := cake.Properties["bites"]; ok {
		fmt.Sscanf(s, "%d", &bites)
	}
	if bites > 6 {
		return false
	}

	// Restore 2 hunger + 0.4 saturation in survival/adventure.
	if p.GameMode != player.GameModeCreative {
		if !p.ConsumeFoodAllowFull(2, 0.1, false) {
			return false // Too full to eat
		}
		if conn != nil {
			_ = sendUpdateHealth(conn, p)
		}
	}

	bites++
	if bites >= 7 {
		// Last slice eaten — remove the cake.
		applyBlockChange(x, y, z, coreworld.Air, w, mgr)
	} else {
		updated := copyBlockProperties(cake)
		updated.Properties["bites"] = strconv.Itoa(bites)
		applyBlockChange(x, y, z, updated, w, mgr)
	}
	broadcastSoundAt(mgr, "minecraft:entity.generic.eat", soundCategoryPlayers,
		float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 1, 1)
	return true
}

func replaceJavaBucket(p *player.Player, replacement string) {
	if p == nil || p.GameMode == player.GameModeCreative {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	if p.Inventory[slot].Count <= 1 {
		p.Inventory[slot] = player.ItemStack{ItemID: replacement, Count: 1}
		return
	}
	p.Inventory[slot].Count--
	p.GiveItem(player.ItemStack{ItemID: replacement, Count: 1})
}

// fishBucketEntity returns the entity type for a filled fish/aquatic bucket, or "".
func fishBucketEntity(itemID string) corentity.EntityType {
	switch itemID {
	case "minecraft:cod_bucket":
		return corentity.TypeCod
	case "minecraft:salmon_bucket":
		return corentity.TypeSalmon
	case "minecraft:pufferfish_bucket":
		return corentity.TypePufferfish
	case "minecraft:tropical_fish_bucket":
		return corentity.TypeTropicalFish
	case "minecraft:axolotl_bucket":
		return corentity.TypeAxolotl
	case "minecraft:tadpole_bucket":
		return corentity.TypeTadpole
	default:
		return ""
	}
}

func isChestBlock(blockName string) bool {
	return blockName == "minecraft:chest" || blockName == "minecraft:trapped_chest" || blockName == "minecraft:barrel"
}

// candleItemForBlock returns the item ID that matches a candle block.
func candleItemForBlock(blockName string) string {
	// Candle cake variants don't accept additional candles.
	if strings.HasSuffix(blockName, "_candle_cake") {
		return ""
	}
	// "minecraft:white_candle" → "minecraft:white_candle" (block and item share ID).
	if strings.HasSuffix(blockName, "_candle") || blockName == "minecraft:candle" {
		return blockName
	}
	return ""
}

// isCandleBlock returns true for all candle and candle cake block variants.
func isCandleBlock(name string) bool {
	if name == "minecraft:candle" || name == "minecraft:candle_cake" {
		return true
	}
	for _, color := range []string{"white", "orange", "magenta", "light_blue", "yellow", "lime",
		"pink", "gray", "light_gray", "cyan", "purple", "blue", "brown", "green", "red", "black"} {
		if name == "minecraft:"+color+"_candle" || name == "minecraft:"+color+"_candle_cake" {
			return true
		}
	}
	return false
}

// isBedBlock reports whether a resource location is one of the 16 bed colours.
func isBedBlock(name string) bool {
	switch name {
	case "minecraft:white_bed", "minecraft:orange_bed", "minecraft:magenta_bed",
		"minecraft:light_blue_bed", "minecraft:yellow_bed", "minecraft:lime_bed",
		"minecraft:pink_bed", "minecraft:gray_bed", "minecraft:light_gray_bed",
		"minecraft:cyan_bed", "minecraft:purple_bed", "minecraft:blue_bed",
		"minecraft:brown_bed", "minecraft:green_bed", "minecraft:red_bed",
		"minecraft:black_bed":
		return true
	}
	return false
}

func isDoorBlock(name string) bool {
	return strings.HasSuffix(name, "_door") && !isTrapdoorBlock(name)
}

func isTrapdoorBlock(name string) bool {
	return name == "minecraft:trapdoor" || strings.HasSuffix(name, "_trapdoor")
}

func javaTrapdoorPlacementState(block coreworld.Block, face int32, cursorY, yaw float32, w *coreworld.World, x, y, z int) (coreworld.Block, bool) {
	if face < 0 || int(face) >= len(faceOffset) {
		return coreworld.Block{}, false
	}
	offset := faceOffset[face]
	if !coreworld.IsSolidLandingSurface(w.GetBlock(x-int(offset[0]), y-int(offset[1]), z-int(offset[2])).ResourceLocation()) {
		return coreworld.Block{}, false
	}
	facing := bedFacingFromYaw(yaw)
	switch face {
	case 2:
		facing = "north"
	case 3:
		facing = "south"
	case 4:
		facing = "west"
	case 5:
		facing = "east"
	}
	half := "bottom"
	if face == 0 || (face >= 2 && cursorY > 0.5) {
		half = "top"
	}
	block = copyBlockProperties(block)
	block.Properties = map[string]string{
		"facing": facing, "half": half, "open": "false", "powered": "false", "waterlogged": "false",
	}
	return block, true
}

func doorHinge(facing string, clickX, clickZ float32) string {
	switch facing {
	case "north":
		if clickX < 0.5 {
			return "right"
		}
	case "south":
		if clickX >= 0.5 {
			return "right"
		}
	case "east":
		if clickZ < 0.5 {
			return "right"
		}
	case "west":
		if clickZ >= 0.5 {
			return "right"
		}
	}
	return "left"
}

func placeDoorBlock(p *player.Player, x, y, z int, kind string, clickX, clickZ float32, w *coreworld.World, mgr *session.Manager) bool {
	if y >= coreworld.WorldMaxY || !placementReplaceable(w.GetBlock(x, y+1, z).ResourceLocation()) ||
		!coreworld.IsSolidLandingSurface(w.GetBlock(x, y-1, z).ResourceLocation()) {
		return false
	}
	ns, name, _ := strings.Cut(kind, ":")
	facing := bedFacingFromYaw(p.Rotation.Yaw)
	lower := coreworld.Block{Namespace: ns, Name: name, Properties: map[string]string{
		"facing": facing, "half": "lower", "hinge": coreworld.DoorHinge(w, x, y, z, facing, clickX, clickZ),
		"open": "false", "powered": "false",
	}}
	upper := copyBlockProperties(lower)
	upper.Properties["half"] = "upper"
	applyBlockChange(x, y, z, lower, w, mgr)
	applyBlockChange(x, y+1, z, upper, w, mgr)
	return true
}

func breakLinkedDoorHalf(x, y, z int, broken coreworld.Block, w *coreworld.World, mgr *session.Manager) {
	if !isDoorBlock(broken.ResourceLocation()) {
		return
	}
	otherY := y + 1
	if broken.Properties["half"] == "upper" {
		otherY = y - 1
	}
	other := w.GetBlock(x, otherY, z)
	if other.ResourceLocation() == broken.ResourceLocation() {
		applyBlockChange(x, otherY, z, coreworld.Air, w, mgr)
	}
}

// bedFacingFromYaw returns the facing direction a placed bed should use,
// matching chest logic: the bed faces in the direction the player is looking.
func bedFacingFromYaw(yaw float32) string {
	// Normalise to (-180, 180].
	for yaw > 180 {
		yaw -= 360
	}
	for yaw <= -180 {
		yaw += 360
	}
	switch {
	case yaw >= -45 && yaw < 45:
		return "south"
	case yaw >= 45 && yaw < 135:
		return "west"
	case yaw >= -135 && yaw < -45:
		return "east"
	default:
		return "north"
	}
}

// bedHeadOffset returns the (dx, dz) offset from the foot to the head for a
// given facing direction.
func bedHeadOffset(facing string) (int, int) {
	switch facing {
	case "north":
		return 0, -1
	case "south":
		return 0, 1
	case "east":
		return 1, 0
	default: // west
		return -1, 0
	}
}

func bedFacingYaw(facing string) float32 {
	switch facing {
	case "west":
		return 90
	case "north":
		return 180
	case "east":
		return -90
	default: // south
		return 0
	}
}

// prepareJavaBedWake moves a waking player to a safe position beside the bed
// and aligns their view with the bed's actual facing. Keeping the pre-sleep
// position and yaw made the client correct the player in the opposite
// direction as soon as it sent its first post-sleep movement packet.
func prepareJavaBedWake(p *player.Player, w *coreworld.World, mgr *session.Manager) bool {
	if p == nil || w == nil || !p.HasSpawnPoint {
		return false
	}
	bed := w.GetBlock(int(p.SpawnPoint.X), int(p.SpawnPoint.Y), int(p.SpawnPoint.Z))
	if !isBedBlock(bed.ResourceLocation()) {
		return false
	}
	wakePosition, ok := resolveBedRespawn(p, w)
	if !ok {
		return false
	}
	p.Position = wakePosition
	p.Rotation.Yaw = bedFacingYaw(bed.Properties["facing"])
	p.Rotation.Pitch = 0
	p.FallDistance = 0
	p.OnGround = false
	if mgr != nil {
		if current, found := mgr.Get(p.UUID); found && current.TeleportTo != nil {
			_ = current.TeleportTo(wakePosition.X, wakePosition.Y, wakePosition.Z)
		}
		broadcastPosition(mgr, p)
	}
	return true
}

// placeBedBlock places both halves of a bed at (fx, fy, fz) facing the player.
func placeBedBlock(p *player.Player, fx, fy, fz int, kind string, w *coreworld.World, mgr *session.Manager) bool {
	facing := bedFacingFromYaw(p.Rotation.Yaw)
	dx, dz := bedHeadOffset(facing)
	hx, hz := fx+dx, fz+dz

	// Both positions must be free.
	if !placementReplaceable(w.GetBlock(hx, fy, hz).ResourceLocation()) {
		return false
	}
	ns, name, _ := strings.Cut(kind, ":")

	footProps := map[string]string{"facing": facing, "occupied": "false", "part": "foot"}
	headProps := map[string]string{"facing": facing, "occupied": "false", "part": "head"}

	applyBlockChange(fx, fy, fz, coreworld.Block{Namespace: ns, Name: name, Properties: footProps}, w, mgr)
	applyBlockChange(hx, fy, hz, coreworld.Block{Namespace: ns, Name: name, Properties: headProps}, w, mgr)
	data := bedBlockEntityData(kind)
	w.SetBlockEntity(fx, fy, fz, "minecraft:bed", data)
	w.SetBlockEntity(hx, fy, hz, "minecraft:bed", data)
	return true
}

func bedBlockEntityData(kind string) []byte {
	colors := map[string]int32{
		"minecraft:white_bed": 0, "minecraft:orange_bed": 1, "minecraft:magenta_bed": 2,
		"minecraft:light_blue_bed": 3, "minecraft:yellow_bed": 4, "minecraft:lime_bed": 5,
		"minecraft:pink_bed": 6, "minecraft:gray_bed": 7, "minecraft:light_gray_bed": 8,
		"minecraft:cyan_bed": 9, "minecraft:purple_bed": 10, "minecraft:blue_bed": 11,
		"minecraft:brown_bed": 12, "minecraft:green_bed": 13, "minecraft:red_bed": 14,
		"minecraft:black_bed": 15,
	}
	color := colors[kind]
	return []byte{
		0x0a,
		0x03, 0x00, 0x05, 'c', 'o', 'l', 'o', 'r',
		byte(color >> 24), byte(color >> 16), byte(color >> 8), byte(color),
		0x00,
	}
}

// breakLinkedBedHalf removes the other half of a bed when one half is broken.
func breakLinkedBedHalf(x, y, z int, broken coreworld.Block, w *coreworld.World, mgr *session.Manager) {
	if !isBedBlock(broken.ResourceLocation()) {
		return
	}
	part := broken.Properties["part"]
	facing := broken.Properties["facing"]
	if part == "" || facing == "" {
		return
	}
	dx, dz := bedHeadOffset(facing)
	var ox, oz int
	if part == "foot" {
		ox, oz = x+dx, z+dz // foot → head is in facing direction
	} else {
		ox, oz = x-dx, z-dz // head → foot is opposite of facing direction
	}
	other := w.GetBlock(ox, y, oz)
	if other.ResourceLocation() == broken.ResourceLocation() {
		applyBlockChange(ox, y, oz, coreworld.Air, w, mgr)
	}
}

// handleBedInteract is called when a player right-clicks a bed block.
//
// Spawn-point is ALWAYS updated (like vanilla), regardless of time.
// Sleeping / time-skip only triggers at night (tod 12541–23459).
func handleBedInteract(p *player.Player, bx, by, bz int, w *coreworld.World, conn *network.ClientConn, mgr *session.Manager) {
	// ── Always: set personal respawn point at the bed ─────────────────────
	p.SpawnPoint = spatial.BlockPos{X: int32(bx), Y: int32(by), Z: int32(bz)}
	p.HasSpawnPoint = true
	// Send Set Default Spawn Position so the compass points at the bed.
	packed := (int64(bx)&0x3FFFFFF)<<38 | (int64(bz)&0x3FFFFFF)<<12 | (int64(by) & 0xFFF)
	_ = conn.WritePacket(buildPackedSpawnPosition(packed))
	// ── Night only: enter sleep and potentially skip to morning ───────────
	tod := w.WorldTime()
	// Night window: 12541–23459 (matches vanilla mob-spawn / sleep window).
	// Confirm spawn point in chat (always visible, never overwritten).
	_ = conn.WritePacket(buildSystemChatMessage("Respawn point set", false))
	if tod < 12541 || tod > 23459 {
		_ = conn.WritePacket(buildSystemChatMessage("You can only sleep at night.", false))
		return
	}
	if p.Sleeping {
		return // already waiting
	}

	// Mark player as sleeping and broadcast the lying-down animation.
	// The server tick goroutine (tickSleep) will handle the actual time skip
	// after a short delay so the client has time to play the animation.
	p.Sleeping = true
	BroadcastPlayerSleeping(p.EntityID, spatial.BlockPos{
		X: int32(bx),
		Y: int32(by),
		Z: int32(bz),
	}, mgr)

	// Show waiting count in the action bar.
	sessions := mgr.SnapshotAll()
	total, sleeping := 0, 0
	for _, s := range sessions {
		if s.Player != nil {
			total++
			if s.Player.Sleeping {
				sleeping++
			}
		}
	}
	if total > 1 {
		_ = conn.WritePacket(buildSystemChatMessage(
			fmt.Sprintf("Sleeping... (%d/%d players)", sleeping, total), true))
	}
}

// SkipNightAndWake requests a time skip to morning, broadcasts the stand-up
// (wake) animation for every sleeping player, and clears the sleeping flag.
// Called when all online players are sleeping.
func SkipNightAndWake(w *coreworld.World, mgr *session.Manager) {
	w.RequestTimeSkip()
	for _, s := range mgr.SnapshotAll() {
		if s.Player == nil || !s.Player.Sleeping {
			continue
		}
		s.Player.Sleeping = false
		prepareJavaBedWake(s.Player, w, mgr)
		// Broadcast the STANDING pose so all clients see the wake animation.
		BroadcastPlayerWaking(s.Player.EntityID, mgr)
		_ = sendSystemMessage(s.Conn, "Good morning! The night has passed.")
	}
}

// buildPackedSpawnPosition builds a Set Default Spawn Position packet with a
// pre-packed 64-bit block position (X<<38 | Z<<12 | Y) and angle 0.
func buildPackedSpawnPosition(packed int64) *protocol.Packet {
	return protocol.NewBuilder(packetIDSpawnPosition).Long(packed).Float(0).Build()
}

func isHoe(item string) bool {
	return toolCategory(item) == itemregistry.ToolHoe
}

func isAxe(item string) bool {
	return toolCategory(item) == itemregistry.ToolAxe
}

func isShovel(item string) bool {
	return toolCategory(item) == itemregistry.ToolShovel
}

func isBlockUseTool(item string) bool {
	category := toolCategory(item)
	return category == itemregistry.ToolHoe || category == itemregistry.ToolAxe ||
		category == itemregistry.ToolShovel || category == itemregistry.ToolFlintAndSteel

}

func toolCategory(item string) itemregistry.ToolCategory {
	definition, ok := itemregistry.Lookup(item)
	if !ok || definition.Tool == nil {
		return ""
	}
	return definition.Tool.Category
}

func axeTransformation(block coreworld.Block) (coreworld.Block, string, bool) {
	name := block.Name
	if strings.HasPrefix(name, "waxed_") {
		replacement := copyBlockProperties(block)
		replacement.Name = strings.TrimPrefix(name, "waxed_")
		return replacement, "minecraft:item.axe.wax_off", true
	}
	for _, stage := range []struct{ from, to string }{
		{"oxidized_", "weathered_"},
		{"weathered_", "exposed_"},
		{"exposed_", ""},
	} {
		if strings.HasPrefix(name, stage.from) {
			replacement := copyBlockProperties(block)
			replacement.Name = stage.to + strings.TrimPrefix(name, stage.from)
			return replacement, "minecraft:item.axe.scrape", true
		}
	}
	strippable := strings.HasSuffix(name, "_log") || strings.HasSuffix(name, "_wood") ||
		strings.HasSuffix(name, "_stem") || strings.HasSuffix(name, "_hyphae") ||
		name == "bamboo_block"
	if strippable && !strings.HasPrefix(name, "stripped_") {
		replacement := copyBlockProperties(block)
		replacement.Name = "stripped_" + name
		return replacement, "minecraft:item.axe.strip", true
	}
	return coreworld.Block{}, "", false
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

// toggleTrapdoor handles all hand-openable trapdoors. Iron trapdoors are
// intentionally redstone-only, matching Vanilla.
func toggleTrapdoor(x, y, z int, trapdoor coreworld.Block, w *coreworld.World, mgr *session.Manager) bool {
	if trapdoor.Namespace != "minecraft" || trapdoor.Name == "iron_trapdoor" ||
		!isTrapdoorBlock(trapdoor.ResourceLocation()) {
		return false
	}
	if _, ok := trapdoor.Properties["open"]; !ok {
		return false
	}
	toggled := copyBlockProperties(trapdoor)
	toggled.Properties["open"] = strconv.FormatBool(trapdoor.Properties["open"] != "true")
	applyBlockChange(x, y, z, toggled, w, mgr)
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
	if name := block.ResourceLocation(); name != "minecraft:sculk_sensor" && name != "minecraft:calibrated_sculk_sensor" {
		w.EmitVibration(x, y, z)
	}
	stateID := javaworld.StateID(block)
	pkt := buildBlockUpdate(x, y, z, stateID)
	stemChanges := w.UpdateAttachedStemsAround(x, y, z)
	bubbleChanges := w.UpdateBubbleColumnsAround(x, y, z)
	if mgr != nil {
		for _, s := range mgr.SnapshotAll() {
			_ = s.Conn.WritePacket(pkt)
		}
		for _, change := range stemChanges {
			BroadcastBlockChange(change, mgr)
		}
		for _, change := range bubbleChanges {
			BroadcastBlockChange(change, mgr)
		}
	}
	refreshJavaConnectedBlocks(x, y, z, w, mgr)
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

// ── Boat item placement ───────────────────────────────────────────────────────

// boatItemToType maps boat item resource locations to entity types.
var boatItemToType = map[string]corentity.EntityType{
	"minecraft:oak_boat":            corentity.TypeOakBoat,
	"minecraft:spruce_boat":         corentity.TypeSpruceBoat,
	"minecraft:birch_boat":          corentity.TypeBirchBoat,
	"minecraft:jungle_boat":         corentity.TypeJungleBoat,
	"minecraft:acacia_boat":         corentity.TypeAcaciaBoat,
	"minecraft:dark_oak_boat":       corentity.TypeDarkOakBoat,
	"minecraft:mangrove_boat":       corentity.TypeMangroveBoat,
	"minecraft:cherry_boat":         corentity.TypeCherryBoat,
	"minecraft:bamboo_raft":         corentity.TypeBambooRaft,
	"minecraft:oak_chest_boat":      corentity.TypeOakChestBoat,
	"minecraft:spruce_chest_boat":   corentity.TypeSpruceChestBoat,
	"minecraft:birch_chest_boat":    corentity.TypeBirchChestBoat,
	"minecraft:jungle_chest_boat":   corentity.TypeJungleChestBoat,
	"minecraft:acacia_chest_boat":   corentity.TypeAcaciaChestBoat,
	"minecraft:dark_oak_chest_boat": corentity.TypeDarkOakChestBoat,
	"minecraft:mangrove_chest_boat": corentity.TypeMangroveChestBoat,
	"minecraft:cherry_chest_boat":   corentity.TypeCherryChestBoat,
	"minecraft:bamboo_chest_raft":   corentity.TypeBambooChestRaft,
}

// isBoatItem reports whether an item resource location is a placeable boat item.
func isBoatItem(itemID string) bool {
	_, ok := boatItemToType[itemID]
	return ok
}

// spawnBoatFromItem creates a boat entity at the target surface position.
// The boat is placed at the clicked block's top face (or water surface).
// Returns nil if the item doesn't map to a boat type.
func spawnBoatFromItem(itemID string, bx, by, bz, face int, w *coreworld.World, nextEntityID func() int32) *corentity.Entity {
	eType, ok := boatItemToType[itemID]
	if !ok {
		return nil
	}

	// Spawn position: top of clicked block, or on water surface.
	spawnX := float64(bx) + 0.5
	spawnZ := float64(bz) + 0.5
	spawnY := float64(by + 1) // top face of clicked block by default

	// If clicking on water, float on the water surface.
	clickedBlock := w.GetBlock(bx, by, bz)
	if coreworld.IsFluidBlock(clickedBlock.ResourceLocation()) {
		spawnY = float64(by) + 1
	} else if face == 1 { // top face
		spawnY = float64(by + 1)
	}

	id := nextEntityID()
	var uuid [16]byte
	if _, err := cryptorand.Read(uuid[:]); err != nil {
		for index := range uuid {
			uuid[index] = byte(uint32(id) >> (uint(index%4) * 8))
		}
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	boat := corentity.New(id, uuid, eType, spawnX, spawnY, spawnZ)
	w.Entities.Add(boat)
	return boat
}

// spawnBoatFromLook handles BoatItem's vanilla Use Item behaviour. Boat items
// raycast fluids and blocks themselves, so clients do not normally send Use
// Item On when placing one on water.
func spawnBoatFromLook(p *player.Player, w *coreworld.World, nextEntityID func() int32) *corentity.Entity {
	if p == nil || w == nil || nextEntityID == nil || !isBoatItem(p.HeldItem().ItemID) {
		return nil
	}
	yaw := float64(p.Rotation.Yaw) * math.Pi / 180
	pitch := float64(p.Rotation.Pitch) * math.Pi / 180
	directionX := -math.Sin(yaw) * math.Cos(pitch)
	directionY := -math.Sin(pitch)
	directionZ := math.Cos(yaw) * math.Cos(pitch)
	eyeX, eyeY, eyeZ := p.Position.X, p.Position.Y+1.62, p.Position.Z
	lastBlock := [3]int{math.MinInt, math.MinInt, math.MinInt}
	for distance := 0.2; distance <= 5; distance += 0.1 {
		bx := int(math.Floor(eyeX + directionX*distance))
		by := int(math.Floor(eyeY + directionY*distance))
		bz := int(math.Floor(eyeZ + directionZ*distance))
		if lastBlock == [3]int{bx, by, bz} {
			continue
		}
		lastBlock = [3]int{bx, by, bz}
		block := w.GetBlock(bx, by, bz)
		name := block.ResourceLocation()
		if block.IsAir() || (placementReplaceable(name) && !coreworld.IsFluidBlock(name)) {
			continue
		}
		boat := spawnBoatFromItem(p.HeldItem().ItemID, bx, by, bz, 1, w, nextEntityID)
		if boat != nil {
			boat.Yaw = p.Rotation.Yaw
		}
		return boat
	}
	return nil
}

func consumePlacedBoat(p *player.Player, conn *network.ClientConn) {
	if p == nil || p.GameMode != player.GameModeSurvival {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	p.Inventory[slot].Count--
	normalizeStack(&p.Inventory[slot])
	if conn != nil {
		_ = SyncPlayerInventory(conn, p)
	}
}

// redstoneWireConnections returns the block properties for a redstone wire
// placed at (x, y, z), computing side connections based on neighbors.
// A side connects ("side" value) when an adjacent block is redstone wire or a
// powered component that emits redstone. For simplicity we connect to adjacent
// redstone_wire blocks at the same level, one above, or one below.
// PumpkinMC reference: redstone_wire.rs connection logic.
func redstoneWireConnections(x, y, z int, w *coreworld.World) map[string]string {
	props := map[string]string{"north": "none", "east": "none", "south": "none", "west": "none", "power": "0"}
	type offset struct {
		dx, dz int
		dir    string
	}
	neighbors := []offset{{0, -1, "north"}, {1, 0, "east"}, {0, 1, "south"}, {-1, 0, "west"}}
	for _, n := range neighbors {
		nx, nz := x+n.dx, z+n.dz
		same := w.GetBlock(nx, y, nz)
		if javaRedstoneConnectsFrom(n.dir, same) {
			props[n.dir] = "side"
			continue
		}
		// A wire may climb a rigid neighboring block only when the space above
		// this wire is clear. The previous entity-collision approximation also
		// classified dust itself as a solid step.
		if javaSupportsRedstoneComponent(same) && placementReplaceable(w.GetBlock(x, y+1, z).ResourceLocation()) {
			up := w.GetBlock(nx, y+1, nz)
			if up.ResourceLocation() == "minecraft:redstone_wire" {
				props[n.dir] = "up"
				continue
			}
		}
		// Check one block down (wire on the ground on the other side of a step).
		below := w.GetBlock(nx, y-1, nz)
		if below.ResourceLocation() == "minecraft:redstone_wire" && !javaSupportsRedstoneComponent(same) {
			props[n.dir] = "side"
		}
	}
	return props
}

// dragonEggTeleport moves the dragon egg to a random replaceable position
// within 15 blocks (x,z ±7, y ±1), removes it from (x,y,z), and places it at
// the new location. Returns true when a valid destination was found.
func dragonEggTeleport(x, y, z int, w *coreworld.World, mgr *session.Manager) bool {
	const tries = 1000
	for range tries {
		nx := x + (rand.Intn(15) - 7) //nolint:gosec
		ny := y + (rand.Intn(3) - 1)  //nolint:gosec
		nz := z + (rand.Intn(15) - 7) //nolint:gosec
		if ny < coreworld.WorldMinY || ny > coreworld.WorldMaxY {
			continue
		}
		if !placementReplaceable(w.GetBlock(nx, ny, nz).ResourceLocation()) {
			continue
		}
		if !w.GetBlock(nx, ny-1, nz).IsAir() { // needs solid or replaceable base
			applyBlockChange(x, y, z, coreworld.Air, w, mgr)
			applyBlockChange(nx, ny, nz, coreworld.Block{Namespace: "minecraft", Name: "dragon_egg"}, w, mgr)
			return true
		}
	}
	return false
}

func javaRedstoneConnectsFrom(direction string, block coreworld.Block) bool {
	name := block.ResourceLocation()
	if name == "minecraft:repeater" || name == "minecraft:comparator" {
		facing := block.Properties["facing"]
		if direction == "north" || direction == "south" {
			return facing == "north" || facing == "south"
		}
		return facing == "east" || facing == "west"
	}
	return name == "minecraft:redstone_wire" || name == "minecraft:lever" ||
		name == "minecraft:redstone_torch" || name == "minecraft:redstone_wall_torch" ||
		name == "minecraft:redstone_block" || name == "minecraft:target" ||
		strings.HasSuffix(name, "_button") || strings.HasSuffix(name, "_pressure_plate") ||
		name == "minecraft:observer" || coreworld.IsRedstoneLoad(name)
}
