package fsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestWhatAnErrorSays: each errno, and what each question about it answers. A disk that fails is never a name that
// is absent, and a name that is absent never a disk that fails (invariant 4); what refuses an account, a name or a
// flag says nothing of the disk (specs.md §15.10, §17.1).
func TestWhatAnErrorSays(t *testing.T) {
	questions := []struct {
		name string
		ask  func(error) bool
	}{
		{"absent", IsAbsent}, {"not-a-folder", IsNotAFolder}, {"not-empty", IsNotEmpty}, {"device-gone", IsDeviceGone},
		{"not-connected", IsNotConnected}, {"unreadable-name", IsUnreadableName}, {"access-refused", IsAccessRefused},
		{"disk-lost", IsDiskLost}, {"out-of-files", IsOutOfFiles}, {"no-room", IsNoRoom}, {"flag-refused", IsFlagRefused},
		{"name-refused", IsNameRefused}, {"denied", IsDenied}, {"existing", IsExisting}, {"owner-refused", IsOwnerRefused},
		{"xattr-refused", IsXattrRefused}, {"no-xattr", IsNoXattr},
	}
	for err, want := range map[error][]string{
		syscall.ENOENT:       {"absent", "device-gone", "unreadable-name"},
		syscall.ENOTDIR:      {"absent", "not-a-folder", "device-gone", "unreadable-name"},
		syscall.ELOOP:        {"unreadable-name"},
		syscall.ENOTEMPTY:    {"not-empty"},
		syscall.EIO:          {"disk-lost"},
		syscall.ENOTCONN:     {"not-connected", "disk-lost"},
		syscall.ECONNREFUSED: {"not-connected"},
		syscall.ENODEV:       {"device-gone", "disk-lost"},
		syscall.ENXIO:        {"device-gone", "disk-lost"},
		syscall.EROFS:        {"disk-lost"},
		syscall.ESTALE:       {"disk-lost"},
		syscall.EACCES:       {"access-refused", "denied"},
		syscall.EPERM:        {"denied", "owner-refused", "xattr-refused"},
		syscall.EMFILE:       {"out-of-files"},
		syscall.ENFILE:       {"out-of-files"},
		syscall.ENOSPC:       {"no-room", "xattr-refused"},
		syscall.EDQUOT:       {"xattr-refused"},
		syscall.EINVAL:       {"flag-refused", "name-refused", "owner-refused", "xattr-refused"},
		syscall.ENOSYS:       {"flag-refused"},
		syscall.EEXIST:       {"existing"},
		syscall.EOPNOTSUPP:   {"owner-refused", "xattr-refused"},
		syscall.ERANGE:       {"xattr-refused"},
		syscall.E2BIG:        {"xattr-refused"},
		syscall.ENAMETOOLONG: {"xattr-refused"},
		syscall.ENODATA:      {"no-xattr"},
		ErrNotARegularFile:   {"unreadable-name"},
		ErrNotALink:          {"unreadable-name"},
		errInjected:          nil,
	} {
		// As a caller sees it: wrapped, maybe twice.
		wrapped := errors.Join(errors.New("context"), errors.Join(err))
		for _, q := range questions {
			if got := q.ask(wrapped); got != slices.Contains(want, q.name) {
				t.Errorf("%v: %s says %v", err, q.name, got)
			}
		}
	}
}

