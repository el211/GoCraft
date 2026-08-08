package main

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func main() {
	l, err := minecraft.ListenConfig{
		StatusProvider: minecraft.NewStatusProvider("mini2168", "1.26.40"),
		PacketFunc: func(h packet.Header, payload []byte, src, dst net.Addr) {
			short := payload
			if len(short) > 64 {
				short = short[:64]
			}
			slog.Info("wire packet",
				"id", fmt.Sprintf("0x%02x", h.PacketID),
				"dir", fmt.Sprintf("%v→%v", src, dst),
				"bodyLen", len(payload),
				"bodyHex64", hex.EncodeToString(short),
			)
		},
	}.Listen("raknet", "0.0.0.0:19200")
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	slog.Info("mini2168 listening on :19200")

	for {
		c, err := l.Accept()
		if err != nil {
			slog.Error("accept", "err", err)
			return
		}
		go handle(c.(*minecraft.Conn))
	}
}

func handle(conn *minecraft.Conn) {
	defer conn.Close()
	slog.Info("client connecting", "remote", conn.RemoteAddr())

	if err := conn.StartGame(minecraft.GameData{
		WorldName:                    "mini2168",
		EntityUniqueID:               1,
		EntityRuntimeID:              1,
		PlayerPosition:               [3]float32{0, 65, 0},
		PlayerGameMode:               0,
		WorldGameMode:                0,
		Difficulty:                   1,
		WorldSpawn:                   protocol.BlockPos{0, 65, 0},
		ChunkRadius:                  4,
		ServerAuthoritativeInventory: true,
	}); err != nil {
		slog.Error("StartGame", "err", err)
		return
	}
	slog.Info("StartGame OK")

	if err := conn.WritePacket(&packet.NetworkChunkPublisherUpdate{
		Position: protocol.BlockPos{0, 65, 0},
		Radius:   64,
	}); err != nil {
		slog.Error("NCPU failed", "err", err)
		return
	}
	slog.Info("NCPU sent")

	// 24 sub-chunks × single-value plains (zigzag ID 1 = 0x02) + border byte
	biome := make([]byte, 0, 49)
	for range 24 {
		biome = append(biome, 0x01, 0x02)
	}
	biome = append(biome, 0x00)

	for dx := int32(-4); dx <= 4; dx++ {
		for dz := int32(-4); dz <= 4; dz++ {
			if err := conn.WritePacket(&packet.LevelChunk{
				Position:      protocol.ChunkPos{dx, dz},
				Dimension:     0,
				SubChunkCount: protocol.SubChunkRequestModeLimited,
				SubChunkLimit: protocol.Option[int32](19),
				CacheEnabled:  false,
				RawPayload:    biome,
			}); err != nil {
				slog.Error("LevelChunk failed", "err", err)
				return
			}
		}
	}
	slog.Info("all chunks sent, play loop starting")

	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			slog.Warn("play loop ended", "err", err)
			return
		}
		switch p := pk.(type) {
		case *packet.SubChunkRequest:
			slog.Info("SubChunkRequest", "dim", p.Dimension, "pos", p.Position, "offsets", len(p.Offsets))
			entries := make([]protocol.SubChunkEntry, len(p.Offsets))
			for i, off := range p.Offsets {
				entries[i] = protocol.SubChunkEntry{
					Offset:        off,
					Result:        protocol.SubChunkResultSuccessAllAir,
					HeightMapType: protocol.HeightMapDataTooHigh,
				}
			}
			_ = conn.WritePacket(&packet.SubChunk{
				Dimension:       p.Dimension,
				Position:        p.Position,
				SubChunkEntries: entries,
			})
		default:
			slog.Info("unhandled packet", "type", fmt.Sprintf("%T", pk))
		}
	}
}
