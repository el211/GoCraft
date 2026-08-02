package handler

import (
	"bytes"
	"testing"

	"GoCraft/core/player"
	"GoCraft/java/protocol"
)

func TestDamageableSlotCarriesVisibleDurability(t *testing.T) {
	b := protocol.NewBuilder(0)
	encodeSlot(b, player.ItemStack{ItemID: "minecraft:golden_sword", Count: 1, Damage: 7})
	data := b.Build().Data
	if !bytes.Contains(data, []byte("Durability: 25 / 32")) {
		t.Fatalf("encoded slot does not contain visible durability lore: %x", data)
	}
	if !bytes.Contains(data, []byte("italic")) || !bytes.Contains(data, []byte("green")) {
		t.Fatalf("encoded durability lore is missing explicit non-italic styling: %x", data)
	}
	decoded, err := readPlainSlot(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("readPlainSlot: %v", err)
	}
	if decoded.ItemID != "minecraft:golden_sword" || decoded.Count != 1 || decoded.Damage != 7 {
		t.Fatalf("decoded slot = %+v", decoded)
	}
}

func TestLegacyTooltipHidesVanillaAttributesAndUsesInstantLabel(t *testing.T) {
	ConfigureItemTooltips(true, true, true, true)
	defer ConfigureItemTooltips(true, true, true, false)
	b := protocol.NewBuilder(0)
	encodeSlot(b, player.ItemStack{ItemID: "minecraft:iron_sword", Count: 1})
	data := b.Build().Data
	if !bytes.Contains(data, []byte("Instant Attack Speed")) {
		t.Fatalf("legacy tooltip is missing clean instant label: %x", data)
	}
	if !bytes.Contains(data, []byte("When in Main Hand:")) || !bytes.Contains(data, []byte(" 6 Attack Damage")) {
		t.Fatalf("legacy tooltip is missing the vanilla-style main-hand section: %x", data)
	}
	if bytes.Contains(data, []byte("1024")) {
		t.Fatalf("legacy tooltip leaked internal attack-speed override: %x", data)
	}
	if _, err := readPlainSlot(bytes.NewReader(data)); err != nil {
		t.Fatalf("slot with hidden vanilla attributes did not decode: %v", err)
	}
}

func TestCustomTooltipSectionsCanBeDisabled(t *testing.T) {
	ConfigureItemTooltips(false, false, true, true)
	defer ConfigureItemTooltips(true, true, true, false)
	b := protocol.NewBuilder(0)
	encodeSlot(b, player.ItemStack{ItemID: "minecraft:iron_sword", Count: 1})
	data := b.Build().Data
	if bytes.Contains(data, []byte("Durability:")) || bytes.Contains(data, []byte("Attack Damage:")) {
		t.Fatalf("disabled custom lore is still present: %x", data)
	}
}

func TestArmorTooltipUsesVanillaStyleSlotSection(t *testing.T) {
	ConfigureItemTooltips(true, true, true, true)
	defer ConfigureItemTooltips(true, true, true, false)
	b := protocol.NewBuilder(0)
	encodeSlot(b, player.ItemStack{ItemID: "minecraft:diamond_helmet", Count: 1})
	data := b.Build().Data
	for _, text := range []string{"When on Head:", " 3 Armor", " 2 Armor Toughness"} {
		if !bytes.Contains(data, []byte(text)) {
			t.Fatalf("armor tooltip is missing %q: %x", text, data)
		}
	}
}

func TestPlayerInventoryClickEquipsArmor(t *testing.T) {
	p := &player.Player{}
	p.CarriedItem = player.ItemStack{ItemID: "minecraft:diamond_chestplate", Count: 1}
	clickPlayerInventorySlot(p, 6, 0)
	if got := p.Inventory[6].ItemID; got != "minecraft:diamond_chestplate" {
		t.Fatalf("chest slot = %q", got)
	}
	if !p.CarriedItem.IsEmpty() || p.ArmorPoints() != 8 {
		t.Fatalf("equip state carried=%+v armor=%d", p.CarriedItem, p.ArmorPoints())
	}
}

func TestArmorAttributeUsesEquippedPoints(t *testing.T) {
	p := &player.Player{EntityID: 42}
	p.Inventory[5] = player.ItemStack{ItemID: "minecraft:golden_helmet", Count: 1}
	p.Inventory[6] = player.ItemStack{ItemID: "minecraft:golden_chestplate", Count: 1}
	packet := buildArmorAttributes(p)
	r := bytes.NewReader(packet.Data)
	entityID, _ := protocol.ReadVarInt(r)
	count, _ := protocol.ReadVarInt(r)
	attributeID, _ := protocol.ReadVarInt(r)
	value, _ := protocol.ReadDouble(r)
	modifiers, _ := protocol.ReadVarInt(r)
	if entityID != 42 || count != 1 || attributeID != 0 || value != 7 || modifiers != 0 || r.Len() != 0 {
		t.Fatalf("armor payload entity=%d count=%d id=%d value=%v modifiers=%d trailing=%d",
			entityID, count, attributeID, value, modifiers, r.Len())
	}
}
