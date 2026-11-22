package trash

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/watch"
)

// conflicted gives the bench a file c in conflict: one version on t1 and t2, another, longer, on t3.
func conflicted(t *testing.T) *bench {
	t.Helper()
	b := newBench(t)
	b.write("t1", "c", "one")
	b.write("t2", "c", "one")
	b.write("t3", "c", "three")
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if got := b.text("SELECT state FROM files WHERE lpath = x'63'"); got != "conflict" {
		t.Fatalf("the file is %s", got)
	}
	return b
}

// resolved runs conflicts resolve on c, keeping the copy on keep, and gives each copy's outcome by disk, in the order
// given.
func (b *bench) resolved(keep string, apply bool) ([]ResolveItem, *ResolveSummary) {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	var items []ResolveItem
	summary, err := s.Resolve(b.t.Context(), "c", keep, func(i *ResolveItem) error {
		items = append(items, *i)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return items, summary
}

func outcomesOf(items []ResolveItem) string {
	out := ""
	for _, i := range items {
		out += i.Disk + ":" + i.Outcome + " "
	}
	return out
}

// What resolve refuses, it refuses before it looks at a disk or journals anything (specs.md §15.7).
func TestAResolutionRefusedLooksAtNoDisk(t *testing.T) {
	b := conflicted(t)
	s := b.opened()
	b.confirm = map[string]int{}
	for _, tc := range []struct {
		path, keep string
		want       error
	}{
		{"none", "t1", ErrNoFile},
		{"a", "t1", ErrNotConflict},
		{"c", "t4", ErrNoCopy},
	} {
		if _, err := s.Resolve(t.Context(), tc.path, tc.keep, func(*ResolveItem) error { return nil }); !errors.Is(err, tc.want) {
			t.Errorf("%s on %s: %v", tc.path, tc.keep, err)
		}
	}
	if len(b.confirm) != 0 || b.count("SELECT count(*) FROM ops") != 0 {
		t.Errorf("a refusal looked at %v, and journaled %d operations", b.confirm, b.count("SELECT count(*) FROM ops"))
	}
}

// The copy kept comes first, then the others by the position of their disks; a run that may not write says what
// would go and moves nothing; with --yes every other copy goes to the trash, overwritten, and the conflict is over
// (specs.md §15.7).
func TestAResolutionKeepsOneVersion(t *testing.T) {
	b := conflicted(t)
	items, summary := b.resolved("t3", false)
	if got := outcomesOf(items); got != "t3:kept t1:to-trash t2:to-trash " {
		t.Errorf("a dry run: %s", got)
	}
	if summary.Applied || summary.ToDo != 2 || summary.Bytes != 6 || summary.Resolved {
		t.Errorf("a dry run: %+v", summary)
	}
	if b.count("SELECT count(*) FROM ops") != 0 || b.read("t1", "c") != "one" {
		t.Error("a dry run moved something")
	}
	items, summary = b.resolved("t3", true)
	if got := outcomesOf(items); got != "t3:kept t1:trashed t2:trashed " {
		t.Errorf("with --yes: %s", got)
	}
	if !summary.Resolved || summary.Bytes != 6 || summary.DiskLost {
		t.Errorf("with --yes: %+v", summary)
	}
	if b.read("t1", "c") != "" || b.read("t2", "c") != "" || b.read("t3", "c") != "three" {
		t.Error("a version other than the one kept stays")
	}
	if got := b.text("SELECT group_concat(reason, ',') FROM trash"); got != "overwritten,overwritten" {
		t.Errorf("the trash holds %s", got)
	}
	// The file takes the kept copy's size, its version now the file's.
	if got := b.text("SELECT (state <> 'conflict') || ' ' || size FROM files WHERE lpath = x'63'"); got != "1 5" {
		t.Errorf("the file is no conflict, of 5 bytes: %s", got)
	}
}

// A copy that left its disk since the last walk has nothing to send: its row goes, and the conflict ends with what is
// left. A run that may not write counts it among what it would do.
func TestAResolutionOfACopyGone(t *testing.T) {
	b := conflicted(t)
	if err := os.Remove(filepath.Join(b.dirs["t2"], "media", "c")); err != nil {
		t.Fatal(err)
	}
	items, summary := b.resolved("t3", false)
	if got := outcomesOf(items); got != "t3:kept t1:to-trash t2:gone " || summary.ToDo != 2 {
		t.Errorf("a dry run: %s, %+v", got, summary)
	}
	if b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = x'63'") != 3 {
		t.Error("a dry run dropped a row")
	}
	items, summary = b.resolved("t3", true)
	if got := outcomesOf(items); got != "t3:kept t1:trashed t2:gone " || !summary.Resolved {
		t.Errorf("with --yes: %s, %+v", got, summary)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND file_id = (SELECT id FROM files WHERE lpath = x'63')"); got != 0 {
		t.Errorf("%d rows for the copy gone", got)
	}
}

// A copy to keep that cannot be reached, or that is gone, keeps every version where it is: nothing says what the file
// is to be, and the conflict stays for a run that can (specs.md §15.7).
func TestAResolutionWhoseKeptCopyIsOutOfReach(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		b := conflicted(t)
		b.unread[b.dirs["t3"]] = true
		items, summary := b.resolved("t3", true)
		if got := outcomesOf(items); got != "t3:disk-unavailable t1:not-moved t2:not-moved " {
			t.Errorf("%s", got)
		}
		if summary.Resolved || summary.Stayed != 3 || !summary.DiskLost {
			t.Errorf("%+v", summary)
		}
		if b.count("SELECT count(*) FROM ops") != 0 || b.read("t1", "c") != "one" {
			t.Error("something moved")
		}
	})
	t.Run("gone", func(t *testing.T) {
		b := conflicted(t)
		if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "c")); err != nil {
			t.Fatal(err)
		}
		items, summary := b.resolved("t3", true)
		if got := outcomesOf(items); got != "t3:gone t1:not-moved t2:not-moved " || summary.Resolved {
			t.Errorf("%s, %+v", got, summary)
		}
		if got := b.text("SELECT state FROM files WHERE lpath = x'63'"); got != "conflict" {
			t.Errorf("the file is %s", got)
		}
	})
}

