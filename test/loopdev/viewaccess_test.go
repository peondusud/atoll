//go:build loopdev

package loopdev

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/names"
)

// The accounts of TestViewAsAnotherAccount: another asks, owner owns some of the library, root the rest.
const (
	another = 991
	owner   = 992
)

// anotherEnv says what TestViewAsAnotherChild does, as the account setpriv made it: an operation and its arguments,
// one a line.
const anotherEnv = "ATOLL_AS_ANOTHER"

// TestViewAsAnotherAccount: Atoll's own view that writes, served as atoll mount serves it at view.mount, used by an
// account that owns none of what it asks of, through a real mount, as a member of the household, or a program of its
// own account, writes the library (specs.md §21.2). The kernel refuses it, from what the view presents
// (default_permissions), what it would refuse it on any filesystem: a name made in a folder it may not write, a file
// of another removed from a sticky folder, a chmod and a chown of another's file, a setuid bit it sets on one, a hard
// link that fs.protected_hardlinks refuses; and a file of another it writes loses its setuid bit, on its disk too. The
// view refuses it what a disk holds stricter than the catalog, before any pass: a file made 0600 on its disk, opened
// to write, and a name made in a folder made 0755 on its disks. A folder one disk holds 0755 and the other 0700, as a
// walk recorded them, presents 0700, so the kernel refuses it the listing the stricter disk refuses (G11). Nothing it
// was refused is journaled or changed.
func TestViewAsAnotherAccount(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skip("no setpriv")
	}
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
	for _, f := range []struct {
		path string
		mode os.FileMode
		uid  int
	}{
		{"Closed/A.mkv", 0o644, 0},
		{"Shared/Theirs.mkv", 0o644, owner},
		{"Open/Theirs.mkv", 0o644, owner},
		{"Open/Secret.mkv", 0o600, owner},
		{"Open/Setuid.mkv", 0o666 | os.ModeSetuid, owner},
		{"Open/Later.mkv", 0o666, owner},
		{"Later/B.mkv", 0o644, owner},
		{"Split/C.mkv", 0o644, 0},
	} {
		write(t, b.keep, f.path, "of "+strconv.Itoa(f.uid))
		full := filepath.Join(b.keep, "media", f.path)
		mustDo(t, os.Chown(full, f.uid, f.uid))
		mustDo(t, os.Chmod(full, f.mode))
	}
	for folder, mode := range map[string]os.FileMode{"Closed": 0o755, "Shared": 0o777 | os.ModeSticky, "Open": 0o777, "Later": 0o777} {
		mustDo(t, os.Chmod(filepath.Join(b.keep, "media", folder), mode))
	}
	b.scan(false)
	catchUp(b)
	// A folder another disk holds stricter, as a pass records it: the view presents the strictest of the two (G11).
	mustDo(t, os.Chmod(filepath.Join(b.at, "media", "Split"), 0o700))
	b.scan(false)
	// Linux's default is off; systemd sets it on, as Debian and the others do.
	hardlinks, err := os.ReadFile("/proc/sys/fs/protected_hardlinks")
	mustDo(t, err)
	mustDo(t, os.WriteFile("/proc/sys/fs/protected_hardlinks", []byte("1"), 0o644))
	t.Cleanup(func() { _ = os.WriteFile("/proc/sys/fs/protected_hardlinks", hardlinks, 0o644) })
	// Where another account may reach it: a test's own folders are root's alone.
	point, err := os.MkdirTemp("", "atoll-view-")
	mustDo(t, err)
	mustDo(t, os.Chmod(point, 0o755))
	t.Cleanup(func() { _ = os.Remove(point) })
	served, err := serveView(t, b, point, true)
	mustDo(t, err)
	in := func(path string) string { return filepath.Join(point, path) }

	for _, tc := range []struct {
		name string
		asks []string
		want syscall.Errno
	}{
		{"a name made in a folder 0755 of root", []string{"create", in("Closed/new.mkv")}, syscall.EACCES},
		{"a file of another removed from a sticky folder", []string{"unlink", in("Shared/Theirs.mkv")}, syscall.EPERM},
		{"a chmod of another's file", []string{"chmod", in("Open/Theirs.mkv"), "0666"}, syscall.EPERM},
		{"a chown of another's file", []string{"chown", in("Open/Theirs.mkv"), strconv.Itoa(another), strconv.Itoa(another)}, syscall.EPERM},
		{"a setuid bit set on another's file", []string{"chmod", in("Open/Theirs.mkv"), "04755"}, syscall.EPERM},
		{"a hard link of a file it may neither read nor write", []string{"link", in("Open/Secret.mkv"), in("Open/Secret-link.mkv")}, syscall.EPERM},
		{"a name made in a folder it may write", []string{"create", in("Open/Mine.mkv")}, 0},
		{"a file of another it may write, setuid", []string{"write", in("Open/Setuid.mkv")}, 0},
		{"a folder 0755 of root on k and 0700 on v, listed", []string{"list", in("Split")}, syscall.EACCES},
		{"a folder 0755 of root on both disks, listed", []string{"list", in("Closed")}, 0},
	} {
		if got := asAnother(t, tc.asks...); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if info, err := os.Stat(in("Split")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the folder 0755 on k and 0700 on v presents %v, %v: not the stricter", info, err)
	}
	made := 0
	for _, top := range []string{b.keep, b.at} {
		if info, err := os.Stat(filepath.Join(top, "media", "Open", "Mine.mkv")); err == nil {
			made++
			if info.Sys().(*syscall.Stat_t).Uid != another {
				t.Errorf("the file it made, on %s: owned by %d", top, info.Sys().(*syscall.Stat_t).Uid)
			}
		}
	}
	if made != 1 {
		t.Errorf("the file it made is on %d disks, want 1", made)
	}
	// The journal says who made it, as the kernel named the caller of the request: its account, and a thread (G15).
	if got := b.text("SELECT COALESCE(uid, -1) || ' ' || (pid IS NOT NULL AND pid > 0) FROM view_ops WHERE kind = 'create' AND CAST(lpath AS TEXT) = 'Open/Mine.mkv'"); got != strconv.Itoa(another)+" 1" {
		t.Errorf("who made it, as the journal says: %q, want the account %d and a thread", got, another)
	}
	// The kernel takes setuid off a file a caller without CAP_FSETID writes: through the view, on its disk too, where
	// the view writes with CAP_FSETID.
	for _, p := range []string{in("Open/Setuid.mkv"), filepath.Join(b.keep, "media", "Open", "Setuid.mkv")} {
		if info, err := os.Stat(p); err != nil || info.Mode()&os.ModeSetuid != 0 {
			t.Errorf("%s after another wrote it: %v, %v", p, info, err)
		}
	}

	// A chmod made directly on the disks, which no pass records: the file's original is on k.
	mustDo(t, os.Chmod(filepath.Join(b.keep, "media", "Open", "Later.mkv"), 0o600))
	for _, top := range []string{b.keep, b.at} {
		mustDo(t, os.Chmod(filepath.Join(top, "media", "Later"), 0o755))
	}
	for _, tc := range []struct {
		name string
		asks []string
	}{
		{"a file 0600 on its disk, opened to write", []string{"write", in("Open/Later.mkv")}},
		{"a name made in a folder 0755 on its disks", []string{"create", in("Later/new.mkv")}},
	} {
		if got := asAnother(t, tc.asks...); got != syscall.EACCES {
			t.Errorf("%s: %v, want EACCES", tc.name, got)
		}
	}

	// Nothing it was refused happened, on a disk or in the journal.
	for _, top := range []string{b.keep, b.at} {
		for _, p := range []string{"Closed/new.mkv", "Open/Secret-link.mkv", "Later/new.mkv"} {
			if _, err := os.Lstat(filepath.Join(top, "media", p)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s holds %s: %v", top, p, err)
			}
		}
		info, err := os.Stat(filepath.Join(top, "media", "Open", "Theirs.mkv"))
		if err != nil || info.Mode() != 0o644 || info.Sys().(*syscall.Stat_t).Uid != owner {
			t.Errorf("%s: another's file: %v, %v", top, info, err)
		}
		if data, err := os.ReadFile(filepath.Join(top, "media", "Shared", "Theirs.mkv")); err != nil || string(data) != "of 992" {
			t.Errorf("%s: another's file in the sticky folder: %q, %v", top, data, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(b.keep, "media", "Open", "Later.mkv")); err != nil || string(data) != "of 992" {
		t.Errorf("the file 0600 on its disk: %q, %v", data, err)
	}
	open, err := catalog.OpenViewOps(t.Context(), b.db)
	mustDo(t, err)
	for _, o := range open {
		if o.Path != "Open/Mine.mkv" && o.Path != "Open/Setuid.mkv" {
			t.Errorf("journaled: %+v", o)
		}
	}
	served.stop()
	mustDo(t, served.wait())
}

// errnoLine is what TestViewAsAnotherChild says.
var errnoLine = regexp.MustCompile(`(?m)^errno=(\d+)$`)

// asAnother does an operation as the account another, with no other group and no capability, through
// TestViewAsAnotherChild run by setpriv, and gives the error it met, 0 for none.
func asAnother(t *testing.T, asks ...string) syscall.Errno {
	t.Helper()
	id := strconv.Itoa(another)
	cmd := exec.CommandContext(t.Context(), "setpriv", "--reuid="+id, "--regid="+id, "--clear-groups", "--no-new-privs", "--",
		os.Args[0], "-test.run=^TestViewAsAnotherChild$")
	cmd.Env = append(os.Environ(), anotherEnv+"="+strings.Join(asks, "\n"))
	out, err := cmd.CombinedOutput()
	m := errnoLine.FindSubmatch(out)
	if err != nil || m == nil {
		t.Fatalf("%v as another account: %v\n%s", asks, err, out)
	}
	n, err := strconv.Atoi(string(m[1]))
	mustDo(t, err)
	return syscall.Errno(n)
}

// TestViewAsAnotherChild does what asAnother asks, as the account setpriv made it, and says the error it met; it is
// skipped otherwise.
func TestViewAsAnotherChild(t *testing.T) {
	asked := os.Getenv(anotherEnv)
	if asked == "" {
		t.Skip("run by asAnother")
	}
	args := strings.Split(asked, "\n")
	var err error
	switch args[0] {
	case "create":
		var fd int
		if fd, err = unix.Open(args[1], unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0o644); err == nil {
			err = unix.Close(fd)
		}
	case "write":
		var fd int
		if fd, err = unix.Open(args[1], unix.O_WRONLY|unix.O_APPEND|unix.O_CLOEXEC, 0); err == nil {
			_, err = unix.Write(fd, []byte(", and more"))
			err = errors.Join(err, unix.Close(fd))
		}
	case "list":
		var fd int
		if fd, err = unix.Open(args[1], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); err == nil {
			_, err = unix.Getdents(fd, make([]byte, 4096))
			err = errors.Join(err, unix.Close(fd))
		}
	case "unlink":
		err = unix.Unlink(args[1])
	case "chmod":
		var mode uint64
		if mode, err = strconv.ParseUint(args[2], 8, 32); err == nil {
			err = unix.Chmod(args[1], uint32(mode))
		}
	case "chown":
		uid, uerr := strconv.Atoi(args[2])
		gid, gerr := strconv.Atoi(args[3])
		if err = errors.Join(uerr, gerr); err == nil {
			err = unix.Chown(args[1], uid, gid)
		}
	case "link":
		err = unix.Link(args[1], args[2])
	default:
		t.Fatalf("no operation %q", args[0])
	}
	var errno syscall.Errno
	if err != nil && !errors.As(err, &errno) {
		t.Fatalf("%v: %v", args, err)
	}
	fmt.Printf("errno=%d\n", int(errno))
}
