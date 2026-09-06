package world

import (
	"math"
	"strconv"
	"strings"
)

const (
	cropGateSalt       = uint64(0x243f6a8885a308d3)
	cropGrowthSalt     = uint64(0x13198a2e03707344)
	cropDirectionSalt  = uint64(0xa4093822299f31d0)
	cropBoneMealSalt   = uint64(0x082efa98ec4e6c89)
	cropLegacyTickSalt = uint64(0x452821e638d01377)
)

var horizontalCropDirections = [...]struct {
	dx, dz int
	facing string
}{
	{dx: 0, dz: -1, facing: "north"},
	{dx: 0, dz: 1, facing: "south"},
	{dx: -1, dz: 0, facing: "west"},
	{dx: 1, dz: 0, facing: "east"},
}

// CropMaxAge returns the conceptual maximum age used by a supported crop.
// Torchflower crops only encode ages 0 and 1: conceptual age 2 is represented
// by the mature minecraft:torchflower block.
func CropMaxAge(name string) (int, bool) {
	switch name {
	case "minecraft:wheat", "minecraft:carrots", "minecraft:potatoes",
		"minecraft:pumpkin_stem", "minecraft:melon_stem":
		return 7, true
	case "minecraft:beetroots", "minecraft:nether_wart", "minecraft:sweet_berry_bush":
		return 3, true
	case "minecraft:torchflower_crop":
		return 2, true
	case "minecraft:cocoa":
		return 2, true
	case "minecraft:pitcher_crop":
		return 4, true
	default:
		return 0, false
	}
}

// CropAge reads a crop's age property. Missing or malformed ages are treated
// as zero so imported worlds with incomplete state maps remain recoverable.
func CropAge(block Block) int {
	age, err := strconv.Atoi(block.Properties["age"])
	if err != nil || age < 0 {
		return 0
	}
	return age
}

// SetCropAge returns the canonical state for age. Torchflower's final stage is
// a block conversion, matching Pumpkin/vanilla rather than inventing age=2.
func SetCropAge(block Block, age int) Block {
	if block.ResourceLocation() == "minecraft:torchflower_crop" && age >= 2 {
		return Block{Namespace: "minecraft", Name: "torchflower"}
	}
	maximum, ok := CropMaxAge(block.ResourceLocation())
	if ok && age > maximum {
		age = maximum
	}
	if age < 0 {
		age = 0
	}
	block = copyWorldBlock(block)
	block.Properties["age"] = strconv.Itoa(age)
	return block
}

func isCanonicalCrop(name string) bool {
	if _, ok := CropMaxAge(name); ok {
		return true
	}
	return name == "minecraft:attached_pumpkin_stem" || name == "minecraft:attached_melon_stem"
}

// isTickableGrowth reports whether a block participates in the crop scan: a
// canonical crop, a self-paced tall plant (sugar cane, cactus), a kelp tip,
// a nether vine head, or a bamboo segment/sapling.
func isTickableGrowth(name string) bool {
	return isCanonicalCrop(name) || isTallPlantGrowth(name) ||
		name == "minecraft:kelp" || isNetherVineHead(name) ||
		isBambooGrowthBlock(name)
}

func standardMoistureCrop(name string) bool {
	switch name {
	case "minecraft:wheat", "minecraft:carrots", "minecraft:potatoes",
		"minecraft:beetroots", "minecraft:torchflower_crop",
		"minecraft:pumpkin_stem", "minecraft:melon_stem":
		return true
	default:
		return false
	}
}

// CropAvailableMoisture ports Pumpkin's vanilla-style 3x3 farmland growth
// factor. Dry farmland contributes 1, hydrated farmland contributes 3, and
// every non-central contribution is quartered. Crowded rows/diagonals halve
// the resulting factor.
func (w *World) CropAvailableMoisture(x, y, z int, crop Block) float64 {
	moisture := 1.0
	for dx := -1; dx <= 1; dx++ {
		for dz := -1; dz <= 1; dz++ {
			local := 0.0
			below, loaded := w.blockIfLoaded(x+dx, y-1, z+dz)
			if loaded && below.ResourceLocation() == "minecraft:farmland" {
				local = 1
				farmlandMoisture, _ := strconv.Atoi(below.Properties["moisture"])
				if farmlandMoisture != 0 {
					local = 3
				}
			}
			if dx != 0 || dz != 0 {
				local /= 4
			}
			moisture += local
		}
	}

	name := crop.ResourceLocation()
	same := func(dx, dz int) bool {
		neighbor, loaded := w.blockIfLoaded(x+dx, y, z+dz)
		return loaded && neighbor.ResourceLocation() == name
	}
	horizontal := same(-1, 0) || same(1, 0)
	vertical := same(0, -1) || same(0, 1)
	if (horizontal && vertical) || same(-1, -1) || same(1, -1) || same(1, 1) || same(-1, 1) {
		moisture /= 2
	}
	return moisture
}

