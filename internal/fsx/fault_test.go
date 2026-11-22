package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// errInjected is what a test makes one call fail with.
var errInjected = errors.New("injected")

// faultFixture is a folder holding a file f, a folder sub and a symbolic link l to f, opened through the fault layer.
// failing names the one call that fails, as "call name", and calls lists every call the layer was asked about.
type faultFixture struct {
	t       *testing.T
	top     string
	dir     Dir
	failing string
	calls   []string
}

func newFaultFixture(t *testing.T) *faultFixture {
	t.Helper()
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(top, "l")); err != nil {
		t.Fatal(err)
	}
	real, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = real.Close() })
	x := &faultFixture{t: t, top: top}
	x.dir = Fault(real, func(call, name string) error {
		x.calls = append(x.calls, call+" "+name)
		if call+" "+name == x.failing {
			return errInjected
		}
		return nil
	})
	return x
}

// there says whether a name is in the fixture's folder, a symbolic link included.
func (x *faultFixture) there(name string) bool {
	_, err := os.Lstat(filepath.Join(x.top, name))
	return err == nil
}

func (x *faultFixture) content(name string) string {
	data, err := os.ReadFile(filepath.Join(x.top, name))
	if err != nil {
		return ""
	}
	return string(data)
}

// unchanged says that the fixture holds what it was made with, and nothing else.
func (x *faultFixture) unchanged() bool {
	x.t.Helper()
	entries, err := os.ReadDir(x.top)
	if err != nil {
		x.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	info, err := os.Stat(filepath.Join(x.top, "sub"))
	return err == nil && info.IsDir() && slices.Equal(names, []string{"f", "l", "sub"}) && x.content("f") == "abc"
}

// TestTheFaultLayerFailsOneCallOfAFolder: each call on a folder can be made to fail on its own, about the name the
// layer's documentation gives it: the error comes back as it is, nothing reaches the folder underneath, and the same
// call goes through once nothing fails. A test that breaks one step of an operation must find that step broken, and
// only it (CLAUDE.md, "Testing rules").
func TestTheFaultLayerFailsOneCallOfAFolder(t *testing.T) {
	for _, tc := range []struct {
		fault string
		do    func(d Dir) error
		// done says that the call reached the folder underneath.
		done func(x *faultFixture) bool
	}{
		{"names ", func(d Dir) error { _, err := d.Names(); return err }, nil},
		{"stat f", func(d Dir) error { _, err := d.Stat("f"); return err }, nil},
		{"readlink l", func(d Dir) error { _, err := d.ReadLink("l"); return err }, nil},
		{"opendir sub", func(d Dir) error { return closeDir(d.OpenDir("sub")) }, nil},
		{"openfile f", func(d Dir) error { return closeFile(d.OpenFile("f")) }, nil},
		{"openwrite f", func(d Dir) error { return closeWritable(d.OpenWritable("f")) }, nil},
		{"create new", func(d Dir) error { return closeWritable(d.CreateFile("new", 0o600)) }, func(x *faultFixture) bool { return x.there("new") }},
		{"mkdir new", func(d Dir) error { return d.Mkdir("new", 0o755) }, func(x *faultFixture) bool { return x.there("new") }},
		{"symlink new", func(d Dir) error { return d.Symlink("f", "new") }, func(x *faultFixture) bool { return x.there("new") }},
		{"adopt sub", func(d Dir) error { st, _ := d.Info(); return d.Adopt("sub", &st) }, nil},
		// A move or a link is about the name it makes, which is what a driver refuses.
		{"rename g", func(d Dir) error { return d.Rename("f", d, "g") }, func(x *faultFixture) bool { return x.there("g") }},
		{"plainrename g", func(d Dir) error { return d.RenamePlain("f", d, "g") }, func(x *faultFixture) bool { return x.there("g") }},
		{"link g", func(d Dir) error { return d.Link("f", d, "g") }, func(x *faultFixture) bool { return x.there("g") }},
		{"exchange sub", func(d Dir) error { return d.Exchange("f", d, "sub") }, func(x *faultFixture) bool { return x.content("sub") == "abc" }},
		{"unlink f", func(d Dir) error { return d.Unlink("f") }, func(x *faultFixture) bool { return !x.there("f") }},
		{"rmdir sub", func(d Dir) error { return d.Rmdir("sub") }, func(x *faultFixture) bool { return !x.there("sub") }},
		{"syncentry f", func(d Dir) error { return d.SyncEntry("f") }, nil},
		// A call on the folder itself is about the name it was opened by, which the top does not have.
		{"sync ", func(d Dir) error { return d.Sync() }, nil},
		{"syncfs ", func(d Dir) error { return d.SyncFS() }, nil},
		{"setmtime ", func(d Dir) error { return d.SetMtime(Time{Sec: 1_000_000_000}) }, func(x *faultFixture) bool {
			info, err := os.Stat(x.top)
			return err == nil && info.ModTime().Unix() == 1_000_000_000
		}},
		{"setmode ", func(d Dir) error { return d.SetMode(0o700) }, func(x *faultFixture) bool {
			info, err := os.Stat(x.top)
			return err == nil && info.Mode().Perm() == 0o700
		}},
		{"xattrs f", func(d Dir) error { _, err := d.Xattrs("f", UserNamespace); return err }, nil},
		{"setxattr f", func(d Dir) error { return d.SetXattr("f", "user.atoll-test", []byte("1"), 0) }, nil},
		{"removexattr f", func(d Dir) error { return d.RemoveXattr("f", "user.atoll-test") }, nil},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			x := newFaultFixture(t)
			x.failing = tc.fault
			if err := tc.do(x.dir); err != errInjected { //nolint:errorlint // it must come back as it is
				t.Fatalf("failing: %v", err)
			}
			if !x.unchanged() || (tc.done != nil && tc.done(x)) {
				t.Error("the call that failed reached the folder")
			}
			if !slices.Contains(x.calls, tc.fault) {
				t.Errorf("the layer was asked about %q, not %q", x.calls, tc.fault)
			}
			x.failing = ""
			err := tc.do(x.dir)
			switch {
			case errors.Is(err, errInjected):
				t.Errorf("nothing failing: %v", err)
			case err != nil && !IsXattrRefused(err) && !IsNoXattr(err) && !IsFlagRefused(err):
				// An attribute or an exchange the filesystem of the test refuses is the filesystem's answer, which
				// went through.
				t.Errorf("nothing failing: %v", err)
			case err == nil && tc.done != nil && !tc.done(x):
				t.Error("the call went through, and did nothing")
			}
		})
	}
}

