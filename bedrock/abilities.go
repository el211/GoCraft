package bedrock

import (
	"GoCraft/core/player"
	"math"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// AbilityLayer.WalkSpeed is an absolute Bedrock ability value, not a scalar.
// Keep it at the protocol's vanilla 0.1; using 1.0 causes distorted FOV and
// movement behaviour even when minecraft:movement is otherwise correct.
const bedrockAbilityWalkSpeed = protocol.AbilityBaseWalkSpeed

func bedrockGameType(mode player.GameMode) int32 {
	switch mode {
	case player.GameModeCreative:
		return packet.GameTypeCreative
	case player.GameModeAdventure:
		return packet.GameTypeAdventure
	case player.GameModeSpectator:
		return packet.GameTypeSurvivalSpectator
	default:
		return packet.GameTypeSurvival
	}
}

func bedrockMovementAttribute(p *player.Player) protocol.Attribute {
	speed := p.WalkSpeed
	if speed <= 0 {
		speed = protocol.AbilityBaseWalkSpeed
	}
	return protocol.Attribute{
		AttributeValue: protocol.AttributeValue{
			Name:  `minecraft:movement`,
			Min:   0,
			Max:   math.MaxFloat32,
			Value: speed,
		},
		DefaultMin: 0,
		DefaultMax: math.MaxFloat32,
		Default:    protocol.AbilityBaseWalkSpeed,
	}
}

func bedrockAbilityData(p *player.Player) protocol.AbilityData {
	var values uint32
	if p.GameMode != player.GameModeSpectator {
		values |= protocol.AbilityDoorsAndSwitches |
			protocol.AbilityOpenContainers |
			protocol.AbilityAttackPlayers |
			protocol.AbilityAttackMobs
	}
	if p.GameMode == player.GameModeSurvival || p.GameMode == player.GameModeCreative {
		values |= protocol.AbilityBuild | protocol.AbilityMine
	}

	mayFly := p.AllowFlying || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator
	if mayFly {
		values |= protocol.AbilityMayFly
		if p.Flying || p.GameMode == player.GameModeSpectator {
			values |= protocol.AbilityFlying
		}
	}
	if p.GameMode == player.GameModeCreative || p.GodMode {
		values |= protocol.AbilityInvulnerable | protocol.AbilityInstantBuild
		if p.GameMode != player.GameModeCreative {
			values &^= protocol.AbilityInstantBuild
		}
	}
	if p.GameMode == player.GameModeSpectator {
		values |= protocol.AbilityInvulnerable | protocol.AbilityNoClip
	}

	flySpeed := p.FlySpeed
	if flySpeed <= 0 {
		flySpeed = protocol.AbilityBaseFlySpeed
	}
	permission := uint8(packet.PermissionLevelMember)
	commandPermission := uint8(protocol.CommandPermissionLevelAny)
	if p.Operator {
		permission = packet.PermissionLevelOperator
		commandPermission = protocol.CommandPermissionLevelAdmin
	}
	return protocol.AbilityData{
		EntityUniqueID:     int64(bedrockRemoteRuntimeID(p.EntityID)),
		PlayerPermissions:  permission,
		CommandPermissions: commandPermission,
		Layers: []protocol.AbilityLayer{{
			Type:             protocol.AbilityLayerTypeBase,
			Abilities:        protocol.AbilityCount - 1,
			Values:           values,
			FlySpeed:         flySpeed,
			VerticalFlySpeed: protocol.AbilityBaseVerticalFlySpeed,
			WalkSpeed:        bedrockAbilityWalkSpeed,
		}},
	}
}

func bedrockSelfAbilityData(p *player.Player) protocol.AbilityData {
	data := bedrockAbilityData(p)
	data.EntityUniqueID = int64(bedrockSelfRuntimeID)
	return data
}

func bedrockAdventureSettings(p *player.Player) *packet.UpdateAdventureSettings {
	immutable := p.GameMode == player.GameModeAdventure || p.GameMode == player.GameModeSpectator
	invulnerable := p.GodMode || p.GameMode == player.GameModeCreative || p.GameMode == player.GameModeSpectator
	return &packet.UpdateAdventureSettings{
		NoPvM:          p.GameMode == player.GameModeSpectator,
		NoMvP:          invulnerable,
		ImmutableWorld: immutable,
		ShowNameTags:   true,
		AutoJump:       true,
	}
}
