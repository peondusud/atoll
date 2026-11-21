package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// A set-aside never replaces what holds the name it gives, as a rebuild in the same second, or a clock set back, would
// meet: a log's name taken leaves everything as it was; the catalog's, once the swap went through, leaves the old one
// at fresh, which the error says (specs.md §9).
func TestSetAsideNeverReplaces(t *testing.T) {
	at := time.Date(2026, 11, 21, 6, 30, 0, 0, time.UTC).UnixNano()
	setUp := func(t *testing.T, files map[string]string) (file, fresh, old string) {
		t.Helper()
		dir := t.TempDir()
		file, fresh = filepath.Join(dir, "catalog.db"), filepath.Join(dir, "catalog.db.rebuild")
		old = file + ".old-20261121T063000Z"
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return file, fresh, old
	}
	read := func(t *testing.T, files map[string]string) {
		t.Helper()
		for name, want := range files {
			if data, err := os.ReadFile(name); err != nil || string(data) != want {
				t.Errorf("%s: %q, %v", name, data, err)
			}
		}
	}
	t.Run("its log's name taken", func(t *testing.T) {
		file, fresh, old := setUp(t, map[string]string{"catalog.db": "old", "catalog.db-wal": "old log", "catalog.db.rebuild": "new",
			"catalog.db.old-20261121T063000Z-wal": "earlier log"})
		if _, err := SetAside(file, fresh, at); !errors.Is(err, os.ErrExist) {
			t.Errorf("a name taken: %v", err)
		}
		read(t, map[string]string{file: "old", file + "-wal": "old log", fresh: "new", old + "-wal": "earlier log"})
	})
	t.Run("its name taken", func(t *testing.T) {
		file, fresh, old := setUp(t, map[string]string{"catalog.db": "old", "catalog.db.rebuild": "new",
			"catalog.db.old-20261121T063000Z": "earlier"})
		_, err := SetAside(file, fresh, at)
		var half *OldNotSetAside
		if !errors.As(err, &half) || half.At != fresh || half.Want != old || !errors.Is(err, os.ErrExist) {
			t.Errorf("a name taken: %v", err)
		}
		read(t, map[string]string{file: "new", fresh: "old", old: "earlier"})
	})
}

// What an earlier rebuild left at <catalog>.rebuild, and its log, is kept beside the catalog under a name that holds
// the time, never removed: it may be the old catalog (specs.md §9). A name taken there moves nothing.
func TestKeepLeftover(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "catalog.db.rebuild")
	at := time.Date(2026, 11, 21, 6, 30, 0, 0, time.UTC).UnixNano()
	kept := fresh + "-20261121T063000Z"
	write := func(files map[string]string) {
		t.Helper()
		for name, text := range files {
			if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	look := func(files map[string]string, gone ...string) {
		t.Helper()
		for name, want := range files {
			if data, err := os.ReadFile(name); err != nil || string(data) != want {
				t.Errorf("%s: %q, %v", name, data, err)
			}
		}
		for _, name := range gone {
			if _, err := os.Stat(name); !os.IsNotExist(err) {
				t.Errorf("%s is there: %v", name, err)
			}
		}
	}

	// Nothing left: nothing moves.
	if moved, err := KeepLeftover(fresh, at); err != nil || len(moved) != 0 {
		t.Fatalf("%v, %v", moved, err)
	}

	// A name taken where it would go: nothing moves, the log put back.
	write(map[string]string{fresh: "old", fresh + "-wal": "old log", kept: "earlier"})
	if _, err := KeepLeftover(fresh, at); !errors.Is(err, os.ErrExist) {
		t.Errorf("a name taken: %v", err)
	}
	look(map[string]string{fresh: "old", fresh + "-wal": "old log", kept: "earlier"}, kept+"-wal")
	if err := os.Remove(kept); err != nil {
		t.Fatal(err)
	}

	// The catalog and its log, kept together, the catalog said first.
	write(map[string]string{fresh + "-shm": "old shm"})
	moved, err := KeepLeftover(fresh, at)
	if err != nil {
		t.Fatal(err)
	}
	want := []Moved{{fresh, kept}, {fresh + "-wal", kept + "-wal"}, {fresh + "-shm", kept + "-shm"}}
	if !slices.Equal(moved, want) {
		t.Errorf("moved %v, want %v", moved, want)
	}
	look(map[string]string{kept: "old", kept + "-wal": "old log", kept + "-shm": "old shm"}, fresh, fresh+"-wal", fresh+"-shm")

	// A log alone is kept too.
	write(map[string]string{fresh + "-wal": "a log"})
	later := time.Date(2026, 11, 21, 6, 31, 0, 0, time.UTC).UnixNano()
	moved, err = KeepLeftover(fresh, later)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Moved{{fresh + "-wal", fresh + "-20261121T063100Z-wal"}}; !slices.Equal(moved, want) {
		t.Errorf("moved %v, want %v", moved, want)
	}
	look(map[string]string{fresh + "-20261121T063100Z-wal": "a log", kept: "old"}, fresh+"-wal")
}
