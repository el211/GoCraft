package server

import (
	"strconv"
	"strings"

	"GoCraft/core/blockloot"
	corentity "GoCraft/core/entity"
	"GoCraft/core/intent"
	"GoCraft/core/itemregistry"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"
)

// applyBedrockItemAction applies Pumpkin/vanilla-style item behaviour before
// normal block activation and placement. Returning true means the click was
// consumed, including clicks that intentionally make no generic placement.
func (s *Server) applyBedrockItemAction(p *player.Player, i intent.BlockInteractIntent, target coreworld.Block) bool {
	if p == nil || s.bedrockWorld() == nil {
		return false
	}
	held := p.HeldItem()
	if held.IsEmpty() {
		if _, potted := coreworld.PottedItem(target); !potted {
			return false
		}
	}
	if held.ItemID == "minecraft:firework_rocket" && p.GameMode != player.GameModeSpectator {
		return s.applyFireworkUse(intent.FireworkUseIntent{
			PlayerUUID: p.UUID,
			HotbarSlot: int32(p.HeldSlot),
			Position: spatial.Vec3{
				X: float64(i.Position.X) + float64(i.ClickX),
				Y: float64(i.Position.Y) + float64(i.ClickY),
				Z: float64(i.Position.Z) + float64(i.ClickZ),
			},
		}) != nil
	}
	x, y, z := int(i.Position.X), int(i.Position.Y), int(i.Position.Z)
	name, item := target.ResourceLocation(), held.ItemID
	if (name == "minecraft:campfire" || name == "minecraft:soul_campfire") && target.Properties["lit"] != "false" {
		if _, ok := handler.FindCookingRecipe("minecraft:campfire", item); ok {
			items := s.bedrockWorld().ContainerItems(x, y, z)
			usedSlots := make(map[int]bool, len(items))
			for _, stored := range items {
				usedSlots[stored.Slot] = true
			}
			for slot := 0; slot < 4; slot++ {
				if usedSlots[slot] {
					continue
				}
				cooking := held
				cooking.Count = 1
				items = append(items, coreworld.ContainerItemFromStack(slot, cooking))
				s.bedrockWorld().SetContainerItems(x, y, z, name, items)
				s.consumeBedrockHeldItem(p, 1)
				return true
			}
			return true
		}
	}

	if item == "minecraft:ender_eye" && name == "minecraft:end_portal_frame" && target.Properties["eye"] != "true" {
		replacement := bedrockCopyBlock(target)
		replacement.Properties["eye"] = "true"
		s.setBedrockActionBlock(x, y, z, replacement)
		s.consumeBedrockHeldItem(p, 1)
		s.tryActivateEndPortal(x, y, z)
		return true
	}

	if bedrockIsHoe(item) {
		canMakeFarmland := i.Face != 0 && s.bedrockWorld().GetBlock(x, y+1, z).IsAir()
		if replacement, drop, ok := coreworld.UseHoe(target, canMakeFarmland); ok {
			s.setBedrockActionBlock(x, y, z, replacement)
			if p.GameMode != player.GameModeCreative && !drop.IsEmpty() {
				s.giveBedrockActionItem(p, drop)
			}
			s.damageBedrockHeldItem(p, 1)
			return true
		}
	}

	if bedrockIsAxe(item) {
		if replacement, ok := bedrockAxeTransformation(target); ok {
			s.setBedrockActionBlock(x, y, z, replacement)
			s.damageBedrockHeldItem(p, 1)
			return true
		}
	}

	if bedrockIsShovel(item) {
		if name == "minecraft:campfire" || name == "minecraft:soul_campfire" {
			if target.Properties["lit"] == "true" {
				replacement := bedrockCopyBlock(target)
				replacement.Properties["lit"] = "false"
				s.setBedrockActionBlock(x, y, z, replacement)
				s.damageBedrockHeldItem(p, 1)
				return true
			}
		}
		if i.Face != 0 && s.bedrockWorld().GetBlock(x, y+1, z).IsAir() {
			switch name {
			case "minecraft:grass_block", "minecraft:dirt", "minecraft:coarse_dirt",
				"minecraft:rooted_dirt", "minecraft:podzol", "minecraft:mycelium":
				s.setBedrockActionBlock(x, y, z, bedrockBlock("dirt_path", nil))
				s.damageBedrockHeldItem(p, 1)
				return true
			}
		}
	}

	if item == "minecraft:honeycomb" {
		if waxed, ok := coreworld.WaxCopper(target); ok {
			s.setBedrockActionBlock(x, y, z, waxed)
			s.consumeBedrockHeldItem(p, 1)
			return true
		}
	}

	if item == "minecraft:shears" && name == "minecraft:pumpkin" {
		carved, _ := coreworld.CarvePumpkin(target, bedrockOppositeFacing(bedrockPlayerFacing(p.Rotation.Yaw)))
		s.setBedrockActionBlock(x, y, z, carved)
		s.giveBedrockActionItem(p, player.ItemStack{ItemID: "minecraft:pumpkin_seeds", Count: 4})
		s.damageBedrockHeldItem(p, 1)
		return true
	}
	if replacement, output, harvested := coreworld.HarvestBeehive(target, item); harvested {
		s.setBedrockActionBlock(x, y, z, replacement)
		switch item {
		case "minecraft:shears":
			s.giveBedrockActionItem(p, output)
			s.damageBedrockHeldItem(p, 1)
		case "minecraft:glass_bottle":
			s.replaceBedrockHeldItem(p, output.ItemID)
		}
		return true
	}

	if candleCake, ok := coreworld.AddCandleToCake(target, item); ok {
		s.setBedrockActionBlock(x, y, z, candleCake)
		s.consumeBedrockHeldItem(p, 1)
		return true
	}

	if name == "minecraft:flower_pot" {
		if potted, ok := coreworld.PottedBlock(item); ok {
			s.setBedrockActionBlock(x, y, z, potted)
			s.consumeBedrockHeldItem(p, 1)
			return true
		}
	} else if pottedItem, ok := coreworld.PottedItem(target); ok {
		if _, canPot := coreworld.PottedBlock(item); canPot {
			return true
		}
		s.setBedrockActionBlock(x, y, z, bedrockBlock("flower_pot", nil))
		s.giveBedrockActionItem(p, player.ItemStack{ItemID: pottedItem, Count: 1})
		return true
	}

	if replacement, consumed, schedule := coreworld.AddToComposter(target, item, x, y, z, s.worldAge); consumed {
		s.consumeBedrockHeldItem(p, 1)
		s.setBedrockActionBlock(x, y, z, replacement)
		if schedule {
			s.bedrockWorld().BlockPhysics.ScheduleComposter(x, y, z, s.worldAge, 20)
		}
		return true
	}

	if charged, ok := coreworld.ChargeRespawnAnchor(target, item); ok {
		s.setBedrockActionBlock(x, y, z, charged)
		s.consumeBedrockHeldItem(p, 1)
		return true
	}

	if item == "minecraft:bone_meal" {
		if name == "minecraft:grass_block" && s.bedrockWorld().GetBlock(x, y+1, z).IsAir() {
			s.setBedrockActionBlock(x, y+1, z, bedrockBlock("short_grass", nil))
			s.consumeBedrockHeldItem(p, 1)
			return true
		}
		seed := uint64(s.worldAge) + uint64(coreworld.CropAge(target)+1)*0x9e3779b97f4a7c15
		if changes, used := s.bedrockWorld().ApplyBoneMeal(x, y, z, seed); used {
			s.broadcastCanonicalCropChanges(changes)
			s.consumeBedrockHeldItem(p, 1)
			return true
		}
	}

	if item == "minecraft:bucket" {
		var filled string
		switch {
		case (name == "minecraft:water" || name == "minecraft:lava") && coreworld.FluidLevel(target) == 0:
			filled = "minecraft:" + target.Name + "_bucket"
			s.setBedrockActionBlock(x, y, z, coreworld.Air)
		case name == "minecraft:powder_snow":
			filled = "minecraft:powder_snow_bucket"
			s.setBedrockActionBlock(x, y, z, coreworld.Air)
		case name == "minecraft:water_cauldron" && target.Properties["level"] == "3":
			filled = "minecraft:water_bucket"
			s.setBedrockActionBlock(x, y, z, bedrockBlock("cauldron", nil))
		case name == "minecraft:lava_cauldron":
			filled = "minecraft:lava_bucket"
			s.setBedrockActionBlock(x, y, z, bedrockBlock("cauldron", nil))
		case name == "minecraft:powder_snow_cauldron" && target.Properties["level"] == "3":
			filled = "minecraft:powder_snow_bucket"
			s.setBedrockActionBlock(x, y, z, bedrockBlock("cauldron", nil))
		}
		if filled != "" {
			s.replaceBedrockHeldItem(p, filled)
			return true
		}
	}
	if entityType := bedrockFishBucketEntity(item); entityType != "" && s.game != nil {
		dx, dy, dz := bedrockFaceOffset(i.Face)
		px, py, pz := x+dx, y+dy, z+dz
		w := s.bedrockWorld()
		if py >= coreworld.WorldMinY && py <= coreworld.WorldMaxY && bedrockPlacementReplaceable(w.GetBlock(px, py, pz).ResourceLocation()) {
			s.setBedrockActionBlock(px, py, pz, coreworld.MakeFluid("minecraft:water", 0))
			fish := corentity.New(s.game.NextEntityID(), newRandomUUID(), entityType,
				float64(px)+0.5, float64(py)+0.5, float64(pz)+0.5)
			fish.OnGround = true
			w.Entities.Add(fish)
			handler.BroadcastSpawnMobInDimension(fish, s.sessions, p.Dimension)
			s.replaceBedrockHeldItem(p, "minecraft:bucket")
		}
		return true
	}

	if item == "minecraft:water_bucket" || item == "minecraft:lava_bucket" || item == "minecraft:powder_snow_bucket" {
		if name == "minecraft:cauldron" {
			var filled coreworld.Block
			switch item {
			case "minecraft:water_bucket":
				filled = bedrockBlock("water_cauldron", map[string]string{"level": "3"})
			case "minecraft:lava_bucket":
				filled = bedrockBlock("lava_cauldron", nil)
			default:
				filled = bedrockBlock("powder_snow_cauldron", map[string]string{"level": "3"})
			}
			s.setBedrockActionBlock(x, y, z, filled)
			s.replaceBedrockHeldItem(p, "minecraft:bucket")
			return true
		}
		if strings.HasSuffix(name, "_cauldron") {
			return true
		}
		dx, dy, dz := bedrockFaceOffset(i.Face)
		px, py, pz := x+dx, y+dy, z+dz
		if py < coreworld.WorldMinY || py > coreworld.WorldMaxY || !bedrockPlacementReplaceable(s.bedrockWorld().GetBlock(px, py, pz).ResourceLocation()) {
			return true
		}
		var placed coreworld.Block
		switch item {
		case "minecraft:water_bucket":
			placed = coreworld.MakeFluid("minecraft:water", 0)
		case "minecraft:lava_bucket":
			placed = coreworld.MakeFluid("minecraft:lava", 0)
		default:
			placed = bedrockBlock("powder_snow", nil)
		}
		s.setBedrockActionBlock(px, py, pz, placed)
		s.replaceBedrockHeldItem(p, "minecraft:bucket")
		return true
	}

	if item == "minecraft:flint_and_steel" || item == "minecraft:fire_charge" {
		if name == "minecraft:tnt" {
			var changes []coreworld.BlockChange
			s.activateTNT(x, y, z, &changes)
			for _, change := range changes {
				if s.sessions != nil {
					handler.BroadcastBlockChange(change, s.sessions)
				}
			}
			s.finishBedrockIgniterUse(p, item)
			return true
		}
		if name == "minecraft:campfire" || name == "minecraft:soul_campfire" || strings.HasSuffix(name, "_candle") ||
			strings.HasSuffix(name, "_candle_cake") || name == "minecraft:candle" || name == "minecraft:candle_cake" {
			if target.Properties["lit"] != "true" {
				replacement := bedrockCopyBlock(target)
				replacement.Properties["lit"] = "true"
				s.setBedrockActionBlock(x, y, z, replacement)
				s.finishBedrockIgniterUse(p, item)
				return true
			}
		}
		if name == "minecraft:obsidian" && s.igniteNetherPortal(x, y, z) {
			s.finishBedrockIgniterUse(p, item)
			return true
		}
		dx, dy, dz := bedrockFaceOffset(i.Face)
		fx, fy, fz := x+dx, y+dy, z+dz
		if fy >= coreworld.WorldMinY && fy <= coreworld.WorldMaxY &&
			bedrockPlacementReplaceable(s.bedrockWorld().GetBlock(fx, fy, fz).ResourceLocation()) {
			s.setBedrockActionBlock(fx, fy, fz, bedrockBlock("fire", nil))
			s.finishBedrockIgniterUse(p, item)
			return true
		}
	}

	if crop, ok := bedrockCropForItem(item, name); ok && i.Face == 1 && s.bedrockWorld().GetBlock(x, y+1, z).IsAir() {
		s.setBedrockActionBlock(x, y+1, z, crop)
		s.consumeBedrockHeldItem(p, 1)
		return true
	}

	if name == "minecraft:jukebox" && coreworld.IsMusicDisc(item) {
		be := s.bedrockWorld().GetBlockEntity(x, y, z)
		stored := coreworld.JukeboxRecordItem(be)
		if stored == "" {
			updated, ok := coreworld.InsertJukeboxRecord(target, item)
			if ok {
				s.setBedrockActionBlock(x, y, z, updated)
				s.bedrockWorld().SetContainerItems(x, y, z, "minecraft:jukebox",
					[]coreworld.ContainerItem{coreworld.ContainerItemFromStack(0, player.ItemStack{ItemID: item, Count: 1})})
				s.consumeBedrockHeldItem(p, 1)
				return true
			}
		}
	}
	if name == "minecraft:lectern" && coreworld.IsLecternBook(item) {
		be := s.bedrockWorld().GetBlockEntity(x, y, z)
		if coreworld.LecternBook(be) == "" {
			updated, ok := coreworld.InsertLecternBook(target, item)
			if ok {
				s.setBedrockActionBlock(x, y, z, updated)
				s.bedrockWorld().SetContainerItems(x, y, z, "minecraft:lectern",
					[]coreworld.ContainerItem{coreworld.ContainerItemFromStack(0, player.ItemStack{ItemID: item, Count: 1})})
				s.consumeBedrockHeldItem(p, 1)
				return true
			}
		}
	}
	return false
}

