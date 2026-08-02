package handler

import (
	"bytes"
	"fmt"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/java/protocol"
)

type commandTestNode struct {
	flags    byte
	name     string
	children []int32
}

func TestBuiltinsRegisterNavigationVersionAndSummonCommands(t *testing.T) {
	dispatcher := NewDispatcher()
	RegisterBuiltins(dispatcher)

	for _, name := range []string{
		"gm", "tp", "xyz", "locate", "summon", "version", "ver",
		"give", "get", "fly", "potioneffect", "walkspeed", "flyspeed",
	} {
		if _, ok := dispatcher.cmds[name]; !ok {
			t.Errorf("command %q was not registered", name)
		}
	}
	if goCraftVersion != "GoCraft 1.21.4" {
		t.Fatalf("version response = %q", goCraftVersion)
	}
}

func TestEverySummonCompletionBuildsAJavaSpawnPacket(t *testing.T) {
	for _, name := range summonableMobNames {
		entity := corentity.New(1, [16]byte{}, corentity.EntityType("minecraft:"+name), 0, 64, 0)
		if _, ok := buildSpawnMob(entity); !ok {
			t.Errorf("summon completion %q has no Java 1.21.4 entity type", name)
		}
	}
}

func TestCommandsPacketTabCompletesLocateSummonAndVersion(t *testing.T) {
	nodes, root, err := parseCommandTestGraph(buildCommandsPacket().Data)
	if err != nil {
		t.Fatalf("parse Commands packet: %v", err)
	}
	if root != 0 {
		t.Fatalf("root index = %d, want 0", root)
	}

	top := commandTestChildrenByName(t, nodes, nodes[root])
	for _, name := range []string{
		"gm", "tp", "xyz", "locate", "summon", "version", "ver",
		"give", "get", "fly", "potioneffect", "walkspeed", "flyspeed",
	} {
		if _, ok := top[name]; !ok {
			t.Errorf("top-level command %q is missing from tab completion", name)
		}
	}

	locate := top["locate"]
	locateChildren := commandTestChildrenByName(t, nodes, locate)
	if len(locateChildren) != len(locatableTargets) {
		t.Fatalf("locate target count = %d, want %d", len(locateChildren), len(locatableTargets))
	}
	for _, target := range locatableTargets {
		node, ok := locateChildren[target]
		if !ok {
			t.Errorf("locate target %q is missing", target)
		} else if node.flags&0x04 == 0 {
			t.Errorf("locate target %q is not executable", target)
		}
	}

	summon := top["summon"]
	summonChildren := commandTestChildrenByName(t, nodes, summon)
	if len(summonChildren) != len(summonableMobNames) {
		t.Fatalf("summon target count = %d, want %d", len(summonChildren), len(summonableMobNames))
	}
	for _, mob := range summonableMobNames {
		if _, ok := summonChildren[mob]; !ok {
			t.Errorf("summon target %q is missing", mob)
		}
	}

	villager := summonChildren["villager"]
	if villager.flags&0x04 == 0 {
		t.Error("/summon villager is not executable without a profession")
	}
	professions := commandTestChildrenByName(t, nodes, villager)
	if len(professions) != len(villagerProfessionNames) {
		t.Fatalf("villager profession count = %d, want %d", len(professions), len(villagerProfessionNames))
	}
	for _, profession := range villagerProfessionNames {
		node, ok := professions[profession]
		if !ok {
			t.Errorf("villager profession %q is missing", profession)
		} else if node.flags&0x04 == 0 {
			t.Errorf("villager profession %q is not executable", profession)
		}
	}

	giveTarget := commandTestChildrenByName(t, nodes, top["give"])["player"]
	giveItem := commandTestChildrenByName(t, nodes, giveTarget)["item"]
	if giveItem.flags&0x04 == 0 {
		t.Error("/give <player> <item> is not executable without a count")
	}
	if _, ok := commandTestChildrenByName(t, nodes, giveItem)["count"]; !ok {
		t.Error("/give item count completion is missing")
	}

	effectTarget := commandTestChildrenByName(t, nodes, top["potioneffect"])["player"]
	effects := commandTestChildrenByName(t, nodes, effectTarget)
	if len(effects) != len(potionEffectNames) {
		t.Fatalf("potion effect completion count = %d, want %d", len(effects), len(potionEffectNames))
	}
	if len(potionEffectNames) > 0 {
		seconds := commandTestChildrenByName(t, nodes, effects[potionEffectNames[0]])
		if _, ok := seconds["seconds"]; !ok {
			t.Error("potion effect seconds completion is missing")
		}
	}

	for _, speedCommand := range []string{"walkspeed", "flyspeed"} {
		children := commandTestChildrenByName(t, nodes, top[speedCommand])
		if _, ok := children["reset"]; !ok {
			t.Errorf("/%s reset completion is missing", speedCommand)
		}
		if _, ok := children["value"]; !ok {
			t.Errorf("/%s value argument is missing", speedCommand)
		}
	}
}

