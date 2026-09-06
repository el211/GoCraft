package server

import (
	"math"
	"strconv"

	"GoCraft/core/intent"
	"GoCraft/core/player"
	"GoCraft/core/spatial"
	coreworld "GoCraft/core/world"
	"GoCraft/java/handler"
)

func (s *Server) applyBellRing(i intent.BellRingIntent) {
	p := s.game.GetPlayer(i.PlayerUUID)
	if p == nil || p.Dead || p.GameMode == player.GameModeSpectator {
		return
	}
	world := s.worldForPlayer(p)
	if world == nil {
		return
	}
	centre := spatial.Vec3{X: float64(i.Position.X) + 0.5, Y: float64(i.Position.Y) + 0.5, Z: float64(i.Position.Z) + 0.5}
	if p.Position.Distance(centre) > 6.5 {
		return
	}
	block := world.GetBlock(int(i.Position.X), int(i.Position.Y), int(i.Position.Z))
	direction, valid := coreworld.BellRingDirection(block, i.Face, i.HitY)
	if valid {
		s.ringBell(world, p.Dimension, i.Position, direction)
	}
}

// playNoteBlock broadcasts the note sound when a note block is powered by
// redstone. The instrument is derived from the block below (or from the stored
// block state when already set), and the pitch from the note value.
func (s *Server) playNoteBlock(x, y, z int, block coreworld.Block) {
	instrument := block.Properties["instrument"]
	if instrument == "" {
		below := s.world.GetBlock(x, y-1, z)
		instrument = coreworld.NoteBlockInstrument(below)
	}
	note, _ := strconv.Atoi(block.Properties["note"])
	pitch := float32(math.Pow(2, (float64(note)-12)/12))
	handler.BroadcastSoundAtDimension(s.sessions, s.simulationDimension,
		"minecraft:block.note_block."+instrument, 1, // category: blocks
		float64(x)+0.5, float64(y)+0.5, float64(z)+0.5, 3, pitch)
	handler.BroadcastNoteBlockAction(x, y, z, note, block, s.sessions, s.simulationDimension)
}

func (s *Server) ringBell(world *coreworld.World, dimension int32, position spatial.BlockPos, direction string) {
	if world == nil || world.GetBlock(int(position.X), int(position.Y), int(position.Z)).ResourceLocation() != "minecraft:bell" {
		return
	}
	handler.BroadcastBellRing(position, direction, dimension, s.sessions)
	if s.bedrockListener != nil {
		s.bedrockListener.BroadcastBellRing(dimension, position, direction)
	}
	world.EmitVibration(int(position.X), int(position.Y), int(position.Z))
}
