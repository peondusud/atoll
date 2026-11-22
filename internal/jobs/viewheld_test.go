package jobs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// treeOf gives a view's tree of the bench's pool, over the same catalog and disks.
func (b *bench) treeOf() *view.Tree {
	b.t.Helper()
	roots := view.NewDiskRoots()
	b.t.Cleanup(roots.Close)
	for i := range b.config.Disks {
		d := &b.config.Disks[i]
		root, err := b.OpenRoot(d.Mount, d.Root)
		if err != nil {
			b.t.Fatal(err)
		}
		roots.Set(d.ID, root)
	}
	return view.NewTree(b.db, roots, view.Owner{}, b.now).WithCache(true)
}

// openThroughView opens a path to write through the view, as the kernel would after its lookup.
func (b *bench) openThroughView(w *writer.Writer, tree *view.Tree, path string) (*writer.File, error) {
	b.t.Helper()
	n, err := tree.Lookup(b.t.Context(), path)
	if err != nil || n == nil {
		b.t.Fatalf("the view presents %+v at %s (%v)", n, path, err)
	}
	return w.Open(b.t.Context(), path, n, viewer)
}

// A program that opens a file to write through the view while the SSD cache's transfer moves its copy on the cache to
// the trash waits for that move, then writes the copy the view presents once it is over, on a data disk: what it
// writes is never in the copy the transfer purges (specs.md §19, §21.2; conformance/models/README.md, flaw 10, the
// maintainer's remedy). It once wrote into that copy, gone once it closed it.
func TestAWriteOpenedThroughTheViewDuringATransferWaitsForIt(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.write("c", "show/e1.mkv", "episode")
	b.write("t1", "show/e1.mkv", "episode")
	b.write("t2", "show/e1.mkv", "episode")
	b.scan(true)
	w, tree := b.viewOf(), b.treeOf()
	opened := make(chan struct{})
	var f *writer.File
	var openErr error
	b.at["journaled"] = func() {
		// Right after the transfer journaled its move: a program opens the file to write, which waits.
		n, err := tree.Lookup(t.Context(), "show/e1.mkv")
		if err != nil || n == nil || n.Disk != "c" {
			t.Errorf("the view presents %+v (%v)", n, err)
			close(opened)
			return
		}
		go func() {
			defer close(opened)
			f, openErr = w.Open(t.Context(), "show/e1.mkv", n, viewer)
		}()
		time.Sleep(50 * time.Millisecond)
	}
	out := b.pass()
	<-opened
	if openErr != nil || f == nil {
		t.Fatalf("open to write: %v, the transfer %s", openErr, outcomeOf(out, KindTransfer, "show/e1.mkv"))
	}
	if f.Disk == "c" {
		t.Errorf("the open wrote the copy on the cache, which the transfer %s", outcomeOf(out, KindTransfer, "show/e1.mkv"))
	}
	const better = "a better release"
	if _, err := f.WriteAt([]byte(better), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	at := b.contentAnywhere(better)
	if len(at) != 1 || !strings.Contains(at[0], filepath.ToSlash("media/show/e1.mkv")) {
		t.Errorf("what the program wrote is at %v, not in the library", at)
	}
}

// The transfer reads the view's operations again in the transaction that journals its move: a file opened to write
// through the view after the pass read them, as the transfer of another file ran, is not moved under the program, and
// waits (flaw 10).
func TestATransferWaitsForAWriteOpenedSinceThePassReadTheView(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	for _, p := range []string{"a.mkv", "b.mkv"} {
		b.write("c", p, "episode")
		b.write("t1", p, "episode")
		b.write("t2", p, "episode")
	}
	b.scan(true)
	w, tree := b.viewOf(), b.treeOf()
	var f *writer.File
	b.at["journaled"] = func() {
		// The transfer of a.mkv journaled: b.mkv is opened to write, its move not planned yet.
		var err error
		if f, err = b.openThroughView(w, tree, "b.mkv"); err != nil {
			t.Errorf("open to write: %v", err)
		}
	}
	out := b.pass()
	if f == nil {
		t.Fatal("nothing opened")
	}
	if got := outcomeOf(out, KindTransfer, "b.mkv"); got == OutcomeTransferred || !b.exists("c", "media/b.mkv") {
		t.Errorf("b.mkv went from the cache (%q) while the view held it open to write", got)
	}
	const better = "a better release"
	if _, err := f.WriteAt([]byte(better), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if at := b.contentAnywhere(better); len(at) != 1 || at[0] != "c:media/b.mkv" {
		t.Errorf("what the program wrote is at %v", at)
	}
	if got := b.text("SELECT state FROM view_ops ORDER BY id DESC LIMIT 1"); got != "done" {
		t.Errorf("the write's row is %s once closed", got)
	}
}

// A file the view holds open to write is never taken in, whatever the catalog presents meanwhile, and its transfer
// waits, pass after pass, until the program closes it: a write it makes late, as a program that keeps a file open for
// days may, reaches the library (flaw 10).
func TestAFileTheViewHoldsOpenIsNeverTransferred(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.scan(false)
	w := b.viewOf()
	f, _, err := w.Create(t.Context(), "new.mkv", 0o644, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if f.Disk != "c" {
		t.Fatalf("the new file landed on %s", f.Disk)
	}
	if _, err := f.WriteAt([]byte("episode"), 0); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		b.pass()
		b.now += 60_000_000_000
	}
	if got := b.text("SELECT state || ':' || (taken_ns IS NULL) FROM view_ops WHERE kind = 'create'"); got != "open:1" {
		t.Errorf("the creation's row is %s while the file is open", got)
	}
	if !b.exists("c", "media/new.mkv") {
		t.Fatal("the file left the cache while the view held it open")
	}
	const late = "a late write"
	if _, err := f.WriteAt([]byte(late), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for range 4 {
		b.pass()
		b.now += 60_000_000_000
	}
	if at := b.contentAnywhere(late); len(at) == 0 {
		t.Errorf("the late write is nowhere")
	}
	if b.exists("c", "media/new.mkv") {
		t.Errorf("once closed and taken in, the file stays on the cache: %v", b.contentAnywhere(late))
	}
}
