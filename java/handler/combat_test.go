package handler

import (
	"bytes"
	"testing"

	"GoCraft/core/player"
	"GoCraft/java/protocol"
)

func TestLegacyCombatSendsInstantAttackSpeedAttribute(t *testing.T) {
	p := player.New([16]byte{}, "fighter", player.ClientEditionJava)
	p.EntityID = 42
	p.AttackCooldown = false
	packet := buildCombatAttributes(p)
	if packet == nil || packet.ID != packetIDUpdateAttributes {
		t.Fatalf("attribute packet = %#v", packet)
	}
	r := bytes.NewReader(packet.Data)
	entityID, _ := protocol.ReadVarInt(r)
	count, _ := protocol.ReadVarInt(r)
	attributeID, _ := protocol.ReadVarInt(r)
	value, _ := protocol.ReadDouble(r)
	modifiers, _ := protocol.ReadVarInt(r)
	if entityID != 42 || count != 1 || attributeID != 4 || value != 1024 || modifiers != 0 || r.Len() != 0 {
		t.Fatalf("attribute payload entity=%d count=%d id=%d value=%v modifiers=%d trailing=%d", entityID, count, attributeID, value, modifiers, r.Len())
	}
	p.AttackCooldown = true
	if packet := buildCombatAttributes(p); packet != nil {
		t.Fatal("modern cooldown mode unexpectedly overrides attack speed")
	}
}
