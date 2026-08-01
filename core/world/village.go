package world

import (
	"math"

	"GoCraft/core/entity"
	"GoCraft/core/spatial"
)

const (
	villageCellSize        = 384
	villageSalt     uint64 = 0x76696c6c61676531
)

// VillageCenter describes a deterministically-placed village.
type VillageCenter struct {
	WorldX, WorldZ int
	Biome          string
	Hash           uint64
}

func isVillageBiome(biome string) bool {
	switch biome {
	case "minecraft:plains", "minecraft:meadow",
		"minecraft:savanna", "minecraft:desert",
		"minecraft:taiga", "minecraft:snowy_plains":
		return true
	}
	return false
}

// villageCenterInCell returns the deterministic village center for a placement
// cell when that cell contains a village on suitable terrain.
func (g *OverworldGenerator) villageCenterInCell(cellX, cellZ int) (VillageCenter, bool) {
	h := g.featureHash(int32(cellX), int32(cellZ), villageSalt)
	if h%5 != 0 {
		return VillageCenter{}, false
	}
	state := h
	const margin = 32
	cx := cellX*villageCellSize + margin + int(nextRandom(&state)*float64(villageCellSize-2*margin))
	cz := cellZ*villageCellSize + margin + int(nextRandom(&state)*float64(villageCellSize-2*margin))
	terrain := g.sampleTerrain(cx, cz)
	if !isVillageBiome(terrain.biome) {
		return VillageCenter{}, false
	}
	if terrain.height <= SeaLevel || terrain.height > 210 {
		return VillageCenter{}, false
	}
	return VillageCenter{WorldX: cx, WorldZ: cz, Biome: terrain.biome, Hash: h}, true
}

// NearestVillage finds the closest generated village center within maxDistance
// blocks of x/z. The lookup uses the same placement rules as chunk generation.
func (g *OverworldGenerator) NearestVillage(x, z, maxDistance int) (VillageCenter, bool) {
	if maxDistance < 0 {
		return VillageCenter{}, false
	}
	maxDistanceSquared := int64(maxDistance) * int64(maxDistance)
	bestDistanceSquared := maxDistanceSquared + 1
	var best VillageCenter
	found := false
	for cellX := floorDiv(x-maxDistance, villageCellSize); cellX <= floorDiv(x+maxDistance, villageCellSize); cellX++ {
		for cellZ := floorDiv(z-maxDistance, villageCellSize); cellZ <= floorDiv(z+maxDistance, villageCellSize); cellZ++ {
			center, ok := g.villageCenterInCell(cellX, cellZ)
			if !ok {
				continue
			}
			dx := int64(center.WorldX - x)
			dz := int64(center.WorldZ - z)
			distanceSquared := dx*dx + dz*dz
			if distanceSquared <= maxDistanceSquared && distanceSquared < bestDistanceSquared {
				best = center
				bestDistanceSquared = distanceSquared
				found = true
			}
		}
	}
	return best, found
}

// VillageCentersNear returns every village center whose structures could
// overlap the given chunk (search radius = 100 blocks beyond chunk edge).
func (g *OverworldGenerator) VillageCentersNear(chunkX, chunkZ int32) []VillageCenter {
	const searchRadius = 100
	chunkMinX := int(chunkX) * SectionSize
	chunkMinZ := int(chunkZ) * SectionSize

	var result []VillageCenter
	for cellX := floorDiv(chunkMinX-searchRadius, villageCellSize); cellX <= floorDiv(chunkMinX+SectionSize+searchRadius, villageCellSize); cellX++ {
		for cellZ := floorDiv(chunkMinZ-searchRadius, villageCellSize); cellZ <= floorDiv(chunkMinZ+SectionSize+searchRadius, villageCellSize); cellZ++ {
			if center, ok := g.villageCenterInCell(cellX, cellZ); ok {
				result = append(result, center)
			}
		}
	}
	return result
}

// villageStyle holds biome-dependent building materials.
type villageStyle struct {
	wall       Block
	roof       Block
	pillar     Block
	fence      Block
	log        Block
	doorName   string
	roofStairs string
}

