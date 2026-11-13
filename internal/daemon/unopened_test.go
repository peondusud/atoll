package daemon

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// refusingDisks is a machine whose disks are folders, where the files named in refuse answer EACCES to an open, as a
// rule of the system's makes them.
type refusingDisks struct {
	folderDisks
	refuse map[string]bool
}

func (m *refusingDisks) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := fsx.OpenDir(filepath.Join(mount, root))
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return fsx.Fault(dir, func(call, name string) error {
		if call == "openfile" && m.refuse[name] {
			return syscall.EACCES
		}
		return nil
	}), nil
}

func (m *refusingDisks) OpenTop(mount string) (fsx.Dir, error) {
	return fsx.OpenDir(mount) //nolint:wrapcheck // a test double
}

func (m *refusingDisks) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, entry)
}
func (m *refusingDisks) Checkpoint(string) {}
func (m *refusingDisks) Space(string) (capacity, free int64, err error) {
	return 1 << 30, 1 << 29, nil
}

// The files a slice of the hashes could not open are kept by disk until one of them opens, and bring a pass, whose
// alerts count them (specs.md §15.13, §23): nothing else would tell of a file that is never hashed. A slice that
// finds no other brings none, as each pass is followed by slices that try them again. What opens, or is gone, when
// the alerts are worked out is told of no more.
func TestTheHashesTellOfWhatTheyCouldNotOpen(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	mount := filepath.Join(dir, "t1")
	if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(mount, "media", name), []byte("x"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Parse([]byte("pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	m := &refusingDisks{folderDisks: folderDisks{list: c.Disks}, refuse: map[string]bool{"b": true, "c": true}}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), db, c, m, watch.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l := &loop{
		opt: &Options{Machine: m, Config: c, Apply: true}, log: slog.New(slog.DiscardHandler),
		set:   &settings{catalog: file, disksByID: map[string]*config.Disk{"t1": &c.Disks[0]}},
		disks: map[string]*diskState{"t1": {state: disks.Online, walked: true}},
	}
	l.hash(t.Context())
	if want := map[string][]string{"t1": {"b", "c"}}; !maps.EqualFunc(l.unopened, want, slices.Equal[[]string]) {
		t.Fatalf("kept %v, want %v", l.unopened, want)
	}
	if l.passDue == nil {
		t.Error("no pass is brought to tell of them")
	}
	// The slices after that pass try them again, and find nothing new.
	l.passDue = nil
	l.hash(t.Context())
	if l.passDue != nil || len(l.unopened["t1"]) != 2 {
		t.Errorf("the same files bring a pass again: %v, %v", l.passDue, l.unopened)
	}
	// One opens by the time the alerts are worked out, and the other is gone.
	clear(m.refuse)
	if err := os.Remove(filepath.Join(mount, "media", "c")); err != nil {
		t.Fatal(err)
	}
	if got := l.stillUnopened(); len(got) != 0 || len(l.unopened) != 0 {
		t.Errorf("still told of: %v, kept %v", got, l.unopened)
	}
}

// A file that still refuses when the alerts are worked out is still told of.
func TestWhatStillRefusesIsStillToldOf(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media", "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := []config.Disk{{ID: "t1", UUID: "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", Mount: dir, Root: "media", Workdir: "Atoll", FS: names.Ext4}}
	m := &refusingDisks{folderDisks: folderDisks{list: list}, refuse: map[string]bool{"a": true}}
	l := &loop{
		opt: &Options{Machine: m, Config: &config.Config{Disks: list}}, log: slog.New(slog.DiscardHandler),
		set:      &settings{disksByID: map[string]*config.Disk{"t1": &list[0]}},
		disks:    map[string]*diskState{"t1": {state: disks.Online, walked: true}},
		unopened: map[string][]string{"t1": {"a"}, "gone": {"z"}},
	}
	// A disk the configuration no longer lists is told of no more; one that is missing keeps what is known of it.
	if got, want := l.stillUnopened(), map[string][]string{"t1": {"a"}}; !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("told of %v, want %v", got, want)
	}
	l.disks["t1"].state = disks.Missing
	if got := l.stillUnopened(); len(got) != 0 || len(l.unopened["t1"]) != 1 {
		t.Errorf("a missing disk: told of %v, kept %v", got, l.unopened)
	}
}
