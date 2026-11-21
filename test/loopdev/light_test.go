//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

func lineOf(o *jobs.Outcome, id string) jobs.DiskLine {
	at := slices.IndexFunc(o.Disks, func(l jobs.DiskLine) bool { return l.ID == id })
	if at < 0 {
		return jobs.DiskLine{}
	}
	return o.Disks[at]
}

// TestDaemonSparesTheDiskNothingChangedOn: once the first pass has walked both disks, a user's file on one calls for a
// pass that walks that disk alone, and copies the file to the other, which it opens for that copy only; the copy's
// own events, Atoll's, call for no pass. A user's file on the other then walks it, and spares the first (specs.md
// §15.13). On a driver watched by a filesystem mark, and on one watched folder by folder.
func TestDaemonSparesTheDiskNothingChangedOn(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{{"xfs", names.XFS, ""}, {"ntfs-3g", names.NTFS, "ntfs-3g"}} {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			passes, stop := runDaemon(t, b, filepath.Join(t.TempDir(), "fstab"))
			first := waitPass(t, passes, daemon.BecauseStart)
			if !lineOf(first.outcome, "k").Walked || !lineOf(first.outcome, "v").Walked {
				t.Fatalf("the first pass: %+v", first.outcome.Disks)
			}

			// From another process: what the test's own process writes, the watcher counts as the daemon's, Atoll's.
			must(t, "sh", "-c", "printf 'a film' > '"+filepath.Join(b.keep, "media", "on-k.mkv")+"'")
			p := waitPass(t, passes, daemon.BecauseEvents)
			if k, other := lineOf(p.outcome, "k"), lineOf(p.outcome, "v"); !k.Walked || other.Walked || !other.Spared {
				t.Fatalf("the pass for k's file (%v): %+v", p.because, p.outcome.Disks)
			}
			waitFor(t, "the copy on v", func() bool {
				data, err := os.ReadFile(filepath.Join(at, "media", "on-k.mkv"))
				return err == nil && string(data) == "a film"
			})
			// What Atoll wrote calls for no pass: the next one is the user's file on v.
			time.Sleep(2 * shortDebounce)
			must(t, "sh", "-c", "printf 'another' > '"+filepath.Join(at, "media", "on-v.mkv")+"'")
			for {
				p = waitPass(t, passes, daemon.BecauseEvents)
				if lineOf(p.outcome, "v").Walked {
					break
				}
				t.Errorf("a pass for events before v's file (%v): %+v", p.because, p.outcome.Disks)
			}
			if k := lineOf(p.outcome, "k"); k.Walked || !k.Spared {
				t.Errorf("the pass for v's file (%v): %+v", p.because, p.outcome.Disks)
			}
			waitFor(t, "the copy on k", func() bool {
				data, err := os.ReadFile(filepath.Join(b.keep, "media", "on-v.mkv"))
				return err == nil && string(data) == "another"
			})
			stop()
		})
	}
}
