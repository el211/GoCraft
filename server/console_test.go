package server

import (
	`path/filepath`
	`strings`
	`testing`

	`GoCraft/core/game`
	`GoCraft/core/player`
	`GoCraft/java/handler`
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
