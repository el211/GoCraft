package bedrock

import (
	"testing"
	"time"

	bedrockworld "GoCraft/bedrock/world"
	"GoCraft/core/game"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

func TestOakLogUsesRegistryBreakDuration(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	g := game.New()
	p := player.New([16]byte{21}, "lumberjack", player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	p.OnGround = true
	p.Position = spatial.Vec3{X: 0.5, Y: 64, Z: 0.5}
	if err := g.AddPlayer(p); err != nil {
		t.Fatal(err)
	}
	w.SetBlock(1, 64, 0, coreworld.Block{Namespace: "minecraft", Name: "oak_log", Properties: map[string]string{"axis": "y"}})
	l := &Listener{world: w, game: g, encoder: bedrockworld.NewEncoder()}
	duration := l.blockBreakDuration(p.UUID, protocol.BlockPos{1, 64, 0})
	if duration <= 750*time.Millisecond || duration > 10*time.Second {
		t.Fatalf("oak log break duration = %v, want actual registry duration", duration)
	}
}
