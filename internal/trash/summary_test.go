package trash

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/watch"
)

// sized is a bench with the files a, of one byte, and b, of three, each a copy too many.
func sized(t *testing.T) *bench {
	t.Helper()
	b := newBench(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "b", "yyy")
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	return b
}

// session opens a session that may write or not, closed with the test.
func (b *bench) session(apply bool) *Session {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(s.Close)
	return s
}

// A summary says what its items say: what a run that may not write would do, what one that may did, the bytes of
// either, and what stays (specs.md §25).
func TestAPruneSumsWhatItSends(t *testing.T) {
	b := sized(t)
	for _, apply := range []bool{false, true} {
		var items []PruneItem
		summary, err := b.session(apply).Prune(t.Context(), func(i *PruneItem) error {
			items = append(items, *i)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		toDo, done, stayed, bytes := 0, 0, 0, int64(0)
		for _, i := range items {
			switch i.Outcome {
			case "to-trash":
				toDo++
				bytes += i.Size
			case OutcomeTrashed:
				done++
				bytes += i.Size
			default:
				stayed++
			}
		}
		if toDo+done != 2 || bytes != 4 || summary.ToDo != toDo || summary.Bytes != bytes || summary.Stayed != stayed {
			t.Errorf("may write %v: %d to do, %d done, %d bytes, %d stayed; the summary says %+v", apply, toDo, done, bytes, stayed, summary)
		}
	}
}

// A restore counts the entries a run with --yes would put back, and those that stay where their disk cannot be
// reached.
func TestARestoreCountsWhatItPutsBack(t *testing.T) {
	b := sized(t)
	b.prune()
	summary, err := b.session(false).Restore(t.Context(), "a", func(*EntryItem) error { return nil })
	if err != nil || summary.ToDo != 1 || summary.Stayed != 0 {
		t.Errorf("a dry run: %+v, %v", summary, err)
	}
	b.unread[b.dirs["t3"]] = true
	summary, err = b.session(true).Restore(t.Context(), "a", func(*EntryItem) error { return nil })
	if err != nil || summary.ToDo != 0 || summary.Stayed != 1 {
		t.Errorf("their disk out of reach: %+v, %v", summary, err)
	}
}

// What a purge reaches counts in its summary, and comes off what the trash holds, whether it goes now or would with
// --yes: a trash over its share once those are gone is none (specs.md §17).
func TestAPurgeSumsWhatItReaches(t *testing.T) {
	var zero time.Duration
	for _, apply := range []bool{false, true} {
		b := sized(t)
		b.prune()
		summary, err := b.session(apply).Purge(t.Context(), &zero, func(*EntryItem) error { return nil })
		toDo := 2
		if apply {
			toDo = 0
		}
		if err != nil || summary.ToDo != toDo || summary.Bytes != 4 || summary.Stayed != 0 {
			t.Errorf("may write %v: %+v, %v", apply, summary, err)
		}
		// Four bytes in the trash of a disk of 40, whose share is 5 %: both entries go for room.
		b = sized(t)
		b.prune()
		b.capacity = 40
		b.opened().Close()
		summary, err = b.session(apply).Purge(t.Context(), nil, func(*EntryItem) error { return nil })
		if err != nil || summary.Bytes != 4 || len(summary.Full) != 0 {
			t.Errorf("may write %v, for room: %+v, %v", apply, summary, err)
		}
	}
}

// An entry of the trash keeps the file's hash only when its copy held the file's version: one sent as overwritten
// held another, a stale one, and one quarantined or repaired a corrupt one, none of which the hash says (specs.md §9,
// §16, §17.1).
func TestAnEntryKeepsTheHashOfTheVersionItHolds(t *testing.T) {
	for _, tc := range []struct {
		reason string
		hashed bool
	}{
		{"surplus", true}, {"deleted", true}, {catalog.ReasonOverwritten, false}, {catalog.ReasonQuarantined, false}, {catalog.ReasonRepaired, false},
	} {
		b := newBench(t)
		f := b.fileA()
		if len(f.Hash) == 0 {
			t.Fatal("the file has no hash")
		}
		c := copyOn(f, "t3")
		outcome, err := b.session(true).Send(t.Context(), f, c, tc.reason)
		if err != nil || outcome != OutcomeTrashed {
			t.Fatalf("%s: %s, %v", tc.reason, outcome, err)
		}
		if got := b.count("SELECT count(*) FROM trash WHERE blake3 IS NOT NULL"); got != map[bool]int{false: 0, true: 1}[tc.hashed] {
			t.Errorf("%s: %d entries with a hash", tc.reason, got)
		}
	}
}

// On ext4 two names that differ by their case are two names: an entry is put back beside a file whose name differs
// from its own by case alone, on its disk or on another, as only NTFS folds them (specs.md §15.7, §15.8).
func TestARestoreOnExt4IgnoresCase(t *testing.T) {
	cased := func(t *testing.T) *bench {
		t.Helper()
		b := newBench(t)
		for _, id := range []string{"t1", "t2", "t3"} {
			b.write(id, "B", "upper")
		}
		b.write("t1", "b", "lower")
		b.write("t3", "b", "lower")
		if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
			t.Fatal(err)
		}
		b.prune()
		if b.read("t3", "B") != "" || b.read("t3", "b") != "lower" {
			t.Fatalf("t3 holds B %q and b %q", b.read("t3", "B"), b.read("t3", "b"))
		}
		return b
	}
	b := cased(t)
	if outcomes, _ := b.restored("B"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored || b.read("t3", "B") != "upper" {
		t.Errorf("%v: t3 holds B %q", outcomes, b.read("t3", "B"))
	}
	// To t1, where B went behind Atoll's back, which holds b.
	b = cased(t)
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "B")); err != nil {
		t.Fatal(err)
	}
	if outcomes, _ := b.restoredTo("B", "t1", true); len(outcomes) != 1 || outcomes[0] != OutcomeRestored || b.read("t1", "B") != "upper" {
		t.Errorf("to t1: %v, t1 holds B %q", outcomes, b.read("t1", "B"))
	}
}

// A copy's row shows what its disk holds now: its creation time when the disk gives one, and none, whatever the row said,
// when it gives none (specs.md §7, §16).
func TestACopyShownHasTheCreationTimeItsDiskGives(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	birth := int64(9)
	for _, seen := range []*catalog.SeenCopy{
		{Size: 4, MtimeNS: 1, CtimeNS: 2, BtimeNS: &birth, FileKey: "7", Nlink: 1},
		{Size: 4, MtimeNS: 1, CtimeNS: 2, FileKey: "7", Nlink: 1},
	} {
		c := catalog.CopyRow{BtimeNS: valid(5)}
		show(&c, seen)
		want := sql.NullInt64{}
		if seen.BtimeNS != nil {
			want = valid(9)
		}
		if c.BtimeNS != want || c.Size != valid(4) || c.MtimeNS != valid(1) || c.CtimeNS != valid(2) || c.Nlink != valid(1) ||
			c.FileKey != (sql.NullString{String: "7", Valid: true}) {
			t.Errorf("%+v shown as %+v", seen, c)
		}
	}
}
