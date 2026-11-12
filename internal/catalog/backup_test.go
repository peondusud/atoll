package catalog

import (
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
