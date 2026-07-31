package world

import "math"

// SeaLevel is the Java overworld sea level.
const SeaLevel = 63

// Generator creates chunks for positions not yet loaded or generated.
// Implementations must be safe for concurrent calls.
type Generator interface {
	Generate(x, z int32) *Chunk
}

// OverworldGenerator creates deterministic, seed-backed overworld terrain.
// It is an original Go implementation, not a byte-for-byte copy of Mojang's
// density router. It models the same broad stages: climate and continents,
// erosion and ridged peaks, biome surfaces, caves, ores, and vegetation.
type OverworldGenerator struct {
	seed int64
}

func NewOverworldGenerator(seed int64) *OverworldGenerator { return &OverworldGenerator{seed: seed} }
func (g *OverworldGenerator) Seed() int64                  { return g.seed }

var generatedBiomeNames = []string{
	"deep_frozen_ocean",
	"deep_ocean",
	"frozen_ocean",
	"mushroom_fields",
	"ocean",
	"snowy_beach",
	"stony_shore",
	"beach",
	"frozen_peaks",
	"stony_peaks",
	"jagged_peaks",
	"snowy_slopes",
	"meadow",
	"windswept_hills",
	"cherry_grove",
	"badlands",
	"desert",
	"savanna",
	"bamboo_jungle",
	"jungle",
	"mangrove_swamp",
	"swamp",
	"snowy_plains",
	"taiga",
	"dark_forest",
	"flower_forest",
	"forest",
	"old_growth_birch_forest",
	"birch_forest",
	"plains",
}

// GeneratedBiomeNames returns every biome name that the GoCraft overworld
// generator can produce, without the minecraft namespace prefix.
func GeneratedBiomeNames() []string {
	return append([]string(nil), generatedBiomeNames...)
}

func block(name string) Block { return Block{Namespace: "minecraft", Name: name} }

func blockProps(name string, kvPairs ...string) Block {
	b := Block{Namespace: "minecraft", Name: name}
	if len(kvPairs) >= 2 {
		b.Properties = make(map[string]string)
		for i := 0; i+1 < len(kvPairs); i += 2 {
			b.Properties[kvPairs[i]] = kvPairs[i+1]
		}
	}
	return b
}

