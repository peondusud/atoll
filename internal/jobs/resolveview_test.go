package jobs

import (
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// resolve runs atoll conflicts resolve on the bench, with --yes, and gives each copy's outcome by disk.
func (b *bench) resolve(path, keep string) (*trash.ResolveSummary, map[string]string) {
	b.t.Helper()
	session, err := trash.Open(b.t.Context(), b.db, b.config, b, true)
	if err != nil {
		b.t.Fatal(err)
	}
	defer session.Close()
	outcomes := map[string]string{}
	sum, err := session.Resolve(b.t.Context(), path, keep, func(item *trash.ResolveItem) error {
		outcomes[item.Disk] = item.Outcome
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return sum, outcomes
}

// Flaw 26 of conformance/models/README.md (Conflicts.tla, NoVersionLost: FlawConflictViewWrite). A file in conflict,
// two versions on two disks, Atoll's own view presenting t1's; a program opens it through the view to write, and atoll
// conflicts resolve --keep t2 runs. The resolve moves no copy the view holds open (resolve-view-free, the maintainer's
// remedy, 2026-11-25): t1's copy stays, reported changed, as one that changed between the look and the move, and so
// does the conflict. What the program writes stays at the file's path; a resolve run once the program closed it sends
// that copy to the trash, as the user then asks, with no pass between: no pass takes in a write to a file in conflict,
// so a resolve that waited for one, as the SSD cache's transfer waits for any operation of the view not taken in,
// would never end (specs.md §15.7, §21.2). Once it is resolved, a pass takes the write in. The resolve sent t1's copy
// to the trash under the program, whose write went with it.
func TestAWriteOpenedThroughTheViewDuringAResolveIsKept(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a.mkv", "the film")
	b.write("t2", "a.mkv", "another cut")
	b.scan(true)
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Fatalf("the file is %s", got)
	}
	w, tree := b.viewOf(), b.treeOf()
	f, err := b.openThroughView(w, tree, "a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if f.Disk != "t1" {
		t.Fatalf("the view opened the copy on %s", f.Disk)
	}
	sum, outcomes := b.resolve("a.mkv", "t2")
	if sum.Resolved || sum.Stayed != 1 || outcomes["t1"] != trash.OutcomeChanged || outcomes["t2"] != trash.OutcomeKept {
		t.Errorf("resolved %v, stayed %d, outcomes %v", sum.Resolved, sum.Stayed, outcomes)
	}
	const late = "a write the program makes after the resolve"
	if _, err := f.WriteAt([]byte(late), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, place := range b.contentAnywhere(late) {
		if strings.Contains(place, "/trash/") {
			t.Errorf("the write through the view is in the trash: %s", place)
		}
	}
	if !b.exists("t1", "media/a.mkv") || !strings.HasPrefix(b.read("t1", "a.mkv"), late) {
		t.Error("t1's copy does not hold the program's write")
	}
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the file is %s once the resolve left a copy", got)
	}
	// The program closed it: the user's resolve now sends t1's copy, as it says, though no pass took the write in.
	b.pass()
	sum, outcomes = b.resolve("a.mkv", "t2")
	if !sum.Resolved || outcomes["t1"] != trash.OutcomeTrashed {
		t.Errorf("resolved %v, outcomes %v", sum.Resolved, outcomes)
	}
	trashed := false
	for _, place := range b.contentAnywhere(late) {
		trashed = trashed || strings.Contains(place, "/trash/")
	}
	if !trashed {
		t.Error("the version the second resolve sent is not in the trash")
	}
	b.pass()
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("the passes left the view's operations %+v", open)
	}
	if n, err := b.treeOf().Lookup(t.Context(), "a.mkv"); err != nil || n == nil || n.Disk != "t2" {
		t.Errorf("the view presents %+v (%v)", n, err)
	}
}
