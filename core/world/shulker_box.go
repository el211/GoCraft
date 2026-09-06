package world

import (
	"encoding/json"
	"strings"

	"GoCraft/core/player"
)

// IsShulkerBox reports whether the block or item name is a shulker box.
func IsShulkerBox(name string) bool {
	return name == "minecraft:shulker_box" || strings.HasSuffix(name, "_shulker_box")
}

// ShulkerBoxDropItem returns the item to drop when a shulker box is broken.
// If the box contains items, they are serialised into the minecraft:container
// component so the dropped item preserves its inventory rather than spilling.
func ShulkerBoxDropItem(blockName string, contents []ContainerItem) player.ItemStack {
	drop := player.ItemStack{ItemID: blockName, Count: 1}
	if len(contents) == 0 {
		return drop
	}

	type slotEntry struct {
		Slot int                    `json:"slot"`
		Item map[string]interface{} `json:"item"`
	}
	entries := make([]slotEntry, 0, len(contents))
	for _, ci := range contents {
		if ci.ItemID == "" || ci.Count <= 0 {
			continue
		}
		item := map[string]interface{}{
			"id":    ci.ItemID,
			"count": ci.Count,
		}
		if ci.Components != "" {
			var comp map[string]json.RawMessage
			if err := json.Unmarshal([]byte(ci.Components), &comp); err == nil && len(comp) > 0 {
				item["components"] = comp
			}
		}
		entries = append(entries, slotEntry{Slot: ci.Slot, Item: item})
	}
	if len(entries) == 0 {
		return drop
	}
	if err := drop.SetComponent("minecraft:container", entries); err == nil {
		return drop
	}
	return drop
}