// applyBedrockBlockActivation handles stateful blocks before held-block
// placement, mirroring Pumpkin's normal_use behaviour for common mechanisms.
func (s *Server) applyBedrockBlockActivation(p *player.Player, pos spatial.BlockPos, block coreworld.Block) bool {
	x, y, z := int(pos.X), int(pos.Y), int(pos.Z)
	name := block.ResourceLocation()
	if name == "minecraft:jukebox" {
		be := s.bedrockWorld().GetBlockEntity(x, y, z)
		stored := coreworld.JukeboxRecordItem(be)
		if stored != "" {
			_, updated, ok := coreworld.EjectJukeboxRecord(block, stored)
			if ok {
				s.setBedrockActionBlock(x, y, z, updated)
				s.bedrockWorld().SetContainerItems(x, y, z, "minecraft:jukebox", nil)
				s.giveBedrockActionItem(p, player.ItemStack{ItemID: stored, Count: 1})
			}
		}
		return true
	}
	if name == "minecraft:lectern" {
		held := p.HeldItem()
		be := s.bedrockWorld().GetBlockEntity(x, y, z)
		stored := coreworld.LecternBook(be)
		if stored != "" && !coreworld.IsLecternBook(held.ItemID) {
			// Eject when not holding a book.
			if _, cleared, ok := coreworld.EjectLecternBook(block, stored); ok {
				s.setBedrockActionBlock(x, y, z, cleared)
				s.bedrockWorld().SetContainerItems(x, y, z, "minecraft:lectern", nil)
				s.giveBedrockActionItem(p, player.ItemStack{ItemID: stored, Count: 1})
			}
		}
		return true
	}
	if name == "minecraft:sweet_berry_bush" {
		seed := uint64(s.worldAge) + uint64(coreworld.CropAge(block)+1)*0x9e3779b97f4a7c15
		if count, changes, harvested := s.bedrockWorld().HarvestSweetBerryBush(x, y, z, seed); harvested {
			s.broadcastCanonicalCropChanges(changes)
			s.giveBedrockActionItem(p, player.ItemStack{ItemID: "minecraft:sweet_berries", Count: count})
			return true
		}
	}
	if name == "minecraft:decorated_pot" {
		held := p.HeldItem()
		if held.IsEmpty() {
			return true
		}
		items := s.bedrockWorld().ContainerItems(x, y, z)
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
		s.bedrockWorld().SetContainerItems(x, y, z, name, []coreworld.ContainerItem{coreworld.ContainerItemFromStack(0, stored)})
		s.consumeBedrockHeldItem(p, 1)
		return true
	}

	if name == "minecraft:lever" {
		replacement := bedrockCopyBlock(block)
		replacement.Properties["powered"] = bedrockToggleBool(block.Properties["powered"])
		s.setBedrockActionBlock(x, y, z, replacement)
		return true
	}
	if strings.HasSuffix(name, "_button") {
		if block.Properties["powered"] != "true" {
			replacement := bedrockCopyBlock(block)
			replacement.Properties["powered"] = "true"
			s.setBedrockActionBlock(x, y, z, replacement)
			delay := int64(30)
			if name == "minecraft:stone_button" || name == "minecraft:polished_blackstone_button" {
				delay = 20
			}
			s.bedrockWorld().BlockPhysics.ScheduleButton(x, y, z, s.worldAge, delay)
		}
		return true
	}
	if bedrockIsTrapdoor(name) && name != "minecraft:iron_trapdoor" {
		replacement := bedrockCopyBlock(block)
		replacement.Properties["open"] = bedrockToggleBool(block.Properties["open"])
		s.setBedrockActionBlock(x, y, z, replacement)
		return true
	}
	if strings.HasSuffix(name, "_fence_gate") {
		replacement := bedrockCopyBlock(block)
		opening := block.Properties["open"] != "true"
		replacement.Properties["open"] = strconv.FormatBool(opening)
		if opening && replacement.Properties["facing"] == bedrockOppositeFacing(bedrockPlayerFacing(p.Rotation.Yaw)) {
			replacement.Properties["facing"] = bedrockPlayerFacing(p.Rotation.Yaw)
		}
		s.setBedrockActionBlock(x, y, z, replacement)
		return true
	}
	if bedrockIsDoor(name) && name != "minecraft:iron_door" {
		nextOpen := bedrockToggleBool(block.Properties["open"])
		replacement := bedrockCopyBlock(block)
		replacement.Properties["open"] = nextOpen
		s.setBedrockActionBlock(x, y, z, replacement)
		otherY := y + 1
		if block.Properties["half"] == "upper" {
			otherY = y - 1
		}
		other := s.bedrockWorld().GetBlock(x, otherY, z)
		if other.ResourceLocation() == name {
			other = bedrockCopyBlock(other)
			other.Properties["open"] = nextOpen
			s.setBedrockActionBlock(x, otherY, z, other)
		}
		return true
	}
	if name == "minecraft:repeater" {
		replacement := bedrockCopyBlock(block)
		delay, _ := strconv.Atoi(block.Properties["delay"])
		replacement.Properties["delay"] = strconv.Itoa(delay%4 + 1)
		s.setBedrockActionBlock(x, y, z, replacement)
		return true
	}
	if name == "minecraft:comparator" {
		replacement := bedrockCopyBlock(block)
		if block.Properties["mode"] == "subtract" {
			replacement.Properties["mode"] = "compare"
		} else {
			replacement.Properties["mode"] = "subtract"
		}
		s.setBedrockActionBlock(x, y, z, replacement)
		return true
	}
	if name == "minecraft:daylight_detector" || name == "minecraft:inverted_daylight_detector" {
		replacement := bedrockCopyBlock(block)
		if name == "minecraft:daylight_detector" {
			replacement.Name = "inverted_daylight_detector"
		} else {
			replacement.Name = "daylight_detector"
		}
		s.setBedrockActionBlock(x, y, z, replacement)
		return true
	}
	if name == "minecraft:note_block" {
		if replacement, ok := coreworld.TuneNoteBlock(block); ok {
			s.setBedrockActionBlock(x, y, z, replacement)
			return true
		}
	}
	if name == "minecraft:cake" && p.ConsumeFood(2, 0.1) {
		bites, _ := strconv.Atoi(block.Properties["bites"])
		if bites >= 6 {
			s.setBedrockActionBlock(x, y, z, coreworld.Air)
		} else {
			replacement := bedrockCopyBlock(block)
			replacement.Properties["bites"] = strconv.Itoa(bites + 1)
			s.setBedrockActionBlock(x, y, z, replacement)
		}
		return true
	}
	if name == "minecraft:candle" || strings.HasSuffix(name, "_candle") || strings.HasSuffix(name, "_candle_cake") {
		if block.Properties["lit"] == "true" {
			replacement := bedrockCopyBlock(block)
			replacement.Properties["lit"] = "false"
			s.setBedrockActionBlock(x, y, z, replacement)
		}
		return true
	}
	if replacement, ready := coreworld.EmptyComposter(block); ready {
		s.setBedrockActionBlock(x, y, z, replacement)
		s.giveBedrockActionItem(p, player.ItemStack{ItemID: "minecraft:bone_meal", Count: 1})
		return true
	}
	return false
}

