package handler

import (
	"bytes"
	"encoding/binary"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/java/protocol"
)

func TestDroppedItemMetadataCarriesStack(t *testing.T) {
	e := corentity.New(77, [16]byte{}, corentity.TypeItem, 1, 2, 3)
	e.ItemID, e.ItemCount, e.ItemDamage = "minecraft:iron_sword", 1, 7
	pkt := buildMobMetadata(e)
	if pkt == nil || pkt.ID != packetIDSetEntityData {
		t.Fatal("dropped item metadata packet is missing")
	}
	r := bytes.NewReader(pkt.Data)
	entityID, _ := protocol.ReadVarInt(r)
	index, _ := protocol.ReadByte(r)
	serializer, _ := protocol.ReadVarInt(r)
	stack, err := readPlainSlot(r)
	if err != nil {
		t.Fatalf("decode dropped stack: %v", err)
	}
	terminator, _ := protocol.ReadByte(r)
	if entityID != 77 || index != 8 || serializer != 7 || stack.ItemID != "minecraft:iron_sword" || stack.Damage != 7 || terminator != 0xff {
		t.Fatalf("metadata entity=%d index=%d serializer=%d stack=%+v terminator=%x", entityID, index, serializer, stack, terminator)
	}
}

func TestEntityDeathEventUsesProtocol769Layout(t *testing.T) {
	pkt := buildEntityEvent(1234, 3)
	if pkt.ID != packetIDEntityEvent || len(pkt.Data) != 5 {
		t.Fatalf("death event id=%d payload=%x", pkt.ID, pkt.Data)
	}
	if got := int32(binary.BigEndian.Uint32(pkt.Data[:4])); got != 1234 || pkt.Data[4] != 3 {
		t.Fatalf("death event payload=%x", pkt.Data)
	}
}
