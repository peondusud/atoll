package jobs

import (
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// farNS is a time no slice reaches.
const farNS = int64(1) << 62

// passLater runs a pass as the daemon that runs for good does: it reads no file whole (specs.md §15.13).
func (b *bench) passLater() *Outcome {
	b.t.Helper()
	out, err := RunLater(b.t.Context(), b.db, b.config, b, true, nil, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// readChanged decides what a pass left to read, until a time, and gives what it had no time for.
func (b *bench) readChanged(pending []PendingRead, until int64) (*Outcome, []PendingRead) {
	b.t.Helper()
	out, left, err := ReadChanged(b.t.Context(), b.db, b.config, b, nil, pending, until)
	if err != nil {
		b.t.Fatal(err)
	}
	return out, left
}

// countReads counts the reads of file content on every disk of the bench.
func (b *bench) countReads() *int {
	reads := new(int)
	for _, dir := range b.dirs {
		b.fail[dir] = func(call, _ string) error {
			if call == "read" {
				*reads++
			}
			return nil
		}
	}
	return reads
}

// A pass of the daemon reads no file whole (specs.md §15.13): a file whose changed copy must be read to be decided
// is left to the slices that follow the pass, and decided there as a pass would have. The pass read it itself, under
// the catalog's lock: after a chmod -R or a touch over a library, that was every file, hours for a few TB during which
// no command ran and no mount was checked.
func TestAPassOfTheDaemonReadsNoFileWhole(t *testing.T) {
	b := touched(t)
	reads := b.countReads()
	out := b.passLater()
	if len(out.Decisions)+len(out.Operations) != 0 || *reads != 0 || len(out.ToRead) != 1 || out.ToRead[0].AnswerID != 0 {
		t.Fatalf("%+v, %d reads", out, *reads)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 0 {
		t.Fatalf("%d rows changed with nothing read", got)
	}
	slice, left := b.readChanged(out.ToRead, b.now+30_000_000_000)
	if !slices.Equal(slice.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(slice.Operations, []Operation{timeTo("t2", OutcomeApplied)}) ||
		len(left) != 0 || slice.Partial || *reads == 0 {
		t.Fatalf("%+v, %d left, %d reads", slice, len(left), *reads)
	}
	if got := b.mtimeOf("t2", "a"); got != touchedNS {
		t.Errorf("t2 holds %d", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ? AND verified_ns IS NOT NULL", touchedNS); got != 2 {
		t.Errorf("%d rows say the new time, verified", got)
	}
	if out := b.passLater(); len(out.Decisions)+len(out.Operations)+len(out.ToRead) != 0 {
		t.Errorf("then %+v", out)
	}
}

// What takes no read is decided in the pass, as before: a copy of another size is a modification.
func TestAPassOfTheDaemonDecidesWhatTakesNoRead(t *testing.T) {
	b := pool(t)
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.passLater()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) || len(out.ToRead) != 0 {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "longer" {
		t.Error("the other copy was not refreshed")
	}
}

// A dry run records nothing, so its pass still reads what it must to say what it would decide.
func TestADryRunStillReadsInItsPass(t *testing.T) {
	b := touched(t)
	out, err := RunLater(t.Context(), b.db, b.config, b, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.ToRead) != 0 {
		t.Errorf("%+v", out)
	}
}

// A slice stops at its time, between two files, and gives what it had no time for, in the same order.
func TestASliceOfReadsStopsAtItsTime(t *testing.T) {
	b := guarded(t, "files = 100\npercent = \"100%\"\n", "a", "b", "c")
	b.rewrite("a", "b", "c")
	out := b.passLater()
	if len(out.ToRead) != 3 || len(out.Decisions) != 0 {
		t.Fatalf("%+v", out)
	}
	b.at["modified"] = func() { b.now += int64(time.Minute) }
	slice, left := b.readChanged(out.ToRead, b.now+30_000_000_000)
	if !slices.Equal(decisionsOf(slice), []Decision{{Path: "a", Kind: "modified"}}) || !slices.Equal(left, out.ToRead[1:]) {
		t.Fatalf("%+v, left %+v of %+v", slice, left, out.ToRead)
	}
	slice, left = b.readChanged(left, farNS)
	if want := []Decision{{Path: "b", Kind: "modified"}, {Path: "c", Kind: "modified"}}; !slices.Equal(decisionsOf(slice), want) || len(left) != 0 {
		t.Fatalf("then %+v, left %+v", slice, left)
	}
	// The copies made stale are for the pass that follows the slices.
	if b.read("t2", "a") != "x" {
		t.Error("a slice ran a job")
	}
	if out := b.passLater(); len(out.ToRead) != 0 || b.read("t2", "a") != "z" || b.read("t2", "c") != "z" {
		t.Errorf("the pass that follows: %+v", out)
	}
}

// A slice looks at each copy again before anything is read: one that changed again since the pass is not due, and
// waits, the slice saying when it is; one that is gone leaves its file to the next pass.
func TestASliceLooksAgainAtWhatItReads(t *testing.T) {
	b := touched(t)
	out := b.passLater()
	b.changeAgain("t1", "a", "z")
	due := b.ctimeOf("t1", "a") + settleNS
	slice, left := b.readChanged(out.ToRead, farNS)
	if len(slice.Decisions)+len(slice.Operations)+len(left) != 0 || slice.Waiting != 1 || !slice.NextDue.Valid || slice.NextDue.Int64 != due {
		t.Fatalf("%+v, want one waiting until %d", slice, due)
	}

	b = touched(t)
	out = b.passLater()
	b.remove("t2", "a")
	slice, left = b.readChanged(out.ToRead, farNS)
	if len(slice.Decisions)+len(slice.Operations)+len(left) != 0 {
		t.Fatalf("a copy gone: %+v", slice)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 0 {
		t.Errorf("%d rows changed", got)
	}
}

// The mass-modification guard counts in the slices as in a pass: the decision that would trip it is not applied, what
// comes after it is not read, and the pass that follows queues nothing the guard holds (specs.md §15.6).
func TestTheModifyGuardTripsInASlice(t *testing.T) {
	b := guarded(t, "files = 1\npercent = \"100%\"\nwindow = \"10m\"\n", "a", "b", "c")
	b.rewrite("a", "b", "c")
	out := b.passLater()
	if len(out.ToRead) != 3 || out.Held {
		t.Fatalf("%+v", out)
	}
	slice, left := b.readChanged(out.ToRead, farNS)
	if !slices.Equal(decisionsOf(slice), []Decision{{Path: "a", Kind: "modified"}}) || !slice.Held || len(left) != 0 {
		t.Fatalf("%+v, left %+v", slice, left)
	}
	if want := []GuardLine{{Disk: "t1", Kind: GuardModifications, Count: 1}}; !slices.Equal(slice.Guards, want) {
		t.Errorf("guards %+v", slice.Guards)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'modify-guard'"); got != 1 {
		t.Errorf("%d guard events", got)
	}
	out = b.passLater()
	if !out.Held || len(out.ToRead) != 0 || b.read("t2", "a") != "z" || b.read("t2", "b") != "x" {
		t.Fatalf("the pass that follows: %+v", out)
	}

	// An answer decides what a pass took under it, whenever its slices come: the window is counted to the start of
	// that pass, and a batch that takes hours to read is asked about once.
	if _, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardConfirm, b.now); err != nil {
		t.Fatal(err)
	}
	out = b.passLater()
	if len(out.ToRead) != 2 || out.ToRead[0].AnswerID == 0 || out.ToRead[1].AnswerID != out.ToRead[0].AnswerID || out.Held {
		t.Fatalf("after the answer: %+v", out)
	}
	b.now += int64(11 * time.Minute)
	slice, left = b.readChanged(out.ToRead, farNS)
	if want := []Decision{{Path: "b", Kind: "modified"}, {Path: "c", Kind: "modified"}}; !slices.Equal(decisionsOf(slice), want) || slice.Held || len(left) != 0 {
		t.Fatalf("past the window: %+v", slice)
	}
	if got := b.answered(); got != "0,1,1" {
		t.Errorf("answered %s", got)
	}
}

// A file taken under no answer finds none later either: one given since its pass is asked of the events, as in a pass.
func TestASliceUnderNoAnswerCounts(t *testing.T) {
	b := guarded(t, "files = 0\npercent = \"100%\"\n", "a")
	b.rewrite("a")
	out := b.passLater()
	if len(out.ToRead) != 1 || out.ToRead[0].AnswerID != 0 {
		t.Fatalf("%+v", out)
	}
	slice, _ := b.readChanged(out.ToRead, farNS)
	if len(slice.Decisions) != 0 || !slice.Held {
		t.Fatalf("%+v", slice)
	}
	if b.read("t2", "a") != "x" || b.count("SELECT count(*) FROM copies WHERE state = 'stale'") != 0 {
		t.Error("a held modification was applied")
	}
}
