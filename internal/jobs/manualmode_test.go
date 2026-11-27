package jobs

import (
	"slices"
	"testing"
	"time"
)

// A file in manual mode is copied by no pass, however long the daemon runs, its settle past, and stays unprotected;
// atoll replicate makes its copy; once a user edits the file, the pass that takes the edit in refreshes the stale copy,
// as refreshing a stale copy ignores the mode, and no atoll replicate is asked again (specs.md §12.3, §14.1, §15.4).
func TestAManualFileWaitsForReplicateAndItsRefreshForNothing(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nmode = \"manual\"\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(true)
	for range 3 {
		if got := outcomeOf(b.pass(), KindReplicate, "a"); got != "" {
			t.Fatalf("a pass copied a manual file: %q", got)
		}
		b.now += time.Hour.Nanoseconds()
	}
	if b.exists("t2", "media/a") || b.text("SELECT state FROM files") != "unprotected" {
		t.Fatalf("after three passes: t2 holds a copy %v, the file is %s", b.exists("t2", "media/a"), b.text("SELECT state FROM files"))
	}
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if got := b.text("SELECT state FROM files"); got != "ok" || b.read("t2", "a") != "x" {
		t.Fatalf("after atoll replicate: the file is %s, t2 holds %q", got, b.read("t2", "a"))
	}
	b.changeAgain("t1", "a", "y")
	b.now = b.ctimeOf("t1", "a") + time.Minute.Nanoseconds()
	out := b.pass()
	if !slices.Contains(out.Decisions, Decision{Path: "a", Kind: "modified"}) {
		t.Fatalf("the edit was not taken in: %+v", out.Decisions)
	}
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeReplicated || b.read("t2", "a") != "y" {
		t.Errorf("the stale copy of a manual file waits for atoll replicate: %q, t2 holds %q", got, b.read("t2", "a"))
	}
}
