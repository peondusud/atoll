package fsx

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ErrNotBlockDevice says that a path does not lead to a block device.
var ErrNotBlockDevice = errors.New("not a block device")

// PathInfo is what statx says about a path itself, a symlink included.
type PathInfo struct {
	MountID uint64
	Dev     uint64
	IsDir   bool
	Ino     uint64
}

// StatPath asks statx about a path, without following a last symlink. It fails on a kernel older
// than Linux 5.8, which gives no mount id: a check that cannot ask must not conclude anything.
func StatPath(path string) (PathInfo, error) {
	var st unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_TYPE|unix.STATX_INO|unix.STATX_MNT_ID, &st)
	if err != nil {
		return PathInfo{}, fmt.Errorf("statx %q: %w", path, err)
	}
	if st.Mask&unix.STATX_MNT_ID == 0 {
		return PathInfo{}, fmt.Errorf("statx %q: this kernel gives no mount id: Atoll needs Linux 5.8 or later", path)
	}
	return PathInfo{MountID: st.Mnt_id, Dev: unix.Mkdev(st.Dev_major, st.Dev_minor), IsDir: st.Mode&unix.S_IFMT == unix.S_IFDIR, Ino: st.Ino}, nil
}

// IsAbsent says whether an error means that a path does not exist. Any other error says nothing
// about the path (invariant 4).
func IsAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)
}

// IsNotAFolder says whether something other than a folder stands on the way to a name.
func IsNotAFolder(err error) bool {
	return errors.Is(err, unix.ENOTDIR)
}

// IsNotEmpty says that a folder at a target holds something, which a rename does not replace.
func IsNotEmpty(err error) bool {
	return errors.Is(err, unix.ENOTEMPTY)
}

// IsDeviceGone says whether an error from opening a device means that it is no longer there.
func IsDeviceGone(err error) bool {
	return IsAbsent(err) || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENODEV)
}

// IsNotConnected says that a path is the mount of a FUSE filesystem whose process has gone: the kernel answers
// ENOTCONN for it until it is unmounted, and Linux 7.3 ECONNREFUSED when the process never answered its first
// request (measured).
func IsNotConnected(err error) bool {
	return errors.Is(err, unix.ENOTCONN) || errors.Is(err, unix.ECONNREFUSED)
}

// DetachMount detaches the mount at a path at once, as umount -l does: what is open there stays usable until closed.
// It takes CAP_SYS_ADMIN.
func DetachMount(path string) error {
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("umount -l %q: %w", path, err)
	}
	return nil
}

// CanReadWrite says whether this process may open a file for reading and writing: it opens it so, and closes it.
func CanReadWrite(path string) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	_ = unix.Close(fd)
	return nil
}

// ReadMountTable reads the mount table of this process.
func ReadMountTable() ([]byte, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("mount table: %w", err)
	}
	return data, nil
}

// BlockHead is the start of a block device, with its numbers.
type BlockHead struct {
	Major, Minor uint32
	Data         []byte
}

// directHeadSize is what a confirmation reads, at least the head a check reads: a whole number of
// 4 KiB sectors, the largest logical block size in use.
const directHeadSize = 68 << 10

// ReadBlockHeadDirect reads the start of a block device past the page cache: it always reaches the
// device, wakes the disk, and gives EIO on one that fails while a cached read still answers
// (specs.md §10, measured).
func ReadBlockHeadDirect(path string) (head BlockHead, err error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_DIRECT, 0)
	if err != nil {
		return BlockHead{}, fmt.Errorf("device: %w", err)
	}
	defer func() { _ = file.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return BlockHead{}, fmt.Errorf("device %q: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return BlockHead{}, fmt.Errorf("device %q: %w", path, ErrNotBlockDevice)
	}
	// O_DIRECT needs an aligned buffer, and an anonymous mapping is aligned to a page.
	buffer, err := unix.Mmap(-1, 0, directHeadSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return BlockHead{}, fmt.Errorf("device %q: %w", path, err)
	}
	defer func() { err = errors.Join(err, unix.Munmap(buffer)) }()
	done := 0
	for done < len(buffer) {
		n, err := unix.Pread(int(file.Fd()), buffer[done:], int64(done))
		if err != nil {
			return BlockHead{}, fmt.Errorf("device %q: %w", path, err)
		}
		if n == 0 {
			break
		}
		done += n
	}
	return BlockHead{Major: unix.Major(st.Rdev), Minor: unix.Minor(st.Rdev), Data: append([]byte(nil), buffer[:done]...)}, nil
}

// ReadBlockHead reads up to size bytes from the start of a block device, through the page cache:
// once they are there, this reads nothing from the device and wakes no disk (measured).
func ReadBlockHead(path string, size int) (BlockHead, error) {
	// O_NONBLOCK: what a stale name leads to now may not be a device, and must not hang this.
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return BlockHead{}, fmt.Errorf("device: %w", err)
	}
	defer func() { _ = file.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return BlockHead{}, fmt.Errorf("device %q: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return BlockHead{}, fmt.Errorf("device %q: %w", path, ErrNotBlockDevice)
	}
	data := make([]byte, size)
	n, err := io.ReadFull(file, data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return BlockHead{}, fmt.Errorf("device %q: %w", path, err)
	}
	return BlockHead{Major: unix.Major(st.Rdev), Minor: unix.Minor(st.Rdev), Data: data[:n]}, nil
}