// CropGrowthDenominator returns the number of equally likely outcomes for a
// standard crop growth roll. Growth occurs on outcome zero.
func CropGrowthDenominator(moisture float64) int {
	if moisture <= 0 {
		moisture = 1
	}
	return int(math.Floor(25/moisture)) + 1
}

func cropRandom(seed uint64, x, y, z int, salt uint64, bound int) int {
	if bound <= 1 {
		return 0
	}
	n := seed ^ uint64(int64(x)*0x9e3779b1) ^ uint64(int64(y)*0x85ebca77) ^
		uint64(int64(z)*0xc2b2ae3d) ^ salt
	n ^= n >> 30
	n *= 0xbf58476d1ce4e5b9
	n ^= n >> 27
	n *= 0x94d049bb133111eb
	n ^= n >> 31
	return int(n % uint64(bound))
}

func (w *World) blockIfLoaded(x, y, z int) (Block, bool) {
	if y < WorldMinY || y > WorldMaxY {
		return Air, true
	}
	cx, cz := ChunkCoordsFor(x, z)
	w.mu.RLock()
	chunk, loaded := w.chunks[[2]int32{cx, cz}]
	w.mu.RUnlock()
	if !loaded {
		return Air, false
	}
	relY := y - WorldMinY
	section := chunk.Sections[relY/SectionSize]
	if section == nil {
		return Air, true
	}
	lx := x - int(cx)*SectionSize
	lz := z - int(cz)*SectionSize
	return section.At(lx, relY%SectionSize, lz), true
}

func isFarmlandPlantedCrop(name string) bool {
	switch name {
	case "minecraft:wheat", "minecraft:carrots", "minecraft:potatoes", "minecraft:beetroots",
		"minecraft:pumpkin_stem", "minecraft:melon_stem", "minecraft:attached_pumpkin_stem",
		"minecraft:attached_melon_stem", "minecraft:torchflower_crop", "minecraft:pitcher_crop":
		return true
	default:
		return false
	}
}

func validSweetBerrySupport(name string) bool {
	switch name {
	case "minecraft:grass_block", "minecraft:dirt", "minecraft:coarse_dirt", "minecraft:podzol",
		"minecraft:rooted_dirt", "minecraft:mycelium", "minecraft:moss_block", "minecraft:mud",
		"minecraft:farmland":
		return true
	default:
		return false
	}
}

// CanCropSurvive applies the crop-specific support rules shared by Java,
// Bedrock, random ticks, and neighbour-removal handling.
func CanCropSurvive(block, support Block) bool {
	name := block.ResourceLocation()
	if isFarmlandPlantedCrop(name) {
		return support.ResourceLocation() == "minecraft:farmland"
	}
	switch name {
	case "minecraft:nether_wart":
		return support.ResourceLocation() == "minecraft:soul_sand"
	case "minecraft:sweet_berry_bush", "minecraft:torchflower":
		return validSweetBerrySupport(support.ResourceLocation())
	case "minecraft:cocoa":
		// Cocoa attaches horizontally. GoCraft does not yet retain the attachment
		// support position here, so leave its legacy survival behaviour unchanged.
		return true
	default:
		return true
	}
}

func (w *World) cropCanSurviveAt(x, y, z int, block Block) bool {
	support, loaded := w.blockIfLoaded(x, y-1, z)
	return loaded && CanCropSurvive(block, support)
}

// BreakUnsupportedCropsAbove performs an immediate canonical neighbour update
// for agriculture blocks after their support changes.
func (w *World) BreakUnsupportedCropsAbove(x, y, z int) []BlockChange {
	changes := make([]BlockChange, 0, 2)
	for plantY := y + 1; plantY <= WorldMaxY; plantY++ {
		plant, loaded := w.blockIfLoaded(x, plantY, z)
		if !loaded || !isCanonicalCrop(plant.ResourceLocation()) || w.cropCanSurviveAt(x, plantY, z, plant) {
			break
		}
		w.SetBlock(x, plantY, z, Air)
		changes = append(changes, BlockChange{X: x, Y: plantY, Z: z, Block: Air})
	}
	return changes
}

