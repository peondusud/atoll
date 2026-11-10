package integrity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/zeebo/blake3"
)

// ChunkSize is the size of a chunk unless the catalog says otherwise: 4 MiB
// (specs.md §16).
const ChunkSize = 4 << 20

// HashSize is the size of a BLAKE3-256 hash in bytes.
const HashSize = 32

// Hasher computes, from one read of a file, its whole-file hash and the hash of
// each of its chunks (specs.md §16). It does no I/O.
type Hasher struct {
	file, chunk *blake3.Hasher
	chunkSize   int64
	inChunk     int64
	chunks      []byte
}

// NewHasher returns a Hasher for chunks of chunkSize bytes. An error means that
// chunkSize is not positive.
func NewHasher(chunkSize int64) (*Hasher, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("integrity: chunk size %d", chunkSize)
	}
	return &Hasher{file: blake3.New(), chunk: blake3.New(), chunkSize: chunkSize}, nil
}

// Write adds the next bytes of the file.
func (h *Hasher) Write(p []byte) (int, error) {
	if _, err := h.file.Write(p); err != nil {
		return 0, fmt.Errorf("integrity: %w", err)
	}
	n := len(p)
	for len(p) > 0 {
		take := min(int64(len(p)), h.chunkSize-h.inChunk)
		if _, err := h.chunk.Write(p[:take]); err != nil {
			return 0, fmt.Errorf("integrity: %w", err)
		}
		p = p[take:]
		h.inChunk += take
		if h.inChunk == h.chunkSize {
			h.chunks = h.chunk.Sum(h.chunks)
			h.chunk.Reset()
			h.inChunk = 0
		}
	}
	return n, nil
}

// HashReader reads to the end, and gives the whole-file hash and the chunk hashes (specs.md §16).
func HashReader(ctx context.Context, r io.Reader) (file [HashSize]byte, chunks []byte, err error) {
	h, err := NewHasher(ChunkSize)
	if err != nil {
		return file, nil, err
	}
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return file, nil, fmt.Errorf("integrity: %w", err)
		}
		n, err := r.Read(buffer)
		if n > 0 {
			if _, err := h.Write(buffer[:n]); err != nil {
				return file, nil, err
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			file, chunks = h.Sum()
			return file, chunks, nil
		case err != nil:
			return file, nil, fmt.Errorf("integrity: %w", err)
		}
	}
}

// ChunkSum is the hash of one chunk on its own, as a Hasher records it: what a copy checks a chunk
// against, one at a time (specs.md §14, §16).
func ChunkSum(chunk []byte) [HashSize]byte {
	return blake3.Sum256(chunk)
}

// Sum returns the whole-file hash and the chunk hashes so far, concatenated. A
// file has as many chunks as it takes to hold its bytes: none when it is empty,
// and no empty chunk after a size that is an exact multiple. Sum changes nothing,
// so more writes may follow.
func (h *Hasher) Sum() (file [HashSize]byte, chunks []byte) {
	copy(file[:], h.file.Sum(nil))
	chunks = slices.Clone(h.chunks)
	if h.inChunk > 0 {
		chunks = h.chunk.Sum(chunks)
	}
	return file, chunks
}
