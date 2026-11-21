package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// sysTree makes folders as /sys/devices lays block devices out: a disk, its partitions inside it, each with a file
// named partition, and for a device made of others a folder slaves of links to them.
func sysTree(t *testing.T, dirs []string, partitions []string, slaves map[string][]string) string {
	t.Helper()
	top := t.TempDir()
	for _, dir := range append(slices.Clone(dirs), partitions...) {
		if err := os.MkdirAll(filepath.Join(top, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range partitions {
		if err := os.WriteFile(filepath.Join(top, p, "partition"), []byte("1\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for device, below := range slaves {
		folder := filepath.Join(top, device, "slaves")
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, s := range below {
			if err := os.Symlink(filepath.Join(top, s), filepath.Join(folder, filepath.Base(s))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return top
}

// The disks a filesystem lives on are followed down through everything between it and them (specs.md §10, R-06). A
// partition gave its parent's name and stopped there: a filesystem on md0p1, a partition of an array made of sda1
// and sdb1, lived on "md0", and one on sda3 on "sda", so the two were not seen to share a disk, and both were taken
// in one pool.
func TestDisksBelowAPartitionOfAnArray(t *testing.T) {
	top := sysTree(t,
		[]string{"pci/block/sda", "pci/block/sdb", "pci/block/nvme0n1", "virtual/block/md0", "virtual/block/dm-0", "virtual/block/dm-1", "virtual/block/loop0"},
		[]string{"pci/block/sda/sda1", "pci/block/sda/sda3", "pci/block/sdb/sdb1", "pci/block/nvme0n1/nvme0n1p2", "virtual/block/md0/md0p1"},
		map[string][]string{
			"virtual/block/md0":  {"pci/block/sda/sda1", "pci/block/sdb/sdb1"},
			"virtual/block/dm-0": {"virtual/block/md0/md0p1"},
			"virtual/block/dm-1": {"pci/block/nvme0n1/nvme0n1p2", "virtual/block/dm-0"},
		})
	for device, want := range map[string][]string{
		"pci/block/sda":               {"sda"},
		"pci/block/sda/sda3":          {"sda"},
		"pci/block/nvme0n1/nvme0n1p2": {"nvme0n1"},
		"virtual/block/loop0":         {"loop0"},
		"virtual/block/md0":           {"sda", "sdb"},
		// A partition of an array: the array's disks, not the array.
		"virtual/block/md0/md0p1": {"sda", "sdb"},
		// And what is made of it, however deep.
		"virtual/block/dm-0": {"sda", "sdb"},
		"virtual/block/dm-1": {"nvme0n1", "sda", "sdb"},
	} {
		got, err := disksBelow(filepath.Join(top, device), 0)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s lives on %v, %v, want %v", device, got, err, want)
		}
	}
}

// The kernel numbers each disk it makes, and a disk plugged again gets a new number, under the same name and device
// numbers too: a whole disk holds its own, and a partition its disk's. A kernel that gives none gives 0 (specs.md
// §15.13).
func TestDiskSeqIn(t *testing.T) {
	top := sysTree(t, []string{"pci/block/sdb", "virtual/block/loop0", "virtual/block/dm-0"}, []string{"pci/block/sdb/sdb1"}, nil)
	for dir, seq := range map[string]string{"pci/block/sdb": "41\n", "virtual/block/dm-0": "12\n"} {
		if err := os.WriteFile(filepath.Join(top, dir, "diskseq"), []byte(seq), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for dir, want := range map[string]uint64{
		"pci/block/sdb":       41,
		"pci/block/sdb/sdb1":  41,
		"virtual/block/dm-0":  12,
		"virtual/block/loop0": 0,
	} {
		if got, err := diskSeqIn(filepath.Join(top, dir)); got != want || err != nil {
			t.Errorf("%s: %d, %v, want %d", dir, got, err, want)
		}
	}
	if err := os.WriteFile(filepath.Join(top, "virtual/block/loop0", "diskseq"), []byte("x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := diskSeqIn(filepath.Join(top, "virtual/block/loop0")); err == nil {
		t.Error("a sequence number that is no number read")
	}
}

// What a device's name leads to: nothing, something that is no block device, or a block device with its numbers
// (specs.md §15.13).
func TestDeviceNodeOf(t *testing.T) {
	top := t.TempDir()
	file, link, gone := filepath.Join(top, "file"), filepath.Join(top, "by-uuid"), filepath.Join(top, "dangling")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, l := range [][2]string{{file, link}, {filepath.Join(top, "none"), gone}} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]DeviceNode{
		filepath.Join(top, "none"): {}, gone: {}, filepath.Join(file, "below"): {},
		file: {There: true}, link: {There: true}, "/dev/null": {There: true},
	} {
		if got, err := DeviceNodeOf(path); got != want || err != nil {
			t.Errorf("%s: %+v, %v, want %+v", path, got, err, want)
		}
	}
	// A block device, where the machine has one: its numbers are those stat gives.
	entries, _ := os.ReadDir("/sys/block")
	for _, e := range entries {
		path := "/dev/" + e.Name()
		var st unix.Stat_t
		if unix.Stat(path, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFBLK {
			continue
		}
		got, err := DeviceNodeOf(path)
		if err != nil || !got.There || !got.Block || got.Major != unix.Major(st.Rdev) || got.Minor != unix.Minor(st.Rdev) {
			t.Errorf("%s: %+v, %v", path, got, err)
		}
		return
	}
	t.Log("no block device to look at")
}