func commandTestChildrenByName(t *testing.T, nodes []commandTestNode, parent commandTestNode) map[string]commandTestNode {
	t.Helper()
	children := make(map[string]commandTestNode, len(parent.children))
	for _, index := range parent.children {
		if index < 0 || int(index) >= len(nodes) {
			t.Fatalf("child index %d is outside node count %d", index, len(nodes))
		}
		child := nodes[index]
		if child.name == "" {
			t.Fatalf("child index %d has no command name", index)
		}
		if _, duplicate := children[child.name]; duplicate {
			t.Fatalf("duplicate child name %q", child.name)
		}
		children[child.name] = child
	}
	return children
}

func parseCommandTestGraph(data []byte) ([]commandTestNode, int32, error) {
	reader := bytes.NewReader(data)
	count, err := protocol.ReadVarInt(reader)
	if err != nil {
		return nil, 0, err
	}
	if count < 1 {
		return nil, 0, fmt.Errorf("invalid node count %d", count)
	}

	nodes := make([]commandTestNode, count)
	for index := range nodes {
		flags, err := protocol.ReadByte(reader)
		if err != nil {
			return nil, 0, fmt.Errorf("node %d flags: %w", index, err)
		}
		childCount, err := protocol.ReadVarInt(reader)
		if err != nil {
			return nil, 0, fmt.Errorf("node %d child count: %w", index, err)
		}
		if childCount < 0 {
			return nil, 0, fmt.Errorf("node %d has negative child count", index)
		}
		children := make([]int32, childCount)
		for childIndex := range children {
			children[childIndex], err = protocol.ReadVarInt(reader)
			if err != nil {
				return nil, 0, fmt.Errorf("node %d child %d: %w", index, childIndex, err)
			}
			if children[childIndex] < 0 || children[childIndex] >= count {
				return nil, 0, fmt.Errorf("node %d references invalid child %d", index, children[childIndex])
			}
		}
		if flags&0x08 != 0 {
			if _, err := protocol.ReadVarInt(reader); err != nil {
				return nil, 0, fmt.Errorf("node %d redirect: %w", index, err)
			}
		}

		node := commandTestNode{flags: flags, children: children}
		switch flags & 0x03 {
		case 0x00:
			// Root node has no name or parser.
		case 0x01:
			node.name, err = protocol.ReadString(reader)
		case 0x02:
			node.name, err = protocol.ReadString(reader)
			if err == nil {
				err = skipCommandTestParser(reader)
			}
		default:
			err = fmt.Errorf("invalid node type")
		}
		if err != nil {
			return nil, 0, fmt.Errorf("node %d: %w", index, err)
		}
		if flags&0x10 != 0 {
			if _, err := protocol.ReadString(reader); err != nil {
				return nil, 0, fmt.Errorf("node %d suggestions: %w", index, err)
			}
		}
		nodes[index] = node
	}

	root, err := protocol.ReadVarInt(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("root index: %w", err)
	}
	if root < 0 || root >= count {
		return nil, 0, fmt.Errorf("invalid root index %d", root)
	}
	if reader.Len() != 0 {
		return nil, 0, fmt.Errorf("%d trailing packet bytes", reader.Len())
	}
	return nodes, root, nil
}

