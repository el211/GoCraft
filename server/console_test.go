package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GoCraft/config"
	"GoCraft/core/game"
	"GoCraft/core/player"
	"GoCraft/java/handler"
)

func TestConsoleOpPromotesOnlinePlayerAndPersists(t *testing.T) {
	tempDir := t.TempDir()
	opsPath := filepath.Join(tempDir, `ops.json`)
	if err := handler.ConfigureOperators(opsPath, nil); err != nil {
		t.Fatalf(`configure operators: %v`, err)
	}
	t.Cleanup(func() {
		_ = handler.ConfigureOperators(filepath.Join(tempDir, `reset-ops.json`), nil)
	})

	gameCore := game.New()
	p := player.New([16]byte{1}, `Sushii4025`, player.ClientEditionBedrock)
	if err := gameCore.AddPlayer(p); err != nil {
		t.Fatalf(`add player: %v`, err)
	}
	s := &Server{game: gameCore}
	result := s.executeConsoleCommand(`op Sushii4025`)
	if !p.Operator {
		t.Fatal(`console op did not promote the online player`)
	}
	if !handler.IsOperatorName(`sushii4025`) {
		t.Fatal(`console op did not persist the player name`)
	}
	if !strings.Contains(result, `Sushii4025`) {
		t.Fatalf(`console result = %q`, result)
	}
}

func TestConsoleListAndTimingsIncludeCrossEditionServerStats(t *testing.T) {
	gameCore := game.New()
	for _, online := range []*player.Player{
		player.New([16]byte{1}, `Sushii4025`, player.ClientEditionBedrock),
		player.New([16]byte{2}, `NekoMochiiiii`, player.ClientEditionJava),
	} {
		if err := gameCore.AddPlayer(online); err != nil {
			t.Fatalf(`add player: %v`, err)
		}
	}
	timings := newTickTimings(func() (int, int) { return gameCore.OnlineCount(), 20 })
	timings.commit(5 * time.Millisecond)
	s := &Server{
		cfg:     &config.Config{MaxPlayers: 20},
		game:    gameCore,
		timings: timings,
	}

	list := s.executeConsoleCommand(`/list`)
	if !strings.Contains(list, `Online (2/20): NekoMochiiiii, Sushii4025`) {
		t.Fatalf(`console list = %q`, list)
	}
	report := s.executeConsoleCommand(`timings`)
	for _, expected := range []string{`RAM:`, `CPU:`, `Players: 2/20`} {
		if !strings.Contains(report, expected) {
			t.Errorf(`console timings does not contain %q: %s`, expected, report)
		}
	}
	if strings.ContainsRune(report, '§') {
		t.Fatalf(`console timings contains Minecraft formatting: %q`, report)
	}
}
