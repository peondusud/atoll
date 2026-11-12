package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A rebuilt catalog's operations start past an id, and never go back below what they reached.
func TestStartOpsAfter(t *testing.T) {
	db, _ := newPool(t)
	ctx := t.Context()
	newOp := func() int64 {
		t.Helper()
		res, err := db.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES ('d', 'trash', x'61', 0, 'done')")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	if err := StartOpsAfter(ctx, db, 9); err != nil {
		t.Fatal(err)
	}
	if id := newOp(); id != 10 {
		t.Errorf("first operation %d", id)
	}
	if err := StartOpsAfter(ctx, db, 3); err != nil {
		t.Fatal(err)
	}
	if id := newOp(); id != 11 {
		t.Errorf("an operation after going back: %d", id)
	}
}

// The new catalog takes the old one's name, with no moment without one; the old one keeps its log beside it, under
// its new name; with no old one, the new one is only renamed (specs.md §9).
func TestSetAside(t *testing.T) {
	dir := t.TempDir()
	file, fresh := filepath.Join(dir, "catalog.db"), filepath.Join(dir, "catalog.db.rebuild")
	for name, text := range map[string]string{file: "old", file + "-wal": "old log", file + "-shm": "old shm", fresh: "new"} {
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 11, 12, 6, 30, 0, 0, time.UTC).UnixNano()
	old, err := SetAside(file, fresh, at)
	if err != nil {
		t.Fatal(err)
	}
	if want := file + ".old-20261112T063000Z"; old != want {
		t.Errorf("set aside as %s", old)
	}
	for name, want := range map[string]string{file: "new", old: "old", old + "-wal": "old log", old + "-shm": "old shm"} {
		if data, err := os.ReadFile(name); err != nil || string(data) != want {
			t.Errorf("%s: %q, %v", name, data, err)
		}
	}
	for _, gone := range []string{fresh, file + "-wal", file + "-shm"} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", gone, err)
		}
	}
	// No old catalog: the new one is only renamed.
	other := filepath.Join(dir, "other.db")
	if err := os.WriteFile(fresh, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if old, err := SetAside(other, fresh, at); err != nil || old != "" {
		t.Errorf("%q, %v", old, err)
	}
	if data, _ := os.ReadFile(other); string(data) != "newer" {
		t.Errorf("%q", data)
	}
}
