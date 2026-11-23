//go:build flaws

package writer

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Flaw 17 of conformance/models/README.md (ViewMove.tla, FlawMoveTargetGone.cfg, NoUserDataLost): the move of a file
// whose write ran out of space sends the copy left on the full disk to its trash, compared with what was copied, then
// purges it at once (specs.md §21.2, step 8), looking at nothing on the target. A user or a program that deletes the
// moved copy on the target directly, once the handle is its and before that purge, as flaw 8's transfer did of its
// data copies, loses the file: the copy left, its last content, is purged, where a deletion made directly on one disk
// reaches the other copies only in their trash (invariant 10).
func TestFlawAMoveOutOfSpacePurgesTheLastCopyOfAFileDeletedMeanwhile(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	moved := filepath.Join(b.mount["t2"], "media", "Films", "new.mkv")
	once := false
	b.full("t1", "new.mkv", syscall.ENOSPC)
	write := b.machine.failOf(b.mount["t1"])
	b.machine.setFail(b.mount["t1"], func(call, name string) error {
		if call == "rename" && !once {
			// The copy left goes to t1's trash: right before, the moved copy on t2 is deleted directly.
			once = true
			if err := os.Remove(moved); err != nil {
				t.Error(err)
			}
		}
		return write(call, name)
	})
	_, _ = f.WriteAt([]byte(second), int64(len(first)))
	_ = f.Close()
	if !once {
		t.Fatal("the copy left never went to the trash")
	}
	held := b.on("t1", "Films/new.mkv") == first
	for _, data := range b.trashed("t1") {
		held = held || data == first
	}
	if !held {
		t.Errorf("the file's content is on no disk and in no trash: t1 %q, t2 %q, t1's trash %v",
			b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"), b.trashed("t1"))
	}
}

// Flaw 15 of conformance/models/README.md (ViewMove.tla, FlawMoveRecheckOpen.cfg, ViewPresentsLatest): a move of a
// file whose write ran out of space, stopped at step 5 by a target that cannot be relied on once the part was at its
// name there, stays planned, and the file stays open on the full disk, where the program goes on writing (specs.md
// §21.2, "What a failure leaves"). The view's check every 30 s, once that disk is back, resolves the move as a start
// would: the full disk's copy, of the size and time the target's took, goes to its trash, the move done, the view
// presenting the target's copy, while the program still holds the full disk's open: what it writes then, in place,
// goes to the copy in the trash, and the view presents the file without it.
func TestFlawARecheckMovesAFileTheProgramHoldsOpen(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	// The target's sync of the file at its name fails: the rename went through, the move stays planned.
	b.machine.setFail(b.mount["t2"], func(call, name string) error {
		if call == "syncentry" && name == "new.mkv" {
			return syscall.EIO
		}
		return nil
	})
	b.full("t1", "new.mkv", syscall.ENOSPC)
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err == nil {
		t.Fatal("the write that ran out of space went through")
	}
	if f.OnDisk() != "t1" {
		t.Fatalf("the file moved to %s", f.OnDisk())
	}
	if got := b.moves(); len(got) != 1 || !strings.HasSuffix(got[0], ":planned:create:planned") {
		t.Fatalf("the moves: %v", got)
	}
	// The target is back, and the full disk takes a write in place, which needs no block.
	b.machine.setFail(b.mount["t2"], nil)
	b.machine.setFail(b.mount["t1"], nil)
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	const header = "HEADER"
	if _, err := f.WriteAt([]byte(header), 0); err != nil {
		t.Fatalf("the program's write in place: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	n := b.lookup("Films/new.mkv")
	if n == nil {
		t.Fatal("the view presents nothing at the path")
	}
	if got := b.on(n.Disk, "Films/new.mkv"); !strings.HasPrefix(got, header) {
		t.Errorf("the view presents the file from %s, holding %q: the program's write is not there (the trash of t1 holds %v)",
			n.Disk, got, b.trashed("t1"))
	}
}