func villageStyleFor(biome string) villageStyle {
	switch biome {
	case "minecraft:desert":
		return villageStyle{
			wall:       block("sandstone"),
			roof:       block("cut_sandstone"),
			pillar:     block("sandstone"),
			fence:      block("oak_fence"),
			log:        block("oak_log"),
			doorName:   "oak_door",
			roofStairs: "sandstone_stairs",
		}
	case "minecraft:savanna":
		return villageStyle{
			wall:       block("acacia_planks"),
			roof:       block("acacia_planks"),
			pillar:     block("cobblestone"),
			fence:      block("acacia_fence"),
			log:        block("acacia_log"),
			doorName:   "acacia_door",
			roofStairs: "acacia_stairs",
		}
	case "minecraft:taiga", "minecraft:snowy_plains":
		return villageStyle{
			wall:       block("spruce_planks"),
			roof:       block("spruce_planks"),
			pillar:     block("cobblestone"),
			fence:      block("spruce_fence"),
			log:        block("spruce_log"),
			doorName:   "spruce_door",
			roofStairs: "spruce_stairs",
		}
	default: // plains, meadow
		return villageStyle{
			wall:       block("oak_planks"),
			roof:       block("oak_planks"),
			pillar:     block("cobblestone"),
			fence:      block("oak_fence"),
			log:        block("oak_log"),
			doorName:   "oak_door",
			roofStairs: "oak_stairs",
		}
	}
}

type villageBuilding struct {
	centerX, groundY, centerZ int
	width, depth              int
	variant                   int
	farm                      bool
}

// villageLayout is the single deterministic source for both structures and
// residents, ensuring every spawned villager refers to a generated house.
func (g *OverworldGenerator) villageLayout(v VillageCenter) []villageBuilding {
	state := v.Hash
	buildingCount := 10 + int(nextRandom(&state)*6) // 10–15 buildings per village
	offsets := [][2]int{
		// Inner ring — 4 cardinal, 4 diagonal
		{14, 0}, {-14, 0}, {0, 14}, {0, -14},
		{12, 12}, {-12, 12}, {12, -12}, {-12, -12},
		// Outer ring — more houses further from center
		{22, 0}, {-22, 0}, {0, 22}, {0, -22},
		{20, 10}, {-20, 10}, {20, -10}, {-20, -10},
		{10, 20}, {-10, 20}, {10, -20}, {-10, -20},
		{28, 6}, {-28, 6}, {28, -6}, {-28, -6},
	}
	buildings := make([]villageBuilding, 0, buildingCount)
	for i := 0; i < buildingCount && i < len(offsets); i++ {
		j := i + int(nextRandom(&state)*float64(len(offsets)-i))
		if j >= len(offsets) {
			j = len(offsets) - 1
		}
		offsets[i], offsets[j] = offsets[j], offsets[i]
		x := v.WorldX + offsets[i][0]
		z := v.WorldZ + offsets[i][1]
		y := g.SurfaceHeight(x, z)
		if y <= SeaLevel {
			continue
		}
		building := villageBuilding{centerX: x, groundY: y, centerZ: z, width: 7, depth: 5, variant: i}
		switch i % 6 {
		case 0:
			building.farm = true
		case 3:
			building.width, building.depth = 9, 7 // larger house fits 2 beds comfortably
		}
		buildings = append(buildings, building)
	}
	return buildings
}

// VillageResident describes one deterministic village resident and the home,
// bed, and job-site blocks assigned to it.
type VillageResident struct {
	Home        spatial.BlockPos
	Center      spatial.BlockPos
	Spawn       spatial.BlockPos
	Bed         spatial.BlockPos
	Workstation spatial.BlockPos
	Profession  entity.VillagerProfession
}

