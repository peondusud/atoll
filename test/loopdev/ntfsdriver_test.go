//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/names"
)

// TestALinkOnTheTypeNTFSMovesItsTime pins a bug of the driver of the type ntfs, measured on Linux 7.3: link sets every
// time of its file to now, the modification time included, where ntfs3 keeps it (docs/research/ntfs-driver.md). So the
// link guard makes no link there, and a hard link made there reads as a change of time (specs.md §17, §22). A kernel
// whose driver keeps the time fails here: the guard can link there again.
func TestALinkOnTheTypeNTFSMovesItsTime(t *testing.T) {
	needRoot(t)
	for _, driver := range []string{"ntfs3", "ntfs"} {
		t.Run(driver, func(t *testing.T) {
			options := driverOptions(t, driver)
			v := newVolume(t, names.NTFS)
			at := mountPoint(t)
			mountVolume(t, v, driver, at, options...)
			file := filepath.Join(at, "a")
			mustDo(t, os.WriteFile(file, []byte("a"), 0o644))
			when := time.Unix(1500000000, 0)
			mustDo(t, os.Chtimes(file, when, when))
			mustDo(t, os.Link(file, filepath.Join(at, "b")))
			dropCaches(t)
			info, err := os.Stat(file)
			mustDo(t, err)
			moved := !info.ModTime().Equal(when)
			if moved != (driver == "ntfs") {
				t.Errorf("the modification time after a link: %v, was %v", info.ModTime(), when)
			}
		})
	}
}

// TestTheLinkGuardOnTheTypeNTFS: with trash.link_guard, a pass gives the ext4 disk's copy its link, and none to the
// copy on a disk of the type ntfs, whose time a link would move: that copy keeps its time, and the next pass finds
// nothing changed, on either disk (specs.md §17, §22).
func TestTheLinkGuardOnTheTypeNTFS(t *testing.T) {
	needRoot(t)
	options := driverOptions(t, "ntfs")
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mountVolume(t, v, "ntfs", at, options...)
	b := newPassBenchWith(t, v, at, "[trash]\nlink_guard = true\n")
	when := time.Unix(1500000000, 0)
	for _, top := range []string{b.keep, at} {
		write(t, top, "Films/a.mkv", "first")
		mustDo(t, os.Chtimes(filepath.Join(top, "media", "Films", "a.mkv"), when, when))
	}
	b.scan(true)
	b.pass()
	if media, link := linkedNames(t, b.keep, "Films/a.mkv"); !os.SameFile(media, link) {
		t.Fatal("the ext4 disk's link is not its copy")
	}
	if _, err := os.Lstat(filepath.Join(at, "Atoll", "links")); !os.IsNotExist(err) {
		t.Errorf("the links folder of the ntfs disk: %v", err)
	}
	dropCaches(t)
	info, err := os.Stat(filepath.Join(at, "media", "Films", "a.mkv"))
	mustDo(t, err)
	if !info.ModTime().Equal(when) {
		t.Errorf("the copy's time moved: %v", info.ModTime())
	}
	if out := b.pass(); len(out.Decisions) != 0 || len(out.Operations) != 0 || out.Partial {
		t.Errorf("a pass found something to do: %+v", out)
	}
	if got := b.text("SELECT count(*) FROM ops WHERE kind = 'link'"); got != "1" {
		t.Errorf("%s links journaled", got)
	}
}
