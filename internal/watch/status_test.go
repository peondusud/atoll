package watch

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

func readAll(t *testing.T, b *bench) *Status {
	t.Helper()
	s, err := ReadStatus(t.Context(), b.db, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStatusCountsWhatCanBeReadNow: a copy that is stale or corrupt is no copy to rely on, and a
// disk draining or failed is no disk to rely on. No scenario reaches these states yet, as nothing
// writes them.
func TestStatusCountsWhatCanBeReadNow(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(Options{})
	if s := readAll(t, b); s.FilesAtLevel != 1 {
		t.Fatalf("at the start: %+v", s)
	}
	const undoCopy, undoDisk = "UPDATE copies SET state = 'ok'", "UPDATE disks SET state = 'online'"
	for _, tc := range []struct {
		name, undo, set string
		counted         bool
	}{
		{"stale", undoCopy, "UPDATE copies SET state = 'stale' WHERE disk_id = 't1'", false},
		{"corrupt", undoCopy, "UPDATE copies SET state = 'corrupt' WHERE disk_id = 't1'", false},
		{"pending", undoCopy, "UPDATE copies SET state = 'pending' WHERE disk_id = 't1'", false},
		{"unavailable", undoCopy, "UPDATE copies SET state = 'unavailable' WHERE disk_id = 't1'", false},
		{"draining", undoDisk, "UPDATE disks SET state = 'draining' WHERE id = 't1'", false},
		{"failed", undoDisk, "UPDATE disks SET state = 'failed' WHERE id = 't1'", false},
		{"retired", undoDisk, "UPDATE disks SET state = 'retired' WHERE id = 't1'", false},
		{"missing", undoDisk, "UPDATE disks SET state = 'missing' WHERE id = 't1'", false},
		{"surplus", undoCopy, "UPDATE copies SET state = 'surplus' WHERE disk_id = 't1'", true},
		{"standby", undoDisk, "UPDATE disks SET state = 'standby' WHERE id = 't1'", true},
		{"readonly", undoDisk, "UPDATE disks SET state = 'readonly' WHERE id = 't1'", true},
	} {
		if _, err := b.db.ExecContext(t.Context(), tc.set); err != nil {
			t.Fatal(err)
		}
		s := readAll(t, b)
		if got := s.FilesAtLevel == 1 && s.BytesAtLevel == 1; got != tc.counted {
			t.Errorf("%s: %+v", tc.name, s)
		}
		if _, err := b.db.ExecContext(t.Context(), tc.undo); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStatusCorruptIsCounted: the one state no scenario can reach, as none can corrupt a disk.
func TestStatusCorruptIsCounted(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	if _, err := b.db.ExecContext(t.Context(), "UPDATE files SET state = 'corrupt'"); err != nil {
		t.Fatal(err)
	}
	if s := readAll(t, b); s.ByState["corrupt"] != 1 || s.FilesTotal != 1 {
		t.Errorf("%+v", s)
	}
}

// TestStatusCopyOnAnUnknownDisk: pages are read apart, so a scan running meanwhile can show a copy
// on a disk that was not there when the list was read. It counts nowhere, and nothing breaks.
func TestStatusCopyOnAnUnknownDisk(t *testing.T) {
	at := map[string]int{"t1": 0}
	s := &Status{Disks: []DiskStatus{{Disk: catalog.Disk{ID: "t1", State: "online"}}}, ByState: map[string]int{}}
	f := &catalog.FileRow{Size: 7, ReplicasTarget: 1, State: "ok", Copies: []catalog.CopyRow{
		{Disk: "new", State: "ok", Size: sql.NullInt64{Int64: 7, Valid: true}},
		{Disk: "t1", State: "ok", Size: sql.NullInt64{Int64: 7, Valid: true}},
	}}
	s.countCopies(f, at)
	s.countFile(f, at)
	if s.Disks[0].Copies != 1 || s.Disks[0].Bytes != 7 || s.FilesAtLevel != 0 || s.BytesTotal != 7 {
		t.Errorf("%+v", s)
	}
	// With no disk listed at all, as when a first scan starts just after the list was read.
	empty := &Status{ByState: map[string]int{}}
	empty.countCopies(f, map[string]int{})
	empty.countFile(f, map[string]int{})
	if empty.FilesTotal != 1 || empty.FilesAtLevel != 0 {
		t.Errorf("%+v", empty)
	}
}

// TestStatusGivesFilesAndStops: each sees only what under keeps, by path and with copies by
// position, and its error ends the reading, as does a cancellation.
func TestStatusGivesFilesAndStops(t *testing.T) {
	b := newBench(t, "z", "a")
	for _, p := range []string{"Films/a", "Films/sub/b", "Filmsx", "Fil"} {
		b.write("z", p, "x")
	}
	b.write("a", "Films/a", "x")
	b.scan(Options{})

	var seen []string
	s, err := ReadStatus(t.Context(), b.db, "Films", func(f *catalog.FileRow) error {
		seen = append(seen, f.Path)
		if f.Path == "Films/a" && (len(f.Copies) != 2 || f.Copies[0].Disk != "z" || f.Copies[1].Disk != "a") {
			t.Errorf("copies: %+v", f.Copies)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Films/a" || seen[1] != "Films/sub/b" || s.FilesTotal != 2 {
		t.Errorf("seen %q, %+v", seen, s)
	}
	if s.Disks[0].ID != "z" || s.Disks[0].Copies != 4 || s.Disks[1].Copies != 1 {
		t.Errorf("the disks follow every file: %+v", s.Disks)
	}

	stop := errors.New("stop")
	calls := 0
	if _, err := ReadStatus(t.Context(), b.db, "Films", func(*catalog.FileRow) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Errorf("%v after %d calls", err, calls)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadStatus(ctx, b.db, "", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}
