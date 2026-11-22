package view

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
)

// branchesKey is the runtime setting of mergerfs that holds its branches, on its control file (specs.md §21.1).
const branchesKey = "user.mergerfs.branches"

// ErrNoMergerfs says that no mergerfs serves the view's mount point, so there are no runtime branches to set.
var ErrNoMergerfs = errors.New("no mergerfs at the view's mount point")

// Runtime gives the branches as mergerfs takes them at runtime: each branch of the configuration whose disk
// present says is there, in configuration order, as `atoll mergerfs-config` writes it but not escaped. With no
// branch present it gives "", which must not be written: mergerfs would have no branch.
func (m *Mergerfs) Runtime(present func(disk string) bool) string {
	var parts []string
	for _, b := range m.Branches {
		if !present(b.Disk) {
			continue
		}
		part := b.Path + "=" + b.Mode
		if b.MinFree != nil {
			part += "," + mergerfsSize(*b.MinFree)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ":")
}

// mergerfsType is the type the mount table gives a mount of mergerfs.
const mergerfsType = "fuse.mergerfs"

// controlFile opens the view's mount point itself, a link there refused, checks that it is the top of a mount that the
// mount table, read by that mount's id, gives as mergerfs's, and gives the path of mergerfs's control file through that
// handle, with what closes it (specs.md §21.1; docs/threat-model.md, G19): set by its path alone, the branches went to
// whatever a link there led to, or to a file of that name in a folder that no mergerfs serves. ErrNoMergerfs when
// nothing is there, or no mergerfs.
func controlFile(mount string) (string, func(), error) {
	fd, err := unix.Open(mount, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP):
		return "", nil, ErrNoMergerfs
	case err != nil:
		return "", nil, fmt.Errorf("open the view's mount point: %w", err)
	}
	release := func() { _ = unix.Close(fd) }
	if err := isMergerfs(fd); err != nil {
		release()
		return "", nil, err
	}
	return "/proc/self/fd/" + strconv.Itoa(fd) + "/.mergerfs", release, nil
}

// isMergerfs says whether a handle on a folder is the top of a mount of mergerfs, as the mount table gives it.
func isMergerfs(fd int) error {
	var st unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_TYPE|unix.STATX_MNT_ID, &st); err != nil {
		return fmt.Errorf("look at the view's mount point: %w", err)
	}
	switch {
	case st.Mask&unix.STATX_MNT_ID == 0:
		return errors.New("look at the view's mount point: this kernel gives no mount id: Atoll needs Linux 5.8 or later")
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return ErrNoMergerfs
	case st.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && st.Attributes&unix.STATX_ATTR_MOUNT_ROOT == 0:
		return ErrNoMergerfs
	}
	data, err := fsx.ReadMountTable()
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	seen := disks.ParseMountInfo(data).Find(st.Mnt_id)
	switch {
	case seen.Path != disks.PathOK:
		return fmt.Errorf("the mount at the view's mount point: %s", seen.Note)
	case seen.Entry == nil || seen.Entry.FSType != mergerfsType:
		return ErrNoMergerfs
	}
	return nil
}

// ReadBranches reads the branches mergerfs has at runtime at the view's mount point.
func ReadBranches(mount string) (string, error) {
	control, release, err := controlFile(mount)
	if err != nil {
		return "", err
	}
	defer release()
	buf := make([]byte, 64<<10)
	n, err := unix.Lgetxattr(control, branchesKey, buf)
	switch {
	case errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP):
		return "", ErrNoMergerfs
	case err != nil:
		return "", fmt.Errorf("read the branches of mergerfs: %w", err)
	}
	return string(buf[:n]), nil
}

// WriteBranches gives mergerfs the whole list of its branches at runtime, which puts a branch back at its place
// (measured on mergerfs 2.40.2). The change does not outlive mergerfs.
func WriteBranches(mount, branches string) error {
	if branches == "" {
		return errors.New("write the branches of mergerfs: none")
	}
	control, release, err := controlFile(mount)
	if err != nil {
		return err
	}
	defer release()
	if err := unix.Lsetxattr(control, branchesKey, []byte(branches), 0); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTSUP) {
			return ErrNoMergerfs
		}
		return fmt.Errorf("write the branches of mergerfs: %w", err)
	}
	return nil
}
