package jobs

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"

	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// settleNS is the settle of the bench's configuration: the default, 10m (specs.md §12.3).
const settleNS = 600_000_000_000

// reconfigure gives the bench another configuration: the same disks, with more after them.
func (b *bench) reconfigure(more string) {
	b.t.Helper()
	c, err := config.Parse([]byte(b.base + more))
	if err != nil {
		b.t.Fatal(err)
	}
	b.config = c
}

func replicated(path string) Operation {
	return Operation{Disk: "t2", Kind: KindReplicate, Path: path, Source: "t1", Outcome: OutcomeReplicated}
}

// A pass queues nothing before settle has passed since the file was first seen, and says when it will;
// the first pass at that time queues the copy and runs it (specs.md §14.1, §15.12).
func TestAPassCopiesOnceSettled(t *testing.T) {
	b := replicatePool(t)
	due := b.now - 60_000_000_000 + settleNS
	out := b.pass()
	if len(out.Operations) != 0 || out.Waiting != 1 || !out.NextDue.Valid || out.NextDue.Int64 != due {
		t.Fatalf("%+v, want one waiting until %d", out, due)
	}
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); got != 0 {
		t.Errorf("%d queued before settle", got)
	}
	b.now = due - 1
	if out := b.pass(); len(out.Operations) != 0 || out.Waiting != 1 {
		t.Fatalf("a nanosecond early: %+v", out)
	}
	b.now = due
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{replicated("a")}) || out.Waiting != 0 || out.NextDue.Valid || out.Partial {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "content" {
		t.Error("not copied")
	}
	// The pass worked the file out again after the copy.
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("the file is %s", got)
	}
	if out := b.pass(); len(out.Operations) != 0 || out.Waiting != 0 {
		t.Errorf("then %+v", out)
	}
}

// What a pass says waits is the earliest of a deletion's debounce and a copy's settle, and counts both.
func TestWhatWaitsIsTheEarliest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove int64 // when the copy goes, after the scan
		first  bool  // whether the deletion is due first
	}{
		{"the debounce first", 0, true},
		{"settle first", settleNS - 10_000_000_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.write("t1", "a", "x")
			b.write("t2", "a", "x")
			b.write("t1", "b", "y")
			first := b.now
			b.scan(true)
			b.now = first + tc.remove
			b.remove("t2", "a")
			out := b.pass()
			want := first + settleNS
			if tc.first {
				want = b.now + 30_000_000_000
			}
			if out.Waiting != 2 || !out.NextDue.Valid || out.NextDue.Int64 != want {
				t.Errorf("waiting %d until %v, want 2 until %d", out.Waiting, out.NextDue, want)
			}
		})
	}
}

// A copy queued from a copy that a look then found gone has no source: its job goes with its pending copy, with
// nothing journaled, and no plan queues another until the file is decided, which a tripped guard can put off for
// days (specs.md §13.4, §14.1).
func TestACopyWhoseSourceIsGone(t *testing.T) {
	b := replicatePool(t)
	b.replicate("a", ReplicateOptions{})
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "a")); err != nil {
		t.Fatal(err)
	}
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{{Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: trash.OutcomeChanged}}) {
		t.Fatalf("%+v", out.Operations)
	}
	for _, again := range []bool{false, true} {
		if again {
			if out := b.pass(); len(out.Operations) != 0 {
				t.Errorf("then %+v", out.Operations)
			}
		}
		if n := b.count("SELECT count(*) FROM ops"); n != 0 {
			t.Errorf("%d operations journaled", n)
		}
		if n := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); n != 0 {
			t.Errorf("%d jobs and pending copies left", n)
		}
	}
}

// A copy queued for later runs at its time, and until then counts as waiting.
func TestACopyQueuedForLater(t *testing.T) {
	b := replicatePool(t)
	b.replicate("a", ReplicateOptions{})
	later := b.now + 300_000_000_000
	b.exec("UPDATE jobs SET not_before_ns = ?", later)
	out := b.pass()
	if len(out.Operations) != 0 || out.Waiting != 1 || out.NextDue.Int64 != later {
		t.Fatalf("%+v, want it waiting until %d", out, later)
	}
	b.now = later
	if out := b.pass(); !slices.Equal(out.Operations, []Operation{replicated("a")}) || out.Waiting != 0 {
		t.Fatalf("%+v", out)
	}
}

