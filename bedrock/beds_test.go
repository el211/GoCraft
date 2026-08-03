package bedrock

import (
	coreworld "GoCraft/core/world"
	"testing"
)

func TestBedBlockActors(t *testing.T) {
	chunk := &coreworld.Chunk{X: -2, Z: 3}
	section := coreworld.NewSection()
	bed := coreworld.Block{Namespace: "minecraft", Name: "red_bed"}
	section.Set(1, 0, 2, bed)
	section.Set(1, 0, 3, bed)
	chunk.Sections[8] = section
	actors := bedBlockActors(chunk, 4)
	if len(actors) != 2 || actors[0].Colour != 14 || actors[0].X != -31 || actors[0].Y != 64 {
		t.Fatalf("unexpected bed actors: %+v", actors)
	}
}
