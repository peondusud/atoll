//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// TestTheLinkGuardThroughMergerfs: with trash.link_guard on an ext4 disk and each driver, under a real mergerfs view.
// Each copy gets a link in Atoll/links/ to its own inode, which the walk does not count among the file's names; a
// rename through the view keeps it; a deletion through the view, which removes every copy at once, leaves each disk's
// data in that disk's trash, deleted, which a restore brings back; and all of it still so through a cold cache
// (specs.md §17).
func TestTheLinkGuardThroughMergerfs(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[trash]\nlink_guard = true\n")
			tops := []string{b.keep, b.at}
			for _, top := range tops {
				write(t, top, "Films/a.mkv", "first")
			}
			b.scan(true)
			b.pass()
			for _, top := range tops {
				media, link := linkedNames(t, top, "Films/a.mkv")
				if !os.SameFile(media, link) {
					t.Fatalf("%s: the link is not the copy", top)
				}
				if n := media.Sys().(*syscall.Stat_t).Nlink; n != 2 { //nolint:forcetypeassert // Linux only
					t.Errorf("%s: %d names", top, n)
				}
			}
			if out := b.pass(); len(out.Decisions) != 0 || len(out.Operations) != 0 {
				t.Fatalf("the links read as a change: %+v", out)
			}

			pool := viewOf(t, b)
			mustDo(t, os.Rename(filepath.Join(pool, "Films", "a.mkv"), filepath.Join(pool, "Films", "b.mkv")))
			b.settle()
			if got := b.text("SELECT group_concat(nlink, ',') FROM copies"); got != "1,1" {
				t.Errorf("the rows count %s names", got)
			}
			for _, top := range tops {
				if media, link := linkedNames(t, top, "Films/b.mkv"); !os.SameFile(media, link) {
					t.Errorf("%s: the link did not follow the rename", top)
				}
			}

			mustDo(t, os.Remove(filepath.Join(pool, "Films", "b.mkv")))
			b.settle()
			if got := b.text("SELECT count(*) FROM files"); got != "0" {
				t.Fatalf("%s files left", got)
			}
			if got := b.text("SELECT group_concat(line, ', ') FROM (SELECT disk_id || ' ' || reason || ' ' || CAST(lpath AS TEXT) AS line FROM trash ORDER BY disk_id)"); got != "k deleted Films/b.mkv, v deleted Films/b.mkv" {
				t.Fatalf("the trash holds %s", got)
			}
			dropCaches(t)
			for i, top := range tops {
				id := []string{"k", "v"}[i]
				entry := filepath.Join(top, "Atoll", "trash", filepath.FromSlash(b.text("SELECT CAST(trash_path AS TEXT) FROM trash WHERE disk_id = ?", id)))
				if data, err := os.ReadFile(entry); err != nil || string(data) != "first" {
					t.Errorf("%s: the trash entry: %q, %v", top, data, err)
				}
				names, err := os.ReadDir(filepath.Join(top, "Atoll", "links"))
				if err != nil || len(names) != 0 {
					t.Errorf("%s: the links folder holds %v, %v", top, names, err)
				}
			}

			s, err := trash.Open(t.Context(), b.db, b.config, b.machine, true)
			mustDo(t, err)
			var got string
			_, err = s.Restore(t.Context(), "Films/b.mkv", func(i *trash.EntryItem) error { got = i.Outcome; return nil })
			s.Close()
			mustDo(t, err)
			if got != trash.OutcomeRestored {
				t.Fatalf("restore: %s", got)
			}
			dropCaches(t)
			if data, err := os.ReadFile(filepath.Join(pool, "Films", "b.mkv")); err != nil || string(data) != "first" {
				t.Errorf("through the view: %q, %v", data, err)
			}
		})
	}
}

// linkedNames looks at a copy and at the link the guard gives it, file 1.
func linkedNames(t *testing.T, top, logical string) (os.FileInfo, os.FileInfo) {
	t.Helper()
	media, err := os.Lstat(filepath.Join(top, "media", filepath.FromSlash(logical)))
	mustDo(t, err)
	link, err := os.Lstat(filepath.Join(top, "Atoll", "links", "1"))
	mustDo(t, err)
	return media, link
}
