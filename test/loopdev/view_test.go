//go:build loopdev

package loopdev

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/fusefs"
)

type viewRoots map[string]fsx.Dir

func (r viewRoots) Root(disk string) (fsx.Dir, bool) {
	d, ok := r[disk]
	return d, ok
}

// TestViewMatchesMergerfs: on each driver, a library replicated on two disks, then mounted by mergerfs from the line
// atoll mergerfs-config prints, and by Atoll's own view from the catalog alone, read only. Both list the same names,
// with the same kind, mode, owner, size, modification time, symlink target and content (specs.md §21.2: "the
// listing matches what mergerfs would show"). Where the copies differ, the view reads the original, the user's copy,
// while mergerfs reads the first branch; and the view writes nothing.
func TestViewMatchesMergerfs(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n[[rule]]\nlabel = \"partial\"\nfilename = \"*.part\"\naction = \"ignore\"\n")
			write(t, b.keep, "Films/A.mkv", "a film")
			write(t, b.keep, "Series/S01/e01.mkv", "e01")
			write(t, b.at, "Films/B.mkv", "the user's own")
			write(t, b.at, "Music/x.flac", "x")
			write(t, b.at, "Music/y.part", "partial")
			mustDo(t, os.MkdirAll(filepath.Join(b.keep, "media", "Empty"), 0o750))
			mustDo(t, os.Symlink("../Films/A.mkv", filepath.Join(b.at, "media", "Music", "link")))
			b.scan(false)
			catchUp(b)
			b.scan(false)

			line, merged := fstabLine(b)
			mountView(t, line, merged)
			roots := viewRoots{}
			for id, top := range map[string]string{"k": b.keep, "v": b.at} {
				dir, err := fsx.OpenDir(filepath.Join(top, "media"))
				mustDo(t, err)
				t.Cleanup(func() { _ = dir.Close() })
				roots[id] = dir
			}
			own := mountPoint(t)
			mustDo(t, os.MkdirAll(own, 0o755))
			tree := view.NewTree(b.db, roots, view.Owner{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}, time.Now().UnixNano())
			m, err := fusefs.Mount(tree, own)
			mustDo(t, err)
			t.Cleanup(func() { _ = m.Unmount() })

			want, got := described(t, merged), described(t, own)
			if !maps.Equal(got, want) {
				for p := range maps.Keys(want) {
					if got[p] != want[p] {
						t.Errorf("%s: the view %q, mergerfs %q", p, got[p], want[p])
					}
				}
				for p := range maps.Keys(got) {
					if _, ok := want[p]; !ok {
						t.Errorf("%s: the view %q, mergerfs nothing", p, got[p])
					}
				}
			}

			// B.mkv is the user's on v, Atoll's replica on k, the first branch. The replica rots, its times kept: mergerfs
			// reads it, the view reads the original.
			rotted := filepath.Join(b.keep, "media", "Films", "B.mkv")
			info, err := os.Stat(rotted)
			mustDo(t, err)
			mustDo(t, os.WriteFile(rotted, []byte("THE USER'S OWN"), 0o644))
			mustDo(t, os.Chtimes(rotted, info.ModTime(), info.ModTime()))
			time.Sleep(1100 * time.Millisecond) // past the kernel's caches of 1 s
			if data, err := os.ReadFile(filepath.Join(own, "Films", "B.mkv")); err != nil || string(data) != "the user's own" {
				t.Errorf("the view reads %q, %v: not the original", data, err)
			}
			if data, err := os.ReadFile(filepath.Join(merged, "Films", "B.mkv")); err != nil || string(data) != "THE USER'S OWN" {
				t.Errorf("mergerfs reads %q, %v", data, err)
			}

			// Read only: nothing is written through the view.
			for _, err := range []error{
				os.WriteFile(filepath.Join(own, "Films", "C.mkv"), []byte("c"), 0o644),
				os.Remove(filepath.Join(own, "Films", "A.mkv")),
				os.Rename(filepath.Join(own, "Music"), filepath.Join(own, "Sound")),
				os.Mkdir(filepath.Join(own, "New"), 0o755),
			} {
				if !errors.Is(err, syscall.EROFS) {
					t.Errorf("a write through the view: %v, want EROFS", err)
				}
			}
			var st syscall.Statfs_t
			mustDo(t, syscall.Statfs(own, &st))
			if st.Bsize != 4096 || st.Blocks == 0 || st.Namelen != 255 {
				t.Errorf("statfs: %+v", st)
			}
			mustDo(t, m.Unmount())
		})
	}
}

