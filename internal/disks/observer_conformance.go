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

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// controlEnv names the control file of a conformance build (specs.md §29.4). This file holds the
// only code that reads it, and the delivered binary has none of it.
const controlEnv = "ATOLL_CONFORMANCE"

// control is the file conformance/scenarios/README.md describes.
type control struct {
	Now   string        `json:"now"`
	Disks []controlDisk `json:"disks"`
}

type controlDisk struct {
	ID       string           `json:"id"`
	UUID     string           `json:"uuid"`
	FS       names.Filesystem `json:"fs"`
	Path     string           `json:"path"`
	Capacity int64            `json:"capacity"`
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
	return folders{c.Disks, now.UnixNano()}, nil
}

type folders struct {
	disks []controlDisk
	now   int64
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

func (f folders) Device(_ names.Filesystem, entry *MountEntry) DeviceSeen {
	d := f.find(entry.MountPoint)
	return DeviceSeen{Device: DeviceOK, Identity: &Identity{ID: d.UUID}}
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
