package ops

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// TestAWildcardNameIsLeftOnTheTypeNTFS: on a disk of the type ntfs, which cannot take a name holding a wildcard
// character out of its folder, by a rename or an unlink, without setting its whole volume read-only (measured on Linux
// 7.3, docs/research/ntfs-driver.md), every operation that would is refused before anything is asked of the disk, as a
// name refused, and nothing moves: a move to the trash, out of it, a rename of a file or a folder, from or to such a
// path, a purge, the tidy of the trash's folders, an empty folder's removal, a link's move to the trash, and the view's
// rename and removal (specs.md §15.8, §22). On ntfs3, which moves and removes such a name, the same moves go.
func TestAWildcardNameIsLeftOnTheTypeNTFS(t *testing.T) {
	const entry = "2026-01-01/7/Who?/q?.mkv"
	for _, driver := range []string{"ntfs", "ntfs3"} {
		t.Run(driver, func(t *testing.T) {
			mount := t.TempDir()
			media := filepath.Join(mount, "media")
			trash := filepath.Join(mount, "Atoll", "trash")
			for _, folder := range []string{filepath.Join(media, "Who?", "Empty*"), filepath.Join(trash, "2026-01-01", "7", "Who?"), filepath.Join(mount, "Atoll", "links")} {
				mustMkdir(t, folder)
			}
			for _, file := range []string{filepath.Join(media, "Who?", "q?.mkv"), filepath.Join(media, "plain.mkv"), filepath.Join(trash, filepath.FromSlash(entry)), filepath.Join(mount, "Atoll", "links", "9")} {
				if err := os.WriteFile(file, []byte("film"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			m := &onDisk{driver: driver, fail: func(call, name string) error {
				switch call {
				case "rename", "plainrename", "exchange", "link", "unlink", "rmdir":
					calls = append(calls, call+" "+name)
				}
				return nil
			}}
			d, err := Open(m, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll", FS: names.NTFS}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if got := d.Leaves("Who?/q?.mkv"); got != (driver == "ntfs") {
				t.Errorf("Leaves: %v", got)
			}
			if d.Leaves("plain.mkv") {
				t.Error("Leaves a name with no wildcard")
			}
			like := func(string) (*fsx.Stat, error) { return &fsx.Stat{Mode: 0o755}, nil }
			key := keyOfPath(t, filepath.Join(media, "Who?", "q?.mkv"))
			row := &catalog.KnownCopy{Path: "Who?/q?.mkv", FileKey: sql.NullString{String: key, Valid: true}}
			left := []struct {
				what string
				do   func() error
			}{
				{"a move to the trash", func() error {
					_, err := d.ToTrash(&Op{ID: 3, Disk: "t1", Kind: catalog.OpTrash, Path: "Who?/q?.mkv", Path2: "2026-01-01/3/Who?/q?.mkv", FileKey: key}, row)
					return err
				}},
				{"a move out of the trash", func() error {
					_, err := d.FromTrash(&Op{ID: 4, Disk: "t1", Kind: catalog.OpRestore, Path: "Who?/q?.mkv", Path2: entry})
					return err
				}},
				{"a rename from such a path", func() error {
					_, _, err := d.RenameFile(&Op{ID: 5, Disk: "t1", Kind: catalog.OpRename, Path: "Who?/q?.mkv", Path2: "q.mkv", FileKey: key}, row)
					return err
				}},
				{"a rename to such a path", func() error {
					_, _, err := d.RenameFile(&Op{ID: 6, Disk: "t1", Kind: catalog.OpRename, Path: "plain.mkv", Path2: "plain?.mkv"}, &catalog.KnownCopy{Path: "plain.mkv"})
					return err
				}},
				{"a folder's rename", func() error {
					return d.RenameFolder(&Op{ID: 7, Disk: "t1", Kind: catalog.OpRename, Path: "Who?", Path2: "Who"})
				}},
				{"a purge", func() error {
					_, err := d.PurgeEntry(&Op{ID: 8, Disk: "t1", Kind: catalog.OpUnlink, Path: "Who?/q?.mkv", Path2: entry})
					return err
				}},
				{"an empty folder's removal", func() error {
					return d.RemoveFolder(&Op{ID: 9, Disk: "t1", Path: "Who?/Empty*"})
				}},
				{"a link's move to the trash", func() error {
					_, err := d.LinkToTrash(&Op{ID: 10, Disk: "t1", Kind: catalog.OpTrash, Path: "Who?/q?.mkv", Path2: "2026-01-01/10/Who?/q?.mkv", Link: 9})
					return err
				}},
				{"the view's rename", func() error { return d.RenameUser("Who?/q?.mkv", "q.mkv", false, key, like) }},
				{"the view's removal", func() error { return d.RemoveUser("Who?/q?.mkv", false) }},
				{"the view's removal of what it left", func() error {
					return d.RemoveLeft(&Op{ID: 11, Disk: "t1", Kind: catalog.OpUnlink, Path: "Who?/q?.mkv", FileKey: key}, row)
				}},
			}
			if driver == "ntfs" {
				for _, l := range left {
					if err := l.do(); !errors.Is(err, ErrNameRefused) {
						t.Errorf("%s: %v, want the name refused", l.what, err)
					}
				}
				d.CleanTrash(entry)
				if len(calls) > 0 {
					t.Errorf("asked of the disk: %v", calls)
				}
				for _, file := range []string{filepath.Join(media, "Who?", "q?.mkv"), filepath.Join(media, "Who?", "Empty*"), filepath.Join(trash, filepath.FromSlash(entry)), filepath.Join(media, "plain.mkv")} {
					if _, err := os.Lstat(file); err != nil {
						t.Errorf("%s: %v", file, err)
					}
				}
				return
			}
			// ntfs3 moves and removes such a name: the view's rename goes, as any.
			if err := d.RenameUser("Who?/q?.mkv", "q.mkv", false, key, like); err != nil {
				t.Errorf("the view's rename: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(media, "q.mkv")); err != nil {
				t.Errorf("the renamed file: %v", err)
			}
		})
	}
}
