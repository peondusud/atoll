package writer

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// A move of a file whose write ran out of space, stopped at step 5 by a target that cannot be relied on once the part
// was at its name there, stays planned, and the file stays open on the full disk, where the program goes on writing
// (specs.md §21.2, "What a failure leaves"). It waits for that file to be closed before the view's check every 30 s
// resolves it, as a part being written does (the maintainer, 2026-11-23; conformance/models/README.md, flaw 15): a check
// while the program holds the file open leaves it planned, the full disk's copy presented and written in place; once
// the file is closed, the next check resolves it, and the view presents what the program wrote last. Before, the check
// sent the full disk's copy, still open, to its trash, and the program's later writes went there, never presented.
func TestARecheckWaitsForAMovedFileToBeClosed(t *testing.T) {
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
	if got := b.moves(); len(got) != 1 || !strings.HasSuffix(got[0], ":planned:create:planned") {
		t.Errorf("the moves after a check while the file is open: %v", got)
	}
	if got := b.trashed("t1"); len(got) != 0 {
		t.Errorf("t1's trash holds %v while the file is open there", got)
	}
	// A write a clock tick later, so that the modification time it gives says it.
	time.Sleep(20 * time.Millisecond)
	const header = "HEADER"
	if _, err := f.WriteAt([]byte(header), 0); err != nil {
		t.Fatalf("the program's write in place: %v", err)
	}
	if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t1" || !strings.HasPrefix(b.on("t1", "Films/new.mkv"), header) {
		t.Errorf("while open, the view presents %+v, t1 holding %q", n, b.on("t1", "Films/new.mkv"))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	for _, m := range b.moves() {
		if strings.Contains(m, "planned") {
			t.Errorf("a move left planned once the file is closed: %v", m)
		}
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
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

// A move left planned once the file's handle was the target's, its copy left on the full disk to the view's reprise,
// waits likewise for the file to be closed before a check resolves it; then it is resolved as ever, the copy left in
// the full disk's trash, unpurged (specs.md §21.2).
func TestARecheckWaitsForAMovedFileToBeClosedAfterItsSwap(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	// The trash of the copy left fails as a disk lost fails, once the handle is the target's.
	b.machine.setFail(b.mount["t1"], func(call, name string) error {
		switch {
		case call == "write" && name == "new.mkv":
			return syscall.ENOSPC
		case call == "rename" && name == "new.mkv":
			return syscall.EIO
		}
		return nil
	})
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	if f.OnDisk() != "t2" {
		t.Fatalf("the file is on %s", f.OnDisk())
	}
	if got := b.moves(); len(got) != 1 || !strings.HasSuffix(got[0], ":planned:trash:planned") {
		t.Fatalf("the moves: %v", got)
	}
	b.machine.setFail(b.mount["t1"], nil)
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	if got := b.moves(); len(got) != 1 || !strings.HasSuffix(got[0], ":planned:trash:planned") {
		t.Errorf("the moves after a check while the file is open: %v", got)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	if got := b.moves(); len(got) != 1 || got[0] != "t1>t2:done:trash:done" {
		t.Errorf("the moves once the file is closed: %v", got)
	}
	if b.on("t2", "Films/new.mkv") != first+second || b.on("t1", "Films/new.mkv") != "absent" {
		t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	if got := b.trashed("t1"); len(got) != 1 {
		t.Errorf("t1's trash holds %v, want the copy left, unpurged", got)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
	}
}
