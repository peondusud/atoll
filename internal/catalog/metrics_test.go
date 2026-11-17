package catalog

import (
	"testing"
)

// The metrics count the files of each state, the copies of each disk as atoll status does, the trash as atoll trash
// list gives it, and the copies the scrub would read, as it would read them (specs.md §16, §23.1).
func TestCountForMetrics(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	for _, q := range []string{
		`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state)
			VALUES ('t1', 'u1', 'ext4', '/mnt/t1', 'media', 'Atoll', 'data', 0, '[]', 'online'),
			       ('t2', 'u2', 'ext4', '/mnt/t2', 'media', 'Atoll', 'data', 1, '[]', 'online')`,
		// a: ok, hashed, verified long ago on t1 and never on t2. b: ok, not hashed. c: corrupt, its copy on t2 corrupt.
		// d: in conflict, never verified. e: hashed, its copy on t1 verified recently, its size unknown on t2.
		`INSERT INTO files(id, lpath, size, mtime_ns, blake3, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES
			(1, CAST('a' AS BLOB), 100, 1, zeroblob(32), 1, 'deferred', 'ok', 1, 1),
			(2, CAST('b' AS BLOB), 20, 1, NULL, 1, 'deferred', 'ok', 1, 1),
			(3, CAST('c' AS BLOB), 300, 1, zeroblob(32), 1, 'deferred', 'corrupt', 1, 1),
			(4, CAST('d' AS BLOB), 4000, 1, zeroblob(32), 1, 'deferred', 'conflict', 1, 1),
			(5, CAST('e' AS BLOB), 50000, 1, zeroblob(32), 1, 'deferred', 'ok', 1, 1)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, ctime_ns, created_by, first_seen_ns, verified_ns, state) VALUES
			(1, 't1', 100, 1, 1, 'user', 1, 10, 'ok'),
			(1, 't2', 100, 1, 1, 'atoll', 1, NULL, 'ok'),
			(2, 't1', 20, 1, 1, 'user', 1, NULL, 'ok'),
			(3, 't1', 300, 1, 1, 'user', 1, NULL, 'ok'),
			(3, 't2', 300, 1, 1, 'atoll', 1, 10, 'corrupt'),
			(4, 't1', 4000, 1, 1, 'user', 1, NULL, 'ok'),
			(5, 't1', 50000, 1, 1, 'user', 1, 2000, 'ok'),
			(5, 't2', NULL, NULL, NULL, 'atoll', 1, NULL, 'pending')`,
		`INSERT INTO ops(id, disk_id, kind, path, started_ns, state) VALUES
			(1, 't1', 'trash', CAST('x' AS BLOB), 1, 'done'), (2, 't1', 'trash', CAST('y' AS BLOB), 1, 'done'), (3, 't2', 'trash', CAST('z' AS BLOB), 1, 'planned')`,
		`INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, reason, deleted_ns, expires_ns) VALUES
			(CAST('x' AS BLOB), 't1', CAST('1/x' AS BLOB), 1, 7, 'deleted', 1, 9), (CAST('y' AS BLOB), 't1', CAST('2/y' AS BLOB), 2, NULL, 'deleted', 1, 9), (CAST('z' AS BLOB), 't2', CAST('3/z' AS BLOB), 3, 5, 'deleted', 1, 9)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := CountForMetrics(ctx, db, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]CountedFiles{"ok": {3, 50120}, "corrupt": {1, 300}, "conflict": {1, 4000}}); len(got.Files) != len(want) ||
		got.Files["ok"] != want["ok"] || got.Files["corrupt"] != want["corrupt"] || got.Files["conflict"] != want["conflict"] {
		t.Errorf("files %+v, want %+v", got.Files, want)
	}
	// On t1: a verified at 10, due; b not hashed; c ok and never verified, due; d in conflict; e verified at 2000.
	// On t2: a never verified, due; c corrupt; e pending. The trash of t2 holds only what a planned operation left.
	if d := got.Disks["t1"]; d == nil || *d != (CountedDisk{Copies: 5, Bytes: 54420, TrashEntries: 2, TrashBytes: 7, ScrubDueCopies: 2, ScrubDueBytes: 400}) {
		t.Errorf("t1 %+v", d)
	}
	if d := got.Disks["t2"]; d == nil || *d != (CountedDisk{Copies: 3, Bytes: 400, ScrubDueCopies: 1, ScrubDueBytes: 100}) {
		t.Errorf("t2 %+v", d)
	}
}
