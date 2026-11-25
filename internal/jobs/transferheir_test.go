package jobs

import (
	"errors"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/solver"
)

// The copy a copy leaving the SSD cache hands its user part to is a copy ok on a data disk that can be read: a user
// copy first, then the first seen, then by position; none when there is none, as the copy on the cache is then needed
// (specs.md §13.3, §19).
func TestTheHeirOfACopyLeavingTheCache(t *testing.T) {
	disk := func(id, role, state string, position int) catalog.Disk {
		return catalog.Disk{ID: id, Role: role, State: state, Position: position}
	}
	copyOf := func(id, state, by string, firstSeen int64) catalog.CopyRow {
		return catalog.CopyRow{Disk: id, State: state, CreatedBy: by, FirstSeenNS: firstSeen}
	}
	const ok, stale, user, atoll = "ok", "stale", solver.CreatedByUser, solver.CreatedByAtoll
	disks := []catalog.Disk{disk("t1", "data", "online", 0), disk("c", "cache", "online", 1), disk("t2", "data", "standby", 2),
		disk("t3", "data", "online", 3), disk("t4", "data", "missing", 4), disk("t5", "data", "readonly", 5)}
	for _, tc := range []struct {
		name   string
		copies []catalog.CopyRow
		want   string
	}{
		{"the first disk of the list", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t1", ok, atoll, 2)}, "t1"},
		{"a user copy before an Atoll copy seen first", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t1", ok, atoll, 2), copyOf("t3", ok, user, 9)}, "t3"},
		{"the first seen of two Atoll copies", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t2", ok, atoll, 7), copyOf("t3", ok, atoll, 3)}, "t3"},
		{"by position, seen at once", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t3", ok, atoll, 3), copyOf("t2", ok, atoll, 3)}, "t2"},
		{"a stale user copy is none", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t1", stale, user, 1), copyOf("t3", ok, atoll, 3)}, "t3"},
		{"a copy on a disk the list does not know is none", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t9", ok, user, 1), copyOf("t3", ok, atoll, 3)}, "t3"},
		{"a missing disk is none, a read-only one is", []catalog.CopyRow{copyOf("c", ok, user, 1), copyOf("t4", ok, user, 1), copyOf("t5", ok, atoll, 3)}, "t5"},
		{"a cache disk is none", []catalog.CopyRow{copyOf("c", ok, user, 1)}, ""},
	} {
		f := &catalog.FileRow{Path: "a", Copies: tc.copies}
		if got := transferHeir(f, disks); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An operation of the view no pass has taken in holds a path when it is at it, above it or below it, by either of its
// names, and never when it is beside it (specs.md §19, §21.2).
func TestAViewOperationHoldsWhatItTouches(t *testing.T) {
	ops := func(path, path2 string) []catalog.ViewOp {
		return []catalog.ViewOp{{Path: "other/x"}, {Path: path, Path2: path2}}
	}
	for _, tc := range []struct {
		name        string
		path, path2 string
		want        bool
	}{
		{"at it", "d/a", "", true},
		{"above it", "d", "", true},
		{"below it", "d/a/b", "", true},
		{"renamed to it", "x", "d/a", true},
		{"renamed to a folder above it", "x", "d", true},
		{"beside it", "d/b", "", false},
		{"a name that only starts as it", "d/ab", "d/a2", false},
	} {
		if got := touchedByView(ops(tc.path, tc.path2), "d/a"); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	if touchedByView(nil, "d/a") {
		t.Error("no operation, and the path is held")
	}
}

// A transfer that waits for an operation of the view counts in what the pass says waits, and is due again a debounce
// on, never before the pass (specs.md §15.12, §19, §21.2).
func TestATransferThatWaitsForTheViewIsDueADebounceOn(t *testing.T) {
	b := cacheBench(t, "[deletes]\ndebounce = \"7m\"\n[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.scan(false)
	f, _, err := b.viewOf().Create(t.Context(), "a.mkv", 0o644, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(errors.Join(err, f.Close()))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := b.pass()
	if got := outcomeOf(out, KindTransfer, "a.mkv"); got != "" {
		t.Fatalf("transferred before the view's operation was taken in: %q", got)
	}
	if out.Waiting != 1 || !out.NextDue.Valid || out.NextDue.Int64 != b.now+int64(7*time.Minute) {
		t.Errorf("waiting %d, next due in %v", out.Waiting, time.Duration(out.NextDue.Int64-b.now))
	}
}
