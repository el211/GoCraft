package handler

import (
	"bytes"
	"testing"

	"GoCraft/core/player"
	"GoCraft/java/protocol"
)

func TestExternalEquipmentUsesProtocol769ContinuationSlots(t *testing.T) {
	p := player.New([16]byte{}, "bedrock", player.ClientEditionBedrock)
	p.EntityID = 51
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:iron_sword", Count: 1}
	p.Inventory[5] = player.ItemStack{ItemID: "minecraft:iron_helmet", Count: 1}
	b := protocol.NewBuilder(packetIDSetEquipment).VarInt(p.EntityID)
	equipment := []struct {
		slot byte
		item player.ItemStack
	}{{0, p.HeldItem()}, {1, p.Inventory[45]}, {2, p.Inventory[8]}, {3, p.Inventory[7]}, {4, p.Inventory[6]}, {5, p.Inventory[5]}}
	for index, entry := range equipment {
		slot := entry.slot
		if index != len(equipment)-1 {
			slot |= 0x80
		}
		b.Byte(slot)
		encodeSlot(b, entry.item)
	}
	pkt := b.Build()
	r := bytes.NewReader(pkt.Data)
	if id, _ := protocol.ReadVarInt(r); id != p.EntityID {
		t.Fatalf("entity id = %d", id)
	}
	first, _ := protocol.ReadByte(r)
	if first != 0x80 {
		t.Fatalf("first equipment slot byte = %#x, want continuation main-hand", first)
	}
}
