package ops

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
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
