//go:build conformance

package disks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// controlEnv names the control file of a conformance build (specs.md §29.4). This file holds the
// only code that reads it, and the delivered binary has none of it.
const controlEnv = "ATOLL_CONFORMANCE"

// control is the file conformance/scenarios/README.md describes.
type control struct {
	Now     string        `json:"now"`
	Disks   []controlDisk `json:"disks"`
	CrashAt string        `json:"crash_at"`
}

type controlDisk struct {
	ID       string           `json:"id"`
	UUID     string           `json:"uuid"`
	FS       names.Filesystem `json:"fs"`
	Path     string           `json:"path"`
	Capacity int64            `json:"capacity"`
	// Renameat2 and Refuses are said of a disk by a scenario, so that a folder can behave as a
	// driver does: see conformance/scenarios/README.md.
	Renameat2 *bool    `json:"renameat2"`
	Refuses   []string `json:"refuses"`
	// Device names the whole disk the folder stands for, its id when left out; Immutable says that the folder
	// under its mount point has the immutable flag (specs.md §10).
	Device    string `json:"device"`
	Immutable bool   `json:"immutable"`
	// Full says that an atoll step of the scenario said the disk has no room left, for that run.
	Full bool `json:"full"`
}

// NewMachine gives what a conformance build sees: the folders listed in the control file as
// mounted disks, and its clock.
func NewMachine() (Machine, error) {
	file := os.Getenv(controlEnv)
	if file == "" {
		return nil, errors.New("a conformance build needs " + controlEnv)
	}
	data, err := fsx.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("control file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c control
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("control file: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("control file: trailing data")
	}
	now, err := time.Parse(time.RFC3339, c.Now)
	if err != nil {
		return nil, fmt.Errorf("control file: %w", err)
	}
	return folders{c.Disks, now.UnixNano(), c.CrashAt}, nil
}

type folders struct {
	disks   []controlDisk
	now     int64
	crashAt string
}

func (f folders) find(path string) *controlDisk {
	at := slices.IndexFunc(f.disks, func(d controlDisk) bool { return d.Path == path })
	if at < 0 {
		return nil
	}
	return &f.disks[at]
}

func (f folders) Mount(mount string) MountSeen {
	d := f.find(mount)
	if d == nil {
		return MountSeen{Path: PathOK}
	}
	entry := MountEntry{Root: "/", MountPoint: mount, FSType: string(d.FS), MountOptions: []string{"rw"}, SuperOptions: []string{"rw"}}
	if d.FS == names.NTFS {
		entry.FSType = "ntfs3"
		entry.SuperOptions = append(entry.SuperOptions, "iocharset=utf8")
	}
	return MountSeen{Path: PathOK, Entry: &entry}
}

// Device has no head to read either: a folder holds a time as finely as a disk of its kind would.
func (f folders) Device(fs names.Filesystem, entry *MountEntry) DeviceSeen {
	d := f.find(entry.MountPoint)
	return DeviceSeen{Device: DeviceOK, Identity: &Identity{ID: d.UUID, TimePrecisionNS: TimePrecision(fs)}}
}

// PhysicalDisks gives the disk the control file names: two folders on one device share it.
func (f folders) PhysicalDisks(_ names.Filesystem, entry *MountEntry, _ string) ([]string, error) {
	d := f.find(entry.MountPoint)
	switch {
	case d == nil:
		return nil, fmt.Errorf("%s: no disk of the control file", entry.MountPoint)
	case d.Device != "":
		return []string{d.Device}, nil
	default:
		return []string{d.ID}, nil
	}
}

// MountPointImmutable answers as the control file says.
func (f folders) MountPointImmutable(mount string) (bool, error) {
	d := f.find(mount)
	return d != nil && d.Immutable, nil
}

// ReadPastCache has no device to read: a folder answers as the control file says.
func (f folders) ReadPastCache(fs names.Filesystem, entry *MountEntry) DeviceSeen {
	return f.Device(fs, entry)
}

// Checkpoint stops at once at the point the control file names, as kill -9 would: nothing runs
// after it, neither a deferred call nor a flush.
func (f folders) Checkpoint(point string) {
	if point == f.crashAt {
		os.Exit(crashExit)
	}
}

// crashExit is what kill -9 gives in a shell, and what a scenario expects of a crash.
const crashExit = 137

func (f folders) OpenTop(mount string) (fsx.Dir, error) {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return nil, fmt.Errorf("top: %w", err)
	}
	d := f.find(mount)
	if d == nil {
		return top, nil
	}
	return fsx.Fault(top, func(call, name string) error {
		switch {
		case (call == "rename" || call == "exchange") && d.Renameat2 != nil && !*d.Renameat2:
			return unix.EINVAL
		case (call == "rename" || call == "plainrename" || call == "link" || call == "mkdir") && slices.Contains(d.Refuses, name):
			return unix.EINVAL
		case (call == "mkdir" || call == "create") && d.Full:
			return unix.ENOSPC
		default:
			return nil
		}
	}), nil
}

func (folders) Root(mount, root string) RootSeen {
	return lookAtRoot(mount, root)
}

func (f folders) Now() int64 {
	return f.now
}

// Space says what the control file does: a folder has no capacity of its own.
func (f folders) Space(mount string) (capacity, free int64, err error) {
	d := f.find(mount)
	if d == nil {
		return 0, 0, fmt.Errorf("no disk at %q", mount)
	}
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return 0, 0, fmt.Errorf("space: %w", err)
	}
	used, err := fsx.SumSizes(top)
	if err = errors.Join(err, top.Close()); err != nil {
		return 0, 0, fmt.Errorf("space: %w", err)
	}
	return d.Capacity, max(d.Capacity-used, 0), nil
}

func (folders) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := fsx.OpenDir(path.Join(mount, root))
	if err != nil {
		return nil, fmt.Errorf("root: %w", err)
	}
	return dir, nil
}
