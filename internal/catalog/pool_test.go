package catalog

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/names"
)

func newPool(t *testing.T) (*sql.DB, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "deep", "er", "catalog.db")
	created, err := Create(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("not reported as created")
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, file
}

func TestCreateMayRunAgain(t *testing.T) {
	db, file := newPool(t)
	created, err := Create(t.Context(), file, "p")
	if err != nil || created {
		t.Errorf("again: %v, %v", created, err)
	}
	if _, err := Create(t.Context(), file, "other"); err == nil || !strings.Contains(err.Error(), "other") {
		t.Errorf("another pool: %v", err)
	}
	if got := countOf(t, db, "SELECT count(*) FROM meta"); got != 2 {
		t.Errorf("%d rows in meta", got)
	}
	// Through Open, as OpenPool would write the right version itself.
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	if _, err := Create(t.Context(), fresh, "p"); err != nil {
		t.Fatal(err)
	}
	raw, err := Open(t.Context(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if got := countOf(t, raw, "SELECT count(*) FROM meta WHERE key = 'lpath_key_version' AND value = ?", names.CollisionKeyVersion); got != 1 {
		t.Error("init does not record the keys' version")
	}
}

func TestOpenPoolRefuses(t *testing.T) {
	if _, err := OpenPool(t.Context(), filepath.Join(t.TempDir(), "none.db"), "p"); err == nil {
		t.Error("no catalog, and no error")
	}
	_, file := newPool(t)
	if _, err := OpenPool(t.Context(), file, "other"); err == nil {
		t.Error("another pool, and no error")
	}
	// A catalog that init did not finish: it has its tables, and no pool.
	empty := filepath.Join(t.TempDir(), "empty.db")
	db, err := Open(t.Context(), empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPool(t.Context(), empty, "p"); err == nil {
		t.Error("no pool, and no error")
	}
}

func countOf(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// TestKeysOfAnotherVersionAreComputedAgain: keys from another implementation or release are never
// compared (specs.md §15.8).
func TestKeysOfAnotherVersionAreComputedAgain(t *testing.T) {
	db, file := newPool(t)
	if err := SyncDisks(t.Context(), db, []Disk{oneDisk("t1", "u1", 0)}); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO files(lpath, lpath_key, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
		VALUES (?, 'stale', 1, 1, 1, 'deferred', 'ok', 1, 1), (?, 'stale', 1, 1, 1, 'deferred', 'ok', 1, 1)`, []byte("Film"), []byte("\xff"))
	exec(t, db, `INSERT INTO entries(lpath, lpath_key, disk_id, kind, created_by) VALUES (?, 'stale', 't1', 'dir', 'user')`, []byte("Dir"))
	for _, version := range []string{"rust something", ""} {
		if version == "" {
			exec(t, db, "DELETE FROM meta WHERE key = 'lpath_key_version'")
		} else {
			exec(t, db, "UPDATE meta SET value = ? WHERE key = 'lpath_key_version'", version)
		}
		exec(t, db, "UPDATE files SET lpath_key = 'stale'")
		exec(t, db, "UPDATE entries SET lpath_key = 'stale'")
		second, err := OpenPool(t.Context(), file, "p")
		if err != nil {
			t.Fatal(err)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		if got := countOf(t, db, "SELECT count(*) FROM files WHERE lpath = ? AND lpath_key = 'FILM'", []byte("Film")); got != 1 {
			t.Error("the key of a file stayed")
		}
		if got := countOf(t, db, "SELECT count(*) FROM files WHERE lpath = ? AND lpath_key IS NULL", []byte("\xff")); got != 1 {
			t.Error("a name that is not UTF-8 has a key")
		}
		if got := countOf(t, db, "SELECT count(*) FROM entries WHERE lpath_key = 'DIR'"); got != 1 {
			t.Error("the key of an entry stayed")
		}
		if got := countOf(t, db, "SELECT count(*) FROM meta WHERE key = 'lpath_key_version' AND value = ?", names.CollisionKeyVersion); got != 1 {
			t.Error("the version was not recorded")
		}
	}
	// With the right version, nothing is computed again.
	exec(t, db, "UPDATE files SET lpath_key = 'kept'")
	second, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, "SELECT count(*) FROM files WHERE lpath_key = 'kept'"); got != 2 {
		t.Error("keys of the right version were computed again")
	}
}

func oneDisk(id, uuid string, position int) Disk {
	return Disk{ID: id, UUID: uuid, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: position, State: "online"}
}

// TestFileReaderPages: past one page, nothing is lost or given twice, and a file with no copy is
// still a file.
func TestFileReaderPages(t *testing.T) {
	db, _ := newPool(t)
	if err := SyncDisks(t.Context(), db, []Disk{oneDisk("t1", "u1", 0), oneDisk("t2", "u2", 1)}); err != nil {
		t.Fatal(err)
	}
	const total = 2*filePageSize + 37
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range total {
		// Paths whose byte order is not that of their ids.
		path := []byte(fmt.Sprintf("%05d", total-i))
		result, err := tx.ExecContext(t.Context(), `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 1, 1, 1, 'deferred', 'ok', 1, 1)`, path)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if i%100 == 7 {
			continue
		}
		for _, disk := range []string{"t2", "t1"} {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO copies(file_id, disk_id, size, created_by, first_seen_ns, state) VALUES (?, ?, 1, 'user', 1, 'ok')`, id, disk); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	reader := NewFileReader(db)
	seen, last, withCopies := 0, "", 0
	const withNone = 21 // i%100 == 7, from 7 to 2007
	for {
		page, err := reader.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > filePageSize {
			t.Errorf("a page of %d files", len(page))
		}
		for i := range page {
			f := &page[i]
			seen++
			if f.Path <= last {
				t.Errorf("%q after %q", f.Path, last)
			}
			last = f.Path
			switch len(f.Copies) {
			case 0:
			case 2:
				withCopies++
				if f.Copies[0].Disk != "t1" || f.Copies[1].Disk != "t2" {
					t.Errorf("%q: copies %+v", f.Path, f.Copies)
				}
			default:
				t.Errorf("%q: %d copies", f.Path, len(f.Copies))
			}
		}
	}
	if seen != total {
		t.Errorf("%d files, want %d", seen, total)
	}
	if withCopies != total-withNone {
		t.Errorf("%d files with copies, want %d", withCopies, total-withNone)
	}
}

func TestSyncDisks(t *testing.T) {
	db, _ := newPool(t)
	seen := oneDisk("a", "ua", 0)
	seen.Seen, seen.Capacity, seen.Free, seen.LastSeenNS, seen.TimePrecisionNS = true, 10, 5, 7, 100
	seen.Tags, seen.Domain = []string{"usb", "sata"}, "d1"
	sync := func(list ...Disk) error { return SyncDisks(t.Context(), db, list) }
	if err := sync(seen, oneDisk("b", "ub", 1), oneDisk("c", "uc", 2)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'a' AND tags = '["usb","sata"]' AND domain = 'd1' AND capacity_bytes = 10 AND free_bytes = 5 AND last_seen_ns = 7 AND time_precision_ns = 100`); got != 1 {
		t.Error("what a check saw was not written")
	}
	list, err := Disks(t.Context(), db)
	if err != nil || len(list) != 3 || list[0].TimePrecisionNS != 100 || list[1].TimePrecisionNS != 0 {
		t.Errorf("%+v, %v", list, err)
	}
	// A disk the check could not confirm keeps what was last seen.
	missing := oneDisk("a", "ua", 0)
	missing.State = "missing"
	if err := sync(missing, oneDisk("b", "ub", 1), oneDisk("c", "uc", 2)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'a' AND state = 'missing' AND capacity_bytes = 10 AND free_bytes = 5 AND last_seen_ns = 7 AND time_precision_ns = 100 AND tags = '[]' AND domain IS NULL`); got != 1 {
		t.Error("a missing disk lost what was last seen, or kept old settings")
	}
	// Seen again by a check that read no head, it keeps the precision an earlier one read (specs.md §9).
	unread := oneDisk("a", "ua", 0)
	unread.Seen, unread.Capacity, unread.Free, unread.LastSeenNS = true, 10, 5, 8
	if err := sync(unread, oneDisk("b", "ub", 1), oneDisk("c", "uc", 2)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'a' AND time_precision_ns = 100 AND last_seen_ns = 8`); got != 1 {
		t.Error("a check that read no head lost the precision")
	}
	// The order is the configuration's, and it may change.
	if err := sync(oneDisk("c", "uc", 0), oneDisk("b", "ub", 1), missing); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'c' AND position = 0`); got != 1 {
		t.Error("the position did not follow")
	}
	// Retired ones go after the others, by id.
	if err := sync(oneDisk("a", "ua", 0)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE (id = 'b' AND state = 'retired' AND position = 1) OR (id = 'c' AND state = 'retired' AND position = 2)`); got != 2 {
		t.Error("retired disks are not after the others")
	}
	// An id that holds nothing may take another uuid, and a uuid another id.
	if err := sync(oneDisk("a", "ua2", 0)); err != nil {
		t.Fatal(err)
	}
	if err := sync(oneDisk("z", "ua2", 0)); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'z' AND fs_uuid = 'ua2'`); got != 1 {
		t.Error("a uuid free to move did not")
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'a'`); got != 0 {
		t.Error("the id it left is still there")
	}
	// With something on it, neither.
	exec(t, db, `INSERT INTO entries(lpath, disk_id, kind, created_by) VALUES (?, 'z', 'dir', 'user')`, []byte("x"))
	if err := sync(oneDisk("z", "other", 0)); err == nil {
		t.Error("an id with entries took another uuid")
	}
	if err := sync(oneDisk("y", "ua2", 0)); err == nil {
		t.Error("a uuid with entries moved to another id")
	}
	if got := countOf(t, db, `SELECT count(*) FROM disks WHERE id = 'z' AND fs_uuid = 'ua2' AND state = 'online'`); got != 1 {
		t.Error("a refused sync changed something")
	}
}

// A walk records the mode of a new copy, and gives one to a row that has none; a row given one since keeps it
// (specs.md §9).
func TestAWalkRecordsModes(t *testing.T) {
	db, _ := newPool(t)
	if err := SyncDisks(t.Context(), db, []Disk{oneDisk("t1", "u1", 0)}); err != nil {
		t.Fatal(err)
	}
	walk := &Walk{Copies: []NewCopy{{Path: "a", Size: 1, FileKey: "1", Nlink: 1, Mode: 0o640}, {Path: "b", Size: 1, FileKey: "2", Nlink: 1, Mode: 0o644}, {Path: "c", Size: 1, FileKey: "3", Nlink: 1, Mode: 0o644}}}
	if err := WriteWalk(t.Context(), db, "t1", 1, walk); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, "SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST('a' AS BLOB) AND c.mode = 416"); got != 1 {
		t.Error("a new copy has no mode")
	}
	exec(t, db, "UPDATE copies SET mode = NULL WHERE file_id IN (SELECT id FROM files WHERE lpath IN (CAST('b' AS BLOB), CAST('c' AS BLOB)))")
	exec(t, db, "UPDATE copies SET mode = 420 WHERE file_id = (SELECT id FROM files WHERE lpath = CAST('c' AS BLOB))")
	var b, c int64
	if err := db.QueryRowContext(t.Context(), "SELECT (SELECT id FROM files WHERE lpath = CAST('b' AS BLOB)), (SELECT id FROM files WHERE lpath = CAST('c' AS BLOB))").Scan(&b, &c); err != nil {
		t.Fatal(err)
	}
	if err := WriteWalk(t.Context(), db, "t1", 2, &Walk{Modes: []CopyMode{{FileID: b, Mode: 0o600}, {FileID: c, Mode: 0o600}}}); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, db, "SELECT count(*) FROM copies WHERE (file_id = ? AND mode = 384) OR (file_id = ? AND mode = 420)", b, c); got != 2 {
		t.Error("a row without a mode did not get it, or a row with one lost it")
	}
}

// What the mass-modification guard of a disk counts and lists: its modified events within the window, after the last
// answer, not answered, a path that is not UTF-8 included (specs.md §15.6).
func TestTheModifyGuardReadsItsEvents(t *testing.T) {
	db, _ := newPool(t)
	event := func(ts int64, kind, data string) {
		exec(t, db, "INSERT INTO events(ts_ns, level, kind, data) VALUES (?, 'info', ?, ?)", ts, kind, data)
	}
	event(10, EventModified, `{"disk":"t1","path":"old"}`)
	event(100, EventModified, `{"disk":"t1","path":"z"}`)
	event(101, EventModified, `{"disk":"t1","path_b64":"YP8="}`)
	event(102, EventModified, `{"disk":"t1","path":"answered","answered":true}`)
	event(103, EventModified, `{"disk":"t2","path":"elsewhere"}`)
	read, err := ReadModifyGuard(t.Context(), db, "t1", 110, 50)
	if err != nil || read.Count != 2 || read.Stands || read.Answer != "" {
		t.Fatalf("%+v, %v", read, err)
	}
	paths, err := ModifiedPaths(t.Context(), db, "t1", 110, 50, read.AnswerID)
	if err != nil || !slices.Equal(paths, []string{"`\xff", "z"}) {
		t.Errorf("%q, %v", paths, err)
	}
	if upTo, err := AnswerModifyGuard(t.Context(), db, "t1", EventModifyGuardConfirm, 111); err != nil || upTo.Valid {
		t.Errorf("an answer with no guard: %+v, %v", upTo, err)
	}
	event(120, EventModifyGuard, `{"disk":"t1"}`)
	if read, err = ReadModifyGuard(t.Context(), db, "t1", 130, 50); err != nil || !read.Stands || read.GuardNS != 120 {
		t.Fatalf("%+v, %v", read, err)
	}
	upTo, err := AnswerModifyGuard(t.Context(), db, "t1", EventModifyGuardReject, 140)
	if err != nil || !upTo.Valid || upTo.Int64 != 120 {
		t.Fatalf("%+v, %v", upTo, err)
	}
	read, err = ReadModifyGuard(t.Context(), db, "t1", 150, 50)
	if err != nil || read.Stands || read.Count != 0 || read.Answer != EventModifyGuardReject || !read.Answers(120) || read.Answers(121) {
		t.Errorf("%+v, %v", read, err)
	}
}