// A folder opened through the layer is in it too, and its own calls are about its name.
func TestTheFaultLayerFollowsAFolderDown(t *testing.T) {
	x := newFaultFixture(t)
	sub, err := x.dir.OpenDir("sub")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := Descriptor(sub); err != nil {
		t.Errorf("no descriptor below the layer: %v", err)
	}
	for _, fault := range []string{"sync sub", "names sub", "setmtime sub"} {
		x.failing = fault
		var err error
		switch fault {
		case "sync sub":
			err = sub.Sync()
		case "names sub":
			_, err = sub.Names()
		default:
			err = sub.SetMtime(Time{Sec: 1})
		}
		if err != errInjected { //nolint:errorlint // it must come back as it is
			t.Errorf("%s: %v", fault, err)
		}
	}
	x.failing = ""
	if err := sub.Mkdir("deeper", 0o755); err != nil || !x.there("sub/deeper") {
		t.Errorf("a folder made below: %v", err)
	}
}

// TestTheFaultLayerFailsOneCallOfAFile: every call on a file open for reading, and on a copy in progress, can fail
// on its own, about the file's name, and changes nothing of the file; once nothing fails, it goes through.
func TestTheFaultLayerFailsOneCallOfAFile(t *testing.T) {
	type file interface {
		Info() (Stat, error)
		SetTimes(atime, mtime Time) error
		SetMode(mode uint32) error
		SetOwner(uid, gid uint32) error
		Sync() error
		DropCache() error
		Xattrs(namespace string) (map[string][]byte, error)
		SetXattr(attr string, value []byte, flags int) error
		RemoveXattr(attr string) error
		Close() error
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid()) //nolint:gosec // an account's number
	common := map[string]func(f file) error{
		"fileinfo":    func(f file) error { _, err := f.Info(); return err },
		"settimes":    func(f file) error { return f.SetTimes(OmitTime, Time{Sec: 1_000_000_000}) },
		"setmode":     func(f file) error { return f.SetMode(0o600) },
		"setowner":    func(f file) error { return f.SetOwner(uid, gid) },
		"filesync":    func(f file) error { return f.Sync() },
		"dropcache":   func(f file) error { return f.DropCache() },
		"xattrs":      func(f file) error { _, err := f.Xattrs(UserNamespace); return err },
		"setxattr":    func(f file) error { return f.SetXattr("user.atoll-test", []byte("1"), 0) },
		"removexattr": func(f file) error { return f.RemoveXattr("user.atoll-test") },
	}
	reading := map[string]func(f File) error{
		"read":   func(f File) error { _, err := f.Read(make([]byte, 3)); return err },
		"readat": func(f File) error { _, err := f.ReadAt(make([]byte, 3), 0); return err },
	}
	writing := map[string]func(f WritableFile) error{
		"write":    func(f WritableFile) error { _, err := f.Write([]byte("xyz")); return err },
		"writeat":  func(f WritableFile) error { _, err := f.WriteAt([]byte("xyz"), 0); return err },
		"readback": func(f WritableFile) error { _, err := f.ReadAt(make([]byte, 3), 0); return err },
		"truncate": func(f WritableFile) error { return f.Truncate(0) },
	}
	// call gives the name the layer is asked about for a step, which reads and writes at an offset share.
	call := func(step string) string {
		switch step {
		case "readat":
			return "read"
		case "writeat":
			return "write"
		}
		return step
	}
	check := func(t *testing.T, x *faultFixture, step string, do func() error) {
		t.Helper()
		x.failing = call(step) + " f"
		if err := do(); err != errInjected { //nolint:errorlint // it must come back as it is
			t.Fatalf("failing: %v", err)
		}
		info, err := os.Stat(filepath.Join(x.top, "f"))
		if err != nil || x.content("f") != "abc" || info.Mode().Perm() != 0o644 || info.ModTime().Unix() == 1_000_000_000 {
			t.Errorf("the call that failed reached the file: %v, %v", info, err)
		}
		x.failing = ""
		if err := do(); errors.Is(err, errInjected) || (err != nil && !IsXattrRefused(err) && !IsNoXattr(err)) {
			t.Errorf("nothing failing: %v", err)
		}
	}
	for step, do := range common {
		t.Run("reading "+step, func(t *testing.T) {
			x := newFaultFixture(t)
			f, err := x.dir.OpenFile("f")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			check(t, x, step, func() error { return do(f) })
		})
		t.Run("writing "+step, func(t *testing.T) {
			x := newFaultFixture(t)
			f, err := x.dir.OpenWritable("f")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			check(t, x, step, func() error { return do(f) })
		})
	}
	for step, do := range reading {
		t.Run("reading "+step, func(t *testing.T) {
			x := newFaultFixture(t)
			f, err := x.dir.OpenFile("f")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			check(t, x, step, func() error { return do(f) })
		})
	}
	for step, do := range writing {
		t.Run("writing "+step, func(t *testing.T) {
			x := newFaultFixture(t)
			f, err := x.dir.OpenWritable("f")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			x.failing = call(step) + " f"
			if err := do(f); err != errInjected { //nolint:errorlint // it must come back as it is
				t.Fatalf("failing: %v", err)
			}
			if x.content("f") != "abc" {
				t.Errorf("the call that failed reached the file: %q", x.content("f"))
			}
			x.failing = ""
			if err := do(f); err != nil {
				t.Errorf("nothing failing: %v", err)
			}
		})
	}
}