// The files of the system disk, the catalog's and the configuration's: each answers what it was asked, and an error
// that keeps its cause otherwise.
func TestSystemFiles(t *testing.T) {
	top := t.TempDir()
	a, b := filepath.Join(top, "a"), filepath.Join(top, "b")
	if err := os.WriteFile(a, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadFile(a); err != nil || string(data) != "first" {
		t.Errorf("read: %q, %v", data, err)
	}
	if _, err := ReadFile(filepath.Join(top, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read what is not there: %v", err)
	}
	// Both names swap at once: at no moment is either free.
	if err := ExchangeFiles(a, b); err != nil {
		t.Fatal(err)
	}
	if read(t, top, "a") != "second" || read(t, top, "b") != "first" {
		t.Errorf("after the exchange: %q, %q", read(t, top, "a"), read(t, top, "b"))
	}
	if err := ExchangeFiles(a, filepath.Join(top, "none")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("an exchange with what is not there: %v", err)
	}
	if read(t, top, "a") != "second" {
		t.Error("an exchange that failed moved a name")
	}
	r, err := OpenSystemFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(r); err != nil || string(data) != "second" {
		t.Errorf("opened: %q, %v", data, err)
	}
	_ = r.Close()
	if _, err := OpenSystemFile(filepath.Join(top, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("open what is not there: %v", err)
	}
	// A rename replaces what is at the new name.
	if err := RenameFile(a, b); err != nil {
		t.Fatal(err)
	}
	if read(t, top, "b") != "second" {
		t.Errorf("renamed: %q", read(t, top, "b"))
	}
	if err := RenameFile(a, b); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rename of what is not there: %v", err)
	}
	if names, err := ListNames(top); err != nil || !slices.Equal(names, []string{"b"}) {
		t.Errorf("names: %v, %v", names, err)
	}
	if _, err := ListNames(filepath.Join(top, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("names of what is not there: %v", err)
	}
	if err := RemoveFile(b); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFile(b); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a removal of what is not there: %v", err)
	}
	if err := MkdirAll(filepath.Join(top, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAll(filepath.Join(top, "file", "y")); err == nil {
		t.Error("a folder made below a file")
	}
}

// ReadAt reads a part of a file, fewer bytes where it ends first and none past it; nothing there is fs.ErrNotExist,
// as its documentation says.
func TestReadAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off  int64
		n    int
		want string
	}{{0, 4, "0123"}, {6, 10, "6789"}, {10, 4, ""}, {20, 4, ""}} {
		if got, err := ReadAt(path, tc.off, tc.n); err != nil || string(got) != tc.want {
			t.Errorf("%d bytes from %d: %q, %v", tc.n, tc.off, got, err)
		}
	}
	if _, err := ReadAt(filepath.Join(filepath.Dir(path), "none"), 0, 4); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("nothing there: %v", err)
	}
	if _, err := ReadAt(path, -1, 4); err == nil {
		t.Error("read before the start")
	}
}

// Readlink reads where a link leads, without following it; what is no link is an error.
func TestReadlink(t *testing.T) {
	top := t.TempDir()
	link := filepath.Join(top, "exe")
	if err := os.Symlink("/usr/bin/atoll", link); err != nil {
		t.Fatal(err)
	}
	if got, err := Readlink(link); err != nil || got != "/usr/bin/atoll" {
		t.Errorf("%q, %v", got, err)
	}
	if _, err := Readlink(top); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("a folder: %v", err)
	}
}

