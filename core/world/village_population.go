package world

import (
	"encoding/binary"
	"log/slog"
	"math"
	"strings"

	"GoCraft/core/entity"
	"GoCraft/core/spatial"
)

type discoveredVillageResident struct {
	home, center, spawn, bed, workstation spatial.BlockPos
	profession                            entity.VillagerProfession
	variant                               entity.VillagerVariant
}

func (w *World) discoverLoadedVillagePopulation(chunk *Chunk) {
	if !w.villagersEnabled || chunk == nil {
		return
	}
	beds, doors, jobs := scanVillagePOIs(chunk)
	if len(beds) == 0 && len(doors) == 0 && len(jobs) == 0 {
		return
	}

	live := w.Entities.Snapshot()
	occupiedBeds := make(map[spatial.BlockPos]struct{})
	for _, current := range live {
		if current.Type == entity.TypeVillager && current.HasVillageHome {
			occupiedBeds[current.VillageBed] = struct{}{}
		}
	}

	w.villagePOIMu.Lock()
	for _, bed := range beds {
		w.villageBeds[bed] = struct{}{}
	}
	for pos, block := range doors {
		w.villageDoors[pos] = block
	}
	for pos, profession := range jobs {
		w.villageJobs[pos] = profession
	}

	var residents []discoveredVillageResident
	plannedByCluster := make(map[[2]int]int)

	for bed := range w.villageBeds {
		if _, occupied := occupiedBeds[bed]; occupied {
			continue
		}
		if _, spawned := w.spawnedVillageHomes[bed]; spawned {
			continue
		}
		nearBeds := villagePositionsNearSet(w.villageBeds, bed, 48)
		if len(nearBeds) < 1 {
			continue
		}
		nearDoors := villageDoorPositionsNear(w.villageDoors, bed, 48)
		nearJobs := villageJobsNear(w.villageJobs, bed, 48)
		center := villagePositionCenter(nearBeds)
		home := bed
		if door, ok := nearestVillagePosition(bed, nearDoors); ok {
			home = door
		}
		workstation, profession := nearestVillageJob(bed, nearJobs)
		if profession == "" {
			profession = fallbackVillageProfession(bed)
			workstation = home
		}
		spawn := spatial.BlockPos{X: bed.X, Y: bed.Y + 1, Z: bed.Z}
		w.spawnedVillageHomes[bed] = struct{}{}
		residents = append(residents, discoveredVillageResident{
			home: home, center: center, spawn: spawn, bed: bed,
			workstation: workstation, profession: profession,
			variant: w.discoveredVillageVariant(center),
		})
		plannedByCluster[villageClusterKey(center)]++
	}

	// Legacy GoCraft villages may predate beds and workstations. Three nearby
	// wooden house doors are treated as a legacy village and receive residents.
	for door := range w.villageDoors {
		if _, spawned := w.spawnedVillageHomes[door]; spawned {
			continue
		}
		if len(villagePositionsNearSet(w.villageBeds, door, 48)) > 0 {
			continue
		}
		nearDoors := villageDoorPositionsNear(w.villageDoors, door, 48)
		if len(nearDoors) < 3 {
			continue
		}
		center := villagePositionCenter(nearDoors)
		cluster := villageClusterKey(center)
		desired := len(nearDoors)
		if desired > 5 {
			desired = 5
		}
		existing := liveVillagersNear(live, center, 48)
		if existing+plannedByCluster[cluster] >= desired {
			continue
		}
		nearJobs := villageJobsNear(w.villageJobs, door, 48)
		workstation, profession := nearestVillageJob(door, nearJobs)
		if profession == "" {
			profession = fallbackVillageProfession(door)
			workstation = door
		}
		spawn := door
		spawn.Z++
		w.spawnedVillageHomes[door] = struct{}{}
		residents = append(residents, discoveredVillageResident{
			home: door, center: center, spawn: spawn, bed: door,
			workstation: workstation, profession: profession,
			variant: w.discoveredVillageVariant(center),
		})
		plannedByCluster[cluster]++
	}

	guards := make([]spatial.BlockPos, 0, len(plannedByCluster))
	for cluster, planned := range plannedByCluster {
		if planned == 0 {
			continue
		}
		center := spatial.BlockPos{X: int32(cluster[0]*64 + 32), Z: int32(cluster[1]*64 + 32)}
		for _, resident := range residents {
			if villageClusterKey(resident.center) == cluster {
				center = resident.center
				break
			}
		}
		if _, spawned := w.spawnedVillageGuards[cluster]; spawned || liveGolemNear(live, center, 64) {
			continue
		}
		if liveVillagersNear(live, center, 64)+planned < 3 {
			continue
		}
		w.spawnedVillageGuards[cluster] = struct{}{}
		guards = append(guards, center)
	}
	w.villagePOIMu.Unlock()

	for i, resident := range residents {
		id := w.nextVillagerID.Add(1) + 10_000_000
		villager := entity.New(id, villageEntityUUID(resident.bed, 0x7265736964656e74), entity.TypeVillager,
			float64(resident.spawn.X)+0.5, float64(resident.spawn.Y), float64(resident.spawn.Z)+0.5)
		villager.VillagerVariant = resident.variant
		villager.VillagerProfession = resident.profession
		villager.VillagerLevel = 1
		villager.HasVillageHome = true
		villager.VillageHome = resident.home
		villager.VillageCenter = resident.center
		villager.VillageBed = resident.bed
		villager.VillageWorkstation = resident.workstation
		villager.OnGround = true
		// Spawn 1 baby for every 4 adults — gives the village a lived-in feel.
		if i%4 == 3 {
			villager.IsBaby = true
		}
		w.addGeneratedVillageEntity(villager)
		slog.Info("village resident discovered and spawned",
			"id", id, "x", resident.spawn.X, "y", resident.spawn.Y, "z", resident.spawn.Z,
			"profession", resident.profession, "centerX", resident.center.X, "centerZ", resident.center.Z,
			"baby", villager.IsBaby)
	}
	for _, center := range guards {
		id := w.nextVillagerID.Add(1) + 10_000_000
		spawn := center
		if spawn.Y == 0 {
			spawn.Y = int32(w.SurfaceY(int(spawn.X), int(spawn.Z)) + 1)
		}
		golem := entity.New(id, villageEntityUUID(center, 0x69726f6e676f6c65), entity.TypeIronGolem,
			float64(spawn.X)+0.5, float64(spawn.Y), float64(spawn.Z)+0.5)
		golem.HasVillageHome = true
		golem.VillageHome = center
		golem.VillageCenter = center
		golem.OnGround = true
		w.addGeneratedVillageEntity(golem)
		slog.Info("village guard discovered and spawned", "id", id, "x", spawn.X, "y", spawn.Y, "z", spawn.Z)
	}
}

