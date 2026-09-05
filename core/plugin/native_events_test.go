package plugin

import (
	"bytes"
	"context"
	"testing"
	"time"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"

	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

func TestBlockBreakPayloadIsEditionNeutral(t *testing.T) {
	bus := NewBus(context.Background(), time.Second)
	var received *abi.Event
	instance := &fakeInstance{
		manifest: gcpkg.Manifest{
			ID: "protect",
			Subscriptions: []gcpkg.Subscription{{
				Event:       EventBlockBreak,
				Permissions: []string{"zone.trusted", "zone.build"},
			}},
		},
		dispatch: func(_ context.Context, event *abi.Event) (abi.Verdict, error) {
			received = event
			return abi.Verdict{Cancelled: true}, nil
		},
	}
	if err := bus.Attach(instance); err != nil {
		t.Fatal(err)
	}
	bus.SetPermissionResolver(func(_ *player.Player, node string) bool { return node == "zone.build" })
	identity := [16]byte{1, 2, 3}
	p := player.New(identity, "Alex", player.ClientEditionBedrock)
	block := coreworld.Block{Namespace: "minecraft", Name: "oak_log", Properties: map[string]string{
		"waterlogged": "false", "axis": "y",
	}}
	// The tool is the item id, not an ItemStack: the schema declares a string,
	// because nothing downstream reads a count or durability off it and a
	// vocabulary type nobody uses is one more thing every runtime must model.
	allowed := bus.EmitBlockBreak(p, spatial.BlockPos{X: 4, Y: 64, Z: -2}, block,
		"minecraft:iron_axe")
	if allowed || received == nil {
		t.Fatalf("allowed = %v, event = %v", allowed, received)
	}
	if received.Type != EventBlockBreak || received.OnFailure != abi.FailureAllow {
		t.Fatalf("event metadata = %+v", received)
	}
	playerFields := received.Fields[0].List
	if string(playerFields[0].Bytes) != string(identity[:]) || playerFields[1].String != "Alex" || playerFields[2].String != "bedrock" {
		t.Fatalf("player reference = %+v", playerFields)
	}
	position := received.Fields[1].List
	if position[0].Int64 != 4 || position[1].Int64 != 64 || position[2].Int64 != -2 {
		t.Fatalf("position = %+v", position)
	}
	blockFields := received.Fields[2].List
	if blockFields[0].String != "minecraft:oak_log" {
		t.Fatalf("block name = %q", blockFields[0].String)
	}
	properties := blockFields[1].List
	if properties[0].List[0].String != "axis" || properties[1].List[0].String != "waterlogged" {
		t.Fatalf("properties not sorted: %+v", properties)
	}
	if received.Fields[3].String != "minecraft:iron_axe" {
		t.Fatalf("tool = %q", received.Fields[3].String)
	}
	permissions := received.Fields[4].List
	if len(permissions) != 2 {
		t.Fatalf("permissions = %+v", permissions)
	}
	build, trusted := permissions[0].List, permissions[1].List
	if build[0].String != "zone.build" || !build[1].Bool || trusted[0].String != "zone.trusted" || trusted[1].Bool {
		t.Fatalf("resolved permissions = %+v", permissions)
	}
}

// Two shapes name a recipient, because two kinds of event do.
//
// A native event passes back the whole PlayerRef it carried. A plugin-defined
// one has no PlayerRef to pass — its author may declare primitives, a string
// and a byte slice — so a bare uuid has to work, or a subscriber cannot answer
// the player its event is about.
func TestPlayerUUIDFromReadsBothShapes(t *testing.T) {
	raw := make([]byte, 16)
	for index := range raw {
		raw[index] = byte(index)
	}
	for name, value := range map[string]abi.Value{
		"a whole PlayerRef": abi.List(abi.Bytes(raw), abi.String("oreo"), abi.String("java")),
		"a bare uuid":       abi.Bytes(raw),
	} {
		uuid, ok := PlayerUUIDFrom(value)
		if !ok {
			t.Fatalf("PlayerUUIDFrom(%s) refused it", name)
		}
		if !bytes.Equal(uuid[:], raw) {
			t.Fatalf("PlayerUUIDFrom(%s) = %x, want %x", name, uuid, raw)
		}
	}
	for name, value := range map[string]abi.Value{
		"an empty list":      abi.List(),
		"a short uuid":       abi.Bytes(raw[:8]),
		"a number":           abi.Int64(7),
		"a list of no bytes": abi.List(abi.String("oreo")),
	} {
		if _, ok := PlayerUUIDFrom(value); ok {
			t.Fatalf("PlayerUUIDFrom(%s) accepted it as a recipient", name)
		}
	}
}
