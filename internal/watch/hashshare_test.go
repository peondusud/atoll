package watch

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
)

// errKilled stands for kill -9: the scan stops where it is, and nothing after that point runs.
var errKilled = errors.New("killed")

// killedAt scans with --full on a clock that moves by tick at each file opened for its hash, and stops as kill -9
// would when the file named is opened. It gives the files that have a hash by then, in byte order.
func (b *bench) killedAt(name string, tick int64) string {
	b.t.Helper()
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return fsx.Fault(d, func(call, n string) error {
			if call != "openfile" {
				return nil
			}
			if n == name {
				panic(errKilled) //nolint:forbidigo // what stands for kill -9
			}
			b.now += tick
			return nil
		})
	}
	func() {
		defer func() {
			if err, ok := recover().(error); !ok || !errors.Is(err, errKilled) {
				b.t.Fatalf("the scan did not stop at %s: %v", name, err)
			}
		}()
		_, _ = Scan(b.t.Context(), b.db, b.config, b, Options{Full: true})
	}()
	b.wrap = nil
	return b.text("SELECT COALESCE(group_concat(CAST(lpath AS TEXT), ','), '') FROM (SELECT lpath FROM files WHERE blake3 IS NOT NULL ORDER BY lpath)")
}

// The hashes of the files read within a second share a transaction (specs.md §15.10): a scan killed keeps what the
// seconds before it held, and reads again what the second under way had read. Each file had a transaction of its own:
// on a library of small files, a sync of the catalog for each, 31 minutes for 400,000 copies of a few bytes.
func TestTheFilesReadWithinASecondShareATransaction(t *testing.T) {
	b := newBench(t, "t1")
	for _, name := range strings.Split("a b c d e f g h i", " ") {
		b.write("t1", name, "x"+name)
	}
	// 400 ms a file: a, b, c and d are read within a second and a bit, and committed with d; e, f and g wait for h.
	if got := b.killedAt("h", 400_000_000); got != "a,b,c,d" {
		t.Errorf("killed at h, the catalog holds the hashes of %q, want a, b, c and d", got)
	}
	// The next scan reads the rest, and what was lost.
	b.scan(Options{Full: true})
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NULL"); got != 0 {
		t.Errorf("%d files without a hash after the scan that follows", got)
	}
}

// A file of more than a chunk has a transaction of its own: what waits is committed before it is read, and its own
// hashes once it is, so that an interruption loses no more than the second under way and the file being read.
func TestALargeFileHasATransactionOfItsOwn(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "xa")
	b.write("t1", "b", "xb")
	b.write("t1", "c", strings.Repeat("c", integrity.ChunkSize+1))
	b.write("t1", "d", "xd")
	b.write("t1", "e", "xe")
	// The clock does not move: only the size of c commits anything before e is opened.
	if got := b.killedAt("e", 0); got != "a,b,c" {
		t.Errorf("killed at e, the catalog holds the hashes of %q, want a and b, committed before c was read, and c", got)
	}
}

// What was read is kept when the reading stops for something else than a kill: an error, or a scan canceled.
func TestWhatWasReadIsKeptWhenTheReadingStops(t *testing.T) {
	b := newBench(t, "t1")
	for _, name := range strings.Split("a b c d", " ") {
		b.write("t1", name, "x"+name)
	}
	b.wrap = refusing("t1", "d", syscall.EMFILE)
	if _, err := Scan(t.Context(), b.db, b.config, b, Options{Full: true}); !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("the scan ends with %v", err)
	}
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 3 {
		t.Errorf("%d hashes kept, want those of a, b and c", got)
	}
}