func (w *World) addGeneratedVillageEntity(value *entity.Entity) {
	w.Entities.Add(value)
	w.spawnedMu.Lock()
	w.spawnedEntities = append(w.spawnedEntities, value)
	w.spawnedMu.Unlock()
}

func scanVillagePOIs(chunk *Chunk) ([]spatial.BlockPos, map[spatial.BlockPos]Block, map[spatial.BlockPos]entity.VillagerProfession) {
	var beds []spatial.BlockPos
	doors := make(map[spatial.BlockPos]Block)
	jobs := make(map[spatial.BlockPos]entity.VillagerProfession)
	for sectionIndex, section := range chunk.Sections {
		if section == nil || section.NonAir == 0 {
			continue
		}
		palette := section.BlockPalette()
		relevant := make([]bool, len(palette))
		for i, block := range palette {
			name := block.ResourceLocation()
			_, isJob := villageProfessionForWorkstation(name)
			relevant[i] = isVillageBedBlock(block) || isVillageDoorBlock(block) || isJob
		}
		data := section.BlockData()
		for index, paletteIndex := range data {
			if int(paletteIndex) >= len(relevant) || !relevant[paletteIndex] {
				continue
			}
			block := palette[paletteIndex]
			localX := index % SectionSize
			localZ := (index / SectionSize) % SectionSize
			localY := index / (SectionSize * SectionSize)
			pos := spatial.BlockPos{
				X: int32(int(chunk.X)*SectionSize + localX),
				Y: int32(SectionMinY(sectionIndex) + localY),
				Z: int32(int(chunk.Z)*SectionSize + localZ),
			}
			switch {
			case isVillageBedBlock(block):
				beds = append(beds, pos)
			case isVillageDoorBlock(block):
				doors[pos] = block
			default:
				if profession, ok := villageProfessionForWorkstation(block.ResourceLocation()); ok {
					jobs[pos] = profession
				}
			}
		}
	}
	return beds, doors, jobs
}

