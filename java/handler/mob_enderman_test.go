package handler

import (
	"bytes"
	"testing"

	corentity "GoCraft/core/entity"
	coreworld "GoCraft/core/world"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

func TestEndermanMetadataCarriesBlockState(t *testing.T) {
	e := corentity.New(42, [16]byte{}, corentity.TypeEnderman, 1, 2, 3)
	e.EndermanCarriedBlock = "minecraft:grass_block"
	pkt := buildMobMetadata(e)
	if pkt == nil || pkt.ID != packetIDSetEntityData {
		t.Fatal("enderman carry-state metadata packet is missing")
	}
	r := bytes.NewReader(pkt.Data)
	entityID, _ := protocol.ReadVarInt(r)
	index, _ := protocol.ReadByte(r)
	serializer, _ := protocol.ReadVarInt(r)
	stateID, _ := protocol.ReadVarInt(r)
	terminator, _ := protocol.ReadByte(r)

	want := javaworld.StateID(coreworld.Block{Namespace: "minecraft", Name: "grass_block"})
	// The client reads 0 as "carrying nothing", so an unresolved block would
	// silently render an empty-handed enderman rather than fail.
	if want == 0 {
		t.Fatal("grass_block resolved to state ID 0, which the client treats as empty")
	}
	if entityID != 42 || index != endermanMetadataCarriedBlockIndex ||
		serializer != metadataTypeOptionalBlockState || stateID != want || terminator != 0xff {
		t.Fatalf("metadata entity=%d index=%d serializer=%d state=%d want=%d terminator=%x",
			entityID, index, serializer, stateID, want, terminator)
	}
}

func TestEndermanWithoutCarriedBlockOmitsCarryState(t *testing.T) {
	e := corentity.New(42, [16]byte{}, corentity.TypeEnderman, 1, 2, 3)
	if pkt := buildMobMetadata(e); pkt != nil {
		t.Fatalf("empty-handed enderman produced metadata %x", pkt.Data)
	}
}
