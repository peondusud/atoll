package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// folderDisks is a machine whose disks are folders, each of which passes its mount check.
type folderDisks struct {
	disks.Machine
	list []config.Disk
}

func (m *folderDisks) Mount(mount string) disks.MountSeen {
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: "ext4", MountOptions: []string{"rw"}, SuperOptions: []string{"rw"},
	}}
}

func (m *folderDisks) Device(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	for i := range m.list {
		if m.list[i].Mount == entry.MountPoint {
			return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: m.list[i].UUID}}
		}
	}
	return disks.DeviceSeen{Device: disks.DeviceGoneAway}
}

func (m *folderDisks) Root(string, string) disks.RootSeen {
	return disks.RootSeen{Folder: disks.FolderOK}
}
func (m *folderDisks) Now() int64 { return time.Now().UnixNano() }

// A disk whose watch ended while the disk stayed there is watched again at the next mount check, and looked at
// whole for what nobody heard meanwhile (specs.md §15.1, §15.13). A disk unmounted and mounted again between two
// checks leaves such a watch: the kernel's marks went with the filesystem that was unmounted, and the check finds
// the disk mounted both times. Here its root is removed and made again, which leaves an inotify watch the same way.
// The daemon kept the disk for watched, and it sent no event until the daemon started again.
func TestACheckWatchesAgainADiskWhoseWatchEnded(t *testing.T) {
	top := t.TempDir()
	root := filepath.Join(top, "media")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	list := []config.Disk{{ID: "t1", UUID: "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", Mount: top, Root: "media", Workdir: "Atoll", FS: names.Ext4}}
	m := &folderDisks{list: list}
	grouping, err := watch.NewGrouping(0)
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{
		opt: &Options{Machine: m, Config: &config.Config{Disks: list}}, log: slog.New(slog.DiscardHandler),
		set:      &settings{checkEvery: 30 * time.Second, disksByID: map[string]*config.Disk{"t1": &list[0]}},
		disks:    map[string]*diskState{},
		grouping: grouping,
		watcher:  watch.NewWatcher(watch.WatcherOptions{Now: m.Now, Inotify: true}),
	}
	defer l.watcher.Close()
	l.check(t.Context())
	if st := l.disks["t1"]; st == nil || st.state != disks.Online {
		t.Fatalf("the disk is %+v", st)
	}
	l.watch(t.Context(), "t1")
	if !l.disks["t1"].watched {
		t.Fatal("the disk could not be watched")
	}
	// A check with the watch alive changes nothing, and asks for no look.
	l.check(t.Context())
	if l.grouping.Len() != 0 {
		t.Fatalf("a check of a disk that is heard grouped %d events", l.grouping.Len())
	}

	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The watch tells of its end, as the loop would hear it; the disk is as mounted as before.
	select {
	case e := <-l.watcher.Events():
		l.add(e)
	case <-time.After(5 * time.Second):
	}
	for deadline := time.Now().Add(5 * time.Second); l.watcher.Alive("t1") && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	l.check(t.Context())
	if !l.disks["t1"].watched || !l.watcher.Alive("t1") {
		t.Fatalf("after the check, watched %v, alive %v", l.disks["t1"].watched, l.watcher.Alive("t1"))
	}
	// The disk is looked at whole: an overflow waits in the grouping, which brings a pass.
	ripe, _, _, _ := l.grouping.Ripe(m.Now())
	if !slices.ContainsFunc(ripe, func(g watch.Group) bool { return g.Disk == "t1" && slices.Contains(g.Kinds, watch.KindOverflow) }) {
		t.Errorf("no look at the whole disk is asked for: %+v", ripe)
	}
	// And it is heard again.
	if err := os.WriteFile(filepath.Join(root, "a.mkv"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-l.watcher.Events():
			if e.Disk == "t1" && e.Path == "a.mkv" {
				return
			}
		case <-deadline:
			t.Fatal("a file made after the check sent no event: the disk is still not heard")
		}
	}
}

// A configuration read again takes effect at once (specs.md §15.13), deletes.debounce and view.mount included. The
// grouping and the watcher kept those they were made with: a debounce changed for a disk that takes long to spin
// up, or a view moved, waited for the daemon to start again, and until then the view's own changes read as a user's.
func TestAReloadTakesTheDebounceAndTheView(t *testing.T) {
	dir := t.TempDir()
	text := func(debounce, pool string) string {
		return "pool_id = \"p\"\ncatalog = " + strconv.Quote(filepath.Join(dir, "catalog.db")) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, pool)) + "\n" +
			"[deletes]\ndebounce = " + strconv.Quote(debounce) + "\n"
	}
	first, err := config.Parse([]byte(text("30s", "pool")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.Parse([]byte(text("5s", "other")))
	if err != nil {
		t.Fatal(err)
	}
	m := &folderDisks{}
	opt := &Options{Machine: m, Config: first, Load: func() (*config.Config, error) { return second, nil }}
	set, err := readSettings(first, opt)
	if err != nil {
		t.Fatal(err)
	}
	grouping, err := watch.NewGrouping(set.debounce.Nanoseconds())
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{
		opt: opt, set: set, log: slog.New(slog.DiscardHandler), disks: map[string]*diskState{}, grouping: grouping,
		watcher: watch.NewWatcher(watch.WatcherOptions{ViewMount: set.viewMount, Now: m.Now}),
		queues:  newQueues(set.servers, nil),
	}
	defer l.watcher.Close()
	l.add(watch.Event{Disk: "t1", Path: "a", Kind: watch.KindWritten, Origin: watch.OriginUser, AtNS: 0})
	l.reload()
	if got, want := l.watcher.ViewMount(), filepath.Join(dir, "other"); got != want {
		t.Errorf("the watcher's view is at %q, want %q", got, want)
	}
	if ripe, _, _, _ := l.grouping.Ripe(10_000_000_000); len(ripe) != 1 {
		t.Errorf("ten seconds on, under a debounce of 5 s: %d groups ripe", len(ripe))
	}
}
