package server

import (
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/game"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/session"
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

func TestVillagerSnapsToBedAndWakesBesideIt(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	bed := spatial.BlockPos{X: 0, Y: 64, Z: 0}
	w.SetBlock(0, 64, 0, coreworld.Block{
		Namespace: "minecraft",
		Name:      "red_bed",
		Properties: map[string]string{
			"part": "head", "facing": "south", "occupied": "false",
		},
	})
	server := &Server{world: w, worldAge: 13000, mobAIs: make(map[int32]*mobAI)}
	villager := corentity.New(9, [16]byte{}, corentity.TypeVillager, 0.5, 64, 0.4)
	villager.HasVillageHome = true
	villager.VillageBed = bed
	villager.OnGround = true

	if !server.tickPassiveMobAI(villager) || !villager.Sleeping {
		t.Fatal("villager did not enter sleeping state")
	}
	if villager.Position.X != 0.5 || villager.Position.Y != 64.6875 || villager.Position.Z != 0.5 {
		t.Fatalf("sleep position = %+v, want bed centre", villager.Position)
	}

	server.worldAge = 6000
	if !server.tickPassiveMobAI(villager) || villager.Sleeping {
		t.Fatal("villager did not wake in daytime")
	}
	if int(villager.Position.X) == 0 && int(villager.Position.Z) == 0 {
		t.Fatalf("villager woke inside bed at %+v", villager.Position)
	}
	if ok, loaded := w.CanEntityOccupyIfLoaded(villager.Position.X, villager.Position.Y, villager.Position.Z); !loaded || !ok {
		t.Fatalf("wake position is not occupiable: %+v", villager.Position)
	}
}

func TestSleepRequiresAndWakesBothEditions(t *testing.T) {
	w := coreworld.New(&coreworld.FlatGenerator{}, nil, false)
	defer w.Close()
	g := game.New()
	javaPlayer := player.New([16]byte{1}, "java", player.ClientEditionJava)
	bedrockPlayer := player.New([16]byte{2}, "bedrock", player.ClientEditionBedrock)
	javaPlayer.Sleeping = true
	bedrockPlayer.Sleeping = true
	if err := g.AddPlayer(javaPlayer); err != nil {
		t.Fatal(err)
	}
	if err := g.AddPlayer(bedrockPlayer); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		game: g, world: w, sessions: session.NewManager(),
		worldAge: 13000, sleepAllTick: 13000 - sleepAnimTicks,
	}

	s.tickSleep()

	if javaPlayer.Sleeping || bedrockPlayer.Sleeping {
		t.Fatalf("players remained asleep: java=%v bedrock=%v", javaPlayer.Sleeping, bedrockPlayer.Sleeping)
	}
	if got := s.worldAge % 24000; got != 6000 {
		t.Fatalf("time after shared sleep = %d, want 6000", got)
	}
}
