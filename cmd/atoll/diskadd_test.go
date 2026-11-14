package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// fakeDisk is a machine with one disk mounted, as a mount entry and a head describe it.
type fakeDisk struct {
	mount   string
	fsType  string
	options []string
	variant disks.Variant
	// noJournal says that the ext4 keeps no journal.
	noJournal bool
}

func (f *fakeDisk) Mount(mount string) disks.MountSeen {
	if mount != f.mount {
		return disks.MountSeen{Path: disks.PathAbsent}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{Root: "/", MountPoint: mount, FSType: f.fsType,
		MountOptions: []string{"rw"}, SuperOptions: f.options}}
}

func (f *fakeDisk) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d", Variant: f.variant, NoJournal: f.noJournal, TimePrecisionNS: 1}}
}

func (f *fakeDisk) Root(string, string) disks.RootSeen { return disks.RootSeen{Folder: disks.FolderOK} }
func (f *fakeDisk) Now() int64                         { return 0 }
func (f *fakeDisk) Space(string) (int64, int64, error) { return 1 << 30, 1 << 29, nil }
func (f *fakeDisk) OpenRoot(mount, root string) (fsx.Dir, error) {
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}
func (f *fakeDisk) OpenTop(mount string) (fsx.Dir, error) {
	return fsx.OpenDir(mount) //nolint:wrapcheck // a test double
}

func (f *fakeDisk) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return f.Device(fs, entry)
}
func (f *fakeDisk) Checkpoint(string) {}

// The variants that change the rules of names are refused at disk add; a disk only mounted read-only is taken,
// with a warning; a machine that cannot tell the topology says so (specs.md §10).
func TestDiskAddRefusesNameRules(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := []byte("pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n")
	c, err := config.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(dir, "u2")
	if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		machine *fakeDisk
		refused string
		state   string
	}{
		{"ntfs3 nocase", &fakeDisk{mount: mount, fsType: "ntfs3", options: []string{"rw", "iocharset=utf8", "nocase"}}, "nocase", ""},
		{"ntfs3 ads", &fakeDisk{mount: mount, fsType: "ntfs3", options: []string{"rw", "iocharset=utf8", "ads"}}, "ads", ""},
		{"ext4 casefold", &fakeDisk{mount: mount, fsType: "ext4", options: []string{"rw"}, variant: disks.VariantCasefold}, "casefold", ""},
		{"xfs ascii-ci", &fakeDisk{mount: mount, fsType: "xfs", options: []string{"rw"}, variant: disks.VariantASCIICI}, "ascii-ci", ""},
		// A name made there is not durable once its folder is synced, which every operation of Atoll's rests on.
		{"ext4 without a journal", &fakeDisk{mount: mount, fsType: "ext4", options: []string{"rw"}, noJournal: true}, "no-journal", ""},
		{"read-only", &fakeDisk{mount: mount, fsType: "ext4", options: []string{"ro"}}, "", "readonly"},
		{"exfat", &fakeDisk{mount: mount, fsType: "exfat", options: []string{"rw"}}, "exfat", ""},
		{"ext4", &fakeDisk{mount: mount, fsType: "ext4", options: []string{"rw"}}, "", "online"},
	} {
		var stderr bytes.Buffer
		a, err := checkDiskAdd(t.Context(), c, tc.machine, text, &diskAddArgs{mount: mount}, &stderr)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		want := []string{warnTopologyUnchecked, warnImmutableUnchecked}
		if tc.state == "readonly" {
			want = append([]string{warnReadonly}, want...)
		}
		if a.out.Disk.State != tc.state || !slices.Equal(a.out.Warnings, want) || a.out.PhysicalDisks != nil {
			t.Errorf("%s: %+v", tc.name, a.out)
		}
		// A read-only disk cannot take its marker now.
		if (a.marker != "") != (tc.state == "online") {
			t.Errorf("%s: marker %q", tc.name, a.marker)
		}
	}
}

// A disk whose work folder is there already, and belongs to an account that is neither root's, the one that runs
// the command nor the one that owns the catalog, is refused: whoever made that folder first reads what Atoll copies
// through it, and removes what its trash holds. Given back, the disk is taken (specs.md §10). It was taken as it was.
func TestDiskAddRefusesAWorkFolderOfAnotherAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a folder to another account takes root")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := []byte("pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n")
	c, err := config.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(dir, "u2")
	work := filepath.Join(mount, "Atoll")
	for _, folder := range []string{filepath.Join(mount, "media"), work} {
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	add := func() error {
		t.Helper()
		var stderr bytes.Buffer
		_, err := checkDiskAdd(t.Context(), c, &fakeDisk{mount: mount, fsType: "ext4", options: []string{"rw"}}, text, &diskAddArgs{mount: mount}, &stderr)
		return err
	}
	if err := os.Chown(work, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	err = add()
	if err == nil || !strings.Contains(err.Error(), "the account 1000") || !strings.Contains(err.Error(), "chown -R 0 "+work) {
		t.Errorf("a work folder of another account: %v", err)
	}
	if err := os.Chown(work, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := add(); err != nil {
		t.Errorf("given back: %v", err)
	}
	// The account that owns the catalog is the daemon's: its work folder is Atoll's.
	for _, path := range []string{file, work} {
		if err := os.Chown(path, 997, 997); err != nil {
			t.Fatal(err)
		}
	}
	if err := add(); err != nil {
		t.Errorf("the catalog's owner: %v", err)
	}
}

// A marker that says the same is left as it is, however it is written: with the lines of another system, or its keys
// in another order (specs.md §10). Atoll then went to write its own over it, which is refused, as a marker is never
// replaced: the disk was added with a warning and exit 4, for a marker that was right.
func TestDiskAddLeavesAMarkerThatSaysTheSame(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := []byte("pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n")
	c, err := config.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(dir, "u2")
	for _, folder := range []string{filepath.Join(mount, "media"), filepath.Join(mount, "Atoll")} {
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(mount, "Atoll", "disk.toml")
	const uuid = "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"
	for name, written := range map[string]string{
		"none":            "",
		"as Atoll writes": "format_version = 1\npool_id = \"p\"\ndisk_id = \"u2\"\nfs_uuid = \"" + uuid + "\"\n",
		"other line ends": "format_version = 1\r\npool_id = \"p\"\r\ndisk_id = \"u2\"\r\nfs_uuid = \"" + uuid + "\"\r\n",
		"another order":   "fs_uuid = \"" + uuid + "\"\ndisk_id = \"u2\"\npool_id = \"p\"\nformat_version = 1\n",
		"literal strings": "format_version = 1\npool_id = 'p'\ndisk_id = 'u2'\nfs_uuid = '" + uuid + "'\n",
	} {
		if written == "" {
			_ = os.Remove(marker)
		} else if err := os.WriteFile(marker, []byte(written), 0o644); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		a, err := checkDiskAdd(t.Context(), c, &fakeDisk{mount: mount, fsType: "ext4", options: []string{"rw"}}, text, &diskAddArgs{mount: mount}, &stderr)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// Only a disk with no marker has one to write.
		if (a.marker != "") != (written == "") {
			t.Errorf("%s: the marker to write is %q", name, a.marker)
		}
	}
}
