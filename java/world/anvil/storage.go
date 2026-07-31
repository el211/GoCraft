package anvil

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	coreworld "GoCraft/core/world"
)

// Storage implements core/world.Storage using the Minecraft Anvil region format.
//
// LoadChunk reads chunk data from .mca region files on demand.
// SaveChunk buffers the encoded chunk and flushes it to disk on Close (or
// explicit Flush).  Each SaveChunk call compresses and stores the NBT bytes
// immediately, then writes the region file on flush — avoiding repeated file
// rewrites for multiple blocks broken in the same region.
type Storage struct {
	worldDir string
	mu       sync.Mutex
	// pending maps chunk coords to ready-to-write compressed bytes.
	// Key: [cx, cz]; Value: [compression=2, zlib-data…].
	pending  map[[2]int32][]byte
	original map[[2]int32]map[string]Tag
}

// NewStorage returns a Storage rooted at worldDir.
// worldDir should be the Minecraft world folder (containing region/, level.dat, …).
// The world and region directories are created immediately so operators can
// verify the configured persistent path before the first modified chunk.
func NewStorage(worldDir string) (*Storage, error) {
	if worldDir == "" {
		return nil, fmt.Errorf("anvil storage: world directory is empty")
	}
	if err := os.MkdirAll(filepath.Join(worldDir, "region"), 0o755); err != nil {
		return nil, fmt.Errorf("anvil storage: creating world directory %s: %w", worldDir, err)
	}
	return &Storage{
		worldDir: worldDir,
		pending:  make(map[[2]int32][]byte),
		original: make(map[[2]int32]map[string]Tag),
	}, nil
}

// LoadChunk loads the chunk at (x, z) from the Anvil region files.
// Returns (nil, nil) if the region file or chunk slot is absent.
func (s *Storage) LoadChunk(x, z int32) (*coreworld.Chunk, error) {
	root, err := loadChunkFromRegion(s.worldDir, x, z)
	if err != nil {
		return nil, fmt.Errorf("anvil storage: loading chunk (%d,%d): %w", x, z, err)
	}
	if root == nil {
		return nil, nil
	}
	c, err := chunkFromNBT(root, x, z)
	if err != nil {
		return nil, fmt.Errorf("anvil storage: decoding chunk (%d,%d): %w", x, z, err)
	}
	if c != nil {
		s.mu.Lock()
		s.original[[2]int32{x, z}] = cloneCompound(root)
		s.mu.Unlock()
	}
	return c, nil
}

// SaveChunk encodes c as Anvil NBT, compresses it with zlib, and buffers it
// for the next Flush.  Repeated calls for the same chunk coordinate replace
// the earlier buffered version.
func (s *Storage) SaveChunk(c *coreworld.Chunk) error {
	key := [2]int32{c.X, c.Z}
	s.mu.Lock()
	base := s.original[key]
	s.mu.Unlock()
	nbt := encodeChunkNBT(c)
	if base != nil {
		nbt = encodeChunkNBTWithBase(c, base)
	}

	// Compress now so the work happens on the caller's goroutine rather than
	// delaying the eventual Flush.
	compressed, err := zlibCompress(nbt)
	if err != nil {
		return fmt.Errorf("anvil storage: compressing chunk (%d,%d): %w", c.X, c.Z, err)
	}
	// raw = [compression_type=2] + [compressed bytes]
	raw := make([]byte, 1+len(compressed))
	raw[0] = 2
	copy(raw[1:], compressed)

	s.mu.Lock()
	s.pending[key] = raw
	s.mu.Unlock()
	return nil
}

// Flush writes all buffered chunks to their respective region files and clears
// the pending buffer.  Safe to call multiple times.
func (s *Storage) Flush() error {
	s.mu.Lock()
	pending := s.pending
	s.pending = make(map[[2]int32][]byte)
	s.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	// Group pending chunks by region.
	type regionKey = [2]int32
	regions := make(map[regionKey][]regionKey)
	for key := range pending {
		rk := regionKey{key[0] >> 5, key[1] >> 5}
		regions[rk] = append(regions[rk], key)
	}

	for rk, keys := range regions {
		if err := s.flushRegion(rk[0], rk[1], keys, pending); err != nil {
			s.restorePending(pending)
			return err
		}
	}
	return nil
}

func (s *Storage) restorePending(pending map[[2]int32][]byte) {
	s.mu.Lock()
	for key, raw := range pending {
		if _, newer := s.pending[key]; !newer {
			s.pending[key] = raw
		}
	}
	s.mu.Unlock()
}

// flushRegion writes all pending chunks for region (rx, rz) to disk.
func (s *Storage) flushRegion(rx, rz int32, keys [][2]int32, pending map[[2]int32][]byte) error {
	path := regionPath(s.worldDir, rx, rz)

	// Build the full rawChunks slice for this region.
	rawChunks := make([][]byte, 1024)

	// Snapshot existing chunk data from disk (for slots we are NOT replacing).
	dirty := make(map[int]struct{}, len(keys))
	for _, k := range keys {
		dirty[chunkIndex(k[0], k[1])] = struct{}{}
	}

	if f, err := openRegionFile(path); err == nil {
		for i := 0; i < 1024; i++ {
			if _, isDirty := dirty[i]; isDirty {
				continue // will be replaced
			}
			raw, _ := readRawChunk(f, i)
			rawChunks[i] = raw
		}
		f.Close()
	}

	// Insert pending chunks.
	for _, k := range keys {
		rawChunks[chunkIndex(k[0], k[1])] = pending[k]
	}

	tmpPath := path + ".tmp"
	if err := writeRegionFile(tmpPath, rawChunks); err != nil {
		return fmt.Errorf("anvil storage: writing region (%d,%d): %w", rx, rz, err)
	}
	if err := renameFile(tmpPath, path); err != nil {
		return fmt.Errorf("anvil storage: renaming region (%d,%d): %w", rx, rz, err)
	}

	slog.Debug("anvil: flushed region", "rx", rx, "rz", rz, "chunks", len(keys))
	return nil
}

// Close flushes all pending chunks and releases any resources.
func (s *Storage) Close() error {
	if err := s.Flush(); err != nil {
		return fmt.Errorf("anvil storage: close: %w", err)
	}
	return nil
}