// TestViewBrowsesNoDisk: browsing the view reads the catalog alone (specs.md §21.2, §21.6). Once a disk's device
// answers every read with an error and the page cache is dropped, the whole library still lists, every name with its
// attributes as before; a file presented from the other disk reads, and one presented from the failed disk fails
// with EIO, as nothing was read from it until then.
func TestViewBrowsesNoDisk(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			m := newMapped(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, m.volume, tc.driver, at, options...)
			b := newPassBenchWith(t, m.volume, at, "[defaults]\nsettle = \"0s\"\n")
			write(t, b.keep, "Films/A.mkv", "from k")
			// Past what NTFS keeps in the file's own record, which the driver may still hold once the device fails.
			write(t, b.at, "Films/B.mkv", strings.Repeat("v", 64<<10))
			write(t, b.at, "Series/S01/e01.mkv", "e01")
			b.scan(false)
			catchUp(b)
			b.scan(false)
			roots := viewRoots{}
			for id, top := range map[string]string{"k": b.keep, "v": b.at} {
				dir, err := fsx.OpenDir(filepath.Join(top, "media"))
				mustDo(t, err)
				t.Cleanup(func() { _ = dir.Close() })
				roots[id] = dir
			}
			own := mountPoint(t)
			mustDo(t, os.MkdirAll(own, 0o755))
			tree := view.NewTree(b.db, roots, view.Owner{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}, time.Now().UnixNano())
			mounted, err := fusefs.Mount(tree, own)
			mustDo(t, err)
			t.Cleanup(func() { _ = mounted.Unmount() })
			before := listedOnly(t, own)

			must(t, "sync")
			m.fail(t)
			mustDo(t, os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644))
			time.Sleep(1100 * time.Millisecond) // past the kernel's caches of the view
			if after := listedOnly(t, own); !maps.Equal(after, before) {
				t.Errorf("listed with a disk that fails: %v, before: %v", after, before)
			}
			if data, err := os.ReadFile(filepath.Join(own, "Films", "A.mkv")); err != nil || string(data) != "from k" {
				t.Errorf("a file presented from the disk that answers: %q, %v", data, err)
			}
			if _, err := os.ReadFile(filepath.Join(own, "Films", "B.mkv")); !errors.Is(err, syscall.EIO) {
				t.Errorf("a file presented from the disk that fails: %v, want EIO", err)
			}
			mustDo(t, mounted.Unmount())
			m.heal(t)
		})
	}
}

// listedOnly gives, for each path below root, its kind, mode, owner and size, as a listing finds them, reading no
// file.
func listedOnly(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		size := info.Size()
		if info.IsDir() {
			size = 0
		}
		out[rel] = fmt.Sprintf("%v %d:%d %d %d", info.Mode(), st.Uid, st.Gid, size, info.ModTime().UnixNano())
		return nil
	})
	mustDo(t, err)
	return out
}

// described gives, for each path below root, what it is: its kind, mode and owner, and for a file or a symlink its
// size, its modification time or target, and its content. A folder's size and times are left out: each copy made in
// it moves them on its disk, between the last walk and the mount.
func described(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		desc := fmt.Sprintf("%v %d:%d", info.Mode(), st.Uid, st.Gid)
		switch {
		case info.IsDir():
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			desc += fmt.Sprintf(" %d -> %s", info.Size(), target)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			desc += fmt.Sprintf(" %d %d %q", info.Size(), info.ModTime().UnixNano(), data)
		}
		out[rel] = desc
		return nil
	})
	mustDo(t, err)
	return out
}
