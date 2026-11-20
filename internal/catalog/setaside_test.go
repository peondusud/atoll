package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// A swap that fails leaves the old catalog as it was, its log beside it: the log holds what was committed to it and
// not yet moved into its file, when a checkpoint could not run (specs.md §9). The new catalog stays where it was made.
func TestASwapThatFailsKeepsTheOldCatalogWhole(t *testing.T) {
	dir := t.TempDir()
	file, fresh := filepath.Join(dir, "catalog.db"), filepath.Join(dir, "catalog.db.rebuild")
	for name, text := range map[string]string{file: "old", file + "-wal": "old log", file + "-shm": "old shm"} {
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 11, 20, 6, 30, 0, 0, time.UTC).UnixNano()
	// No new catalog to swap in: the exchange fails, as it does on a filesystem without RENAME_EXCHANGE.
	if old, err := SetAside(file, fresh, at); err == nil {
		t.Fatalf("set aside as %q with nothing to swap in", old)
	}
	for name, want := range map[string]string{file: "old", file + "-wal": "old log", file + "-shm": "old shm"} {
		if data, err := os.ReadFile(name); err != nil || string(data) != want {
			t.Errorf("%s: %q, %v", name, data, err)
		}
	}
	names, err := fsx.ListNames(dir)
	slices.Sort(names)
	if want := []string{"catalog.db", "catalog.db-shm", "catalog.db-wal"}; err != nil || !slices.Equal(names, want) {
		t.Errorf("the folder holds %q, %v", names, err)
	}

	// With no old catalog and no new one, nothing is renamed.
	if old, err := SetAside(filepath.Join(dir, "other.db"), fresh, at); err == nil {
		t.Errorf("set aside as %q", old)
	}
}
