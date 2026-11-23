package writer

import (
	"math"
	"syscall"
	"testing"
)

// everything is a check that finds every disk back on a mount of its own and confirmed: whatever was journaled may be
// resolved.
func everything(string) (int64, bool) { return math.MaxInt64, true }

// plannedDeletion leaves a deletion through the view planned on the disk that presents Films/a.mkv, as a disk that
// cannot be relied on leaves it, and gives that disk and the time the deletion was journaled.
func plannedDeletion(t *testing.T, b *bench) (string, int64) {
	t.Helper()
	const path = "Films/a.mkv"
	disk := b.lookup(path).Disk
	b.machine.setFail(b.mount[disk], func(call, _ string) error {
		if call == "rename" {
			return syscall.EIO
		}
		return nil
	})
	if err := b.w.Unlink(t.Context(), path, root); err == nil {
		t.Fatal("the deletion went through a disk that fails")
	}
	b.machine.setFail(b.mount[disk], nil)
	if b.planned() != 1 {
		t.Fatalf("%d operations planned, want the deletion's", b.planned())
	}
	var at int64
	if err := b.w.db.QueryRowContext(t.Context(), "SELECT at_ns FROM view_ops WHERE kind = 'unlink'").Scan(&at); err != nil {
		t.Fatal(err)
	}
	return disk, at
}

// What the view's check every 30 s may resolve of what a disk lost left planned (the maintainer, 2026-11-23; specs.md
// §21.2): nothing of a disk its check did not find back on a mount of its own, confirmed past the page cache, as a
// disk whose device fails while it stays mounted, its answers from the cache; nothing journaled since the check that
// first found the mount the disk is on now, as the operation then failed on that very mount; what was journaled before
// it, once the disk is back.
func TestARecheckResolvesOnlyADiskBackOnAMountOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready func(disk string, at int64) Ready
		want  int // operations planned once the check ran
	}{
		{"not back: never found lost, or failing a confirm", func(string, int64) Ready {
			return func(string) (int64, bool) { return 0, false }
		}, 1},
		{"back, but journaled on that mount", func(disk string, at int64) Ready {
			return func(d string) (int64, bool) { return at, d == disk }
		}, 1},
		{"another disk back", func(disk string, at int64) Ready {
			return func(d string) (int64, bool) { return at + 1, d != disk }
		}, 1},
		{"back on a mount of its own, confirmed", func(disk string, at int64) Ready {
			return func(d string) (int64, bool) { return at + 1, d == disk }
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "ext4")
			disk, at := plannedDeletion(t, b)
			asked := map[string]int{}
			ready := tc.ready(disk, at)
			if err := b.w.Recheck(t.Context(), func(d string) (int64, bool) {
				asked[d]++
				return ready(d)
			}); err != nil {
				t.Fatal(err)
			}
			if got := b.planned(); got != tc.want {
				t.Errorf("%d operations planned once the view's check ran, want %d", got, tc.want)
			}
			if asked[disk] > 1 {
				t.Errorf("the check was asked of %s %d times in one recheck", disk, asked[disk])
			}
			if got := b.on(disk, "Films/a.mkv"); got == "absent" {
				t.Error("the copy left its name, though nothing moved")
			}
		})
	}
}