// VillageResidents returns one resident for every generated house. Farms are
// assigned to farmers first; remaining professions match each house job site.
func (g *OverworldGenerator) VillageResidents(v VillageCenter) []VillageResident {
	layout := g.villageLayout(v)
	farms := make([]villageBuilding, 0, 2)
	houses := make([]villageBuilding, 0, len(layout))
	for _, building := range layout {
		if building.farm {
			farms = append(farms, building)
		} else {
			houses = append(houses, building)
		}
	}
	residents := make([]VillageResident, 0, len(houses)*2)
	for i, house := range houses {
		hw, hd := house.width/2, house.depth/2
		workstation := spatial.BlockPos{X: int32(house.centerX + hw - 1), Y: int32(house.groundY + 1), Z: int32(house.centerZ - hd + 1)}
		profession := villageProfessionForVariant(house.variant)
		if i < len(farms) {
			farm := farms[i]
			profession = entity.VillagerProfessionFarmer
			workstation = spatial.BlockPos{X: int32(farm.centerX + 2), Y: int32(farm.groundY), Z: int32(farm.centerZ + 2)}
		}
		center := spatial.BlockPos{X: int32(v.WorldX), Y: int32(g.SurfaceHeight(v.WorldX, v.WorldZ) + 1), Z: int32(v.WorldZ)}
		home := spatial.BlockPos{X: int32(house.centerX), Y: int32(house.groundY + 1), Z: int32(house.centerZ)}
		spawnPos := spatial.BlockPos{X: int32(house.centerX), Y: int32(g.SurfaceHeight(house.centerX, house.centerZ+hd+1) + 1), Z: int32(house.centerZ + hd + 1)}

		// Resident 1 — left bed
		bed1 := spatial.BlockPos{X: int32(house.centerX - hw + 2), Y: int32(house.groundY + 1), Z: int32(house.centerZ + hd - 1)}
		residents = append(residents, VillageResident{
			Home: home, Center: center, Spawn: spawnPos,
			Bed: bed1, Workstation: workstation, Profession: profession,
		})

		// Resident 2 — right bed (only present when house is wide enough)
		if hw >= 3 {
			bed2 := spatial.BlockPos{X: int32(house.centerX + hw - 2), Y: int32(house.groundY + 1), Z: int32(house.centerZ + hd - 1)}
			profession2 := villageProfessionForVariant(house.variant + 1)
			residents = append(residents, VillageResident{
				Home: home, Center: center,
				Spawn:       spatial.BlockPos{X: spawnPos.X + 2, Y: spawnPos.Y, Z: spawnPos.Z},
				Bed:         bed2,
				Workstation: workstation,
				Profession:  profession2,
			})
		}
	}
	return residents
}

func villageProfessionForVariant(variant int) entity.VillagerProfession {
	switch variant % 6 {
	case 1:
		return entity.VillagerProfessionLibrarian
	case 2:
		return entity.VillagerProfessionFletcher
	case 3:
		return entity.VillagerProfessionToolsmith
	case 4:
		return entity.VillagerProfessionArmorer
	case 5:
		return entity.VillagerProfessionShepherd
	default:
		return entity.VillagerProfessionCartographer
	}
}

// addVillageStructures places all village fragments that fall inside chunk c.
func (g *OverworldGenerator) addVillageStructures(c *Chunk) {
	for _, village := range g.VillageCentersNear(c.X, c.Z) {
		wellY := g.SurfaceHeight(village.WorldX, village.WorldZ)
		if wellY <= SeaLevel || wellY > 210 {
			continue
		}
		style := villageStyleFor(village.Biome)
		g.placeWell(c, village.WorldX, wellY, village.WorldZ, style)
		for _, building := range g.villageLayout(village) {
			g.placeVillagePath(c, village.WorldX, village.WorldZ, building.centerX, building.centerZ)
			if building.farm {
				g.placeVillageFarm(c, building.centerX, building.groundY, building.centerZ, style)
			} else {
				g.placeVillageHouse(c, building.centerX, building.groundY, building.centerZ,
					building.width, building.depth, style, building.variant)
			}
		}
	}
}

// addBedBlockEntity appends a bed block entity record to c so the Java client
// renders the correct bed model. Beds require a block entity with their dye
// color in NBT (color=14 for red).
//
// Network NBT format for a red bed:
//
//	TAG_Compound '' { color: TAG_Int(14) }
func addBedBlockEntity(c *Chunk, wx, y, wz int) {
	// Network NBT (1.20.2+): root compound has NO name field — just type byte
	// then payload. Format: TAG_Compound(0x0a) | TAG_Int "color"=14 | TAG_End.
	nbt := []byte{
		0x0a,                               // TAG_Compound (root, no name in network NBT)
		0x03,                               // TAG_Int type
		0x00, 0x05,                         // name length=5
		'c', 'o', 'l', 'o', 'r',           // "color"
		0x00, 0x00, 0x00, 0x0e,             // value=14 (red, big-endian)
		0x00,                               // TAG_End
	}
	c.BlockEntities = append(c.BlockEntities, BlockEntity{
		X:    wx,
		Y:    y,
		Z:    wz,
		Type: "minecraft:bed",
		Data: nbt,
	})
}

// setVB places block b at world coordinates (wx, y, wz), skipping if outside c.
func setVB(c *Chunk, wx, y, wz int, b Block) {
	chunkMinX := int(c.X) * SectionSize
	chunkMinZ := int(c.Z) * SectionSize
	lx := wx - chunkMinX
	lz := wz - chunkMinZ
	if lx < 0 || lx >= SectionSize || lz < 0 || lz >= SectionSize {
		return
	}
	setGeneratedBlock(c, lx, y, lz, b)
}

