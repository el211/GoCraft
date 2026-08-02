package bedrock

import (
	"GoCraft/core/player"
	"math"
	"testing"
	"time"

	"GoCraft/core/spatial"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

func TestPlayerPositionRoundTrip(t *testing.T) {
	want := spatial.Vec3{X: -3.25, Y: 64, Z: 8.5}
	network := playerNetworkPosition(want)
	if math.Abs(float64(network.Y())-65.62) > 0.0001 {
		t.Fatalf("network Y = %f, want 65.62", network.Y())
	}

	got := canonicalPlayerPosition(network)
	if math.Abs(got.X-want.X) > 0.0001 || math.Abs(got.Y-want.Y) > 0.0001 || math.Abs(got.Z-want.Z) > 0.0001 {
		t.Fatalf("round trip position = %+v, want %+v", got, want)
	}
}

func TestCanonicalPlayerPositionSubtractsEyeHeight(t *testing.T) {
	got := canonicalPlayerPosition(mgl32.Vec3{2, 71.62, -4})
	if math.Abs(got.Y-70) > 0.0001 {
		t.Fatalf("canonical Y = %f, want 70", got.Y)
	}
}

func TestInitialChunkPublisher(t *testing.T) {
	got := initialChunkPublisher(spatial.Vec3{X: -0.25, Y: 70.9, Z: -16.01}, 4)
	wantPosition := protocol.BlockPos{-1, 70, -17}
	if got.Position != wantPosition {
		t.Fatalf("publisher position = %v, want %v", got.Position, wantPosition)
	}
	if got.Radius != 64 {
		t.Fatalf("publisher radius = %d, want 64", got.Radius)
	}
}

func TestChunkCoordinateFloorsNegativePositions(t *testing.T) {
	tests := []struct {
		position float64
		want     int32
	}{
		{position: 0, want: 0},
		{position: 15.99, want: 0},
		{position: 16, want: 1},
		{position: -0.01, want: -1},
		{position: -16, want: -1},
		{position: -16.01, want: -2},
	}
	for _, test := range tests {
		if got := chunkCoordinate(test.position); got != test.want {
			t.Errorf("chunkCoordinate(%v) = %d, want %d", test.position, got, test.want)
		}
	}
}

func TestChunkWindowDeltaSize(t *testing.T) {
	const radius int32 = 4
	entered := 0
	for x := int32(1) - radius; x <= int32(1)+radius; x++ {
		for z := -radius; z <= radius; z++ {
			if !chunkInsideWindow(x, z, 0, 0, radius) {
				entered++
			}
		}
	}
	if entered != 9 {
		t.Fatalf("new columns after moving one chunk = %d, want 9", entered)
	}
}

func TestBedrockTeleportMovementGate(t *testing.T) {
	session := &bedrockSession{}
	expected := spatial.Vec3{X: 10.5, Y: 70, Z: -4.5}
	session.expectTeleport(expected)

	if session.acceptMovement(spatial.Vec3{X: 10.5, Y: 140, Z: -4.5}) {
		t.Fatal("stale movement far from respawn was accepted")
	}
	if !session.acceptMovement(spatial.Vec3{X: 10.6, Y: 70, Z: -4.5}) {
		t.Fatal("movement at the respawn teleport was rejected")
	}
	if !session.acceptMovement(spatial.Vec3{X: 20, Y: 70, Z: -4.5}) {
		t.Fatal("movement remained gated after teleport acknowledgement")
	}
}

func TestBedrockRuntimeIDsReserveOneForSelf(t *testing.T) {
	for _, entityID := range []int32{1, 2, 42, 1000000} {
		runtimeID := bedrockRemoteRuntimeID(entityID)
		if runtimeID == bedrockSelfRuntimeID {
			t.Fatalf(`entity %d collided with the self runtime ID`, entityID)
		}
		got, ok := canonicalEntityID(runtimeID)
		if !ok || got != entityID {
			t.Fatalf(`runtime ID %d decoded as %d, %v; want %d`, runtimeID, got, ok, entityID)
		}
	}
	if _, ok := canonicalEntityID(bedrockSelfRuntimeID); ok {
		t.Fatal(`self runtime ID decoded as a remote entity`)
	}
}

func TestBedrockPlayerMetadataStartsWithFullAir(t *testing.T) {
	p := player.New([16]byte{1}, `air`, player.ClientEditionBedrock)
	metadata := bedrockPlayerMetadata(p)
	if got := metadata[protocol.EntityDataKeyAirSupply]; got != int16(300) {
		t.Fatalf(`air supply = %v, want 300`, got)
	}
	if got := metadata[protocol.EntityDataKeyAirSupplyMax]; got != int16(300) {
		t.Fatalf(`maximum air supply = %v, want 300`, got)
	}
	flags, ok := metadata[protocol.EntityDataKeyFlags].(int64)
	if !ok || flags&(int64(1)<<protocol.EntityDataFlagBreathing) == 0 {
		t.Fatal(`player metadata does not mark the player as breathing`)
	}
}

func TestBlockCrackSpeedMatchesBreakDuration(t *testing.T) {
	if got := blockCrackSpeed(1500 * time.Millisecond); got != 2184 {
		t.Fatalf(`crack speed = %d, want 2184`, got)
	}
	if got := blockCrackSpeed(0); got != 0 {
		t.Fatalf(`instant crack speed = %d, want 0`, got)
	}
}
