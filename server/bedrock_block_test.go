package server

import (
	"testing"

	"GoCraft/core/game"
	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/session"
)

func TestBedrockBreakingDoublePlantRemovesBothHalves(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	g := game.New()
	p := player.New([16]byte{11}, "bedrock-gardener", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Position = spatial.Vec3{X: 0.5, Y: 64, Z: 0.5}
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	lower := coreworld.Block{Namespace: "minecraft", Name: "peony", Properties: map[string]string{"half": "lower"}}
	upper := coreworld.Block{Namespace: "minecraft", Name: "peony", Properties: map[string]string{"half": "upper"}}
	w.SetBlock(0, 64, 0, lower)
	w.SetBlock(0, 65, 0, upper)
	s := &Server{game: g, world: w, sessions: session.NewManager()}

	s.applyBedrockBlockInteract(intent.BlockInteractIntent{
		PlayerUUID: p.UUID,
		Action:     intent.BlockActionBreak,
		Position:   spatial.BlockPos{X: 0, Y: 64, Z: 0},
	})

	for y := 64; y <= 65; y++ {
		if got := w.GetBlock(0, y, 0); !got.IsAir() {
			t.Fatalf("plant half y=%d = %q, want air", y, got.ResourceLocation())
		}
	}
}

func TestBedrockPlayerStateAcceptsSurvivalSprintButRejectsFlight(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{12}, `bedrock-runner`, player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	s := &Server{game: g}

	s.applyBedrockPlayerState(intent.PlayerStateIntent{
		PlayerUUID: p.UUID,
		State:      intent.PlayerStateSprinting,
		Enabled:    true,
	})
	s.applyBedrockPlayerState(intent.PlayerStateIntent{
		PlayerUUID: p.UUID,
		State:      intent.PlayerStateFlying,
		Enabled:    true,
	})
	if !p.Sprinting {
		t.Fatal(`Bedrock sprint transition was not accepted`)
	}
	if p.Flying {
		t.Fatal(`survival Bedrock player was allowed to fly`)
	}
}

func TestBedrockPlayerStateAcceptsCreativeFlight(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{13}, `bedrock-flyer`, player.ClientEditionBedrock)
	p.GameMode = player.GameModeCreative
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	s := &Server{game: g}

	s.applyBedrockPlayerState(intent.PlayerStateIntent{
		PlayerUUID: p.UUID,
		State:      intent.PlayerStateFlying,
		Enabled:    true,
	})
	if !p.Flying {
		t.Fatal(`creative Bedrock flight transition was rejected`)
	}
}

func TestBedrockBreakingLogAwardsLog(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	g := game.New()
	p := player.New([16]byte{14}, "bedrock-lumberjack", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Position = spatial.Vec3{X: 0.5, Y: 64, Z: 0.5}
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	w.SetBlock(1, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "oak_log", Properties: map[string]string{"axis": "y"}})
	s := &Server{game: g, world: w, sessions: session.NewManager()}
	s.applyBedrockBlockInteract(intent.BlockInteractIntent{
		PlayerUUID: p.UUID,
		Action:     intent.BlockActionBreak,
		Position:   spatial.BlockPos{X: 1, Y: 64, Z: 0},
	})
	if got := w.GetBlock(1, 64, 0); !got.IsAir() {
		t.Fatalf("log remained as %q", got.ResourceLocation())
	}
	for _, stack := range p.Inventory {
		if stack.ItemID == "minecraft:oak_log" && stack.Count == 1 {
			return
		}
	}
	t.Fatal("broken oak log was not awarded to Bedrock inventory")
}

func TestBedrockCanConsumeFood(t *testing.T) {
	g := game.New()
	p := player.New([16]byte{15}, "bedrock-eater", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.Food = 14
	p.Saturation = 0
	p.Inventory[player.HotbarStart] = player.ItemStack{ItemID: "minecraft:bread", Count: 2}
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	s := &Server{game: g}
	s.applyBedrockConsumeFood(intent.ConsumeFoodIntent{PlayerUUID: p.UUID, HotbarSlot: 0})
	_, food, saturation, _ := p.HealthSnapshot()
	if food != 19 || saturation != 6 {
		t.Fatalf("after eating bread = food %d saturation %.1f, want 19/6", food, saturation)
	}
	if got := p.Inventory[player.HotbarStart].Count; got != 1 {
		t.Fatalf("bread count = %d, want 1", got)
	}
}
