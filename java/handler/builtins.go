package handler

// Built-in command implementations.
//
// Each command receives a CommandContext and returns an error whose text is
// shown to the issuing player. The Dispatcher catches all returns. The
// Commands packet in commands_packet.go mirrors these handlers so arguments
// and registry-backed values are tab-completable in the vanilla client.
import (
	"crypto/rand"
	"fmt"
	"math"
	"strconv"
	"strings"

	corentity "GoCraft/core/entity"
	"GoCraft/core/player"
	coreworld "GoCraft/core/world"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

var locatableTargets = append([]string{"village"}, coreworld.GeneratedBiomeNames()...)

var villagerProfessionNames = []string{
	"normal", "none", "armorer", "butcher", "cartographer", "cleric",
	"farmer", "fisherman", "fletcher", "leatherworker", "librarian",
	"mason", "nitwit", "shepherd", "toolsmith", "weaponsmith",
}

var potionEffectNames = javaworld.MobEffectNames()

var summonableMobNames = []string{
	"allay", "armadillo", "axolotl", "bat", "camel", "cat", "chicken",
	"cod", "cow", "donkey", "fox", "frog", "glow_squid", "goat", "horse",
	"mooshroom", "mule", "ocelot", "panda", "parrot", "pig", "pufferfish",
	"rabbit", "salmon", "sheep", "skeleton_horse", "sniffer", "squid",
	"tadpole", "tropical_fish", "turtle", "villager", "wandering_trader",
	"zombie_horse", "bee", "dolphin", "iron_golem", "llama", "polar_bear",
	"snow_golem", "strider", "trader_llama", "wolf", "zombified_piglin",
	"blaze", "bogged", "breeze", "cave_spider", "creaking", "creeper",
	"drowned", "elder_guardian", "enderman", "endermite", "evoker", "ghast",
	"guardian", "hoglin", "husk", "illusioner", "magma_cube", "phantom",
	"piglin", "piglin_brute", "pillager", "ravager", "shulker", "silverfish",
	"skeleton", "slime", "spider", "stray", "vex", "vindicator", "warden",
	"witch", "wither", "wither_skeleton", "zoglin", "zombie",
	"zombie_villager",
}

// RegisterBuiltins registers all built-in GoCraft commands with d.
func RegisterBuiltins(d *Dispatcher) {
	d.Register("help", cmdHelp)
	d.Register("list", cmdList)
	d.Register("gamemode", cmdGameMode)
	d.Register("gm", cmdGameMode) // short alias
	d.Register("tp", cmdTp)
	d.Register("xyz", cmdXYZ)
	d.Register("locate", cmdLocate)
	d.Register("summon", cmdSummon)
	d.Register("version", cmdVersion)
	d.Register("ver", cmdVersion)
	d.Register("give", cmdGive)
	d.Register("get", cmdGet)
	d.Register("fly", cmdFly)
	d.Register("potioneffect", cmdPotionEffect)
	d.Register("walkspeed", cmdWalkSpeed)
	d.Register("walkspeen", cmdWalkSpeed) // compatibility with the commonly typed spelling
	d.Register("flyspeed", cmdFlySpeed)
	d.Register("flyyspeed", cmdFlySpeed) // compatibility with the commonly typed spelling
	d.Register("kick", cmdKick)
	d.Register("seed", cmdSeed)
	d.Register("spawnboat", cmdSpawnBoat)
}

// ── /help ─────────────────────────────────────────────────────────────────────

func cmdHelp(ctx CommandContext) error {
	_ = sendSystemMessage(ctx.Conn,
		"Commands: /gamemode /tp /xyz /locate /summon /give /get /fly /potioneffect /walkspeed /flyspeed /kick /list /version /seed /spawnboat /time /tps /timings /help")
	return nil
}

// ── /seed ─────────────────────────────────────────────────────────────────────

func cmdSeed(ctx CommandContext) error {
	if ctx.World == nil {
		return fmt.Errorf("world state is unavailable")
	}
	seed := ctx.World.Seed()
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf("World seed: %d", seed))
	return nil
}

