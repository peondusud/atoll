//go:build loopdev

package loopdev

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/names"
)

// cli runs the atoll binary the guest has with the bench's configuration, written beside its catalog, and gives its
// exit code and what it wrote.
func (b *passBench) cli(args ...string) (int, string) {
	b.t.Helper()
	if _, err := os.Stat(atollBinary); err != nil {
		b.t.Skipf("no %s: make qemu-build builds it, and test/qemu/run.sh puts it there", atollBinary)
	}
	file := filepath.Join(filepath.Dir(b.catalog), "atoll.toml")
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(file, []byte(b.configText), 0o600); err != nil {
			b.t.Fatal(err)
		}
	}
	out, err := exec.CommandContext(b.t.Context(), atollBinary, append([]string{"--config", file}, args...)...).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	case err != nil:
		b.t.Fatalf("atoll %s: %v", strings.Join(args, " "), err)
	}
	return 0, string(out)
}

func writeAt(t *testing.T, top, logical, text string) {
	t.Helper()
	full := filepath.Join(top, "media", filepath.FromSlash(logical))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestADiskRemovedOnEachDriver: atoll disk remove looks at the disk it takes out, on each driver (specs.md §25; flaw 29
// of conformance/models/README.md): the file x1 on k and v, v's copy the surplus one, an edit made on v directly keeps
// v, exit 1, as its look reads the edit there past the page cache. Once passes took the edit in, v drained, its copy
// made on k, v goes, and what it holds stays on it.
func TestADiskRemovedOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[defaults]\nreplicas = 0\nsettle = \"0s\"\n")
			for _, top := range []string{b.keep, b.at} {
				writeAt(t, top, "Films/a.mkv", "a film")
			}
			if code, out := b.cli("scan", "--full"); code != 0 {
				t.Fatalf("atoll scan --full: exit %d\n%s", code, out)
			}
			writeAt(t, b.at, "Films/a.mkv", "the user's edit")
			dropCaches(t)
			if code, out := b.cli("disk", "remove", "v", "--yes"); code != 1 || !strings.Contains(out, "the catalog does not hold") {
				t.Fatalf("disk remove v --yes, its edit not taken in: exit %d\n%s", code, out)
			}
			for range 2 {
				b.cli("daemon", "--until-idle")
			}
			if code, out := b.cli("disk", "remove", "v", "--evacuate"); code != 0 {
				t.Fatalf("disk remove v --evacuate: exit %d\n%s", code, out)
			}
			for range 3 {
				b.cli("daemon", "--until-idle")
			}
			if code, out := b.cli("disk", "remove", "v", "--yes"); code != 0 {
				t.Fatalf("disk remove v --yes, its edit taken in: exit %d\n%s", code, out)
			}
			for top, want := range map[string]string{b.keep: "the user's edit", b.at: "the user's edit"} {
				if data, err := os.ReadFile(filepath.Join(top, "media", "Films", "a.mkv")); err != nil || string(data) != want {
					t.Errorf("%s holds %q, %v", top, data, err)
				}
			}
		})
	}
}

// TestAFailedDiskReplacedAndRemoved: a disk not mounted is refused by atoll disk remove, which cannot look at it; once
// declared failed, atoll disk replace brings a spare in its place, a pass makes the file there, and the failed disk goes
// (specs.md §18, §25).
func TestAFailedDiskReplacedAndRemoved(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	// No settle: the copy on the spare, a new replica, goes at the next pass (specs.md §12.3).
	b := newPassBenchWith(t, v, at, "[defaults]\nreplicas = 1\nsettle = \"0s\"\n")
	for _, top := range []string{b.keep, b.at} {
		writeAt(t, top, "Films/a.mkv", "a film")
	}
	if code, out := b.cli("scan", "--full"); code != 0 {
		t.Fatalf("atoll scan --full: exit %d\n%s", code, out)
	}
	if err := exec.CommandContext(context.Background(), "umount", at).Run(); err != nil {
		t.Fatal(err)
	}
	if code, out := b.cli("disk", "remove", "v", "--yes"); code != 1 || !strings.Contains(out, "atoll disk fail v") {
		t.Fatalf("disk remove of a disk not mounted: exit %d\n%s", code, out)
	}
	if code, out := b.cli("disk", "fail", "v", "--yes"); code != 0 {
		t.Fatalf("disk fail v --yes: exit %d\n%s", code, out)
	}
	spare := mountPoint(t)
	mountVolume(t, newVolume(t, names.Ext4), "", spare)
	if err := os.MkdirAll(filepath.Join(spare, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := b.cli("disk", "replace", "v", "--with", spare, "--id", "s"); code != 0 {
		t.Fatalf("disk replace v --with the spare: exit %d\n%s", code, out)
	}
	for range 2 {
		b.cli("daemon", "--until-idle")
	}
	if code, out := b.cli("disk", "remove", "v", "--yes"); code != 0 {
		t.Fatalf("disk remove of the failed disk: exit %d\n%s", code, out)
	}
	if data, err := os.ReadFile(filepath.Join(spare, "media", "Films", "a.mkv")); err != nil || string(data) != "a film" {
		t.Errorf("the spare holds %q, %v", data, err)
	}
}