func isVillageBedBlock(block Block) bool {
	if !strings.HasSuffix(block.ResourceLocation(), "_bed") {
		return false
	}
	part := block.Properties["part"]
	return part == "" || part == "head"
}

func isVillageDoorBlock(block Block) bool {
	name := block.ResourceLocation()
	if !strings.HasSuffix(name, "_door") || name == "minecraft:iron_door" {
		return false
	}
	half := block.Properties["half"]
	return half == "" || half == "lower"
}

func villageProfessionForWorkstation(name string) (entity.VillagerProfession, bool) {
	switch name {
	case "minecraft:blast_furnace":
		return entity.VillagerProfessionArmorer, true
	case "minecraft:smoker":
		return entity.VillagerProfessionButcher, true
	case "minecraft:cartography_table":
		return entity.VillagerProfessionCartographer, true
	case "minecraft:brewing_stand":
		return entity.VillagerProfessionCleric, true
	case "minecraft:composter":
		return entity.VillagerProfessionFarmer, true
	case "minecraft:barrel":
		return entity.VillagerProfessionFisherman, true
	case "minecraft:fletching_table":
		return entity.VillagerProfessionFletcher, true
	case "minecraft:cauldron":
		return entity.VillagerProfessionLeatherworker, true
	case "minecraft:lectern":
		return entity.VillagerProfessionLibrarian, true
	case "minecraft:stonecutter":
		return entity.VillagerProfessionMason, true
	case "minecraft:loom":
		return entity.VillagerProfessionShepherd, true
	case "minecraft:smithing_table":
		return entity.VillagerProfessionToolsmith, true
	case "minecraft:grindstone":
		return entity.VillagerProfessionWeaponsmith, true
	default:
		return "", false
	}
}

func villagePositionsNearSet(values map[spatial.BlockPos]struct{}, origin spatial.BlockPos, radius int32) []spatial.BlockPos {
	result := make([]spatial.BlockPos, 0, 8)
	radiusSquared := int64(radius) * int64(radius)
	for pos := range values {
		dx, dz := int64(pos.X-origin.X), int64(pos.Z-origin.Z)
		if dx*dx+dz*dz <= radiusSquared {
			result = append(result, pos)
		}
	}
	return result
}

func villageDoorPositionsNear(values map[spatial.BlockPos]Block, origin spatial.BlockPos, radius int32) []spatial.BlockPos {
	result := make([]spatial.BlockPos, 0, 8)
	radiusSquared := int64(radius) * int64(radius)
	for pos := range values {
		dx, dz := int64(pos.X-origin.X), int64(pos.Z-origin.Z)
		if dx*dx+dz*dz <= radiusSquared {
			result = append(result, pos)
		}
	}
	return result
}

func villageJobsNear(values map[spatial.BlockPos]entity.VillagerProfession, origin spatial.BlockPos, radius int32) map[spatial.BlockPos]entity.VillagerProfession {
	result := make(map[spatial.BlockPos]entity.VillagerProfession)
	radiusSquared := int64(radius) * int64(radius)
	for pos, profession := range values {
		dx, dz := int64(pos.X-origin.X), int64(pos.Z-origin.Z)
		if dx*dx+dz*dz <= radiusSquared {
			result[pos] = profession
		}
	}
	return result
}

func villagePositionCenter(values []spatial.BlockPos) spatial.BlockPos {
	if len(values) == 0 {
		return spatial.BlockPos{}
	}
	var x, y, z int64
	for _, pos := range values {
		x += int64(pos.X)
		y += int64(pos.Y)
		z += int64(pos.Z)
	}
	n := int64(len(values))
	return spatial.BlockPos{X: int32(x / n), Y: int32(y/n + 1), Z: int32(z / n)}
}

