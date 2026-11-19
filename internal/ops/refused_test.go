package ops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// On the type ntfs, EINVAL from a rename that must replace nothing is a name the driver refuses: neither link nor a
// plain rename is tried, as that driver always knows the flag, and does not apply windows_names to link while it does to
// unlink, so a file linked there would keep both names (specs.md §17.1, docs/research/ntfs-driver.md). On every other
// driver link tells, as on ntfs-3g, which gives EINVAL whatever the name; and a folder, which has no link, takes a look
// and a plain rename.
func TestARenameRefusedOnTheTypeNTFS(t *testing.T) {
	for _, tc := range []struct {
		name, driver string
		fs           names.Filesystem
		refused      bool
	}{
		{"ntfs", "ntfs", names.NTFS, true},
		{"ntfs3", "ntfs3", names.NTFS, false},
		{"ntfs-3g", "fuseblk", names.NTFS, false},
		{"no entry in the mount table", "", names.NTFS, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := t.TempDir()
			media := filepath.Join(mount, "media")
			mustMkdir(t, filepath.Join(media, "Films", "Old"))
			if err := os.WriteFile(filepath.Join(media, "Films", "a.mkv"), []byte("film"), 0o644); err != nil {
				t.Fatal(err)
			}
			var tried []string
			m := &onDisk{driver: tc.driver, fail: func(call, _ string) error {
				switch call {
				case "rename":
					return syscall.EINVAL
				case "link", "plainrename":
					tried = append(tried, call)
				}
				return nil
			}}
			d, err := Open(m, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll", FS: tc.fs}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			like := func(string) (*fsx.Stat, error) { return &fsx.Stat{Mode: 0o755}, nil }

			err = d.RenameUser("Films/a.mkv", "Films/b.mkv", false, keyOfPath(t, filepath.Join(media, "Films", "a.mkv")), like)
			switch {
			case tc.refused && !errors.Is(err, ErrNameRefused):
				t.Errorf("a file: %v", err)
			case !tc.refused && err != nil:
				t.Errorf("a file: %v", err)
			}
			if got := slices.Contains(tried, "link"); got == tc.refused {
				t.Errorf("a file: link tried %v, calls %v", got, tried)
			}
			if _, err := os.Lstat(filepath.Join(media, "Films", "a.mkv")); (err == nil) != tc.refused {
				t.Errorf("a file: the old name there: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(media, "Films", "b.mkv")); (err == nil) == tc.refused {
				t.Errorf("a file: the new name there: %v", err)
			}

			tried = nil
			err = d.RenameUser("Films/Old", "Films/New", true, keyOfPath(t, filepath.Join(media, "Films", "Old")), like)
			switch {
			case tc.refused && !errors.Is(err, ErrNameRefused):
				t.Errorf("a folder: %v", err)
			case !tc.refused && err != nil:
				t.Errorf("a folder: %v", err)
			}
			if got := slices.Contains(tried, "plainrename"); got == tc.refused {
				t.Errorf("a folder: plain rename tried %v, calls %v", got, tried)
			}
			if _, err := os.Lstat(filepath.Join(media, "Films", "Old")); (err == nil) != tc.refused {
				t.Errorf("a folder: the old name there: %v", err)
			}
		})
	}
}