// A failure underneath comes back as the folder's own, and opens nothing for the layer to wrap.
func TestTheFaultLayerPassesAFailureUnderneath(t *testing.T) {
	x := newFaultFixture(t)
	if f, err := x.dir.OpenWritable("absent"); !errors.Is(err, syscall.ENOENT) || f != nil {
		t.Errorf("a copy in progress that is not there: %v", err)
	}
	if f, err := x.dir.CreateFile("f", 0o600); !IsExisting(err) || f != nil {
		t.Errorf("a copy in progress over a file: %v", err)
	}
	if f, err := x.dir.OpenFile("sub"); !errors.Is(err, ErrNotARegularFile) || f != nil {
		t.Errorf("a folder opened as a file: %v", err)
	}
	if d, err := x.dir.OpenDir("f"); !IsUnreadableName(err) || d != nil {
		t.Errorf("a file opened as a folder: %v", err)
	}
	if err := x.dir.SyncFS(); err != nil {
		t.Errorf("syncfs: %v", err)
	}
}

func closeDir(d Dir, err error) error {
	if err != nil {
		return err
	}
	return d.Close()
}

func closeFile(f File, err error) error {
	if err != nil {
		return err
	}
	return f.Close()
}

func closeWritable(f WritableFile, err error) error {
	if err != nil {
		return err
	}
	return f.Close()
}
