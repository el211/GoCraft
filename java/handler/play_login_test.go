package handler

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"GoCraft/core/player"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	"GoCraft/java/registry"
)

type loginTraceField struct {
	Start int
	End   int
	Name  string
	Type  string
	Value any
}

type loginTraceReader struct {
	data   []byte
	offset int
	fields []loginTraceField
}

func (r *loginTraceReader) require(name, wireType string, size int) error {
	if size < 0 || r.offset+size > len(r.data) {
		return fmt.Errorf("field %q (%s) at offset %d needs %d bytes, only %d remain", name, wireType, r.offset, size, len(r.data)-r.offset)
	}
	return nil
}

func (r *loginTraceReader) record(start int, name, wireType string, value any) {
	r.fields = append(r.fields, loginTraceField{Start: start, End: r.offset, Name: name, Type: wireType, Value: value})
}

func (r *loginTraceReader) readI32(name string) (int32, error) {
	start := r.offset
	if err := r.require(name, "i32", 4); err != nil {
		return 0, err
	}
	value := int32(binary.BigEndian.Uint32(r.data[r.offset : r.offset+4]))
	r.offset += 4
	r.record(start, name, "i32", value)
	return value, nil
}

func (r *loginTraceReader) readI64(name string) (int64, error) {
	start := r.offset
	if err := r.require(name, "i64", 8); err != nil {
		return 0, err
	}
	value := int64(binary.BigEndian.Uint64(r.data[r.offset : r.offset+8]))
	r.offset += 8
	r.record(start, name, "i64", value)
	return value, nil
}

func (r *loginTraceReader) readByte(name, wireType string) (byte, error) {
	start := r.offset
	if err := r.require(name, wireType, 1); err != nil {
		return 0, err
	}
	value := r.data[r.offset]
	r.offset++
	r.record(start, name, wireType, value)
	return value, nil
}

func (r *loginTraceReader) readBool(name string) (bool, error) {
	start := r.offset
	if err := r.require(name, "bool", 1); err != nil {
		return false, err
	}
	raw := r.data[r.offset]
	if raw > 1 {
		return false, fmt.Errorf("field %q (bool) at offset %d has invalid byte 0x%02x", name, r.offset, raw)
	}
	r.offset++
	value := raw == 1
	r.record(start, name, "bool", value)
	return value, nil
}

func (r *loginTraceReader) rawVarInt(name string) (int32, error) {
	var value uint32
	for byteIndex := 0; byteIndex < 5; byteIndex++ {
		if err := r.require(name, "VarInt", 1); err != nil {
			return 0, err
		}
		current := r.data[r.offset]
		r.offset++
		value |= uint32(current&0x7f) << (7 * byteIndex)
		if current&0x80 == 0 {
			return int32(value), nil
		}
	}
	return 0, fmt.Errorf("field %q (VarInt) at offset %d exceeds 5 bytes", name, r.offset-5)
}

func (r *loginTraceReader) readVarInt(name string) (int32, error) {
	start := r.offset
	value, err := r.rawVarInt(name)
	if err != nil {
		return 0, err
	}
	r.record(start, name, "VarInt", value)
	return value, nil
}

func (r *loginTraceReader) readString(name string) (string, error) {
	start := r.offset
	length, err := r.rawVarInt(name + ".length")
	if err != nil {
		return "", err
	}
	if length < 0 {
		return "", fmt.Errorf("field %q (String) at offset %d has negative length %d", name, start, length)
	}
	if err := r.require(name, "String", int(length)); err != nil {
		return "", err
	}
	value := string(r.data[r.offset : r.offset+int(length)])
	r.offset += int(length)
	r.record(start, name, "String", value)
	return value, nil
}