func (s *Server) placeBedrockHeldBlock(p *player.Player, i intent.BlockInteractIntent, clicked coreworld.Block) bool {
	if p == nil || p.GameMode == player.GameModeAdventure || p.GameMode == player.GameModeSpectator {
		return false
	}
	held := p.HeldItem()
	if _, minecart := canonicalMinecartType(held.ItemID); minecart {
		return s.placeBedrockMinecart(p, i.Position)
	}
	block, ok := placementBlockForItem(held.ItemID)
	if !ok {
		return false
	}
	if block.ResourceLocation() == "minecraft:light" {
		block.Properties = map[string]string{"level": strconv.Itoa(bedrockHeldLightLevel(held))}
	}
	x, y, z := int(i.Position.X), int(i.Position.Y), int(i.Position.Z)

	// Clicking a matching single slab combines it in-place before adjacent
	// placement is considered.
	if strings.HasSuffix(block.Name, "_slab") && clicked.ResourceLocation() == block.ResourceLocation() && clicked.Properties["type"] != "double" {
		replacement := bedrockCopyBlock(clicked)
		replacement.Properties["type"] = "double"
		s.setBedrockActionBlock(x, y, z, replacement)
		s.consumeBedrockHeldItem(p, 1)
		return true
	}
	if (block.Name == "candle" || strings.HasSuffix(block.Name, "_candle")) && clicked.ResourceLocation() == block.ResourceLocation() {
		candles, _ := strconv.Atoi(clicked.Properties["candles"])
		if candles < 4 {
			replacement := bedrockCopyBlock(clicked)
			replacement.Properties["candles"] = strconv.Itoa(candles + 1)
			s.setBedrockActionBlock(x, y, z, replacement)
			s.consumeBedrockHeldItem(p, 1)
		}
		return true
	}
	if block.ResourceLocation() == "minecraft:snow" && clicked.ResourceLocation() == "minecraft:snow" {
		layers, _ := strconv.Atoi(clicked.Properties["layers"])
		if layers < 8 {
			replacement := bedrockCopyBlock(clicked)
			replacement.Properties["layers"] = strconv.Itoa(layers + 1)
			s.setBedrockActionBlock(x, y, z, replacement)
			s.consumeBedrockHeldItem(p, 1)
		}
		return true
	}

	px, py, pz := x, y, z
	if !bedrockPlacementReplaceable(clicked.ResourceLocation()) {
		dx, dy, dz := bedrockFaceOffset(i.Face)
		px, py, pz = x+dx, y+dy, z+dz
	}
	if py < coreworld.WorldMinY || py > coreworld.WorldMaxY ||
		!bedrockPlacementReplaceable(s.bedrockWorld().GetBlock(px, py, pz).ResourceLocation()) {
		return true
	}

	name := block.ResourceLocation()
	if bedrockIsBed(name) {
		facing := bedrockPlayerFacing(p.Rotation.Yaw)
		dx, dz := bedrockHorizontalOffset(facing)
		hx, hz := px+dx, pz+dz
		if !bedrockPlacementReplaceable(s.bedrockWorld().GetBlock(hx, py, hz).ResourceLocation()) {
			return true
		}
		foot := bedrockBlock(block.Name, map[string]string{"facing": facing, "occupied": "false", "part": "foot"})
		head := bedrockBlock(block.Name, map[string]string{"facing": facing, "occupied": "false", "part": "head"})
		s.setBedrockActionBlock(px, py, pz, foot)
		s.setBedrockActionBlock(hx, py, hz, head)
		data := bedrockBedBlockEntityData(name)
		s.bedrockWorld().SetBlockEntity(px, py, pz, "minecraft:bed", data)
		s.bedrockWorld().SetBlockEntity(hx, py, hz, "minecraft:bed", data)
		s.consumeBedrockHeldItem(p, 1)
		return true
	}
	if bedrockIsDoor(name) {
		if py >= coreworld.WorldMaxY || !bedrockPlacementReplaceable(s.bedrockWorld().GetBlock(px, py+1, pz).ResourceLocation()) ||
			!bedrockSolidSupport(s.bedrockWorld().GetBlock(px, py-1, pz)) {
			return true
		}
		facing := bedrockPlayerFacing(p.Rotation.Yaw)
		hinge := coreworld.DoorHinge(s.bedrockWorld(), px, py, pz, facing, i.ClickX, i.ClickZ)
		props := map[string]string{"facing": facing, "half": "lower", "hinge": hinge, "open": "false", "powered": "false"}
		lower := bedrockBlock(block.Name, props)
		upper := bedrockCopyBlock(lower)
		upper.Properties["half"] = "upper"
		s.setBedrockActionBlock(px, py, pz, lower)
		s.setBedrockActionBlock(px, py+1, pz, upper)
		s.consumeBedrockHeldItem(p, 1)
		return true
	}

	placed, valid := s.bedrockPlacementState(p, block, px, py, pz, i)
	if !valid {
		return true
	}
	s.setBedrockActionBlock(px, py, pz, placed)
	if placed.ResourceLocation() == "minecraft:redstone_wire" {
		s.refreshBedrockWireConnections(px, py, pz)
	}
	if strings.HasSuffix(placed.ResourceLocation(), "_pressure_plate") {
		s.bedrockWorld().BlockPhysics.SchedulePressurePlate(px, py, pz, s.worldAge, 1)
	}
	if isBedrockGenericContainer(name) || name == "minecraft:decorated_pot" {
		s.bedrockWorld().SetContainerItems(px, py, pz, name, nil)
	}
	if name == "minecraft:decorated_pot" {
		s.bedrockWorld().SetDecoratedPotDecorations(px, py, pz, held.NormalizedPotDecorations())
	}
	if blockEntityType, ok := coreworld.PlacementBlockEntityType(placed.ResourceLocation()); ok {
		s.bedrockWorld().SetBlockEntity(px, py, pz, blockEntityType, []byte{10, 0})
	}
	s.consumeBedrockHeldItem(p, 1)
	return true
}

