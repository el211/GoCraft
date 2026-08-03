package bedrock

import (
	coreworld "GoCraft/core/world"
	"bytes"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
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

func TestEncodedBedsAreInlineNetworkBlockActors(t *testing.T) {
	chunk := &coreworld.Chunk{X: -2, Z: 3}
	section := coreworld.NewSection()
	section.Set(1, 0, 2, coreworld.Block{Namespace: "minecraft", Name: "red_bed"})
	chunk.Sections[8] = section
	payload, err := encodeBedBlockActors(chunk, 4)
	if err != nil {
		t.Fatal(err)
	}
	var actor map[string]any
	if err := nbt.NewDecoderWithEncoding(bytes.NewReader(payload), nbt.NetworkLittleEndian).Decode(&actor); err != nil {
		t.Fatalf("decode inline block actor: %v", err)
	}
	if actor["id"] != "Bed" || actor["x"] != int32(-31) || actor["y"] != int32(64) || actor["z"] != int32(50) || actor["color"] != uint8(14) {
		t.Fatalf("unexpected inline bed actor: %#v", actor)
	}
}
