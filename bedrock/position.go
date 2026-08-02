package bedrock

import (
	"math"

	"GoCraft/core/spatial"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Bedrock sends player positions at eye height, while the canonical GoCraft
// simulation and the Java protocol use the position at the player's feet.
const (
	bedrockPlayerNetworkOffset = 1.62
	// Bedrock reserves runtime/unique ID 1 for the player controlled by the
	// current connection. Remote entities must never be assigned this ID.
	bedrockSelfRuntimeID uint64 = 1
)

func bedrockRemoteRuntimeID(entityID int32) uint64 {
	return uint64(uint32(entityID)) + bedrockSelfRuntimeID
}

func canonicalEntityID(runtimeID uint64) (int32, bool) {
	if runtimeID <= bedrockSelfRuntimeID || runtimeID > uint64(math.MaxUint32)+bedrockSelfRuntimeID {
		return 0, false
	}
	return int32(uint32(runtimeID - bedrockSelfRuntimeID)), true
}

func playerNetworkPosition(position spatial.Vec3) mgl32.Vec3 {
	return mgl32.Vec3{
		float32(position.X),
		float32(position.Y + bedrockPlayerNetworkOffset),
		float32(position.Z),
	}
}

func canonicalPlayerPosition(position mgl32.Vec3) spatial.Vec3 {
	return spatial.Vec3{
		X: float64(position.X()),
		Y: float64(position.Y()) - bedrockPlayerNetworkOffset,
		Z: float64(position.Z()),
	}
}

func initialChunkPublisher(position spatial.Vec3, radius int32) *packet.NetworkChunkPublisherUpdate {
	return &packet.NetworkChunkPublisherUpdate{
		Position: protocol.BlockPos{
			int32(math.Floor(position.X)),
			int32(math.Floor(position.Y)),
			int32(math.Floor(position.Z)),
		},
		Radius: uint32(radius) << 4,
	}
}

func chunkCoordinate(position float64) int32 {
	return int32(math.Floor(position)) >> 4
}

func chunkInsideWindow(x, z, centerX, centerZ, radius int32) bool {
	return x >= centerX-radius && x <= centerX+radius &&
		z >= centerZ-radius && z <= centerZ+radius
}
