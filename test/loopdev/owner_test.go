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

func ownerOf(t *testing.T, path string) (uid, gid int64) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	return int64(sys.Uid), int64(sys.Gid)
}

// chownKeep gives the copy on the ext4 disk an owner that neither it nor the copy on the driver holds, which moves its
// change time, and says that owner's id, its user and group alike.
func chownKeep(t *testing.T, b *passBench, held int64) int64 {
	t.Helper()
	full := filepath.Join(b.keep, "media", "big")
	before, _ := ownerOf(t, full)
	for _, id := range []int64{4242, 4243, 4244} {
		if id == held || id == before {
			continue
		}
		if err := os.Chown(full, int(id), int(id)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Fatal("no owner left")
	return 0
}

// TestAPassAppliesAnOwnerOnEachDriver: a chown on the copy on ext4 reaches the copy on every driver, which applies it
// or keeps its own: the row says what the disk holds, read through a cold cache, and the next pass has nothing left
// to apply, whatever the driver did (specs.md §14, §15.4).
func TestAPassAppliesAnOwnerOnEachDriver(t *testing.T) {
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
			heldUID, _ := ownerOf(t, copyPath)
			id := chownKeep(t, b, heldUID)
			b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
			out := b.pass()
			if len(out.Operations) != 1 || out.Operations[0].Kind != jobs.KindChown ||
				!slices.Contains([]string{jobs.OutcomeApplied, jobs.OutcomeKept}, out.Operations[0].Outcome) ||
				!slices.Equal(out.Decisions, []jobs.Decision{{Path: "big", Kind: "metadata"}}) || out.Partial {
				t.Errorf("%+v, %+v, partial %v", out.Decisions, out.Operations, out.Partial)
			}
			dropCaches(t)
			uid, gid := ownerOf(t, copyPath)
			t.Logf("%s holds %d:%d for %d:%d (%v)", tc.name, uid, gid, id, id, out.Operations)
			if got := b.text("SELECT uid || ':' || gid FROM copies WHERE disk_id = 'v'"); got != strconv.FormatInt(uid, 10)+":"+strconv.FormatInt(gid, 10) {
				t.Errorf("the row says %s, the disk holds %d:%d", got, uid, gid)
			}
			if got := b.text("SELECT uid || ':' || gid FROM copies WHERE disk_id = 'k'"); got != strconv.FormatInt(id, 10)+":"+strconv.FormatInt(id, 10) {
				t.Errorf("the changed copy's row says %s", got)
			}
			if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
				t.Errorf("then %+v, %+v", out.Decisions, out.Operations)
			}
		})
	}
}

// TestARepriseOfAnOwnerOnEachDriver: a pass killed once an owner is set on a copy, before that is recorded. Whatever
// the driver kept, the reprise and at most one more pass leave the row saying what the disk holds, with no warning,
// and nothing left to apply (specs.md §17.2).
func TestARepriseOfAnOwnerOnEachDriver(t *testing.T) {
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
			heldUID, _ := ownerOf(t, copyPath)
			chownKeep(t, b, heldUID)
			b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
			b.crashAt("owner-set")
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
			uid, gid := ownerOf(t, copyPath)
			if got := b.text("SELECT uid || ':' || gid FROM copies WHERE disk_id = 'v'"); got != strconv.FormatInt(uid, 10)+":"+strconv.FormatInt(gid, 10) {
				t.Errorf("the row says %s, the disk holds %d:%d", got, uid, gid)
			}
		})
	}
}
