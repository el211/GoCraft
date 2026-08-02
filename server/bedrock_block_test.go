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
