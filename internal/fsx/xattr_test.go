package fsx

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// attrs is a file's extended attributes as listxattr and getxattr give them: each call fills a buffer, or says how
// large one must be when it is empty. grow makes a value or the listing larger between those two calls, as another
// process writing it does, the number of times given.
type attrs struct {
	values  map[string]string
	order   []string
	grow    int
	listErr error
	getErr  map[string]error
}

func (a *attrs) list(buf []byte) (int, error) {
	if a.listErr != nil {
		return 0, a.listErr
	}
	var raw strings.Builder
	for _, name := range a.order {
		raw.WriteString(name + "\x00")
	}
	return fill(raw.String(), buf, &a.grow)
}

func (a *attrs) get(name string, buf []byte) (int, error) {
	if err := a.getErr[name]; err != nil {
		return 0, err
	}
	value, ok := a.values[name]
	if !ok {
		return 0, unix.ENODATA
	}
	return fill(value, buf, &a.grow)
}

// fill answers as the system calls do: the size for an empty buffer, ERANGE for one too small, which a value that
// grew since its size was asked gives.
func fill(text string, buf []byte, grow *int) (int, error) {
	if len(buf) == 0 {
		return len(text), nil
	}
	if *grow > 0 {
		*grow--
		return 0, unix.ERANGE
	}
	if len(buf) < len(text) {
		return 0, unix.ERANGE
	}
	return copy(buf, text), nil
}

// TestReadXattrs: the attributes of one namespace, each with its value; none on a filesystem that holds none; one
// gone since it was listed is left out; one that grew while it was read is read again, a few times and no more; and
// any other failure is an error, never an empty set, which a pass would give a copy as the source's (specs.md §14,
// §21.2).
func TestReadXattrs(t *testing.T) {
	a := &attrs{
		values: map[string]string{"user.DOSATTRIB": "0x20", "user.empty": "", "security.selinux": "label"},
		order:  []string{"security.selinux", "user.DOSATTRIB", "user.empty", "user.gone"},
	}
	got, err := readXattrs(a.list, a.get, UserNamespace)
	want := map[string][]byte{"user.DOSATTRIB": []byte("0x20"), "user.empty": {}}
	if err != nil || !maps.EqualFunc(got, want, func(x, y []byte) bool { return string(x) == string(y) }) {
		t.Errorf("%q, %v", got, err)
	}
	a.grow = xattrTries - 1
	if got, err := readXattrs(a.list, a.get, UserNamespace); err != nil || len(got) != 2 {
		t.Errorf("grown while read: %q, %v", got, err)
	}
	a.grow = xattrTries
	if got, err := readXattrs(a.list, a.get, UserNamespace); !errors.Is(err, unix.ERANGE) || got != nil {
		t.Errorf("grown each time: %q, %v", got, err)
	}
	a.grow = 0
	for _, refused := range []error{unix.ENOTSUP, unix.EOPNOTSUPP} {
		a.listErr = refused
		if got, err := readXattrs(a.list, a.get, UserNamespace); err != nil || got == nil || len(got) != 0 {
			t.Errorf("%v: %q, %v", refused, got, err)
		}
	}
	a.listErr = unix.EIO
	if got, err := readXattrs(a.list, a.get, UserNamespace); !IsDiskLost(err) || got != nil {
		t.Errorf("a listing that fails: %q, %v", got, err)
	}
	a.listErr = nil
	a.getErr = map[string]error{"user.DOSATTRIB": unix.EIO}
	if got, err := readXattrs(a.list, a.get, UserNamespace); !IsDiskLost(err) || got != nil || !strings.Contains(err.Error(), "user.DOSATTRIB") {
		t.Errorf("a value that fails: %q, %v", got, err)
	}
	a.getErr = map[string]error{"user.DOSATTRIB": unix.ENODATA}
	if got, err := readXattrs(a.list, a.get, UserNamespace); err != nil || len(got) != 1 {
		t.Errorf("a value gone since listed: %q, %v", got, err)
	}
	empty := &attrs{}
	if got, err := readXattrs(empty.list, empty.get, UserNamespace); err != nil || got == nil || len(got) != 0 {
		t.Errorf("no attribute: %q, %v", got, err)
	}
	// A size that cannot be asked is the call's error, as it is.
	if _, err := grown(func([]byte) (int, error) { return 0, unix.EIO }); !errors.Is(err, unix.EIO) {
		t.Errorf("a size not given: %v", err)
	}
	// And so is a read that fails once the size was given.
	if _, err := grown(func(buf []byte) (int, error) {
		if len(buf) == 0 {
			return 4, nil
		}
		return 0, unix.EIO
	}); !errors.Is(err, unix.EIO) {
		t.Errorf("a value not read: %v", err)
	}
}

// The attributes of a real file, by its name in a folder and through each kind of handle: set, read, replaced and
// removed, the namespace kept, and what is not there an error that says so.
func TestXattrsOfAFile(t *testing.T) {
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.SetXattr("f", "user.a", []byte("1"), XattrCreate); err != nil {
		if IsXattrRefused(err) {
			t.Skipf("this filesystem holds no user attribute: %v", err)
		}
		t.Fatal(err)
	}
	if err := dir.SetXattr("f", "user.a", []byte("2"), XattrCreate); !IsExisting(err) {
		t.Errorf("created over one that is there: %v", err)
	}
	if err := dir.SetXattr("f", "user.b", []byte("2"), XattrReplace); !IsNoXattr(err) {
		t.Errorf("replaced where there is none: %v", err)
	}
	if got, err := dir.Xattrs("f", UserNamespace); err != nil || len(got) != 1 || string(got["user.a"]) != "1" {
		t.Errorf("by name: %q, %v", got, err)
	}
	if _, err := dir.Xattrs("absent", UserNamespace); !IsUnreadableName(err) {
		t.Errorf("a name that is not there: %v", err)
	}
	r, err := dir.OpenFile("f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := r.SetXattr("user.b", []byte("from a reader"), 0); err != nil {
		t.Fatal(err)
	}
	w, err := dir.OpenWritable("f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.SetXattr("user.c", []byte("from a writer"), 0); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (map[string][]byte, error){
		func() (map[string][]byte, error) { return r.Xattrs(UserNamespace) },
		func() (map[string][]byte, error) { return w.Xattrs(UserNamespace) },
		func() (map[string][]byte, error) { return dir.Xattrs("f", UserNamespace) },
	} {
		if got, err := read(); err != nil || len(got) != 3 || string(got["user.b"]) != "from a reader" || string(got["user.c"]) != "from a writer" {
			t.Errorf("%q, %v", got, err)
		}
	}
	if err := r.RemoveXattr("user.a"); err != nil {
		t.Error(err)
	}
	if err := w.RemoveXattr("user.b"); err != nil {
		t.Error(err)
	}
	if err := dir.RemoveXattr("f", "user.c"); err != nil {
		t.Error(err)
	}
	for _, remove := range []func() error{
		func() error { return r.RemoveXattr("user.a") },
		func() error { return w.RemoveXattr("user.a") },
		func() error { return dir.RemoveXattr("f", "user.a") },
	} {
		if err := remove(); !IsNoXattr(err) {
			t.Errorf("removed twice: %v", err)
		}
	}
	if got, err := w.Xattrs(UserNamespace); err != nil || len(got) != 0 {
		t.Errorf("all removed: %q, %v", got, err)
	}
}
