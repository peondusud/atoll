package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// sysBlock is where Linux names each block device by its numbers.
const sysBlock = "/sys/dev/block"

// PhysicalDisks gives the whole disks a block device lives on, by their kernel names, in byte order: a
// partition's disk, and the disks below a device-mapper or md device, followed down, through a partition of such a
// device too. A loop device, or any device with nothing below it, is its own (specs.md §10, R-06).
func PhysicalDisks(major, minor uint32) ([]string, error) {
	dir, err := filepath.EvalSymlinks(fmt.Sprintf("%s/%d:%d", sysBlock, major, minor))
	if err != nil {
		return nil, fmt.Errorf("block device %d:%d: %w", major, minor, err)
	}
	return disksBelow(dir, 0)
}

// BtrfsDisks gives the whole disks a btrfs filesystem lives on, from its devices as /sys/fs/btrfs lists them: a
// btrfs mount holds an anonymous device number, which says nothing of them.
func BtrfsDisks(uuid string) ([]string, error) {
	folder := filepath.Join("/sys/fs/btrfs", uuid, "devices")
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, fmt.Errorf("btrfs %s: %w", uuid, err)
	}
	var out []string
	for _, e := range entries {
		dir, err := filepath.EvalSymlinks(filepath.Join(folder, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("btrfs %s: %w", uuid, err)
		}
		below, err := disksBelow(dir, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, below...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("btrfs %s: no device", uuid)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// disksBelow follows a device of /sys/devices down to the disks it lives on.
func disksBelow(dir string, depth int) ([]string, error) {
	if depth > 16 {
		return nil, fmt.Errorf("%s: devices below devices, too deep", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		// A partition lives where the device it cuts lives: a disk, or an array, whose own disks are below it. Its
		// folder is inside that device's.
		return disksBelow(filepath.Dir(dir), depth+1)
	}
	slaves, err := os.ReadDir(filepath.Join(dir, "slaves"))
	if err != nil || len(slaves) == 0 {
		return []string{filepath.Base(dir)}, nil //nolint:nilerr // no slaves folder: a device with nothing below it
	}
	var out []string
	for _, s := range slaves {
		below, err := filepath.EvalSymlinks(filepath.Join(dir, "slaves", s.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		list, err := disksBelow(below, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// openTreeClone is OPEN_TREE_CLONE of linux/mount.h, which x/sys does not name; OPEN_TREE_CLOEXEC is O_CLOEXEC.
const openTreeClone = 1

// ErrNoImmutableFlag says that the filesystem under a mount point cannot say whether a folder is immutable.
var ErrNoImmutableFlag = errors.New("the filesystem under the mount point does not report the immutable flag")

// MountPointImmutable says whether the folder that a mount covers has the immutable flag (chattr +i), which keeps
// anything from writing there while the disk is unmounted (specs.md §10). It reads it through a detached,
// non-recursive clone of the mount of the parent folder, where the mounts below are not: it needs
// CAP_SYS_ADMIN, and the clone goes when its descriptor closes.
func MountPointImmutable(mount string) (bool, error) {
	parent, name := filepath.Dir(mount), filepath.Base(mount)
	tree, err := unix.OpenTree(unix.AT_FDCWD, parent, openTreeClone|unix.O_CLOEXEC)
	if err != nil {
		return false, fmt.Errorf("a clone of %s: %w", parent, err)
	}
	defer func() { _ = unix.Close(tree) }()
	var st unix.Statx_t
	if err := unix.Statx(tree, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS, &st); err != nil {
		return false, fmt.Errorf("the folder under %s: %w", mount, err)
	}
	if st.Attributes_mask&unix.STATX_ATTR_IMMUTABLE == 0 {
		return false, ErrNoImmutableFlag
	}
	return st.Attributes&unix.STATX_ATTR_IMMUTABLE != 0, nil
}