var (
	stoneBlock                = block("stone")
	deepslateBlock            = block("deepslate")
	grassBlock                = block("grass_block")
	dirtBlock                 = block("dirt")
	bedrockBlock              = block("bedrock")
	sandBlock                 = block("sand")
	sandstoneBlock            = block("sandstone")
	redSandBlock              = block("red_sand")
	terracottaBlock           = block("terracotta")
	gravelBlock               = block("gravel")
	mudBlock                  = block("mud")
	podzolBlock               = block("podzol")
	waterBlock                = block("water")
	iceBlock                  = block("ice")
	snowBlock                 = block("snow_block")
	oakLogBlock               = block("oak_log")
	oakLeafBlock              = block("oak_leaves")
	birchLogBlock             = block("birch_log")
	birchLeafBlock            = block("birch_leaves")
	spruceLogBlock            = block("spruce_log")
	spruceLeafBlock           = block("spruce_leaves")
	acaciaLogBlock            = block("acacia_log")
	acaciaLeafBlock           = block("acacia_leaves")
	jungleLogBlock            = block("jungle_log")
	jungleLeafBlock           = block("jungle_leaves")
	cactusBlock               = block("cactus")
	coalOreBlock              = block("coal_ore")
	deepslateCoalOreBlock     = block("deepslate_coal_ore")
	ironOreBlock              = block("iron_ore")
	deepslateIronOreBlock     = block("deepslate_iron_ore")
	copperOreBlock            = block("copper_ore")
	deepslateCopperOreBlock   = block("deepslate_copper_ore")
	goldOreBlock              = block("gold_ore")
	deepslateGoldOreBlock     = block("deepslate_gold_ore")
	redstoneOreBlock          = block("redstone_ore")
	deepslateRedstoneOreBlock = block("deepslate_redstone_ore")
	lapisOreBlock             = block("lapis_ore")
	deepslateLapisOreBlock    = block("deepslate_lapis_ore")
	diamondOreBlock           = block("diamond_ore")
	deepslateDiamondOreBlock  = block("deepslate_diamond_ore")
	emeraldOreBlock           = block("emerald_ore")
	deepslateEmeraldOreBlock  = block("deepslate_emerald_ore")

	// Ground cover — grass types
	shortGrassBlock     = block("short_grass")
	fernBlock           = block("fern")
	tallGrassLowerBlock = blockProps("tall_grass", "half", "lower")
	tallGrassUpperBlock = blockProps("tall_grass", "half", "upper")
	largeFernLowerBlock = blockProps("large_fern", "half", "lower")
	largeFernUpperBlock = blockProps("large_fern", "half", "upper")

	// Single-block flowers
	dandelionBlock       = block("dandelion")
	poppyBlock           = block("poppy")
	alliumBlock          = block("allium")
	azureBluetBlock      = block("azure_bluet")
	redTulipBlock        = block("red_tulip")
	orangeTulipBlock     = block("orange_tulip")
	whiteTulipBlock      = block("white_tulip")
	pinkTulipBlock       = block("pink_tulip")
	oxeyeDaisyBlock      = block("oxeye_daisy")
	cornflowerBlock      = block("cornflower")
	lilyOfTheValleyBlock = block("lily_of_the_valley")
	blueOrchidBlock      = block("blue_orchid")

	// Double-block tall flowers
	sunflowerLowerBlock = blockProps("sunflower", "half", "lower")
	sunflowerUpperBlock = blockProps("sunflower", "half", "upper")
	lilacLowerBlock     = blockProps("lilac", "half", "lower")
	lilacUpperBlock     = blockProps("lilac", "half", "upper")
	roseBushLowerBlock  = blockProps("rose_bush", "half", "lower")
	roseBushUpperBlock  = blockProps("rose_bush", "half", "upper")
	peonyLowerBlock     = blockProps("peony", "half", "lower")
	peonyUpperBlock     = blockProps("peony", "half", "upper")

	// Other vegetation
	deadBushBlock      = block("dead_bush")
	sugarCaneBlock     = block("sugar_cane")
	bambooBlock        = block("bamboo")
	lilyPadBlock       = block("lily_pad")
	seagrassBlock      = block("seagrass")
	brownMushroomBlock = block("brown_mushroom")
	redMushroomBlock   = block("red_mushroom")

	// Additional tree types
	darkOakLogBlock  = block("dark_oak_log")
	darkOakLeafBlock = block("dark_oak_leaves")
	cherryLogBlock   = block("cherry_log")
	cherryLeafBlock  = block("cherry_leaves")
)

type terrainSample struct {
	height       int
	biome        string
	temperature  float64
	humidity     float64
	continental  float64
	erosion      float64
	peakStrength float64
}

