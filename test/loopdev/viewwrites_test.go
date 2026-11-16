//go:build loopdev

package loopdev

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/names"
)

// TestViewWrites: Atoll's own view that writes, served as atoll mount serves it at view.mount (specs.md §21.2), used as
// Sonarr and a user use a library, on a pool of ext4 and of each driver: a file made, a folder made, a rename, an
// atomic save, a file written in place, given a time and a mode, a folder renamed, a file deleted, an empty folder
// removed; a hard link refused as across two filesystems, and on NTFS a name the disk refuses. Then the passes take it
// all in: the deleted file's other copy goes to the trash, what is new is replicated, the file written makes its other
// copy stale and refreshed, and every operation of the view is taken in, the view presenting the same as before from
// the catalog alone. Then break glass: the view stopped, plain mergerfs at the same point presents the same.
func TestViewWrites(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{{"ext4", names.Ext4, ""}, {"ntfs-3g", names.NTFS, "ntfs-3g"}} {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			write(t, b.keep, "Films/A.mkv", "a film")
			write(t, b.keep, "Films/B.mkv", "b film")
			b.scan(false)
			catchUp(b)
			b.scan(false)
			line, point := fstabLine(b)
			mustDo(t, os.MkdirAll(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			in := func(path string) string { return filepath.Join(point, path) }

			mustDo(t, os.WriteFile(in("Films/New.mkv"), []byte("a new film"), 0o644))
			mustDo(t, os.MkdirAll(in("Series/S01"), 0o755))
			mustDo(t, os.WriteFile(in("Series/S01/e01.mkv"), []byte("e01"), 0o644))
			mustDo(t, os.Rename(in("Films/New.mkv"), in("Films/New (2024).mkv")))
			mustDo(t, os.WriteFile(in("Films/A.nfo"), []byte("v1"), 0o644))
			mustDo(t, os.WriteFile(in("Films/A.nfo.tmp"), []byte("v2"), 0o644))
			mustDo(t, os.Rename(in("Films/A.nfo.tmp"), in("Films/A.nfo")))
			f, err := os.OpenFile(in("Films/A.mkv"), os.O_WRONLY|os.O_APPEND, 0)
			mustDo(t, err)
			_, err = f.WriteString(", and more")
			mustDo(t, errors.Join(err, f.Close()))
			when := time.Unix(1_700_000_000, 0)
			mustDo(t, os.Chtimes(in("Films/A.mkv"), when, when))
			mustDo(t, os.Chmod(in("Films/A.mkv"), 0o640))
			mustDo(t, os.Rename(in("Series"), in("Shows")))
			mustDo(t, os.Remove(in("Films/B.mkv")))
			mustDo(t, os.Mkdir(in("Empty"), 0o755))
			mustDo(t, os.Remove(in("Empty")))
			if err := os.Link(in("Films/A.mkv"), in("Films/A-link.mkv")); !errors.Is(err, syscall.EXDEV) {
				t.Errorf("a hard link: %v, want EXDEV", err)
			}
			if tc.fs == names.NTFS {
				if err := os.Rename(in("Films/A.mkv"), in("Films/A:b.mkv")); !errors.Is(err, syscall.EINVAL) {
					t.Errorf("a name NTFS refuses: %v, want EINVAL", err)
				}
			}
			if data, err := os.ReadFile(in("Films/A.mkv")); err != nil || string(data) != "a film, and more" {
				t.Errorf("a file written in place: %q, %v", data, err)
			}
			if data, err := os.ReadFile(in("Films/A.nfo")); err != nil || string(data) != "v2" {
				t.Errorf("an atomic save: %q, %v", data, err)
			}
			if _, err := os.Stat(in("Films/B.mkv")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a file deleted: %v", err)
			}
			before := described(t, point)

			catchUp(b)
			b.scan(false)
			b.settle()
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
			for _, top := range []string{b.keep, b.at} {
				if _, err := os.Stat(filepath.Join(top, "media", "Films", "B.mkv")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s still holds the file deleted: %v", top, err)
				}
				for _, p := range []string{"Films/New (2024).mkv", "Shows/S01/e01.mkv", "Films/A.nfo"} {
					if data, err := os.ReadFile(filepath.Join(top, "media", p)); err != nil {
						t.Errorf("%s: %s not replicated: %v", top, p, err)
					} else if p == "Films/A.nfo" && string(data) != "v2" {
						t.Errorf("%s: %s holds %q", top, p, data)
					}
				}
				if data, err := os.ReadFile(filepath.Join(top, "media", "Films", "A.mkv")); err != nil || string(data) != "a film, and more" {
					t.Errorf("%s: the copy of the file written: %q, %v", top, data, err)
				}
			}
			if got := b.text("SELECT count(*) FROM trash WHERE reason = 'deleted' AND CAST(lpath AS TEXT) = 'Films/B.mkv'"); got != "2" {
				t.Errorf("copies of the file deleted in the trash: %s, want 2", got)
			}
			time.Sleep(1100 * time.Millisecond) // past the kernel's caches of the view
			if after := described(t, point); !maps.Equal(after, before) {
				t.Errorf("once taken in, the view presents\n%v\nwhere it presented\n%v", after, before)
			}

			served.stop()
			mustDo(t, served.wait())
			mountView(t, line, point)
			if merged := described(t, point); !maps.Equal(merged, before) {
				t.Errorf("break glass: mergerfs presents\n%v\nwhere the view presented\n%v", merged, before)
			}
		})
	}
}
