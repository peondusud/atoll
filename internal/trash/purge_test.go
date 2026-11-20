package trash

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// untilBy purges by a slice in a session that may write or not, with the context given, and gives what it reached.
func (b *bench) untilBy(ctx context.Context, at int64, apply bool, each func(*EntryItem) error) (*Summary, bool, error) {
	b.t.Helper()
	s, err := Open(ctx, b.db, b.config, b, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	return s.PurgeUntil(ctx, at, each)
}

// A purge by slices that may not write says what it would purge, and purges nothing; one asked to stop stops before
// the next entry; what its caller's function returns stops it too.
func TestAPurgeBySlicesThatPurgesNothing(t *testing.T) {
	b := trashed(t, "b")
	b.exec("UPDATE trash SET expires_ns = 1")
	summary, left, err := b.untilBy(t.Context(), b.Now()+int64(time.Hour), false, func(*EntryItem) error { return nil })
	if err != nil || left || summary.ToDo != 2 || summary.Bytes != 2 || summary.Applied {
		t.Errorf("a dry run: %+v, left %v, %v", summary, left, err)
	}
	// Asked to stop once the first entry is purged, it purges no other.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, _, err := b.untilBy(ctx, b.Now()+int64(time.Hour), true, func(*EntryItem) error { cancel(); return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 1 {
		t.Errorf("%d entries left once canceled, want one", got)
	}
	stop := errors.New("stop")
	if _, _, err := b.untilBy(t.Context(), b.Now()+int64(time.Hour), true, func(*EntryItem) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("stopped: %v", err)
	}
}

// What goes for room goes by slices too, and stops at its time; a trash that holds too much of what never goes for
// room is said to be full once the slices end (specs.md §17).
func TestAPurgeBySlicesForRoom(t *testing.T) {
	b := trashed(t, "b", "c")
	b.capacity = 40
	b.opened().Close()
	// Two bytes is 5 % of 40: one entry goes for room. A slice with no time left reaches none.
	summary, left, err := b.untilBy(t.Context(), b.Now(), true, func(*EntryItem) error { return nil })
	if err != nil || !left || summary.Bytes != 0 || b.count("SELECT count(*) FROM trash") != 3 {
		t.Errorf("no time: %+v, left %v, %v", summary, left, err)
	}
	var reached []string
	summary, left, err = b.untilBy(t.Context(), b.Now()+int64(time.Hour), true, func(i *EntryItem) error {
		reached = append(reached, i.Why+" "+i.Outcome)
		return nil
	})
	if err != nil || left || len(reached) != 1 || reached[0] != "max-size purged" || len(summary.Full) != 0 {
		t.Errorf("for room: %v, %+v, left %v, %v", reached, summary, left, err)
	}
	stop := errors.New("stop")
	b = trashed(t, "b", "c")
	b.capacity = 40
	b.opened().Close()
	if _, _, err := b.untilBy(t.Context(), b.Now()+int64(time.Hour), true, func(*EntryItem) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("stopped: %v", err)
	}
	b = trashed(t, "b", "c")
	b.capacity = 40
	b.opened().Close()
	b.exec("UPDATE trash SET reason = 'deleted'")
	summary, _, err = b.untilBy(t.Context(), b.Now()+int64(time.Hour), true, func(*EntryItem) error { return nil })
	if err != nil || len(summary.Full) != 1 || summary.Full[0] != "t3" {
		t.Errorf("full: %+v, %v", summary, err)
	}
}

// An entry of a disk the pool no longer lists stays, unavailable, and the purge of the others goes on; what the
// caller's function returns stops it.
func TestAPurgeOfAnEntryOfADiskGone(t *testing.T) {
	b := trashed(t, "b")
	b.exec("UPDATE trash SET disk_id = 't9' WHERE lpath = x'62'")
	var outcomes []string
	s := b.opened()
	var zero time.Duration
	summary, err := s.Purge(t.Context(), &zero, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Disk+" "+i.Outcome)
		return nil
	})
	if err != nil || len(outcomes) != 2 || outcomes[0] != "t3 purged" || outcomes[1] != "t9 disk-unavailable" || summary.Stayed != 1 {
		t.Errorf("%v, %+v, %v", outcomes, summary, err)
	}
	stop := errors.New("stop")
	if _, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("stopped: %v", err)
	}
}

// A file in the trash that is no longer the entry's stays, with its entry: changed, with an op-undone event (specs.md
// §17.4).
func TestAPurgeOfAnEntryReplaced(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.replace(b.inTrash(1, "a"), "x")
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomeChanged {
		t.Fatalf("%v", outcomes)
	}
	if _, err := os.Stat(b.inTrash(1, "a")); err != nil || b.count("SELECT count(*) FROM trash") != 1 {
		t.Errorf("the file: %v", err)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events", got)
	}
}

// The chunks a repair saved are purged as Atoll's own files; ones that cannot be removed stay with their entry, and a
// disk that fails there is lost (specs.md §17).
func TestAPurgeOfSavedChunks(t *testing.T) {
	for _, tc := range []struct {
		err     error
		outcome string
		entries int
		disk    string
	}{
		{nil, OutcomePurged, 0, "online"},
		{syscall.EACCES, OutcomeIOError, 1, "online"},
		{syscall.EIO, OutcomeIOError, 1, "missing"},
	} {
		r := rotted(t)
		if got := r.repaired(true); got != OutcomeRepaired {
			t.Fatalf("%s", got)
		}
		if tc.err != nil {
			r.b.failOn("t3", "unlink", tc.err)
		}
		outcomes, _ := r.b.purgedAll()
		if len(outcomes) != 1 || outcomes[0] != tc.outcome {
			t.Errorf("%v: %v", tc.err, outcomes)
		}
		if got := r.b.count("SELECT count(*) FROM trash"); got != tc.entries || r.b.diskState("t3") != tc.disk {
			t.Errorf("%v: %d entries, t3 %s", tc.err, got, r.b.diskState("t3"))
		}
		if tc.entries == 0 {
			if entries, _ := os.ReadDir(filepath.Join(r.b.dirs["t3"], "Atoll", "trash")); len(entries) != 0 {
				t.Errorf("the trash holds %v", entries)
			}
		}
	}
}
