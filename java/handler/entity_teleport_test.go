package handler

import (
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	"GoCraft/java/protocol"
)

func TestTeleportEntityPacketsIncludeRelativeFlags(t *testing.T) {
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

	flags, err := protocol.ReadInt(r)
	if err != nil {
		t.Fatalf("read relative flags: %v", err)
	}
	if flags != 0 {
		t.Fatalf("relative flags = %d, want 0", flags)
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

	pkt := buildMobMetadata(villager)
	if pkt == nil {
		t.Fatal("buildMobMetadata(villager) = nil")
	}
	if pkt.ID != packetIDSetEntityData {
		t.Fatalf("packet ID = %d, want %d", pkt.ID, packetIDSetEntityData)
	}
	r := pkt.Reader()
	assertVarInt := func(name string, want int32) {
		t.Helper()
		got, err := protocol.ReadVarInt(r)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	assertVarInt("entity ID", 77)
	index, err := protocol.ReadByte(r)
	if err != nil {
		t.Fatalf("read metadata index: %v", err)
	}
	if index != 18 {
		t.Fatalf("metadata index = %d, want 18", index)
	}
	assertVarInt("serializer ID", 19)
	assertVarInt("savanna variant ID", 3)
	assertVarInt("librarian profession ID", 9)
	assertVarInt("villager level", 2)
	terminator, err := protocol.ReadByte(r)
	if err != nil {
		t.Fatalf("read metadata terminator: %v", err)
	}
	if terminator != 0xff || r.Len() != 0 {
		t.Fatalf("metadata terminator/trailing bytes = 0x%02x/%d, want 0xff/0", terminator, r.Len())
	}

	cow := corentity.New(78, [16]byte{4}, corentity.TypeCow, 0, 64, 0)
	if got := buildMobMetadata(cow); got != nil {
		t.Fatalf("buildMobMetadata(cow) = %v, want nil", got)
	}
}
