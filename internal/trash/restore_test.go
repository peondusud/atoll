package trash

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/watch"
)

// restoredBy restores what is at or below a path in a session that may write or not, and gives the outcomes.
func (b *bench) restoredBy(under string, apply bool) []string {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	var outcomes []string
	if _, err := s.Restore(b.t.Context(), under, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Outcome)
		return nil
	}); err != nil {
		b.t.Fatal(err)
	}
	return outcomes
}

// A restore says what it would do without --yes, and moves nothing; a disk that may only be read takes nothing back,
// and a look at the path that fails leaves the entry where it is (specs.md §17.4).
func TestARestoreThatMovesNothing(t *testing.T) {
	b := newBench(t)
	b.prune()
	if got := b.restoredBy("a", false); !slices.Equal(got, []string{OutcomeToRestore}) || b.sourceThere() {
		t.Errorf("a dry run: %v", got)
	}
	b.ro[b.dirs["t3"]] = true
	if got := b.restoredBy("a", true); !slices.Equal(got, []string{outcomeUnavailable}) || b.sourceThere() {
		t.Errorf("read-only: %v", got)
	}
	delete(b.ro, b.dirs["t3"])
	b.failOn("t3", "stat", syscall.EACCES)
	if got := b.restoredBy("a", true); !slices.Equal(got, []string{OutcomeIOError}) || b.sourceThere() || b.diskState("t3") != "online" {
		t.Errorf("a look that fails: %v, t3 %s", got, b.diskState("t3"))
	}
	if b.count("SELECT count(*) FROM ops WHERE kind = 'restore'") != 0 || b.count("SELECT count(*) FROM trash") != 1 {
		t.Error("something was journaled, or left the trash")
	}
}

// On an NTFS disk, a path whose name collides with another the catalog holds there is never restored: Windows would
// see one of the two (specs.md §15.7).
func TestARestoreThatCollidesOnNTFS(t *testing.T) {
	b := newBenchOf(t, "ntfs3")
	b.prune()
	b.write("t3", "A", "w")
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if got := b.restoredBy("a", true); !slices.Equal(got, []string{OutcomeNameCollision}) || b.sourceThere() {
		t.Errorf("%v", got)
	}
}

// A restore the reprise of this run finished is what this run was asked for: an older entry of that path stays in the
// trash, where a retry would otherwise restore it too (specs.md §17.2).
func TestARestoreFinishedByTheRepriseCountsForItsPath(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.write("t3", "a", "x")
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("the second prune: %+v", items)
	}
	newest := b.count("SELECT max(op_id) FROM trash")
	b.failOn("t3", "sync", syscall.EIO)
	if got := b.restoredBy("a", true); !slices.Equal(got, []string{OutcomeIOError}) {
		t.Fatalf("%v", got)
	}
	delete(b.fail, b.dirs["t3"])
	if got := b.restoredBy("a", true); len(got) != 0 {
		t.Errorf("the run whose reprise finished the restore: %v", got)
	}
	if !b.sourceThere() || b.count("SELECT count(*) FROM trash") != 1 || b.count("SELECT max(op_id) FROM trash") >= newest {
		t.Errorf("the copy back %v; the trash holds %d entries", b.sourceThere(), b.count("SELECT count(*) FROM trash"))
	}
}

// What the caller's function returns stops a restore, with it, whichever entry it was told of.
func TestARestoreStops(t *testing.T) {
	b := trashed(t, "d/x", "d/y")
	stop := errors.New("stop")
	for _, under := range []string{"d", "a", "d/y"} {
		s, err := Open(t.Context(), b.db, b.config, b, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Restore(t.Context(), under, func(*EntryItem) error { return stop }); !errors.Is(err, stop) {
			t.Errorf("%q: %v", under, err)
		}
		s.Close()
	}
	if b.count("SELECT count(*) FROM trash") != 3 {
		t.Error("an entry left the trash")
	}
}

// A folder a restore makes on its own disk, which someone made meanwhile, is theirs: its operation is undone, and the
// copy goes back into it (specs.md §17.4).
func TestARestoreWhoseFolderIsMadeMeanwhile(t *testing.T) {
	b := newBench(t)
	b.addFiles("Films/b")
	b.prune()
	folder := filepath.Join(b.dirs["t3"], "media", "Films")
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	b.at["journaled"] = func() {
		if err := os.Mkdir(folder, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.restoredBy("Films/b", true); !slices.Equal(got, []string{OutcomeRestored}) {
		t.Fatalf("%v", got)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "undone" || !b.there("t3", "Films/b") {
		t.Errorf("the folder's operation is %s", got)
	}
}
