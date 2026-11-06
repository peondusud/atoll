package fsx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrNotARegularFile says that a name no longer leads to a regular file.
var ErrNotARegularFile = errors.New("not a regular file")

// Kind says what a name is.
type Kind int8

// The kinds of file.
const (
	KindFile Kind = iota
	KindDir
	KindSymlink
	KindOther // a FIFO, a socket or a device
)

// Stat is what statx says about a name itself, a symlink included. Times are as the filesystem
// holds them, and nanoseconds count forward, also before the epoch (specs.md §9).
type Stat struct {
	Kind     Kind
	Mode     uint32 // the permission bits
	Size     int64
	Ino      uint64
	Nlink    uint32
	MountID  uint64
	Mtime    Time
	Ctime    Time
	Birth    Time
	HasBirth bool // false when the filesystem or driver exposes none (specs.md §7)
}

// Time is a time as a filesystem gives it.
type Time struct {
	Sec  int64
	Nsec int64
}

// Dir is a handle on a folder of a disk. Everything below it is reached through handles, never
// through a path from the top, so that a folder renamed during a walk, or a mount that changed,
// cannot lead elsewhere (invariant 3), and a deep tree has no path limit. No method follows a
// symlink.
type Dir interface {
	// Names gives every name in the folder, in no order.
	Names() ([]string, error)
	// Info is the Stat of the folder itself.
	Info() (Stat, error)
	Stat(name string) (Stat, error)
	OpenDir(name string) (Dir, error)
	OpenFile(name string) (File, error)
	ReadLink(name string) (string, error)
	Close() error
}

// File is a regular file open for reading.
type File interface {
	io.Reader
	Info() (Stat, error)
	Close() error
}

type realDir struct {
	file *os.File
}

// OpenDir opens a folder given by a path whose last component is no symlink.
func OpenDir(path string) (Dir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	return &realDir{file: os.NewFile(uintptr(fd), path)}, nil
}

func (d *realDir) fd() int {
	return int(d.file.Fd())
}

func (d *realDir) Names() ([]string, error) {
	// Not the file's own position: a folder is read once, and from its start.
	names, err := d.file.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", d.file.Name(), err)
	}
	return names, nil
}

func (d *realDir) Info() (Stat, error) {
	return statx(d.fd(), "", unix.AT_EMPTY_PATH)
}

func (d *realDir) Stat(name string) (Stat, error) {
	st, err := statx(d.fd(), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return Stat{}, err
	}
	// ntfs-3g and lowntfs-3g expose none through statx, and hold it in an attribute (specs.md §7).
	if !st.HasBirth && st.Kind != KindSymlink {
		if err := d.readCreationXattr(name, &st); err != nil {
			return Stat{}, err
		}
	}
	return st, nil
}

// readCreationXattr goes through /proc, as a handle on a folder takes no name for this call, and
// so the path stays short however deep the folder is. A file with no such attribute has no
// creation time; any other error says nothing, and is the caller's to judge.
func (d *realDir) readCreationXattr(name string, st *Stat) error {
	path := "/proc/self/fd/" + strconv.Itoa(d.fd()) + "/" + name
	var raw [8]byte
	n, err := unix.Lgetxattr(path, "system.ntfs_crtime", raw[:])
	switch {
	case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
		return nil
	case err != nil:
		return fmt.Errorf("creation time of %q: %w", name, err)
	case n != len(raw):
		return nil
	}
	st.Birth, st.HasBirth = FileTime(binary.LittleEndian.Uint64(raw[:])), true
	return nil
}

// FileTime turns an NTFS FILETIME, 100 ns units since 1601-01-01 UTC, into a time since the Unix
// epoch (specs.md §7).
func FileTime(units uint64) Time {
	const (
		perSecond      = 10_000_000
		toUnixEpochSec = 11644473600
	)
	return Time{
		Sec:  int64(units/perSecond) - toUnixEpochSec,
		Nsec: int64(units%perSecond) * 100,
	}
}

func (d *realDir) OpenDir(name string) (Dir, error) {
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", name, err)
	}
	return &realDir{file: os.NewFile(uintptr(fd), name)}, nil
}

func (d *realDir) OpenFile(name string) (File, error) {
	// O_NONBLOCK: a name that has become a FIFO must not hang this.
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", name, err)
	}
	f := &realFile{file: os.NewFile(uintptr(fd), name)}
	st, err := f.Info()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if st.Kind != KindFile {
		return nil, errors.Join(fmt.Errorf("open %q: %w", name, ErrNotARegularFile), f.Close())
	}
	return f, nil
}

