package catalog

import (
	"errors"
	"path/filepath"
	"testing"
)

// The rows of a resolution: a copy of a file in conflict brought to what its disk holds, a copy no longer there
// removed but never the last, and the end of the conflict once the kept copy is alone, which reads its hash again and
// drops what was queued; none of it touches a file not in conflict (specs.md §15.7).
func TestConflictRows(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var list []Disk
	for i, id := range []string{"t1", "t2", "t3"} {
		list = append(list, Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO files(id, lpath, size, mtime_ns, blake3, replicas_target, mode, state, first_seen_ns, updated_ns)
		VALUES (1, x'61', 2, 2, zeroblob(32), 1, 'deferred', 'conflict', 0, 0), (2, x'62', 5, 5, NULL, 1, 'deferred', 'ok', 0, 0)`)
	exec(`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, verified_ns, state)
		VALUES (1, 't1', 1024, 1, 'user', 0, 9, 'ok'), (1, 't2', 2, 2, 'user', 0, 9, 'ok'), (2, 't1', 5, 5, 'user', 0, NULL, 'ok')`)
	exec(`INSERT INTO file_chunks(file_id, chunks) VALUES (1, zeroblob(32))`)
	exec(`INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state) VALUES ('replicate', 1, 't2', 't3', 'queued'), ('replicate', 2, 't1', 't2', 'queued')`)

	page, err := ConflictFiles(t.Context(), NewFileReader(db))
	if err != nil || len(page) != 1 || page[0].Path != "a" || len(page[0].Copies) != 2 {
		t.Fatalf("%+v, %v", page, err)
	}
	birth := int64(17)
	if err := RefreshCopy(t.Context(), db, 1, "t1", &SeenCopy{Size: 6, MtimeNS: 18, CtimeNS: 19, BtimeNS: &birth, FileKey: "77", Nlink: 1}); err != nil {
		t.Fatal(err)
	}
	if err := RefreshCopy(t.Context(), db, 2, "t1", &SeenCopy{Size: 1}); err == nil {
		t.Error("a copy of a file not in conflict refreshed")
	}
	for _, tc := range []struct {
		file int64
		disk string
	}{{1, "t3"}, {2, "t1"}} {
		if err := DropCopy(t.Context(), db, tc.file, tc.disk); err == nil {
			t.Errorf("dropped %d on %s", tc.file, tc.disk)
		}
	}
	if err := ResolveConflict(t.Context(), db, 1, "t1", 50); !errors.Is(err, ErrStillAConflict) {
		t.Errorf("with t2 left: %v", err)
	}
	if err := DropCopy(t.Context(), db, 1, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := DropCopy(t.Context(), db, 1, "t1"); err == nil {
		t.Error("the last copy dropped")
	}
	if err := ResolveConflict(t.Context(), db, 1, "t1", 50); err != nil {
		t.Fatal(err)
	}
	var size, mtime, updated, chunks, jobs, otherJobs int64
	var hashless, unverified bool
	var state, original, key string
	err = db.QueryRowContext(t.Context(), `SELECT f.size, f.mtime_ns, f.blake3 IS NULL, f.state, f.original_disk, f.updated_ns, c.verified_ns IS NULL, c.file_key,
		(SELECT count(*) FROM file_chunks WHERE file_id = 1), (SELECT count(*) FROM jobs WHERE file_id = 1), (SELECT count(*) FROM jobs WHERE file_id = 2)
		FROM files f JOIN copies c ON c.file_id = f.id WHERE f.id = 1`).Scan(&size, &mtime, &hashless, &state, &original, &updated, &unverified, &key, &chunks, &jobs, &otherJobs)
	if err != nil {
		t.Fatal(err)
	}
	if size != 6 || mtime != 18 || !hashless || state != "pending" || original != "t1" || updated != 50 || !unverified || key != "77" ||
		chunks != 0 || jobs != 0 || otherJobs != 1 {
		t.Errorf("%d %d %v %s %s %d %v %s %d %d %d", size, mtime, hashless, state, original, updated, unverified, key, chunks, jobs, otherJobs)
	}
	// Resolved already: nothing changes.
	if err := ResolveConflict(t.Context(), db, 1, "t1", 60); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT updated_ns FROM files WHERE id = 1").Scan(&updated); err != nil || updated != 50 {
		t.Errorf("updated %d, %v", updated, err)
	}
	// A resolution with no look first still reads the kept copy again.
	exec(`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (3, x'63', 2, 2, 1, 'deferred', 'conflict', 0, 0)`)
	exec(`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, verified_ns, state) VALUES (3, 't1', 4, 4, 'user', 0, 9, 'ok')`)
	if err := ResolveConflict(t.Context(), db, 3, "t1", 70); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT verified_ns IS NULL FROM copies WHERE file_id = 3").Scan(&unverified); err != nil || !unverified {
		t.Errorf("still verified: %v", err)
	}
	if err := BeginResolve(t.Context(), db, "a", "t1", 40); err != nil {
		t.Fatal(err)
	}
	var data string
	if err := db.QueryRowContext(t.Context(), "SELECT data FROM events WHERE kind = 'conflict-resolve' AND level = 'info' AND ts_ns = 40").Scan(&data); err != nil ||
		data != `{"disk":"t1","path":"a"}` {
		t.Errorf("event %q, %v", data, err)
	}
}
