//go:build loopdev

package loopdev

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// TestAFailedMountIsAMissingDisk: a disk whose mount failed leaves the folder of its mount path, where a service may
// make the root folder. A scan and the passes after it, past the debounce of a deletion, find that disk missing,
// never empty: nothing of it is decided gone, nothing goes to a trash, and nothing is written to that folder. Mounted
// again, it holds what it held, and a pass has nothing to do (CLAUDE.md, invariant 4; specs.md §10, §15.10). The
// bench sets the mass-deletion guard aside, so that a disk read as empty would be decided, not held.
func TestAFailedMountIsAMissingDisk(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			when := time.Unix(1500000000, 0)
			writeBytes(t, b.keep, "big", threeChunks(), when)
			writeBytes(t, b.keep, "Show/S01/e01.mkv", []byte("episode"), when)
			b.scan(false)
			if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{}, func(*jobs.ReplicateItem) error { return nil }); err != nil {
				t.Fatal(err)
			}
			b.pass()
			if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok'"); got != "2" {
				t.Fatalf("%s copies on the driver", got)
			}

			// The mount fails: what is left at the mount path is the folder below it, where a service that did not
			// notice, as a media server, makes its folder and writes. Only the mount check now tells it from the disk:
			// the root folder alone would pass.
			must(t, "umount", at)
			time.Sleep(500 * time.Millisecond)
			if entries, err := os.ReadDir(at); err != nil || len(entries) != 0 {
				t.Fatalf("the mount path after umount: %v, %v", entries, err)
			}
			stray := filepath.Join(at, "media", "stray")
			if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stray, []byte("a service's"), 0o644); err != nil {
				t.Fatal(err)
			}
			b.scan(false)
			// Past the debounce of a deletion, then a day past that: a disk read as empty would be decided by then.
			base := b.machine
			b.machine = &later{Machine: base, by: time.Hour}
			out := b.pass()
			b.machine = &later{Machine: base, by: 48 * time.Hour}
			again := b.pass()
			for _, o := range []*jobs.Outcome{out, again} {
				if len(o.Decisions)+len(o.Operations) != 0 {
					t.Errorf("decided %+v, ran %+v", o.Decisions, o.Operations)
				}
				if len(o.Disks) != 2 || o.Disks[1].ID != "v" || o.Disks[1].Verdict.State != disks.Missing || o.Disks[1].Verdict.Reason != disks.NotMounted || o.Disks[1].Walked {
					t.Errorf("the driver's disk: %+v", o.Disks)
				}
			}
			if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok' AND gone_kind IS NULL"); got != "2" {
				t.Errorf("%s copies left as they were", got)
			}
			if got := b.text("SELECT count(*) FROM events WHERE kind IN ('copy-gone', 'trash', 'guard')") + b.text("SELECT count(*) FROM jobs") + b.text("SELECT count(*) FROM trash"); got != "000" {
				t.Errorf("events, jobs and trash: %s", got)
			}
			top, err := os.ReadDir(at)
			below, err2 := os.ReadDir(filepath.Join(at, "media"))
			if err != nil || err2 != nil || len(top) != 1 || len(below) != 1 || below[0].Name() != "stray" {
				t.Errorf("written to the mount path: %v %v, %v %v", top, err, below, err2)
			}
			if got, err := os.ReadFile(stray); err != nil || string(got) != "a service's" {
				t.Errorf("the service's file: %q, %v", got, err)
			}

			// Mounted again, over what the service left, it holds what it held.
			if err := os.RemoveAll(filepath.Join(at, "media")); err != nil {
				t.Fatal(err)
			}
			mountVolume(t, v, tc.driver, at, options...)
			b.machine = &later{Machine: base, by: 49 * time.Hour}
			out = b.pass()
			if len(out.Decisions)+len(out.Operations) != 0 || out.Partial {
				t.Errorf("back: %+v, %+v, partial %v", out.Decisions, out.Operations, out.Partial)
			}
			for path, want := range map[string][]byte{"big": threeChunks(), "Show/S01/e01.mkv": []byte("episode")} {
				got, err := os.ReadFile(filepath.Join(at, "media", path))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s back: %d bytes, %v", path, len(got), err)
				}
			}
		})
	}
}
