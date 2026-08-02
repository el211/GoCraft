package handler

import (
	"fmt"
	"strings"

	"GoCraft/core/player"
)

func cmdOp(ctx CommandContext) error {
	if ctx.Player == nil {
		return fmt.Errorf(`player state is unavailable`)
	}
	if len(ctx.Args) != 1 {
		return fmt.Errorf(`usage: /op <player>`)
	}

	requested := strings.TrimSpace(ctx.Args[0])
	name := requested
	if requested == `@s` {
		name = ctx.Player.Username
	}
	if !ctx.Player.Operator {
		if OperatorCount() != 0 {
			return fmt.Errorf(`you do not have permission to use this command`)
		}
		if !strings.EqualFold(name, ctx.Player.Username) {
			return fmt.Errorf(`the first operator must bootstrap with /op @s`)
		}
	}

	if err := SetOperator(name); err != nil {
		return fmt.Errorf(`saving ops.json: %w`, err)
	}
	target := findCanonicalPlayer(ctx, name)
	if target != nil {
		target.Operator = true
		if ctx.SyncAbilities != nil {
			ctx.SyncAbilities(target)
		}
	}
	return sendCommandMessage(ctx, fmt.Sprintf(`Made %s a server operator`, name))
}

func cmdGod(ctx CommandContext) error {
	return setGodMode(ctx, true)
}

func cmdUngod(ctx CommandContext) error {
	return setGodMode(ctx, false)
}

func setGodMode(ctx CommandContext, enabled bool) error {
	if ctx.Player == nil {
		return fmt.Errorf(`player state is unavailable`)
	}
	if len(ctx.Args) > 1 {
		if enabled {
			return fmt.Errorf(`usage: /god [player]`)
		}
		return fmt.Errorf(`usage: /ungod [player]`)
	}

	target := ctx.Player
	if len(ctx.Args) == 1 {
		target = findCanonicalPlayer(ctx, ctx.Args[0])
		if target == nil {
			return fmt.Errorf(`player not found: %s`, ctx.Args[0])
		}
	}
	target.GodMode = enabled
	if ctx.SyncAbilities != nil {
		ctx.SyncAbilities(target)
	}
	state := `disabled`
	if enabled {
		state = `enabled`
	}
	return sendCommandMessage(ctx, fmt.Sprintf(`God mode %s for %s`, state, target.Username))
}

func findCanonicalPlayer(ctx CommandContext, name string) *player.Player {
	if name == `@s` || (ctx.Player != nil && strings.EqualFold(name, ctx.Player.Username)) {
		return ctx.Player
	}
	if ctx.FindPlayer != nil {
		return ctx.FindPlayer(name)
	}
	if ctx.Manager != nil {
		for _, candidate := range ctx.Manager.SnapshotAll() {
			if strings.EqualFold(candidate.Player.Username, name) {
				return candidate.Player
			}
		}
	}
	return nil
}
