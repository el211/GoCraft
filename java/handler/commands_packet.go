package handler

// Commands packet builder for Minecraft Java Edition 1.21.4.
//
// The command graph uses vanilla Brigadier argument parsers wherever they can
// provide richer client-side completion: game_profile for online players and
// item_stack for item/block registry names. Dynamic GoCraft-only sets such as
// generated biomes, summonable mobs, villager jobs, and potion effects are
// represented as literal children so every value appears in tab completion.

import "GoCraft/java/protocol"

const (
	commandNodeRoot     byte = 0x00
	commandNodeLiteral  byte = 0x01
	commandNodeArgument byte = 0x02
	commandExecutable   byte = 0x04

	parserFloat       int32 = 1
	parserDouble      int32 = 2
	parserInteger     int32 = 3
	parserString      int32 = 5
	parserGameProfile int32 = 7
	parserItemStack   int32 = 14
)

type commandGraphNode struct {
	flags      byte
	children   []int32
	name       string
	parser     int32
	parserData func(*protocol.Builder)
}

func buildCommandsPacket() *protocol.Packet {
	nodes := []commandGraphNode{{flags: commandNodeRoot}}
	addLiteral := func(name string, executable bool, children ...int32) int32 {
		flags := commandNodeLiteral
		if executable {
			flags |= commandExecutable
		}
		index := int32(len(nodes))
		nodes = append(nodes, commandGraphNode{flags: flags, children: children, name: name})
		return index
	}
	addArgument := func(name string, parser int32, executable bool, parserData func(*protocol.Builder), children ...int32) int32 {
		flags := commandNodeArgument
		if executable {
			flags |= commandExecutable
		}
		index := int32(len(nodes))
		nodes = append(nodes, commandGraphNode{
			flags: flags, children: children, name: name,
			parser: parser, parserData: parserData,
		})
		return index
	}

	var rootChildren []int32

	modeChildren := make([]int32, 0, 4)
	for _, mode := range []string{"survival", "creative", "adventure", "spectator"} {
		modeChildren = append(modeChildren, addLiteral(mode, true))
	}
	rootChildren = append(rootChildren,
		addLiteral("gamemode", false, modeChildren...),
		addLiteral("gm", false, modeChildren...),
	)

	tpZ := addArgument("z", parserDouble, true, noNumberBounds)
	tpY := addArgument("y", parserDouble, false, noNumberBounds, tpZ)
	tpX := addArgument("x", parserDouble, false, noNumberBounds, tpY)
	tpTarget := addArgument("target", parserGameProfile, true, nil)
	rootChildren = append(rootChildren, addLiteral("tp", false, tpX, tpTarget))

	giveCount := addArgument("count", parserInteger, true, integerBounds(1, 64))
	giveItem := addArgument("item", parserItemStack, true, nil, giveCount)
	giveTarget := addArgument("player", parserGameProfile, false, nil, giveItem)
	rootChildren = append(rootChildren, addLiteral("give", false, giveTarget))

	getCount := addArgument("count", parserInteger, true, integerBounds(1, 64))
	getItem := addArgument("item", parserItemStack, true, nil, getCount)
	rootChildren = append(rootChildren, addLiteral("get", false, getItem))

	kickReason := addArgument("reason", parserString, true, stringParser(2))
	kickTarget := addArgument("player", parserGameProfile, true, nil, kickReason)
	rootChildren = append(rootChildren, addLiteral("kick", false, kickTarget))

	for _, name := range []string{"help", "list", "xyz", "version", "ver", "fly"} {
		rootChildren = append(rootChildren, addLiteral(name, true))
	}

	locateChildren := make([]int32, 0, len(locatableTargets))
	for _, target := range locatableTargets {
		locateChildren = append(locateChildren, addLiteral(target, true))
	}
	rootChildren = append(rootChildren, addLiteral("locate", false, locateChildren...))

	professionChildren := make([]int32, 0, len(villagerProfessionNames))
	for _, profession := range villagerProfessionNames {
		professionChildren = append(professionChildren, addLiteral(profession, true))
	}
	summonChildren := make([]int32, 0, len(summonableMobNames))
	for _, mob := range summonableMobNames {
		if mob == "villager" {
			summonChildren = append(summonChildren, addLiteral(mob, true, professionChildren...))
		} else {
			summonChildren = append(summonChildren, addLiteral(mob, true))
		}
	}
	rootChildren = append(rootChildren, addLiteral("summon", false, summonChildren...))

	effectSeconds := addArgument("seconds", parserInteger, true, integerBounds(1, 1_000_000))
	effectChildren := make([]int32, 0, len(potionEffectNames))
	for _, effect := range potionEffectNames {
		effectChildren = append(effectChildren, addLiteral(effect, false, effectSeconds))
	}
	effectTarget := addArgument("player", parserGameProfile, false, nil, effectChildren...)
	rootChildren = append(rootChildren, addLiteral("potioneffect", false, effectTarget))

	speedValue := func() int32 {
		return addArgument("value", parserFloat, true, floatBounds(0.001, 1))
	}
	speedReset := func() int32 { return addLiteral("reset", true) }
	for _, command := range []string{"walkspeed", "walkspeen", "flyspeed", "flyyspeed"} {
		rootChildren = append(rootChildren, addLiteral(command, false, speedValue(), speedReset()))
	}

	nodes[0].children = rootChildren

	b := protocol.NewBuilder(packetIDCommands).VarInt(int32(len(nodes)))
	for _, node := range nodes {
		b.Byte(node.flags)
		nodeChildren(b, node.children...)
		switch node.flags & 0x03 {
		case commandNodeLiteral:
			b.String(node.name)
		case commandNodeArgument:
			b.String(node.name).VarInt(node.parser)
			if node.parserData != nil {
				node.parserData(b)
			}
		}
	}
	b.VarInt(0) // root node index
	return b.Build()
}

func noNumberBounds(b *protocol.Builder) {
	b.Byte(0x00)
}

func integerBounds(minimum, maximum int32) func(*protocol.Builder) {
	return func(b *protocol.Builder) {
		b.Byte(0x03).Int(minimum).Int(maximum)
	}
}

func floatBounds(minimum, maximum float32) func(*protocol.Builder) {
	return func(b *protocol.Builder) {
		b.Byte(0x03).Float(minimum).Float(maximum)
	}
}

func stringParser(kind int32) func(*protocol.Builder) {
	return func(b *protocol.Builder) {
		b.VarInt(kind)
	}
}

func nodeChildren(b *protocol.Builder, indices ...int32) {
	b.VarInt(int32(len(indices)))
	for _, index := range indices {
		b.VarInt(index)
	}
}
