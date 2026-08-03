package server

import (
	"testing"

	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	"GoCraft/java/session"
)

func TestBedrockAttackDamagesJavaPlayer(t *testing.T) {
	g := game.New()
	attacker := player.New([16]byte{1}, "bedrock", player.ClientEditionBedrock)
	target := player.New([16]byte{2}, "java", player.ClientEditionJava)
	attacker.GameMode = player.GameModeSurvival
	target.GameMode = player.GameModeSurvival
	attacker.Position = spatial.Vec3{X: 0, Y: 64, Z: 0}
	target.Position = spatial.Vec3{X: 1, Y: 64, Z: 0}
	attacker.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:diamond_sword", Count: 1}
	if err := g.AddPlayer(attacker); err != nil {
		t.Fatal(err)
	}
	if err := g.AddPlayer(target); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager()
	s := &Server{game: g, sessions: mgr}

	s.applyBedrockEntityInteract(intent.EntityInteractIntent{PlayerUUID: attacker.UUID, TargetID: target.EntityID, Attack: true})
	health, _, _, _ := target.HealthSnapshot()
	if health >= target.MaxHealth {
		t.Fatalf("Java target health = %v, want damage from Bedrock attacker", health)
	}
}
