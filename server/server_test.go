package server

import (
	"testing"

	corentity "GoCraft/core/entity"
	coreworld "GoCraft/core/world"
)

func TestPassiveMobPanicsAwayFromAttacker(t *testing.T) {
	server := &Server{mobAIs: make(map[int32]*mobAI)}
	cow := corentity.New(7, [16]byte{}, corentity.TypeCow, 10, 64, 0)
	cow.OnGround = true

	server.startPassiveMobPanic(cow, coreworld.EntityDamage{
		Amount: 1, SourceX: 0, SourceZ: 0, HasSource: true,
	})
	ai := server.mobAIs[cow.EntityID]
	if ai == nil || ai.panicTick != 60 {
		t.Fatalf("panic state = %+v, want 60 ticks", ai)
	}
	if ai.dirX <= 0 || ai.dirZ != 0 {
		t.Fatalf("panic direction = (%.2f, %.2f), want away from attacker along +X", ai.dirX, ai.dirZ)
	}
	if cow.VY != 0.4 {
		t.Fatalf("jump velocity = %.2f, want configured legacy value 0.4", cow.VY)
	}

	server.tickPassiveMobAI(cow)
	if cow.VX != 0.4 || cow.VZ != 0 {
		t.Fatalf("knockback velocity = (%.2f, %.2f), want 0.4 along +X", cow.VX, cow.VZ)
	}
	if ai.knockbackTick != 7 || ai.panicTick != 60 {
		t.Fatalf("knockback/panic ticks = %d/%d, want 7/60 after one AI tick", ai.knockbackTick, ai.panicTick)
	}
}

func TestPassiveMobPanicHasFallbackDirectionAtAttackerPosition(t *testing.T) {
	server := &Server{mobAIs: make(map[int32]*mobAI)}
	cow := corentity.New(8, [16]byte{}, corentity.TypeCow, 0, 64, 0)
	server.startPassiveMobPanic(cow, coreworld.EntityDamage{
		Amount: 1, SourceX: 0, SourceZ: 0, HasSource: true,
	})
	ai := server.mobAIs[cow.EntityID]
	if ai.dirX == 0 && ai.dirZ == 0 {
		t.Fatal("overlapping attacker produced no fallback panic direction")
	}
}
