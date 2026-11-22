//go:build loopdev

package loopdev

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// TestViewMovesAFileThatRunsOutOfSpace: a new file written through Atoll's own view, served as atoll mount serves it
// with view.move_on_enospc, lands on a small ext4 disk, the first of the order, and is written past what that disk
// holds (specs.md §21.2): the write the disk answers with ENOSPC moves the file to the other disk, and the program never
// sees the error. Once it closed the file, the other disk holds every byte it wrote, as the view presents it; the small
// disk holds nothing at its path, nor in its trash; the move's operations are done; and the pass that follows records
// the file on the disk it went to and takes the view's operations in. Two small disks, so that what the full one can
// still make does not depend on its last free blocks, which differ from one kernel to the next:
//   - with no inode left once the file is made there, no folder of the trash can be made, and the copy the move left is
//     removed at its path (the maintainer, 2026-11-21), an unlink with no place in the trash;
//   - with inline_data, a new folder holds its few names in its inode and needs no block, so the trash's folders are
//     made with no block free, and the copy left goes to the trash, then is purged from there.
func TestViewMovesAFileThatRunsOutOfSpace(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name string
		mkfs []string
		// noInode fills the small disk's inodes but one, which the file takes.
		noInode bool
		// here says that the copy left is removed at its path, not from the trash.
		here bool
	}{
		{name: "no folder can be made", mkfs: []string{"-N", "128"}, noInode: true, here: true},
		{name: "the trash's folders need no block", mkfs: []string{"-O", "inline_data", "-I", "256"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := newVolumeSized(t, names.Ext4, 24, tc.mkfs...)
			v := newVolume(t, names.Ext4)
			at := mountPoint(t)
			mountVolume(t, v, "", at)
			b := newPassBenchKept(t, small, v, at, benchGuards, "[placement]\nnew_files = \"ordered\"\nfill_to = \"99%\"\nmin_free = [\"1MiB\"]\n")
			text := strings.Replace(b.configText, "[view]\n", "[view]\nmove_on_enospc = true\n", 1)
			c, err := config.Parse([]byte(text))
			mustDo(t, err)
			b.config, b.configText = c, text
			b.scan(false)
			point := filepath.Join(t.TempDir(), "pool")
			mustDo(t, os.MkdirAll(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			defer func() {
				served.stop()
				mustDo(t, served.wait())
			}()
			if tc.noInode {
				fillInodes(t, b.keep, 1)
			}

			// 40 MiB, past the 24 of the small disk, written a MiB at a time as a program writes a download.
			chunk := bytes.Repeat([]byte("a film that does not fit, "), (1<<20)/26+1)[:1<<20]
			f, err := os.Create(filepath.Join(point, "Big.mkv"))
			mustDo(t, err)
			var want []byte
			for range 40 {
				if _, err := f.Write(chunk); err != nil {
					t.Fatalf("a write through the view, at %d MiB: %v", len(want)>>20, err)
				}
				want = append(want, chunk...)
			}
			mustDo(t, f.Close())

			through, err := os.ReadFile(filepath.Join(point, "Big.mkv"))
			mustDo(t, err)
			if !bytes.Equal(through, want) {
				t.Fatalf("the view gives %d bytes of other content, want %d", len(through), len(want))
			}
			moved, err := os.ReadFile(filepath.Join(b.at, "media", "Big.mkv"))
			mustDo(t, err)
			if !bytes.Equal(moved, want) {
				t.Fatalf("the disk it moved to holds %d bytes of other content", len(moved))
			}
			if _, err := os.Stat(filepath.Join(b.keep, "media", "Big.mkv")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the small disk still holds it: %v", err)
			}
			left := 0
			_ = filepath.WalkDir(filepath.Join(b.keep, "Atoll", "trash"), func(_ string, e fs.DirEntry, err error) error {
				if err == nil && !e.IsDir() {
					left++
				}
				return nil
			})
			if left != 0 {
				t.Errorf("the small disk's trash holds %d files", left)
			}
			want2 := "k>v done unlink done in the trash"
			if tc.here {
				want2 = "k>v done unlink done at its path"
			}
			if got := b.text("SELECT group_concat(v.disk2_id || '>' || v.disk_id || ' ' || v.state || ' ' || o.kind || ' ' || o.state || ' ' || CASE WHEN o.path2 IS NULL THEN 'at its path' ELSE 'in the trash' END, ', ') FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'move'"); got != want2 {
				t.Errorf("the moves: %q, want %q", got, want2)
			}

			b.settle()
			if got := b.text("SELECT group_concat(c.disk_id || ' ' || c.created_by, ', ') FROM copies c JOIN files f ON f.id = c.file_id WHERE CAST(f.lpath AS TEXT) = 'Big.mkv'"); got != "v user" {
				t.Errorf("the copies the pass recorded: %q", got)
			}
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
		})
	}
}

// fillInodes makes empty files at the top of a mounted filesystem, outside its root, until it has free but inodes left,
// so that nothing more than that can be made there: no file, no folder.
func fillInodes(t *testing.T, top string, free uint64) {
	t.Helper()
	for i := 0; ; i++ {
		var st syscall.Statfs_t
		mustDo(t, syscall.Statfs(top, &st))
		if st.Ffree <= free {
			return
		}
		f, err := os.Create(filepath.Join(top, fmt.Sprintf("fill-%05d", i)))
		mustDo(t, err)
		mustDo(t, f.Close())
	}
}
