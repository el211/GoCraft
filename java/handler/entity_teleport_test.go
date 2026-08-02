package handler

import (
	"bytes"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	"GoCraft/java/protocol"
)

func TestEntityPositionSyncPacketsHaveProtocol769Layout(t *testing.T) {
	t.Run("player", func(t *testing.T) {
		p := player.New([16]byte{1}, "player", player.ClientEditionJava)
		p.EntityID = 42
		p.Position.X, p.Position.Y, p.Position.Z = 1.25, 64, -3.5
		p.Rotation.Yaw, p.Rotation.Pitch = 90, -15
		p.OnGround = true

		assertTeleportEntityPayload(t, buildTeleportEntity(p), 42, true)
	})

	t.Run("mob", func(t *testing.T) {
		e := corentity.New(7, [16]byte{2}, corentity.TypeCow, 4, 70, -8)
		e.VX, e.VY, e.VZ = 0.1, -0.2, 0.3
		e.Yaw, e.Pitch = 45, 10
		e.OnGround = false

		assertTeleportEntityPayload(t, buildTeleportMob(e), 7, false)
	})
}

func assertTeleportEntityPayload(t *testing.T, pkt *protocol.Packet, wantEntityID int32, wantOnGround bool) {
	t.Helper()

	if pkt.ID != packetIDTeleportEntity {
		t.Fatalf("packet ID = %d, want %d", pkt.ID, packetIDTeleportEntity)
	}

	r := pkt.Reader()
	entityID, err := protocol.ReadVarInt(r)
	if err != nil {
		t.Fatalf("read entity ID: %v", err)
	}
	if entityID != wantEntityID {
		t.Fatalf("entity ID = %d, want %d", entityID, wantEntityID)
	}

	for i := 0; i < 6; i++ {
		if _, err := protocol.ReadDouble(r); err != nil {
			t.Fatalf("read double field %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := protocol.ReadFloat(r); err != nil {
			t.Fatalf("read float field %d: %v", i, err)
		}
	}

	onGround, err := protocol.ReadBool(r)
	if err != nil {
		t.Fatalf("read on-ground flag: %v", err)
	}
	if onGround != wantOnGround {
		t.Fatalf("on-ground = %v, want %v", onGround, wantOnGround)
	}
	if r.Len() != 0 {
		t.Fatalf("unexpected trailing payload: %d bytes", r.Len())
	}
}

func TestVillagerMetadataUsesProtocol769RegistryValues(t *testing.T) {
	villager := corentity.New(77, [16]byte{3}, corentity.TypeVillager, 0, 64, 0)
	villager.VillagerVariant = corentity.VillagerVariantSavanna
	villager.VillagerProfession = corentity.VillagerProfessionLibrarian
	villager.VillagerLevel = 2

	assertVillagerMetadata := func(wantBed *spatial.BlockPos) {
		t.Helper()
		pkt := buildMobMetadata(villager)
		if pkt == nil {
			t.Fatal("buildMobMetadata(villager) = nil")
		}
		r := assertLivingEntitySleepMetadata(t, pkt, villager.EntityID, wantBed)
		index, err := protocol.ReadByte(r)
		if err != nil {
			t.Fatalf("read villager data index: %v", err)
		}
		if index != 18 {
			t.Fatalf("villager data index = %d, want 18", index)
		}
		assertMetadataVarInt(t, r, "villager data serializer ID", 19)
		assertMetadataVarInt(t, r, "savanna variant ID", 3)
		assertMetadataVarInt(t, r, "librarian profession ID", 9)
		assertMetadataVarInt(t, r, "villager level", 2)
		terminator, err := protocol.ReadByte(r)
		if err != nil {
			t.Fatalf("read metadata terminator: %v", err)
		}
		if terminator != 0xff || r.Len() != 0 {
			t.Fatalf("metadata terminator/trailing bytes = 0x%02x/%d, want 0xff/0", terminator, r.Len())
		}
	}

	assertVillagerMetadata(nil)
	villager.Sleeping = true
	villager.VillageBed = spatial.BlockPos{X: -12, Y: 68, Z: 34}
	assertVillagerMetadata(&villager.VillageBed)

	cow := corentity.New(78, [16]byte{4}, corentity.TypeCow, 0, 64, 0)
	if got := buildMobMetadata(cow); got != nil {
		t.Fatalf("buildMobMetadata(cow) = %v, want nil", got)
	}
}

func TestPlayerSleepMetadataUsesProtocol769PoseAndBedPosition(t *testing.T) {
	bed := spatial.BlockPos{X: 123, Y: 70, Z: -456}

	sleeping := buildPlayerPoseMetadata(42, true, bed)
	r := assertLivingEntitySleepMetadata(t, sleeping, 42, &bed)
	assertMetadataTerminator(t, r)

	waking := buildPlayerPoseMetadata(42, false, spatial.BlockPos{})
	r = assertLivingEntitySleepMetadata(t, waking, 42, nil)
	assertMetadataTerminator(t, r)
}

func assertLivingEntitySleepMetadata(t *testing.T, pkt *protocol.Packet, wantEntityID int32, wantBed *spatial.BlockPos) *bytes.Reader {
	t.Helper()
	if pkt.ID != packetIDSetEntityData {
		t.Fatalf("packet ID = %d, want %d", pkt.ID, packetIDSetEntityData)
	}
	r := pkt.Reader()
	assertMetadataVarInt(t, r, "entity ID", wantEntityID)

	index, err := protocol.ReadByte(r)
	if err != nil {
		t.Fatalf("read pose index: %v", err)
	}
	if index != entityMetadataPoseIndex {
		t.Fatalf("pose index = %d, want %d", index, entityMetadataPoseIndex)
	}
	assertMetadataVarInt(t, r, "pose serializer ID", metadataTypePose)
	wantPose := entityPoseStanding
	if wantBed != nil {
		wantPose = entityPoseSleeping
	}
	assertMetadataVarInt(t, r, "pose", wantPose)

	index, err = protocol.ReadByte(r)
	if err != nil {
		t.Fatalf("read sleeping position index: %v", err)
	}
	if index != livingEntityMetadataSleepingPosIndex {
		t.Fatalf("sleeping position index = %d, want %d", index, livingEntityMetadataSleepingPosIndex)
	}
	assertMetadataVarInt(t, r, "sleeping position serializer ID", metadataTypeOptionalBlockPos)
	present, err := protocol.ReadBool(r)
	if err != nil {
		t.Fatalf("read sleeping position presence: %v", err)
	}
	if present != (wantBed != nil) {
		t.Fatalf("sleeping position presence = %v, want %v", present, wantBed != nil)
	}
	if wantBed != nil {
		packed, err := protocol.ReadLong(r)
		if err != nil {
			t.Fatalf("read sleeping position: %v", err)
		}
		if got := spatial.DecodeBlockPos(packed); got != *wantBed {
			t.Fatalf("sleeping position = %v, want %v", got, *wantBed)
		}
	}
	return r
}

func assertMetadataVarInt(t *testing.T, r *bytes.Reader, name string, want int32) {
	t.Helper()
	got, err := protocol.ReadVarInt(r)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", name, got, want)
	}
}

func assertMetadataTerminator(t *testing.T, r *bytes.Reader) {
	t.Helper()
	terminator, err := protocol.ReadByte(r)
	if err != nil {
		t.Fatalf("read metadata terminator: %v", err)
	}
	if terminator != 0xff || r.Len() != 0 {
		t.Fatalf("metadata terminator/trailing bytes = 0x%02x/%d, want 0xff/0", terminator, r.Len())
	}
}
