package handler

import (
	`testing`

	`GoCraft/core/player`
	`GoCraft/java/session`
)

func TestAdministrativeCommandsRequireOperator(t *testing.T) {
	dispatcher := NewDispatcher()
	RegisterBuiltins(dispatcher)
	for _, name := range []string{`gamemode`, `tp`, `give`, `kill`, `fly`, `god`, `ungod`, `heal`, `effect`} {
		command, ok := dispatcher.cmds[name]
		if !ok {
			t.Errorf(`administrative command %q is not registered`, name)
			continue
		}
		if !command.operatorOnly {
			t.Errorf(`administrative command %q is not operator-only`, name)
		}
	}
	for _, name := range []string{`help`, `list`, `xyz`, `version`, `op`} {
		command, ok := dispatcher.cmds[name]
		if !ok {
			t.Errorf(`public command %q is not registered`, name)
			continue
		}
		if command.operatorOnly {
			t.Errorf(`bootstrap/public command %q unexpectedly requires operator`, name)
		}
	}
}

func TestGodModeBlocksNormalDamageButKillOverridesIt(t *testing.T) {
	p := player.New([16]byte{1}, `invincible`, player.ClientEditionJava)
	p.GodMode = true
	target := &session.Session{Player: p}
	if DamagePlayer(target, 5, `was tested`, nil) {
		t.Fatal(`normal damage was applied while god mode was enabled`)
	}
	if health, _, _, _ := p.HealthSnapshot(); health != p.MaxHealth {
		t.Fatalf(`god-mode health = %v, want %v`, health, p.MaxHealth)
	}
	if !KillPlayer(target, `was killed`, nil) {
		t.Fatal(`/kill did not override god mode`)
	}
}
