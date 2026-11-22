//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
)

// TestTheMarkerOnEachDriver: the marker disk add and disk mark write, ops.(*Disk).WriteMarker through a handle held to
// the mount, by way of disk.toml.new moved without replacing anything, a link then the removal of the first name on a
// driver that refuses such a rename, is there as its four lines, and nothing is left beside it. The check of what runs
// for long, the daemon's and the view's, reads it once per mount (specs.md §10): a disk whose marker no one saw is
// online without one, and one whose marker was seen missing, no-marker; its own marker written, online, an absence
// being read again; the marker removed while the same mount holds it, kept; mounted again, missing, no-marker. Another
// pool's marker is readonly, other-marker, and so is a symbolic link at its name, never followed.
func TestTheMarkerOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name    string
		fs      names.Filesystem
		driver  func(t *testing.T) string
		options func(t *testing.T) []string
	}{
		{"ext4", names.Ext4, func(*testing.T) string { return "" }, func(*testing.T) []string { return nil }},
		{"ntfs3", names.NTFS, ntfs3Type, ntfs3Options},
		{"ntfs", names.NTFS, ntfsType, func(*testing.T) []string { return ntfsOptions }},
		{"ntfs-3g", names.NTFS, func(*testing.T) string { return "ntfs-3g" }, func(*testing.T) []string { return nil }},
		{"lowntfs-3g", names.NTFS, func(*testing.T) string { return "lowntfs-3g" }, func(*testing.T) []string { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := tc.driver(t)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, driver, at, tc.options(t)...)
			mustDo(t, os.MkdirAll(filepath.Join(at, "media"), 0o755))
			d := diskOf(v, at)
			d.ID, d.Workdir = "t1", "Atoll"
			m, err := disks.NewMachine()
			mustDo(t, err)
			want(t, disks.CheckKnown(m, d, "p", false), disks.Online, "")
			want(t, disks.CheckKnown(m, d, "p", true), disks.Missing, disks.NoMarker)

			text, err := layout.MarkerText(layout.Marker{PoolID: "p", DiskID: d.ID, FSUUID: d.UUID})
			mustDo(t, err)
			handle, err := ops.Open(m, d, 0)
			mustDo(t, err)
			mustDo(t, handle.Verify())
			mustDo(t, handle.WriteMarker(text))
			handle.Close()
			marker := filepath.Join(at, "Atoll", layout.MarkerName)
			if got, err := os.ReadFile(marker); err != nil || string(got) != text {
				t.Fatalf("the marker written: %q, %v", got, err)
			}
			if names, err := os.ReadDir(filepath.Join(at, "Atoll")); err != nil || len(names) != 1 {
				t.Errorf("beside the marker: %v, %v", names, err)
			}
			want(t, disks.CheckKnown(m, d, "p", true), disks.Online, "")

			// Kept for as long as the same mount holds the path; read again once mounted anew.
			mustDo(t, os.Remove(marker))
			want(t, disks.CheckKnown(m, d, "p", true), disks.Online, "")
			unmountFor(t, driver, at)
			want(t, disks.CheckKnown(m, d, "p", true), disks.Missing, disks.NotMounted)
			mustDo(t, remount(t, v, driver, at, tc.options(t)))
			letGo(t, at)
			want(t, disks.CheckKnown(m, d, "p", true), disks.Missing, disks.NoMarker)

			other, err := layout.MarkerText(layout.Marker{PoolID: "q", DiskID: d.ID, FSUUID: d.UUID})
			mustDo(t, err)
			mustDo(t, os.WriteFile(marker, []byte(other), 0o644))
			fresh, err := disks.NewMachine()
			mustDo(t, err)
			want(t, disks.CheckKnown(fresh, d, "p", true), disks.Readonly, disks.OtherMarker)
			mustDo(t, os.Remove(marker))
			if err := os.Symlink("/etc/hostname", marker); err != nil {
				t.Logf("no symbolic link on %s: %v", tc.name, err)
				return
			}
			fresh, err = disks.NewMachine()
			mustDo(t, err)
			want(t, disks.CheckKnown(fresh, d, "p", true), disks.Readonly, disks.OtherMarker)
		})
	}
}