func skipCommandTestParser(reader *bytes.Reader) error {
	parserID, err := protocol.ReadVarInt(reader)
	if err != nil {
		return err
	}
	switch parserID {
	case 1: // brigadier:float
		properties, err := protocol.ReadByte(reader)
		if err != nil {
			return err
		}
		if properties&0x01 != 0 {
			if _, err := protocol.ReadFloat(reader); err != nil {
				return err
			}
		}
		if properties&0x02 != 0 {
			if _, err := protocol.ReadFloat(reader); err != nil {
				return err
			}
		}
	case 2: // brigadier:double
		properties, err := protocol.ReadByte(reader)
		if err != nil {
			return err
		}
		if properties&0x01 != 0 {
			if _, err := protocol.ReadDouble(reader); err != nil {
				return err
			}
		}
		if properties&0x02 != 0 {
			if _, err := protocol.ReadDouble(reader); err != nil {
				return err
			}
		}
	case 3: // brigadier:integer
		properties, err := protocol.ReadByte(reader)
		if err != nil {
			return err
		}
		if properties&0x01 != 0 {
			if _, err := protocol.ReadInt(reader); err != nil {
				return err
			}
		}
		if properties&0x02 != 0 {
			if _, err := protocol.ReadInt(reader); err != nil {
				return err
			}
		}
	case 5: // brigadier:string
		_, err = protocol.ReadVarInt(reader)
	case 7, 14: // minecraft:game_profile, minecraft:item_stack
		// These parsers have no extra command-node properties.
	default:
		return fmt.Errorf("unsupported parser ID %d", parserID)
	}
	return err
}

func TestCommandArgumentValidation(t *testing.T) {
	if got := normalizeResourceLocation("Stone"); got != "minecraft:stone" {
		t.Fatalf("normalized item = %q", got)
	}
	if count, err := parseGiveCount([]string{"64"}); err != nil || count != 64 {
		t.Fatalf("give count = %d, err=%v", count, err)
	}
	if _, err := parseGiveCount([]string{"65"}); err == nil {
		t.Fatal("give count above one stack was accepted")
	}
	if speed, err := parseSpeedArgument([]string{"reset"}, 0.1, "/walkspeed"); err != nil || speed != 0.1 {
		t.Fatalf("reset speed = %v, err=%v", speed, err)
	}
	if _, err := parseSpeedArgument([]string{"2"}, 0.1, "/walkspeed"); err == nil {
		t.Fatal("out-of-range speed was accepted")
	}
}

func TestHealAndEffectCommandsAreRegisteredAndCompletable(t *testing.T) {
	dispatcher := NewDispatcher()
	RegisterBuiltins(dispatcher)
	for _, name := range []string{`heal`, `effect`} {
		if _, ok := dispatcher.cmds[name]; !ok {
			t.Errorf(`command %q was not registered`, name)
		}
	}
	nodes, root, err := parseCommandTestGraph(buildCommandsPacket().Data)
	if err != nil {
		t.Fatalf(`parse Commands packet: %v`, err)
	}
	top := commandTestChildrenByName(t, nodes, nodes[root])
	if top[`heal`].flags&commandExecutable == 0 {
		t.Error(`/heal is not executable for the issuing player`)
	}
	if _, ok := commandTestChildrenByName(t, nodes, top[`heal`])[`player`]; !ok {
		t.Error(`/heal player target completion is missing`)
	}
	effectTarget := commandTestChildrenByName(t, nodes, top[`effect`])[`player`]
	effects := commandTestChildrenByName(t, nodes, effectTarget)
	if len(effects) != len(potionEffectNames) {
		t.Fatalf(`/effect completion count = %d, want %d`, len(effects), len(potionEffectNames))
	}
}