// Whether a path is there fails when that cannot be told, as a name too long for any filesystem or a loop of links:
// nothing is concluded from a look that could not be made.
func TestALookThatCannotBeMadeConcludesNothing(t *testing.T) {
	top := t.TempDir()
	long := filepath.Join(top, strings.Repeat("n", 300))
	if there, err := Exists(long); there || err == nil {
		t.Errorf("a name too long: %v, %v", there, err)
	}
	loop := filepath.Join(top, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if there, err := Exists(loop); !there || err != nil {
		t.Errorf("a link is there, wherever it leads: %v, %v", there, err)
	}
	if there, err := Reachable(loop); there || err == nil {
		t.Errorf("a loop of links: %v, %v", there, err)
	}
	if capacity, free, err := Space(top); err != nil || capacity <= 0 || free < 0 || free > capacity {
		t.Errorf("space: %d, %d, %v", capacity, free, err)
	}
	if _, _, err := Space(filepath.Join(top, "none")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("the space of what is not there: %v", err)
	}
}

// A file made whole or not at all: where it cannot be made, nothing is left, and a name with no folder is made in the
// current one.
func TestWriteNewWhereItCannotBeMade(t *testing.T) {
	top := t.TempDir()
	if err := WriteNew(filepath.Join(top, "none", "file"), []byte("x")); err == nil {
		t.Error("written in a folder that is not there")
	}
	for path, want := range map[string]string{"file": ".", "/file": "/", "/a/b/file": "/a/b", "a/file": "a"} {
		if got := pathDir(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	t.Chdir(top)
	if err := WriteNew("here", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(top); err != nil || len(entries) != 1 || read(t, top, "here") != "x" {
		t.Errorf("%v, %v", entries, err)
	}
}

// A file with several names holds its data once, and is counted once; what a folder below cannot say stops the count,
// which would otherwise be too small.
func TestSumSizesCountsAFileOnceAndStopsOnAFailure(t *testing.T) {
	top := t.TempDir()
	if err := os.MkdirAll(filepath.Join(top, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "a"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(top, "a"), filepath.Join(top, "sub", "again")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "sub", "b"), make([]byte, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	// A handle lists its folder once: each count has its own.
	open := func() Dir {
		dir, err := OpenDir(top)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		return dir
	}
	if got, err := SumSizes(open()); got != 15 || err != nil {
		t.Errorf("%d, %v", got, err)
	}
	for _, fault := range []string{"names ", "stat a", "opendir sub", "names sub", "stat b"} {
		faulty := Fault(open(), func(call, name string) error {
			if call+" "+name == fault {
				return syscall.EIO
			}
			return nil
		})
		if got, err := SumSizes(faulty); !errors.Is(err, syscall.EIO) || got != 0 {
			t.Errorf("%s: %d, %v", fault, got, err)
		}
	}
}

// A socket of the local API is made in a folder that is there, and nothing else at its path is replaced (specs.md
// §25.1).
func TestListenSocketWhereItCannotBe(t *testing.T) {
	top := t.TempDir()
	if _, err := ListenSocket(context.Background(), filepath.Join(top, "none", "api.sock")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a folder that is not there: %v", err)
	}
	file := filepath.Join(top, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenSocket(context.Background(), filepath.Join(file, "api.sock")); err == nil {
		t.Error("a socket below a file")
	}
	if _, err := ListenSocket(context.Background(), file); err == nil || read(t, top, "file") != "x" {
		t.Errorf("a file at the socket's path: %v", err)
	}
	// A path longer than a socket's address can hold.
	deep := filepath.Join(top, strings.Repeat("d", 120))
	if err := os.Mkdir(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if l, err := ListenSocket(context.Background(), filepath.Join(deep, "api.sock")); err == nil {
		_ = l.Close()
		t.Error("a socket at a path too long for one")
	}
	if entries, err := os.ReadDir(deep); err != nil || len(entries) != 0 {
		t.Errorf("left behind: %v, %v", entries, err)
	}
}

// The folders on the way to a secret are each given once, though the path goes through one twice.
func TestSecretFoldersGivesEachOnce(t *testing.T) {
	top := t.TempDir()
	file := filepath.Join(top, "token")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("token", filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	list, err := SecretFolders(filepath.Join(top, "link"))
	if err != nil || len(list) != 1 || list[0].Path != top {
		t.Errorf("%+v, %v", list, err)
	}
	if owner, err := StatSecretFile(filepath.Join(top, "link")); err != nil || owner.Mode.Perm() != 0o600 {
		t.Errorf("%+v, %v", owner, err)
	}
	if _, err := StatSecretFile(filepath.Join(top, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("what is not there: %v", err)
	}
}

// A mount table can always be read, and holds the root's mount.
func TestReadMountTable(t *testing.T) {
	data, err := ReadMountTable()
	if err != nil || !strings.Contains(string(data), " / / ") {
		t.Errorf("%q, %v", data, err)
	}
}

// Whether this process may write a file is asked by opening it so; nothing is written.
func TestCanReadWrite(t *testing.T) {
	top := t.TempDir()
	file := filepath.Join(top, "catalog.db")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CanReadWrite(file); err != nil {
		t.Error(err)
	}
	if err := CanReadWrite(top); !errors.Is(err, syscall.EISDIR) {
		t.Errorf("a folder: %v", err)
	}
	if err := CanReadWrite(filepath.Join(top, "none")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("what is not there: %v", err)
	}
	if read(t, top, "catalog.db") != "x" {
		t.Error("written")
	}
}

// What is not a mount is never detached, whoever asks.
func TestDetachMountOfWhatIsNoMount(t *testing.T) {
	top := t.TempDir()
	if err := DetachMount(top); err == nil {
		t.Error("a folder that is no mount was detached")
	}
	if info, err := os.Stat(top); err != nil || !info.IsDir() {
		t.Errorf("%v, %v", info, err)
	}
}

// A name that no longer leads to a block device is never read as one, and a FIFO put there never hangs the read
// (specs.md §10).
func TestABlockHeadOfWhatIsNoBlockDevice(t *testing.T) {
	top := t.TempDir()
	file := filepath.Join(top, "file")
	if err := os.WriteFile(file, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(top, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, fifo, "/dev/null"} {
		if head, err := ReadBlockHead(path, 4096); !errors.Is(err, ErrNotBlockDevice) || len(head.Data) != 0 {
			t.Errorf("%s: %d bytes, %v", path, len(head.Data), err)
		}
		// O_DIRECT is refused by some filesystems before anything is asked of the file: an error either way.
		if head, err := ReadBlockHeadDirect(path); err == nil || len(head.Data) != 0 {
			t.Errorf("%s past the cache: %d bytes, %v", path, len(head.Data), err)
		} else if !errors.Is(err, ErrNotBlockDevice) && !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%s past the cache: %v", path, err)
		}
	}
	for _, read := range []func(string) error{
		func(p string) error { _, err := ReadBlockHead(p, 4096); return err },
		func(p string) error { _, err := ReadBlockHeadDirect(p); return err },
	} {
		if err := read(filepath.Join(top, "none")); !IsDeviceGone(err) {
			t.Errorf("a device that is not there: %v", err)
		}
	}
	if _, err := StatPath(filepath.Join(top, "none")); !IsAbsent(err) {
		t.Errorf("a path that is not there: %v", err)
	}
	if _, err := CheckPowerMode(filepath.Join(top, "none"), 0); !IsDeviceGone(err) {
		t.Errorf("the power mode of a device that is not there: %v", err)
	}
}

// A disk's attributes under /sys/block, read for a disk the machine has: what the kernel says, and an error for a
// disk it does not have.
func TestSysBlockOfADisk(t *testing.T) {
	if _, err := Rotational("atoll-no-such-disk"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rotational of no disk: %v", err)
	}
	if _, err := ReadBlockStat("atoll-no-such-disk"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat of no disk: %v", err)
	}
	entries, _ := os.ReadDir("/sys/block")
	if len(entries) == 0 {
		t.Skip("no disk under /sys/block")
	}
	name := entries[0].Name()
	raw, err := os.ReadFile(filepath.Join("/sys/block", name, "queue", "rotational"))
	if err != nil {
		t.Skipf("%s: %v", name, err)
	}
	if got, err := Rotational(name); err != nil || got != (strings.TrimSpace(string(raw)) == "1") {
		t.Errorf("%s: rotational %v, %v, the kernel says %q", name, got, err, raw)
	}
	if stat, err := ReadBlockStat(name); err != nil || len(strings.Fields(stat)) < 11 {
		t.Errorf("%s: %q, %v", name, stat, err)
	}
	// A disk is its own: a whole disk under /sys/block has nothing below it but itself, or the disks of what it is
	// made of.
	dev, err := os.ReadFile(filepath.Join("/sys/block", name, "dev"))
	if err != nil {
		t.Skipf("%s: %v", name, err)
	}
	var major, minor uint32
	if _, err := fmt.Sscanf(strings.TrimSpace(string(dev)), "%d:%d", &major, &minor); err != nil {
		t.Fatal(err)
	}
	disks, err := PhysicalDisks(major, minor)
	if err != nil || len(disks) == 0 {
		t.Errorf("%s: %v, %v", name, disks, err)
	}
	if slaves, _ := os.ReadDir(filepath.Join("/sys/block", name, "slaves")); len(slaves) == 0 && !slices.Equal(disks, []string{name}) {
		t.Errorf("%s, made of nothing, lives on %v", name, disks)
	}
}

// What names no device is an error, never a disk of no name.
func TestPhysicalDisksOfWhatIsNoDevice(t *testing.T) {
	if disks, err := PhysicalDisks(0, 0); err == nil {
		t.Errorf("0:0 lives on %v", disks)
	}
	if disks, err := BtrfsDisks("00000000-0000-0000-0000-000000000000"); err == nil {
		t.Errorf("a btrfs that is not there lives on %v", disks)
	}
}

// Devices below devices that lead back to themselves end with an error, and a link below that leads nowhere is one
// too: the disks of a pool are never guessed (specs.md §10).
func TestDisksBelowThatCannotBeFollowed(t *testing.T) {
	top := sysTree(t, []string{"virtual/block/dm-0", "virtual/block/dm-1"}, nil, map[string][]string{
		"virtual/block/dm-0": {"virtual/block/dm-1"},
		"virtual/block/dm-1": {"virtual/block/dm-0"},
	})
	if got, err := disksBelow(filepath.Join(top, "virtual/block/dm-0"), 0); err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Errorf("a loop: %v, %v", got, err)
	}
	gone := sysTree(t, []string{"virtual/block/dm-2"}, nil, nil)
	if err := os.MkdirAll(filepath.Join(gone, "virtual/block/dm-2/slaves"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(gone, "virtual/block/sdz"), filepath.Join(gone, "virtual/block/dm-2/slaves/sdz")); err != nil {
		t.Fatal(err)
	}
	if got, err := disksBelow(filepath.Join(gone, "virtual/block/dm-2"), 0); err == nil {
		t.Errorf("a slave that leads nowhere: %v", got)
	}
}

// A sequence number that cannot be read is an error, never 0, which says the kernel gives none.
func TestDiskSeqThatCannotBeRead(t *testing.T) {
	top := t.TempDir()
	if err := os.Mkdir(filepath.Join(top, "diskseq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if seq, err := diskSeqIn(top); err == nil {
		t.Errorf("a diskseq that is a folder: %d", seq)
	}
	file := filepath.Join(top, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if seq, err := diskSeqIn(file); err == nil {
		t.Errorf("a device's folder that is a file: %d", seq)
	}
	if _, err := DeviceNodeOf(filepath.Join(top, strings.Repeat("n", 300))); err == nil {
		t.Error("a name too long led nowhere")
	}
}

// Whether a mount point is immutable is never guessed: a folder that is not there, or a clone this process may not
// make, is an error.
func TestMountPointImmutableOfWhatIsNotThere(t *testing.T) {
	if immutable, err := MountPointImmutable(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Errorf("a folder that is not there: %v", immutable)
	}
}
