package bedrock

import (
	_ "embed"
	"fmt"
	"log/slog"

	dfworld "github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

//go:embed creative_items.nbt
var creativeItemsNBT []byte

// creativeNBTItem mirrors one item entry in Dragonfly's creative_items.nbt.
type creativeNBTItem struct {
	Name            string         `nbt:"name"`
	Meta            int16          `nbt:"meta"`
	NBTData         map[string]any `nbt:"nbt,omitempty"`
	BlockProperties map[string]any `nbt:"block_properties,omitempty"`
	GroupIndex      int32          `nbt:"group_index,omitempty"`
}

// creativeNBTGroup mirrors one creative group entry.
type creativeNBTGroup struct {
	Category int32           `nbt:"category"`
	Name     string          `nbt:"name"`
	Icon     creativeNBTItem `nbt:"icon"`
}

// creativeKnownItem is the canonical identity associated with a creative
// network ID. The simulation uses this when a Bedrock player selects an item.
type creativeKnownItem struct {
	name string
	meta int16
}

// initCreativeContent loads Dragonfly's bundled vanilla creative catalogue.
//
// This intentionally mirrors Dragonfly's behaviour:
//   - anonymous groups receive stable internal names such as "anon0";
//   - block entries resolve through BlockByName;
//   - regular items resolve through ItemByName;
//   - metadata must match exactly to prevent fallback duplicates;
//   - raw NBT group indices are preserved without adding one;
//   - invalid or unavailable entries are skipped.
func (l *Listener) initCreativeContent() {
	var root struct {
		Groups []creativeNBTGroup `nbt:"groups"`
		Items  []creativeNBTItem  `nbt:"items"`
	}
	if err := nbt.Unmarshal(creativeItemsNBT, &root); err != nil {
		slog.Warn(
			"bedrock: could not parse creative_items.nbt; creative menu will be empty",
			"err", err,
		)
		return
	}

	l.creativeGroups = make([]protocol.CreativeGroup, 0, len(root.Groups))
	for index, group := range root.Groups {
		name := group.Name
		if name == "" {
			name = fmt.Sprintf("anon%d", index)
		}

		icon, ok := creativeItemStack(group.Icon)
		if !ok {
			slog.Debug(
				"bedrock: creative group icon unavailable",
				"group_index", index,
				"group_name", name,
				"icon", group.Icon.Name,
				"meta", group.Icon.Meta,
			)
			icon = protocol.ItemStack{}
		}

		l.creativeGroups = append(l.creativeGroups, protocol.CreativeGroup{
			Category: group.Category,
			Name:     name,
			Icon:     icon,
		})
	}

	l.creativeItems = make([]protocol.CreativeItem, 0, len(root.Items))
	l.creativeNames = make(map[uint32]creativeKnownItem, len(root.Items))

	var skipped int
	for _, entry := range root.Items {
		if entry.GroupIndex < 0 || entry.GroupIndex >= int32(len(l.creativeGroups)) {
			slog.Warn(
				"bedrock: creative item has invalid group index",
				"name", entry.Name,
				"meta", entry.Meta,
				"group_index", entry.GroupIndex,
				"group_count", len(l.creativeGroups),
			)
			skipped++
			continue
		}

		stack, ok := creativeItemStack(entry)
		if !ok {
			slog.Debug(
				"bedrock: creative item unavailable",
				"name", entry.Name,
				"meta", entry.Meta,
				"group_index", entry.GroupIndex,
			)
			skipped++
			continue
		}

		creativeNetworkID := uint32(len(l.creativeItems) + 1)
		l.creativeItems = append(l.creativeItems, protocol.CreativeItem{
			CreativeItemNetworkID: creativeNetworkID,
			Item:                  stack,
			GroupIndex:            uint32(entry.GroupIndex),
		})
		l.creativeNames[creativeNetworkID] = creativeKnownItem{
			name: entry.Name,
			meta: entry.Meta,
		}
	}

	slog.Info(
		"bedrock: creative catalogue ready",
		"groups", len(l.creativeGroups),
		"items", len(l.creativeItems),
		"skipped", skipped,
	)
}

// creativeItemStack converts one NBT catalogue entry into a protocol stack.
func creativeItemStack(data creativeNBTItem) (protocol.ItemStack, bool) {
	if data.Name == "" {
		return protocol.ItemStack{}, false
	}

	var (
		resolvedItem dfworld.Item
		ok           bool
		blockRID     int32
	)

	if len(data.BlockProperties) > 0 {
		block, found := dfworld.BlockByName(data.Name, data.BlockProperties)
		if !found {
			return protocol.ItemStack{}, false
		}

		resolvedItem, ok = block.(dfworld.Item)
		if !ok {
			return protocol.ItemStack{}, false
		}
		blockRID = int32(dfworld.BlockRuntimeID(block))
	} else {
		resolvedItem, ok = dfworld.ItemByName(data.Name, data.Meta)
		if !ok {
			return protocol.ItemStack{}, false
		}

		_, resultingMeta := resolvedItem.EncodeItem()
		if resultingMeta != data.Meta {
			return protocol.ItemStack{}, false
		}
	}

	if decoder, ok := resolvedItem.(dfworld.NBTer); ok && len(data.NBTData) > 0 {
		decoded := decoder.DecodeNBT(cloneCreativeNBT(data.NBTData))
		item, valid := decoded.(dfworld.Item)
		if !valid {
			return protocol.ItemStack{}, false
		}
		resolvedItem = item
	}

	runtimeID, metadata, ok := dfworld.ItemRuntimeID(resolvedItem)
	if !ok {
		return protocol.ItemStack{}, false
	}

	if blockRID == 0 {
		if block, ok := resolvedItem.(dfworld.Block); ok {
			blockRID = int32(dfworld.BlockRuntimeID(block))
		}
	}

	return protocol.ItemStack{
		ItemType: protocol.ItemType{
			NetworkID:     runtimeID,
			MetadataValue: uint32(uint16(metadata)),
		},
		Count:          1,
		NBTData:        cloneCreativeNBT(data.NBTData),
		BlockRuntimeID: blockRID,
		HasNetworkID:   true,
	}, true
}

func cloneCreativeNBT(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// creativePlayerStack resolves the creative network ID selected by the client
// back to the canonical item identity used by GoCraft.
func (l *Listener) creativePlayerStack(creativeNetID uint32, count int) (creativeKnownItem, bool) {
	item, ok := l.creativeNames[creativeNetID]
	return item, ok
}

// creativeGroupDebugName returns the same stable anonymous-group naming scheme
// used while building the protocol catalogue.
func creativeGroupDebugName(index int, name string) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("anon%d", index)
}
