package world

// Storage is the persistence backend for a World.
// Implementations are edition-specific: Anvil for Java Edition,
// LevelDB for Bedrock Edition (future).  The interface is edition-agnostic so
// the core never imports any adapter package.
//
// All methods must be safe for concurrent use.
type Storage interface {
	// LoadChunk loads the chunk at (x, z) from the backing store.
	// Returns (nil, nil) if the chunk is not present in storage (not yet
	// generated or never saved).
	LoadChunk(x, z int32) (*Chunk, error)

	// SaveChunk stages c for persistence in the backing store.
	SaveChunk(c *Chunk) error

	// Flush commits all staged writes to durable storage.
	Flush() error

	// Close flushes any buffered writes and releases file handles.
	// After Close returns, no other methods may be called.
	Close() error
}
