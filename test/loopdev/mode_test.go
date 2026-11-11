//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
)

func modeOf(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int64(st.Sys().(*syscall.Stat_t).Mode & 0o7777)
}

// chmodKeep gives the copy on the ext4 disk a mode that neither it nor the copy on the driver holds, which moves its
// change time, and says that mode.
func chmodKeep(t *testing.T, b *passBench, held int64) os.FileMode {
	t.Helper()
	full := filepath.Join(b.keep, "media", "big")
	before := modeOf(t, full)
	for _, mode := range []os.FileMode{0o600, 0o604, 0o640} {
		if int64(mode) == held || int64(mode) == before {
			continue
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
		return mode
	}
	t.Fatal("no mode left")
	return 0
}

// TestAPassAppliesAModeOnEachDriver: a chmod on the copy on ext4 reaches the copy on every driver, which keeps of it
// what it can: the row says what the disk holds, read through a cold cache, and the next pass has nothing left to
// apply, whatever NTFS made of the mode (specs.md §14, §15.4).
func TestAPassAppliesAModeOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			writeBytes(t, b.keep, "big", threeChunks(), time.Unix(1500000000, 0))
			b.scan(false)
			if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{}, func(*jobs.ReplicateItem) error { return nil }); err != nil {
				t.Fatal(err)
			}
			b.pass()
			copyPath := filepath.Join(at, "media", "big")
			mode := chmodKeep(t, b, modeOf(t, copyPath))
			b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
			out := b.pass()
			want := []jobs.Operation{{Disk: "v", Kind: jobs.KindChmod, Path: "big", Outcome: jobs.OutcomeApplied}}
			if !slices.Equal(out.Decisions, []jobs.Decision{{Path: "big", Kind: "metadata"}}) || !slices.Equal(out.Operations, want) || out.Partial {
				t.Errorf("%+v, %+v, partial %v", out.Decisions, out.Operations, out.Partial)
			}
			dropCaches(t)
			held := modeOf(t, copyPath)
			t.Logf("%s holds %04o for %04o", tc.name, held, mode)
			if got := b.text("SELECT mode FROM copies WHERE disk_id = 'v'"); got != strconv.FormatInt(held, 10) {
				t.Errorf("the row says %s, the disk holds %d", got, held)
			}
			if got := b.text("SELECT mode FROM copies WHERE disk_id = 'k'"); got != strconv.FormatInt(int64(mode), 10) {
				t.Errorf("the changed copy's row says %s", got)
			}
			if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
				t.Errorf("then %+v, %+v", out.Decisions, out.Operations)
			}
		})
	}
}

// TestARepriseOfAModeOnEachDriver: a pass killed once a mode is set on a copy, before that is recorded. Whatever the
// driver kept, the reprise and at most one more pass leave the row saying what the disk holds, with no warning, and
// nothing left to apply (specs.md §17.2).
func TestARepriseOfAModeOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			writeBytes(t, b.keep, "big", threeChunks(), time.Unix(1500000000, 0))
			b.scan(false)
			if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{}, func(*jobs.ReplicateItem) error { return nil }); err != nil {
				t.Fatal(err)
			}
			b.pass()
			copyPath := filepath.Join(at, "media", "big")
			chmodKeep(t, b, modeOf(t, copyPath))
			b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
			b.crashAt("mode-set")
			b.pass()
			if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
				t.Errorf("then %+v, %+v", out.Decisions, out.Operations)
			}
			if got := b.text("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != "0" {
				t.Errorf("%s warnings", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations planned", got)
			}
			dropCaches(t)
			if got, held := b.text("SELECT mode FROM copies WHERE disk_id = 'v'"), modeOf(t, copyPath); got != strconv.FormatInt(held, 10) {
				t.Errorf("the row says %s, the disk holds %d", got, held)
			}
		})
	}
}
