package catalog

import (
	"database/sql"
	"slices"
	"testing"
)

// The view's journal (specs.md §9, §21.2): an operation is planned before it runs and a write done at once; a
// deletion is also a trash operation of ops, with its trash row and event, which no reprise but the view's resolves
// and which a pass still waits at; done, its entry is in the trash and its copy keeps its row for the pass; undone, it
// loses its trash row; and what no pass has taken in, undone left out, is what the view presents.
func TestViewOps(t *testing.T) {
	b := openForOps(t)
	ctx := t.Context()
	b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES ('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online')`)
	b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", []byte("F/a.mkv"))
	b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (1, 'u1', '7', 1, 1, 1, 1, 'user', 1, 'ok')")

	made := &ViewOp{Kind: ViewCreate, Disk: "u1", Path: "F/new.mkv", AtNS: 10}
	written := &ViewOp{Kind: ViewWrite, Disk: "u1", Path: "F/a.mkv", AtNS: 11}
	for _, v := range []*ViewOp{made, written} {
		if err := BeginViewOp(ctx, b.sql(), v); err != nil {
			t.Fatal(err)
		}
	}
	if made.State != OpPlanned || written.State != OpDone {
		t.Errorf("a create is %s and a write %s, want planned and done", made.State, written.State)
	}
	deleted := &ViewOp{Kind: ViewUnlink, Disk: "u1", Path: "F/a.mkv", FileKey: "7", AtNS: 12}
	op, err := BeginViewTrash(ctx, b.sql(), deleted, &TrashStart{Disk: "u1", Path: "F/a.mkv", FileKey: "7", Reason: "deleted", NowNS: 12, ExpiresNS: 99})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.OpID != op.ID || op.Path2 == "" {
		t.Fatalf("the deletion's operation: %+v, %+v", deleted, op)
	}
	planned, err := PlannedOps(ctx, b.sql())
	if err != nil || len(planned) != 0 {
		t.Errorf("a reprise would resolve the view's operation: %+v, %v", planned, err)
	}
	if waits, err := PlannedAt(ctx, b.sql(), "u1", "F/a.mkv"); err != nil || !waits {
		t.Errorf("a pass would not wait at the deletion under way: %v, %v", waits, err)
	}
	var events int
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind = 'trash'").Scan(&events); err != nil || events != 1 {
		t.Errorf("trash events: %d, %v", events, err)
	}

	if err := FinishViewOp(ctx, b.sql(), deleted, OpDone, false, 13); err != nil {
		t.Fatal(err)
	}
	entries, err := TrashEntries(ctx, b.sql())
	if err != nil || len(entries) != 1 || entries[0].Path != "F/a.mkv" || entries[0].Reason != "deleted" {
		t.Errorf("the trash once the deletion is done: %+v, %v", entries, err)
	}
	var copies int
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM copies").Scan(&copies); err != nil || copies != 1 {
		t.Errorf("the copy's row is the pass's to decide: %d rows, %v", copies, err)
	}

	if err := FinishViewOp(ctx, b.sql(), made, OpUndone, false, 14); err != nil {
		t.Fatal(err)
	}
	replaced := &ViewOp{Kind: ViewReplace, Disk: "u1", Path: "F/tmp", Path2: "F/b.nfo", FileKey: "8", AtNS: 15}
	if _, err := BeginViewTrash(ctx, b.sql(), replaced, &TrashStart{Disk: "u1", Path: "F/b.nfo", FileKey: "8", Reason: "overwritten", NowNS: 15, ExpiresNS: 99}); err != nil {
		t.Fatal(err)
	}
	if err := FinishViewOp(ctx, b.sql(), replaced, OpUndone, true, 16); err != nil {
		t.Fatal(err)
	}
	if entries, err := TrashEntries(ctx, b.sql()); err != nil || len(entries) != 1 {
		t.Errorf("an undone replacement left its trash entry: %+v, %v", entries, err)
	}
	var state string
	if err := b.db.QueryRowContext(ctx, "SELECT state FROM ops WHERE id = ?", replaced.OpID).Scan(&state); err != nil || state != OpUndone {
		t.Errorf("the replacement's operation: %s, %v", state, err)
	}
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind = 'op-undone'").Scan(&events); err != nil || events != 1 {
		t.Errorf("op-undone events: %d, %v", events, err)
	}

	open, err := OpenViewOps(ctx, b.sql())
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(open); !slices.Equal(ids, []int64{written.ID, deleted.ID}) {
		t.Errorf("open operations %v, want the write and the deletion", ids)
	}
	if open[1].OpID != deleted.OpID || open[1].FileKey != "7" || open[1].State != OpDone {
		t.Errorf("the deletion as read back: %+v", open[1])
	}
	if err := TakeViewOps(ctx, b.sql(), []int64{written.ID}, 20); err != nil {
		t.Fatal(err)
	}
	if open, err = OpenViewOps(ctx, b.sql()); err != nil || !slices.Equal(idsOf(open), []int64{deleted.ID}) {
		t.Errorf("open once the write is taken in: %v, %v", idsOf(open), err)
	}
	var taken sql.NullInt64
	if err := b.db.QueryRowContext(ctx, "SELECT taken_ns FROM view_ops WHERE id = ?", written.ID).Scan(&taken); err != nil || taken.Int64 != 20 {
		t.Errorf("taken_ns: %v, %v", taken, err)
	}

	// The table refuses what no operation of the view is.
	for _, q := range []string{
		"INSERT INTO view_ops(kind, disk_id, lpath, folder, state, at_ns) VALUES ('rename', 'u1', x'61', 0, 'planned', 1)",
		"INSERT INTO view_ops(kind, disk_id, lpath, folder, state, at_ns) VALUES ('replace', 'u1', x'61', 0, 'planned', 1)",
		"INSERT INTO view_ops(kind, disk_id, lpath, folder, state, at_ns) VALUES ('copy', 'u1', x'61', 0, 'planned', 1)",
		"INSERT INTO view_ops(kind, disk_id, lpath, folder, op_id, state, at_ns) VALUES ('create', 'u1', x'61', 0, 1, 'planned', 1)",
	} {
		if _, err := b.db.ExecContext(ctx, q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func idsOf(list []ViewOp) []int64 {
	ids := make([]int64, 0, len(list))
	for _, v := range list {
		ids = append(ids, v.ID)
	}
	return ids
}
