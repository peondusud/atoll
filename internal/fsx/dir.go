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

// ErrNotOpened says that SyncEntry could not open what a name leads to, and so synced nothing: why is in
// the error it wraps beside it.
var ErrNotOpened = errors.New("not opened")

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
	Dev      uint64 // the device of the filesystem that holds it
	Mode     uint32 // the permission bits
	Uid      uint32
	Gid      uint32
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

// OmitTime, given to SetTimes, leaves that time as it is.
var OmitTime = Time{Nsec: unix.UTIME_OMIT}

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
	// Mkdir makes a folder with a mode, less the umask, and fails on a name that exists.
	Mkdir(name string, mode uint32) error
	// Adopt gives a folder of this folder the mode and owner of another.
	Adopt(name string, like *Stat) error
	// Rename moves a name to another folder of the same filesystem, and never replaces anything:
	// renameat2 with RENAME_NOREPLACE.
	Rename(name string, to Dir, newName string) error
	// RenamePlain moves a name with a plain rename, which replaces an empty folder at the target: for a
	// folder on a driver that refuses the flag, as a folder has no link (specs.md §15.12).
	RenamePlain(name string, to Dir, newName string) error
	// Link gives a file a second name, in a folder of the same filesystem.
	Link(name string, to Dir, newName string) error
	// Unlink removes the name of a file, and Rmdir an empty folder.
	Unlink(name string) error
	Rmdir(name string) error
	// Sync makes what was done to the names of this folder durable.
	Sync() error
	// SyncEntry makes durable what the file or folder a name leads to records of itself, its own names
	// included: on NTFS each keeps them in its own record, which ntfs3 does not write when only the folder
	// is synced, so that a device failing then leaves neither name (measured). A name it cannot open gives
	// ErrNotOpened.
	SyncEntry(name string) error
	// CreateFile makes a regular file with a mode, less the umask, and fails on a name that exists: a
	// copy in progress (specs.md §14.2). Nothing else on a pool disk is ever opened for writing.
	CreateFile(name string, mode uint32) (WritableFile, error)
	// OpenWritable opens a regular file for reading and writing, to resume a copy in progress.
	OpenWritable(name string) (WritableFile, error)
	// SetMtime sets the modification time of the folder itself.
	SetMtime(t Time) error
	Close() error
}

// File is a regular file open for reading.
type File interface {
	io.Reader
	// ReaderAt serves a resume, which reads the source from the first chunk it still needs.
	io.ReaderAt
	Info() (Stat, error)
	Close() error
}