// ── /spawnboat ────────────────────────────────────────────────────────────────

// cmdSpawnBoat spawns a boat at the player's current position.
// Usage: /spawnboat [wood_type]   e.g. /spawnboat oak (default) or /spawnboat spruce
func cmdSpawnBoat(ctx CommandContext) error {
	if ctx.Player == nil || ctx.World == nil || ctx.Manager == nil {
		return fmt.Errorf("world state is unavailable")
	}
	if ctx.NextEntityID == nil {
		return fmt.Errorf("entity allocator is unavailable")
	}

	woodType := "oak"
	if len(ctx.Args) >= 1 {
		woodType = strings.ToLower(strings.TrimPrefix(ctx.Args[0], "minecraft:"))
	}
	entityTypeName := corentity.EntityType("minecraft:" + woodType + "_boat")
	if !corentity.IsBoat(entityTypeName) {
		entityTypeName = corentity.TypeOakBoat
	}

	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return fmt.Errorf("creating boat UUID: %w", err)
	}
	boat := corentity.New(
		ctx.NextEntityID(),
		uuid,
		entityTypeName,
		ctx.Player.Position.X,
		ctx.Player.Position.Y,
		ctx.Player.Position.Z,
	)
	ctx.World.Entities.Add(boat)
	BroadcastSpawnMob(boat, ctx.Manager)
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf("Spawned %s at your position. Right-click to board, sneak to dismount.", entityTypeName))
	return nil
}

// ── /list ─────────────────────────────────────────────────────────────────────

func cmdList(ctx CommandContext) error {
	sessions := ctx.Manager.SnapshotAll()
	names := make([]string, 0, len(sessions))
	for _, s := range sessions {
		names = append(names, s.Player.Username)
	}
	_ = sendSystemMessage(ctx.Conn,
		fmt.Sprintf("Online (%d): %s", len(names), strings.Join(names, ", ")))
	return nil
}

// ── /gamemode ─────────────────────────────────────────────────────────────────

func cmdXYZ(ctx CommandContext) error {
	if ctx.Player == nil {
		return fmt.Errorf("player state is unavailable")
	}
	position := ctx.Player.Position
	block := position.ToBlock()
	chunkX := int32(math.Floor(position.X / float64(coreworld.SectionSize)))
	chunkZ := int32(math.Floor(position.Z / float64(coreworld.SectionSize)))
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf(
		"Position: X=%.2f Y=%.2f Z=%.2f | Block: %d %d %d | Chunk: %d %d",
		position.X, position.Y, position.Z, block.X, block.Y, block.Z, chunkX, chunkZ,
	))
	return nil
}

const (
	locateMaxDistance = 8192
	goCraftVersion    = "GoCraft 1.21.4"
)

func cmdLocate(ctx CommandContext) error {
	if len(ctx.Args) != 1 {
		return fmt.Errorf("usage: /locate <village|biome>")
	}
	if ctx.World == nil || ctx.Player == nil {
		return fmt.Errorf("world state is unavailable")
	}

	target := strings.ToLower(strings.TrimPrefix(ctx.Args[0], "minecraft:"))
	originX := int(math.Floor(ctx.Player.Position.X))
	originZ := int(math.Floor(ctx.Player.Position.Z))

	if target == "village" {
		center, ok := ctx.World.NearestVillage(originX, originZ, locateMaxDistance)
		if !ok {
			return fmt.Errorf("no village found within %d blocks", locateMaxDistance)
		}
		y := ctx.World.SurfaceY(center.WorldX, center.WorldZ) + 1
		distance := int(math.Round(math.Hypot(
			float64(center.WorldX-originX),
			float64(center.WorldZ-originZ),
		)))
		biome := strings.TrimPrefix(center.Biome, "minecraft:")
		_ = sendSystemMessage(ctx.Conn, fmt.Sprintf(
			"Nearest village: %d %d %d (%d blocks, %s). Teleport: /tp %d %d %d",
			center.WorldX, y, center.WorldZ, distance, biome,
			center.WorldX, y, center.WorldZ,
		))
		return nil
	}

	if !containsName(coreworld.GeneratedBiomeNames(), target) {
		return fmt.Errorf("unknown locate target %q; use tab completion", ctx.Args[0])
	}
	x, z, ok := ctx.World.NearestBiome(
		originX, originZ, "minecraft:"+target, locateMaxDistance,
	)
	if !ok {
		return fmt.Errorf("no %s biome found within %d blocks", target, locateMaxDistance)
	}
	y := ctx.World.SurfaceY(x, z) + 1
	distance := int(math.Round(math.Hypot(float64(x-originX), float64(z-originZ))))
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf(
		"Nearest %s: %d %d %d (%d blocks). Teleport: /tp %d %d %d",
		target, x, y, z, distance, x, y, z,
	))
	return nil
}

