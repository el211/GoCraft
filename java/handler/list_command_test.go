package handler

import (
	"strings"
	"testing"

	"GoCraft/core/player"
)

func TestListCommandIncludesJavaAndBedrockPlayers(t *testing.T) {
	dispatcher := NewDispatcher()
	dispatcher.SetPlayerLister(func() []*player.Player {
		return []*player.Player{
			player.New([16]byte{1}, `Sushii4025`, player.ClientEditionBedrock),
			player.New([16]byte{2}, `NekoMochiiiii`, player.ClientEditionJava),
		}
	})
	dispatcher.SetMaxPlayers(20)
	RegisterBuiltins(dispatcher)

	var reply string
	dispatcher.Dispatch(`/list`, CommandContext{
		Player: player.New([16]byte{3}, `viewer`, player.ClientEditionBedrock),
		Reply: func(message string) error {
			reply = message
			return nil
		},
	})
	if !strings.Contains(reply, `Online (2/20): NekoMochiiiii, Sushii4025`) {
		t.Fatalf(`list reply = %q`, reply)
	}
}

func TestTimingsAndTPSAreTabCompletable(t *testing.T) {
	nodes, root, err := parseCommandTestGraph(buildCommandsPacket().Data)
	if err != nil {
		t.Fatalf(`parse Commands packet: %v`, err)
	}
	top := commandTestChildrenByName(t, nodes, nodes[root])
	for _, name := range []string{`timings`, `tps`} {
		node, ok := top[name]
		if !ok {
			t.Errorf(`top-level command %q is missing`, name)
			continue
		}
		if node.flags&commandExecutable == 0 {
			t.Errorf(`command %q is not executable`, name)
		}
	}
}
