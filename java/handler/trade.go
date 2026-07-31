package handler

// trade.go implements villager trading for Java Edition 1.21.4.
//
// When a player right-clicks (INTERACT or INTERACT_AT) a villager entity, the
// server:
//  1. Sends Open Screen (0x35) to open the Merchant UI (container type 19).
//  2. Sends Merchant Offers (0x2E) with the trade list.
//
// The trade list is currently static — no persistence is needed for a basic
// implementation.  Each trade is a cheap input-item → output-item mapping; the
// client renders prices, use counters, and restock indicators automatically.

import (
	"fmt"
	"log/slog"
	"time"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

// merchantContainerType is the protocol ID for minecraft:merchant in the
// minecraft:menu registry (confirmed from registries.json).
const merchantContainerType = int32(19)

// villagerWindowID is the fixed merchant window ID used for all villager UIs.
// A real implementation should use a per-player sequential counter; the simple
// fixed value works because only one GUI can be open at a time per player.
const villagerWindowID = int32(1)

// tradeOffer defines a single merchant trade: items the player pays (up to two
// inputs) and the item they receive.
type tradeOffer struct {
	input1     tradeItem
	input2     tradeItem // zero-value means no second input
	output     tradeItem
	maxUses    int32
	xpPerTrade int32
}

type tradeItem struct {
	itemName string // canonical resource location, e.g. "minecraft:wheat"
	count    int32
}

// defaultVillagerTrades returns the static trade list shown for all villagers.
// For a biome-aware implementation, pass the villager biome to vary the trades.
var defaultVillagerTrades = []tradeOffer{
	// Farmer: sell wheat to the villager for emeralds
	{
		input1:     tradeItem{"minecraft:wheat", 20},
		output:     tradeItem{"minecraft:emerald", 1},
		maxUses:    12,
		xpPerTrade: 5,
	},
	// Farmer: buy bread with an emerald
	{
		input1:     tradeItem{"minecraft:emerald", 1},
		output:     tradeItem{"minecraft:bread", 6},
		maxUses:    12,
		xpPerTrade: 5,
	},
	// Librarian: sell paper for emeralds
	{
		input1:     tradeItem{"minecraft:paper", 24},
		output:     tradeItem{"minecraft:emerald", 1},
		maxUses:    12,
		xpPerTrade: 3,
	},
	// General: buy an apple with an emerald
	{
		input1:     tradeItem{"minecraft:emerald", 1},
		output:     tradeItem{"minecraft:apple", 4},
		maxUses:    12,
		xpPerTrade: 3,
	},
	// Farmer: sell carrots for emeralds
	{
		input1:     tradeItem{"minecraft:carrot", 22},
		output:     tradeItem{"minecraft:emerald", 1},
		maxUses:    12,
		xpPerTrade: 3,
	},
}

// handleInteractPacket parses a C→S Interact (0x19) packet.
//
// Wire layout (1.21.4):
//
//	VarInt  entity_id
//	VarInt  type  (0=INTERACT, 1=ATTACK, 2=INTERACT_AT)
//	Float×3 target_x/y/z  (only if type == 2)
//	VarInt  hand  (0=MAIN, 1=OFF; only if type == 0 or 2)
//	Bool    sneaking
//
// If the targeted entity is a villager and the interaction is INTERACT with the
// main hand, the trading UI is opened.
func handleInteractPacket(pkt *protocol.Packet, p *player.Player, w *coreworld.World, conn *network.ClientConn) error {
	r := pkt.Reader()

	entityID, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("interact: reading entity id: %w", err)
	}
	interactType, err := protocol.ReadVarInt(r)
	if err != nil {
		return fmt.Errorf("interact: reading type: %w", err)
	}
	if interactType < 0 || interactType > 2 {
		return fmt.Errorf("interact: invalid type %d", interactType)
	}

	if interactType == 2 {
		for i := 0; i < 3; i++ {
			if _, err := protocol.ReadFloat(r); err != nil {
				return fmt.Errorf("interact: reading target coord: %w", err)
			}
		}
	}

	mainHand := true
	if interactType == 0 || interactType == 2 {
		hand, err := protocol.ReadVarInt(r)
		if err != nil {
			return fmt.Errorf("interact: reading hand: %w", err)
		}
		mainHand = hand == 0
	}
	if _, err := protocol.ReadBool(r); err != nil {
		return fmt.Errorf("interact: reading sneaking flag: %w", err)
	}
	if r.Len() != 0 {
		return fmt.Errorf("interact: %d trailing payload bytes", r.Len())
	}

	if interactType == 1 {
		if p.GameMode != player.GameModeSpectator {
			now := time.Now()
			if p.AttackCooldown && !p.LastAttack.IsZero() && now.Sub(p.LastAttack) < playerAttackCooldown(p) {
				return nil
			}
			damage := playerAttackDamage(p)
			if w.QueueEntityDamageFrom(entityID, damage, p.Position.X, p.Position.Z) {
				p.LastAttack = now
				slog.Info("entity attack queued", "player", p.Username, "entityID", entityID, "damage", damage)
			} else {
				slog.Debug("entity attack ignored", "player", p.Username, "entityID", entityID)
			}
		}
		return nil
	}
	if !mainHand {
		return nil
	}

	entity, ok := w.Entities.Get(entityID)
	if !ok || entity.Type != corentity.TypeVillager {
		return nil
	}

	if err := sendOpenScreen(conn, villagerWindowID, merchantContainerType, "Villager"); err != nil {
		return fmt.Errorf("interact: opening screen: %w", err)
	}
	if err := sendMerchantOffers(conn, villagerWindowID, tradesForProfession(entity.VillagerProfession)); err != nil {
		return fmt.Errorf("interact: sending offers: %w", err)
	}
	return nil
}