func cmdVersion(ctx CommandContext) error {
	_ = sendSystemMessage(ctx.Conn, goCraftVersion)
	return nil
}

func cmdSummon(ctx CommandContext) error {
	if len(ctx.Args) < 1 || len(ctx.Args) > 2 {
		return fmt.Errorf("usage: /summon <mob> [villager_profession]")
	}
	if ctx.Player == nil || ctx.World == nil || ctx.Manager == nil {
		return fmt.Errorf("world state is unavailable")
	}
	if ctx.NextEntityID == nil {
		return fmt.Errorf("entity allocator is unavailable")
	}

	mobName := strings.ToLower(strings.TrimPrefix(ctx.Args[0], "minecraft:"))
	if !containsName(summonableMobNames, mobName) {
		return fmt.Errorf("unknown mob %q; use tab completion", ctx.Args[0])
	}

	professionName := "normal"
	if len(ctx.Args) == 2 {
		if mobName != "villager" {
			return fmt.Errorf("villager professions can only be used with /summon villager")
		}
		professionName = strings.ToLower(strings.TrimPrefix(ctx.Args[1], "minecraft:"))
		if !containsName(villagerProfessionNames, professionName) {
			return fmt.Errorf("unknown villager profession %q; use tab completion", ctx.Args[1])
		}
	}

	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return fmt.Errorf("creating entity UUID: %w", err)
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	position := ctx.Player.Position
	entity := corentity.New(
		ctx.NextEntityID(),
		uuid,
		corentity.EntityType("minecraft:"+mobName),
		position.X+1.5,
		position.Y,
		position.Z,
	)
	entity.OnGround = ctx.Player.OnGround
	if mobName == "villager" {
		entity.VillagerVariant = corentity.VillagerVariantPlains
		entity.VillagerProfession = corentity.VillagerProfessionNone
		entity.VillagerLevel = 1
		if professionName != "normal" && professionName != "none" {
			entity.VillagerProfession = corentity.VillagerProfession("minecraft:" + professionName)
		}
	}

	spawnPacket, ok := buildSpawnMob(entity)
	if !ok {
		return fmt.Errorf("mob %q is unavailable in the Java 1.21.4 registry", mobName)
	}
	metadataPacket := buildMobMetadata(entity)
	ctx.World.Entities.Add(entity)
	for _, session := range ctx.Manager.SnapshotAll() {
		_ = session.Conn.WritePacket(spawnPacket)
		if metadataPacket != nil {
			_ = session.Conn.WritePacket(metadataPacket)
		}
	}
	if mobName == "villager" {
		_ = sendSystemMessage(ctx.Conn, fmt.Sprintf(
			"Summoned villager (%s) with entity ID %d",
			professionName, entity.EntityID,
		))
	} else {
		_ = sendSystemMessage(ctx.Conn, fmt.Sprintf(
			"Summoned %s with entity ID %d", mobName, entity.EntityID,
		))
	}
	return nil
}

