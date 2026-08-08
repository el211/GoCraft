package bedrock

import (
	"bytes"
	"testing"

	"GoCraft/core/player"
	"GoCraft/core/spatial"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
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

func TestCrossEditionFallbackSkinKeepsValidGeometry(t *testing.T) {
	source := protocol.Skin{
		SkinID:            "bedrock-owner",
		PlayFabID:         "playfab-owner",
		SkinResourcePatch: []byte(`{"geometry":{"default":"geometry.humanoid.custom"}}`),
		SkinGeometry:      []byte(`{"format_version":"1.12.0","minecraft:geometry":[]}`),
		SkinData:          []byte{1, 2, 3, 4},
		PrimaryUser:       true,
	}
	id := [16]byte{9, 8, 7}
	got := crossEditionFallbackSkin(source, id)
	if got.SkinID == source.SkinID || got.FullID == "" {
		t.Fatal("fallback skin retained the viewing player's identity")
	}
	if got.PlayFabID != "" || got.PrimaryUser {
		t.Fatal("fallback skin retained owner-only account fields")
	}
	if !bytes.Equal(got.SkinGeometry, source.SkinGeometry) ||
		!bytes.Equal(got.SkinResourcePatch, source.SkinResourcePatch) {
		t.Fatal("fallback skin lost its validated geometry")
	}
	if !got.Trusted || !got.OverrideAppearance {
		t.Fatal("fallback skin is not marked as trusted/visible")
	}
}
