package integrity

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/peondusud/atoll/internal/vectors"
)

type hashVector struct {
	Length    *int      `json:"length"`
	ChunkSize *int64    `json:"chunk_size"`
	File      *string   `json:"file"`
	Chunks    *[]string `json:"chunks"`
	Error     bool      `json:"error"`
	Note      string    `json:"note"`
}

// pattern is the input of the official BLAKE3 test vectors: bytes cycling through
// 0 to 250.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// The same bytes split in different ways must give the same hashes. A Sum in the
// middle must change nothing.
func splits() map[string][]int {
	return map[string][]int{
		"at once": nil,
		"1 byte":  {1},
		"mixed":   {1, 7, 1000, 1024, 4097, 65536, 1 << 20},
		"large":   {4<<20 - 1, 2, 4 << 20},
	}
}

func TestHasherVectors(t *testing.T) {
	for i, v := range vectors.Load[hashVector](t, "blake3.json") {
		if v.Length == nil || v.ChunkSize == nil {
			t.Errorf("vector %d (%s): want length and chunk_size", i, v.Note)
			continue
		}
		if v.Error == (v.File != nil) || (v.File == nil) != (v.Chunks == nil) {
			t.Errorf("vector %d (%s): want file and chunks, or error", i, v.Note)
			continue
		}
		if v.Error {
			if h, err := NewHasher(*v.ChunkSize); err == nil {
				t.Errorf("vector %d (%s): got %+v, want an error", i, v.Note, h)
			}
			continue
		}
		wantFile, err := hex.DecodeString(*v.File)
		if err != nil || len(wantFile) != HashSize {
			t.Errorf("vector %d (%s): file %q: %v", i, v.Note, *v.File, err)
			continue
		}
		var wantChunks []byte
		for _, c := range *v.Chunks {
			b, err := hex.DecodeString(c)
			if err != nil || len(b) != HashSize {
				t.Errorf("vector %d (%s): chunk %q: %v", i, v.Note, c, err)
			}
			wantChunks = append(wantChunks, b...)
		}
		data := pattern(*v.Length)
		for name, sizes := range splits() {
			if name == "1 byte" && len(data) > 1<<16 {
				continue // too slow to be worth it on the long cases
			}
			h, err := NewHasher(*v.ChunkSize)
			if err != nil {
				t.Errorf("vector %d (%s): %v", i, v.Note, err)
				continue
			}
			rest := data
			for step := 0; len(rest) > 0; step++ {
				n := len(rest)
				if sizes != nil {
					n = min(n, sizes[step%len(sizes)])
				}
				written, err := h.Write(rest[:n])
				if err != nil || written != n {
					t.Errorf("vector %d (%s, %s): Write = %d, %v, want %d", i, v.Note, name, written, err, n)
				}
				rest = rest[n:]
				h.Sum()
			}
			file, chunks := h.Sum()
			if !bytes.Equal(file[:], wantFile) {
				t.Errorf("vector %d (%s, %s): wrong file hash %x", i, v.Note, name, file)
			}
			if !bytes.Equal(chunks, wantChunks) {
				t.Errorf("vector %d (%s, %s): %d chunk bytes, want %d, or wrong ones", i, v.Note, name, len(chunks), len(wantChunks))
			}
		}
	}
}

