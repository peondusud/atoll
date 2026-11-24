//go:build flaws

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// viewFolderPool makes two disks, each with the folder Shows/D and a file in it, scanned, and the time of the debounce
// gone by.
func viewFolderPool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Shows/D/e1.mkv", "the episode")
	}
	b.scan(true)
	b.now += 60_000_000_000
	return b
}

// passes runs n passes of the daemon, the debounce going by between them.
func (b *bench) passes(n int) {
	b.t.Helper()
	for range n {
		b.pass()
		b.now += 31_000_000_000
	}
}

func (b *bench) mkdir(id, path string) {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Join(b.dirs[id], "media", path), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

// Flaw 18 of conformance/models/README.md (ViewNames.tla, FlawViewReplaceRestore.cfg, liveness): a rename through
// Atoll's own view onto a name it presents replaces the copy there, which goes to the trash as `overwritten` (specs.md
// §21.2, writer.replace). With deletes.direct = "restore", the pass that follows finds that copy gone from its disk and,
// as jobs.readViewOps marks only a deletion of the view's as the view's, reads it as a user's deletion made directly
// on a disk: Kept, the copy to make again, onto the name that now holds the file renamed there, and the rename never
// reaches the other disk, where the copy replaced stays. §21.2 says that "a copy the view sent to the trash" is gone
// by the user, its deletion propagating "whatever deletes.direct says". The view's operation is then never taken in.
func TestFlawAReplacementThroughTheViewIsKeptOnTheOtherDisk(t *testing.T) {
	b := newBenchWith(t, "[deletes]\ndirect = \"restore\"\n", "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Films/a.mkv", "the film at a")
		b.write(id, "Films/b.mkv", "the film at b, older")
	}
	b.scan(true)
	b.now += 60_000_000_000
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Films/a.mkv", "Films/b.mkv", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.passes(6)
	for _, id := range []string{"t1", "t2"} {
		if got := b.readMedia(id, "Films/b.mkv"); got != "the film at a" {
			t.Errorf("%s holds %q at Films/b.mkv", id, got)
		}
		if b.exists(id, "media/Films/a.mkv") {
			t.Errorf("%s still holds Films/a.mkv", id)
		}
	}
	if got := b.count("SELECT count(*) FROM files WHERE state = 'conflict'"); got != 0 {
		t.Errorf("%d files in conflict", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("the replacement is never taken in: %+v", open)
	}
}

// Flaw 19 (ViewNames.tla, FlawViewFolderReprise.cfg, ViewJournalTrue): a folder renamed through the view is journaled
// with no inode (writer.renameFolder), and the reprise of one left planned calls it done when its first holder has
// any folder at the new name (writer.resolve). With the rename stopped before any disk renamed it, here every disk
// failing as one that cannot be relied on, and a user making a folder at the new name directly on that disk, the
// reprise calls it done though nothing moved: the view presents the folder at the new name, from the old one's rows,
// and nothing at the old name, where every disk still holds it (§21.2, The view's journal: "done only once what it
// names happened").
func TestFlawTheRepriseOfAFolderRenameTakesAUsersFolderForIt(t *testing.T) {
	b := viewFolderPool(t)
	w := b.viewOf()
	for _, id := range []string{"t1", "t2"} {
		b.fail[b.dirs[id]] = func(call, name string) error {
			if call == "rename" && name == "E" {
				return syscall.EIO
			}
			return nil
		}
	}
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err == nil {
		t.Fatal("the rename went through")
	}
	for _, id := range []string{"t1", "t2"} {
		b.fail[b.dirs[id]] = nil
	}
	open := b.openViewOps()
	if len(open) != 1 || open[0].State != "planned" {
		t.Fatalf("journal %+v", open)
	}
	// A user makes Shows/E directly on the operation's disk, the first that holds the folder.
	b.mkdir(open[0].Disk, "Shows/E")
	if err := w.Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	switch got := b.text("SELECT state FROM view_ops WHERE id = ?", open[0].ID); got {
	case "planned":
		t.Fatal("the reprise resolved nothing")
	case "done":
		t.Errorf("the reprise calls the rename done, though no disk renamed the folder")
	}
	for _, id := range []string{"t1", "t2"} {
		if !b.exists(id, "media/Shows/D/e1.mkv") {
			t.Fatalf("%s no longer holds Shows/D/e1.mkv", id)
		}
	}
	if n, err := w.Tree().Lookup(t.Context(), "Shows/D/e1.mkv"); err != nil || n == nil {
		t.Errorf("the view presents nothing at Shows/D/e1.mkv, where every disk holds it (%v)", err)
	}
}

// Flaw 20 (ViewNames.tla, FlawViewRenameUndone.cfg, liveness): a file renamed through the view, which a user moves back
// directly on that disk before a pass takes the rename in: the view presents the new name from what that disk holds
// there, nothing, and the old name nothing, as renamed from, while the catalog presents the file at the old name. The
// take-in waits for the two to agree (view.TakenIn), which they never do: the operation is never taken in, and the view
// presents the file at neither name, as long as it runs.
func TestFlawARenameThroughTheViewMovedBackIsNeverTakenIn(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Films/a.mkv", "the film")
	}
	b.scan(true)
	b.now += 60_000_000_000
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Films/a.mkv", "Films/b.mkv", false, viewer); err != nil {
		t.Fatal(err)
	}
	open := b.openViewOps()
	if len(open) != 1 {
		t.Fatalf("journal %+v", open)
	}
	// The user moves it back, directly on the disk the view renamed it on.
	b.rename(open[0].Disk, "Films/b.mkv", "Films/a.mkv")
	b.passes(6)
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("the rename is never taken in: %+v", open)
	}
	a, _ := w.Tree().Lookup(t.Context(), "Films/a.mkv")
	bb, _ := w.Tree().Lookup(t.Context(), "Films/b.mkv")
	if a == nil && bb == nil {
		t.Error("the view presents the file at neither name")
	}
}

