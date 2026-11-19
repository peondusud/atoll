package jobs

import (
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// A disk a copy loses is lost at once for what starts, and for the session, its handle closed, only once no copy uses it
// any more: a handle closed under a copy could lead its next call to another file that took its number (specs.md §14).
func TestTheSessionLetsGoOfADiskOnceNoCopyUsesIt(t *testing.T) {
	t.Parallel()
	p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
	p.fill("t1", 1)
	session, err := trash.Open(t.Context(), p.db, p.config, p, true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	shared := newSharedSession(session)
	handle, err := shared.Handle(t.Context(), "t2")
	if err != nil || handle == nil {
		t.Fatalf("%v, %v", handle, err)
	}
	shared.use("t1", "t2")
	shared.use("t2")
	if err := shared.Lose(t.Context(), "t2", syscall.EIO); err != nil {
		t.Fatal(err)
	}
	if shared.Writable("t2") || shared.Readable("t2") || !shared.halted("t2") {
		t.Error("a disk lost still starts copies")
	}
	if h, err := shared.Handle(t.Context(), "t2"); h != nil || err != nil {
		t.Errorf("a disk lost gives a handle: %v, %v", h, err)
	}
	if !session.Readable("t2") || p.text("SELECT state FROM disks WHERE id = 't2'") == "missing" {
		t.Error("the session let go of a disk two copies still use")
	}
	if err := shared.release(t.Context(), "t1", "t2"); err != nil {
		t.Fatal(err)
	}
	if !session.Readable("t2") {
		t.Error("the session let go of a disk a copy still uses")
	}
	if err := shared.release(t.Context(), "t2"); err != nil {
		t.Fatal(err)
	}
	if session.Readable("t2") || p.text("SELECT state FROM disks WHERE id = 't2'") != "missing" {
		t.Error("the session kept a disk lost once no copy used it")
	}
	if !shared.Readable("t1") || shared.halted("t1") {
		t.Error("the other disk was lost too")
	}
}
