//go:build !conformance

package disks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// NewMachine gives what the delivered binary sees, which is the machine. Taking a folder for a
// disk takes another build (specs.md §29.4), so nothing here can skip a check.
func NewMachine() (Machine, error) {
	return machine{hiberfil: &hiberfilCache{}, logFile: &logFileCache{}, marker: &markerCache{}}, nil
}

// markerCache keeps what a machine that runs for long read of each disk's marker, once per mount.
type markerCache = onceAMount[MarkerSeen]

// machine keeps what it read of hiberfil.sys, of $LogFile and of each disk's marker on each mount, for as long as it
// runs.
type machine struct {
	hiberfil *hiberfilCache
	logFile  *logFileCache
	marker   *markerCache
}

// Mount answers questions 1 and 2. A path at which no mount of its own is found forgets what was read of hiberfil.sys,
// $LogFile and the marker there: what is mounted there next is another mount, whatever id the kernel gives it.
func (m machine) Mount(mount string) MountSeen {
	seen := lookAtMount(mount)
	if seen.Path != PathOK || seen.Entry == nil || seen.Entry.MountPoint != mount {
		m.hiberfil.forget(mount)
		m.logFile.forget(mount)
		m.marker.forget(mount)
	}
	return seen
}

// Marker reads a disk's marker once per mount, as hiberfil.sys is read (specs.md §10), through a handle on the top whose
// mount is the one the check found at the path; an absence is read again when again says so, as atoll disk mark may
// have written the marker since.
func (m machine) Marker(mount, workdir string, entry *MountEntry, again bool) MarkerSeen {
	read := func() MarkerSeen {
		top, err := fsx.OpenDir(mount)
		if err != nil {
			return markerError(err)
		}
		defer func() { _ = top.Close() }()
		info, err := top.Info()
		switch {
		case err != nil:
			return markerError(err)
		case info.MountID != entry.ID:
			return MarkerSeen{}.changedMount()
		}
		return ReadMarker(top, workdir)
	}
	seen := m.marker.answer(mount, entry, fsx.UniqueMountID, read)
	if again && seen.File == MarkerAbsent {
		m.marker.forget(mount)
		seen = m.marker.answer(mount, entry, fsx.UniqueMountID, read)
	}
	return seen
}

func lookAtMount(mount string) MountSeen {
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

// Hiberfil reads hiberfil.sys once per mount (specs.md §10): the daemon checks every 30 s, and must wake no disk for it.
// The read goes through a handle on the top whose mount is the one the check found at the path.
func (m machine) Hiberfil(mount string, entry *MountEntry) HiberfilSeen {
	return m.hiberfil.answer(mount, entry, fsx.UniqueMountID, func() HiberfilSeen {
		top, err := fsx.OpenDir(mount)
		if err != nil {
			return hiberfilError(err)
		}
		defer func() { _ = top.Close() }()
		info, err := top.Info()
		switch {
		case err != nil:
			return hiberfilError(err)
		case info.MountID != entry.ID:
			return mountChanged
		}
		return ReadHiberfil(top)
	})
}

// LogFile reads $LogFile once per mount, as hiberfil.sys is read (specs.md §10), through a handle on the top whose
// mount is the one the check found at the path.
func (m machine) LogFile(mount string, entry *MountEntry) LogFileSeen {
	return m.logFile.answer(mount, entry, fsx.UniqueMountID, func() LogFileSeen {
		top, err := fsx.OpenDir(mount)
		if err != nil {
			return logFileError(err)
		}
		defer func() { _ = top.Close() }()
		info, err := top.Info()
		switch {
		case err != nil:
			return logFileError(err)
		case info.MountID != entry.ID:
			return LogFileSeen{}.changedMount()
		}
		return ReadLogFile(top)
	})
}

// PhysicalDisks reads them from /sys: a btrfs mount holds an anonymous device number, so its devices come from
// its UUID.
func (machine) PhysicalDisks(fs names.Filesystem, entry *MountEntry, uuid string) ([]string, error) {
	if fs == names.Btrfs {
		return fsx.BtrfsDisks(uuid) //nolint:wrapcheck // fsx says what it read
	}
	return fsx.PhysicalDisks(entry.Major, entry.Minor) //nolint:wrapcheck // likewise
}

func (machine) MountPointImmutable(mount string) (bool, error) {
	return fsx.MountPointImmutable(mount) //nolint:wrapcheck // likewise
}

// ReadDevice reads a few bytes of a device that holds no mounted filesystem (specs.md §22).
func (machine) ReadDevice(path string, off int64, n int) ([]byte, error) {
	return fsx.ReadAt(path, off, n) //nolint:wrapcheck // fsx says what it read
}

// DeviceNode says what a device's name leads to (specs.md §15.13).
func (machine) DeviceNode(path string) (fsx.DeviceNode, error) {
	return fsx.DeviceNodeOf(path) //nolint:wrapcheck // fsx says what it looked at
}

// Smartctl runs smartctl from the PATH (specs.md §23). Its exit status is in its output too, so only a failure to
// run it is an error: exec.ErrNotFound when it is not installed.
func (machine) Smartctl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "smartctl", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		return nil, fmt.Errorf("smartctl: %w", err)
	}
	return out.Bytes(), nil
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

// powerTimeout is how long a whole disk has to answer CHECK POWER MODE: a USB bridge that hangs must not hold the
// daemon's check.
const powerTimeout = 5 * time.Second

func (machine) Rotational(name string) (bool, error) {
	return fsx.Rotational(name) //nolint:wrapcheck // fsx says what it read
}

func (machine) BlockStat(name string) (BlockStat, error) {
	text, err := fsx.ReadBlockStat(name)
	if err != nil {
		return BlockStat{}, err //nolint:wrapcheck // likewise
	}
	return ParseBlockStat(text)
}

func (machine) PowerMode(name string) (Power, error) {
	sense, err := fsx.CheckPowerMode("/dev/"+name, powerTimeout)
	if err != nil {
		return PowerUnknown, err //nolint:wrapcheck // likewise
	}
	if p := PowerModeOf(sense); p != PowerUnknown {
		return p, nil
	}
	return PowerUnknown, fmt.Errorf("/dev/%s: %w (sense %x)", name, ErrNoPowerMode, sense)
}