// Flaw 21 (ViewNames.tla, FlawViewFolderTaken.cfg, liveness): a user makes Shows/E directly on one disk, which no pass
// has recorded, so that the view presents nothing there; the view renames Shows/D to Shows/E on every disk that holds
// it, refused on that disk, made on the other: done, as it reached one (§21.2). The folder's job for the first disk can
// never run, as its new name is taken there; the copies below follow one by one, into the user's folder, and the old
// folder stays there, empty, its entry with it. The view presents nothing at Shows/D, renamed from, the catalog that
// folder: the operation is never taken in.
func TestFlawAFolderRenamedThroughTheViewOntoAUsersFolderIsNeverTakenIn(t *testing.T) {
	b := viewFolderPool(t)
	b.mkdir("t1", "Shows/E")
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	if !b.exists("t2", "media/Shows/E/e1.mkv") || !b.exists("t1", "media/Shows/D/e1.mkv") {
		t.Fatal("the rename did not go as the flaw needs: refused on t1, made on t2")
	}
	b.passes(6)
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("the rename is never taken in: %+v", open)
	}
}

// Flaw 22 (ViewNames.tla, FlawViewFolderJobBack.cfg, ViewNamesKept): the view renames Shows/D to Shows/E on both
// disks, then back; atoll mount stops short (kill -9) once the rename back is made on its first holder, t1, before t2,
// and t1 is lost then. The rename back stays planned, on t1 alone (writer.renameFolder), so that no walk of t2 waits
// for it (jobs.viewPlannedOn): the passes find the first rename on t2 and decide it, queueing a folder's job
// Shows/D to Shows/E for t1, away. t1 back, the job runs, as folderReady asks nothing of the view, and carries t1's
// folder away from Shows/D, the name the view gave it back.
func TestFlawAFolderJobCarriesAFolderFromTheNameTheViewGaveItBack(t *testing.T) {
	b := viewFolderPool(t)
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stopped")
	b.fail[b.dirs["t2"]] = func(string, string) error {
		if b.exists("t1", "media/Shows/D") {
			// The rename back made on t1, not yet on t2: atoll mount stops short.
			panic(stop) //nolint:forbidigo // what stands for kill -9
		}
		return nil
	}
	func() {
		defer func() {
			if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
				t.Fatalf("it did not stop between the two disks: %v", err)
			}
		}()
		_ = w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer)
	}()
	b.fail[b.dirs["t2"]] = nil
	b.gone[b.dirs["t1"]] = true
	b.passes(3)
	b.gone[b.dirs["t1"]] = false
	b.passes(1)
	if !b.exists("t1", "media/Shows/D/e1.mkv") {
		t.Errorf("t1's folder was carried from Shows/D, the name the view gave it back")
	}
}