// HashReader gives, from one read of a file to its end, the hashes of the vectors with chunks of ChunkSize bytes, the
// size a scan and a copy use (specs.md §16), however the reader splits its bytes: short reads, and the last bytes
// with io.EOF.
func TestHashReaderVectors(t *testing.T) {
	readers := map[string]func(io.Reader) io.Reader{
		"whole":         func(r io.Reader) io.Reader { return r },
		"half":          iotest.HalfReader,
		"data with EOF": iotest.DataErrReader,
		"one byte":      iotest.OneByteReader,
	}
	checked := 0
	for i, v := range vectors.Load[hashVector](t, "blake3.json") {
		if v.Length == nil || v.ChunkSize == nil || *v.ChunkSize != ChunkSize || v.Error {
			continue
		}
		wantFile, wantChunks := decodeHashes(t, v)
		data := pattern(*v.Length)
		for name, wrap := range readers {
			if name == "one byte" && len(data) > 1<<16 {
				continue // too slow to be worth it on the long cases
			}
			file, chunks, err := HashReader(context.Background(), wrap(bytes.NewReader(data)))
			if err != nil {
				t.Errorf("vector %d (%s, %s): %v", i, v.Note, name, err)
				continue
			}
			if !bytes.Equal(file[:], wantFile) || !bytes.Equal(chunks, wantChunks) {
				t.Errorf("vector %d (%s, %s): file %x and %d chunk bytes, want %x and %d", i, v.Note, name, file, len(chunks), wantFile, len(wantChunks))
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no vector of chunks of ChunkSize bytes")
	}
}

// decodeHashes returns the whole-file hash and the chunk hashes of a vector, concatenated.
func decodeHashes(t *testing.T, v hashVector) (file, chunks []byte) {
	t.Helper()
	if v.File == nil || v.Chunks == nil {
		t.Fatalf("vector %s: want file and chunks", v.Note)
	}
	file, err := hex.DecodeString(*v.File)
	if err != nil || len(file) != HashSize {
		t.Fatalf("vector %s: file %q: %v", v.Note, *v.File, err)
	}
	for _, c := range *v.Chunks {
		b, err := hex.DecodeString(c)
		if err != nil || len(b) != HashSize {
			t.Fatalf("vector %s: chunk %q: %v", v.Note, c, err)
		}
		chunks = append(chunks, b...)
	}
	return file, chunks
}

// A read that fails ends HashReader with that error, and no hashes: a part of a file is never taken for the file.
func TestHashReaderReadError(t *testing.T) {
	broken := errors.New("broken read")
	for name, r := range map[string]io.Reader{
		"at once":                 iotest.ErrReader(broken),
		"after some bytes":        io.MultiReader(bytes.NewReader(pattern(3*ChunkSize/2)), iotest.ErrReader(broken)),
		"with the last bytes":     &failingReader{data: pattern(1000), err: broken},
		"after a chunk and a bit": io.MultiReader(bytes.NewReader(pattern(ChunkSize+1)), iotest.ErrReader(broken)),
	} {
		file, chunks, err := HashReader(context.Background(), r)
		if !errors.Is(err, broken) {
			t.Errorf("%s: error %v, want the reader's", name, err)
		}
		if file != ([HashSize]byte{}) || chunks != nil {
			t.Errorf("%s: file %x and %d chunk bytes, want none", name, file, len(chunks))
		}
	}
}

// failingReader gives its bytes and its error in the same read.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

// A cancelled context ends HashReader before its next read, with the context's error and no hashes: every long loop
// can be cancelled.
func TestHashReaderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads := &countingReader{r: bytes.NewReader(pattern(10))}
	file, chunks, err := HashReader(ctx, reads)
	if !errors.Is(err, context.Canceled) || file != ([HashSize]byte{}) || chunks != nil {
		t.Errorf("cancelled before: %x, %d chunk bytes, %v, want only context.Canceled", file, len(chunks), err)
	}
	if reads.n != 0 {
		t.Errorf("cancelled before: %d reads, want none", reads.n)
	}

	// Cancelled during the first read: no other read follows.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reads = &countingReader{r: bytes.NewReader(pattern(3 << 20)), cancel: cancel}
	file, chunks, err = HashReader(ctx, reads)
	if !errors.Is(err, context.Canceled) || file != ([HashSize]byte{}) || chunks != nil {
		t.Errorf("cancelled during: %x, %d chunk bytes, %v, want only context.Canceled", file, len(chunks), err)
	}
	if reads.n != 1 {
		t.Errorf("cancelled during: %d reads, want 1", reads.n)
	}
}

// countingReader counts its reads, and calls cancel, when set, at the first.
type countingReader struct {
	r      io.Reader
	n      int
	cancel context.CancelFunc
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.n++
	if r.cancel != nil {
		r.cancel()
	}
	return r.r.Read(p)
}

// ChunkSum gives what a Hasher records for each chunk, so that a copy checked one chunk at a time is checked
// against the same hashes.
func TestChunkSumIsAHashersChunk(t *testing.T) {
	data := make([]byte, 2*ChunkSize+1000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	h, err := NewHasher(ChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write(data); err != nil {
		t.Fatal(err)
	}
	_, chunks := h.Sum()
	var want []byte
	for at := 0; at < len(data); at += ChunkSize {
		sum := ChunkSum(data[at:min(at+ChunkSize, len(data))])
		want = append(want, sum[:]...)
	}
	if !bytes.Equal(chunks, want) {
		t.Errorf("chunks differ: %d bytes, want %d", len(chunks), len(want))
	}
}

// What Sum returns belongs to the caller: changing it changes nothing in the
// Hasher. Both sizes matter: appending a short chunk's hash can hide a shared array
// behind a new one, so only an exact multiple would return it as it is.
func TestSumIsACopy(t *testing.T) {
	for _, size := range []int{6, 8} {
		h, err := NewHasher(4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Write(pattern(size)); err != nil {
			t.Fatal(err)
		}
		_, first := h.Sum()
		kept := bytes.Clone(first)
		for i := range first {
			first[i] ^= 0xff
		}
		if _, second := h.Sum(); !bytes.Equal(second, kept) {
			t.Errorf("%d bytes: changing what Sum returned changed the Hasher", size)
		}
	}
}