// WritableFile is a copy in progress, open for reading and writing through its own handle, so that
// nothing done to it can reach another file by a name.
type WritableFile interface {
	io.Writer
	io.WriterAt
	io.ReaderAt
	Info() (Stat, error)
	Truncate(size int64) error
	// Sync is fsync of the file.
	Sync() error
	// SetTimes sets the access and modification times, as futimens does.
	SetTimes(atime, mtime Time) error
	// SetMode and SetOwner set the permission bits and the owner, as fchmod and fchown do.
	SetMode(mode uint32) error
	SetOwner(uid, gid uint32) error
	// DropCache asks the kernel to forget the file's pages, so that the next read comes from the disk,
	// as a verification needs (specs.md §14, step 5). Only clean pages go: sync first.
	DropCache() error
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

func (d *realDir) Mkdir(name string, mode uint32) error {
	if err := unix.Mkdirat(d.fd(), name, mode); err != nil {
		return fmt.Errorf("make folder %q: %w", name, err)
	}
	return nil
}

func (d *realDir) Adopt(name string, like *Stat) error {
	mode := unix.Fchmodat(d.fd(), name, like.Mode, 0)
	owner := unix.Fchownat(d.fd(), name, int(like.Uid), int(like.Gid), unix.AT_SYMLINK_NOFOLLOW)
	if err := errors.Join(mode, owner); err != nil {
		return fmt.Errorf("adopt %q: %w", name, err)
	}
	return nil
}

func (d *realDir) Rename(name string, to Dir, newName string) error {
	target, err := realOf(to)
	if err != nil {
		return err
	}
	if err := unix.Renameat2(d.fd(), name, target.fd(), newName, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("rename %q: %w", name, err)
	}
	return nil
}

func (d *realDir) RenamePlain(name string, to Dir, newName string) error {
	target, err := realOf(to)
	if err != nil {
		return err
	}
	if err := unix.Renameat(d.fd(), name, target.fd(), newName); err != nil {
		return fmt.Errorf("rename %q: %w", name, err)
	}
	return nil
}

func (d *realDir) Link(name string, to Dir, newName string) error {
	target, err := realOf(to)
	if err != nil {
		return err
	}
	if err := unix.Linkat(d.fd(), name, target.fd(), newName, 0); err != nil {
		return fmt.Errorf("link %q: %w", name, err)
	}
	return nil
}

func (d *realDir) Unlink(name string) error {
	if err := unix.Unlinkat(d.fd(), name, 0); err != nil {
		return fmt.Errorf("unlink %q: %w", name, err)
	}
	return nil
}

func (d *realDir) Rmdir(name string) error {
	if err := unix.Unlinkat(d.fd(), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove folder %q: %w", name, err)
	}
	return nil
}

func (d *realDir) Sync() error {
	if err := unix.Fsync(d.fd()); err != nil {
		return fmt.Errorf("fsync %q: %w", d.file.Name(), err)
	}
	return nil
}

func (d *realDir) SyncEntry(name string) error {
	// O_NONBLOCK: a name that has become a FIFO must not hang this. A folder opens as a file does.
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w: %w", name, ErrNotOpened, err)
	}
	synced := unix.Fsync(fd)
	closed := unix.Close(fd)
	switch {
	case synced != nil:
		return fmt.Errorf("fsync %q: %w", name, synced)
	case closed != nil:
		return fmt.Errorf("close %q: %w", name, closed)
	}
	return nil
}

func (d *realDir) CreateFile(name string, mode uint32) (WritableFile, error) {
	fd, err := unix.Openat(d.fd(), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, fmt.Errorf("create %q: %w", name, err)
	}
	return &realWritable{file: os.NewFile(uintptr(fd), name)}, nil
}

func (d *realDir) OpenWritable(name string) (WritableFile, error) {
	// O_NONBLOCK: a name that has become a FIFO must not hang this. It changes nothing for a regular file.
	fd, err := unix.Openat(d.fd(), name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", name, err)
	}
	f := &realWritable{file: os.NewFile(uintptr(fd), name)}
	st, err := f.Info()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if st.Kind != KindFile {
		return nil, errors.Join(fmt.Errorf("open %q: %w", name, ErrNotARegularFile), f.Close())
	}
	return f, nil
}

func (d *realDir) SetMtime(t Time) error {
	times := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, {Sec: t.Sec, Nsec: t.Nsec}}
	if err := unix.UtimesNanoAt(d.fd(), ".", times, 0); err != nil {
		return fmt.Errorf("set the time of %q: %w", d.file.Name(), err)
	}
	return nil
}