func decodeLoginPayload769(data []byte) ([]loginTraceField, error) {
	r := &loginTraceReader{data: data}
	if _, err := r.readI32("entityId"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("hardcore"); err != nil {
		return r.fields, err
	}
	worldCount, err := r.readVarInt("worldNames.count")
	if err != nil {
		return r.fields, err
	}
	if worldCount < 0 || worldCount > 64 {
		return r.fields, fmt.Errorf("field %q (VarInt) has invalid value %d", "worldNames.count", worldCount)
	}
	for i := int32(0); i < worldCount; i++ {
		if _, err := r.readString(fmt.Sprintf("worldNames[%d]", i)); err != nil {
			return r.fields, err
		}
	}
	if _, err := r.readVarInt("maxPlayers"); err != nil {
		return r.fields, err
	}
	if _, err := r.readVarInt("viewDistance"); err != nil {
		return r.fields, err
	}
	if _, err := r.readVarInt("simulationDistance"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("reducedDebugInfo"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("enableRespawnScreen"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("doLimitedCrafting"); err != nil {
		return r.fields, err
	}
	if _, err := r.readVarInt("commonPlayerSpawnInfo.dimensionType"); err != nil {
		return r.fields, err
	}
	if _, err := r.readString("commonPlayerSpawnInfo.dimensionName"); err != nil {
		return r.fields, err
	}
	if _, err := r.readI64("commonPlayerSpawnInfo.hashedSeed"); err != nil {
		return r.fields, err
	}
	if _, err := r.readByte("commonPlayerSpawnInfo.gameMode", "i8"); err != nil {
		return r.fields, err
	}
	if _, err := r.readByte("commonPlayerSpawnInfo.previousGameMode", "u8"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("commonPlayerSpawnInfo.debug"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("commonPlayerSpawnInfo.flat"); err != nil {
		return r.fields, err
	}
	hasDeathLocation, err := r.readBool("commonPlayerSpawnInfo.lastDeathLocation.present")
	if err != nil {
		return r.fields, err
	}
	if hasDeathLocation {
		if _, err := r.readString("commonPlayerSpawnInfo.lastDeathLocation.dimension"); err != nil {
			return r.fields, err
		}
		if _, err := r.readI64("commonPlayerSpawnInfo.lastDeathLocation.position"); err != nil {
			return r.fields, err
		}
	}
	if _, err := r.readVarInt("commonPlayerSpawnInfo.portalCooldown"); err != nil {
		return r.fields, err
	}
	if _, err := r.readVarInt("commonPlayerSpawnInfo.seaLevel"); err != nil {
		return r.fields, err
	}
	if _, err := r.readBool("enforcesSecureChat"); err != nil {
		return r.fields, err
	}
	if r.offset != len(data) {
		return r.fields, fmt.Errorf("Login payload has %d trailing bytes: final offset %d, payload length %d", len(data)-r.offset, r.offset, len(data))
	}
	return r.fields, nil
}

// Golden bytes are derived independently from:
//   - PrismarineJS data/pc/1.21.4/protocol.json packet_login and SpawnInfo
//   - Mojang 1.21.4 ClientboundLoginPacket/CommonPlayerSpawnInfo codecs
//
// Entity ID 0x01020304 is deliberately four fixed-width bytes.
func TestLoginPayload769GoldenAndIndependentTrace(t *testing.T) {
	provider := &registry.VanillaProvider{}
	dimensionTypeID, err := provider.DimensionTypeID(overworldDimensionName)
	if err != nil {
		t.Fatal(err)
	}
	if dimensionTypeID != 0 {
		t.Fatalf("overworld dimension type ID = %d, want 0", dimensionTypeID)
	}

	p := &player.Player{EntityID: 0x01020304, GameMode: player.GameModeSurvival}
	const hashedSeed = int64(0x0102030405060708)
	pkt := buildLoginPlay(p, dimensionTypeID, hashedSeed)
	if pkt.ID != 0x2c {
		t.Fatalf("Login packet ID = 0x%02x, want 0x2c", pkt.ID)
	}

	const goldenPayloadHex = "010203040001136d696e6563726166743a6f766572776f726c64140a0a00010000136d696e6563726166743a6f766572776f726c64010203040506070800ff000000003f00"
	if got := hex.EncodeToString(pkt.Data); got != goldenPayloadHex {
		t.Fatalf("Login payload differs from protocol-769 golden fixture\n got: %s\nwant: %s", got, goldenPayloadHex)
	}

	frame, err := protocol.MarshalPacket(pkt)
	if err != nil {
		t.Fatal(err)
	}
	const goldenFrameHex = "462c" + goldenPayloadHex
	if got := hex.EncodeToString(frame); got != goldenFrameHex {
		t.Fatalf("Login frame differs from uncompressed golden fixture\n got: %s\nwant: %s", got, goldenFrameHex)
	}

	trace, err := decodeLoginPayload769(pkt.Data)
	if err != nil {
		t.Fatalf("independent Login decoder stopped: %v", err)
	}
	for _, field := range trace {
		t.Logf("[%02d,%02d) %-58s %-7s %v", field.Start, field.End, field.Name, field.Type, field.Value)
	}

	wantTrace := []loginTraceField{
		{0, 4, "entityId", "i32", int32(0x01020304)},
		{4, 5, "hardcore", "bool", false},
		{5, 6, "worldNames.count", "VarInt", int32(1)},
		{6, 26, "worldNames[0]", "String", "minecraft:overworld"},
		{26, 27, "maxPlayers", "VarInt", int32(20)},
		{27, 28, "viewDistance", "VarInt", int32(10)},
		{28, 29, "simulationDistance", "VarInt", int32(10)},
		{29, 30, "reducedDebugInfo", "bool", false},
		{30, 31, "enableRespawnScreen", "bool", true},
		{31, 32, "doLimitedCrafting", "bool", false},
		{32, 33, "commonPlayerSpawnInfo.dimensionType", "VarInt", int32(0)},
		{33, 53, "commonPlayerSpawnInfo.dimensionName", "String", "minecraft:overworld"},
		{53, 61, "commonPlayerSpawnInfo.hashedSeed", "i64", hashedSeed},
		{61, 62, "commonPlayerSpawnInfo.gameMode", "i8", byte(0)},
		{62, 63, "commonPlayerSpawnInfo.previousGameMode", "u8", byte(0xff)},
		{63, 64, "commonPlayerSpawnInfo.debug", "bool", false},
		{64, 65, "commonPlayerSpawnInfo.flat", "bool", false},
		{65, 66, "commonPlayerSpawnInfo.lastDeathLocation.present", "bool", false},
		{66, 67, "commonPlayerSpawnInfo.portalCooldown", "VarInt", int32(0)},
		{67, 68, "commonPlayerSpawnInfo.seaLevel", "VarInt", int32(63)},
		{68, 69, "enforcesSecureChat", "bool", false},
	}
	if !reflect.DeepEqual(trace, wantTrace) {
		t.Fatalf("decoded Login field trace mismatch\n got: %#v\nwant: %#v", trace, wantTrace)
	}
}

func TestSendLoginPlayRequiresPlayState(t *testing.T) {
	conn := &network.ClientConn{State: network.StateConfiguration}
	p := &player.Player{EntityID: 1, GameMode: player.GameModeSurvival}
	err := sendLoginPlay(conn, p, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "configuration state") {
		t.Fatalf("sendLoginPlay state error = %v, want configuration-state rejection", err)
	}
}
func TestLoginPayload769IndependentDecoderNamesTruncatedField(t *testing.T) {
	p := &player.Player{EntityID: 1, GameMode: player.GameModeSurvival}
	payload := buildLoginPlay(p, 0, 0).Data
	_, err := decodeLoginPayload769(payload[:60])
	if err == nil {
		t.Fatal("decoder accepted truncated hashed seed")
	}
	if !strings.Contains(err.Error(), "commonPlayerSpawnInfo.hashedSeed") {
		t.Fatalf("truncation error did not name the exact field: %v", err)
	}
}
func TestLoginPayload769IndependentDecoderRejectsTrailingBytes(t *testing.T) {
	p := &player.Player{EntityID: 1, GameMode: player.GameModeSurvival}
	payload := append(append([]byte(nil), buildLoginPlay(p, 0, 0).Data...), 0x00)
	_, err := decodeLoginPayload769(payload)
	if err == nil {
		t.Fatal("decoder accepted a trailing byte")
	}
}

func TestObfuscateSeedMatchesMinecraftBiomeManager(t *testing.T) {
	tests := []struct {
		seed int64
		want int64
	}{
		{0, -5812615543772869766},
		{1, 8980073438861410214},
		{12345, -2210082894509960444},
	}
	for _, tc := range tests {
		if got := obfuscateSeed(tc.seed); got != tc.want {
			t.Errorf("obfuscateSeed(%d) = %d, want %d", tc.seed, got, tc.want)
		}
	}
}

func TestBuildPlayerAbilitiesUsesCommandState(t *testing.T) {
	p := &player.Player{
		GameMode:    player.GameModeSurvival,
		AllowFlying: true,
		Flying:      true,
		FlySpeed:    0.2,
		WalkSpeed:   0.35,
	}
	packet := buildPlayerAbilities(p)
	reader := bytes.NewReader(packet.Data)
	flags, err := protocol.ReadByte(reader)
	if err != nil {
		t.Fatal(err)
	}
	if flags != 0x06 {
		t.Fatalf("ability flags = 0x%02x, want flying+allow_flying", flags)
	}
	flySpeed, err := protocol.ReadFloat(reader)
	if err != nil {
		t.Fatal(err)
	}
	walkSpeed, err := protocol.ReadFloat(reader)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(flySpeed-0.2)) > 1e-6 || math.Abs(float64(walkSpeed-0.35)) > 1e-6 {
		t.Fatalf("speeds fly=%f walk=%f", flySpeed, walkSpeed)
	}
	if reader.Len() != 0 {
		t.Fatalf("abilities packet has %d trailing bytes", reader.Len())
	}
}
