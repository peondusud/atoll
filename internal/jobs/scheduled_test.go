package jobs

import (
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/trash"
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

// The copy that replaces a stale one waits for no window: refreshing stale copies ignores the mode (specs.md §12.3).
// The stale copy goes to the trash at once, so that mergerfs never presents it, and until its replacement is made
// the file has a copy less. That replacement waited for the window, the next night for a film edited in the morning.
func TestARefreshOfAScheduledFileWaitsForNoWindow(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nmode = \"scheduled\"\nsettle = \"0s\"\n[replication]\nwindow = \"02:00-04:00\"\n", "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(true)
	b.changeAgain("t1", "a", "y")
	// Half past midnight of the day after the edit, in the local time zone: past its settle, and outside the window.
	edited := time.Unix(0, b.ctimeOf("t1", "a")).In(time.Local)
	b.now = time.Date(edited.Year(), edited.Month(), edited.Day()+1, 0, 30, 0, 0, time.Local).UnixNano()
	out := b.pass()
	if !slices.Contains(out.Decisions, Decision{Path: "a", Kind: "modified"}) || !slices.Contains(out.Operations, overwriteOf("t2", trash.OutcomeTrashed)) {
		t.Fatalf("%+v", out)
	}
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeReplicated {
		t.Errorf("the copy that replaces a stale one, outside the window: %q", got)
	}
	if !b.exists("t2", "media/a") || b.read("t2", "a") != "y" {
		t.Error("the stale copy is in the trash, and nothing is in its place")
	}
	if out.Waiting != 0 {
		t.Errorf("%d copies waiting", out.Waiting)
	}
}