func (d *realDir) ReadLink(name string) (string, error) {
	for size := 4096; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(d.fd(), name, buf)
		if err != nil {
			return "", fmt.Errorf("read link %q: %w", name, err)
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

func (d *realDir) Close() error {
	if err := d.file.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

type realFile struct {
	file *os.File
}

func (f *realFile) Read(p []byte) (int, error) {
	return f.file.Read(p) // not wrapped: io.EOF must stay as it is
}

func (f *realFile) Info() (Stat, error) {
	return statx(int(f.file.Fd()), "", unix.AT_EMPTY_PATH)
}

func (f *realFile) Close() error {
	if err := f.file.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

func statx(fd int, name string, flags int) (Stat, error) {
	var st unix.Statx_t
	mask := unix.STATX_BASIC_STATS | unix.STATX_BTIME | unix.STATX_MNT_ID
	if err := unix.Statx(fd, name, flags, mask, &st); err != nil {
		return Stat{}, fmt.Errorf("statx %q: %w", name, err)
	}
	if st.Mask&unix.STATX_MNT_ID == 0 {
		return Stat{}, fmt.Errorf("statx %q: this kernel gives no mount id: Atoll needs Linux 5.8 or later", name)
	}
	out := Stat{
		Mode:     uint32(st.Mode) &^ unix.S_IFMT,
		Size:     int64(st.Size),
		Ino:      st.Ino,
		Nlink:    st.Nlink,
		MountID:  st.Mnt_id,
		Mtime:    Time{st.Mtime.Sec, int64(st.Mtime.Nsec)},
		Ctime:    Time{st.Ctime.Sec, int64(st.Ctime.Nsec)},
		Birth:    Time{st.Btime.Sec, int64(st.Btime.Nsec)},
		HasBirth: st.Mask&unix.STATX_BTIME != 0,
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
		out.Kind = KindFile
	case unix.S_IFDIR:
		out.Kind = KindDir
	case unix.S_IFLNK:
		out.Kind = KindSymlink
	default:
		out.Kind = KindOther
	}
	return out, nil
}

// OpenFileAt opens a regular file given by its path below a folder, one component at a time, so
// that no symlink is followed on the way.
func OpenFileAt(root Dir, path string) (File, error) {
	folders, name := splitPath(path)
	dir := root
	var opened []Dir
	defer func() {
		for _, d := range opened {
			_ = d.Close()
		}
	}()
	for _, folder := range folders {
		next, err := dir.OpenDir(folder)
		if err != nil {
			return nil, fmt.Errorf("open %q: %w", path, err)
		}
		opened = append(opened, next)
		dir = next
	}
	f, err := dir.OpenFile(name)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	return f, nil
}

func splitPath(path string) (folders []string, name string) {
	parts := strings.Split(path, "/")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// IsUnreadableName says whether an error about a listed name means that it cannot be reached by
// that name: gone since it was listed, a name the driver cannot read, or no longer what it was.
// It says nothing about the disk (specs.md §15.10).
func IsUnreadableName(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, ErrNotARegularFile)
}

// Space is what statfs says about a disk: all of it, and what a program may fill.
func Space(path string) (capacity, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, nil
}

// Exists says whether a path exists, and fails when it cannot be told.
func Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat: %w", err)
	}
}

// MkdirAll makes a folder and its parents, for what is not on a pool disk: the configuration and
// the catalog.
func MkdirAll(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("make folder: %w", err)
	}
	return nil
}

// WriteNew writes a new file that is not on a pool disk, whole or not at all, and never over one
// that exists.
func WriteNew(path string, data []byte) (err error) {
	temp, err := os.CreateTemp(pathDir(path), "atoll-new-")
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	defer func() { err = errors.Join(err, removeIfThere(temp.Name())) }()
	if _, err := temp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write: %w", err), temp.Close())
	}
	if err := temp.Chmod(0o644); err != nil {
		return errors.Join(fmt.Errorf("write: %w", err), temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("write: %w", err), temp.Close())
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// link, and not rename, which would replace a file that appeared meanwhile.
	if err := os.Link(temp.Name(), path); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func pathDir(path string) string {
	at := strings.LastIndexByte(path, '/')
	switch at {
	case -1:
		return "."
	case 0:
		return "/"
	default:
		return path[:at]
	}
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}

// SumSizes adds up the size of every regular file below a folder. It serves a conformance build,
// whose folders have no capacity of their own.
func SumSizes(root Dir) (int64, error) {
	names, err := root.Names()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range names {
		st, err := root.Stat(name)
		if err != nil {
			return 0, err
		}
		switch st.Kind {
		case KindFile:
			total += st.Size
		case KindDir:
			below, err := root.OpenDir(name)
			if err != nil {
				return 0, err
			}
			sum, err := SumSizes(below)
			if err := errors.Join(err, below.Close()); err != nil {
				return 0, err
			}
			total += sum
		case KindSymlink, KindOther:
		}
	}
	return total, nil
}