func containsName(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}

func cmdGameMode(ctx CommandContext) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("usage: /gamemode <survival|creative|adventure|spectator>")
	}
	var mode player.GameMode
	switch strings.ToLower(ctx.Args[0]) {
	case "survival", "s", "0":
		mode = player.GameModeSurvival
	case "creative", "c", "1":
		mode = player.GameModeCreative
	case "adventure", "a", "2":
		mode = player.GameModeAdventure
	case "spectator", "sp", "3":
		mode = player.GameModeSpectator
	default:
		return fmt.Errorf("unknown game mode: %q", ctx.Args[0])
	}
	ctx.Player.GameMode = mode

	// Notify the client of its new game mode via a Game Event.
	// Reason 3 = change_game_mode; value = mode as float32.
	if err := sendGameEvent(ctx.Conn, 3, float32(mode)); err != nil {
		return fmt.Errorf("sending game event: %w", err)
	}

	// Resync flight / speed / instant-break flags for the new mode.
	if err := sendPlayerAbilities(ctx.Conn, ctx.Player); err != nil {
		return fmt.Errorf("sending abilities: %w", err)
	}

	// Update the tab-list game mode for all connected players.
	updatePkt := buildGameModeUpdate(ctx.Player)
	for _, s := range ctx.Manager.SnapshotAll() {
		_ = s.Conn.WritePacket(updatePkt)
	}

	modeName := [4]string{"survival", "creative", "adventure", "spectator"}[mode]
	_ = sendSystemMessage(ctx.Conn, "Game mode changed to "+modeName)
	return nil
}

// buildGameModeUpdate builds a Player Info Update (action 0x04 = UPDATE_GAME_MODE)
// packet to update p's game mode entry in every client's tab list.
//
// Wire layout (1.21.4):
//
//	Byte    actions        = 0x04 (UPDATE_GAME_MODE)
//	VarInt  player_count   = 1
//	UUID    player_uuid
//	VarInt  game_mode
func buildGameModeUpdate(p *player.Player) *protocol.Packet {
	return protocol.NewBuilder(packetIDPlayerInfoUpdate).
		Byte(0x04). // UPDATE_GAME_MODE action mask
		VarInt(1).
		UUID(protocol.UUID(p.UUID)).
		VarInt(int32(p.GameMode)).
		Build()
}

// ── /tp ───────────────────────────────────────────────────────────────────────

func cmdTp(ctx CommandContext) error {
	if len(ctx.Args) == 0 {
		return fmt.Errorf("usage: /tp <x> <y> <z>  or  /tp <player>")
	}

	if len(ctx.Args) >= 3 {
		// Coordinate teleport.
		x, err := strconv.ParseFloat(ctx.Args[0], 64)
		if err != nil {
			return fmt.Errorf("invalid x: %q", ctx.Args[0])
		}
		y, err := strconv.ParseFloat(ctx.Args[1], 64)
		if err != nil {
			return fmt.Errorf("invalid y: %q", ctx.Args[1])
		}
		z, err := strconv.ParseFloat(ctx.Args[2], 64)
		if err != nil {
			return fmt.Errorf("invalid z: %q", ctx.Args[2])
		}
		if err := ctx.TeleportTo(x, y, z); err != nil {
			return fmt.Errorf("teleporting: %w", err)
		}
		_ = sendSystemMessage(ctx.Conn,
			fmt.Sprintf("Teleported to %.2f %.2f %.2f", x, y, z))
		return nil
	}

	// Player-name teleport.
	targetName := ctx.Args[0]
	for _, s := range ctx.Manager.SnapshotAll() {
		if strings.EqualFold(s.Player.Username, targetName) {
			pos := s.Player.Position
			if err := ctx.TeleportTo(pos.X, pos.Y, pos.Z); err != nil {
				return fmt.Errorf("teleporting: %w", err)
			}
			_ = sendSystemMessage(ctx.Conn,
				fmt.Sprintf("Teleported to %s", s.Player.Username))
			return nil
		}
	}
	return fmt.Errorf("player not found: %s", targetName)
}

