//go:build loopdev

package loopdev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type corpusName struct {
	Name    *string  `json:"name"`
	NameB64 *string  `json:"name_b64"`
	Ext4    []string `json:"ext4"`
	XFS     []string `json:"xfs"`
	Btrfs   []string `json:"btrfs"`
	NTFS    []string `json:"ntfs"`
	Note    string   `json:"note"`
}

// mounter mounts the image img on dir, or returns an error.
type mounter func(img, dir string) error

// TestNameCorpus creates every name of conformance/corpus/names/ that the
// corpus says a filesystem accepts, as a file and as a folder, on a real
// filesystem in a loop image. Each name gets a fresh folder, which must list
// it back byte for byte, and lstat must find it (specs.md §15.8, §29.4).
//
// On ext4, XFS and btrfs, whose only rule is the length, a refused name must
// also fail. On NTFS the Windows rules are stricter than every Linux driver,
// so only accepted names are checked, on each driver this machine has (ntfs3,
// ntfs-3g, lowntfs-3g), with and without windows_names.
func TestNameCorpus(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: sudo make test-loopdev")
	}
	cases := vectors.LoadCorpus[corpusName](t, "names/names.json")
	loop := func(img, dir string) error { return try("mount", "-o", "loop", img, dir) }
	for _, fs := range []struct {
		name    string
		mb      int64
		mkfs    []string
		mount   mounter
		verdict func(corpusName) []string
		exact   bool
	}{
		{"ext4", 64, []string{"mkfs.ext4", "-q", "-F"}, loop, func(c corpusName) []string { return c.Ext4 }, true},
		{"xfs", 320, []string{"mkfs.xfs", "-q", "-f"}, loop, func(c corpusName) []string { return c.XFS }, true},
		{"btrfs", 128, []string{"mkfs.btrfs", "-q", "-f"}, loop, func(c corpusName) []string { return c.Btrfs }, true},
		{"ntfs3", 64, mkntfs, ntfs3(""), ntfsVerdict, false},
		{"ntfs3-windows_names", 64, mkntfs, ntfs3(",windows_names"), ntfsVerdict, false},
		{"ntfs-3g", 64, mkntfs, fuse("ntfs-3g", ""), ntfsVerdict, false},
		{"ntfs-3g-windows_names", 64, mkntfs, fuse("ntfs-3g", "windows_names"), ntfsVerdict, false},
		{"lowntfs-3g", 64, mkntfs, fuse("lowntfs-3g", ""), ntfsVerdict, false},
		{"lowntfs-3g-windows_names", 64, mkntfs, fuse("lowntfs-3g", "windows_names"), ntfsVerdict, false},
	} {
		t.Run(fs.name, func(t *testing.T) {
			dir := mountImage(t, fs.mb, fs.mkfs, fs.mount)
			checked, refused := 0, 0
			for i, c := range cases {
				name, err := vectors.Name(c.Name, c.NameB64)
				if err != nil {
					t.Fatal(err)
				}
				// A fresh folder per name, so what an earlier name left
				// behind, or a driver's rewrite of it, cannot collide.
				sub := filepath.Join(dir, fmt.Sprintf("case-%03d", i))
				if err := os.Mkdir(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				accepted := len(fs.verdict(c)) == 0
				err = createListRemove(sub, name)
				checked++
				if err != nil {
					refused++
				}
				switch {
				case accepted && err != nil:
					t.Errorf("%+q: the corpus accepts it, but %s refused it: %v", name, fs.name, err)
				case !accepted && fs.exact && err == nil:
					t.Errorf("%+q: the corpus refuses it (%v), but %s accepted it", name, fs.verdict(c), fs.name)
				}
			}
			t.Logf("%d names, %d refused by %s", checked, refused, fs.name)
		})
	}
}

var mkntfs = []string{"mkntfs", "-q", "-F", "-f"}

func ntfsVerdict(c corpusName) []string { return c.NTFS }

// ntfs3 mounts with the kernel driver, which needs iocharset=utf8 (§30).
func ntfs3(opts string) mounter {
	return func(img, dir string) error {
		return try("mount", "-t", "ntfs3", "-o", "loop,iocharset=utf8"+opts, img, dir)
	}
}

// fuse mounts with a FUSE driver, ntfs-3g or lowntfs-3g.
func fuse(driver, opts string) mounter {
	return func(img, dir string) error {
		if _, err := exec.LookPath(driver); err != nil {
			return err
		}
		if opts == "" {
			return try(driver, img, dir)
		}
		return try(driver, "-o", opts, img, dir)
	}
}

// createListRemove creates name in dir as a file, then as a folder. Each time
// dir must list it with the exact bytes and lstat must find it. It removes
// what it created, also when a check fails.
func createListRemove(dir, name string) error {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	err = errors.Join(f.Close(), found(dir, name), os.Remove(path))
	if err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return err
	}
	return errors.Join(found(dir, name), os.Remove(path))
}

// found checks that dir lists name with the exact bytes, and that lstat finds
// it under that name.
func found(dir, name string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if bytes.Equal([]byte(e.Name()), []byte(name)) {
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("listed, but lstat fails: %w", err)
			}
			return nil
		}
	}
	return errors.New("created, but not listed with the same bytes")
}

// mountImage makes a filesystem in a sparse image of mb MiB and mounts it. It
// skips the test when a tool or driver is missing.
func mountImage(t *testing.T, mb int64, mkfs []string, mount mounter) string {
	t.Helper()
	if _, err := exec.LookPath(mkfs[0]); err != nil {
		t.Skipf("%s not found", mkfs[0])
	}
	work := t.TempDir()
	img, dir := filepath.Join(work, "fs.img"), filepath.Join(work, "mnt")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(mb << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	run(t, append(mkfs, img)...)
	if err := mount(img, dir); err != nil {
		t.Skipf("cannot mount: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Unmount(dir, 0); err != nil {
			t.Errorf("unmount %s: %v", dir, err)
		}
	})
	return dir
}

func run(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// try runs a command and returns its error. Its process is not tied to the
// test's context, so a mount helper such as ntfs-3g keeps running after it.
func try(args ...string) error {
	return exec.CommandContext(context.Background(), args[0], args[1:]...).Run()
}
