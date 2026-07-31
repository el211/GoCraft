package handler

import (
	"bytes"
	"testing"

	corentity "GoCraft/core/entity"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

func TestEntityHurtSoundUsesRegistryHolder(t *testing.T) {
	entity := corentity.New(42, [16]byte{}, corentity.TypeCow, 0, 64, 0)
	soundName := hurtSound(entity)
	if soundName != "minecraft:entity.cow.hurt" {
		t.Fatalf("hurt sound = %q", soundName)
	}
	packet := buildEntitySound(soundName, soundCategoryNeutral, entity.EntityID, 1, 1)
	if packet == nil {
		t.Fatal("cow hurt sound was not found in registry")
	}
	reader := bytes.NewReader(packet.Data)
	holder := mustReadSoundVarInt(t, reader)
	if want := javaworld.SoundEventID(soundName) + 1; holder != want {
		t.Fatalf("sound holder = %d, want %d", holder, want)
	}
	if category := mustReadSoundVarInt(t, reader); category != soundCategoryNeutral {
		t.Fatalf("sound category = %d", category)
	}
	if entityID := mustReadSoundVarInt(t, reader); entityID != 42 {
		t.Fatalf("sound entity = %d", entityID)
	}
}

func TestEntityMotionCarriesKnockbackVelocity(t *testing.T) {
	entity := corentity.New(7, [16]byte{}, corentity.TypePig, 0, 64, 0)
	entity.VX, entity.VY, entity.VZ = 0.25, 0.35, -0.125
	packet := buildEntityMotion(entity)
	reader := bytes.NewReader(packet.Data)
	if id := mustReadSoundVarInt(t, reader); id != entity.EntityID {
		t.Fatalf("entity ID = %d", id)
	}
	for index, want := range []int16{2000, 2800, -1000} {
		got, err := protocol.ReadShort(reader)
		if err != nil {
			t.Fatalf("velocity %d: %v", index, err)
		}
		if got != want {
			t.Fatalf("velocity %d = %d, want %d", index, got, want)
		}
	}
	if reader.Len() != 0 {
		t.Fatalf("motion packet has %d trailing bytes", reader.Len())
	}
}

func TestExperienceOreFeedbackSelection(t *testing.T) {
	for _, ore := range []string{
		"minecraft:coal_ore", "minecraft:diamond_ore",
		"minecraft:redstone_ore", "minecraft:nether_quartz_ore",
	} {
		if !rewardsExperience(ore) {
			t.Errorf("%s should provide experience pickup feedback", ore)
		}
	}
	if rewardsExperience("minecraft:iron_ore") {
		t.Error("iron ore should not provide vanilla experience pickup feedback")
	}
	if buildSoundAt("minecraft:entity.experience_orb.pickup", soundCategoryPlayers, 1, 2, 3, 0.2, 1) == nil {
		t.Fatal("experience-orb pickup sound is missing from the registry")
	}
}

func mustReadSoundVarInt(t *testing.T, reader *bytes.Reader) int32 {
	t.Helper()
	value, err := protocol.ReadVarInt(reader)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
