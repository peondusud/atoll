//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// TestShortNamesOnEachNTFSDriver: Windows gives a long name a short one, which no driver lists, which a look at finds,
// and which none creates (docs/research/ntfs-driver.md). Set with ntfs-3g, which alone writes them, on a folder and a
// file of an NTFS disk, the short names are refused as targets by each driver: a copy whose path goes through the
// folder's, and one at the file's, in small letters, as only ntfs3 answers them so, and nothing lands in the folder the
// short name names (specs.md §15.8).
func TestShortNamesOnEachNTFSDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		if tc.fs != names.NTFS {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			v := newVolume(t, names.NTFS)
			at := mountPoint(t)
			mountVolume(t, v, "ntfs-3g", at)
			write(t, at, "Long Folder/z.mkv", "z")
			write(t, at, "Film Name.mkv", "f")
			for name, short := range map[string]string{"Long Folder": "LONGFO~1", "Film Name.mkv": "FILMNA~1.MKV"} {
				if err := unix.Setxattr(filepath.Join(at, "media", name), "system.ntfs_dos_name", []byte(short), 0); err != nil {
					t.Fatalf("the short name of %s: %v", name, err)
				}
			}
			unmountWhenFree(t, at)
			mountVolume(t, v, tc.driver, at, driverOptions(t, tc.driver)...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			write(t, b.keep, "LONGFO~1/x.mkv", "x")
			write(t, b.keep, "filmna~1.mkv", "y")
			b.scan(true)
			out := b.settle()
			for _, path := range []string{"LONGFO~1/x.mkv", "filmna~1.mkv"} {
				i := slices.IndexFunc(out.Operations, func(op jobs.Operation) bool { return op.Kind == jobs.KindReplicate && op.Path == path })
				if i < 0 || out.Operations[i].Outcome != trash.OutcomeNameRefused {
					t.Errorf("%s: %+v", path, out.Operations)
				}
			}
			dropCaches(t)
			if got := listed(t, filepath.Join(at, "media")); !slices.Equal(got, []string{"Film Name.mkv", "Long Folder"}) {
				t.Errorf("the disk holds %v", got)
			}
			if got := listed(t, filepath.Join(at, "media", "Long Folder")); !slices.Equal(got, []string{"z.mkv"}) {
				t.Errorf("the folder the short name names holds %v", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE kind = 'create' AND disk_id = 'v' AND state <> 'undone'"); got != "0" {
				t.Errorf("%s copies to the NTFS disk not undone", got)
			}
			if parts, err := os.ReadDir(filepath.Join(at, "Atoll", "tmp")); err == nil && len(parts) != 0 {
				t.Errorf("parts left: %v", parts)
			}
			// The files of the NTFS disk reach the other as ever.
			wantContent(t, b.keep, "Long Folder/z.mkv", "z")
			wantContent(t, b.keep, "Film Name.mkv", "f")
		})
	}
}
