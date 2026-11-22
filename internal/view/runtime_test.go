package view

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// mergerfs's runtime branches are read and written only where the mount table says that the mount at the view's mount
// point is mergerfs's, and through its control file there, never a link (specs.md §21.1; docs/threat-model.md, G19).
// A folder that is no mount of mergerfs, holding a .mergerfs of its own or a link named so, is not mergerfs: nothing is
// read there and nothing written, though what it holds carries the attribute; nor is a link at the mount point.
func TestTheBranchesAreMergerfssAlone(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(elsewhere, branchesKey, []byte("/elsewhere=RW"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("no user. attributes here")
		}
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		make func(mount string) string
	}{
		{"a file named .mergerfs", func(mount string) string {
			control := filepath.Join(mount, ".mergerfs")
			if err := os.WriteFile(control, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(control, branchesKey, []byte("/here=RW"), 0); err != nil {
				t.Fatal(err)
			}
			return mount
		}},
		{"a link named .mergerfs", func(mount string) string {
			if err := os.Symlink(elsewhere, filepath.Join(mount, ".mergerfs")); err != nil {
				t.Fatal(err)
			}
			return mount
		}},
		{"a link at the mount point", func(mount string) string {
			if err := os.Symlink(filepath.Dir(elsewhere), filepath.Join(mount, "pool")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(filepath.Dir(elsewhere), ".mergerfs")); err != nil && !errors.Is(err, os.ErrExist) {
				t.Fatal(err)
			}
			return filepath.Join(mount, "pool")
		}},
	} {
		mount := tc.make(t.TempDir())
		if got, err := ReadBranches(mount); !errors.Is(err, ErrNoMergerfs) {
			t.Errorf("%s: read %q, %v, want no mergerfs", tc.name, got, err)
		}
		if err := WriteBranches(mount, "/written=RW"); !errors.Is(err, ErrNoMergerfs) {
			t.Errorf("%s: written, %v, want no mergerfs", tc.name, err)
		}
	}
	buf := make([]byte, 256)
	n, err := unix.Getxattr(elsewhere, branchesKey, buf)
	if err != nil || string(buf[:n]) != "/elsewhere=RW" {
		t.Errorf("what a link led to holds %q, %v", buf[:n], err)
	}
}
