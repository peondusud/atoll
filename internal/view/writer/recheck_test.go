package writer

import (
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// A deletion through the view stopped by a disk that cannot be relied on stays planned (specs.md §17.1); once the
// view's check every 30 s finds that disk there again, what it left planned is resolved, while the view runs, as its
// start would (specs.md §21.2). It once waited for the next start of atoll mount, the daemon's walks waiting at that
// path meanwhile (conformance/models/README.md, flaw 5).
func TestARecheckResolvesWhatADiskLostLeftPlanned(t *testing.T) {
	b := newBench(t, "ext4")
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
	if b.planned() != 1 {
		t.Fatalf("%d operations planned, want the deletion's", b.planned())
	}
	b.machine.setFail(b.mount[disk], nil)
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned once the disk is back", b.planned())
	}
	if got := b.on(disk, path); got == "absent" {
		t.Error("the copy left its name, though nothing moved")
	}
	open, err := catalog.OpenViewOps(t.Context(), b.w.db)
	if err != nil || len(open) != 0 {
		t.Errorf("the view's journal holds %+v, %v", open, err)
	}
}

// A part the view still writes, or closes, is its writer's: a check while the view runs leaves it as it is, where a
// start drops it (specs.md §21.2), and once the program closes the file it is handed over as ever.
func TestARecheckLeavesAPartBeingWritten(t *testing.T) {
	b := parallelBench(t, "", 400)
	f := b.writeNew("Films/new.mkv", "new file")
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:planned:planned" {
		t.Errorf("the part after a check: %v", got)
	}
	_ = f.Close()
	b.w.Wait()
	if err := b.w.Recheck(t.Context(), everything); err != nil {
		t.Fatal(err)
	}
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:done:planned" {
		t.Errorf("the part handed over: %v", got)
	}
	if parts := b.parts("t2"); len(parts) != 1 {
		t.Errorf("tmp/ of t2 holds %v", parts)
	}
}
