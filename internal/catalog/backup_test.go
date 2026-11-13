package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// A backup is a whole, consistent catalog of this pool, named by its time, and the folder keeps the newest
// BackupKeep (specs.md §9).
func TestBackup(t *testing.T) {
	db, file := newPool(t)
	ctx := t.Context()
	exec(t, db, `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
		VALUES (x'61', 0, 0, 1, 'deferred', 'ok', 0, 0)`)
	start := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC).UnixNano()
	var removedAll []string
	for i := range BackupKeep + 2 {
		written, removed, err := Backup(ctx, db, file, "p", start+int64(i)*int64(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(filepath.Dir(file), BackupFolder, BackupName(start+int64(i)*int64(time.Hour))); written != want {
			t.Errorf("written %s, want %s", written, want)
		}
		removedAll = append(removedAll, removed...)
	}
	if want := []string{"catalog-20261112T030000Z.db", "catalog-20261112T040000Z.db"}; !slices.Equal(removedAll, want) {
		t.Errorf("removed %q", removedAll)
	}
	names, err := fsx.ListNames(filepath.Join(filepath.Dir(file), BackupFolder))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	if len(names) != BackupKeep || names[0] != "catalog-20261112T050000Z.db" {
		t.Errorf("kept %q", names)
	}
	last := filepath.Join(filepath.Dir(file), BackupFolder, names[len(names)-1])
	if err := CheckBackup(ctx, last, "p"); err != nil {
		t.Errorf("the last backup: %v", err)
	}
	if err := CheckBackup(ctx, last, "q"); err == nil {
		t.Error("a backup of another pool passed")
	}
	// Twice in one second: the second replaces the first.
	if _, _, err := Backup(ctx, db, file, "p", start+int64(BackupKeep+1)*int64(time.Hour)); err != nil {
		t.Errorf("again in the same second: %v", err)
	}
	if problems, err := Integrity(ctx, db); err != nil || len(problems) != 0 {
		t.Errorf("the catalog: %q, %v", problems, err)
	}
}

// A part a backup left, as one that stopped for want of room or a crash leaves it, goes at the next backup: only one
// of the very same second did, so a system disk nearly full gave its last room to parts nothing ever removed. And a
// backup that fails leaves none of its own (specs.md §9).
func TestABackupLeavesNoPart(t *testing.T) {
	db, file := newPool(t)
	ctx := t.Context()
	dir := filepath.Join(filepath.Dir(file), BackupFolder)
	if err := fsx.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "catalog-20200101T000000Z.db.part")
	other := filepath.Join(dir, "notes.txt")
	for _, name := range []string{old, other} {
		if err := os.WriteFile(name, []byte("left"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC).UnixNano()
	if _, _, err := Backup(ctx, db, file, "p", now); err != nil {
		t.Fatal(err)
	}
	names, err := fsx.ListNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	// What is no part of a backup stays: the folder is not Atoll's alone.
	if want := []string{"catalog-20261112T030000Z.db", "notes.txt"}; !slices.Equal(names, want) {
		t.Errorf("the folder holds %q", names)
	}
	// A backup that is not this pool's is refused, and its part goes with it.
	if _, _, err := Backup(ctx, db, file, "another", now+int64(time.Hour)); err == nil {
		t.Fatal("a backup checked against another pool passed")
	}
	if names, err = fsx.ListNames(dir); err != nil || len(names) != 2 {
		t.Errorf("after a backup that failed, the folder holds %q, %v", names, err)
	}
}