// A copy that fails in a pass says how, and the exit code says partial for what may need a disk to come
// back (specs.md §15.12).
func TestAPassCopyThatFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arm     func(b *bench)
		outcome string
		partial bool
	}{
		{"no room", failOn("t2", "write", syscall.ENOSPC), OutcomeNoRoom, false},
		{"a disk lost while writing", failOn("t2", "write", syscall.EIO), "io-error", true},
		{"a disk lost at the rename", failOn("t2", "rename", syscall.EIO), "io-error", true},
		{"a name taken since the look", at("verified", func(b *bench) { b.write("t2", "a", "theirs") }), OutcomeConflict, false},
		{"a source changed", at("copied", func(b *bench) { b.write("t1", "a", "changed!") }), "changed", false},
		{"a source that cannot be read", failOn("t1", "read", syscall.EIO), "io-error", true},
		// No disk is lost: the outcome alone says partial.
		{"a copy that reads back wrong", at("copied", func(b *bench) {
			if err := os.WriteFile(filepath.Join(b.dirs["t2"], "Atoll", "tmp", "1.part"), []byte("CONTENT"), 0o600); err != nil {
				b.t.Fatal(err)
			}
		}), "io-error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := replicatePool(t)
			b.replicate("a", ReplicateOptions{})
			tc.arm(b)
			out := b.pass()
			want := []Operation{{Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: tc.outcome}}
			if !slices.Equal(out.Operations, want) || out.Partial != tc.partial {
				t.Errorf("%+v, partial %v; want %+v, partial %v", out.Operations, out.Partial, want, tc.partial)
			}
			// Only a source that changed waits, for settle: what failed otherwise is due again at once.
			var waiting int64
			if tc.outcome == trash.OutcomeChanged {
				waiting = 1
				if !out.NextDue.Valid || out.NextDue.Int64 != b.now+settleNS {
					t.Errorf("due at %v, want %d", out.NextDue, b.now+settleNS)
				}
			}
			if out.Waiting != waiting {
				t.Errorf("%d waiting, want %d", out.Waiting, waiting)
			}
		})
	}
}

// A file the plan wants two copies of waits once for settle.
func TestAFileWaitsOnceForItsCopies(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nreplicas = 2\n", "t1", "t2", "t3")
	b.write("t1", "a", "content")
	b.scan(false)
	if out := b.pass(); out.Waiting != 1 {
		t.Errorf("%d waiting, want 1", out.Waiting)
	}
	b.now += settleNS
	if out := b.pass(); len(out.Operations) != 2 {
		t.Errorf("%+v, want two copies", out.Operations)
	}
}

// Copies whose source is missing cannot run in this pass: partial, each said once, and their jobs wait.
func TestAPassCopyWithItsSourceMissing(t *testing.T) {
	b := replicatePool(t)
	b.write("t1", "b", "more")
	b.scan(false)
	b.replicate("", ReplicateOptions{})
	b.gone[b.dirs["t1"]] = true
	out := b.pass()
	// In the order the plan queued them: the smaller file first.
	want := []Operation{
		{Disk: "t2", Kind: KindReplicate, Path: "b", Source: "t1", Outcome: OutcomeUnavailable},
		{Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: OutcomeUnavailable},
	}
	if !slices.Equal(out.Operations, want) || !out.Partial {
		t.Errorf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE state = 'queued'"); got != 2 {
		t.Errorf("%d jobs queued", got)
	}
}

// A dry run lists the copies and writes nothing of its plan, even where the configuration changed since the
// last pass worked the files out (specs.md §15.12).
func TestADryRunPassWritesNothing(t *testing.T) {
	b := replicatePool(t)
	b.now += settleNS
	files := "SELECT group_concat(id || ' ' || replicas_target || ' ' || state || ' ' || coalesce(rule, ''), ',') FROM files"
	before := b.text(files)
	b.reconfigure("[[rule]]\nlabel = \"two\"\npath = \"a\"\nreplicas = 2\n")
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Operation{{Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: OutcomeToReplicate}}; !slices.Equal(out.Operations, want) {
		t.Errorf("%+v", out.Operations)
	}
	if after := b.text(files); after != before {
		t.Errorf("the files went from %q to %q", before, after)
	}
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); got != 0 {
		t.Errorf("%d written", got)
	}
}

func ioprioOfThisThread(t *testing.T) uintptr {
	t.Helper()
	got, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, uintptr(unix.Gettid()), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return got
}