// Another copy on a disk that cannot be reached stays, and so does the conflict: running it again finishes it.
func TestAResolutionThatCannotReachAnotherCopy(t *testing.T) {
	b := conflicted(t)
	b.unread[b.dirs["t2"]] = true
	items, summary := b.resolved("t3", true)
	if got := outcomesOf(items); got != "t3:kept t1:trashed t2:disk-unavailable " {
		t.Errorf("%s", got)
	}
	if summary.Resolved || summary.Stayed != 1 || !summary.DiskLost {
		t.Errorf("%+v", summary)
	}
	if got := b.text("SELECT state FROM files WHERE lpath = x'63'"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
	delete(b.unread, b.dirs["t2"])
	b.exec("UPDATE disks SET state = 'online' WHERE id = 't2'")
	items, summary = b.resolved("t3", true)
	if got := outcomesOf(items); got != "t3:kept t2:trashed " || !summary.Resolved {
		t.Errorf("again: %s, %+v", got, summary)
	}
}

// A copy that cannot be looked at now is out of reach, and keeps the conflict; a disk that fails at that look is lost.
func TestAResolutionThatCannotLookAtACopy(t *testing.T) {
	for _, tc := range []struct {
		err  syscall.Errno
		disk string
	}{{syscall.EACCES, "online"}, {syscall.EIO, "missing"}} {
		b := conflicted(t)
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "stat" && name == "c" {
				return tc.err
			}
			return nil
		}
		items, summary := b.resolved("t3", true)
		if got := outcomesOf(items); got != "t3:kept t1:trashed t2:disk-unavailable " || summary.Resolved {
			t.Errorf("%v: %s, %+v", tc.err, got, summary)
		}
		if b.diskState("t2") != tc.disk || b.read("t2", "c") != "one" {
			t.Errorf("%v: t2 is %s", tc.err, b.diskState("t2"))
		}
	}
}

// A resolution stopped once every other copy went, before the conflict was ended, leaves the file in conflict with its
// kept copy alone: a run that may not write says so and writes nothing, the conflict left in the catalog as it is;
// one with --yes ends it (specs.md §15.7).
func TestADryRunAfterAResolutionStoppedEndsNothing(t *testing.T) {
	b := conflicted(t)
	s := b.opened()
	stop := errors.New("stop")
	seen := 0
	if _, err := s.Resolve(t.Context(), "c", "t3", func(*ResolveItem) error {
		// The kept copy, then t1 and t2, each reported once it went.
		if seen++; seen == 3 {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatalf("stopped at the last copy: %v", err)
	}
	s.Close()
	if got := b.count("SELECT count(*) FROM copies WHERE file_id = (SELECT id FROM files WHERE lpath = x'63')"); got != 1 {
		t.Fatalf("%d copies left, not the kept one alone", got)
	}
	items, summary := b.resolved("t3", false)
	if got := outcomesOf(items); got != "t3:kept " || summary.Resolved || summary.Applied || summary.ToDo != 0 {
		t.Errorf("a dry run: %s, %+v", got, summary)
	}
	if got := b.text("SELECT state FROM files WHERE lpath = x'63'"); got != "conflict" {
		t.Errorf("a dry run left the file %s", got)
	}
	_, summary = b.resolved("t3", true)
	if !summary.Resolved {
		t.Errorf("with --yes: %+v", summary)
	}
	if got := b.text("SELECT state FROM files WHERE lpath = x'63'"); got == "conflict" {
		t.Error("with --yes, the file is still in conflict")
	}
}

// What the caller's function returns stops the resolution, with it.
func TestAResolutionStops(t *testing.T) {
	stop := errors.New("stop")
	for _, n := range []int{1, 2} {
		b := conflicted(t)
		s := b.opened()
		seen := 0
		_, err := s.Resolve(t.Context(), "c", "t3", func(*ResolveItem) error {
			if seen++; seen == n {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) {
			t.Errorf("stopped at %d: %v", n, err)
		}
	}
	b := conflicted(t)
	b.unread[b.dirs["t3"]] = true
	for _, n := range []int{1, 2} {
		s := b.opened()
		seen := 0
		if _, err := s.Resolve(t.Context(), "c", "t3", func(*ResolveItem) error {
			if seen++; seen == n {
				return stop
			}
			return nil
		}); !errors.Is(err, stop) {
			t.Errorf("out of reach, stopped at %d: %v", n, err)
		}
	}
}
