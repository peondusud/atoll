package ops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// A disk's marker is read as a marker and nothing else: whoever can write at the top of a disk that is being added,
// or in its work folder, can put anything at that name. A link is not followed, a FIFO does not hold the command, with
// the catalog's lock, for good, and a file past a marker's size is not read into memory (specs.md §10).
func TestReadMarkerAt(t *testing.T) {
	text := []byte("pool_id = \"p\"\ndisk_id = \"u1\"\nfs_uuid = \"x\"\n")
	disk := func(t *testing.T) (mount, marker string) {
		t.Helper()
		mount = t.TempDir()
		if err := os.Mkdir(filepath.Join(mount, "Atoll"), 0o755); err != nil {
			t.Fatal(err)
		}
		return mount, filepath.Join(mount, "Atoll", "disk.toml")
	}
	// read gives up after a time: a read that waits on a FIFO would hold the test for good.
	read := func(t *testing.T, mount string) (data []byte, found bool, err error) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			data, found, err = ReadMarkerAt(mount, "Atoll")
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the read of the marker never ended")
		}
		return data, found, err
	}

	t.Run("a marker", func(t *testing.T) {
		mount, marker := disk(t)
		if err := os.WriteFile(marker, text, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, found, err := read(t, mount); err != nil || !found || !bytes.Equal(got, text) {
			t.Errorf("%q, %v, %v", got, found, err)
		}
	})
	t.Run("none", func(t *testing.T) {
		mount, _ := disk(t)
		if _, found, err := read(t, mount); err != nil || found {
			t.Errorf("%v, %v", found, err)
		}
		if _, found, err := read(t, t.TempDir()); err != nil || found {
			t.Errorf("no work folder: %v, %v", found, err)
		}
	})
	t.Run("a link to a marker elsewhere", func(t *testing.T) {
		mount, marker := disk(t)
		elsewhere := filepath.Join(t.TempDir(), "disk.toml")
		if err := os.WriteFile(elsewhere, text, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, marker); err != nil {
			t.Fatal(err)
		}
		if got, _, err := read(t, mount); err == nil || !fsx.IsUnreadableName(err) {
			t.Errorf("a link was followed: %q, %v", got, err)
		}
	})
	t.Run("a work folder that is a link", func(t *testing.T) {
		mount := t.TempDir()
		elsewhere := t.TempDir()
		if err := os.WriteFile(filepath.Join(elsewhere, "disk.toml"), text, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(mount, "Atoll")); err != nil {
			t.Fatal(err)
		}
		// It is no folder of this disk: what it leads to is never read, whether that is said as an error or as no
		// marker.
		if got, found, _ := read(t, mount); found {
			t.Errorf("a linked work folder was entered: %q", got)
		}
	})
	t.Run("a FIFO", func(t *testing.T) {
		mount, marker := disk(t)
		if err := syscall.Mkfifo(marker, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := read(t, mount); err == nil {
			t.Error("a FIFO was read as a marker")
		}
	})
	t.Run("a file past a marker's size", func(t *testing.T) {
		mount, marker := disk(t)
		// Sparse: a terabyte that takes no room, and would take it all in memory.
		f, err := os.Create(marker)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(1 << 40); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if got, _, err := read(t, mount); err == nil {
			t.Errorf("%d bytes read as a marker", len(got))
		}
	})
}

// The work folder of a disk is Atoll's when root, the account that runs, or the account that owns the catalog owns it:
// any other could read what Atoll copies through it, and remove what its trash holds. A filesystem that holds no owner
// of its own, NTFS, says nothing of that, and a disk with no work folder yet has nothing to say (specs.md §10).
func TestForeignWorkdir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a folder to another account takes root")
	}
	mount := t.TempDir()
	d := &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll", FS: names.Ext4}
	trusted := []uint32{0, 4242}
	if uid, foreign, err := ForeignWorkdir(d, trusted); err != nil || foreign {
		t.Fatalf("no work folder: %d, %v, %v", uid, foreign, err)
	}
	work := filepath.Join(mount, "Atoll")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		uid     int
		fs      names.Filesystem
		foreign bool
	}{
		{0, names.Ext4, false},
		{4242, names.Ext4, false},
		{1000, names.Ext4, true},
		{1000, names.XFS, true},
		{1000, names.Btrfs, true},
		{1000, names.NTFS, false},
	} {
		if err := os.Chown(work, tc.uid, tc.uid); err != nil {
			t.Fatal(err)
		}
		d.FS = tc.fs
		uid, foreign, err := ForeignWorkdir(d, trusted)
		if err != nil || foreign != tc.foreign || (foreign && uid != uint32(tc.uid)) { //nolint:gosec // a test's own uid
			t.Errorf("%d on %s: %d, %v, %v", tc.uid, tc.fs, uid, foreign, err)
		}
	}
	// A link at its name is looked at itself, never followed: what it leads to is not the disk's.
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), work); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(work, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	d.FS = names.Ext4
	if uid, foreign, err := ForeignWorkdir(d, trusted); err != nil || !foreign || uid != 1000 {
		t.Errorf("a link: %d, %v, %v", uid, foreign, err)
	}
	// The accounts trusted: root's, this one's, and the catalog's owner's.
	file := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(file, 997, 997); err != nil {
		t.Fatal(err)
	}
	if got := TrustedOwners(file); !slices.Equal(got, []uint32{0, 997}) {
		t.Errorf("trusted: %v", got)
	}
	if got := TrustedOwners(filepath.Join(t.TempDir(), "none.db")); !slices.Equal(got, []uint32{0}) {
		t.Errorf("with no catalog: %v", got)
	}
}

