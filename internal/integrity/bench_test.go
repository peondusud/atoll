package integrity

import (
	"bytes"
	"runtime"
	"testing"

	"golang.org/x/sys/cpu"
)

// The throughput of Atoll's hashing on the machine that runs it, which the CI's arm64 and amd64 jobs log
// (docs/research/blake3.md): no I/O, so it measures the CPU alone.
//
//	go test -run '^$' -bench . ./internal/integrity

// blake3Backend names the code github.com/zeebo/blake3 takes on this CPU for an input of several chunks, as its
// internal/consts and internal/alg/hash choose it, unless an environment variable of its own turns one off.
func blake3Backend() string {
	switch {
	case cpu.X86.HasAVX512 && cpu.X86.HasAVX512VL:
		return "AVX-512"
	case cpu.X86.HasAVX2:
		return "AVX2"
	case cpu.ARM64.HasSVE2:
		return "SVE2"
	case runtime.GOARCH == "arm64":
		return "NEON"
	case cpu.X86.HasSSE41:
		return "SSE4.1 for one chunk, pure Go for several"
	default:
		return "pure Go"
	}
}

// One read of a file feeding its whole-file hash and its chunk hashes, as a scan and a copy do (specs.md §16): 64 MiB,
// read 1 MiB at a time.
func BenchmarkHashReader(b *testing.B) {
	b.Logf("BLAKE3 backend: %s, on %s/%s", blake3Backend(), runtime.GOOS, runtime.GOARCH)
	data := pattern(64 << 20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, _, err := HashReader(b.Context(), bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

// One chunk on its own, as a scrub and a chunk repair check it (specs.md §16).
func BenchmarkChunkSum(b *testing.B) {
	data := pattern(ChunkSize)
	b.SetBytes(ChunkSize)
	for b.Loop() {
		ChunkSum(data)
	}
}
