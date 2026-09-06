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
	"GoCraft/core/intent"
	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
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
	input1          tradeItem
	input2          tradeItem // zero-value means no second input
	output          tradeItem
	maxUses         int32
	xpPerTrade      int32
	tier            int32 // zero-based Bedrock tier
	priceMultiplier float32
}

type tradeItem struct {
	itemName string // canonical resource location, e.g. "minecraft:wheat"
	count    int32
}

// VillagerTrade is the protocol-neutral view of a trade offer shared with the
// Bedrock adapter. The Java wire encoder keeps using the private representation
// below so edition details do not leak into the simulation.
type VillagerTrade struct {
	Input1, Input2  player.ItemStack
	Output          player.ItemStack
	MaxUses         int32
	XP              int32
	Tier            int32
	PriceMultiplier float32
}

// VillagerTrades returns a detached copy of the unlocked offers for exactly one
// profession. The optional level defaults to novice for compatibility.
func VillagerTrades(profession corentity.VillagerProfession, levels ...int32) []VillagerTrade {
	offers := tradesForProfession(profession, levels...)
	out := make([]VillagerTrade, 0, len(offers))
	for _, offer := range offers {
		trade := VillagerTrade{
			Input1:          player.ItemStack{ItemID: offer.input1.itemName, Count: int(offer.input1.count)},
			Output:          player.ItemStack{ItemID: offer.output.itemName, Count: int(offer.output.count)},
			MaxUses:         offer.maxUses,
			XP:              offer.xpPerTrade,
			Tier:            offer.tier,
			PriceMultiplier: offer.priceMultiplier,
		}
		if offer.input2.itemName != "" && offer.input2.count > 0 {
			trade.Input2 = player.ItemStack{ItemID: offer.input2.itemName, Count: int(offer.input2.count)}
		}
		out = append(out, trade)
	}
	return out
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
func handleInteractPacket(pkt *protocol.Packet, p *player.Player, w *coreworld.World, conn *network.ClientConn, mgr *session.Manager, buses ...*intent.Bus) error {
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
			if target := playerSessionByEntityID(mgr, entityID); target != nil {
				if target.Player == p || squaredPlayerDistance(p, target.Player) > 9 {
					return nil
				}
				var damaged bool
				if p.AttackCooldown {
					damaged = DamagePlayer(target, damage, "was slain by "+p.Username, mgr)
				} else {
					damaged = DamagePlayerLegacy(target, damage, "was slain by "+p.Username, mgr)
				}
				if damaged {
					horizontal := p.KnockbackHorizontal
					vertical := p.KnockbackVertical
					if horizontal <= 0 {
						horizontal = 0.4
					}
					if vertical <= 0 {
						vertical = 0.4
					}
					if p.Sprinting {
						horizontal *= 2
					}
					if target.Conn != nil {
						SendLegacyKnockback(target, p.Position.X, p.Position.Z, horizontal, vertical)
					} else {
						mgr.KnockbackExternal(target.Player, p.Position.X, p.Position.Z, horizontal, vertical)
					}
					p.LastAttack = now
					damageHeldItem(p, conn, 1)
					slog.Info("player attacked player", "attacker", p.Username, "target", target.Player.Username, "damage", damage)
				}
			} else if w.QueueEntityDamageFromPlayer(entityID, damage, p.Position.X, p.Position.Z, p.UUID) {
				p.LastAttack = now
				damageHeldItem(p, conn, 1)
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
	if !ok {
		return nil
	}
	// Name tag: applying a name tag to any entity sets its custom name.
	if mainHand && p.HeldItem().ItemID == "minecraft:name_tag" &&
		p.GameMode != player.GameModeSpectator {
		nameTag := p.HeldItem()
		name := nameTag.DisplayName()
		if name != "" {
			entity.DisplayName = name
			entity.CustomNameVisible = true
			BroadcastMobMetadata(entity, mgr)
			if p.GameMode != player.GameModeCreative {
				slot := player.HotbarStart + p.HeldSlot
				p.Inventory[slot].Count--
				normalizeStack(&p.Inventory[slot])
				if conn != nil {
					_ = SyncPlayerInventory(conn, p)
				} else {
					p.ContainerStateID++
				}
			}
			return nil
		}
	}
	if (corentity.IsAgeableAnimal(entity.Type) || corentity.IsTameableAnimal(entity.Type) || corentity.IsAnimalVehicle(entity.Type)) && len(buses) > 0 && buses[0] != nil {
		buses[0].PostEntityInteract(intent.EntityInteractIntent{
			PlayerUUID: p.UUID,
			TargetID:   entityID,
			HotbarSlot: int32(p.HeldSlot),
		})
		return nil
	}

	// Boat boarding: right-clicking a boat mounts the player.
	if corentity.IsBoat(entity.Type) || corentity.IsMinecart(entity.Type) {
		if p.VehicleEntityID == 0 {
			MountPlayer(p, entity.EntityID, w, mgr)
			slog.Info("player boarded vehicle", "player", p.Username, "vehicleID", entity.EntityID)
		}
		return nil
	}

	if entity.Type != corentity.TypeVillager {
		return nil
	}
	if !entity.CanTradeAsVillager() {
		BroadcastVillagerUnhappy(mgr, entity)
		return nil
	}

	if err := sendOpenScreen(conn, villagerWindowID, merchantContainerType, "Villager"); err != nil {
		return fmt.Errorf("interact: opening screen: %w", err)
	}
	if err := sendMerchantOffers(conn, villagerWindowID, tradesForProfession(entity.VillagerProfession, entity.VillagerLevel), entity.VillagerLevel); err != nil {
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
func sendMerchantOffers(conn *network.ClientConn, windowID int32, trades []tradeOffer, levels ...int32) error {
	return conn.WritePacket(buildMerchantOffers(windowID, trades, levels...))
}

func buildMerchantOffers(windowID int32, trades []tradeOffer, levels ...int32) *protocol.Packet {
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

		priceMultiplier := trade.priceMultiplier
		if priceMultiplier == 0 {
			priceMultiplier = 0.05
		}
		b.Bool(false).
			Int(0).
			Int(trade.maxUses).
			Int(trade.xpPerTrade).
			Int(0).
			Float(priceMultiplier).
			Int(0)
	}

	level := int32(1)
	if len(levels) > 0 && levels[0] >= 1 && levels[0] <= 5 {
		level = levels[0]
	}
	return b.VarInt(level).
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

func tradesForProfession(profession corentity.VillagerProfession, levels ...int32) []tradeOffer {
	level := int32(1)
	if len(levels) > 0 {
		level = levels[0]
	}
	if level < 1 {
		level = 1
	} else if level > 5 {
		level = 5
	}
	pools, ok := vanillaVillagerTradeCatalog[profession]
	if !ok {
		return nil
	}
	offers := make([]tradeOffer, 0, level*2)
	for current := int32(1); current <= level; current++ {
		offers = append(offers, pools[current]...)
	}
	return offers
}

func playerAttackCooldown(p *player.Player) time.Duration {
	_, speed, ok := player.AttackAttributes(p.HeldItem().ItemID)
	if !ok || speed <= 0 {
		speed = 4
	}
	return time.Duration(float64(time.Second) / float64(speed))
}
func playerAttackDamage(p *player.Player) float32 {
	if p.AttackCooldown {
		if damage, _, ok := player.AttackAttributes(p.HeldItem().ItemID); ok {
			return damage
		}
	}
	return player.LegacyAttackDamage(p.HeldItem().ItemID)
}

func playerSessionByEntityID(mgr *session.Manager, entityID int32) *session.Session {
	if mgr == nil {
		return nil
	}
	return mgr.PlayerSessionByEntityID(entityID)
}

func squaredPlayerDistance(a, b *player.Player) float64 {
	dx := a.Position.X - b.Position.X
	dy := a.Position.Y - b.Position.Y
	dz := a.Position.Z - b.Position.Z
	return dx*dx + dy*dy + dz*dz
}
