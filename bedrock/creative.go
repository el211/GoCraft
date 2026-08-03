package bedrock

import (
	_ "embed"
	"log/slog"

	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

//go:embed creative_items.nbt
var creativeItemsNBT []byte

// creativeNBTItem mirrors the on-disk NBT entry for one creative item.
type creativeNBTItem struct {
	Name            string         `nbt:"name"`
	Meta            int16          `nbt:"meta"`
	NBTData         map[string]any `nbt:"nbt,omitempty"`
	BlockProperties map[string]any `nbt:"block_properties,omitempty"`
	GroupIndex      int32          `nbt:"group_index,omitempty"`
}

// creativeNBTGroup mirrors the on-disk NBT entry for one creative group.
type creativeNBTGroup struct {
	Category int32           `nbt:"category"`
	Name     string          `nbt:"name"`
	Icon     creativeNBTItem `nbt:"icon"`
}

// creativeKnownItem is a resolved name+meta pair stored in the Listener for
// reverse-lookups when a player picks an item from the creative menu.
type creativeKnownItem struct {
	name string
	meta int16
}

// initCreativeContent parses creative_items.nbt (bundled from dragonfly) and
// populates l.creativeGroups, l.creativeItems, and l.creativeNames.
// Called once from NewListener; errors are logged and result in an empty catalogue.
func (l *Listener) initCreativeContent() {
	var root struct {
		Groups []creativeNBTGroup `nbt:"groups"`
		Items  []creativeNBTItem  `nbt:"items"`
	}
	if err := nbt.Unmarshal(creativeItemsNBT, &root); err != nil {
		slog.Warn("bedrock: could not parse creative_items.nbt — creative menu will be empty", "err", err)
		return
	}

	l.creativeGroups = make([]protocol.CreativeGroup, 0, len(root.Groups))
	for _, g := range root.Groups {
		l.creativeGroups = append(l.creativeGroups, protocol.CreativeGroup{
			Category: g.Category,
			Name:     g.Name,
			Icon:     nbtItemToStack(g.Icon),
		})
	}

	l.creativeItems = make([]protocol.CreativeItem, 0, len(root.Items))
	l.creativeNames = make(map[uint32]creativeKnownItem, len(root.Items))
	networkID := uint32(1)
	for _, item := range root.Items {
		stack := nbtItemToStack(item)
		// Skip items whose Bedrock network IDs could not be resolved.
		if stack.NetworkID == 0 && stack.BlockRuntimeID == 0 {
			continue
		}
		l.creativeItems = append(l.creativeItems, protocol.CreativeItem{
			CreativeItemNetworkID: networkID,
			Item:                  stack,
			GroupIndex:            uint32(item.GroupIndex),
		})
		l.creativeNames[networkID] = creativeKnownItem{name: item.Name, meta: item.Meta}
		networkID++
	}
	slog.Info("bedrock: creative catalogue ready", "groups", len(l.creativeGroups), "items", len(l.creativeItems))
}

// nbtItemToStack converts one creative NBT entry to the protocol.ItemStack the
// Bedrock client expects inside CreativeContent.
// It uses dragonfly's item/block registries directly to ensure runtime IDs match.
func nbtItemToStack(data creativeNBTItem) protocol.ItemStack {
	if len(data.BlockProperties) > 0 {
		// Block item — resolve via dragonfly's block registry which uses the same
		// runtime ID table as our encoder.
		b, ok := dfworld.BlockByName(data.Name, data.BlockProperties)
		if !ok {
			return protocol.ItemStack{}
		}
		blockRID := int32(dfworld.BlockRuntimeID(b))

		var networkID int32
		var metaValue int16
		if it, isItem := b.(dfworld.Item); isItem {
			rid, meta, ok := dfworld.ItemRuntimeID(it)
			if ok {
				networkID = rid
				metaValue = meta
			}
		}
		return protocol.ItemStack{
			ItemType: protocol.ItemType{
				NetworkID:     networkID,
				MetadataValue: uint32(uint16(metaValue)),
			},
			Count:          1,
			BlockRuntimeID: blockRID,
			HasNetworkID:   networkID != 0,
		}
	}

	// Regular item — use dragonfly's item registry.
	it, ok := dfworld.ItemByName(data.Name, data.Meta)
	if !ok {
		return protocol.ItemStack{}
	}
	rid, meta, ok := dfworld.ItemRuntimeID(it)
	if !ok {
		return protocol.ItemStack{}
	}

	// Attach block runtime ID for items that are also placeable blocks.
	blockRID := int32(0)
	if b, ok := it.(dfworld.Block); ok {
		blockRID = int32(dfworld.BlockRuntimeID(b))
	}

	return protocol.ItemStack{
		ItemType: protocol.ItemType{
			NetworkID:     rid,
			MetadataValue: uint32(uint16(meta)),
		},
		Count:          1,
		BlockRuntimeID: blockRID,
		HasNetworkID:   true,
	}
}

// creativePlayerStack returns the player.ItemStack for a creative item by its
// network ID (assigned in initCreativeContent). Returns false if the ID is
// unknown.
func (l *Listener) creativePlayerStack(creativeNetID uint32, count int) (creativeKnownItem, bool) {
	ki, ok := l.creativeNames[creativeNetID]
	return ki, ok
}
