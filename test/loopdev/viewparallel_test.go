//go:build loopdev

package loopdev

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/names"
)

// TestViewWritesInParallel: a new file in immediate mode written through Atoll's own view, served as atoll mount serves
// it, on a pool of ext4 and of each driver (specs.md §12.3, §21.2): as the program writes it, its bytes go to its
// original and to a part in tmp/ of the other disk, which holds the whole file once the program closed it, before any
// pass; then the pass makes that part the replica, by its inode, with nothing left in tmp/, and the view's operations
// are taken in. A second file the program truncates as it writes it has no part left.
func TestViewWritesInParallel(t *testing.T) {
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
			b := newPassBenchWith(t, v, at, "[defaults]\nmode = \"immediate\"\nreplicas = 1\nsettle = \"0s\"\n")
			b.scan(false)
			point := filepath.Join(t.TempDir(), "pool")
			mustDo(t, os.MkdirAll(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			defer func() {
				served.stop()
				mustDo(t, served.wait())
			}()
			film := bytes.Repeat([]byte("a film written once, "), 200_000)
			mustDo(t, os.WriteFile(filepath.Join(point, "New.mkv"), film, 0o644))
			// The view hands the part over once the kernel released the file, which it does after close returns.
			var disk, part string
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				disk = b.text("SELECT COALESCE((SELECT v.disk_id || ' ' || CAST(o.path2 AS TEXT) FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'part' AND v.state = 'done'), '')")
				if disk != "" {
					break
				}
			}
			if disk == "" {
				t.Fatal("no part handed over")
			}
			disk, part, _ = strings.Cut(disk, " ")
			top := map[string]string{"k": b.keep, "v": b.at}[disk]
			held, err := os.ReadFile(filepath.Join(top, "Atoll", "tmp", part))
			mustDo(t, err)
			if !bytes.Equal(held, film) {
				t.Fatalf("the part on %s holds %d bytes of other content", disk, len(held))
			}
			inode := inodeOf(t, filepath.Join(top, "Atoll", "tmp", part))

			b.settle()
			replica := filepath.Join(top, "media", "New.mkv")
			if data, err := os.ReadFile(replica); err != nil || !bytes.Equal(data, film) {
				t.Fatalf("the replica on %s: %d bytes, %v", disk, len(data), err)
			}
			if inodeOf(t, replica) != inode {
				t.Error("the replica is not the part the view wrote")
			}
			if _, err := os.Stat(filepath.Join(top, "Atoll", "tmp", part)); !os.IsNotExist(err) {
				t.Errorf("the part is still in tmp/: %v", err)
			}
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				b.settle()
				if open, err = catalog.OpenViewOps(t.Context(), b.db); err != nil || len(open) != 0 {
					// What the catalog holds of the file, against what its disks hold, says which look did not take it in.
					t.Errorf("not taken in: %+v, %v; the file %s; its copies %s; on v %v", open, err,
						b.text("SELECT COALESCE((SELECT state || ' size ' || size || ' mtime ' || mtime_ns FROM files WHERE CAST(lpath AS TEXT) = 'New.mkv'), 'none')"),
						b.text("SELECT COALESCE(group_concat(c.disk_id || ' ' || c.state || ' ' || c.created_by || ' size ' || COALESCE(c.size, -1) || ' mtime ' || COALESCE(c.mtime_ns, -1) || ' ctime ' || COALESCE(c.ctime_ns, -1) || ' key ' || COALESCE(c.file_key, ''), '; '), 'none') FROM copies c JOIN files f ON f.id = c.file_id WHERE CAST(f.lpath AS TEXT) = 'New.mkv'"),
						statOf(filepath.Join(b.at, "media", "New.mkv")))
				}
			}

			f, err := os.Create(filepath.Join(point, "Cut.mkv"))
			mustDo(t, err)
			_, err = f.Write(film)
			mustDo(t, err)
			mustDo(t, f.Truncate(10))
			mustDo(t, f.Close())
			time.Sleep(500 * time.Millisecond)
			if got := b.text("SELECT count(*) FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'part' AND CAST(v.lpath AS TEXT) = 'Cut.mkv' AND (v.state <> 'undone' OR o.state <> 'undone')"); got != "0" {
				t.Errorf("a part of a file truncated as it was written is left: %s", got)
			}
		})
	}
}

// statOf says what a disk holds at a path, as a failure's message gives it: size, times and inode, or the error.
func statOf(path string) string {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("size %d mtime %d ctime %d ino %d", st.Size, st.Mtim.Nano(), st.Ctim.Nano(), st.Ino)
}
