package bedrock

import (
	"testing"

	"GoCraft/core/player"
	"GoCraft/core/spatial"
)

func TestBedrockPlayerVisibilityFollowsLoadedChunkWindow(t *testing.T) {
	viewer := player.New([16]byte{1}, "viewer", player.ClientEditionBedrock)
	target := player.New([16]byte{2}, "target", player.ClientEditionJava)
	viewer.Position = spatial.Vec3{X: 15, Y: 64, Z: 15}
	target.Position = spatial.Vec3{X: 79, Y: 64, Z: 79} // four chunks away
	if !bedrockPlayerInView(viewer, target) {
		t.Fatal("player inside the loaded chunk window was hidden")
	}
	target.Position.X = 80 // five chunks away from viewer chunk zero
	if bedrockPlayerInView(viewer, target) {
		t.Fatal("player outside the loaded chunk window was published too early")
	}
	target.Position = spatial.Vec3{X: -1, Y: 64, Z: -1}
	if !bedrockPlayerInView(viewer, target) {
		t.Fatal("negative adjacent chunk was not handled with floor coordinates")
	}
}