// fillDown fills air/water downward from (wx, gy-1, wz) with b until solid ground.
func fillDown(c *Chunk, wx, gy, wz int, b Block) {
	chunkMinX := int(c.X) * SectionSize
	chunkMinZ := int(c.Z) * SectionSize
	lx := wx - chunkMinX
	lz := wz - chunkMinZ
	if lx < 0 || lx >= SectionSize || lz < 0 || lz >= SectionSize {
		return
	}
	for y := gy - 1; y >= gy-6; y-- {
		existing := generatedBlock(c, lx, y, lz)
		if !existing.IsAir() && existing.ResourceLocation() != "minecraft:water" {
			break
		}
		setGeneratedBlock(c, lx, y, lz, b)
	}
}

func (g *OverworldGenerator) placeWell(c *Chunk, cx, gy, cz int, style villageStyle) {
	stone := block("stone_bricks")

	// 5×5 cobblestone base; center 3×3 is water.
	for x := cx - 2; x <= cx+2; x++ {
		for z := cz - 2; z <= cz+2; z++ {
			setVB(c, x, gy, z, style.pillar)
			fillDown(c, x, gy, z, style.pillar)
		}
	}
	for x := cx - 1; x <= cx+1; x++ {
		for z := cz - 1; z <= cz+1; z++ {
			setVB(c, x, gy, z, waterBlock)
		}
	}

	// Stone brick ring at gy+1 and gy+2.
	for x := cx - 2; x <= cx+2; x++ {
		for z := cz - 2; z <= cz+2; z++ {
			if x != cx-2 && x != cx+2 && z != cz-2 && z != cz+2 {
				continue
			}
			setVB(c, x, gy+1, z, stone)
			setVB(c, x, gy+2, z, stone)
		}
	}

	// Fence posts at inner corners (gy+3 and gy+4).
	for _, off := range [][2]int{{-1, -1}, {-1, 1}, {1, -1}, {1, 1}} {
		setVB(c, cx+off[0], gy+3, cz+off[1], style.fence)
		setVB(c, cx+off[0], gy+4, cz+off[1], style.fence)
	}

	// Log cross-beam at gy+4.
	for x := cx - 1; x <= cx+1; x++ {
		setVB(c, x, gy+4, cz, style.log)
	}
	for z := cz - 1; z <= cz+1; z++ {
		setVB(c, cx, gy+4, z, style.log)
	}
}

func (g *OverworldGenerator) placeVillageHouse(c *Chunk, cx, gy, cz, width, depth int, style villageStyle, variant int) {
	doorLower := blockProps(style.doorName, "facing", "south", "half", "lower", "hinge", "left", "open", "false", "powered", "false")
	doorUpper := blockProps(style.doorName, "facing", "south", "half", "upper", "hinge", "left", "open", "false", "powered", "false")
	glass := block("glass")

	hw := width / 2
	hd := depth / 2

	// Remove trees, flowers and tall grass from the house volume before placing
	// the shell. This also prevents foliage from surviving inside the rooms.
	for x := cx - hw - 1; x <= cx+hw+1; x++ {
		for z := cz - hd - 1; z <= cz+hd+1; z++ {
			for y := gy + 1; y <= gy+6+hw; y++ {
				setVB(c, x, y, z, Air)
			}
		}
	}

	// Stable foundation and a real interior floor.
	for x := cx - hw; x <= cx+hw; x++ {
		for z := cz - hd; z <= cz+hd; z++ {
			setVB(c, x, gy, z, style.wall)
			fillDown(c, x, gy, z, style.pillar)
		}
	}

	for x := cx - hw; x <= cx+hw; x++ {
		for z := cz - hd; z <= cz+hd; z++ {
			onEdge := x == cx-hw || x == cx+hw || z == cz-hd || z == cz+hd
			if !onEdge {
				continue
			}
			isDoor := z == cz+hd && x == cx
			isWindow := !isDoor && ((z == cz-hd && x == cx) ||
				(x == cx-hw && z == cz) ||
				(x == cx+hw && z == cz))
			for y := gy + 1; y <= gy+4; y++ {
				if isDoor && (y == gy+1 || y == gy+2) {
					continue
				}
				if isWindow && y == gy+2 {
					setVB(c, x, y, z, glass)
					continue
				}
				setVB(c, x, y, z, style.wall)
			}
		}
	}

	setVB(c, cx, gy+1, cz+hd, doorLower)
	setVB(c, cx, gy+2, cz+hd, doorUpper)

	// Stepped stair roof with a one-block overhang and filled gable ends.
	for layer := 0; layer <= hw; layer++ {
		y := gy + 5 + layer
		leftX := cx - hw - 1 + layer
		rightX := cx + hw + 1 - layer
		leftStair := blockProps(style.roofStairs, "facing", "east", "half", "bottom", "shape", "straight", "waterlogged", "false")
		rightStair := blockProps(style.roofStairs, "facing", "west", "half", "bottom", "shape", "straight", "waterlogged", "false")
		for z := cz - hd - 1; z <= cz+hd+1; z++ {
			setVB(c, leftX, y, z, leftStair)
			setVB(c, rightX, y, z, rightStair)
		}
	}
	for x := cx - hw; x <= cx+hw; x++ {
		gableTop := gy + 5 + hw + 1 - absInt(x-cx)
		for y := gy + 5; y < gableTop; y++ {
			setVB(c, x, y, cz-hd, style.wall)
			setVB(c, x, y, cz+hd, style.wall)
		}
	}
	ridgeY := gy + 5 + hw + 1
	for z := cz - hd - 1; z <= cz+hd+1; z++ {
		setVB(c, cx, ridgeY, z, style.roof)
	}

	// Each house has two beds (one per side of the back wall) so that two
	// villagers can share a home, and a workstation near the front corner.
	bedFoot := blockProps("red_bed", "facing", "north", "occupied", "false", "part", "foot")
	bedHead := blockProps("red_bed", "facing", "north", "occupied", "false", "part", "head")
	// Bed 1 — left side of the back wall
	bed1X := cx - hw + 2
	setVB(c, bed1X, gy+1, cz+hd-1, bedFoot)
	setVB(c, bed1X, gy+1, cz+hd-2, bedHead)
	addBedBlockEntity(c, bed1X, gy+1, cz+hd-1)
	addBedBlockEntity(c, bed1X, gy+1, cz+hd-2)
	// Bed 2 — right side of the back wall (only fits when width >= 7)
	if hw >= 3 {
		bed2X := cx + hw - 2
		setVB(c, bed2X, gy+1, cz+hd-1, bedFoot)
		setVB(c, bed2X, gy+1, cz+hd-2, bedHead)
		addBedBlockEntity(c, bed2X, gy+1, cz+hd-1)
		addBedBlockEntity(c, bed2X, gy+1, cz+hd-2)
	}

	workstation := villageWorkstation(variant)
	setVB(c, cx+hw-1, gy+1, cz-hd+1, workstation)
}