// realOf finds the folder below any wrapping, as a system call takes only a real one.
func realOf(d Dir) (*realDir, error) {
	for {
		switch v := d.(type) {
		case *realDir:
			return v, nil
		case interface{ Unwrap() Dir }:
			d = v.Unwrap()
		default:
			return nil, fmt.Errorf("not a folder of a disk: %T", d)
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

func (f *realFile) ReadAt(p []byte, off int64) (int, error) {
	return f.file.ReadAt(p, off) // not wrapped: io.EOF must stay as it is
}

func (f *realFile) Info() (Stat, error) {
	return statx(int(f.file.Fd()), "", unix.AT_EMPTY_PATH)
}

type realWritable struct {
	file *os.File
}

func (f *realWritable) fd() int {
	return int(f.file.Fd())
}

func (f *realWritable) Write(p []byte) (int, error) {
	n, err := f.file.Write(p)
	if err != nil {
		return n, fmt.Errorf("write %q: %w", f.file.Name(), err)
	}
	return n, nil
}

func (f *realWritable) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.file.WriteAt(p, off)
	if err != nil {
		return n, fmt.Errorf("write %q: %w", f.file.Name(), err)
	}
	return n, nil
}

func (f *realWritable) ReadAt(p []byte, off int64) (int, error) {
	return f.file.ReadAt(p, off) // not wrapped: io.EOF must stay as it is
}

func (f *realWritable) Info() (Stat, error) {
	return statx(f.fd(), "", unix.AT_EMPTY_PATH)
}

func (f *realWritable) Truncate(size int64) error {
	if err := unix.Ftruncate(f.fd(), size); err != nil {
		return fmt.Errorf("truncate %q: %w", f.file.Name(), err)
	}
	return nil
}

func (f *realWritable) Sync() error {
	if err := unix.Fsync(f.fd()); err != nil {
		return fmt.Errorf("fsync %q: %w", f.file.Name(), err)
	}
	return nil
}

// SetTimes goes through /proc, as utimensat takes no empty name for a handle: the name there is the
// handle's own, so nothing else can be reached by it.
func (f *realWritable) SetTimes(atime, mtime Time) error {
	times := []unix.Timespec{{Sec: atime.Sec, Nsec: atime.Nsec}, {Sec: mtime.Sec, Nsec: mtime.Nsec}}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(f.fd()), times, 0); err != nil {
		return fmt.Errorf("set the times of %q: %w", f.file.Name(), err)
	}
	return nil
}

func (f *realWritable) SetMode(mode uint32) error {
	if err := unix.Fchmod(f.fd(), mode); err != nil {
		return fmt.Errorf("chmod %q: %w", f.file.Name(), err)
	}
	return nil
}

func (f *realWritable) SetOwner(uid, gid uint32) error {
	if err := unix.Fchown(f.fd(), int(uid), int(gid)); err != nil {
		return fmt.Errorf("chown %q: %w", f.file.Name(), err)
	}
	return nil
}

func (f *realWritable) DropCache() error {
	if err := unix.Fadvise(f.fd(), 0, 0, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("drop the cache of %q: %w", f.file.Name(), err)
	}
	return nil
}

func (f *realWritable) Close() error {
	if err := f.file.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
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
		Dev:      unix.Mkdev(st.Dev_major, st.Dev_minor),
		Mode:     uint32(st.Mode) &^ unix.S_IFMT,
		Uid:      st.Uid,
		Gid:      st.Gid,
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

// IsDiskLost says whether an error means that the disk cannot be relied on any more (invariant 4).
// EROFS is here because ext4 gives it once a device has failed, and ESTALE because ntfs3 gives it for a
// name and a record that writes lost with the device left disagreeing (measured). What it gives for a
// folder in that state, EINVAL, leaves the operation planned too, with the disk as it is.
func IsDiskLost(err error) bool {
	return errors.Is(err, unix.EIO) || errors.Is(err, unix.ENOTCONN) || errors.Is(err, unix.ENODEV) ||
		errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EROFS) || errors.Is(err, unix.ESTALE)
}

// IsFlagRefused says whether renameat2 may have refused its flag rather than the move: ntfs-3g and
// lowntfs-3g give EINVAL whatever the name (measured). Only link can tell.
func IsFlagRefused(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS)
}

// IsNameRefused says whether link refused the name it was given: a driver's own rule (measured with
// windows_names, on ntfs3, ntfs-3g and lowntfs-3g).
func IsNameRefused(err error) bool {
	return errors.Is(err, unix.EINVAL)
}

// IsDenied says whether the process may not do what it asked: a permission, which says nothing about the
// disk.
func IsDenied(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// IsExisting says whether a name was already taken.
func IsExisting(err error) bool {
	return errors.Is(err, unix.EEXIST)
}

// ErrBusy says that another process holds a lock.
var ErrBusy = errors.New("another command holds it")

// LockFile takes an exclusive lock on a file that exists, without waiting, and creates nothing. The
// kernel lets go of it when the process ends, however it ends. The caller must keep the lock until
// it has closed every other use of that file: closing a descriptor of a file drops the POSIX locks a
// process holds on it, which is how SQLite locks its own.
func LockFile(path string) (release func(), err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("lock %q: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("lock %q: %w", path, ErrBusy)
		}
		return nil, fmt.Errorf("lock %q: %w", path, err)
	}
	return func() { _ = file.Close() }, nil
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
