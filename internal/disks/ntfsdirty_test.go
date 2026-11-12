package disks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/vectors"
)

type ntfsDirtyCase struct {
	superblockCase
	Dirty *bool `json:"dirty"`
}

// device reads what a case's device holds, as a block device answers: past its end, fewer bytes.
func (c *ntfsDirtyCase) device(t *testing.T) func(off int64, n int) ([]byte, error) {
	t.Helper()
	data := c.head(t)
	return func(off int64, n int) ([]byte, error) {
		if off >= int64(len(data)) {
			return nil, nil
		}
		return data[off:min(int64(len(data)), off+int64(n))], nil
	}
}

func TestNTFSDirtyVectors(t *testing.T) {
	for i, c := range vectors.Load[ntfsDirtyCase](t, "ntfs_dirty.json") {
		if c.Error == (c.Dirty != nil) {
			t.Fatalf("vector %d: want dirty, or error", i)
		}
		got, err := NTFSDirty(c.device(t))
		switch {
		case c.Error && (err == nil || !errors.Is(err, ErrNotNTFS)):
			t.Errorf("vector %d (%s): %v, %v, want it refused", i, c.Note, got, err)
		case !c.Error && (err != nil || got != *c.Dirty):
			t.Errorf("vector %d (%s): %v, %v, want %v", i, c.Note, got, err, *c.Dirty)
		}
	}
}

// deviceMachine reads one device by path; another path is not there.
type deviceMachine struct {
	Machine
	path  string
	read  func(off int64, n int) ([]byte, error)
	asked []string
}

func (m *deviceMachine) ReadDevice(path string, off int64, n int) ([]byte, error) {
	m.asked = append(m.asked, path)
	if path != m.path {
		return nil, fmt.Errorf("open: %w", fs.ErrNotExist)
	}
	return m.read(off, n)
}

// A disk's NTFS volume is read through the name udev gives its serial; a device not there, or a disk not NTFS, or a
// machine that reads no device, says so (specs.md §22).
func TestProbeNTFSDirty(t *testing.T) {
	cases := vectors.Load[ntfsDirtyCase](t, "ntfs_dirty.json")
	disk := &config.Disk{ID: "u1", UUID: "4A3F1C2B9D8E7F60", FS: names.NTFS}
	m := &deviceMachine{path: "/dev/disk/by-uuid/4A3F1C2B9D8E7F60", read: cases[1].device(t)}
	if dirty, err := ProbeNTFSDirty(m, disk); err != nil || !dirty {
		t.Errorf("%v, %v", dirty, err)
	}
	other := *disk
	other.UUID = "0000000000000000"
	if _, err := ProbeNTFSDirty(m, &other); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no device: %v", err)
	}
	ext4 := *disk
	ext4.FS = names.Ext4
	if _, err := ProbeNTFSDirty(m, &ext4); !errors.Is(err, ErrNoDevice) {
		t.Errorf("ext4: %v", err)
	}
	if _, err := ProbeNTFSDirty(struct{ Machine }{}, disk); !errors.Is(err, ErrNoDevice) {
		t.Errorf("no reader: %v", err)
	}
	_ = context.Background
}
