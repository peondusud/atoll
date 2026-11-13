package ops

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
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
