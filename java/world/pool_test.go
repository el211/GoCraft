package world

import (
	"testing"

	coreworld "GoCraft/core/world"
)

func TestEncodeChunkReturnsOwnedBytesWhenPoolIsReused(t *testing.T) {
	chunk := &coreworld.Chunk{}
	first := EncodeChunk(chunk)
	if len(first) == 0 {
		t.Fatal("encoded chunk is empty")
	}
	original := first[0]
	first[0] ^= 0xff
	second := EncodeChunk(chunk)
	if second[0] != original {
		t.Fatalf("second encoding aliases first: got %#x, want %#x", second[0], original)
	}
}

func TestSkyLightPoolClearsReturnedPages(t *testing.T) {
	page := acquireSkyLightPage()
	for i := range page {
		page[i] = 0xff
	}
	releaseSkyLightPages([][]byte{page})
	reused := acquireSkyLightPage()
	defer releaseSkyLightPages([][]byte{reused})
	for i, value := range reused {
		if value != 0 {
			t.Fatalf("reused skylight byte %d = %#x, want zero", i, value)
		}
	}
}
