package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// ReplaceFile writes all or nothing, keeps the mode, follows a symbolic link, and refuses a file changed since it
// was read.
func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atoll.toml")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(link, []byte("old\n"), []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new\n" {
		t.Errorf("%q, %v", data, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, %v", info.Mode(), err)
	}
	if err := ReplaceFile(path, []byte("old\n"), []byte("newer\n")); !errors.Is(err, ErrChangedMeanwhile) {
		t.Errorf("changed meanwhile: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new\n" {
		t.Errorf("written anyway: %q", data)
	}
	if names, _ := os.ReadDir(dir); len(names) != 2 {
		t.Errorf("left behind: %v", names)
	}
}

// disksBelow follows partitions to their disk, and device-mapper or md devices to the disks below them.
func TestDisksBelow(t *testing.T) {
	sys := t.TempDir()
	mk := func(path string) string {
		t.Helper()
		full := filepath.Join(sys, path)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	touch := func(path string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sys, path), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, at string) {
		t.Helper()
		if err := os.Symlink(filepath.Join(sys, target), filepath.Join(sys, at)); err != nil {
			t.Fatal(err)
		}
	}
	sda1 := mk("block/sda/sda1")
	touch("block/sda/sda1/partition")
	mk("block/sdb/sdb2")
	touch("block/sdb/sdb2/partition")
	loop := mk("block/loop3")
	dm := mk("virtual/block/dm-0/slaves")
	link("block/sda/sda1", "virtual/block/dm-0/slaves/sda1")
	link("block/sdb/sdb2", "virtual/block/dm-0/slaves/sdb2")
	md := mk("virtual/block/md0/slaves")
	link("virtual/block/dm-0", "virtual/block/md0/slaves/dm-0")
	link("block/sda/sda1", "virtual/block/md0/slaves/sda1")
	for dir, want := range map[string][]string{
		sda1: {"sda"}, loop: {"loop3"}, filepath.Dir(dm): {"sda", "sdb"}, filepath.Dir(md): {"sda", "sdb"},
	} {
		got, err := disksBelow(dir, 0)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v, %v", dir, got, err)
		}
	}
}
