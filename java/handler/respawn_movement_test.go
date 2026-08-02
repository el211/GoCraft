package handler

import "testing"

func TestRespawnTeleportGateRecognisesMovementPackets(t *testing.T) {
	movement := []int32{
		packetIDSetPlayerPosition,
		packetIDSetPlayerPositionAndRotation,
		packetIDSetPlayerRotation,
		packetIDSetPlayerOnGround,
	}
	for _, packetID := range movement {
		if !isPlayerMovementPacket(packetID) {
			t.Errorf("packet %d was not recognised as movement", packetID)
		}
	}
	if isPlayerMovementPacket(packetIDClientCommand) {
		t.Fatal("client command was incorrectly recognised as movement")
	}
}
