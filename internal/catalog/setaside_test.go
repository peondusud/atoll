package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// A swap that went through puts the new catalog in place, whatever comes after: when the old one cannot then take its
// name, <catalog>.old-<time>, the error says so, where the old one stays, at the new one's name, and where its log went,
// beside the name it was to take, so that renaming it there puts them together (specs.md §9, §25).
func TestARenameThatFailsAfterTheSwapSaysTheNewCatalogIsInPlace(t *testing.T) {
	for _, logged := range []bool{true, false} {
		t.Run(map[bool]string{true: "with its log", false: "with no log"}[logged], func(t *testing.T) {
			dir := t.TempDir()
			file, fresh := filepath.Join(dir, "catalog.db"), filepath.Join(dir, "catalog.db.rebuild")
			old := file + ".old-20261120T063000Z"
			files := map[string]string{file: "old", fresh: "new"}
			if logged {
				files[file+"-wal"], files[file+"-shm"] = "old log", "old shm"
			}
			for name, text := range files {
				if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A folder at the old one's name: the rename after the swap fails, as one on a disk that fails would.
			if err := os.MkdirAll(filepath.Join(old, "taken"), 0o700); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 11, 20, 6, 30, 0, 0, time.UTC).UnixNano()
			got, err := SetAside(file, fresh, at)
			if err == nil {
				t.Fatalf("set aside as %q", got)
			}
			want := map[string]string{file: "new", fresh: "old"}
			var log []string
			if logged {
				want[old+"-wal"], want[old+"-shm"] = "old log", "old shm"
				log = []string{old + "-wal", old + "-shm"}
			}
			for name, text := range want {
				if data, err := os.ReadFile(name); err != nil || string(data) != text {
					t.Errorf("%s: %q, %v", name, data, err)
				}
			}
			var half *OldNotSetAside
			if !errors.As(err, &half) {
				t.Fatalf("not said as the new catalog in place: %v", err)
			}
			if half.File != file || half.At != fresh || half.Want != old || !slices.Equal(half.Log, log) || !errors.Is(err, syscall.EEXIST) {
				t.Errorf("%+v", half)
			}
			for _, part := range append([]string{"in place", file, fresh, old}, log...) {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%q says nothing of %q", err, part)
				}
			}
		})
	}
}
