package readonly

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// machine is disks as folders, each mounted healthy with the identifier the configuration gives it.
type machine struct{ uuid map[string]string }

func (m machine) Mount(mount string) disks.MountSeen {
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{MountPoint: mount, Root: "/", FSType: "ext4",
		MountOptions: []string{"rw"}, SuperOptions: []string{"rw"}, Source: mount}}
}

func (m machine) Device(_ names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: m.uuid[e.MountPoint], TimePrecisionNS: 1}}
}
func (m machine) Root(string, string) disks.RootSeen { return disks.RootSeen{Folder: disks.FolderOK} }
func (m machine) Hiberfil(string, *disks.MountEntry) disks.HiberfilSeen {
	return disks.HiberfilSeen{File: disks.HiberfilAbsent}
}
func (m machine) LogFile(string, *disks.MountEntry) disks.LogFileSeen {
	return disks.LogFileSeen{File: disks.LogFileFile, Journal: disks.JournalEmpty}
}
func (m machine) Now() int64                         { return 1 }
func (m machine) Space(string) (int64, int64, error) { return 1 << 30, 1 << 29, nil }
func (m machine) OpenRoot(mount, root string) (fsx.Dir, error) {
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}
func (m machine) OpenTop(mount string) (fsx.Dir, error) {
	return fsx.OpenDir(mount) //nolint:wrapcheck // a test double
}
func (m machine) ReadPastCache(fs names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, e)
}
func (m machine) Checkpoint(string) {}
func (m machine) Marker(mount, workdir string, _ *disks.MountEntry, _ bool) disks.MarkerSeen {
	return disks.ReadMarkerAt(mount, workdir)
}

// The view's check asks each disk's marker, as a pass does (specs.md §10, §21.2): a disk whose own marker was seen and
// is gone is missing, no-marker, so the view presents nothing of it and writes nothing there; a disk whose marker is
// another pool's is readonly, other-marker, so it is read and never written; a disk whose marker no one saw, as in a
// pool from before, is written to as before.
func TestTheViewTakesADiskByItsMarker(t *testing.T) {
	top := t.TempDir()
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", filepath.Join(top, "catalog.db"), filepath.Join(top, "pool"))
	m := machine{uuid: map[string]string{}}
	for i, id := range []string{"t1", "t2", "t3"} {
		mount := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		uuid := fmt.Sprintf("3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d", i)
		m.uuid[mount] = uuid
		text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = \"ext4\"\n", id, uuid, mount)
	}
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	other, err := layout.MarkerText(layout.Marker{PoolID: "q", DiskID: "t3", FSUUID: c.Disks[2].UUID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.Disks[2].Mount, "Atoll"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Disks[2].Mount, "Atoll", layout.MarkerName), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(top, "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var rows []catalog.Disk
	for i := range c.Disks {
		d := &c.Disks[i]
		rows = append(rows, catalog.Disk{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role, Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, rows); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetMarkerSeen(t.Context(), db, "t2", c.Disks[1].UUID, 1); err != nil {
		t.Fatal(err)
	}
	roots := view.NewDiskRoots()
	defer roots.Close()
	handles := writer.NewHandles()
	defer handles.Close()
	ch := &checker{o: Options{Config: c, DB: db, Machine: m, Log: slog.New(slog.DiscardHandler)}, roots: roots, handles: handles, readable: map[string]bool{}}
	ch.check(t.Context())
	for id, want := range map[string][2]bool{"t1": {true, true}, "t2": {false, false}, "t3": {true, false}} {
		presented := roots.Root(id, func(fsx.Dir) error { return nil }) == nil
		if presented != want[0] || handles.Has(id) != want[1] {
			t.Errorf("%s: presented %v, written %v, want %v", id, presented, handles.Has(id), want)
		}
	}
}