// Generate creates a full 1.18+ height chunk and applies features in a stable
// order so the same seed and coordinate always produce identical bytes.
func (g *OverworldGenerator) Generate(chunkX, chunkZ int32) *Chunk {
	c := &Chunk{X: chunkX, Z: chunkZ}
	var heights [SectionSize * SectionSize]int
	chunkBiome := g.BiomeAt(int(chunkX)*SectionSize+8, int(chunkZ)*SectionSize+8)

	for localX := 0; localX < SectionSize; localX++ {
		for localZ := 0; localZ < SectionSize; localZ++ {
			worldX := int(chunkX)*SectionSize + localX
			worldZ := int(chunkZ)*SectionSize + localZ
			sample := g.sampleTerrain(worldX, worldZ)
			surfaceY := sample.height
			heights[localZ*SectionSize+localX] = surfaceY
			underwater := surfaceY < SeaLevel

			for y := WorldMinY; y <= surfaceY; y++ {
				material := stoneBlock
				if y < 0 {
					material = deepslateBlock
				}
				switch {
				case y == WorldMinY:
					material = bedrockBlock
				case y < WorldMinY+5 && g.bedrockAt(worldX, y, worldZ):
					material = bedrockBlock
				case surfaceY-y <= 5:
					material = g.surfaceMaterial(sample.biome, surfaceY-y, underwater, worldX, worldZ)
				}
				setGeneratedBlock(c, localX, y, localZ, material)
			}

			for y := surfaceY + 1; y <= SeaLevel; y++ {
				material := waterBlock
				if y == SeaLevel && isFrozenBiome(sample.biome) {
					material = iceBlock
				}
				setGeneratedBlock(c, localX, y, localZ, material)
			}
		}
	}

	g.carveCaves(c, heights)
	g.addOres(c)
	g.addVillageStructures(c)
	g.addVegetation(c)
	g.addGroundCover(c, heights)
	for _, section := range c.Sections {
		if section != nil {
			section.SetUniformBiome(chunkBiome)
		}
	}
	return c
}

// SurfaceHeight returns the highest terrain block before vegetation.
func (g *OverworldGenerator) SurfaceHeight(x, z int) int { return g.sampleTerrain(x, z).height }

// BiomeAt returns the deterministic surface biome at an absolute column.
func (g *OverworldGenerator) BiomeAt(x, z int) string { return g.sampleTerrain(x, z).biome }

// NearestBiome finds a nearby sample of target within maxDistance blocks.
// Samples are spaced 32 blocks apart, matching the broad scale of GoCraft's
// climate regions while keeping an in-game lookup inexpensive.
func (g *OverworldGenerator) NearestBiome(x, z int, target string, maxDistance int) (int, int, bool) {
	if maxDistance < 0 {
		return 0, 0, false
	}
	const sampleStep = 32
	for radius := 0; radius <= maxDistance; radius += sampleStep {
		bestX, bestZ := 0, 0
		bestDistanceSquared := int64(math.MaxInt64)
		found := false
		consider := func(sampleX, sampleZ int) {
			if g.BiomeAt(sampleX, sampleZ) != target {
				return
			}
			dx := int64(sampleX - x)
			dz := int64(sampleZ - z)
			distanceSquared := dx*dx + dz*dz
			if distanceSquared < bestDistanceSquared {
				bestX, bestZ = sampleX, sampleZ
				bestDistanceSquared = distanceSquared
				found = true
			}
		}

		if radius == 0 {
			consider(x, z)
		} else {
			for offset := -radius; offset <= radius; offset += sampleStep {
				consider(x+offset, z-radius)
				consider(x+offset, z+radius)
				if offset != -radius && offset != radius {
					consider(x-radius, z+offset)
					consider(x+radius, z+offset)
				}
			}
		}
		if found {
			return bestX, bestZ, true
		}
	}
	return 0, 0, false
}

