package world

import (
	"testing"

	coreworld "GoCraft/core/world"
	dfworld "github.com/df-mc/dragonfly/server/world"
)

func TestJavaBedsAndOakDoorsMapToVisibleBedrockBlocks(t *testing.T) {
	encoder := NewEncoder()
	tests := []struct {
		block coreworld.Block
		want  string
	}{
		{coreworld.Block{Namespace: `minecraft`, Name: `red_bed`, Properties: map[string]string{`facing`: `north`, `part`: `head`, `occupied`: `false`}}, `minecraft:bed`},
		{coreworld.Block{Namespace: `minecraft`, Name: `oak_door`, Properties: map[string]string{`facing`: `east`, `half`: `lower`, `hinge`: `left`, `open`: `false`, `powered`: `false`}}, `minecraft:wooden_door`},
	}
	for _, test := range tests {
		runtimeID := encoder.BlockNetworkID(test.block)
		name, _, ok := dfworld.DefaultBlockRegistry.RuntimeIDToState(runtimeID)
		if !ok {
			t.Fatalf(`runtime ID %d is absent from the advertised palette`, runtimeID)
		}
		if name != test.want {
			t.Errorf(`%s mapped to %s, want %s`, test.block.ResourceLocation(), name, test.want)
		}
	}
}
