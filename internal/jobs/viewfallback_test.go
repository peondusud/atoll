package jobs

import (
	"io"
	"testing"

	"github.com/peondusud/atoll/internal/view"
)

// Atoll's own view presents a file from its original (specs.md §7, §21.2), and reads it there alone with
// read.policy = "oldest". The SSD cache's transfer moves the copy on the cache, the file's original, to the trash, then
// syncs, then commits its row's removal (§17.1): between the move and the commit an open through the view opens the next
// copy in sync, in the order of §7, where it once found nothing (invariant 14; conformance/models/README.md, flaw 9, the
// maintainer's remedy).
func TestTheViewOpensTheNextCopyWhileATransferMovesTheOriginal(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	const content = "episode"
	b.write("c", "show/e1.mkv", content)
	b.write("t1", "show/e1.mkv", content)
	b.write("t2", "show/e1.mkv", content)
	b.scan(true)
	roots := view.NewDiskRoots()
	t.Cleanup(roots.Close)
	for i := range b.config.Disks {
		d := &b.config.Disks[i]
		root, err := b.OpenRoot(d.Mount, d.Root)
		if err != nil {
			t.Fatal(err)
		}
		roots.Set(d.ID, root)
	}
	tree := view.NewTree(b.db, roots, view.Owner{}, b.now).WithCache(true)
	if n, err := tree.Lookup(t.Context(), "show/e1.mkv"); err != nil || n == nil || n.Disk != "c" {
		t.Fatalf("the view presents %+v (%v), not the copy on the cache", n, err)
	}
	read := "nothing"
	looked := false
	b.at["moved"] = func() {
		// Right after the transfer's move of the copy on the cache, before its syncs and its commit.
		looked = true
		n, err := tree.Lookup(t.Context(), "show/e1.mkv")
		if err != nil || n == nil {
			read = "no node"
			return
		}
		f, err := tree.Open(t.Context(), "show/e1.mkv", n)
		if err != nil {
			read = "an open from " + n.Disk + ": " + err.Error()
			return
		}
		defer func() { _ = f.Close() }()
		data, _ := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
		read = string(data)
	}
	out := b.pass()
	if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); !looked || got != OutcomeTransferred {
		t.Fatalf("transfer %q, looked %v", got, looked)
	}
	if read != content {
		t.Errorf("while the transfer moved the original, the view read %q of a file the data disks hold in sync", read)
	}
}