func (g *OverworldGenerator) sampleTerrain(x, z int) terrainSample {
	fx, fz := float64(x), float64(z)

	// Domain-warp the continental coordinates so continent shapes are irregular.
	warpX := g.fractal(fx, fz, 700, 3, 0.5, 0x7761727058585858) * 220
	warpZ := g.fractal(fx, fz, 700, 3, 0.5, 0x776172705a5a5a5a) * 220
	continental := g.fractal(fx+warpX, fz+warpZ, 1800, 5, 0.54, 0x636f6e74696e656e)

	erosion := g.fractal(fx, fz, 600, 4, 0.52, 0x65726f73696f6e31)
	ridgeNoise := g.fractal(fx, fz, 500, 4, 0.53, 0x7269646765733031)
	weirdness := g.fractal(fx, fz, 260, 3, 0.5, 0x77656972646e6573)
	detail := g.fractal(fx, fz, 80, 4, 0.46, 0x64657461696c3031)
	temperature := g.fractal(fx, fz, 700, 4, 0.52, 0x74656d7065726174)
	humidity := g.fractal(fx, fz, 650, 4, 0.52, 0x68756d6964697479)

	// Bimodal height: land clearly above sea level, ocean clearly below.
	// continental ≥ landThreshold → land (~60% of terrain), else → ocean.
	ridge := 1 - math.Abs(ridgeNoise)
	land := clamp01((continental + 0.15) / 0.85)
	peakStrength := clamp01((ridge-0.25)/0.75) * math.Pow(land, 1.1)

	const landThreshold = -0.1
	var base float64
	if continental >= landThreshold {
		// Land: base ranges 64–100, shaped by how far above threshold we are
		// and pushed down by high erosion (flat plains) or up by low erosion (hills).
		t := (continental - landThreshold) / (1.0 - landThreshold)
		base = 64 + t*36 + erosion*(-14) + detail*6
	} else {
		// Ocean: base ranges 30–52, deeper for more negative continental.
		t := (landThreshold - continental) / (landThreshold + 1.0)
		base = 52 - t*22 + erosion*3 + detail*2
	}
	peaks := math.Pow(peakStrength, 1.1) * (110 + math.Max(0, weirdness)*80)
	height := int(math.Round(base + peaks))

	// Keep a large landmass around spawn so players never start in the ocean.
	distance := math.Hypot(fx, fz)
	if distance < 200 {
		minimum := float64(SeaLevel+6) - distance/36
		if float64(height) < minimum {
			height = int(math.Round(minimum))
		}
	}
	if height < 28 {
		height = 28
	}
	if height > 245 {
		height = 245
	}

	biome := chooseBiome(height, temperature, humidity, continental, erosion, peakStrength)
	return terrainSample{
		height: height, biome: biome, temperature: temperature, humidity: humidity,
		continental: continental, erosion: erosion, peakStrength: peakStrength,
	}
}

func chooseBiome(height int, temperature, humidity, continental, erosion, peaks float64) string {
	// Deep ocean
	if height < SeaLevel-13 {
		if temperature < -0.35 {
			return "minecraft:deep_frozen_ocean"
		}
		return "minecraft:deep_ocean"
	}
	// Shallow ocean / mushroom island (rare warm shallow ocean with high humidity)
	if height < SeaLevel-2 {
		if temperature < -0.35 {
			return "minecraft:frozen_ocean"
		}
		if temperature > 0.1 && humidity > 0.72 && continental > 0.05 {
			return "minecraft:mushroom_fields"
		}
		return "minecraft:ocean"
	}
	// Coastline
	if height <= SeaLevel+2 {
		if temperature < -0.4 {
			return "minecraft:snowy_beach"
		}
		if peaks > 0.48 || erosion < -0.65 {
			return "minecraft:stony_shore"
		}
		return "minecraft:beach"
	}
	// High mountain peaks
	if height >= 178 {
		if temperature < 0.05 {
			return "minecraft:frozen_peaks"
		}
		return "minecraft:stony_peaks"
	}
	if height >= 145 {
		if temperature < -0.18 {
			return "minecraft:jagged_peaks"
		}
		return "minecraft:stony_peaks"
	}
	// Upper slopes
	if height >= 116 {
		if temperature < -0.2 {
			return "minecraft:snowy_slopes"
		}
		if humidity > 0.05 {
			return "minecraft:meadow"
		}
		return "minecraft:windswept_hills"
	}
	// Cherry grove: moderate slopes with mild temperature and moderate humidity
	if height >= 80 && temperature > -0.08 && temperature < 0.32 && humidity > 0.2 && humidity < 0.6 && erosion < 0.1 {
		return "minecraft:cherry_grove"
	}
	// Hot & dry
	if temperature > 0.52 && humidity < -0.28 {
		if continental > 0.45 && erosion < -0.2 {
			return "minecraft:badlands"
		}
		return "minecraft:desert"
	}
	// Savanna
	if temperature > 0.42 && humidity < 0.12 {
		return "minecraft:savanna"
	}
	// Jungle variants
	if temperature > 0.38 && humidity > 0.38 {
		if humidity > 0.65 {
			return "minecraft:bamboo_jungle"
		}
		return "minecraft:jungle"
	}
	// Swamp variants
	if humidity > 0.62 && height < 76 {
		if temperature > 0.28 {
			return "minecraft:mangrove_swamp"
		}
		return "minecraft:swamp"
	}
	// Cold / snowy
	if temperature < -0.48 {
		return "minecraft:snowy_plains"
	}
	if temperature < -0.2 {
		return "minecraft:taiga"
	}
	// Temperate forests
	if humidity > 0.42 {
		if temperature < 0.08 {
			return "minecraft:dark_forest"
		}
		if humidity > 0.68 {
			return "minecraft:flower_forest"
		}
		return "minecraft:forest"
	}
	if humidity > 0.12 && temperature < 0.25 {
		if humidity > 0.34 {
			return "minecraft:old_growth_birch_forest"
		}
		return "minecraft:birch_forest"
	}
	return "minecraft:plains"
}