// A copy of a pass runs at the lowest best-effort level, and one atoll replicate --force asks for does not
// (specs.md §14).
func TestTheIOPriorityOfACopy(t *testing.T) {
	const lowestBE = 2<<13 | 7
	b := replicatePool(t)
	b.replicate("a", ReplicateOptions{})
	var during uintptr
	b.at["copied"] = func() { during = ioprioOfThisThread(t) }
	b.pass()
	if during != lowestBE {
		t.Errorf("a pass copies at %#x, want %#x", during, lowestBE)
	}
	b.write("t1", "b", "more")
	b.scan(false)
	b.at["copied"] = func() { during = ioprioOfThisThread(t) }
	b.replicate("b", ReplicateOptions{Force: true})
	if during == lowestBE {
		t.Errorf("--force copies at %#x", during)
	}
}

// sinkFunc is a Sink that hands each operation to a function.
type sinkFunc func(op *Operation) error

func (sinkFunc) Disks([]DiskLine) error          { return nil }
func (sinkFunc) Decision(*Decision) error        { return nil }
func (f sinkFunc) Operation(op *Operation) error { return f(op) }

// A sink that fails stops the copies: what follows is not run, and the pass ends with that error.
func TestASinkThatFailsStopsTheCopies(t *testing.T) {
	b := replicatePool(t)
	b.write("t1", "b", "more")
	b.scan(false)
	b.replicate("", ReplicateOptions{})
	failed := errors.New("the output is gone")
	_, err := RunTo(t.Context(), b.db, b.config, b, true, sinkFunc(func(*Operation) error { return failed }))
	if !errors.Is(err, failed) {
		t.Fatalf("%v", err)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE state = 'queued'"); got != 1 {
		t.Errorf("%d jobs left, want the second", got)
	}
}

// A pass canceled between two copies stops there: the second waits for the next pass.
func TestACanceledPassStopsBetweenCopies(t *testing.T) {
	b := replicatePool(t)
	b.write("t1", "b", "more")
	b.scan(false)
	b.replicate("", ReplicateOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := RunTo(ctx, b.db, b.config, b, true, sinkFunc(func(*Operation) error {
		cancel()
		return nil
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE state = 'queued'"); got != 1 {
		t.Errorf("%d jobs left, want the second", got)
	}
}

// A pass killed at any step of a copy: the next pass's reprise finishes or undoes the operation, and the
// copy is made, verified, with nothing left of its job, of its operation or in tmp/ (specs.md §14.2, §17.2).
func TestAPassKilledDuringACopy(t *testing.T) {
	for _, point := range []string{"journaled", "copied", "verified", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.replicate("a", ReplicateOptions{})
			b.crashAt(point)
			out := b.pass()
			if out.Partial {
				t.Errorf("partial: %+v", out)
			}
			if b.read("t2", "a") != "content" {
				t.Error("not copied")
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok' AND verified_ns IS NOT NULL"); got != 1 {
				t.Error("the copy is not verified")
			}
			if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d left", got)
			}
			if parts := b.parts("t2"); len(parts) != 0 {
				t.Errorf("tmp/ holds %q", parts)
			}
		})
	}
}

// With replication off a pass queues and copies nothing, and counts nothing as waiting for settle.
func TestAPassWithReplicationOff(t *testing.T) {
	b := newBenchWith(t, "[replication]\nenabled = false\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	b.now += settleNS
	out := b.pass()
	if len(out.Operations) != 0 || out.Waiting != 0 || out.NextDue.Valid {
		t.Errorf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d queued", got)
	}
}

// A copy that is no longer what its row says is not read: it is for the next pass to look at (specs.md §15.10,
// §15.13). It was compared with its row only once read whole, so that a file still being written, a download of
// tens of gigabytes, was read from end to end at each slice of the hashes, every 30 s, under the catalog's lock, to
// be left aside each time.
func TestACopyThatChangedSinceItsRowIsNotRead(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.write("t1", "same", "y")
	b.scan(false)
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 0 {
		t.Fatalf("%d files hashed by a scan that reads none", got)
	}
	// Still being written: larger than its row says.
	b.write("t1", "a", "more than the row says")
	read := map[string]int{}
	b.walkFail[b.dirs["t1"]] = func(call, name string) error {
		if call == "read" {
			read[name]++
		}
		return nil
	}
	if _, err := watch.HashNew(t.Context(), b.db, b.config, b, b.now+30_000_000_000); err != nil {
		t.Fatal(err)
	}
	if read["a"] != 0 {
		t.Errorf("the copy that changed since its row was read %d times", read["a"])
	}
	if read["same"] == 0 {
		t.Error("the copy that is what its row says was not read")
	}
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT), ' ') FROM files WHERE blake3 IS NOT NULL"); got != "same" {
		t.Errorf("hashed: %q", got)
	}
}
