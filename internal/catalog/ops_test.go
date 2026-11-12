package catalog

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type testDB struct {
	t  *testing.T
	db *sql.DB
}

func (b *testDB) exec(query string, args ...any) {
	b.t.Helper()
	if _, err := b.db.ExecContext(b.t.Context(), query, args...); err != nil {
		b.t.Fatal(err)
	}
}

func (b *testDB) sql() *sql.DB { return b.db }

func openForOps(t *testing.T) *testDB {
	t.Helper()
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &testDB{t: t, db: db}
}

// A restore takes the row of a file that sat behind at that path. With no copy left its file goes, as it would
// otherwise hold its own path for good.
func TestARestoreOverTheLastRowOfAFileBehind(t *testing.T) {
	b := openForOps(t)
	b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES ('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online')`)
	b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", []byte("E/x"))
	b.exec("INSERT INTO copies(file_id, disk_id, old_lpath, old_lpath_key, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (1, 'u1', ?, ?, '7', 1, 1, 1, 1, 'user', 1, 'ok')", []byte("D/x"), keyOf("D/x"))
	op := &Op{ID: 1, Disk: "u1", Kind: OpRestore, Path: "D/x", Path2: "t/x"}
	if err := FinishRestore(t.Context(), b.sql(), op, 5, &NewCopy{Path: "D/x", Size: 1, MtimeNS: 1, CtimeNS: 1, FileKey: "9", Nlink: 1}, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := b.db.QueryRowContext(t.Context(), "SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'E/x'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d files left with no copy", count)
	}
	if err := b.db.QueryRowContext(t.Context(), "SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE CAST(f.lpath AS TEXT) = 'D/x'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d copies of what came back", count)
	}
}

// What happened to a file on one disk says nothing of a file with the same inode number on another, and
// a name that recorded another change time keeps it, for the next scan to report.
func TestLinksOfAFile(t *testing.T) {
	b := openForOps(t)
	b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES
		('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online'),
		('u2', 'b', 'ext4', '/b', 'media', 'Atoll', 'data', 1, '[]', 'online')`)
	for id, path := range map[int]string{1: "c", 2: "d", 3: "e"} {
		b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", id, []byte(path))
	}
	row := func(file int, disk string, ctime int64) {
		b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (?, ?, '7', 3, 1, 1, ?, 'user', 1, 'ok')", file, disk, ctime)
	}
	row(1, "u1", 5)
	row(2, "u1", 5)
	row(3, "u1", 4)
	row(2, "u2", 5)
	op := &Op{ID: 1, Disk: "u1", Kind: OpTrash, Path: "c"}
	if err := FinishTrash(t.Context(), b.sql(), op, nil, &Links{FileKey: "7", CtimeNS: 9, Nlink: 2, PriorNS: sql.NullInt64{Int64: 5, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	read := func(file int, disk string) (ctime, nlink int64) {
		t.Helper()
		if err := b.db.QueryRowContext(t.Context(), "SELECT ctime_ns, nlink FROM copies WHERE file_id = ? AND disk_id = ?", file, disk).Scan(&ctime, &nlink); err != nil {
			t.Fatal(err)
		}
		return ctime, nlink
	}
	if ctime, nlink := read(2, "u1"); ctime != 9 || nlink != 2 {
		t.Errorf("the name that follows: %d, %d", ctime, nlink)
	}
	if ctime, nlink := read(3, "u1"); ctime != 4 || nlink != 2 {
		t.Errorf("the name that had another time keeps it: %d, %d", ctime, nlink)
	}
	if ctime, nlink := read(2, "u2"); ctime != 5 || nlink != 3 {
		t.Errorf("another disk: %d, %d", ctime, nlink)
	}
	// With nothing known of the time before, as in the reprise of a purge, every name follows.
	if err := FinishPurge(t.Context(), b.sql(), &Op{ID: 2, Disk: "u1", Path2: "x"}, &Links{FileKey: "7", CtimeNS: 11, Nlink: 1}); err != nil {
		t.Fatal(err)
	}
	if ctime, nlink := read(3, "u1"); ctime != 11 || nlink != 1 {
		t.Errorf("with no time known: %d, %d", ctime, nlink)
	}
}

// Planned operations come back by id, whatever order they were left in.
func TestPlannedOpsByID(t *testing.T) {
	b := openForOps(t)
	for i, kind := range []string{"trash", "restore", "unlink"} {
		b.exec("INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state) VALUES (?, 't1', ?, CAST('a' AS BLOB), CAST('b' AS BLOB), '7', 1, 'planned')", 3-i, kind)
	}
	b.exec("INSERT INTO ops(id, disk_id, kind, path, started_ns, state) VALUES (4, 't1', 'mkdir', CAST('d' AS BLOB), 1, 'done')")
	list, err := PlannedOps(b.t.Context(), b.sql())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != 1 || list[1].ID != 2 || list[2].ID != 3 {
		t.Fatalf("%+v", list)
	}
	if list[0].Kind != "unlink" || list[0].Path != "a" || list[0].Path2 != "b" || list[0].FileKey != "7" || list[0].Disk != "t1" {
		t.Errorf("%+v", list[0])
	}
}

// Past a page, and with a path holding several entries across its end: every entry once, in order.
func TestTrashEntriesAcrossPages(t *testing.T) {
	b := openForOps(t)
	const total = trashPageSize*2 + 7
	for i := range total {
		id := i + 1
		path := fmt.Sprintf("p%05d", i/2) // two entries of each path, so a page can end between them
		b.exec("INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state) VALUES (?, 't1', 'trash', CAST(? AS BLOB), CAST(? AS BLOB), '7', 1, 'done')", id, path, fmt.Sprint(id))
		b.exec("INSERT INTO trash(lpath, disk_id, trash_path, op_id, reason, deleted_ns, expires_ns) VALUES (CAST(? AS BLOB), 't1', CAST(? AS BLOB), ?, 'surplus', 1, 2)", path, fmt.Sprint(id), id)
	}
	b.exec("INSERT INTO ops(id, disk_id, kind, path, path2, started_ns, state) VALUES (?, 't1', 'trash', CAST('zz' AS BLOB), CAST('zz' AS BLOB), 1, 'planned')", total+1)
	b.exec("INSERT INTO trash(lpath, disk_id, trash_path, op_id, reason, deleted_ns, expires_ns) VALUES (CAST('zz' AS BLOB), 't1', CAST('zz' AS BLOB), ?, 'surplus', 1, 2)", total+1)
	list, err := TrashEntries(b.t.Context(), b.sql())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != total {
		t.Fatalf("%d entries, want %d: a planned one is not in the trash yet", len(list), total)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Path > list[i].Path || (list[i-1].Path == list[i].Path && list[i-1].OpID >= list[i].OpID) {
			t.Fatalf("out of order at %d: %+v, %+v", i, list[i-1], list[i])
		}
	}
}

// The same path can be taken for another disk's copy only by its key, and bytes tell them apart.
// What finds a copy by its place reads that place, never every copy of its disk: each operation asks once, and a
// pass that applied a time to 200,000 copies did 6 a second (specs.md §26). SQLite has no statistics here, so the
// plan is what every catalog gets.
func TestAQueryByPlaceReadsOnlyThatPlace(t *testing.T) {
	b := openForOps(t)
	for _, q := range []struct {
		name, sql string
		args      []any
	}{
		{"CopyOf", copyOfSQL, []any{"u1", []byte("a/b"), "7"}},
		{"finishCopy", finishCopySQL, []any{"u1", []byte("a/b"), "7"}},
		{"renameRow", renameRowSQL, []any{"u1", []byte("a/b"), "7"}},
		{"FinishUtimens", finishUtimensSQL, []any{"u1", []byte("a/b"), "7", 1, 2}},
		{"FinishChmod", finishChmodSQL, []any{"u1", []byte("a/b"), "7", 0o640, 2}},
	} {
		plan := queryPlan(t, b, q.sql, q.args...)
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN") || strings.HasSuffix(line, "(disk_id=?)") {
				t.Errorf("%s: %q in %q", q.name, line, plan)
			}
		}
	}
}

