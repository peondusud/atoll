//go:build loopdev

package loopdev

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/names"
)

// userXattrsOf gives the user. extended attributes of a path, through whatever mount it is on.
func userXattrsOf(t *testing.T, path string) map[string]string {
	t.Helper()
	size, err := syscall.Listxattr(path, nil)
	mustDo(t, err)
	list := make([]byte, size)
	size, err = syscall.Listxattr(path, list)
	mustDo(t, err)
	out := map[string]string{}
	for _, name := range strings.Split(string(list[:size]), "\x00") {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		value := make([]byte, 4096)
		n, err := syscall.Getxattr(path, name, value)
		mustDo(t, err)
		out[name] = string(value[:n])
	}
	return out
}

// TestViewXattrs: extended attributes through Atoll's own view that writes, served as atoll mount serves it, on a pool
// of ext4 and of each driver (specs.md §21.2), as Samba and macOS through Samba write them: one of the user. namespace
// set, read back, listed, and removed through the view, on the original's disk alone at first; of any other namespace
// none is held, and none can be set. Then the passes give the other copy the same set, nothing copied again and no
// copy made stale, and take the view's operations in. A view read only reads them, and sets none.
func TestViewXattrs(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{{"ext4", names.Ext4, ""}, {"ntfs-3g", names.NTFS, "ntfs-3g"}, {"ntfs3", names.NTFS, "ntfs3"}} {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			write(t, b.keep, "Films/A.mkv", "a film")
			b.scan(false)
			catchUp(b)
			b.scan(false)
			point := filepath.Join(t.TempDir(), "pool")
			mustDo(t, os.MkdirAll(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			in := filepath.Join(point, "Films", "A.mkv")

			mustDo(t, syscall.Setxattr(in, "user.DOSATTRIB", []byte("0x20"), 0))
			mustDo(t, syscall.Setxattr(in, "user.org.netatalk.Metadata", []byte("meta"), 0))
			mustDo(t, syscall.Setxattr(in, "user.gone", []byte("x"), 0))
			mustDo(t, syscall.Removexattr(in, "user.gone"))
			if got := userXattrsOf(t, in); len(got) != 2 || got["user.DOSATTRIB"] != "0x20" || got["user.org.netatalk.Metadata"] != "meta" {
				t.Errorf("through the view: %v", got)
			}
			if err := syscall.Setxattr(in, "user.DOSATTRIB", []byte("0x21"), 1); !errors.Is(err, syscall.EEXIST) {
				t.Errorf("created twice: %v, want EEXIST", err)
			}
			if err := syscall.Setxattr(in, "trusted.atoll", []byte("x"), 0); !errors.Is(err, syscall.ENOTSUP) {
				t.Errorf("another namespace: %v, want ENOTSUP", err)
			}
			if _, err := syscall.Getxattr(in, "security.selinux", make([]byte, 64)); !errors.Is(err, syscall.ENODATA) {
				t.Errorf("another namespace read: %v, want ENODATA", err)
			}
			if got := userXattrsOf(t, filepath.Join(b.keep, "media", "Films", "A.mkv")); got["user.DOSATTRIB"] != "0x20" {
				t.Errorf("the original holds %v", got)
			}
			if got := userXattrsOf(t, filepath.Join(b.at, "media", "Films", "A.mkv")); len(got) != 0 {
				t.Errorf("the other copy took it at once: %v", got)
			}

			b.settle()
			b.settle()
			if got := userXattrsOf(t, filepath.Join(b.at, "media", "Films", "A.mkv")); len(got) != 2 || got["user.DOSATTRIB"] != "0x20" {
				t.Errorf("the other copy after the passes: %v", got)
			}
			if got := b.text("SELECT group_concat(state, ' ') FROM copies"); got != "ok ok" {
				t.Errorf("copies %s", got)
			}
			if got := b.text("SELECT count(*) FROM jobs"); got != "0" {
				t.Errorf("%s jobs: something is copied again", got)
			}
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
			served.stop()
			mustDo(t, served.wait())

			ro, err := serveView(t, b, point, false)
			mustDo(t, err)
			if got := userXattrsOf(t, in); got["user.DOSATTRIB"] != "0x20" {
				t.Errorf("read only: %v", got)
			}
			if err := syscall.Setxattr(in, "user.DOSATTRIB", []byte("0x22"), 0); !errors.Is(err, syscall.EROFS) {
				t.Errorf("set through a view read only: %v, want EROFS", err)
			}
			ro.stop()
			mustDo(t, ro.wait())
		})
	}
}
