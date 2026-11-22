package catalog

import (
	"database/sql"
	"reflect"
	"testing"
)

func newBusyPool(t *testing.T) *testDB {
	t.Helper()
	b := openForOps(t)
	busyPool(b)
	return b
}

// A copy that reached its file's path by itself follows it: no old path, no clock of a name gone, the link count and
// change time it has now, a change time unknown keeping the one recorded, and no verification, as nothing says the
// content did not move with it (specs.md §15.12).
func TestCatchUp(t *testing.T) {
	b := newBusyPool(t)
	ctx := t.Context()
	if err := CatchUp(ctx, b.sql(), "u2", 2, &RenameDone{Nlink: 3, Ctime: sql.NullInt64{Int64: 9, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if got := b.count(`SELECT count(*) FROM copies WHERE file_id = 2 AND disk_id = 'u2' AND old_lpath IS NULL AND old_lpath_key IS NULL
		AND nlink = 3 AND ctime_ns = 9 AND verified_ns IS NULL`); got != 1 {
		t.Error("the copy behind did not follow its file")
	}
	// A copy whose name was gone is there again, its change time kept.
	if err := CatchUp(ctx, b.sql(), "u2", 5, &RenameDone{Nlink: 1}); err != nil {
		t.Fatal(err)
	}
	if got := b.count(`SELECT count(*) FROM copies WHERE file_id = 5 AND disk_id = 'u2' AND gone_ns IS NULL AND gone_kind IS NULL AND ctime_ns = 6`); got != 1 {
		t.Error("the clock of the name gone stands")
	}
	// The copies of other disks are left as they are.
	if got := b.count(`SELECT count(*) FROM copies WHERE file_id = 2 AND disk_id = 'u1' AND nlink = 2 AND ctime_ns = 6`); got != 1 {
		t.Error("another disk's copy changed")
	}
}

// EndOps ends the operations it names, in one go, and no other; with none it writes nothing.
func TestEndOps(t *testing.T) {
	b := newBusyPool(t)
	ctx := t.Context()
	if err := EndOps(ctx, b.sql(), nil, OpDone); err != nil {
		t.Fatal(err)
	}
	if err := EndOps(ctx, b.sql(), []int64{5, 6}, OpUndone); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE id IN (5, 6) AND state = 'undone'"); got != 2 {
		t.Errorf("%d operations undone", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 5 {
		t.Errorf("%d operations left planned, want the 5 others", got)
	}
}

// A hold that ended by itself is no longer a guard that stands, and the next trip is another guard, with a time of
// its own for a confirm to go by; what was confirmed before stays (specs.md §15.6).
func TestWriteGuardClear(t *testing.T) {
	b := newBusyPool(t)
	ctx := t.Context()
	g, err := ReadGuard(ctx, b.sql(), "u1", 20, 100)
	if err != nil || !g.HasGuard || g.GuardNS != 10 || g.UpTo.Valid {
		t.Fatalf("before: %+v, %v", g, err)
	}
	if err := WriteGuardClear(ctx, b.sql(), "u1", 30); err != nil {
		t.Fatal(err)
	}
	if g, err = ReadGuard(ctx, b.sql(), "u1", 40, 100); err != nil || g.HasGuard || g.UpTo.Valid {
		t.Errorf("cleared: %+v, %v", g, err)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'guard-clear' AND ts_ns = 30 AND json_extract(data, '$.disk') = 'u1'"); got != 1 {
		t.Error("no event of the clear")
	}
	// Another disk's guard is its own.
	if g, err = ReadGuard(ctx, b.sql(), "u2", 40, 100); err != nil || !g.UpTo.Valid || g.UpTo.Int64 != 14 {
		t.Errorf("u2: %+v, %v", g, err)
	}
	if err := WriteGuardEvent(ctx, b.sql(), "u1", 50); err != nil {
		t.Fatal(err)
	}
	if g, err = ReadGuard(ctx, b.sql(), "u1", 60, 100); err != nil || !g.HasGuard || g.GuardNS != 50 {
		t.Errorf("the next trip: %+v, %v", g, err)
	}
}

// A copy that leaves the path it sat behind at ends the folder renames of its disk that it was waiting for, those whose
// folder held that path; those of another folder, of another disk, or of the folder itself, stay (specs.md §15.12).
func TestAClearedCopyEndsTheFolderJobsAboveIt(t *testing.T) {
	b := newBusyPool(t)
	ctx := t.Context()
	folderJob := func(dst, from, to string) {
		t.Helper()
		payload, err := encodeFolderPayload(from, to)
		if err != nil {
			t.Fatal(err)
		}
		b.exec("INSERT INTO jobs(kind, src_disk, dst_disk, payload, state) VALUES ('rename', 'u1', ?, ?, 'queued')", dst, payload)
	}
	folderJob("u2", "Y", "A")   // another folder
	folderJob("u1", "Z", "A")   // another disk
	folderJob("u2", "Z/b", "A") // the copy's own path, no folder above it
	err := ApplyDecisions(ctx, b.sql(), 100, []FileChange{{ID: 2, Path: "A/b", Rows: []RowChange{{Disk: "u2", Clear: true, ClearedFrom: "Z/b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE file_id = 2 AND disk_id = 'u2' AND old_lpath IS NULL"); got != 1 {
		t.Error("the copy still sits behind")
	}
	jobs, err := FolderJobs(ctx, b.sql(), "u2")
	if err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, j := range jobs {
		from = append(from, j.From)
	}
	if want := []string{"Y", "Z/b"}; !reflect.DeepEqual(from, want) {
		t.Errorf("folder jobs of u2 left: %v", from)
	}
	if jobs, err = FolderJobs(ctx, b.sql(), "u1"); err != nil || len(jobs) != 1 {
		t.Errorf("u1: %+v, %v", jobs, err)
	}
}

// A copy of a trash entry made on another disk is a restore, as its event says; a copy a replication makes is not.
func TestIsRestoreTo(t *testing.T) {
	b := newBusyPool(t)
	ctx := t.Context()
	entry, ok, err := TrashEntryByRow(ctx, b.sql(), 1)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	op, err := BeginRestoreTo(ctx, b.sql(), "u2", &entry, 100)
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != OpCreate || op.Disk != "u2" || op.Path != "Q/q" || op.Path2 == "" {
		t.Errorf("%+v", op)
	}
	for id, want := range map[int64]bool{op.ID: true, 3: false, 999: false} {
		if got, err := IsRestoreTo(ctx, b.sql(), id); err != nil || got != want {
			t.Errorf("operation %d: %v, %v", id, got, err)
		}
	}
	// The entry stays in its trash.
	if _, ok, err := TrashEntryByRow(ctx, b.sql(), 1); err != nil || !ok {
		t.Errorf("the entry went: %v, %v", ok, err)
	}
}

// FolderKey gives the inode a folder's row records on a disk, whoever made it, and nothing for what is no folder there.
func TestFolderKey(t *testing.T) {
	b := newBusyPool(t)
	for _, c := range []struct{ disk, folder, want string }{
		{"u1", "A", "101"}, // the user's
		{"u2", "A", "201"}, // Atoll's own
		{"u2", "C", ""},    // no row on that disk
		{"u1", "A/l", ""},  // a link
		{"u1", "A/a", ""},  // a file
	} {
		if got, err := FolderKey(t.Context(), b.sql(), c.disk, c.folder); err != nil || got != c.want {
			t.Errorf("%s on %s: %q, %v", c.folder, c.disk, got, err)
		}
	}
}

// A stale copy that reached the trash keeps its row as a pending copy, made again from the copy that changed by a job
// that waits for no window; its trash job goes. Without that job, nothing says where to copy it from, and its row goes
// as for any other copy sent to the trash (specs.md §15.4).
func TestAStaleCopyInTheTrashIsMadeAgain(t *testing.T) {
	b := newBusyPool(t)
	ctx := t.Context()
	b.exec("DELETE FROM jobs WHERE id = 1")
	op := &Op{ID: 11, Disk: "u2", Kind: OpTrash, Path: "B/c", Path2: "20260101/11/B/c", FileKey: "23"}
	if err := FinishTrash(ctx, b.sql(), op, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.count(`SELECT count(*) FROM copies WHERE file_id = 3 AND disk_id = 'u2' AND state = 'pending' AND created_by = 'atoll'
		AND file_key IS NULL AND size IS NULL AND first_seen_ns = 11`); got != 1 {
		t.Error("the stale copy is not pending")
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'trash' AND file_id = 3"); got != 0 {
		t.Error("its trash job stays")
	}
	jobs, err := ReplicateJobs(ctx, b.sql(), "B/c")
	if err != nil || len(jobs) != 1 || jobs[0].Src != "u1" || jobs[0].Dst != "u2" || jobs[0].Priority != 4 || !jobs[0].Refresh {
		t.Errorf("%+v, %v", jobs, err)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE id = 11 AND state = 'done'"); got != 1 {
		t.Error("the operation is not done")
	}

	// Its trash job dropped since, the copy goes.
	b = newBusyPool(t)
	b.exec("DELETE FROM jobs WHERE id = 2")
	if err := FinishTrash(ctx, b.sql(), op, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE file_id = 3 AND disk_id = 'u2'"); got != 0 {
		t.Error("a copy with nothing to make it from stays")
	}
}
