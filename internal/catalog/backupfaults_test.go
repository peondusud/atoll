package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// A backup whose rows break a foreign key is refused, with what is wrong, as a damaged one is: a backup is checked
// whole before it takes its name (specs.md §9).
func TestABackupWithARowWithoutItsFileIsRefused(t *testing.T) {
	db, file := newPool(t)
	ctx := t.Context()
	// A copy of no file, which only a catalog written with its foreign keys off can hold.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"PRAGMA foreign_keys = OFF",
		`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES ('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online')`,
		"INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (42, 'u1', 'user', 1, 'ok')",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	problems, err := Integrity(ctx, db)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "copies") || !strings.Contains(problems[0], "files") {
		t.Errorf("%q, %v", problems, err)
	}
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC).UnixNano()
	if _, _, err := Backup(ctx, db, file, "p", now); err == nil || !strings.Contains(err.Error(), "no row in files") {
		t.Errorf("a backup with a copy of no file: %v", err)
	}
	// Neither the backup nor its part is left.
	names, err := fsx.ListNames(filepath.Join(filepath.Dir(file), BackupFolder))
	if err != nil || len(names) != 0 {
		t.Errorf("the folder holds %q, %v", names, err)
	}
}

// A backup that cannot be written where it goes fails, and leaves nothing of its own: no part beside a name it could
// not take, and the catalog's folder as it was when the folder of the backups cannot be made (specs.md §9).
func TestABackupThatCannotBeWritten(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC).UnixNano()

	// The folder of the backups is a file.
	db, file := newPool(t)
	dir := filepath.Join(filepath.Dir(file), BackupFolder)
	if err := os.WriteFile(dir, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Backup(ctx, db, file, "p", now); err == nil {
		t.Error("a backup into a file")
	}
	if data, err := os.ReadFile(dir); err != nil || string(data) != "not a folder" {
		t.Errorf("the file in the way: %q, %v", data, err)
	}

	// The backup's name is a folder that holds something: its part cannot take that name, and goes.
	db, file = newPool(t)
	dir = filepath.Join(filepath.Dir(file), BackupFolder)
	taken := filepath.Join(dir, BackupName(now))
	if err := os.MkdirAll(filepath.Join(taken, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Backup(ctx, db, file, "p", now); err == nil {
		t.Error("a backup over a folder")
	}
	if names, err := fsx.ListNames(dir); err != nil || !slices.Equal(names, []string{BackupName(now)}) {
		t.Errorf("the folder holds %q, %v", names, err)
	}

	// A part left by an earlier backup that cannot be removed stops the backup before it writes anything: a system
	// disk with no room for one must not get another.
	db, file = newPool(t)
	dir = filepath.Join(filepath.Dir(file), BackupFolder)
	stuck := filepath.Join(dir, "catalog-20200101T000000Z.db.part")
	if err := os.MkdirAll(filepath.Join(stuck, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Backup(ctx, db, file, "p", now); err == nil {
		t.Error("a backup with a part left that cannot go")
	}
	if names, err := fsx.ListNames(dir); err != nil || !slices.Equal(names, []string{filepath.Base(stuck)}) {
		t.Errorf("the folder holds %q, %v", names, err)
	}
}

// An old backup that cannot be removed fails the backup after it is written: the new one is there, and says so, with
// what was removed before it.
func TestAnOldBackupThatCannotGo(t *testing.T) {
	db, file := newPool(t)
	ctx := t.Context()
	dir := filepath.Join(filepath.Dir(file), BackupFolder)
	start := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	// The oldest, by its name, is a folder that holds something; then as many backups as are kept, with it one too many.
	oldest := filepath.Join(dir, BackupName(start.UnixNano()))
	if err := os.MkdirAll(filepath.Join(oldest, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < BackupKeep; i++ {
		if _, _, err := Backup(ctx, db, file, "p", start.Add(time.Duration(i)*time.Hour).UnixNano()); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	written, removed, err := Backup(ctx, db, file, "p", start.Add(time.Duration(BackupKeep)*time.Hour).UnixNano())
	if err == nil || written == "" || removed != nil {
		t.Errorf("%q, %q, %v", written, removed, err)
	}
	if _, statErr := os.Stat(written); statErr != nil {
		t.Errorf("the backup written is not there: %v", statErr)
	}
}
