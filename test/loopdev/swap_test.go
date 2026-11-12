//go:build loopdev

package loopdev

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// TestASwapOnEachDriver: two names swapped on the bench's ext4 disk, across two folders, reach each driver. ext4, XFS
// and btrfs exchange them in one step, each copy keeping its inode, as a cold cache reads it; the NTFS drivers refuse
// the exchange, so both copies wait as path-taken, with nothing journaled (specs.md §15.12).
func TestASwapOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "a", "file a")
				write(t, top, "d/b", "file b")
			}
			b.scan(true)
			inode := "SELECT c.file_key FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 'v' AND f.lpath = ?"
			inodeA, inodeB := b.text(inode, []byte("a")), b.text(inode, []byte("d/b"))
			rename(t, b.keep, "a", "t")
			rename(t, b.keep, "d/b", "a")
			rename(t, b.keep, "t", "d/b")
			out := b.settle()
			if out.Partial {
				t.Fatalf("partial: %+v", out)
			}
			want := trash.OutcomeRenamed
			if tc.fs == names.NTFS {
				want = trash.OutcomePathTaken
			}
			if len(out.Operations) != 2 {
				t.Fatalf("%+v", out.Operations)
			}
			for _, op := range out.Operations {
				if op.Kind != jobs.KindRename || op.Outcome != want {
					t.Errorf("%+v, want %s", op, want)
				}
			}
			dropCaches(t)
			if tc.fs == names.NTFS {
				wantContent(t, at, "a", "file a")
				wantContent(t, at, "d/b", "file b")
				if got := b.text("SELECT count(*) FROM ops"); got != "0" {
					t.Errorf("%s operations journaled", got)
				}
				// What a disk declared ntfs is not asked for, as measured here: the driver refuses the flag.
				err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(at, "media", "a"), unix.AT_FDCWD, filepath.Join(at, "media", "d", "b"), unix.RENAME_EXCHANGE)
				if !errors.Is(err, unix.EINVAL) {
					t.Errorf("the exchange gave %v, not EINVAL", err)
				}
				wantContent(t, at, "a", "file a")
				return
			}
			wantContent(t, at, "a", "file b")
			wantContent(t, at, "d/b", "file a")
			if got := b.text(inode, []byte("d/b")); got != inodeA {
				t.Errorf("the copy of a is inode %s, was %s", got, inodeA)
			}
			if got := b.text(inode, []byte("a")); got != inodeB {
				t.Errorf("the copy of b is inode %s, was %s", got, inodeB)
			}
			if got := b.text("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL OR gone_ns IS NOT NULL"); got != "0" {
				t.Errorf("%s rows behind", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'done'"); got != "2" {
				t.Errorf("%s renames done", got)
			}
			if out := b.pass(); len(out.Operations)+len(out.Decisions) != 0 {
				t.Errorf("another pass did more: %+v", out)
			}
		})
	}
}
