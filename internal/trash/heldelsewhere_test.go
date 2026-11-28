package trash

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// entryOfA gives the trash's entry of a, which prune sent there from t3.
func (b *bench) entryOfA() *catalog.TrashEntry {
	b.t.Helper()
	list, err := catalog.TrashEntries(b.t.Context(), b.db)
	if err != nil || len(list) != 1 || list[0].Path != "a" {
		b.t.Fatalf("%+v, %v", list, err)
	}
	return &list[0]
}

// The look before a surplus entry goes for room finds its version on another disk only in a copy the catalog calls ok,
// as its row says, of a file whose hash is the entry's: never in a stale copy, nor in a file of another version or
// one the catalog lost, nor for an entry with no hash (specs.md §17; conformance/models/README.md, flaw 28).
func TestTheLookForAnEntrysVersionElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dress func(b *bench, e *catalog.TrashEntry)
		want  bool
	}{
		{"copies as their rows say", func(*bench, *catalog.TrashEntry) {}, true},
		{"a stale copy before one that holds", func(b *bench, _ *catalog.TrashEntry) {
			b.exec("UPDATE copies SET state = 'stale' WHERE disk_id = 't1'")
		}, true},
		{"every other copy stale", func(b *bench, _ *catalog.TrashEntry) {
			b.exec("UPDATE copies SET state = 'stale' WHERE disk_id IN ('t1', 't2')")
		}, false},
		{"the file of another version", func(b *bench, _ *catalog.TrashEntry) {
			b.exec("UPDATE files SET blake3 = zeroblob(32)")
		}, false},
		{"a file the catalog does not hold", func(_ *bench, e *catalog.TrashEntry) { e.Path = "z" }, false},
		{"an entry with no hash", func(_ *bench, e *catalog.TrashEntry) { e.Hash = nil }, false},
	} {
		b := newBench(t)
		b.prunedSurplus()
		e := b.entryOfA()
		tc.dress(b, e)
		s := b.opened()
		got, err := s.heldElsewhere(t.Context(), e)
		if err != nil || got != tc.want {
			t.Errorf("%s: %v, %v", tc.name, got, err)
		}
	}
}

// A purge that reaches an entry by its age before a surplus one for room still asks the catalog of the surplus one:
// one whose version no copy the catalog calls ok holds waits for its retention, unreached, and the other goes by its
// age (specs.md §17; conformance/models/README.md, flaw 28).
func TestASurplusEntryForRoomAfterOneByItsAgeIsAskedToo(t *testing.T) {
	b := trashed(t, "b")
	b.exec("UPDATE trash SET expires_ns = 1 WHERE CAST(lpath AS TEXT) = 'a'")
	b.exec("UPDATE copies SET state = 'stale' WHERE disk_id IN ('t1', 't2') AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'b')")
	b.overFull()
	if got := b.purgedWhy(); !slices.Equal(got, []string{"expired purged"}) || b.text("SELECT CAST(lpath AS TEXT) FROM trash") != "b" {
		t.Errorf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
	}
}

// A purge by slices whose time has come purges nothing, and says some are left (specs.md §15.13, §17).
func TestAPurgeSliceAtItsTimePurgesNothing(t *testing.T) {
	b := trashed(t, "b")
	b.exec("UPDATE trash SET expires_ns = 1")
	items, _, left := b.until(b.Now())
	if len(items) != 0 || !left || b.count("SELECT count(*) FROM trash") != 2 {
		t.Errorf("%+v, left %v", items, left)
	}
}