func TestKeyTaken(t *testing.T) {
	b := openForOps(t)
	b.exec("INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES ('u1', 'x', 'ntfs', '/u', 'media', 'Atoll', 'data', 0, '[]', 'online')")
	b.exec("INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES ('u2', 'y', 'ntfs', '/v', 'media', 'Atoll', 'data', 1, '[]', 'online')")
	b.exec("INSERT INTO files(id, lpath, lpath_key, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, CAST('Film' AS BLOB), 'FILM', 1, 1, 1, 'deferred', 'ok', 1, 1)")
	b.exec("INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (1, 'u1', 'user', 1, 'ok')")
	b.exec("INSERT INTO entries(lpath, lpath_key, disk_id, kind, mode, mtime_ns, created_by) VALUES (CAST('Dir' AS BLOB), 'DIR', 'u1', 'dir', 493, 1, 'user')")
	for _, tc := range []struct {
		disk, path string
		want       bool
	}{
		{"u1", "film", true}, {"u1", "FILM", true}, {"u1", "Film", false},
		{"u1", "dir", true}, {"u1", "Dir", false},
		{"u2", "film", false}, {"u2", "dir", false},
		{"u1", "other", false}, {"u1", "\xff", false},
	} {
		got, err := KeyTaken(b.t.Context(), b.sql(), tc.disk, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("%s %q: %v, %v", tc.disk, tc.path, got, err)
		}
	}
}