func gourdForStem(name string) (fruit, attached, stem string, ok bool) {
	switch name {
	case "minecraft:pumpkin_stem", "minecraft:attached_pumpkin_stem":
		return "pumpkin", "attached_pumpkin_stem", "pumpkin_stem", true
	case "minecraft:melon_stem", "minecraft:attached_melon_stem":
		return "melon", "attached_melon_stem", "melon_stem", true
	default:
		return "", "", "", false
	}
}

func validGourdGround(name string) bool {
	if name == "minecraft:farmland" {
		return true
	}
	switch name {
	case "minecraft:dirt", "minecraft:grass_block", "minecraft:coarse_dirt", "minecraft:podzol",
		"minecraft:rooted_dirt", "minecraft:mycelium", "minecraft:moss_block", "minecraft:mud":
		return true
	default:
		return false
	}
}

func (w *World) growGourd(x, y, z int, stem Block, seed uint64) []BlockChange {
	fruit, attached, _, ok := gourdForStem(stem.ResourceLocation())
	if !ok {
		return nil
	}
	direction := horizontalCropDirections[cropRandom(seed, x, y, z, cropDirectionSalt, len(horizontalCropDirections))]
	targetX, targetZ := x+direction.dx, z+direction.dz
	target, targetLoaded := w.blockIfLoaded(targetX, y, targetZ)
	ground, groundLoaded := w.blockIfLoaded(targetX, y-1, targetZ)
	if !targetLoaded || !groundLoaded || !target.IsAir() || !validGourdGround(ground.ResourceLocation()) {
		return nil
	}
	fruitBlock := Block{Namespace: "minecraft", Name: fruit}
	attachedBlock := Block{Namespace: "minecraft", Name: attached, Properties: map[string]string{"facing": direction.facing}}
	w.SetBlock(targetX, y, targetZ, fruitBlock)
	w.SetBlock(x, y, z, attachedBlock)
	return []BlockChange{
		{X: targetX, Y: y, Z: targetZ, Block: fruitBlock},
		{X: x, Y: y, Z: z, Block: attachedBlock},
	}
}

// UpdateAttachedStem restores an attached stem to age 7 when its pointed fruit
// disappears or changes type.
func (w *World) UpdateAttachedStem(x, y, z int) (BlockChange, bool) {
	block, loaded := w.blockIfLoaded(x, y, z)
	if !loaded {
		return BlockChange{}, false
	}
	fruit, _, stem, ok := gourdForStem(block.ResourceLocation())
	if !ok || (block.ResourceLocation() != "minecraft:attached_pumpkin_stem" && block.ResourceLocation() != "minecraft:attached_melon_stem") {
		return BlockChange{}, false
	}
	dx, dz := 0, 0
	switch block.Properties["facing"] {
	case "north":
		dz = -1
	case "south":
		dz = 1
	case "west":
		dx = -1
	case "east":
		dx = 1
	default:
		return BlockChange{}, false
	}
	neighbor, neighborLoaded := w.blockIfLoaded(x+dx, y, z+dz)
	if neighborLoaded && neighbor.ResourceLocation() == "minecraft:"+fruit {
		return BlockChange{}, false
	}
	replacement := Block{Namespace: "minecraft", Name: stem, Properties: map[string]string{"age": "7"}}
	w.SetBlock(x, y, z, replacement)
	return BlockChange{X: x, Y: y, Z: z, Block: replacement}, true
}

// UpdateAttachedStemsAround updates every loaded stem that could point at the
// changed fruit position.
func (w *World) UpdateAttachedStemsAround(x, y, z int) []BlockChange {
	changes := make([]BlockChange, 0, 1)
	for _, direction := range horizontalCropDirections {
		if change, ok := w.UpdateAttachedStem(x-direction.dx, y, z-direction.dz); ok {
			changes = append(changes, change)
		}
	}
	return changes
}

