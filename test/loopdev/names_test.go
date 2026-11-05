//go:build loopdev

package loopdev

import (
	"bytes"
	"context"
	"errors"
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

// TestNameCorpus creates every name of conformance/corpus/names/ that the
// corpus says a filesystem accepts, as a file and as a folder, on a real
// filesystem in a loop image, and checks that the folder lists it back byte
// for byte (specs.md §15.8, §29.4). On ext4, XFS and btrfs, whose only rule
// is the length, a refused name must also fail. On NTFS the Windows rules are
// stricter than either Linux driver, so only accepted names are checked.
func TestNameCorpus(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: sudo make test-loopdev")
	}
	cases := vectors.LoadCorpus[corpusName](t, "names/names.json")
	for _, fs := range []struct {
		name    string
		mb      int64
		mkfs    []string
		verdict func(corpusName) []string
		exact   bool
	}{
		{"ext4", 64, []string{"mkfs.ext4", "-q", "-F"}, func(c corpusName) []string { return c.Ext4 }, true},
		{"xfs", 320, []string{"mkfs.xfs", "-q", "-f"}, func(c corpusName) []string { return c.XFS }, true},
		{"btrfs", 128, []string{"mkfs.btrfs", "-q", "-f"}, func(c corpusName) []string { return c.Btrfs }, true},
		{"ntfs", 64, []string{"mkntfs", "-q", "-F", "-f"}, func(c corpusName) []string { return c.NTFS }, false},
	} {
		t.Run(fs.name, func(t *testing.T) {
			dir := mountImage(t, fs.name, fs.mb, fs.mkfs)
			checked, refused := 0, 0
			for _, c := range cases {
				name, err := vectors.Name(c.Name, c.NameB64)
				if err != nil {
					t.Fatal(err)
				}
				accepted := len(fs.verdict(c)) == 0
				err = createListRemove(dir, name)
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

// createListRemove creates name in dir as a file, then as a folder, checks
// each time that dir lists it with the exact bytes, and removes it.
func createListRemove(dir, name string) error {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := listed(dir, name); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return err
	}
	if err := listed(dir, name); err != nil {
		return err
	}
	return os.Remove(path)
}

func listed(dir, name string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if bytes.Equal([]byte(e.Name()), []byte(name)) {
			return nil
		}
	}
	return errors.New("created, but not listed with the same bytes")
}

// mountImage makes a filesystem in a sparse image of mb MiB and mounts it
// through a loop device. NTFS is mounted with ntfs3 when the kernel has it,
// otherwise with ntfs-3g. It skips the test when a tool or driver is missing.
func mountImage(t *testing.T, fs string, mb int64, mkfs []string) string {
	t.Helper()
	if _, err := exec.LookPath(mkfs[0]); err != nil {
		t.Skipf("%s not found", mkfs[0])
	}
	work := t.TempDir()
	img, dir := filepath.Join(work, fs+".img"), filepath.Join(work, "mnt")
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
	driver := fs
	switch {
	case fs != "ntfs":
		if try("mount", "-o", "loop", img, dir) != nil {
			t.Skipf("cannot mount %s", fs)
		}
	case try("mount", "-t", "ntfs3", "-o", "loop,iocharset=utf8", img, dir) == nil:
		driver = "ntfs3"
	case try("ntfs-3g", img, dir) == nil:
		driver = "ntfs-3g"
	default:
		t.Skip("cannot mount ntfs with ntfs3 or ntfs-3g")
	}
	t.Logf("%s mounted with %s", img, driver)
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
