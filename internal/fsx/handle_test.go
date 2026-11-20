package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// keepsItsCause says that an error still holds what the kernel or the os package answered, so that IsDiskLost and
// the others can tell it however it was wrapped (invariant 4): an error that lost it would read as nothing known. A
// listing of a closed folder answers with the os package's own error of a path, whose cause it does not export.
func keepsItsCause(err error) bool {
	var errno syscall.Errno
	var path *fs.PathError
	return errors.As(err, &errno) || errors.Is(err, os.ErrClosed) || errors.As(err, &path)
}

// TestAClosedFolderAnswersEveryCallWithItsCause: a folder's handle closed under its caller answers every call with an
// error that keeps its cause, never with a result: an empty listing, or a sync that says nothing, would read as a
// folder emptied or a move made durable (invariant 4).
func TestAClosedFolderAnswersEveryCallWithItsCause(t *testing.T) {
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	like := Stat{Mode: 0o755, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())} //nolint:gosec // an account's number
	for name, call := range map[string]func() error{
		"names":        func() error { _, err := dir.Names(); return err },
		"info":         func() error { _, err := dir.Info(); return err },
		"stat":         func() error { _, err := dir.Stat("f"); return err },
		"opendir":      func() error { return closeDir(dir.OpenDir("sub")) },
		"openfile":     func() error { return closeFile(dir.OpenFile("f")) },
		"readlink":     func() error { _, err := dir.ReadLink("f"); return err },
		"mkdir":        func() error { return dir.Mkdir("new", 0o755) },
		"adopt":        func() error { return dir.Adopt("sub", &like) },
		"rename":       func() error { return dir.Rename("f", other, "g") },
		"rename plain": func() error { return dir.RenamePlain("f", other, "g") },
		"exchange":     func() error { return dir.Exchange("f", other, "sub") },
		"link":         func() error { return dir.Link("f", other, "g") },
		"unlink":       func() error { return dir.Unlink("f") },
		"rmdir":        func() error { return dir.Rmdir("sub") },
		"sync":         func() error { return dir.Sync() },
		"syncfs":       func() error { return dir.SyncFS() },
		"syncentry":    func() error { return dir.SyncEntry("f") },
		"create":       func() error { return closeWritable(dir.CreateFile("new", 0o600)) },
		"openwrite":    func() error { return closeWritable(dir.OpenWritable("f")) },
		"setmtime":     func() error { return dir.SetMtime(Time{Sec: 1}) },
		"setmode":      func() error { return dir.SetMode(0o700) },
		"setowner":     func() error { return dir.SetOwner(like.Uid, like.Gid) },
		"symlink":      func() error { return dir.Symlink("f", "new") },
		"lchown":       func() error { return dir.Lchown("f", like.Uid, like.Gid) },
		"xattrs":       func() error { _, err := dir.Xattrs("f", UserNamespace); return err },
		"setxattr":     func() error { return dir.SetXattr("f", "user.atoll-test", []byte("1"), 0) },
		"removexattr":  func() error { return dir.RemoveXattr("f", "user.atoll-test") },
		"close":        func() error { return dir.Close() },
	} {
		if err := call(); err == nil || !keepsItsCause(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Nothing the closed handle was asked reached the folder.
	entries, err := os.ReadDir(top)
	if err != nil || len(entries) != 2 {
		t.Errorf("the folder holds %v, %v", entries, err)
	}
}

// TestAClosedFileAnswersEveryCallWithItsCause: so does a file open for reading, and a copy in progress.
func TestAClosedFileAnswersEveryCallWithItsCause(t *testing.T) {
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	r, err := dir.OpenFile("f")
	if err != nil {
		t.Fatal(err)
	}
	w, err := dir.OpenWritable("f")
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(r.Close(), w.Close()); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid()) //nolint:gosec // an account's number
	calls := map[string]func() error{
		"read close":        func() error { return r.Close() },
		"read info":         func() error { _, err := r.Info(); return err },
		"read read":         func() error { _, err := r.Read(make([]byte, 1)); return err },
		"read readat":       func() error { _, err := r.ReadAt(make([]byte, 1), 0); return err },
		"read settimes":     func() error { return r.SetTimes(OmitTime, Time{Sec: 1}) },
		"read setmode":      func() error { return r.SetMode(0o600) },
		"read setowner":     func() error { return r.SetOwner(uid, gid) },
		"read sync":         func() error { return r.Sync() },
		"read dropcache":    func() error { return r.DropCache() },
		"read xattrs":       func() error { _, err := r.Xattrs(UserNamespace); return err },
		"read setxattr":     func() error { return r.SetXattr("user.atoll-test", []byte("1"), 0) },
		"read removexattr":  func() error { return r.RemoveXattr("user.atoll-test") },
		"write close":       func() error { return w.Close() },
		"write info":        func() error { _, err := w.Info(); return err },
		"write write":       func() error { _, err := w.Write([]byte("x")); return err },
		"write writeat":     func() error { _, err := w.WriteAt([]byte("x"), 0); return err },
		"write readat":      func() error { _, err := w.ReadAt(make([]byte, 1), 0); return err },
		"write truncate":    func() error { return w.Truncate(0) },
		"write sync":        func() error { return w.Sync() },
		"write settimes":    func() error { return w.SetTimes(OmitTime, Time{Sec: 1}) },
		"write setmode":     func() error { return w.SetMode(0o600) },
		"write setowner":    func() error { return w.SetOwner(uid, gid) },
		"write dropcache":   func() error { return w.DropCache() },
		"write xattrs":      func() error { _, err := w.Xattrs(UserNamespace); return err },
		"write setxattr":    func() error { return w.SetXattr("user.atoll-test", []byte("1"), 0) },
		"write removexattr": func() error { return w.RemoveXattr("user.atoll-test") },
	}
	for name, call := range calls {
		if err := call(); err == nil || !keepsItsCause(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(top, "f"))
	if err != nil || info.Size() != 3 || info.Mode().Perm() != 0o644 || info.ModTime().Unix() == 1 {
		t.Errorf("a closed handle changed the file: %v, %v", info, err)
	}
}

// notADisk is a folder that is none of a disk's: a test double that wraps nothing real.
type notADisk struct{ Dir }

// A move, an exchange or a link takes two folders of one disk, and the kernel only real ones: a folder that is not
// one is refused before anything moves, and has no descriptor. One below any wrapping is found.
func TestOnlyAFolderOfADiskTakesAMove(t *testing.T) {
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	stranger := notADisk{}
	for name, call := range map[string]func() error{
		"rename":       func() error { return dir.Rename("f", stranger, "g") },
		"rename plain": func() error { return dir.RenamePlain("f", stranger, "g") },
		"exchange":     func() error { return dir.Exchange("f", stranger, "g") },
		"link":         func() error { return dir.Link("f", stranger, "g") },
		"descriptor":   func() error { _, err := Descriptor(stranger); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not a folder of a disk") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if entries, err := os.ReadDir(top); err != nil || len(entries) != 1 || entries[0].Name() != "f" {
		t.Errorf("something moved: %v, %v", entries, err)
	}
	wrapped := Fault(Fault(dir, func(string, string) error { return nil }), func(string, string) error { return nil })
	fd, err := Descriptor(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Descriptor(dir)
	if err != nil || fd != want {
		t.Errorf("the descriptor below two layers: %d, %d, %v", fd, want, err)
	}
	if err := dir.Rename("f", wrapped, "g"); err != nil {
		t.Errorf("a move to a wrapped folder: %v", err)
	}
}

// TestNamesAsTheKernelAnswersThem: what each call says of a name that is not what it asks for, as the callers tell
// it apart (specs.md §15.10, §15.12, §17.1).
func TestNamesAsTheKernelAnswersThem(t *testing.T) {
	top := t.TempDir()
	for name, text := range map[string]string{"f": "abc", "g": "def", "full/x": "y"} {
		full := filepath.Join(top, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, folder := range []string{"empty", "empty2"} {
		if err := os.Mkdir(filepath.Join(top, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if _, err := dir.ReadLink("absent"); !IsUnreadableName(err) || errors.Is(err, ErrNotALink) {
		t.Errorf("a link that is not there: %v", err)
	}
	if err := dir.Symlink("f", "g"); !IsExisting(err) {
		t.Errorf("a link over a file: %v", err)
	}
	if err := dir.Lchown("absent", uint32(os.Getuid()), uint32(os.Getgid())); !IsUnreadableName(err) { //nolint:gosec // an account's number
		t.Errorf("the owner of a name that is not there: %v", err)
	}
	if err := dir.Link("f", dir, "g"); !IsExisting(err) {
		t.Errorf("a link over a file: %v", err)
	}
	if err := dir.Rename("f", dir, "g"); !IsExisting(err) {
		t.Errorf("a move over a file: %v", err)
	}
	if err := dir.Unlink("absent"); !IsAbsent(err) {
		t.Errorf("a name that is not there: %v", err)
	}
	if err := dir.Rmdir("full"); !IsNotEmpty(err) {
		t.Errorf("a folder that holds something: %v", err)
	}
	if err := dir.Mkdir("f", 0o755); !IsExisting(err) {
		t.Errorf("a folder over a file: %v", err)
	}
	// A plain rename replaces an empty folder at the target, and never one that holds something (specs.md §15.12).
	if err := dir.RenamePlain("empty", dir, "full"); !IsNotEmpty(err) && !IsExisting(err) {
		t.Errorf("a folder over one that holds something: %v", err)
	}
	if err := dir.RenamePlain("empty", dir, "empty2"); err != nil {
		t.Errorf("a folder over an empty one: %v", err)
	}
	if _, err := os.Stat(filepath.Join(top, "empty")); !os.IsNotExist(err) {
		t.Errorf("the folder moved stays: %v", err)
	}
	if err := dir.RenamePlain("absent", dir, "h"); !IsAbsent(err) {
		t.Errorf("a name that is not there: %v", err)
	}
	// An exchange swaps two names at once, and neither is ever free; it needs both.
	switch err := dir.Exchange("f", dir, "g"); {
	case IsFlagRefused(err):
		t.Logf("this filesystem has no exchange: %v", err)
	case err != nil:
		t.Errorf("exchange: %v", err)
	default:
		if a, b := read(t, top, "f"), read(t, top, "g"); a != "def" || b != "abc" {
			t.Errorf("after the exchange, f holds %q and g %q", a, b)
		}
	}
	if err := dir.Exchange("f", dir, "absent"); !IsAbsent(err) && !IsFlagRefused(err) {
		t.Errorf("an exchange with a name that is not there: %v", err)
	}
	// A path through a folder that is not there leads nowhere, and so does one through a file.
	for _, path := range []string{"none/f", "f/x"} {
		if f, err := OpenFileAt(dir, path); !IsUnreadableName(err) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("%s: %v", path, err)
		}
	}
	f, err := OpenFileAt(dir, "full/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// The owner of a folder is set through its handle, as the view does on a user's behalf (specs.md §21.2).
func TestAFolderTakesItsOwnerThroughItsHandle(t *testing.T) {
	dir, err := OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid()) //nolint:gosec // an account's number
	if err := dir.SetOwner(uid, gid); err != nil {
		t.Fatal(err)
	}
	if st, err := dir.Info(); err != nil || st.Uid != uid || st.Gid != gid || st.Kind != KindDir {
		t.Errorf("%+v, %v", st, err)
	}
}

// A filesystem that keeps no creation time, and no attribute that holds one, gives none: neither a look by name nor
// one through a handle invents it, and neither fails (specs.md §7). /proc keeps none on any kernel; /proc/self is a
// link, which a folder is never opened through.
func TestNoCreationTimeIsNone(t *testing.T) {
	dir, err := OpenDir("/proc/" + strconv.Itoa(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Stat("status")
	if err != nil || st.HasBirth || st.Kind != KindFile {
		t.Errorf("by its name: %+v, %v", st, err)
	}
	f, err := dir.OpenFile("status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Info(); err != nil || st.HasBirth || st.Kind != KindFile {
		t.Errorf("through a handle: %+v, %v", st, err)
	}
	if st, err := dir.Info(); err != nil || st.HasBirth || st.Kind != KindDir {
		t.Errorf("the folder itself: %+v, %v", st, err)
	}
}

func read(t *testing.T, top, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(top, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