func (w *World) tickCropAt(x, y, z int, crop Block, tick int64, changeBudget int) []BlockChange {
	if changeBudget <= 0 {
		return nil
	}
	name := crop.ResourceLocation()
	if isTallPlantGrowth(name) {
		// Sugar cane and cactus manage their own survival through block physics;
		// the crop survival rule does not apply and would delete them.
		return w.tickTallPlantAt(x, y, z, crop)
	}
	if name == "minecraft:kelp" {
		return w.tickKelpAt(x, y, z, crop, tick)
	}
	if isNetherVineHead(name) {
		return w.tickNetherVineAt(x, y, z, crop, tick)
	}
	if isBambooGrowthBlock(name) {
		return w.tickBambooAt(x, y, z, crop, tick)
	}
	if !w.cropCanSurviveAt(x, y, z, crop) {
		w.SetBlock(x, y, z, Air)
		return []BlockChange{{X: x, Y: y, Z: z, Block: Air}}
	}
	if name == "minecraft:attached_pumpkin_stem" || name == "minecraft:attached_melon_stem" {
		if change, ok := w.UpdateAttachedStem(x, y, z); ok {
			return []BlockChange{change}
		}
		return nil
	}

	age := CropAge(crop)
	maximum, ok := CropMaxAge(name)
	if !ok || (age >= maximum && name != "minecraft:pumpkin_stem" && name != "minecraft:melon_stem") {
		return nil
	}
	seed := uint64(tick / 20)
	switch name {
	case "minecraft:beetroots":
		if cropRandom(seed, x, y, z, cropGateSalt, 3) != 0 {
			return nil
		}
	case "minecraft:nether_wart":
		if cropRandom(seed, x, y, z, cropGateSalt, 10) != 0 {
			return nil
		}
		return w.setCropAt(x, y, z, crop, age+1)
	case "minecraft:torchflower_crop":
		if cropRandom(seed, x, y, z, cropGateSalt, 2) == 0 {
			return nil
		}
	case "minecraft:sweet_berry_bush":
		if cropRandom(seed, x, y, z, cropGateSalt, 5) != 0 {
			return nil
		}
		above, loaded := w.blockIfLoaded(x, y+1, z)
		if !loaded || (!above.IsAir() && IsEntitySupportBlock(above.ResourceLocation())) {
			return nil
		}
		return w.setCropAt(x, y, z, crop, age+1)
	case "minecraft:cocoa", "minecraft:pitcher_crop":
		// Pumpkin currently has no crop behaviour for these blocks. Preserve
		// GoCraft's pre-existing bounded 1-in-5 growth instead of claiming a port.
		if cropRandom(seed, x, y, z, cropLegacyTickSalt, 5) != 0 {
			return nil
		}
		return w.setCropAt(x, y, z, crop, age+1)
	}

	if standardMoistureCrop(name) {
		denominator := CropGrowthDenominator(w.CropAvailableMoisture(x, y, z, crop))
		if cropRandom(seed, x, y, z, cropGrowthSalt, denominator) != 0 {
			return nil
		}
	}
	if (name == "minecraft:pumpkin_stem" || name == "minecraft:melon_stem") && age == 7 {
		if changeBudget < 2 {
			return nil
		}
		return w.growGourd(x, y, z, crop, seed)
	}
	return w.setCropAt(x, y, z, crop, age+1)
}

func (w *World) setCropAt(x, y, z int, crop Block, age int) []BlockChange {
	replacement := SetCropAge(crop, age)
	w.SetBlock(x, y, z, replacement)
	return []BlockChange{{X: x, Y: y, Z: z, Block: replacement}}
}

