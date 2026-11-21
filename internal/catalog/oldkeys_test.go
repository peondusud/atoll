package catalog

import (
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/names"
)

// A copy that waits at its old path for a rename has a key of that path, which the collision checks look up: keys from
// another implementation or release are computed again there too, as for files and entries, never compared (specs.md
// §15.8).
func TestOldPathKeysOfAnotherVersionAreComputedAgain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	b := &testDB{t: t, db: db}
	busyPool(b)
	b.exec("UPDATE meta SET value = 'rust something' WHERE key = 'lpath_key_version'")
	b.exec("UPDATE copies SET old_lpath_key = 'stale' WHERE old_lpath IS NOT NULL")
	again, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE old_lpath = ? AND old_lpath_key = 'Z/B'", []byte("Z/b")); got != 1 {
		t.Error("the key of an old path stayed")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE old_lpath IS NULL AND old_lpath_key IS NOT NULL"); got != 0 {
		t.Error("a copy at its file's path was given a key of its own")
	}
	if got := b.count("SELECT count(*) FROM meta WHERE key = 'lpath_key_version' AND value = ?", names.CollisionKeyVersion); got != 1 {
		t.Error("the version was not recorded")
	}
}