// onDisk is a machine with nothing but disks to open, each call of which a test may make fail.
type onDisk struct {
	fail func(call, name string) error
}

func (m *onDisk) Mount(string) disks.MountSeen { return disks.MountSeen{} }
func (m *onDisk) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (m *onDisk) Root(string, string) disks.RootSeen { return disks.RootSeen{} }
func (m *onDisk) Now() int64                         { return 0 }
func (m *onDisk) Space(string) (int64, int64, error) { return 0, 0, nil }
func (m *onDisk) Checkpoint(string)                  {}
func (m *onDisk) OpenRoot(mount, root string) (fsx.Dir, error) {
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}

func (m *onDisk) ReadPastCache(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}

func (m *onDisk) OpenTop(mount string) (fsx.Dir, error) {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return fsx.Fault(top, func(call, name string) error {
		if m.fail != nil {
			return m.fail(call, name)
		}
		return nil
	}), nil
}

// asNTFS3G refuses a rename that must replace nothing, as ntfs-3g and lowntfs-3g do whatever the names (specs.md
// §17.1): only a link can tell whether a name is free there.
func asNTFS3G(call, _ string) error {
	if call == "rename" {
		return syscall.EINVAL
	}
	return nil
}

// A disk's copy of the catalog is written beside the one before, then put in its place in one step: at any moment
// the disk holds the one before or the new one (specs.md §9). The one before was removed first, then the new one
// renamed without replacing anything: on ntfs-3g, which refuses that rename, every backup left the disk with no copy
// at all, and on any filesystem a failure between the two did.
func TestPutCatalogReplacesInOneStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(call, name string) error
	}{
		{"ext4", nil},
		{"ntfs-3g", asNTFS3G},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := t.TempDir()
			m := &onDisk{fail: tc.fail}
			d, err := Open(m, &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			file := filepath.Join(mount, "Atoll", CatalogFolder, CatalogName)
			for _, text := range []string{"one", "two"} {
				if err := d.PutCatalog(strings.NewReader(text)); err != nil {
					t.Fatalf("%s: %v", text, err)
				}
				if got, err := os.ReadFile(file); err != nil || string(got) != text {
					t.Fatalf("the disk's copy is %q, %v, want %q", got, err, text)
				}
			}
			// A backup that cannot be put in place leaves the one before.
			m.fail = func(call, _ string) error {
				if call == "rename" || call == "plainrename" || call == "link" {
					return syscall.EACCES
				}
				return nil
			}
			if err := d.PutCatalog(strings.NewReader("three")); err == nil {
				t.Error("a backup that could not be put in place is said to be")
			}
			if got, err := os.ReadFile(file); err != nil || string(got) != "two" {
				t.Errorf("after a backup that failed, the disk's copy is %q, %v", got, err)
			}
		})
	}
}