func (g *OverworldGenerator) surfaceMaterial(biome string, depth int, underwater bool, x, z int) Block {
	if underwater {
		if depth <= 3 {
			if g.columnHash(x, z, 0x67726176656c)%5 == 0 {
				return gravelBlock
			}
			return sandBlock
		}
		return sandstoneBlock
	}
	switch biome {
	case "minecraft:desert", "minecraft:beach", "minecraft:snowy_beach":
		if depth <= 3 {
			return sandBlock
		}
		return sandstoneBlock
	case "minecraft:badlands":
		if depth == 0 {
			return redSandBlock
		}
		return terracottaBlock
	case "minecraft:swamp":
		if depth == 0 {
			return grassBlock
		}
		return mudBlock
	case "minecraft:taiga":
		if depth == 0 {
			return grassBlock
		}
		return dirtBlock
	case "minecraft:dark_forest":
		if depth == 0 {
			return grassBlock
		}
		return dirtBlock
	case "minecraft:stony_shore", "minecraft:windswept_hills", "minecraft:stony_peaks", "minecraft:jagged_peaks":
		return stoneBlock
	case "minecraft:snowy_plains", "minecraft:snowy_slopes", "minecraft:frozen_peaks":
		if depth == 0 {
			return snowBlock
		}
		if depth <= 3 {
			return dirtBlock
		}
		return stoneBlock
	default:
		if depth == 0 {
			return grassBlock
		}
		return dirtBlock
	}
}

func isFrozenBiome(biome string) bool {
	switch biome {
	case "minecraft:frozen_ocean", "minecraft:deep_frozen_ocean", "minecraft:snowy_beach":
		return true
	default:
		return false
	}
}

func setGeneratedBlock(c *Chunk, x, y, z int, material Block) {
	if y < WorldMinY || y > WorldMaxY {
		return
	}
	sectionIndex := (y - WorldMinY) / SectionSize
	localY := (y - WorldMinY) % SectionSize
	if c.Sections[sectionIndex] == nil {
		c.Sections[sectionIndex] = NewSection()
	}
	c.Sections[sectionIndex].Set(x, localY, z, material)
}

func generatedBlock(c *Chunk, x, y, z int) Block {
	if y < WorldMinY || y > WorldMaxY {
		return Air
	}
	sectionIndex := (y - WorldMinY) / SectionSize
	localY := (y - WorldMinY) % SectionSize
	if c.Sections[sectionIndex] == nil {
		return Air
	}
	return c.Sections[sectionIndex].At(x, localY, z)
}

func (g *OverworldGenerator) bedrockAt(x, y, z int) bool {
	layer := y - WorldMinY
	return int(g.columnHash(x, z, uint64(y))%5) >= layer
}