// sendOpenScreen sends the Open Screen packet (0x35 S→C).
//
// Wire layout (1.21.4):
//
//	VarInt         window_id
//	VarInt         window_type  (container type index from minecraft:menu registry)
//	Text Component title        (Network NBT format, same as System Chat)
func sendOpenScreen(conn *network.ClientConn, windowID, windowType int32, title string) error {
	pkt := protocol.NewBuilder(packetIDOpenScreen).
		VarInt(windowID).
		VarInt(windowType).
		Bytes(nbtTextComponent(title)).
		Build()
	return conn.WritePacket(pkt)
}

// sendMerchantOffers sends the Merchant Offers packet (0x2E S→C).
//
// Wire layout (1.21.4):
//
//	VarInt  window_id
//	VarInt  size  (number of trades)
//	For each trade:
//	  ItemCost input_item_1
//	  Slot    output_item
//	  Bool    has_second_input
//	  [ItemCost input_item_2  — only when has_second_input]
//	  Bool    out_of_stock
//	  Int     number_of_trades_uses
//	  Int     max_uses
//	  Int     xp
//	  Int     special_price
//	  Float   price_multiplier
//	  Int     demand
//	VarInt  villager_level   (1–5)
//	VarInt  villager_xp
//	Bool    is_regular_villager
//	Bool    can_restock
func sendMerchantOffers(conn *network.ClientConn, windowID int32, trades []tradeOffer) error {
	return conn.WritePacket(buildMerchantOffers(windowID, trades))
}

func buildMerchantOffers(windowID int32, trades []tradeOffer) *protocol.Packet {
	b := protocol.NewBuilder(packetIDMerchantOffers).
		VarInt(windowID).
		VarInt(int32(len(trades)))

	for _, trade := range trades {
		encodeTradeCost(b, trade.input1)
		encodeTradingSlot(b, trade.output)

		hasSecond := trade.input2.itemName != ""
		b.Bool(hasSecond)
		if hasSecond {
			encodeTradeCost(b, trade.input2)
		}

		b.Bool(false).
			Int(0).
			Int(trade.maxUses).
			Int(trade.xpPerTrade).
			Int(0).
			Float(0.05).
			Int(0)
	}

	return b.VarInt(1).
		VarInt(0).
		Bool(true).
		Bool(true).
		Build()
}

// encodeTradeCost encodes the 1.21.4 ItemCost structure used for merchant
// inputs: item ID, count, and the required data-component predicate.
func encodeTradeCost(b *protocol.Builder, item tradeItem) {
	id := javaworld.ItemID(item.itemName)
	if item.itemName == "" || item.count <= 0 || id < 0 {
		b.VarInt(0).VarInt(0).VarInt(0)
		return
	}
	b.VarInt(id).
		VarInt(item.count).
		VarInt(0)
}

// encodeTradingSlot encodes a tradeItem as a 1.21.4 slot into b.
// Empty item name → empty slot (VarInt 0).
func encodeTradingSlot(b *protocol.Builder, item tradeItem) {
	if item.itemName == "" || item.count <= 0 {
		b.VarInt(0) // empty slot
		return
	}
	id := javaworld.ItemID(item.itemName)
	if id < 0 {
		b.VarInt(0) // unknown item — send empty rather than corrupt ID
		return
	}
	b.VarInt(item.count).
		VarInt(id).
		VarInt(0). // components_to_add
		VarInt(0)  // components_to_remove
}

func tradesForProfession(profession corentity.VillagerProfession) []tradeOffer {
	switch profession {
	case corentity.VillagerProfessionLibrarian:
		return []tradeOffer{
			{input1: tradeItem{"minecraft:paper", 24}, output: tradeItem{"minecraft:emerald", 1}, maxUses: 12, xpPerTrade: 3},
			{input1: tradeItem{"minecraft:emerald", 1}, output: tradeItem{"minecraft:bookshelf", 1}, maxUses: 12, xpPerTrade: 5},
		}
	case corentity.VillagerProfessionFletcher:
		return []tradeOffer{
			{input1: tradeItem{"minecraft:stick", 32}, output: tradeItem{"minecraft:emerald", 1}, maxUses: 16, xpPerTrade: 2},
			{input1: tradeItem{"minecraft:emerald", 1}, output: tradeItem{"minecraft:arrow", 16}, maxUses: 12, xpPerTrade: 2},
		}
	default:
		return defaultVillagerTrades
	}
}

func playerAttackCooldown(p *player.Player) time.Duration {
	switch p.HeldItem().ItemID {
	case "minecraft:wooden_axe", "minecraft:stone_axe":
		return 1250 * time.Millisecond
	case "minecraft:iron_axe":
		return 1100 * time.Millisecond
	case "minecraft:diamond_axe", "minecraft:netherite_axe", "minecraft:golden_axe":
		return time.Second
	case "minecraft:wooden_sword", "minecraft:stone_sword", "minecraft:iron_sword",
		"minecraft:diamond_sword", "minecraft:netherite_sword", "minecraft:golden_sword":
		return 625 * time.Millisecond
	default:
		return 250 * time.Millisecond
	}
}
func playerAttackDamage(p *player.Player) float32 {
	switch p.HeldItem().ItemID {
	case "minecraft:wooden_sword", "minecraft:golden_sword":
		return 4
	case "minecraft:stone_sword":
		return 5
	case "minecraft:iron_sword":
		return 6
	case "minecraft:diamond_sword":
		return 7
	case "minecraft:netherite_sword":
		return 8
	case "minecraft:wooden_axe", "minecraft:golden_axe":
		return 7
	case "minecraft:stone_axe", "minecraft:iron_axe", "minecraft:diamond_axe":
		return 9
	case "minecraft:netherite_axe":
		return 10
	default:
		return 1
	}
}
