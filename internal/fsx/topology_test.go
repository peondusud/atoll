package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
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