func (s *Server) bedrockPlacementState(p *player.Player, block coreworld.Block, x, y, z int, i intent.BlockInteractIntent) (coreworld.Block, bool) {
	name := block.ResourceLocation()
	props := map[string]string{}
	playerFacing := bedrockPlayerFacing(p.Rotation.Yaw)
	frontFacing := bedrockOppositeFacing(playerFacing)

	if coreworld.IsAttachmentPlacementItem(name) {
		placingInWater := s.bedrockWorld().GetBlock(x, y, z).ResourceLocation() == "minecraft:water"
		placed, _, valid := coreworld.AttachmentPlacementState(s.bedrockWorld(), block, x, y, z, i.Face, bedrockSignRotation(p.Rotation.Yaw), placingInWater)
		return placed, valid
	}

	if name == "minecraft:torch" || name == "minecraft:soul_torch" || name == "minecraft:redstone_torch" {
		if i.Face == 1 && bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y-1, z)) {
			block.Properties = map[string]string{}
			if name == "minecraft:redstone_torch" {
				block.Properties["lit"] = "true"
			}
			return block, true
		}
		if i.Face >= 2 && i.Face <= 5 {
			supportX, supportY, supportZ := x, y, z
			dx, dy, dz := bedrockFaceOffset(i.Face)
			supportX, supportY, supportZ = x-dx, y-dy, z-dz
			if bedrockSolidSupport(s.bedrockWorld().GetBlock(supportX, supportY, supportZ)) {
				block.Name = strings.Replace(block.Name, "_torch", "_wall_torch", 1)
				if block.Name == "torch" {
					block.Name = "wall_torch"
				}
				block.Properties = map[string]string{"facing": bedrockFacingForFace(i.Face)}
				if name == "minecraft:redstone_torch" {
					block.Properties["lit"] = "true"
				}
				return block, true
			}
		}
		return coreworld.Block{}, false
	}

	switch {
	case strings.HasSuffix(name, "_log"), strings.HasSuffix(name, "_wood"), strings.HasSuffix(name, "_stem"), strings.HasSuffix(name, "_hyphae"), name == "minecraft:bamboo_block":
		props["axis"] = bedrockAxisForFace(i.Face)
	case strings.HasSuffix(name, "_stairs"):
		props = map[string]string{"facing": playerFacing, "half": bedrockPlacementHalf(i), "shape": "straight", "waterlogged": "false"}
	case strings.HasSuffix(name, "_slab"):
		props = map[string]string{"type": bedrockPlacementHalf(i), "waterlogged": "false"}
	case bedrockIsTrapdoor(name):
		if !bedrockPlacementHasFaceSupport(s.bedrockWorld(), x, y, z, i.Face) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"facing": bedrockFacingForFaceOrPlayer(i.Face, playerFacing), "half": bedrockPlacementHalf(i), "open": "false", "powered": "false", "waterlogged": "false"}
	case strings.HasSuffix(name, "_fence_gate"):
		props = map[string]string{"facing": playerFacing, "in_wall": "false", "open": "false", "powered": "false"}
	case name == "minecraft:lever" || strings.HasSuffix(name, "_button"):
		if !bedrockPlacementHasFaceSupport(s.bedrockWorld(), x, y, z, i.Face) {
			return coreworld.Block{}, false
		}
		face := "wall"
		facing := bedrockFacingForFaceOrPlayer(i.Face, playerFacing)
		if i.Face == 1 {
			face, facing = "floor", playerFacing
		} else if i.Face == 0 {
			face, facing = "ceiling", playerFacing
		}
		props = map[string]string{"face": face, "facing": facing, "powered": "false"}
	case name == "minecraft:repeater":
		if !bedrockSupportsRedstoneComponent(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"delay": "1", "facing": frontFacing, "locked": "false", "powered": "false"}
	case name == "minecraft:comparator":
		if !bedrockSupportsRedstoneComponent(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"facing": frontFacing, "mode": "compare", "powered": "false"}
	case name == "minecraft:redstone_wire":
		if !bedrockSupportsRedstoneComponent(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = s.bedrockRedstoneWireProperties(x, y, z)
	case strings.HasSuffix(name, "_pressure_plate"):
		if !bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		if name == "minecraft:light_weighted_pressure_plate" || name == "minecraft:heavy_weighted_pressure_plate" {
			props = map[string]string{"power": "0"}
		} else {
			props = map[string]string{"powered": "false"}
		}
	case name == "minecraft:lantern" || name == "minecraft:soul_lantern":
		hanging := i.Face == 0
		if hanging {
			if !bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y+1, z)) {
				return coreworld.Block{}, false
			}
		} else if !bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"hanging": strconv.FormatBool(hanging), "waterlogged": "false"}
	case name == "minecraft:ladder":
		if i.Face < 2 || i.Face > 5 || !bedrockPlacementHasFaceSupport(s.bedrockWorld(), x, y, z, i.Face) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"facing": bedrockFacingForFace(i.Face), "waterlogged": "false"}
	case name == "minecraft:end_rod" || name == "minecraft:lightning_rod":
		props = map[string]string{"facing": bedrockFacingForFace(i.Face), "waterlogged": "false"}
		if name == "minecraft:lightning_rod" {
			props["powered"] = "false"
		}
	case name == "minecraft:chain":
		props = map[string]string{"axis": bedrockAxisForFace(i.Face), "waterlogged": "false"}
	case strings.HasSuffix(name, "_glazed_terracotta"):
		props = map[string]string{"facing": frontFacing}
	case name == "minecraft:rail":
		props = map[string]string{"shape": "north_south", "waterlogged": "false"}
	case name == "minecraft:powered_rail" || name == "minecraft:detector_rail" || name == "minecraft:activator_rail":
		props = map[string]string{"shape": "north_south", "powered": "false", "waterlogged": "false"}
	case name == "minecraft:campfire" || name == "minecraft:soul_campfire":
		if !bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"facing": playerFacing, "lit": "true", "signal_fire": "false", "waterlogged": "false"}
	case name == "minecraft:candle" || strings.HasSuffix(name, "_candle"):
		if !bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"candles": "1", "lit": "false", "waterlogged": "false"}
	case name == "minecraft:snow":
		props = map[string]string{"layers": "1"}
	case name == "minecraft:light":
		props = bedrockCopyProperties(block.Properties)
	case name == "minecraft:decorated_pot":
		props = map[string]string{"facing": frontFacing, "cracked": "false", "waterlogged": "false"}
	case name == "minecraft:redstone_lamp":
		props = map[string]string{"lit": "false"}
	case name == "minecraft:daylight_detector":
		props = map[string]string{"power": "0"}
	case name == "minecraft:composter":
		props = map[string]string{"level": "0"}
	case name == "minecraft:respawn_anchor":
		props = map[string]string{"charges": "0"}
	case name == "minecraft:target":
		props = map[string]string{"power": "0"}
	case name == "minecraft:end_portal_frame":
		if !bedrockSolidSupport(s.bedrockWorld().GetBlock(x, y-1, z)) {
			return coreworld.Block{}, false
		}
		props = map[string]string{"facing": frontFacing, "eye": "false"}
	case name == "minecraft:note_block":
		props = map[string]string{"instrument": "harp", "note": "0", "powered": "false"}
	case name == "minecraft:chest" || name == "minecraft:trapped_chest":
		props = map[string]string{"facing": frontFacing, "type": "single", "waterlogged": "false"}
	case name == "minecraft:barrel":
		props = map[string]string{"facing": bedrockFacingForFace(i.Face), "open": "false"}
	case name == "minecraft:hopper":
		facing := "down"
		if i.Face >= 2 && i.Face <= 5 {
			facing = bedrockOppositeFacing(bedrockFacingForFace(i.Face))
		}
		props = map[string]string{"facing": facing, "enabled": "true"}
	case strings.HasSuffix(name, "_furnace") || name == "minecraft:furnace" || name == "minecraft:smoker" ||
		name == "minecraft:dispenser" || name == "minecraft:dropper" || name == "minecraft:observer" ||
		name == "minecraft:piston" || name == "minecraft:sticky_piston":
		props = map[string]string{"facing": frontFacing}
		if name == "minecraft:dispenser" || name == "minecraft:dropper" {
			props["triggered"] = "false"
		}
		if name == "minecraft:furnace" || strings.HasSuffix(name, "_furnace") || name == "minecraft:smoker" {
			props["lit"] = "false"
		}
		if name == "minecraft:observer" {
			props["powered"] = "false"
		}
		if name == "minecraft:piston" || name == "minecraft:sticky_piston" {
			props["extended"] = "false"
		}
	case strings.HasSuffix(name, "_sign"):
		if i.Face >= 2 && i.Face <= 5 {
			block.Name = strings.TrimSuffix(block.Name, "_sign") + "_wall_sign"
			props = map[string]string{"facing": bedrockFacingForFace(i.Face), "waterlogged": "false"}
		} else {
			props = map[string]string{"rotation": strconv.Itoa(bedrockSignRotation(p.Rotation.Yaw)), "waterlogged": "false"}
		}
	}
	if len(props) != 0 {
		block.Properties = props
	}
	return block, true
}