// TickCrops advances a bounded number of crops from chunks already in memory.
// Neighbour checks never generate or load chunks.
func (w *World) TickCrops(tick int64, maxChanges int) []BlockChange {
	if maxChanges <= 0 {
		return nil
	}
	w.mu.RLock()
	chunks := make([]*Chunk, 0, len(w.chunks))
	for _, chunk := range w.chunks {
		chunks = append(chunks, chunk)
	}
	w.mu.RUnlock()

	type candidate struct {
		x, y, z int
		block   Block
	}
	candidates := make([]candidate, 0, maxChanges*4)
	for _, chunk := range chunks {
		for sectionIndex, section := range chunk.Sections {
			if section == nil || section.NonAir == 0 {
				continue
			}
			palette := section.BlockPalette()
			hasCrop := false
			for _, block := range palette {
				if isTickableGrowth(block.ResourceLocation()) {
					hasCrop = true
					break
				}
			}
			if !hasCrop {
				continue
			}
			for index, paletteIndex := range section.BlockData() {
				if int(paletteIndex) >= len(palette) {
					continue
				}
				block := palette[paletteIndex]
				if !isTickableGrowth(block.ResourceLocation()) {
					continue
				}
				localX := index % SectionSize
				localZ := (index / SectionSize) % SectionSize
				localY := index / (SectionSize * SectionSize)
				candidates = append(candidates, candidate{
					x:     int(chunk.X)*SectionSize + localX,
					y:     SectionMinY(sectionIndex) + localY,
					z:     int(chunk.Z)*SectionSize + localZ,
					block: block,
				})
			}
		}
	}

	changes := make([]BlockChange, 0, maxChanges)
	for _, candidate := range candidates {
		current, loaded := w.blockIfLoaded(candidate.x, candidate.y, candidate.z)
		if !loaded || current.ResourceLocation() != candidate.block.ResourceLocation() || current.Properties["age"] != candidate.block.Properties["age"] {
			continue
		}
		remaining := maxChanges - len(changes)
		cropChanges := w.tickCropAt(candidate.x, candidate.y, candidate.z, current, tick, remaining)
		changes = append(changes, cropChanges...)
		if len(changes) >= maxChanges {
			break
		}
	}
	return changes
}

// ApplyBoneMeal applies one crop bonemeal interaction. used reports whether
// the target accepted bonemeal, even when beetroot's integer-divided roll adds
// zero stages. Nether wart deliberately never accepts bonemeal.
func (w *World) ApplyBoneMeal(x, y, z int, seed uint64) (changes []BlockChange, used bool) {
	crop := w.GetBlock(x, y, z)
	name := crop.ResourceLocation()
	if (strings.HasSuffix(name, "_sapling") && name != "minecraft:bamboo_sapling") || name == "minecraft:mangrove_propagule" {
		stage, _ := strconv.Atoi(crop.Properties["stage"])
		if stage <= 0 {
			replacement := copyWorldBlock(crop)
			replacement.Properties["stage"] = "1"
			w.SetBlock(x, y, z, replacement)
			return []BlockChange{{X: x, Y: y, Z: z, Block: replacement}}, true
		}
		grown := w.growSaplingTree(x, y, z, name, seed)
		return grown, len(grown) != 0
	}
	age := CropAge(crop)
	maximum, supported := CropMaxAge(name)
	if !supported || name == "minecraft:nether_wart" || name == "minecraft:cocoa" || name == "minecraft:pitcher_crop" || age >= maximum {
		return nil, false
	}

	increase := 2 + cropRandom(seed, x, y, z, cropBoneMealSalt, 4)
	switch name {
	case "minecraft:beetroots":
		increase /= 3
	case "minecraft:torchflower_crop", "minecraft:sweet_berry_bush":
		increase = 1
	}
	newAge := min(maximum, age+increase)
	if newAge != age {
		changes = append(changes, w.setCropAt(x, y, z, crop, newAge)...)
	}
	if (name == "minecraft:pumpkin_stem" || name == "minecraft:melon_stem") && newAge == 7 {
		grownStem := w.GetBlock(x, y, z)
		denominator := CropGrowthDenominator(w.CropAvailableMoisture(x, y, z, grownStem))
		if cropRandom(seed, x, y, z, cropGrowthSalt, denominator) == 0 {
			changes = append(changes, w.growGourd(x, y, z, grownStem, seed)...)
		}
	}
	return changes, true
}

// HarvestSweetBerryBush handles the explicit right-click harvest interaction.
func (w *World) HarvestSweetBerryBush(x, y, z int, seed uint64) (count int, changes []BlockChange, harvested bool) {
	bush := w.GetBlock(x, y, z)
	if bush.ResourceLocation() != "minecraft:sweet_berry_bush" {
		return 0, nil, false
	}
	age := CropAge(bush)
	if age < 2 {
		return 0, nil, false
	}
	count = age - 1 + cropRandom(seed, x, y, z, cropBoneMealSalt, 2)
	replacement := SetCropAge(bush, 1)
	w.SetBlock(x, y, z, replacement)
	return count, []BlockChange{{X: x, Y: y, Z: z, Block: replacement}}, true
}