// ── /give ─────────────────────────────────────────────────────────────────────

func cmdGive(ctx CommandContext) error {
	if len(ctx.Args) < 2 || len(ctx.Args) > 3 {
		return fmt.Errorf("usage: /give <player> <item|block> [count]")
	}
	target, targetConn, err := findOnlinePlayer(ctx, ctx.Args[0])
	if err != nil {
		return err
	}

	itemName := normalizeResourceLocation(ctx.Args[1])
	if itemName == "minecraft:air" || javaworld.ItemID(itemName) < 0 {
		return fmt.Errorf("unknown item or block %q", ctx.Args[1])
	}
	count, err := parseGiveCount(ctx.Args[2:])
	if err != nil {
		return err
	}
	if !target.GiveItem(player.ItemStack{ItemID: itemName, Count: count}) {
		return fmt.Errorf("%s's inventory is full", target.Username)
	}
	if err := sendSetContainerContent(targetConn, target, 1); err != nil {
		return fmt.Errorf("syncing %s's inventory: %w", target.Username, err)
	}
	_ = sendSystemMessage(ctx.Conn,
		fmt.Sprintf("Given %dx %s to %s", count, itemName, target.Username))
	if target != ctx.Player {
		_ = sendSystemMessage(targetConn,
			fmt.Sprintf("You received %dx %s", count, itemName))
	}
	return nil
}

func cmdGet(ctx CommandContext) error {
	if len(ctx.Args) < 1 || len(ctx.Args) > 2 {
		return fmt.Errorf("usage: /get <item|block> [count]")
	}
	args := make([]string, 0, len(ctx.Args)+1)
	args = append(args, ctx.Player.Username)
	args = append(args, ctx.Args...)
	ctx.Args = args
	return cmdGive(ctx)
}

func parseGiveCount(args []string) (int, error) {
	if len(args) == 0 {
		return 1, nil
	}
	count, err := strconv.Atoi(args[0])
	if err != nil || count < 1 || count > 64 {
		return 0, fmt.Errorf("count must be between 1 and 64, got %q", args[0])
	}
	return count, nil
}

func normalizeResourceLocation(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if !strings.Contains(name, ":") {
		return "minecraft:" + name
	}
	return name
}

func findOnlinePlayer(ctx CommandContext, name string) (*player.Player, *network.ClientConn, error) {
	if name == "@s" || strings.EqualFold(name, ctx.Player.Username) {
		return ctx.Player, ctx.Conn, nil
	}
	for _, candidate := range ctx.Manager.SnapshotAll() {
		if strings.EqualFold(candidate.Player.Username, name) {
			return candidate.Player, candidate.Conn, nil
		}
	}
	return nil, nil, fmt.Errorf("player not found: %s", name)
}

func cmdFly(ctx CommandContext) error {
	if len(ctx.Args) != 0 {
		return fmt.Errorf("usage: /fly")
	}
	if ctx.Player.GameMode == player.GameModeCreative ||
		ctx.Player.GameMode == player.GameModeSpectator {
		ctx.Player.Flying = !ctx.Player.Flying
	} else {
		ctx.Player.AllowFlying = !ctx.Player.AllowFlying
		ctx.Player.Flying = ctx.Player.AllowFlying
	}
	if err := sendPlayerAbilities(ctx.Conn, ctx.Player); err != nil {
		return fmt.Errorf("sending flight abilities: %w", err)
	}
	state := "disabled"
	if ctx.Player.Flying {
		state = "enabled"
	}
	_ = sendSystemMessage(ctx.Conn, "Flight "+state)
	return nil
}

func cmdWalkSpeed(ctx CommandContext) error {
	speed, err := parseSpeedArgument(ctx.Args, 0.1, "/walkspeed")
	if err != nil {
		return err
	}
	ctx.Player.WalkSpeed = speed
	if err := sendPlayerAbilities(ctx.Conn, ctx.Player); err != nil {
		return fmt.Errorf("sending walking speed: %w", err)
	}
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf("Walking speed set to %.3g", speed))
	return nil
}

