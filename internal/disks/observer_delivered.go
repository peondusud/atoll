//go:build !conformance

package disks

import (
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// NewMachine gives what the delivered binary sees, which is the machine. Taking a folder for a
// disk takes another build (specs.md §29.4), so nothing here can skip a check.
func NewMachine() (Machine, error) {
	return machine{}, nil
}

type machine struct{}

func (machine) Mount(mount string) MountSeen {
	info, err := fsx.StatPath(mount)
	switch {
	case fsx.IsAbsent(err):
		return MountSeen{Path: PathAbsent}
	case err != nil:
		return MountSeen{Path: PathIOError, Note: err.Error()}
	}
	table, err := fsx.ReadMountTable()
	if err != nil {
		return MountSeen{Path: PathIOError, Note: err.Error()}
	}
	return ParseMountInfo(table).Find(info.MountID)
}

func (machine) OpenTop(mount string) (fsx.Dir, error) {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return nil, fmt.Errorf("top: %w", err)
	}
	return top, nil
}

func (machine) Device(fs names.Filesystem, entry *MountEntry) DeviceSeen {
	head, err := fsx.ReadBlockHead(entry.Source, HeadSize)
	return seenDevice(fs, entry, head, err)
}

func (machine) ReadPastCache(fs names.Filesystem, entry *MountEntry) DeviceSeen {
	head, err := fsx.ReadBlockHeadDirect(entry.Source)
	return seenDevice(fs, entry, head, err)
}

func (machine) Checkpoint(string) {}

func seenDevice(fs names.Filesystem, entry *MountEntry, head fsx.BlockHead, err error) DeviceSeen {
	switch {
	case errors.Is(err, fsx.ErrNotBlockDevice), fsx.IsDeviceGone(err):
		return DeviceSeen{Device: DeviceGoneAway, Note: err.Error()}
	case err != nil:
		return DeviceSeen{Device: DeviceUnreadable, Note: err.Error()}
	}
	// A btrfs entry holds an anonymous number, measured. Elsewhere, a name that now leads to
	// another device must not vouch for this mount.
	if fs != names.Btrfs && (head.Major != entry.Major || head.Minor != entry.Minor) {
		return DeviceSeen{Device: DeviceGoneAway, Note: fmt.Sprintf("that name now leads to %d:%d, and the mount is on %d:%d", head.Major, head.Minor, entry.Major, entry.Minor)}
	}
	identity, err := ReadIdentity(fs, head.Data)
	if err != nil {
		return DeviceSeen{Device: DeviceOK}
	}
	return DeviceSeen{Device: DeviceOK, Identity: &identity}
}

func (machine) Root(mount, root string) RootSeen {
	return lookAtRoot(mount, root)
}

func (machine) Now() int64 {
	return time.Now().UnixNano()
}

func (machine) Space(mount string) (capacity, free int64, err error) {
	capacity, free, err = fsx.Space(mount)
	if err != nil {
		return 0, 0, fmt.Errorf("space: %w", err)
	}
	return capacity, free, nil
}

func (machine) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := fsx.OpenDir(path.Join(mount, root))
	if err != nil {
		return nil, fmt.Errorf("root: %w", err)
	}
	return dir, nil
}
