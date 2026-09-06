package world

// vine_growth.go handles the random-tick spreading of minecraft:vine.
//
// Vanilla rules summary (Java Edition 1.21.4):
//   - 25% gate: skip if cropRandom != 0.
//   - Pick a random horizontal direction (one of 4).
//   - If DOWN: spread downward if air below, copying the current face set.
//   - Horizontal spread in direction D:
//       • If the vine has a face in D: the target block in D direction must be
//         air; the new vine there inherits all faces from the source (minus D
//         itself, plus it needs a solid block on the D-side behind it — but
//         since the source already has D=true and is supported, just copy).
//       • Otherwise (vine doesn't have face D): use a face perpendicular to D.
//         For each perpendicular face that is true on the source vine, check
//         if the perpendicular neighbour of the target position is solid; if
//         so, place a vine there with that perpendicular face.

var vineFaceDirs = [4]struct {
	prop string
	dx   int
	dz   int
}{
	{prop: "north", dx: 0, dz: -1},
	{prop: "south", dx: 0, dz: 1},
	{prop: "west", dx: -1, dz: 0},
	{prop: "east", dx: 1, dz: 0},
}

// IsVine reports whether the block is minecraft:vine.
func IsVine(name string) bool { return name == "minecraft:vine" }

// tickVineAt attempts to spread a vine block and returns any block changes.
func (w *World) tickVineAt(x, y, z int, vine Block, tick int64) []BlockChange {
	seed := uint64(tick / 20)
	if cropRandom(seed, x, y, z, cropGateSalt, 4) != 0 {
		return nil
	}

	// 50% chance: try to spread downward.
	if cropRandom(seed, x, y, z, cropDirectionSalt, 2) == 0 {
		if y > WorldMinY {
			below, loaded := w.blockIfLoaded(x, y-1, z)
			if loaded && below.IsAir() {
				placed := copyWorldBlock(vine)
				w.SetBlock(x, y-1, z, placed)
				return []BlockChange{{X: x, Y: y - 1, Z: z, Block: placed}}
			}
		}
	}

	// Pick a random horizontal direction index.
	dirIdx := cropRandom(seed, x, y, z, cropGrowthSalt, 4)
	dir := vineFaceDirs[dirIdx]
	tx, tz := x+dir.dx, z+dir.dz

	target, loaded := w.blockIfLoaded(tx, y, tz)
	if !loaded || !target.IsAir() {
		return nil
	}

	// Build the face set for the new vine at (tx, y, tz).
	// The new vine needs at least one supported face.
	newProps := map[string]string{
		"north": "false", "south": "false",
		"east": "false", "west": "false", "up": "false",
	}
	placed := false

	// If the source vine has the face in direction dir, the new vine in dir
	// direction can inherit that face (its supporting block is behind it in dir).
	if vine.Properties[dir.prop] == "true" {
		// Source vine's face D is already supported — new vine at tx,tz can
		// inherit all faces from source since its D-side is the source itself
		// (which is solid or at least a vine). Copy all active faces.
		for _, fd := range vineFaceDirs {
			if vine.Properties[fd.prop] == "true" {
				// Check support: solid block in fd direction of new position.
				sx, sz := tx+fd.dx, tz+fd.dz
				support, sLoaded := w.blockIfLoaded(sx, y, sz)
				if sLoaded && vineIsSolidSupport(support.ResourceLocation()) {
					newProps[fd.prop] = "true"
					placed = true
				}
			}
		}
	} else {
		// Source doesn't have dir face. Use perpendicular faces.
		for _, fd := range vineFaceDirs {
			if fd.prop == dir.prop {
				continue
			}
			if vine.Properties[fd.prop] != "true" {
				continue
			}
			// Check if the perpendicular support block exists behind the new pos.
			sx, sz := tx+fd.dx, tz+fd.dz
			support, sLoaded := w.blockIfLoaded(sx, y, sz)
			if sLoaded && vineIsSolidSupport(support.ResourceLocation()) {
				newProps[fd.prop] = "true"
				placed = true
			}
		}
	}

	if !placed {
		return nil
	}
	newVine := Block{Namespace: "minecraft", Name: "vine", Properties: newProps}
	w.SetBlock(tx, y, tz, newVine)
	return []BlockChange{{X: tx, Y: y, Z: tz, Block: newVine}}
}

// vineIsSolidSupport returns true for blocks that can support a vine face.
func vineIsSolidSupport(name string) bool {
	if name == "" {
		return false
	}
	switch name {
	case "minecraft:air", "minecraft:cave_air", "minecraft:void_air",
		"minecraft:water", "minecraft:lava",
		"minecraft:vine", "minecraft:short_grass", "minecraft:grass",
		"minecraft:tall_grass", "minecraft:large_fern", "minecraft:fern":
		return false
	}
	return true
}