// A disk's marker is written where nothing is, also on a driver that refuses a rename that must replace nothing:
// there a link tells whether the name is free, as for every move (specs.md §10, §17.1). On ntfs-3g no marker was
// ever written.
func TestWriteMarkerOnNTFS3G(t *testing.T) {
	const text = "format_version = 1\npool_id = \"p\"\ndisk_id = \"t1\"\nfs_uuid = \"x\"\n"
	for _, tc := range []struct {
		name string
		fail func(call, name string) error
	}{
		{"ext4", nil},
		{"ntfs-3g", asNTFS3G},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := t.TempDir()
			d, err := Open(&onDisk{fail: tc.fail}, &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			for range 2 {
				// Written, then found as it is.
				if err := d.WriteMarker(text); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := os.ReadFile(filepath.Join(mount, "Atoll", "disk.toml")); err != nil || string(got) != text {
				t.Errorf("the marker is %q, %v", got, err)
			}
			if err := d.WriteMarker("another"); !errors.Is(err, ErrOtherMarker) {
				t.Errorf("another marker: %v", err)
			}
			left, err := os.ReadDir(filepath.Join(mount, "Atoll"))
			if err != nil || len(left) != 1 {
				t.Errorf("the work folder holds %v, %v", left, err)
			}
		})
	}
}

// A new catalog starts its operations past every id the disk's work folder names, so that a new operation never meets
// an old one's folder or part (specs.md §9, §17): a folder of the trash, a folder of chunks, a copy in progress, and a
// chunk a repair was saving. The names of a repair, <op-id>.chunks and <op-id>.<index>.part, counted for nothing:
// after catalog rebuild, a repair could take the id of one whose chunks the trash still held.
func TestHighestOpID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  int64
	}{
		{"nothing", nil, 0},
		{"a folder of the trash", []string{"trash/2026-01-01/7/a"}, 7},
		{"a copy in progress", []string{"tmp/9.part", "trash/2026-01-01/7/a"}, 9},
		{"a folder of chunks", []string{"trash/2026-01-02/12.chunks/a/00000000", "trash/2026-01-01/7/a", "tmp/9.part"}, 12},
		{"a chunk on its way", []string{"tmp/15.3.part", "trash/2026-01-02/12.chunks/a/00000000", "tmp/9.part"}, 15},
		{"what Atoll never wrote", []string{"tmp/+20.part", "tmp/021.part", "tmp/22.1.2.part", "tmp/23.01.part", "tmp/24.part.old", "tmp/x.part",
			"trash/2026-01-01/025/a", "trash/2026-01-01/26.chunk/a", "trash/2026-01-01/27.chunks.old/a", "trash/2026-01-01/+28/a", "tmp/3.part"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := t.TempDir()
			if err := os.Mkdir(filepath.Join(mount, "Atoll"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, file := range tc.files {
				path := filepath.Join(mount, "Atoll", filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			d, err := Open(&onDisk{}, &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if got, err := d.HighestOpID(); err != nil || got != tc.want {
				t.Errorf("%d, %v, want %d", got, err, tc.want)
			}
		})
	}
}

// A date of the trash that cannot be read says nothing of the operations it holds: the id is not known, which the
// command that asks says, rather than one too low, which nothing would say (specs.md §9). What is no folder there
// holds none.
func TestHighestOpIDOfATrashItCannotRead(t *testing.T) {
	mount := t.TempDir()
	for _, file := range []string{"trash/2026-01-01/7/a", "trash/2026-01-02/31/a", "tmp/9.part"} {
		path := filepath.Join(mount, "Atoll", filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mount, "Atoll", "trash", "40"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &onDisk{}
	d, err := Open(m, &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, err := d.HighestOpID(); err != nil || got != 31 {
		t.Fatalf("%d, %v, want 31", got, err)
	}
	m.fail = func(call, name string) error {
		if call == "opendir" && name == "2026-01-02" {
			return syscall.EIO
		}
		return nil
	}
	if got, err := d.HighestOpID(); err == nil {
		t.Errorf("%d, with a date of the trash that cannot be read", got)
	}
}
