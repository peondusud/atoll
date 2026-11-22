package catalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a rebuild finds in a disk's trash is listed again, each entry with its operation done, of the id its folder
// has: a file's of the reason unknown with its inode, a repair's chunks of the reason repaired with none; both with no
// hash, as nothing says what the file held (specs.md §9, §17). An empty list writes nothing.
func TestListTrashAgain(t *testing.T) {
	b := openForOps(t)
	ctx := t.Context()
	if err := ListTrashAgain(ctx, b.sql(), "u1", nil); err != nil {
		t.Fatal(err)
	}
	if n := b.count("SELECT count(*) FROM ops") + b.count("SELECT count(*) FROM trash"); n != 0 {
		t.Fatalf("%d rows for nothing listed", n)
	}
	err := ListTrashAgain(ctx, b.sql(), "u1", []FoundTrash{
		{OpID: 40, Path: "A/a", TrashPath: "20260101/40/A/a", Size: 5, FileKey: "77", DeletedNS: 3, ExpiresNS: 9},
		{OpID: 41, Chunks: true, Path: "B/b", TrashPath: "41.chunks", Size: 8, FileKey: "78", DeletedNS: 4, ExpiresNS: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count(`SELECT count(*) FROM ops WHERE id = 40 AND disk_id = 'u1' AND kind = 'trash' AND path = ? AND path2 = ?
		AND file_key = '77' AND started_ns = 3 AND state = 'done'`, []byte("A/a"), []byte("20260101/40/A/a")); got != 1 {
		t.Error("the file's operation is not as found")
	}
	if got := b.count(`SELECT count(*) FROM ops WHERE id = 41 AND kind = 'repair' AND file_key IS NULL AND state = 'done'`); got != 1 {
		t.Error("the chunks' operation is not a repair with no inode")
	}
	if got := b.count(`SELECT count(*) FROM trash WHERE op_id = 40 AND lpath = ? AND disk_id = 'u1' AND size = 5 AND blake3 IS NULL
		AND reason = 'unknown' AND deleted_ns = 3 AND expires_ns = 9`, []byte("A/a")); got != 1 {
		t.Error("the file's entry is not as found")
	}
	if got := b.count(`SELECT count(*) FROM trash WHERE op_id = 41 AND reason = 'repaired' AND trash_path = ?`, []byte("41.chunks")); got != 1 {
		t.Error("the chunks' entry is not as found")
	}
	// What it lists is the trash as a reader finds it, the inode with it.
	var entries []TrashEntry
	if err := EachTrashEntry(ctx, b.sql(), func(e *TrashEntry) error { entries = append(entries, *e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "A/a" || entries[0].FileKey != "77" || entries[1].FileKey != "" {
		t.Errorf("%+v", entries)
	}
	// An operation of that id already there is refused, whole: the second entry is not listed without the first.
	err = ListTrashAgain(ctx, b.sql(), "u2", []FoundTrash{
		{OpID: 42, Path: "C/c", TrashPath: "20260101/42/C/c", DeletedNS: 1, ExpiresNS: 2},
		{OpID: 40, Path: "D/d", TrashPath: "20260101/40/D/d", DeletedNS: 1, ExpiresNS: 2},
	})
	if err == nil || !strings.Contains(err.Error(), "operation 40") {
		t.Errorf("an id met twice: %v", err)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE id = 42"); got != 0 {
		t.Error("half a listing was kept")
	}
}

// A rebuilt catalog's files start past an id, so that an old link of the guard never names a new file, and never go
// back below what they reached (specs.md §9, §17).
func TestStartFilesAfter(t *testing.T) {
	b := openForOps(t)
	ctx := t.Context()
	newFile := func(path string) int64 {
		t.Helper()
		res, err := b.sql().ExecContext(ctx, "INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, 1, 1, 1, 'deferred', 'ok', 1, 1)", []byte(path))
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := StartFilesAfter(ctx, b.sql(), 99); err != nil {
		t.Fatal(err)
	}
	if id := newFile("a"); id != 100 {
		t.Errorf("first file %d", id)
	}
	if err := StartFilesAfter(ctx, b.sql(), 5); err != nil {
		t.Fatal(err)
	}
	if id := newFile("b"); id != 101 {
		t.Errorf("a file after going back: %d", id)
	}
	if err := StartFilesAfter(ctx, b.sql(), 200); err != nil {
		t.Fatal(err)
	}
	if id := newFile("c"); id != 201 {
		t.Errorf("a file after moving on: %d", id)
	}
}

// walSize gives the size of a catalog's write-ahead log, 0 when there is none.
func walSize(t *testing.T, file string) int64 {
	t.Helper()
	info, err := os.Stat(file + "-wal")
	switch {
	case os.IsNotExist(err):
		return 0
	case err != nil:
		t.Fatal(err)
	}
	return info.Size()
}

// A checkpoint moves what the log holds into the catalog and empties the log, while another handle keeps the catalog
// open; it does so for a catalog this version refuses, of a later version, without reading its schema; and it says so
// when a writer keeps it from finishing, rather than leave a log a rebuild would set aside with half of it (specs.md
// §9).
func TestCheckpoint(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(ctx, file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(ctx, file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// One connection, which holds the log open and leaves it full: closing the last would checkpoint by itself.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES ('test', ?)", strings.Repeat("x", 10000)); err != nil {
		t.Fatal(err)
	}
	if walSize(t, file) == 0 {
		t.Fatal("nothing in the log to move")
	}
	if err := Checkpoint(ctx, file); err != nil {
		t.Fatal(err)
	}
	if n := walSize(t, file); n != 0 {
		t.Errorf("the log still holds %d bytes", n)
	}
	// What it moved is in the catalog itself: a copy of the file alone, without its log, holds it.
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	alone := filepath.Join(t.TempDir(), "alone.db")
	if err := os.WriteFile(alone, data, 0o600); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(ctx, alone)
	if err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, copied, "SELECT count(*) FROM meta WHERE key = 'test'"); got != 1 {
		t.Error("the row is not in the catalog's file")
	}
	if err := copied.Close(); err != nil {
		t.Fatal(err)
	}

	// A writer in the middle of a transaction keeps the checkpoint from finishing.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE meta SET value = 'y' WHERE key = 'test'"); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(ctx, file); err == nil {
		t.Error("checkpointed through a writer")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// A catalog of a later version: Open refuses it, and it is checkpointed all the same.
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE meta SET value = ? WHERE key = 'test'", strings.Repeat("z", 10000)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, file); err == nil {
		t.Fatal("a later version opened")
	}
	if err := Checkpoint(ctx, file); err != nil {
		t.Errorf("a later version: %v", err)
	}
	if n := walSize(t, file); n != 0 {
		t.Errorf("the log of a later version still holds %d bytes", n)
	}
}

// What is no catalog cannot be checkpointed, and says so.
func TestCheckpointOfNoCatalog(t *testing.T) {
	file := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(file, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(t.Context(), file); err == nil || !strings.Contains(err.Error(), file) {
		t.Errorf("%v", err)
	}
	if err := Checkpoint(context.Background(), filepath.Join(t.TempDir(), "none", "catalog.db")); err == nil {
		t.Error("a catalog in a folder that is not there")
	}
}
