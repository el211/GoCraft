package world

import "testing"

func TestOverworldGeneratorHasBiomesMountainsAndCliffs(t *testing.T) {
	generator := NewOverworldGenerator(123456789)
	minimum, maximum, steepest := WorldMaxY, WorldMinY, 0
	biomes := make(map[string]struct{})
	for x := -2048; x <= 2048; x += 32 {
		for z := -2048; z <= 2048; z += 32 {
			height := generator.SurfaceHeight(x, z)
			minimum = minInt(minimum, height)
			maximum = maxInt(maximum, height)
			biomes[generator.BiomeAt(x, z)] = struct{}{}
			steepest = maxInt(steepest, absInt(height-generator.SurfaceHeight(x+8, z)))
			steepest = maxInt(steepest, absInt(height-generator.SurfaceHeight(x, z+8)))
		}
	}
	if minimum > SeaLevel-8 {
		t.Fatalf("minimum height=%d, want deep ocean", minimum)
	}
	if maximum < 155 {
		t.Fatalf("maximum height=%d, want mountain peaks", maximum)
	}
	if steepest < 12 {
		t.Fatalf("steepest 8-block rise=%d, want cliffs", steepest)
	}
	if len(biomes) < 10 {
		t.Fatalf("sampled %d biomes (%v), want at least 10", len(biomes), biomes)
	}
}

func TestOverworldGeneratorHasCavesAndMajorOres(t *testing.T) {
	generator := NewOverworldGenerator(8675309)
	foundCave := false
	foundOres := make(map[string]bool)
	for chunkX := int32(-4); chunkX <= 4; chunkX++ {
		for chunkZ := int32(-4); chunkZ <= 4; chunkZ++ {
			chunk := generator.Generate(chunkX, chunkZ)
			for _, section := range chunk.Sections {
				if section == nil {
					continue
				}
				for _, material := range section.BlockPalette() {
					name := material.ResourceLocation()
					switch name {
					case "minecraft:coal_ore", "minecraft:deepslate_coal_ore":
						foundOres["coal"] = true
					case "minecraft:iron_ore", "minecraft:deepslate_iron_ore":
						foundOres["iron"] = true
					case "minecraft:copper_ore", "minecraft:deepslate_copper_ore":
						foundOres["copper"] = true
					case "minecraft:gold_ore", "minecraft:deepslate_gold_ore":
						foundOres["gold"] = true
					case "minecraft:redstone_ore", "minecraft:deepslate_redstone_ore":
						foundOres["redstone"] = true
					case "minecraft:lapis_ore", "minecraft:deepslate_lapis_ore":
						foundOres["lapis"] = true
					case "minecraft:diamond_ore", "minecraft:deepslate_diamond_ore":
						foundOres["diamond"] = true
					}
				}
			}
			if !foundCave {
				for x := 0; x < 16 && !foundCave; x++ {
					for z := 0; z < 16 && !foundCave; z++ {
						for y := WorldMinY + 6; y < 45; y++ {
							if chunkBlock(chunk, x, y, z).IsAir() && !chunkBlock(chunk, x, y-1, z).IsAir() && !chunkBlock(chunk, x, y+1, z).IsAir() {
								foundCave = true
								break
							}
						}
					}
				}
			}
		}
	}
	if !foundCave {
		t.Fatal("sampled chunks contain no enclosed cave air")
	}
	for _, ore := range []string{"coal", "iron", "copper", "gold", "redstone", "lapis", "diamond"} {
		if !foundOres[ore] {
			t.Errorf("sampled chunks contain no %s ore", ore)
		}
	}
}

func TestWarmModeratelyDryClimateSelectsSavanna(t *testing.T) {
	if got := chooseBiome(72, 0.35, 0.1, 0.2, 0, 0); got != "minecraft:savanna" {
		t.Fatalf("warm dry biome = %q, want savanna", got)
	}
}
