package handler

import (
	"bytes"
	"testing"

	"GoCraft/core/player"
	"GoCraft/java/protocol"
	"GoCraft/java/session"
)

func TestReviveProvidesRespawnInvulnerability(t *testing.T) {
	p := &player.Player{
		Health: 0, MaxHealth: 20, Dead: true, GameMode: player.GameModeSurvival,
		OnGround: true, Sleeping: true, Sprinting: true, Flying: true,
		FallDistance: 12, UsingItemID: "minecraft:bow", VehicleEntityID: 42,
	}
	p.Revive()
	target := &session.Session{Player: p}
	if DamagePlayer(target, 10, "was tested", nil) {
		t.Fatal("damage was accepted during respawn invulnerability")
	}
	if p.Health != 20 || p.Dead {
		t.Fatalf("respawn state = health %.1f dead %v", p.Health, p.Dead)
	}
	if p.OnGround || p.Sleeping || p.Sprinting || p.Flying || p.FallDistance != 0 || p.UsingItemID != "" {
		t.Fatalf("transient movement state survived respawn: %+v", p)
	}
	// VehicleEntityID is deliberately cleared by the world-aware respawn path,
	// not Revive, so that the boat's reverse rider link is also removed.
	if p.VehicleEntityID != 42 {
		t.Fatalf("Revive changed vehicle without clearing the vehicle rider link")
	}
}

func TestUpdateHealthPacketProtocol769(t *testing.T) {
	p := player.New([16]byte{}, "health", player.ClientEditionJava)
	p.ApplyDamage(3.5, "test")
	pkt := buildUpdateHealth(p)
	if pkt.ID != packetIDUpdateHealth {
		t.Fatalf("packet ID = %d, want %d", pkt.ID, packetIDUpdateHealth)
	}
	r := bytes.NewReader(pkt.Data)
	health, _ := protocol.ReadFloat(r)
	food, _ := protocol.ReadVarInt(r)
	saturation, _ := protocol.ReadFloat(r)
	if health != 16.5 || food != 20 || saturation != 5 || r.Len() != 0 {
		t.Fatalf("health payload = (%v,%d,%v), trailing=%d", health, food, saturation, r.Len())
	}
}

func TestLegacyArmorReduction(t *testing.T) {
	p := player.New([16]byte{}, "tank", player.ClientEditionJava)
	p.Inventory[5] = player.ItemStack{ItemID: "minecraft:diamond_helmet", Count: 1}
	p.Inventory[6] = player.ItemStack{ItemID: "minecraft:diamond_chestplate", Count: 1}
	p.Inventory[7] = player.ItemStack{ItemID: "minecraft:diamond_leggings", Count: 1}
	p.Inventory[8] = player.ItemStack{ItemID: "minecraft:diamond_boots", Count: 1}
	if got := reducedDamage(p, 10, true); got != 2 {
		t.Fatalf("legacy full diamond damage = %v, want 2", got)
	}
}
