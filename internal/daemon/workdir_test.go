package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// After a pass, the daemon tells of each disk it found mounted whose work folder belongs to an account that is none
// of Atoll's: neither root's, its own, nor the catalog's owner's. A disk that is away, one whose filesystem holds no
// owner, and one with no work folder yet say nothing (specs.md §10, §23).
func TestForeignWorkdirs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a folder to another account takes root")
	}
	top := t.TempDir()
	catalog := filepath.Join(top, "catalog.db")
	if err := os.WriteFile(catalog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(catalog, 997, 997); err != nil {
		t.Fatal(err)
	}
	var list []config.Disk
	var lines []jobs.DiskLine
	for _, tc := range []struct {
		id      string
		fs      names.Filesystem
		owner   int // of its work folder; -1 for none
		verdict disks.State
	}{
		{"mine", names.Ext4, 997, disks.Online},
		{"roots", names.XFS, 0, disks.Online},
		{"theirs", names.Ext4, 1000, disks.Online},
		{"readonly", names.Btrfs, 1001, disks.Readonly},
		{"away", names.Ext4, 1000, disks.Missing},
		{"ntfs", names.NTFS, 1000, disks.Online},
		{"new", names.Ext4, -1, disks.Online},
		{"links", names.Ext4, 997, disks.Online},
	} {
		mount := filepath.Join(top, tc.id)
		if tc.owner >= 0 {
			work := filepath.Join(mount, "Atoll")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(work, tc.owner, tc.owner); err != nil {
				t.Fatal(err)
			}
		}
		if tc.id == "links" {
			// The work folder is Atoll's, and the folder of its links another account's.
			links := filepath.Join(mount, "Atoll", "links")
			if err := os.Mkdir(links, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(links, 1002, 1002); err != nil {
				t.Fatal(err)
			}
		}
		list = append(list, config.Disk{ID: tc.id, Mount: mount, Workdir: "Atoll", FS: tc.fs})
		lines = append(lines, jobs.DiskLine{ID: tc.id, Verdict: disks.Verdict{State: tc.verdict}})
	}
	l := &loop{opt: &Options{Config: &config.Config{Disks: list}}, log: slog.New(slog.DiscardHandler), set: &settings{catalog: catalog}}
	got := l.foreignWorkdirs(&jobs.Outcome{Disks: lines})
	want := []alerts.ForeignWorkdir{
		{ID: "theirs", Path: filepath.Join(top, "theirs", "Atoll"), UID: 1000, Want: 997},
		{ID: "readonly", Path: filepath.Join(top, "readonly", "Atoll"), UID: 1001, Want: 997},
		{ID: "links", Path: filepath.Join(top, "links", "Atoll", "links"), UID: 1002, Want: 997},
	}
	if !slices.Equal(got, want) {
		t.Errorf("%+v", got)
	}
}
