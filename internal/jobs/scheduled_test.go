package jobs

import (
	"testing"
	"time"
)

// The copy of a file in scheduled mode runs only inside replication.window: outside it, it waits in the queue,
// counts as waiting, and the pass says when the window opens; atoll replicate --now runs it whatever the time. A file
// in another mode is copied as ever (specs.md §12.3, §14.1).
func TestScheduledCopiesWaitForTheirWindow(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nmode = \"scheduled\"\nsettle = \"0s\"\n[replication]\nwindow = \"02:00-04:00\"\n"+
		"[[rule]]\nlabel = \"now\"\npath = \"now\"\nmode = \"deferred\"\n", "t1", "t2")
	// The window is read in the local time zone, timezone being left out: half past midnight is outside it anywhere.
	b.now = time.Date(2026, 1, 1, 0, 30, 0, 0, time.Local).UnixNano()
	b.write("t1", "a", "x")
	b.write("t1", "now", "y")
	b.scan(true)
	out := b.pass()
	if got := outcomeOf(out, KindReplicate, "a"); got != "" {
		t.Errorf("a copied outside its window: %q", got)
	}
	if got := outcomeOf(out, KindReplicate, "now"); got != OutcomeReplicated {
		t.Errorf("a deferred file: %q", got)
	}
	opens := time.Date(2026, 1, 1, 2, 0, 0, 0, time.Local).UnixNano()
	if out.Waiting != 1 || !out.NextDue.Valid || out.NextDue.Int64 != opens {
		t.Errorf("waiting %d, next due %v, want 1 and the window's start %d", out.Waiting, out.NextDue, opens)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'replicate' AND state = 'queued'"); got != 1 {
		t.Errorf("%d jobs queued", got)
	}
	b.now = time.Date(2026, 1, 1, 2, 30, 0, 0, time.Local).UnixNano()
	out = b.pass()
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeReplicated {
		t.Errorf("inside its window: %q", got)
	}
	if b.read("t2", "a") != "x" {
		t.Error("not copied")
	}

	// After the window, a new file waits again, and --now copies it at once.
	b.now = time.Date(2026, 1, 1, 5, 0, 0, 0, time.Local).UnixNano()
	b.write("t1", "b", "z")
	b.scan(true)
	if got := outcomeOf(b.pass(), KindReplicate, "b"); got != "" {
		t.Errorf("b copied outside its window: %q", got)
	}
	items, _ := b.replicate("b", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Errorf("--now: %+v", items)
	}
}