func nearestVillagePosition(origin spatial.BlockPos, values []spatial.BlockPos) (spatial.BlockPos, bool) {
	var best spatial.BlockPos
	bestDistance := int64(math.MaxInt64)
	found := false
	for _, pos := range values {
		dx, dy, dz := int64(pos.X-origin.X), int64(pos.Y-origin.Y), int64(pos.Z-origin.Z)
		distance := dx*dx + dy*dy + dz*dz
		if distance < bestDistance {
			best, bestDistance, found = pos, distance, true
		}
	}
	return best, found
}

func nearestVillageJob(origin spatial.BlockPos, values map[spatial.BlockPos]entity.VillagerProfession) (spatial.BlockPos, entity.VillagerProfession) {
	var best spatial.BlockPos
	var profession entity.VillagerProfession
	bestDistance := int64(math.MaxInt64)
	for pos, candidate := range values {
		dx, dy, dz := int64(pos.X-origin.X), int64(pos.Y-origin.Y), int64(pos.Z-origin.Z)
		distance := dx*dx + dy*dy + dz*dz
		if distance < bestDistance {
			best, profession, bestDistance = pos, candidate, distance
		}
	}
	return best, profession
}

func fallbackVillageProfession(pos spatial.BlockPos) entity.VillagerProfession {
	professions := [...]entity.VillagerProfession{
		entity.VillagerProfessionFarmer,
		entity.VillagerProfessionLibrarian,
		entity.VillagerProfessionFletcher,
		entity.VillagerProfessionToolsmith,
		entity.VillagerProfessionArmorer,
		entity.VillagerProfessionShepherd,
	}
	hash := int64(pos.X)*73428767 ^ int64(pos.Z)*912931
	if hash < 0 {
		hash = -hash
	}
	return professions[hash%int64(len(professions))]
}

func (w *World) discoveredVillageVariant(center spatial.BlockPos) entity.VillagerVariant {
	generator, ok := w.generator.(*OverworldGenerator)
	if !ok {
		return entity.VillagerVariantPlains
	}
	return villagerVariantForBiome(generator.BiomeAt(int(center.X), int(center.Z)))
}

func villageClusterKey(center spatial.BlockPos) [2]int {
	return [2]int{floorDiv(int(center.X), 64), floorDiv(int(center.Z), 64)}
}

// VillageBedsNear returns the real bed head positions discovered in loaded
// chunks. Breeding uses this list as a hard population capacity instead of
// assuming that an adjacent coordinate contains another bed.
func (w *World) VillageBedsNear(center spatial.BlockPos, radius int32) []spatial.BlockPos {
	w.villagePOIMu.Lock()
	defer w.villagePOIMu.Unlock()
	beds := villagePositionsNearSet(w.villageBeds, center, radius)
	return beds
}

func liveVillagersNear(values []*entity.Entity, center spatial.BlockPos, radius int32) int {
	count := 0
	radiusSquared := float64(radius * radius)
	for _, current := range values {
		if current.Type != entity.TypeVillager || current.Dead {
			continue
		}
		dx := current.Position.X - float64(center.X)
		dz := current.Position.Z - float64(center.Z)
		if dx*dx+dz*dz <= radiusSquared {
			count++
		}
	}
	return count
}

func liveGolemNear(values []*entity.Entity, center spatial.BlockPos, radius int32) bool {
	radiusSquared := float64(radius * radius)
	for _, current := range values {
		if current.Type != entity.TypeIronGolem || current.Dead {
			continue
		}
		dx := current.Position.X - float64(center.X)
		dz := current.Position.Z - float64(center.Z)
		if dx*dx+dz*dz <= radiusSquared {
			return true
		}
	}
	return false
}

func villageEntityUUID(pos spatial.BlockPos, salt uint64) [16]byte {
	var value [16]byte
	binary.BigEndian.PutUint32(value[0:4], uint32(pos.X))
	binary.BigEndian.PutUint32(value[4:8], uint32(pos.Y))
	binary.BigEndian.PutUint32(value[8:12], uint32(pos.Z))
	binary.BigEndian.PutUint32(value[12:16], uint32(salt^(salt>>32)))
	value[6] = (value[6] & 0x0f) | 0x30
	value[8] = (value[8] & 0x3f) | 0x80
	return value
}
