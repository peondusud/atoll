package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/integrity"
)

// roomBench is a pool whose copy of a to t2 finds, once journaled, that another writer has left t2 with less free space
// than min_free, 5 % of it.
func roomBench(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	b.now += settleNS
	if b.spaces == nil {
		b.spaces = map[string][2]int64{}
	}
	mount := b.dirs["t2"]
	b.at["journaled"] = func() { b.spaces[mount] = [2]int64{1000, 40} }
	return b
}

// partsOf counts what tmp/ of a disk holds.
func (b *bench) partsOf(disk string) int {
	b.t.Helper()
	parts, _ := os.ReadDir(filepath.Join(b.dirs[disk], "Atoll", "tmp"))
	return len(parts)
}

// roomBack gives t2 its room back and says whether the next pass makes the copy, with nothing left in tmp/ and every
// operation ended.
func (b *bench) roomBack() {
	b.t.Helper()
	delete(b.spaces, b.dirs["t2"])
	b.fail = map[string]func(call, name string) error{}
	b.now += settleNS
	b.pass()
	if got := b.read("t2", "a"); got != "content" {
		b.t.Errorf("once there is room, the copy holds %q", got)
	}
	if n := b.partsOf("t2"); n != 0 {
		b.t.Errorf("tmp/ of t2 holds %d parts", n)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
		b.t.Errorf("%d operations still planned", n)
	}
}

// A copy checks min_free as it writes, as the view's parts do: what else wrote to its disk since the plan made it fit
// there, a part of the view, a user's file, may have taken the room the plan counted on, and the copy then stops before
// its disk is past min_free, no-room, its part gone, its job queued again (specs.md §14; conformance/models/README.md,
// flaw 12, the maintainer's remedy). It once wrote on until ENOSPC, past min_free.
func TestACopyStopsBeforeItTakesItsDiskPastMinFree(t *testing.T) {
	b := roomBench(t)
	out := b.pass()
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeNoRoom {
		t.Errorf("the copy says %q: %+v", got, out.Operations)
	}
	if b.exists("t2", "media/a") {
		t.Error("the copy is on t2")
	}
	if n := b.partsOf("t2"); n != 0 {
		t.Errorf("tmp/ of t2 holds %d parts", n)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'create' AND state = 'undone'"); n != 1 {
		t.Errorf("%d operations undone", n)
	}
	b.roomBack()
}

// A disk whose free space does not count a copy's writes until they are flushed, as btrfs's does (test/loopdev,
// TestACopyStopsAtMinFreeOnEachDriver, in qemu): the copy counts what it wrote itself since its first look, so that a
// file of four chunks with room for one and a half past min_free still stops, where the disk's word alone, which never
// moves here, let it write on past min_free.
func TestACopyCountsWhatItWroteWhereItsDiskDoesNotYet(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", strings.Repeat("x", 4*integrity.ChunkSize))
	b.scan(false)
	b.now += settleNS
	if b.spaces == nil {
		b.spaces = map[string][2]int64{}
	}
	mount := b.dirs["t2"]
	b.at["journaled"] = func() {
		// 100 MiB, min_free 5 % of it: 6 MiB past it, as the disk says it all the copy long.
		b.spaces[mount] = [2]int64{100 << 20, 5<<20 + 6<<20}
	}
	out := b.pass()
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeNoRoom {
		t.Errorf("the copy says %q: %+v", got, out.Operations)
	}
	if n := b.partsOf("t2"); n != 0 {
		t.Errorf("tmp/ of t2 holds %d parts", n)
	}
}

// A pass stopped, as by kill -9, as a copy that found no room past min_free drops its part, before the part goes and
// right after: the next pass ends the operation the copy left and makes the copy once there is room.
func TestACopyStoppedAsItDropsForMinFree(t *testing.T) {
	for _, gone := range []bool{false, true} {
		b := roomBench(t)
		stop := errors.New("stopped")
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "unlink" && name == "1.part" {
				if gone {
					_ = os.Remove(filepath.Join(b.dirs["t2"], "Atoll", "tmp", name))
				}
				panic(stop) //nolint:forbidigo // what stands for kill -9
			}
			return nil
		}
		func() {
			defer func() {
				if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
					t.Fatalf("it did not stop as the part goes (gone %v): %v", gone, err)
				}
			}()
			b.pass()
		}()
		if b.exists("t2", "media/a") {
			t.Errorf("the copy is on t2 (gone %v)", gone)
		}
		b.roomBack()
	}
}

// A copy that cannot read the free space of its disk as it writes goes on: ENOSPC still keeps the disk's capacity, and
// the plan made it fit there.
func TestACopyGoesOnWhenItsDiskSpaceCannotBeRead(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	b.now += settleNS
	b.at["journaled"] = func() { b.spaceErr = map[string]error{b.dirs["t2"]: syscall.EIO} }
	out := b.pass()
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeReplicated {
		t.Errorf("the copy says %q: %+v", got, out.Operations)
	}
	if got := b.read("t2", "a"); got != "content" {
		t.Errorf("the copy holds %q", got)
	}
}
