//go:build !conformance

package disks

import (
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// NewObserver gives the observer of the delivered binary, which looks at the machine. Taking a
// folder for a disk takes another build (specs.md §29.4), so nothing here can skip a check.
func NewObserver() (Observer, error) {
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

func (machine) Device(fs names.Filesystem, entry *MountEntry) DeviceSeen {
	head, err := fsx.ReadBlockHead(entry.Source, HeadSize)
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