func (s *Server) setBedrockActionBlock(x, y, z int, block coreworld.Block) {
	s.bedrockWorld().SetBlock(x, y, z, block)
	if name := block.ResourceLocation(); name != "minecraft:sculk_sensor" && name != "minecraft:calibrated_sculk_sensor" {
		s.bedrockWorld().EmitVibration(x, y, z)
	}
	if s.sessions != nil {
		handler.BroadcastBlockChange(coreworld.BlockChange{X: x, Y: y, Z: z, Block: block}, s.sessions)
	}
	s.broadcastCanonicalCropChanges(s.bedrockWorld().UpdateAttachedStemsAround(x, y, z))
	s.broadcastCanonicalCropChanges(s.bedrockWorld().UpdateBubbleColumnsAround(x, y, z))
	s.refreshBedrockConnectedBlocks(x, y, z)
}

func (s *Server) broadcastCanonicalCropChanges(changes []coreworld.BlockChange) {
	if s.sessions == nil {
		return
	}
	for _, change := range changes {
		handler.BroadcastBlockChange(change, s.sessions)
	}
}

func bedrockBedBlockEntityData(kind string) []byte {
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

// refreshBedrockConnectedBlocks mirrors the neighbour-state pass used by
// Pumpkin/vanilla after a block mutation. Walls carry explicit connection
// states in the Bedrock palette, fence gates carry in_wall, and fences need a
// neighbour UpdateBlock to make the client rebuild their dynamic model.
func (s *Server) refreshBedrockConnectedBlocks(x, y, z int) {
	if s.bedrockWorld() == nil {
		return
	}
	positions := [][3]int{
		{x, y, z},
		{x - 1, y, z}, {x + 1, y, z}, {x, y, z - 1}, {x, y, z + 1},
		{x, y - 1, z},
	}
	for _, pos := range positions {
		px, py, pz := pos[0], pos[1], pos[2]
		if py < coreworld.WorldMinY || py > coreworld.WorldMaxY {
			continue
		}
		block := s.bedrockWorld().GetBlock(px, py, pz)
		name := block.ResourceLocation()
		switch {
		case bedrockIsWall(name):
			updated := s.bedrockWallState(px, py, pz, block)
			if updated.Key() != block.Key() {
				s.bedrockWorld().SetBlock(px, py, pz, updated)
				if s.sessions != nil {
					handler.BroadcastBlockChange(coreworld.BlockChange{X: px, Y: py, Z: pz, Block: updated}, s.sessions)
				}
			}
		case strings.HasSuffix(name, "_fence_gate"):
			updated := bedrockCopyBlock(block)
			updated.Properties["in_wall"] = strconv.FormatBool(s.bedrockGateInWall(px, py, pz, block))
			if updated.Key() != block.Key() {
				s.bedrockWorld().SetBlock(px, py, pz, updated)
				if s.sessions != nil {
					handler.BroadcastBlockChange(coreworld.BlockChange{X: px, Y: py, Z: pz, Block: updated}, s.sessions)
				}
			}
		case bedrockIsFence(name):
			if s.bedrockListener != nil {
				s.bedrockListener.BroadcastBlockChange(coreworld.BlockChange{X: px, Y: py, Z: pz, Block: block})
			}
		}
	}
}

func (s *Server) bedrockWallState(x, y, z int, block coreworld.Block) coreworld.Block {
	updated := bedrockCopyBlock(block)
	connections := 0
	above := s.bedrockWorld().GetBlock(x, y+1, z)
	tall := bedrockWallTallAbove(above)
	for _, direction := range []struct {
		name   string
		dx, dz int
	}{
		{name: "north", dz: -1},
		{name: "east", dx: 1},
		{name: "south", dz: 1},
		{name: "west", dx: -1},
	} {
		connection := "none"
		if bedrockWallConnectsTo(s.bedrockWorld().GetBlock(x+direction.dx, y, z+direction.dz), direction.name) {
			connection = "low"
			if tall {
				connection = "tall"
			}
			connections++
		}
		updated.Properties[direction.name] = connection
	}
	north := updated.Properties["north"] != "none"
	east := updated.Properties["east"] != "none"
	south := updated.Properties["south"] != "none"
	west := updated.Properties["west"] != "none"
	post := connections < 2
	if connections >= 2 {
		switch {
		case north && south:
			post = east || west
		case east && west:
			post = north || south
		default:
			post = true
		}
	}
	updated.Properties["up"] = strconv.FormatBool(post)
	if _, ok := updated.Properties["waterlogged"]; !ok {
		updated.Properties["waterlogged"] = "false"
	}
	return updated
}

func bedrockWallConnectsTo(block coreworld.Block, direction string) bool {
	name := block.ResourceLocation()
	if bedrockIsWall(name) || strings.HasSuffix(name, "_glass_pane") || name == "minecraft:glass_pane" || name == "minecraft:iron_bars" {
		return true
	}
	if strings.HasSuffix(name, "_fence_gate") {
		facing := block.Properties["facing"]
		if direction == "north" || direction == "south" {
			return facing == "east" || facing == "west"
		}
		return facing == "north" || facing == "south"
	}
	return bedrockWallFullFace(block)
}

func bedrockWallFullFace(block coreworld.Block) bool {
	name := block.ResourceLocation()
	if bedrockPlacementReplaceable(name) || coreworld.IsFluidBlock(name) || bedrockIsFence(name) ||
		strings.HasSuffix(name, "_fence_gate") || strings.HasSuffix(name, "_door") ||
		bedrockIsTrapdoor(name) || strings.HasSuffix(name, "_button") ||
		strings.HasSuffix(name, "_pressure_plate") || strings.Contains(name, "torch") ||
		strings.HasSuffix(name, "_sign") || strings.HasSuffix(name, "_wall_sign") ||
		name == "minecraft:lever" || name == "minecraft:ladder" || name == "minecraft:chain" || name == "minecraft:lantern" {
		return false
	}
	return name != ""
}

func bedrockWallTallAbove(block coreworld.Block) bool {
	return bedrockWallFullFace(block) || bedrockIsWall(block.ResourceLocation())
}

func (s *Server) bedrockGateInWall(x, y, z int, gate coreworld.Block) bool {
	facing := gate.Properties["facing"]
	if facing == "north" || facing == "south" {
		return bedrockIsWall(s.bedrockWorld().GetBlock(x-1, y, z).ResourceLocation()) ||
			bedrockIsWall(s.bedrockWorld().GetBlock(x+1, y, z).ResourceLocation())
	}
	return bedrockIsWall(s.bedrockWorld().GetBlock(x, y, z-1).ResourceLocation()) ||
		bedrockIsWall(s.bedrockWorld().GetBlock(x, y, z+1).ResourceLocation())
}

func bedrockIsWall(name string) bool {
	return strings.HasSuffix(name, "_wall")
}

func bedrockIsFence(name string) bool {
	return strings.HasSuffix(name, "_fence") && !strings.HasSuffix(name, "_fence_gate")
}

func (s *Server) breakBedrockLinkedBlock(x, y, z int, block coreworld.Block) {
	name := block.ResourceLocation()
	px, py, pz := x, y, z
	switch {
	case bedrockIsDoor(name):
		if block.Properties["half"] == "upper" {
			py--
		} else {
			py++
		}
	case bedrockIsBed(name):
		dx, dz := bedrockHorizontalOffset(block.Properties["facing"])
		if block.Properties["part"] == "head" {
			px, pz = x-dx, z-dz
		} else {
			px, pz = x+dx, z+dz
		}
	default:
		return
	}
	if partner := s.bedrockWorld().GetBlock(px, py, pz); partner.ResourceLocation() == name {
		s.setBedrockActionBlock(px, py, pz, coreworld.Air)
	}
}

// breakBedrockUnsupportedAbove applies vanilla's neighbour update immediately
// after a player removes a supporting block. Doing this in the interaction
// path also guarantees that the Bedrock UpdateBlock packets are ordered with
// the original break instead of leaving vegetation floating client-side.
func (s *Server) breakBedrockUnsupportedAbove(p *player.Player, x, y, z int) {
	world := s.bedrockWorld()
	if world == nil {
		return
	}
	for updateIndex, update := range world.ApplyAttachmentSupportUpdatesAround(x, y, z) {
		if s.sessions != nil {
			handler.BroadcastBlockChange(update.Change, s.sessions)
		}
		if !update.Removed || p == nil {
			continue
		}
		dropPosition := spatial.Vec3{
			X: float64(update.Change.X) + 0.5,
			Y: float64(update.Change.Y) + 0.5,
			Z: float64(update.Change.Z) + 0.5,
		}
		for dropIndex, drop := range blockloot.Drops(blockloot.Context{Block: update.Previous}) {
			if dropped := s.newDroppedItemForPlayer(p, drop, dropPosition, updateIndex*16+dropIndex); dropped != nil && s.sessions != nil {
				handler.BroadcastSpawnMobInDimension(dropped, s.sessions, p.Dimension)
			}
		}
	}
	s.broadcastCanonicalCropChanges(world.BreakUnsupportedCropsAbove(x, y, z))
	for plantY := y + 1; plantY <= coreworld.WorldMaxY; plantY++ {
		plant := world.GetBlock(x, plantY, z)
		if !coreworld.RequiresGroundSupport(plant) || !world.GetBlock(x, plantY-1, z).IsAir() {
			return
		}
		partnerY, partnerHalf, hasPartner := coreworld.DoublePlantPartnerY(plant, plantY)
		s.setBedrockActionBlock(x, plantY, z, coreworld.Air)
		if hasPartner {
			partner := world.GetBlock(x, partnerY, z)
			if partner.ResourceLocation() == plant.ResourceLocation() && partner.Properties["half"] == partnerHalf {
				s.setBedrockActionBlock(x, partnerY, z, coreworld.Air)
			}
		}
	}
}

func (s *Server) consumeBedrockHeldItem(p *player.Player, count int) {
	if p == nil || p.GameMode == player.GameModeCreative || count <= 0 {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	if slot < player.HotbarStart || slot >= player.InventorySize {
		return
	}
	p.Inventory[slot].Count -= count
	if p.Inventory[slot].Count <= 0 {
		p.Inventory[slot] = player.ItemStack{}
	}
}

func (s *Server) replaceBedrockHeldItem(p *player.Player, replacement string) {
	if p == nil || p.GameMode == player.GameModeCreative {
		return
	}
	slot := player.HotbarStart + p.HeldSlot
	stack := &p.Inventory[slot]
	if stack.Count <= 1 {
		*stack = player.ItemStack{ItemID: replacement, Count: 1}
		return
	}
	stack.Count--
	s.giveBedrockActionItem(p, player.ItemStack{ItemID: replacement, Count: 1})
}

func (s *Server) giveBedrockActionItem(p *player.Player, stack player.ItemStack) {
	if !p.GiveItem(stack) {
		s.newDroppedItemForPlayer(p, stack, p.Position, 0)
	}
}

func (s *Server) finishBedrockIgniterUse(p *player.Player, item string) {
	if item == "minecraft:fire_charge" {
		s.consumeBedrockHeldItem(p, 1)
	} else {
		s.damageBedrockHeldItem(p, 1)
	}
}

func bedrockCropForItem(item, target string) (coreworld.Block, bool) {
	if target == "minecraft:farmland" {
		switch item {
		case "minecraft:wheat_seeds":
			return bedrockBlock("wheat", map[string]string{"age": "0"}), true
		case "minecraft:carrot":
			return bedrockBlock("carrots", map[string]string{"age": "0"}), true
		case "minecraft:potato":
			return bedrockBlock("potatoes", map[string]string{"age": "0"}), true
		case "minecraft:beetroot_seeds":
			return bedrockBlock("beetroots", map[string]string{"age": "0"}), true
		case "minecraft:melon_seeds":
			return bedrockBlock("melon_stem", map[string]string{"age": "0"}), true
		case "minecraft:pumpkin_seeds":
			return bedrockBlock("pumpkin_stem", map[string]string{"age": "0"}), true
		case "minecraft:torchflower_seeds":
			return bedrockBlock("torchflower_crop", map[string]string{"age": "0"}), true
		}
	}
	if target == "minecraft:soul_sand" && item == "minecraft:nether_wart" {
		return bedrockBlock("nether_wart", map[string]string{"age": "0"}), true
	}
	return coreworld.Block{}, false
}

func bedrockCropMaxAge(name string) (int, bool) {
	return coreworld.CropMaxAge(name)
}

func bedrockAxeTransformation(block coreworld.Block) (coreworld.Block, bool) {
	replacement := bedrockCopyBlock(block)
	name := block.Name
	if strings.HasPrefix(name, "waxed_") {
		replacement.Name = strings.TrimPrefix(name, "waxed_")
		return replacement, true
	}
	for _, stage := range []struct{ from, to string }{{"oxidized_", "weathered_"}, {"weathered_", "exposed_"}, {"exposed_", ""}} {
		if strings.HasPrefix(name, stage.from) {
			replacement.Name = stage.to + strings.TrimPrefix(name, stage.from)
			return replacement, true
		}
	}
	if (strings.HasSuffix(name, "_log") || strings.HasSuffix(name, "_wood") ||
		strings.HasSuffix(name, "_stem") || strings.HasSuffix(name, "_hyphae") || name == "bamboo_block") &&
		!strings.HasPrefix(name, "stripped_") {
		replacement.Name = "stripped_" + name
		return replacement, true
	}
	return coreworld.Block{}, false
}

func bedrockIsHoe(item string) bool {
	return bedrockToolCategory(item) == itemregistry.ToolHoe
}

func bedrockIsAxe(item string) bool {
	return bedrockToolCategory(item) == itemregistry.ToolAxe
}

func bedrockIsShovel(item string) bool {
	return bedrockToolCategory(item) == itemregistry.ToolShovel
}

func bedrockToolCategory(item string) itemregistry.ToolCategory {
	definition, ok := itemregistry.Lookup(item)
	if !ok || definition.Tool == nil {
		return ""
	}
	return definition.Tool.Category
}

func bedrockBlock(name string, properties map[string]string) coreworld.Block {
	return coreworld.Block{Namespace: "minecraft", Name: name, Properties: properties}
}

func bedrockCopyBlock(block coreworld.Block) coreworld.Block {
	block.Properties = bedrockCopyProperties(block.Properties)
	return block
}

func bedrockCopyProperties(properties map[string]string) map[string]string {
	copy := make(map[string]string, len(properties)+1)
	for key, value := range properties {
		copy[key] = value
	}
	return copy
}

func bedrockToggleBool(value string) string {
	if value == "true" {
		return "false"
	}
	return "true"
}

func bedrockPlacementReplaceable(name string) bool {
	switch name {
	case "", "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:short_grass", "minecraft:grass", "minecraft:fern", "minecraft:tall_grass",
		"minecraft:large_fern", "minecraft:dead_bush", "minecraft:snow", "minecraft:vine",
		"minecraft:fire", "minecraft:water", "minecraft:lava", "minecraft:dandelion",
		"minecraft:poppy", "minecraft:blue_orchid", "minecraft:allium", "minecraft:azure_bluet",
		"minecraft:red_tulip", "minecraft:orange_tulip", "minecraft:white_tulip", "minecraft:pink_tulip",
		"minecraft:oxeye_daisy", "minecraft:cornflower", "minecraft:lily_of_the_valley":
		return true
	}
	return false
}

func bedrockSolidSupport(block coreworld.Block) bool {
	name := block.ResourceLocation()
	return !bedrockPlacementReplaceable(name) && !coreworld.IsFluidBlock(name)
}

func bedrockSupportsRedstoneComponent(block coreworld.Block) bool {
	name := block.ResourceLocation()
	if name == "minecraft:hopper" {
		return true
	}
	if bedrockPlacementReplaceable(name) || coreworld.IsFluidBlock(name) || name == "" {
		return false
	}
	if strings.HasSuffix(name, "_slab") {
		return block.Properties["type"] == "top" || block.Properties["type"] == "double"
	}
	if strings.HasSuffix(name, "_stairs") {
		return block.Properties["half"] == "top"
	}
	if bedrockIsFence(name) || bedrockIsWall(name) || strings.HasSuffix(name, "_fence_gate") ||
		strings.HasSuffix(name, "_door") || bedrockIsTrapdoor(name) || strings.HasSuffix(name, "_button") ||
		strings.HasSuffix(name, "_pressure_plate") || strings.HasSuffix(name, "_carpet") ||
		strings.Contains(name, "torch") || strings.Contains(name, "rail") || strings.Contains(name, "glass") ||
		strings.HasSuffix(name, "_leaves") {
		return false
	}
	switch name {
	case "minecraft:redstone_wire", "minecraft:repeater", "minecraft:comparator", "minecraft:lever",
		"minecraft:ladder", "minecraft:chain", "minecraft:lantern", "minecraft:soul_lantern",
		"minecraft:snow", "minecraft:cake", "minecraft:brewing_stand", "minecraft:flower_pot":
		return false
	default:
		return true
	}
}

func bedrockPlacementHasFaceSupport(world *coreworld.World, x, y, z int, face int32) bool {
	dx, dy, dz := bedrockFaceOffset(face)
	return bedrockSolidSupport(world.GetBlock(x-dx, y-dy, z-dz))
}

func bedrockHeldLightLevel(stack player.ItemStack) int {
	level := stack.Damage
	const prefix = "minecraft:light_block_"
	if strings.HasPrefix(stack.ItemID, prefix) {
		if parsed, err := strconv.Atoi(strings.TrimPrefix(stack.ItemID, prefix)); err == nil {
			level = parsed
		}
	}
	if level < 0 {
		return 0
	}
	if level > 15 {
		return 15
	}
	return level
}

func (s *Server) bedrockRedstoneWireProperties(x, y, z int) map[string]string {
	props := map[string]string{"east": "none", "north": "none", "power": "0", "south": "none", "west": "none"}
	for direction, offset := range map[string][2]int{
		"north": {0, -1}, "south": {0, 1}, "west": {-1, 0}, "east": {1, 0},
	} {
		nx, nz := x+offset[0], z+offset[1]
		neighbor := s.bedrockWorld().GetBlock(nx, y, nz)
		if bedrockRedstoneConnectable(neighbor.ResourceLocation()) {
			props[direction] = "side"
			continue
		}
		if !bedrockSolidSupport(neighbor) && bedrockRedstoneConnectable(s.bedrockWorld().GetBlock(nx, y-1, nz).ResourceLocation()) {
			props[direction] = "side"
		} else if bedrockSolidSupport(neighbor) && bedrockRedstoneConnectable(s.bedrockWorld().GetBlock(nx, y+1, nz).ResourceLocation()) {
			props[direction] = "up"
		}
	}
	return props
}

func bedrockRedstoneConnectable(name string) bool {
	return name == "minecraft:redstone_wire" || name == "minecraft:repeater" || name == "minecraft:comparator" ||
		coreworld.IsRedstoneSource(name) || coreworld.IsRedstoneLoad(name)
}

func (s *Server) refreshBedrockWireConnections(x, y, z int) {
	positions := [][3]int{{x, y, z}}
	for _, offset := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
		for dy := -1; dy <= 1; dy++ {
			positions = append(positions, [3]int{x + offset[0], y + dy, z + offset[1]})
		}
	}
	for _, pos := range positions {
		wire := s.bedrockWorld().GetBlock(pos[0], pos[1], pos[2])
		if wire.ResourceLocation() != "minecraft:redstone_wire" {
			continue
		}
		props := s.bedrockRedstoneWireProperties(pos[0], pos[1], pos[2])
		if power := wire.Properties["power"]; power != "" {
			props["power"] = power
		}
		wire.Properties = props
		s.setBedrockActionBlock(pos[0], pos[1], pos[2], wire)
	}
}

func bedrockIsDoor(name string) bool {
	return strings.HasSuffix(name, "_door") && !bedrockIsTrapdoor(name)
}

// Bedrock's canonical oak trapdoor identifier is minecraft:trapdoor. Other
// wood types (and Java's oak identifier) use the *_trapdoor form.
func bedrockIsTrapdoor(name string) bool {
	return name == "minecraft:trapdoor" || strings.HasSuffix(name, "_trapdoor")
}

func bedrockIsBed(name string) bool {
	return strings.HasSuffix(name, "_bed")
}

func bedrockPlayerFacing(yaw float32) string {
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

func bedrockOppositeFacing(facing string) string {
	switch facing {
	case "north":
		return "south"
	case "south":
		return "north"
	case "east":
		return "west"
	default:
		return "east"
	}
}

func bedrockHorizontalOffset(facing string) (int, int) {
	switch facing {
	case "north":
		return 0, -1
	case "south":
		return 0, 1
	case "east":
		return 1, 0
	default:
		return -1, 0
	}
}

func bedrockFacingForFace(face int32) string {
	switch face {
	case 0:
		return "down"
	case 1:
		return "up"
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

func bedrockFacingForFaceOrPlayer(face int32, playerFacing string) string {
	if face >= 2 && face <= 5 {
		return bedrockFacingForFace(face)
	}
	return playerFacing
}

func bedrockAxisForFace(face int32) string {
	switch face {
	case 0, 1:
		return "y"
	case 2, 3:
		return "z"
	default:
		return "x"
	}
}

func bedrockPlacementHalf(i intent.BlockInteractIntent) string {
	if i.Face == 0 || (i.Face >= 2 && i.ClickY > 0.5) {
		return "top"
	}
	return "bottom"
}

func bedrockDoorHinge(facing string, clickX, clickZ float32) string {
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

func bedrockSignRotation(yaw float32) int {
	rotation := int((yaw+180)*16/360+0.5) & 15
	return rotation
}

func bedrockFishBucketEntity(itemID string) corentity.EntityType {
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
