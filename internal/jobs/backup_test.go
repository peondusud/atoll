package jobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/ops"
)

// A backup of the catalog is written beside it, and with maintenance.catalog_on_disks copied to each disk not failed,
// through a handle on a disk confirmed first: a disk missing, or one whose confirmation fails, takes none and has
// nothing written to it, and a write that fails on one disk keeps neither the others nor the backup from being made
// (specs.md §9, invariant 3).
func TestABackupOfTheCatalogOnTheDisks(t *testing.T) {
	b := newBenchWith(t, "[maintenance]\ncatalog_on_disks = true\n", "t1", "t2", "t3", "t4", "t5")
	b.base = strings.Replace(b.base, "id = \"t5\"\n", "id = \"t5\"\nstate = \"failed\"\n", 1)
	b.reconfigure("[maintenance]\ncatalog_on_disks = true\n")
	b.write("t1", "a", "x")
	b.scan(false)
	b.gone[b.dirs["t2"]] = true
	b.unread[b.dirs["t3"]] = true
	b.fail[b.dirs["t4"]] = func(call, name string) error {
		if call == "write" {
			return syscall.EIO
		}
		return nil
	}
	out, err := BackupCatalog(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(out.Path)
	if err != nil || len(backup) == 0 {
		t.Fatalf("the backup at %q: %v", out.Path, err)
	}
	var ids []string
	for _, d := range out.Disks {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, " ") != "t1 t2 t3 t4" {
		t.Errorf("the disks a copy was tried on: %q", ids)
	}
	onDisk := func(id string) string { return filepath.Join(b.dirs[id], "Atoll", ops.CatalogFolder, ops.CatalogName) }
	if got, err := os.ReadFile(onDisk("t1")); err != nil || !bytes.Equal(got, backup) || out.Disks[0].Err != nil {
		t.Errorf("t1's copy: %d bytes, %v, %v", len(got), err, out.Disks[0].Err)
	}
	for i, id := range []string{"t2", "t3", "t4"} {
		if out.Disks[i+1].Err == nil {
			t.Errorf("%s took a copy", id)
		}
		if _, err := os.Stat(onDisk(id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s holds a copy: %v", id, err)
		}
	}
	for _, id := range []string{"t2", "t3"} {
		if _, err := os.Stat(filepath.Join(b.dirs[id], "Atoll")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("something was written to %s, which no confirmation found: %v", id, err)
		}
	}
	if !errors.Is(out.Disks[3].Err, syscall.EIO) {
		t.Errorf("t4's error: %v", out.Disks[3].Err)
	}
	// Without the setting, the backup is written beside the catalog alone.
	b.reconfigure("")
	b.now += 60_000_000_000
	out, err = BackupCatalog(t.Context(), b.db, b.config, b)
	if err != nil || out.Disks != nil || out.Path == "" {
		t.Errorf("%+v, %v", out, err)
	}
}

// A disk its marker makes readonly takes no copy of the backup, which holds every path of the pool: one whose marker
// names another pool, and one whose own marker was seen and is gone, as a clone made before disk add. One whose own
// marker is there takes it, and its marker is recorded seen; one with none, and none seen, takes it as before
// (specs.md §9, §10).
func TestABackupGoesToNoDiskItsMarkerRefuses(t *testing.T) {
	b := newBenchWith(t, "[maintenance]\ncatalog_on_disks = true\n", "t1", "t2", "t3", "t4")
	b.write("t1", "a", "x")
	b.scan(false)
	mark := func(i int, pool string) {
		t.Helper()
		d := &b.config.Disks[i]
		text, err := layout.MarkerText(layout.Marker{PoolID: pool, DiskID: d.ID, FSUUID: d.UUID})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(b.dirs[d.ID], "Atoll"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.dirs[d.ID], "Atoll", layout.MarkerName), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mark(0, "p")
	mark(1, "other")
	if err := catalog.SetMarkerSeen(t.Context(), b.db, "t3", b.config.Disks[2].UUID, 1); err != nil {
		t.Fatal(err)
	}
	out, err := BackupCatalog(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := func(id string) string { return filepath.Join(b.dirs[id], "Atoll", ops.CatalogFolder, ops.CatalogName) }
	for i, want := range []bool{true, false, false, true} {
		id := b.config.Disks[i].ID
		if got := out.Disks[i].Err == nil; got != want {
			t.Errorf("%s took a copy: %v, want %v (%v)", id, got, want, out.Disks[i].Err)
		}
		if _, err := os.Stat(onDisk(id)); (err == nil) != want {
			t.Errorf("%s holds a copy: %v, want %v", id, err, want)
		}
	}
	var seen string
	if err := b.db.QueryRowContext(t.Context(), "SELECT group_concat(id, ' ') FROM (SELECT id FROM disks WHERE marker_seen_ns IS NOT NULL ORDER BY id)").Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if seen != "t1 t3" {
		t.Errorf("the disks whose own marker is recorded seen: %q", seen)
	}
}
