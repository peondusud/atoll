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
}

func (f *fakeDisk) Mount(mount string) disks.MountSeen {
	if mount != f.mount {
		return disks.MountSeen{Path: disks.PathAbsent}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{Root: "/", MountPoint: mount, FSType: f.fsType,
		MountOptions: []string{"rw"}, SuperOptions: f.options}}
}

func (f *fakeDisk) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d", Variant: f.variant, TimePrecisionNS: 1}}
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
