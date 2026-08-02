package bedrock

import (
	"testing"

	"GoCraft/core/player"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestBedrockSurvivalAbilitiesPermitNormalActions(t *testing.T) {
	p := player.New([16]byte{1}, `survival`, player.ClientEditionBedrock)
	p.GameMode = player.GameModeSurvival
	data := bedrockAbilityData(p)
	values := data.Layers[0].Values
	want := uint32(protocol.AbilityBuild + protocol.AbilityMine + protocol.AbilityDoorsAndSwitches +
		protocol.AbilityOpenContainers + protocol.AbilityAttackPlayers + protocol.AbilityAttackMobs)
	if values != want {
		t.Fatal(`survival abilities do not permit every normal player action`)
	}
	if data.PlayerPermissions != packet.PermissionLevelMember {
		t.Fatal(`survival player was not assigned member permissions`)
	}
}

func TestBedrockCreativeAbilitiesPermitFlight(t *testing.T) {
	p := player.New([16]byte{1}, `creative`, player.ClientEditionBedrock)
	p.GameMode = player.GameModeCreative
	p.Flying = true
	values := bedrockAbilityData(p).Layers[0].Values
	want := uint32(protocol.AbilityBuild + protocol.AbilityMine + protocol.AbilityDoorsAndSwitches +
		protocol.AbilityOpenContainers + protocol.AbilityAttackPlayers + protocol.AbilityAttackMobs +
		protocol.AbilityMayFly + protocol.AbilityFlying + protocol.AbilityInvulnerable + protocol.AbilityInstantBuild)
	if values != want {
		t.Fatal(`creative abilities do not permit editing and flight`)
	}
}

func TestBedrockAdventureAndSpectatorRestrictions(t *testing.T) {
	p := player.New([16]byte{1}, `player`, player.ClientEditionBedrock)
	p.GameMode = player.GameModeAdventure
	adventureValues := bedrockAbilityData(p).Layers[0].Values
	interactionOnly := uint32(protocol.AbilityDoorsAndSwitches + protocol.AbilityOpenContainers +
		protocol.AbilityAttackPlayers + protocol.AbilityAttackMobs)
	if adventureValues != interactionOnly {
		t.Fatal(`adventure abilities unexpectedly permit editing`)
	}

	p.GameMode = player.GameModeSpectator
	spectatorValues := bedrockAbilityData(p).Layers[0].Values
	wantSpectator := uint32(protocol.AbilityMayFly + protocol.AbilityFlying +
		protocol.AbilityNoClip + protocol.AbilityInvulnerable)
	if spectatorValues != wantSpectator {
		t.Fatal(`spectator abilities do not permit invulnerable no-clip flight`)
	}
}

func TestBedrockSelfAbilitiesUseReservedEntityID(t *testing.T) {
	p := player.New([16]byte{1}, `self`, player.ClientEditionBedrock)
	p.EntityID = 42
	if got := bedrockSelfAbilityData(p).EntityUniqueID; got != int64(bedrockSelfRuntimeID) {
		t.Fatalf(`self ability entity ID = %d, want %d`, got, bedrockSelfRuntimeID)
	}
	if got := bedrockAbilityData(p).EntityUniqueID; got != int64(bedrockRemoteRuntimeID(p.EntityID)) {
		t.Fatalf(`remote ability entity ID = %d, want %d`, got, bedrockRemoteRuntimeID(p.EntityID))
	}
}

func TestBedrockMovementAttributeUsesVanillaSpeed(t *testing.T) {
	p := player.New([16]byte{1}, `movement`, player.ClientEditionBedrock)
	attribute := bedrockMovementAttribute(p)
	if attribute.Name != `minecraft:movement` {
		t.Fatalf(`movement attribute name = %q`, attribute.Name)
	}
	if attribute.Value != protocol.AbilityBaseWalkSpeed || attribute.Default != protocol.AbilityBaseWalkSpeed {
		t.Fatalf(`movement speed = %v/%v, want %v`, attribute.Value, attribute.Default, protocol.AbilityBaseWalkSpeed)
	}
	if got := bedrockAbilityData(p).Layers[0].WalkSpeed; got != bedrockWalkSpeedMultiplier {
		t.Fatalf(`ability walk multiplier = %v, want %v`, got, bedrockWalkSpeedMultiplier)
	}
}
