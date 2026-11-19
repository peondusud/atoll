package jobs

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// viewOf gives the bench's pool a view that writes, over the same catalog and disks, as atoll mount serves it.
func (b *bench) viewOf() *writer.Writer {
	b.t.Helper()
	roots := view.NewDiskRoots()
	b.t.Cleanup(roots.Close)
	handles := writer.NewHandles()
	b.t.Cleanup(handles.Close)
	for i := range b.config.Disks {
		d := &b.config.Disks[i]
		root, err := b.OpenRoot(d.Mount, d.Root)
		if err != nil {
			b.t.Fatal(err)
		}
		roots.Set(d.ID, root)
		h, err := ops.Open(b, d, 0)
		if err != nil {
			b.t.Fatal(err)
		}
		handles.Set(d.ID, h)
	}
	settings, err := b.config.Cache()
	if err != nil {
		b.t.Fatal(err)
	}
	tree := view.NewTree(b.db, roots, view.Owner{}, b.now).WithCache(settings.InView)
	w, err := writer.New(b.db, b.config, b, tree, handles, slog.New(slog.DiscardHandler))
	if err != nil {
		b.t.Fatal(err)
	}
	return w
}

func (b *bench) openViewOps() []catalog.ViewOp {
	b.t.Helper()
	open, err := catalog.OpenViewOps(b.t.Context(), b.db)
	if err != nil {
		b.t.Fatal(err)
	}
	return open
}

// A file deleted through Atoll's own view (specs.md §15.5, §21.2): the copy it presents goes to the trash at once; the
// pass that follows sends the other copy there too, as a deletion, under deletes.direct = "restore" as well, as the user
// deleted the file and not one copy of it; then it takes the view's operation in.
func TestAPassTakesInADeletionThroughTheView(t *testing.T) {
	for _, direct := range []string{"propagate", "restore"} {
		t.Run(direct, func(t *testing.T) {
			b := newBenchWith(t, fmt.Sprintf("[deletes]\ndirect = %q\n", direct), "t1", "t2")
			b.write("t1", "Films/a.mkv", "x")
			b.write("t2", "Films/a.mkv", "x")
			b.scan(true)
			w := b.viewOf()
			if err := w.Unlink(t.Context(), "Films/a.mkv"); err != nil {
				t.Fatal(err)
			}
			left := 0
			for _, id := range []string{"t1", "t2"} {
				if b.exists(id, "media/Films/a.mkv") {
					left++
				}
			}
			if left != 1 {
				t.Fatalf("%d copies left once the view deleted the file, want the other one", left)
			}
			b.settle()
			for _, id := range []string{"t1", "t2"} {
				if b.exists(id, "media/Films/a.mkv") {
					t.Errorf("%s still holds the file", id)
				}
			}
			if got := b.count("SELECT count(*) FROM trash WHERE reason = 'deleted'"); got != 2 {
				t.Errorf("%d copies in the trash, want 2", got)
			}
			if got := b.count("SELECT count(*) FROM files"); got != 0 {
				t.Errorf("%d files left", got)
			}
			if open := b.openViewOps(); len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
		})
	}
}

// What the view made is taken in once the catalog presents it as the view does (specs.md §21.2): not before a pass
// recorded it, and then at once; and an empty folder the view removed goes from the disk where it was not empty yet.
func TestAPassTakesInWhatTheViewMade(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "Films/a.mkv", "x")
	b.write("t2", "Films/a.mkv", "x")
	b.scan(true)
	w := b.viewOf()
	f, _, err := w.Create(t.Context(), "Films/new.mkv", 0o644, writer.Owner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("new film"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if open := b.openViewOps(); len(open) != 1 {
		t.Fatalf("open: %+v", open)
	}
	b.settle()
	if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST('Films/new.mkv' AS BLOB)"); got != 1 {
		t.Fatalf("the pass did not record the new file: %d", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in once the catalog presents it: %+v", open)
	}

	// Sub/held.mkv is the catalog's on t2 alone, so the view presents Sub as t1's empty folder and t2's: removed where it
	// is empty, Sub stays on t2 until nothing is left there.
	b.write("t1", "Sub/.keep", "")
	b.remove("t1", "Sub/.keep")
	b.write("t2", "Sub/held.mkv", "h")
	b.scan(true)
	if err := os.Remove(filepath.Join(b.dirs["t2"], "media", "Sub", "held.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := w.Rmdir(t.Context(), "Sub"); err == nil {
		t.Fatal("removed while the catalog still presents what it holds")
	}
	b.settle()
	if err := w.Rmdir(t.Context(), "Sub"); err != nil {
		t.Fatal(err)
	}
	b.settle()
	for _, id := range []string{"t1", "t2"} {
		if b.exists(id, "media/Sub") {
			t.Errorf("%s still holds the folder removed", id)
		}
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
}
