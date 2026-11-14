//go:build loopdev

package loopdev

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
)

// TestADiskPutBackFromAnOlderImage: a disk put back from an image taken before a change holds its copies as they were
// then, with the change times they had then, earlier than what their rows say. That is no user's change, as any change
// moves the change time forward and no program sets it back: the file is a conflict, and the newer copy on the other
// disk stays as it is. It was decided as a modification, which sent the newer copy to the trash and copied the older
// one over it (specs.md §15.4, §15.6). Through a real pass, on each driver.
func TestADiskPutBackFromAnOlderImage(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "a", "first")
			}
			b.scan(true)
			b.pass()
			image := filepath.Join(t.TempDir(), "older.img")
			unmountFor(t, tc.driver, at)
			must(t, "dd", "if="+v.device, "of="+image, "bs=1M", "status=none")
			if err := remount(t, v, tc.driver, at, options); err != nil {
				t.Fatalf("mounted again: %v", err)
			}

			// A change made on that disk after the image: a user's, decided as one.
			write(t, at, "a", "second, longer")
			b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
			if out := b.pass(); !slices.Equal(out.Decisions, []jobs.Decision{{Path: "a", Kind: "modified"}}) || out.Partial {
				t.Fatalf("the user's change: %+v, %+v", out.Decisions, out.Operations)
			}
			if got := readText(t, b.keep, "a"); got != "second, longer" {
				t.Fatalf("the other copy holds %q", got)
			}

			unmountFor(t, tc.driver, at)
			must(t, "dd", "if="+image, "of="+v.device, "bs=1M", "conv=fsync", "status=none")
			must(t, "blockdev", "--flushbufs", v.device)
			if err := remount(t, v, tc.driver, at, options); err != nil {
				t.Skipf("the image did not mount: %v", err)
			}
			dropCaches(t)
			out := b.pass()
			if !slices.Equal(out.Decisions, []jobs.Decision{{Path: "a", Kind: "conflict"}}) || len(out.Operations) != 0 || out.Partial {
				t.Errorf("the disk put back: %+v, %+v, partial %v", out.Decisions, out.Operations, out.Partial)
			}
			if got := b.text("SELECT state FROM files"); got != "conflict" {
				t.Errorf("the file is %s", got)
			}
			if got := readText(t, b.keep, "a"); got != "second, longer" {
				t.Errorf("the newer copy holds %q", got)
			}
			if got := readText(t, at, "a"); got != "first" {
				t.Errorf("the older copy holds %q", got)
			}
			if got := b.text("SELECT count(*) FROM jobs"); got != "0" {
				t.Errorf("%s jobs", got)
			}
		})
	}
}

// unmountFor unmounts a volume whole, a FUSE driver's process included, so that what is on the device is all of it.
func unmountFor(t *testing.T, driver, at string) {
	t.Helper()
	must(t, "umount", at)
	if strings.HasSuffix(driver, "3g") {
		// The driver's process ends once the volume is let go; give it a moment to write what it holds.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if exec.CommandContext(context.Background(), "pgrep", "-f", driver+" .* "+at).Run() != nil {
				break
			}
		}
	}
	must(t, "sync")
}

func readText(t *testing.T, top, logical string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(top, "media", logical))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
