package catalog

import (
	"database/sql"
	"testing"
)

// failed-remove-drops, the maintainer's remedy of flaw 30: what is planned on a failed disk that disk remove takes out
// is undone in the catalog alone. A move to its trash loses its trash row, the row of Atoll's own view that names one is
// undone with it, each gets an op-undone event, and what is planned on another disk stays as it was.
func TestDropPlannedOnADisk(t *testing.T) {
	b := openForOps(t)
	ctx := t.Context()
	b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES
		('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'failed'), ('u2', 'b', 'ext4', '/b', 'media', 'Atoll', 'data', 1, '[]', 'online')`)
	for id, path := range map[int]string{1: "F/a.mkv", 2: "F/b.mkv"} {
		b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", id, []byte(path))
		for _, disk := range []string{"u1", "u2"} {
			b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (?, ?, ?, 1, 1, 1, 1, 'user', 1, 'ok')",
				id, disk, disk+path)
		}
	}
	size := sql.NullInt64{Int64: 1, Valid: true}
	moved, err := BeginTrash(ctx, b.sql(), &TrashStart{Disk: "u1", Path: "F/a.mkv", FileKey: "u1F/a.mkv", Size: size, Hash: make([]byte, 32), Reason: "deleted", NowNS: 10, ExpiresNS: 99})
	if err != nil {
		t.Fatal(err)
	}
	deleted := &ViewOp{Kind: ViewUnlink, Disk: "u1", Path: "F/b.mkv", FileKey: "u1F/b.mkv", AtNS: 11}
	viewed, err := BeginViewTrash(ctx, b.sql(), deleted, &TrashStart{Disk: "u1", Path: "F/b.mkv", FileKey: "u1F/b.mkv", Reason: "deleted", NowNS: 11, ExpiresNS: 99})
	if err != nil {
		t.Fatal(err)
	}
	other, err := BeginTrash(ctx, b.sql(), &TrashStart{Disk: "u2", Path: "F/a.mkv", FileKey: "u2F/a.mkv", Size: size, Hash: make([]byte, 32), Reason: "deleted", NowNS: 12, ExpiresNS: 99})
	if err != nil {
		t.Fatal(err)
	}
	n, err := DropPlannedOn(ctx, b.sql(), "u1", 20)
	if err != nil || n != 2 {
		t.Fatalf("dropped %d, %v", n, err)
	}
	if b.opState(moved.ID) != OpUndone || b.opState(viewed.ID) != OpUndone || b.opState(other.ID) != OpPlanned {
		t.Errorf("u1's move %s, the view's %s, u2's %s", b.opState(moved.ID), b.opState(viewed.ID), b.opState(other.ID))
	}
	var state string
	if err := b.db.QueryRowContext(ctx, "SELECT state FROM view_ops WHERE id = ?", deleted.ID).Scan(&state); err != nil || state != OpUndone {
		t.Errorf("the view's row: %q, %v", state, err)
	}
	var trashed, events int
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM trash WHERE disk_id = 'u1'").Scan(&trashed); err != nil || trashed != 0 {
		t.Errorf("u1's trash rows: %d, %v", trashed, err)
	}
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM trash WHERE disk_id = 'u2'").Scan(&trashed); err != nil || trashed != 1 {
		t.Errorf("u2's trash rows: %d, %v", trashed, err)
	}
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind = 'op-undone'").Scan(&events); err != nil || events != 2 {
		t.Errorf("op-undone events: %d, %v", events, err)
	}
	if n, err := DropPlannedOn(ctx, b.sql(), "u1", 21); err != nil || n != 0 {
		t.Errorf("dropped again: %d, %v", n, err)
	}
}
