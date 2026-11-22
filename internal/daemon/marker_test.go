package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// The daemon's check asks each disk's marker, with the catalog's record of which disks' own marker was seen (specs.md
// §10, §15.13): a disk whose own marker went is missing, no-marker, as for another identifier, so its mergerfs branch
// goes and nothing it holds counts, and no remount is tried for it; once its marker is written again, as atoll disk
// mark writes it, the next check finds it back.
func TestTheCheckFindsADiskWithoutItsMarkerMissing(t *testing.T) {
	top := t.TempDir()
	mount := filepath.Join(top, "u1")
	if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	const uuid = "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"
	list := []config.Disk{{ID: "u1", UUID: uuid, Mount: mount, Root: "media", Workdir: "Atoll", FS: names.Ext4, Role: "data"}}
	file := filepath.Join(top, "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SyncDisks(t.Context(), db, []catalog.Disk{{ID: "u1", UUID: uuid, FS: "ext4", Mount: mount, Root: "media", Workdir: "Atoll", Role: "data", State: "online"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetMarkerSeen(t.Context(), db, "u1", uuid, 1); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	m := &unpluggedBench{mounted: true, nodes: map[string]fsx.DeviceNode{"/dev/sdb1": {There: true, Block: true, Major: 8, Minor: 17}}}
	var said bytes.Buffer
	l := &loop{
		opt: &Options{Machine: m, Config: &config.Config{PoolID: "p", Disks: list}}, log: slog.New(slog.NewTextHandler(&said, nil)),
		set:     &settings{checkEvery: 30 * time.Second, catalog: file, resetReasons: []disks.Reason{disks.DeviceGone, disks.DeviceUnread, disks.IOError}},
		disks:   map[string]*diskState{},
		watcher: watch.NewWatcher(watch.WatcherOptions{Now: m.Now, Inotify: true}),
	}
	defer l.watcher.Close()
	l.check(t.Context())
	if st := l.disks["u1"]; st.state != disks.Missing || st.reset || !strings.Contains(said.String(), "reason=no-marker") {
		t.Fatalf("its marker gone: %s, reset %v\n%s", st.state, st.reset, said.String())
	}
	text, err := layout.MarkerText(layout.Marker{PoolID: "p", DiskID: "u1", FSUUID: uuid})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mount, "Atoll"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "Atoll", layout.MarkerName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	l.check(t.Context())
	if st := l.disks["u1"]; st.state != disks.Online {
		t.Errorf("its marker back: %s\n%s", st.state, said.String())
	}
}
