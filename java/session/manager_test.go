package session

import (
	"testing"

	"GoCraft/core/player"
)

func TestExternalPlayerCanBeResolvedWithoutJoiningJavaBroadcasts(t *testing.T) {
	mgr := NewManager()
	external := player.New([16]byte{1}, "BedrockPlayer", player.ClientEditionBedrock)
	external.EntityID = 77
	mgr.ReplaceExternalPlayers([]*player.Player{external})

	resolved := mgr.PlayerSessionByEntityID(77)
	if resolved == nil || resolved.Player != external || resolved.Conn != nil {
		t.Fatalf("external resolution = %#v", resolved)
	}
	if got := len(mgr.SnapshotAll()); got != 0 {
		t.Fatalf("external player leaked into Java broadcast sessions: %d", got)
	}
}