func villageWorkstation(variant int) Block {
	switch variant % 6 {
	case 1:
		return block("lectern")
	case 2:
		return block("fletching_table")
	case 3:
		return block("smithing_table")
	case 4:
		return block("blast_furnace")
	case 5:
		return block("loom")
	default:
		return block("cartography_table")
	}
}

func (g *OverworldGenerator) placeVillageFarm(c *Chunk, cx, gy, cz int, style villageStyle) {
	farmland := blockProps("farmland", "moisture", "7")
	wheat := blockProps("wheat", "age", "7")
	composter := blockProps("composter", "level", "0")

	for x := cx - 3; x <= cx+3; x++ {
		for z := cz - 3; z <= cz+3; z++ {
			onEdge := x == cx-3 || x == cx+3 || z == cz-3 || z == cz+3
			if onEdge {
				if !(x == cx && z == cz+3) {
					setVB(c, x, gy, z, style.fence)
				}
				continue
			}
			if x == cx && z == cz {
				setVB(c, x, gy, z, waterBlock)
				continue
			}
			if x == cx+2 && z == cz+2 {
				setVB(c, x, gy, z, composter)
				setVB(c, x, gy+1, z, Air)
				continue
			}
			setVB(c, x, gy, z, farmland)
			setVB(c, x, gy+1, z, wheat)
		}
	}
}

func (g *OverworldGenerator) placeVillagePath(c *Chunk, x1, z1, x2, z2 int) {
	dx := x2 - x1
	dz := z2 - z1
	steps := absInt(dx)
	if absInt(dz) > steps {
		steps = absInt(dz)
	}
	if steps == 0 {
		return
	}
	chunkMinX := int(c.X) * SectionSize
	chunkMinZ := int(c.Z) * SectionSize
	pathBlock := block("gravel")
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		wx := x1 + int(math.Round(float64(dx)*t))
		wz := z1 + int(math.Round(float64(dz)*t))
		lx := wx - chunkMinX
		lz := wz - chunkMinZ
		if lx < 0 || lx >= SectionSize || lz < 0 || lz >= SectionSize {
			continue
		}
		surfY := g.SurfaceHeight(wx, wz)
		if surfY <= SeaLevel {
			continue
		}
		setGeneratedBlock(c, lx, surfY, lz, pathBlock)
	}
}
