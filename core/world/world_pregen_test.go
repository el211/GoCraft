package world

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingGenerator struct{ calls atomic.Int32 }

func (g *countingGenerator) Generate(x, z int32) *Chunk {
	g.calls.Add(1)
	return (&FlatGenerator{}).Generate(x, z)
}

func TestQueuePregenerationWarmsSquare(t *testing.T) {
	generator := &countingGenerator{}
	world := New(generator, nil, false)
	defer world.Close()
	if queued := world.QueuePregeneration(5, -3, 2); queued != 25 {
		t.Fatalf("queued=%d, want 25", queued)
	}
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for world.LoadedCount() < 25 {
		select {
		case <-deadline:
			t.Fatalf("loaded=%d, want 25", world.LoadedCount())
		case <-ticker.C:
		}
	}
	if got := generator.calls.Load(); got != 25 {
		t.Fatalf("generator calls=%d, want 25", got)
	}
	if queued := world.QueuePregeneration(5, -3, 2); queued != 0 {
		t.Fatalf("duplicate queued=%d, want 0", queued)
	}
}

func TestConcurrentChunkGenerationIsCoalesced(t *testing.T) {
	generator := &countingGenerator{}
	world := New(generator, nil, false)
	defer world.Close()
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if chunk := world.Chunk(7, 9); chunk.X != 7 || chunk.Z != 9 {
				t.Errorf("chunk=(%d,%d), want (7,9)", chunk.X, chunk.Z)
			}
		}()
	}
	group.Wait()
	if got := generator.calls.Load(); got != 1 {
		t.Fatalf("generator calls=%d, want 1", got)
	}
}

func TestCleanChunkCacheIsBounded(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetMaxCachedChunks(128)
	for x := int32(0); x < 256; x++ {
		world.Chunk(x, 0)
	}
	if got := world.LoadedCount(); got > 128 {
		t.Fatalf("loaded chunks=%d, want at most 128", got)
	}
}

func TestEntityCollisionAndGroundCorrection(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	if !world.CanEntityOccupy(0.5, 64, 0.5) {
		t.Fatal("surface spawn position should be occupiable")
	}
	if world.CanEntityOccupy(0.5, 63, 0.5) {
		t.Fatal("position inside the ground should collide")
	}
	if got := world.GroundYAtOrBelow(0, 0, 64); got != 63 {
		t.Fatalf("ground Y=%d, want 63", got)
	}
}