func (g *OverworldGenerator) fractal(x, z, scale float64, octaves int, persistence float64, salt uint64) float64 {
	amplitude, totalAmplitude, value := 1.0, 0.0, 0.0
	for octave := 0; octave < octaves; octave++ {
		value += g.gradNoise(x/scale, z/scale, salt+uint64(octave)*0x9e3779b97f4a7c15) * amplitude
		totalAmplitude += amplitude
		amplitude *= persistence
		scale *= 0.5
	}
	return value / totalAmplitude
}

// gradNoise is 2-D gradient (Perlin-style) noise. Unlike value noise, it uses
// random gradient vectors at each lattice corner so the output is smooth and
// organic rather than blobby/circular.
func (g *OverworldGenerator) gradNoise(x, z float64, salt uint64) float64 {
	x0, z0 := int64(math.Floor(x)), int64(math.Floor(z))
	dx, dz := x-float64(x0), z-float64(z0)
	tx, tz := smoothstep(dx), smoothstep(dz)

	g00 := g.grad2D(x0, z0, salt, dx, dz)
	g10 := g.grad2D(x0+1, z0, salt, dx-1, dz)
	g01 := g.grad2D(x0, z0+1, salt, dx, dz-1)
	g11 := g.grad2D(x0+1, z0+1, salt, dx-1, dz-1)

	// Gradient noise has a narrower output range than value noise (~0.7 max).
	// Scale up to restore the [-1,1] range expected by terrain callers.
	return lerp(lerp(g00, g10, tx), lerp(g01, g11, tx), tz) * 1.41
}

// grad2D returns the dot product of a pseudorandom unit gradient with (dx,dz).
// The 8 cardinal/diagonal directions cover the full unit circle uniformly.
func (g *OverworldGenerator) grad2D(x, z int64, salt uint64, dx, dz float64) float64 {
	h := mix64(uint64(g.seed) ^ uint64(x)*0x9e3779b185ebca87 ^ uint64(z)*0xc2b2ae3d27d4eb4f ^ salt)
	switch h & 7 {
	case 0:
		return dx + dz
	case 1:
		return -dx + dz
	case 2:
		return dx - dz
	case 3:
		return -dx - dz
	case 4:
		return dx*1.4 + dz*0.7
	case 5:
		return -dx*1.4 + dz*0.7
	case 6:
		return dx*0.7 + dz*1.4
	default:
		return dx*0.7 - dz*1.4
	}
}

func (g *OverworldGenerator) columnHash(x, z int, salt uint64) uint64 {
	return mix64(uint64(g.seed) ^ uint64(int64(x))*0x517cc1b727220a95 ^ uint64(int64(z))*0x6eed0e9da4d94a4f ^ salt)
}

func (g *OverworldGenerator) featureHash(x, z int32, salt uint64) uint64 {
	return mix64(uint64(g.seed) ^ uint64(int64(x))*0x8da6b343 ^ uint64(int64(z))*0xd8163841 ^ salt)
}

func mix64(v uint64) uint64 {
	v += 0x9e3779b97f4a7c15
	v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
	v = (v ^ (v >> 27)) * 0x94d049bb133111eb
	return v ^ (v >> 31)
}

func nextRandom(state *uint64) float64 {
	*state = mix64(*state)
	return float64(*state>>11) / float64(uint64(1)<<53)
}

func floorDiv(value, divisor int) int {
	quotient := value / divisor
	if value%divisor < 0 {
		quotient--
	}
	return quotient
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func smoothstep(v float64) float64 { return v * v * (3 - 2*v) }
func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// FlatGenerator remains available for adapter tests.
type FlatGenerator struct{}

func (g *FlatGenerator) Generate(x, z int32) *Chunk {
	c := &Chunk{X: x, Z: z}
	const groundSectionIdx = 7
	const groundLocalY = 15
	sec := NewSection()
	sec.SetUniformBiome("minecraft:plains")
	for bx := 0; bx < SectionSize; bx++ {
		for bz := 0; bz < SectionSize; bz++ {
			sec.Set(bx, groundLocalY, bz, stoneBlock)
		}
	}
	c.Sections[groundSectionIdx] = sec
	return c
}
