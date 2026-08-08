package bedrock

import (
	"testing"

	"GoCraft/core/intent"
	"GoCraft/core/player"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

func TestCanonicalPersonalCraftingSlots(t *testing.T) {
	for bedrockSlot := byte(28); bedrockSlot <= 31; bedrockSlot++ {
		got, ok := canonicalInventorySlot(protocol.StackRequestSlotInfo{
			Container: protocol.FullContainerName{ContainerID: protocol.ContainerCraftingInput},
			Slot:      bedrockSlot,
		})
		want := int16(1 + bedrockSlot - 28)
		if !ok || got != want {
			t.Fatalf("crafting slot %d = %d, %v; want %d, true", bedrockSlot, got, ok, want)
		}
	}
	output, ok := canonicalInventorySlot(protocol.StackRequestSlotInfo{
		Container: protocol.FullContainerName{ContainerID: protocol.ContainerCreatedOutput},
		Slot:      50,
	})
	if !ok || output != 0 {
		t.Fatalf("created output = %d, %v; want 0, true", output, ok)
	}
	cursor, ok := canonicalInventorySlot(protocol.StackRequestSlotInfo{
		Container: protocol.FullContainerName{ContainerID: protocol.ContainerCursor},
	})
	if !ok || cursor != intent.InventoryCursorSlot {
		t.Fatalf("cursor = %d, %v; want %d, true", cursor, ok, intent.InventoryCursorSlot)
	}
}

func TestCraftingInputResponseIncludesCreatedOutput(t *testing.T) {
	p := player.New([16]byte{31}, "crafter", player.ClientEditionBedrock)
	p.Inventory[3] = player.ItemStack{ItemID: "minecraft:birch_log", Count: 1}
	p.Inventory[0] = player.ItemStack{ItemID: "minecraft:birch_planks", Count: 4}
	session := &bedrockSession{nextStackNetworkID: 1}
	action := &protocol.PlaceStackRequestAction{}
	action.Count = 1
	action.Source = protocol.StackRequestSlotInfo{
		Container: protocol.FullContainerName{ContainerID: protocol.ContainerCursor},
		Slot:      0,
	}
	action.Destination = protocol.StackRequestSlotInfo{
		Container: protocol.FullContainerName{ContainerID: protocol.ContainerCraftingInput},
		Slot:      30,
	}

	groups := (&Listener{}).stackResponseContainerInfo(session, p, []protocol.StackRequestAction{action})
	for _, group := range groups {
		if group.Container.ContainerID != protocol.ContainerCreatedOutput {
			continue
		}
		for _, slot := range group.SlotInfo {
			if slot.Slot == 50 && slot.Count == 4 && slot.StackNetworkID > 0 {
				return
			}
		}
	}
	t.Fatalf("created output slot missing from response: %+v", groups)
}