func cmdFlySpeed(ctx CommandContext) error {
	speed, err := parseSpeedArgument(ctx.Args, 0.05, "/flyspeed")
	if err != nil {
		return err
	}
	ctx.Player.FlySpeed = speed
	if err := sendPlayerAbilities(ctx.Conn, ctx.Player); err != nil {
		return fmt.Errorf("sending flying speed: %w", err)
	}
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf("Flying speed set to %.3g", speed))
	return nil
}

func parseSpeedArgument(args []string, defaultValue float32, command string) (float32, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("usage: %s <value|reset>", command)
	}
	if strings.EqualFold(args[0], "reset") {
		return defaultValue, nil
	}
	value, err := strconv.ParseFloat(args[0], 32)
	if err != nil || value < 0.001 || value > 1 {
		return 0, fmt.Errorf("speed must be between 0.001 and 1, or reset")
	}
	return float32(value), nil
}

func cmdPotionEffect(ctx CommandContext) error {
	if len(ctx.Args) != 3 {
		return fmt.Errorf("usage: /potioneffect <player> <potion_type> <seconds>")
	}
	target, _, err := findOnlinePlayer(ctx, ctx.Args[0])
	if err != nil {
		return err
	}
	effectName := normalizeResourceLocation(ctx.Args[1])
	effectID := javaworld.MobEffectID(effectName)
	if effectID < 0 {
		return fmt.Errorf("unknown potion effect %q; use tab completion", ctx.Args[1])
	}
	seconds, err := strconv.Atoi(ctx.Args[2])
	if err != nil || seconds < 1 || seconds > 1_000_000 {
		return fmt.Errorf("effect time must be between 1 and 1000000 seconds")
	}
	pkt := protocol.NewBuilder(packetIDUpdateMobEffect).
		VarInt(target.EntityID).
		VarInt(effectID).
		VarInt(0). // amplifier: level I
		VarInt(int32(seconds * 20)).
		Byte(0x06). // show particles and icon
		Build()
	for _, viewer := range ctx.Manager.SnapshotAll() {
		_ = viewer.Conn.WritePacket(pkt)
	}
	_ = sendSystemMessage(ctx.Conn, fmt.Sprintf(
		"Applied %s to %s for %d seconds", effectName, target.Username, seconds))
	return nil
}

// -- /kick -----------------------------------------------------------------
func cmdKick(ctx CommandContext) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("usage: /kick <player> [reason]")
	}
	targetName := ctx.Args[0]
	reason := "Kicked by an operator"
	if len(ctx.Args) >= 2 {
		reason = strings.Join(ctx.Args[1:], " ")
	}

	for _, s := range ctx.Manager.SnapshotAll() {
		if strings.EqualFold(s.Player.Username, targetName) {
			// Send a Disconnect packet so the client shows the reason, then
			// close the connection.  The play loop will clean up the session
			// via the deferred onPlayerLeave / mgr.Remove calls.
			_ = s.Conn.WritePacket(buildDisconnectPlay(reason))
			_ = s.Conn.Close()
			_ = sendSystemMessage(ctx.Conn,
				fmt.Sprintf("Kicked %s: %s", s.Player.Username, reason))
			return nil
		}
	}
	return fmt.Errorf("player not found: %s", targetName)
}

// buildDisconnectPlay constructs a Disconnect (Play) S→C packet.
//
// Wire layout (1.21.4):
//
//	Text Component (NBT)  reason
//
// The reason is encoded as a Network NBT text component (same format used by
// System Chat Message since 1.20.3).
func buildDisconnectPlay(reason string) *protocol.Packet {
	return protocol.NewBuilder(packetIDDisconnectPlay).
		Bytes(nbtTextComponent(reason)).
		Build()
}
