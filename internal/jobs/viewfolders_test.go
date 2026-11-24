package jobs

import (
	"errors"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/view/writer"
)

// errStopped stops what a hook of the bench interrupts, as kill -9 stops atoll mount: nothing after it runs, and what
// it held is let go of.
var errStopped = errors.New("stopped")

// stopped runs f, which a hook stops short with errStopped.
func (b *bench) stopped(f func()) {
	b.t.Helper()
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, errStopped) {
			b.t.Fatalf("it did not stop: %v", err)
		}
	}()
	f()
}

// viewStateOf gives the state of an operation of the view.
func (b *bench) viewStateOf(id int64) string {
	b.t.Helper()
	return b.text("SELECT state FROM view_ops WHERE id = ?", id)
}

// folderAt says where a disk holds the folder of viewFolderPool, with its file: "D", "E", "both" or "none".
func (b *bench) folderAt(id string) string {
	b.t.Helper()
	d, e := b.exists(id, "media/Shows/D/e1.mkv"), b.exists(id, "media/Shows/E/e1.mkv")
	switch {
	case d && e:
		return "both"
	case d:
		return "D"
	case e:
		return "E"
	}
	return "none"
}

// A folder's rename through the view left planned is resolved by the folder its journal names, by its inode, on the
// disk it names, which the journal names before each rename of a holder until one went through: done only when that
// disk holds that folder at the new name, undone otherwise, whatever else a user made there (specs.md §21.2;
// conformance/models/README.md, flaw 19, the maintainer's remedy, 2026-11-24). Before, the reprise called it done when
// the first holder held any folder at the new name, and a folder a user made there hid the files, presented at no name
// they were at.
func TestTheRepriseOfAFolderRenameKnowsItsFolder(t *testing.T) {
	eio := func(b *bench, ids ...string) {
		for _, id := range ids {
			b.fail[b.dirs[id]] = func(call, name string) error {
				if call == "rename" && name == "E" {
					return syscall.EIO
				}
				return nil
			}
		}
	}
	cases := []struct {
		name string
		// before is what a user does directly before the rename, rename makes it, left planned, and after is what a
		// user does directly then.
		before, after func(b *bench)
		rename        func(b *bench, w *writer.Writer)
		want          string
		t1, t2        string
	}{
		{"every disk failing the rename, a user's folder made at the new name then", nil,
			func(b *bench) { b.mkdir(b.openViewOps()[0].Disk, "Shows/E") },
			func(b *bench, w *writer.Writer) {
				eio(b, "t1", "t2")
				if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err == nil {
					b.t.Fatal("the rename went through")
				}
			}, "undone", "D", "D"},
		{"stopped before the first rename, a user's folder made at the new name then", nil,
			func(b *bench) { b.mkdir("t1", "Shows/E") },
			func(b *bench, w *writer.Writer) {
				b.at["folder-named"] = func() { panic(errStopped) } //nolint:forbidigo // what stands for kill -9
				b.stopped(func() { _ = w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer) })
			}, "undone", "D", "D"},
		{"stopped once the first holder renamed it", nil, nil,
			func(b *bench, w *writer.Writer) {
				b.fail[b.dirs["t2"]] = func(string, string) error {
					if b.exists("t1", "media/Shows/E") {
						panic(errStopped) //nolint:forbidigo // what stands for kill -9
					}
					return nil
				}
				b.stopped(func() { _ = w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer) })
			}, "done", "E", "D"},
		{"the first holder's new name taken, the second renamed, then stopped",
			func(b *bench) { b.mkdir("t1", "Shows/E") }, nil,
			func(b *bench, w *writer.Writer) {
				b.fail[b.dirs["t2"]] = func(string, string) error {
					if b.exists("t2", "media/Shows/E") {
						panic(errStopped) //nolint:forbidigo // what stands for kill -9
					}
					return nil
				}
				b.stopped(func() { _ = w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer) })
			}, "done", "D", "E"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := viewFolderPool(t)
			if tc.before != nil {
				tc.before(b)
			}
			w := b.viewOf()
			tc.rename(b, w)
			for _, id := range []string{"t1", "t2"} {
				b.fail[b.dirs[id]] = nil
			}
			open := b.openViewOps()
			if len(open) != 1 || open[0].State != "planned" {
				t.Fatalf("journal %+v", open)
			}
			if tc.after != nil {
				tc.after(b)
			}
			if err := b.viewOf().Reprise(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := b.viewStateOf(open[0].ID); got != tc.want {
				t.Errorf("the reprise calls the rename %s, want %s", got, tc.want)
			}
			if got1, got2 := b.folderAt("t1"), b.folderAt("t2"); got1 != tc.t1 || got2 != tc.t2 {
				t.Errorf("the folder at %s on t1 and %s on t2, want %s and %s", got1, got2, tc.t1, tc.t2)
			}
			if tc.want == "undone" {
				if n, err := w.Tree().Lookup(t.Context(), "Shows/D/e1.mkv"); err != nil || n == nil {
					t.Errorf("the view presents nothing at Shows/D/e1.mkv, where every disk holds it (%v)", err)
				}
			}
		})
	}
}

// The reprise's look at the disk the journal names failing leaves the rename planned, as nothing can be said of it;
// the next one says it.
func TestTheRepriseOfAFolderRenameWhoseLookFails(t *testing.T) {
	b := viewFolderPool(t)
	w := b.viewOf()
	b.fail[b.dirs["t2"]] = func(string, string) error {
		if b.exists("t1", "media/Shows/E") {
			panic(errStopped) //nolint:forbidigo // what stands for kill -9
		}
		return nil
	}
	b.stopped(func() { _ = w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer) })
	b.fail[b.dirs["t2"]] = nil
	open := b.openViewOps()
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "stat" && name == "E" {
			return syscall.EIO
		}
		return nil
	}
	if err := b.viewOf().Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.viewStateOf(open[0].ID); got != "planned" {
		t.Errorf("the rename is %s after a look that failed", got)
	}
	b.fail[b.dirs["t1"]] = nil
	if err := b.viewOf().Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.viewStateOf(open[0].ID); got != "done" {
		t.Errorf("the rename is %s, want done", got)
	}
}

// A rename through the view that a user undid directly on its disk before a pass took it in, moving the file back or
// on to another name, is taken in as it is once a pass walked that disk, the catalog then presenting what the disks
// hold (specs.md §21.2; conformance/models/README.md, flaw 20, the maintainer's remedy, 2026-11-24). Before, it was
// never taken in, and the view presented the file at neither name.
func TestARenameThroughTheViewUndoneDirectlyIsTakenIn(t *testing.T) {
	for _, tc := range []struct{ name, to string }{{"moved back", "a"}, {"moved on", "c"}} {
		t.Run(tc.name, func(t *testing.T) {
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
			b.rename(b.openViewOps()[0].Disk, "Films/b.mkv", "Films/"+tc.to+".mkv")
			b.passes(6)
			if open := b.openViewOps(); len(open) != 0 {
				t.Errorf("the rename is not taken in: %+v", open)
			}
			for _, id := range []string{"t1", "t2"} {
				if !b.exists(id, "media/Films/"+tc.to+".mkv") || b.exists(id, "media/Films/b.mkv") {
					t.Errorf("%s does not hold the file at %s.mkv alone", id, tc.to)
				}
			}
			if n, err := b.viewOf().Tree().Lookup(t.Context(), "Films/"+tc.to+".mkv"); err != nil || n == nil {
				t.Errorf("the view presents nothing at %s.mkv (%v)", tc.to, err)
			}
		})
	}
}

// The look at the new name failing, a pass does not take the rename in, and says why; the next one does.
func TestARenameUndoneDirectlyWhoseLookFails(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Films/a.mkv", "the film")
	}
	b.scan(true)
	b.now += 60_000_000_000
	if err := b.viewOf().Rename(t.Context(), "Films/a.mkv", "Films/b.mkv", false, viewer); err != nil {
		t.Fatal(err)
	}
	disk := b.openViewOps()[0].Disk
	b.rename(disk, "Films/b.mkv", "Films/a.mkv")
	failed := false
	b.fail[b.dirs[disk]] = func(call, name string) error {
		if call == "stat" && name == "b.mkv" {
			failed = true
			return syscall.EIO
		}
		return nil
	}
	for range 3 {
		_, _ = Run(t.Context(), b.db, b.config, b, true)
		b.now += 31_000_000_000
	}
	b.fail[b.dirs[disk]] = nil
	if !failed {
		t.Fatal("no look at b.mkv")
	}
	if len(b.openViewOps()) != 1 {
		t.Error("taken in on a look that failed")
	}
	b.passes(3)
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("the rename is not taken in: %+v", open)
	}
}

// A folder renamed through the view onto a name a user made directly on one disk, which no pass recorded: refused
// there, made on the other, done (specs.md §21.2). The folder's job for the first disk cannot run, its new name taken;
// its copies follow one by one into the user's folder, and the old folder, left empty there, makes the rename one a
// user undid directly, taken in as it is (conformance/models/README.md, flaw 21, the maintainer's remedy, 2026-11-24).
// Before, it was never taken in. The same with the daemon killed at each point of the renames of those copies.
func TestAFolderRenamedThroughTheViewOntoAUsersFolderIsTakenIn(t *testing.T) {
	for _, point := range []string{"", "decided", "journaled", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := viewFolderPool(t)
			b.mkdir("t1", "Shows/E")
			if err := b.viewOf().Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
				t.Fatal(err)
			}
			if b.folderAt("t1") != "D" || b.folderAt("t2") != "E" {
				t.Fatal("not refused on t1 and made on t2")
			}
			if point != "" {
				killed := false
				b.at[point] = func() {
					killed = true
					panic(errStopped) //nolint:forbidigo // what stands for kill -9
				}
				for range 6 {
					func() {
						defer func() {
							if r := recover(); r != nil && r != errStopped { //nolint:errorlint // the very value
								panic(r) //nolint:forbidigo // not the kill
							}
						}()
						_, _ = Run(t.Context(), b.db, b.config, b, true)
					}()
					b.now += 31_000_000_000
				}
				delete(b.at, point)
				if !killed {
					t.Fatalf("no pass reached %s", point)
				}
			}
			b.passes(6)
			if open := b.openViewOps(); len(open) != 0 {
				t.Errorf("the rename is not taken in: %+v", open)
			}
			if b.folderAt("t1") != "E" || b.folderAt("t2") != "E" {
				t.Errorf("the file at %s on t1 and %s on t2", b.folderAt("t1"), b.folderAt("t2"))
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// The view renames Shows/D to Shows/E on both disks, then back; atoll mount stops short once the rename back is made on
// t1, before t2, and t1 is lost then. The passes find the first rename on t2 and queue a folder's job Shows/D to
// Shows/E for t1, away. t1 back, the job meets the rename back, an operation of the view at its names no pass has taken
// in, and goes, read in the transaction that would journal it; the copy below waits for that operation (specs.md
// §15.12; conformance/models/README.md, flaw 22, the maintainer's remedy, 2026-11-24). Before, the job carried t1's
// folder away from Shows/D, the name the view gave it back.
func TestAFolderJobGoesForARenameBackThroughTheView(t *testing.T) {
	renamedBack(t)
}

// renamedBack makes the case of TestAFolderJobGoesForARenameBackThroughTheView, up to the view's reprise.
func renamedBack(t *testing.T) *bench {
	t.Helper()
	return renamedBackIn(t, viewFolderPool(t))
}

// renamedBackIn makes that case on a pool that holds Shows/D on both disks.
func renamedBackIn(t *testing.T, b *bench) *bench {
	t.Helper()
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs["t2"]] = func(string, string) error {
		if b.exists("t1", "media/Shows/D") {
			panic(errStopped) //nolint:forbidigo // what stands for kill -9
		}
		return nil
	}
	b.stopped(func() { _ = w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer) })
	b.fail[b.dirs["t2"]] = nil
	b.gone[b.dirs["t1"]] = true
	b.passes(3)
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename' AND file_id IS NULL AND dst_disk = 't1'"); got != 1 {
		t.Fatalf("%d folder jobs for t1", got)
	}
	b.gone[b.dirs["t1"]] = false
	b.passes(1)
	if got := b.folderAt("t1"); got != "D" {
		t.Errorf("t1's folder at %s, carried from Shows/D, the name the view gave it back", got)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename' AND file_id IS NULL"); got != 0 {
		t.Errorf("%d folder jobs left", got)
	}
	// The view starts again: its reprise finds the rename back made on t1, by its inode.
	open := b.openViewOps()
	if err := b.viewOf().Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range open {
		if open[i].Path == "Shows/E" {
			if got := b.viewStateOf(open[i].ID); got != "done" {
				t.Errorf("the rename back is %s after the reprise", got)
			}
		}
	}
	return b
}

// The rename back made on t1 alone, no walk finds it as a folder renamed: t1's entry holds the folder at Shows/D, as no
// walk of t1 found it at Shows/E, and t2 holds it at Shows/E, the name the catalog gives the file. A pass that finds a
// folder's rename through the view done on a disk where the catalog's entry already holds the folder at the new name
// takes it to the disks it missed, as a folder renamed there: the file follows to Shows/D, t2 gets a job from Shows/E to
// Shows/D, and the rename is taken in once t2 holds the folder there (specs.md §15.12, §21.2; conformance/models/
// README.md, observation 5, the maintainer's choice, 2026-11-24). Before, the copy on t1 followed the catalog to
// Shows/E, and the rename back was lost. The same with the job's rename failing once, and with the daemon killed at
// each point of the decision and of that job.
func TestARenameBackThroughTheViewReachesTheDiskItMissed(t *testing.T) {
	for _, point := range []string{"", "eio", "decided", "journaled", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := renamedBack(t)
			switch point {
			case "":
			case "eio":
				failed := false
				b.fail[b.dirs["t2"]] = func(call, name string) error {
					if call == "rename" && name == "D" && !failed {
						failed = true
						return syscall.EIO
					}
					return nil
				}
				b.passes(3)
				b.fail[b.dirs["t2"]] = nil
				if !failed {
					t.Fatal("no rename of the folder on t2")
				}
			default:
				killed := false
				b.at[point] = func() {
					killed = true
					panic(errStopped) //nolint:forbidigo // what stands for kill -9
				}
				for range 6 {
					func() {
						defer func() {
							if r := recover(); r != nil && r != errStopped { //nolint:errorlint // the very value
								panic(r) //nolint:forbidigo // not the kill
							}
						}()
						_, _ = Run(t.Context(), b.db, b.config, b, true)
					}()
					b.now += 31_000_000_000
				}
				delete(b.at, point)
				if !killed {
					t.Fatalf("no pass reached %s", point)
				}
			}
			b.passes(6)
			b.renamedBackEverywhere("e1")
		})
	}
}

// renamedBackEverywhere says what the rename back leaves once the passes took it to t2: each file given at Shows/D on
// both disks, in the catalog and in the view, nothing at Shows/E, and nothing left to do.
func (b *bench) renamedBackEverywhere(files ...string) {
	b.t.Helper()
	for _, name := range files {
		for _, id := range []string{"t1", "t2"} {
			if !b.exists(id, "media/Shows/D/"+name+".mkv") || b.exists(id, "media/Shows/E/"+name+".mkv") {
				b.t.Errorf("%s does not hold %s at Shows/D alone, the name the view gave it back", id, name)
			}
		}
		if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST(? AS BLOB)", "Shows/D/"+name+".mkv"); got != 1 {
			b.t.Errorf("the catalog holds %s at another name than Shows/D", name)
		}
		if n, err := b.viewOf().Tree().Lookup(b.t.Context(), "Shows/D/"+name+".mkv"); err != nil || n == nil {
			b.t.Errorf("the view presents nothing at Shows/D/%s.mkv (%v)", name, err)
		}
	}
	if open := b.openViewOps(); len(open) != 0 {
		b.t.Errorf("the rename back is not taken in: %+v", open)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename'"); got != 0 {
		b.t.Errorf("%d renames left to do", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		b.t.Errorf("%d operations left planned", got)
	}
}

// The same with no disk lost: a pass while the rename back is planned finds the first rename on t2, which the
// journal does not name, and the pass after the reprise decides it there, with the rename back done; the rename back
// then goes on to t2 in that same pass, before anything takes t1's copy from Shows/D (TLC's trace,
// FlawViewFolderRenameBack).
func TestARenameBackThroughTheViewWhileAPassDecidesTheFirst(t *testing.T) {
	b := viewFolderPool(t)
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs["t2"]] = func(string, string) error {
		if b.exists("t1", "media/Shows/D") {
			panic(errStopped) //nolint:forbidigo // what stands for kill -9
		}
		return nil
	}
	b.stopped(func() { _ = w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer) })
	b.fail[b.dirs["t2"]] = nil
	b.passes(1)
	if err := b.viewOf().Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.pass()
	if !b.exists("t1", "media/Shows/D/e1.mkv") {
		t.Error("t1's copy carried from Shows/D in the pass that decides the first rename")
	}
	b.now += 31_000_000_000
	b.passes(6)
	b.renamedBackEverywhere("e1")
}

// The same with t1's looks failing once that pass walked it, as a disk lost then (TLC's trace of the folder's wait,
// FlawViewFolderRenameBack): the rename back stands on t1, as that walk found, and t2's copy found moved from Shows/D
// to Shows/E, against it, is t2 having missed it, never a user's rename: it waits, and with it the folder found renamed
// above it, and the pass renames t2's folder back to Shows/D (specs.md §15.12; conformance/models/README.md, flaw 23,
// the maintainer's remedy, 2026-11-24). Nothing gives t1 a job that, t1 unable to run it before the rename back is
// taken in, would carry its folder from the name the view gave it back in the next pass.
func TestARenameBackThroughTheViewWhileAPassDecidesTheFirstWithT1Lost(t *testing.T) {
	b := viewFolderPool(t)
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs["t2"]] = func(string, string) error {
		if b.exists("t1", "media/Shows/D") {
			panic(errStopped) //nolint:forbidigo // what stands for kill -9
		}
		return nil
	}
	b.stopped(func() { _ = w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer) })
	b.fail[b.dirs["t2"]] = nil
	b.passes(1)
	if err := b.viewOf().Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	// t1's looks fail once a job of this pass waits for t1, past the walks and the decisions.
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "stat" && b.count("SELECT count(*) FROM jobs WHERE kind = 'rename' AND file_id IS NULL AND dst_disk = 't1'") > 0 {
			return syscall.EIO
		}
		return nil
	}
	b.pass()
	b.fail[b.dirs["t1"]] = nil
	b.now += 31_000_000_000
	b.passes(6)
	b.renamedBackEverywhere("e1")
}

// renameBackOnT1 renames Shows/D to Shows/E through the view, made on both disks, then back, made on t1 alone: t2's
// rename fails with EIO, or atoll mount stops short before it, and its reprise calls the rename back done.
func (b *bench) renameBackOnT1(stop bool) {
	b.t.Helper()
	w := b.viewOf()
	if err := w.Rename(b.t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		b.t.Fatal(err)
	}
	b.renameBackOnlyOnT1(w, stop)
}

// renameBackOnlyOnT1 renames Shows/E back to Shows/D through the view, made on t1 alone.
func (b *bench) renameBackOnlyOnT1(w *writer.Writer, stop bool) {
	b.t.Helper()
	if stop {
		b.fail[b.dirs["t2"]] = func(string, string) error {
			if b.exists("t1", "media/Shows/D") {
				panic(errStopped) //nolint:forbidigo // what stands for kill -9
			}
			return nil
		}
		b.stopped(func() { _ = w.Rename(b.t.Context(), "Shows/E", "Shows/D", false, viewer) })
	} else {
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "rename" && name == "D" {
				return syscall.EIO
			}
			return nil
		}
		if err := w.Rename(b.t.Context(), "Shows/E", "Shows/D", false, viewer); err != nil {
			b.t.Fatal(err)
		}
	}
	b.fail[b.dirs["t2"]] = nil
	if b.folderAt("t1") != "D" || b.folderAt("t2") != "E" {
		b.t.Fatalf("the rename back is not made on t1 alone: t1 %s, t2 %s", b.folderAt("t1"), b.folderAt("t2"))
	}
	if err := b.viewOf().Reprise(b.t.Context()); err != nil {
		b.t.Fatal(err)
	}
}

// The same with no pass between the renames and the rename back's end: both are done, and the catalog, which never
// recorded the first, presents what the view presents, while t2 holds the folder at Shows/E. The rename back is not
// taken in while t2, whose entry records the folder it holds at the old name, missed it; the pass that decides the
// first rename from t2 then finds the rename back to take to t2 (conformance/models/README.md, observation 5, the
// maintainer's choice, 2026-11-24). Before, the first pass took both in, and the next read t2's folder at Shows/E as
// a user's rename, which it took to t1.
func TestARenameBackThroughTheViewNoPassRecorded(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "eio", true: "stopped"}[stop], func(t *testing.T) {
			b := viewFolderPool(t)
			b.renameBackOnT1(stop)
			b.passes(6)
			b.renamedBackEverywhere("e1")
		})
	}
}

// The same with a pass between the rename and the rename back, which finds the first rename on both disks and takes
// it in on neither, as the catalog does not present it yet. The job of the rename back for t2 then meets the first
// rename, not taken in, at its names: it runs all the same, as what it carries out is the rename back, which undoes
// it (catalog.viewHoldsRename). Before, the job went, the copy followed into a folder made at Shows/D, and the first
// rename was never taken in, t2 holding Shows/E empty.
func TestARenameBackThroughTheViewAfterAPassFoundTheFirst(t *testing.T) {
	b := viewFolderPool(t)
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.passes(1)
	if len(b.openViewOps()) != 1 {
		t.Fatalf("the first rename is taken in before the catalog presents it: %+v", b.openViewOps())
	}
	b.renameBackOnlyOnT1(b.viewOf(), true)
	b.passes(6)
	b.renamedBackEverywhere("e1")
	if b.exists("t2", "media/Shows/E") {
		t.Error("t2 still holds Shows/E")
	}
}

// The same with t2 lost from the rename back's failure for two passes: a folder's rename of the view is taken in once
// every disk that holds it, by the catalog's entries at either name, was looked at, not only those a pass could read.
// Before, a pass with t2 away took both renames in, and t2's folder, back at Shows/E, read as a user's rename.
func TestARenameBackThroughTheViewWithT2Away(t *testing.T) {
	b := viewFolderPool(t)
	b.renameBackOnT1(false)
	b.gone[b.dirs["t2"]] = true
	b.passes(2)
	if open := b.openViewOps(); len(open) != 2 {
		t.Errorf("taken in with t2 away: %+v", open)
	}
	b.gone[b.dirs["t2"]] = false
	b.passes(6)
	b.renamedBackEverywhere("e1")
}

// The same with the folder empty on t1, where the rename back is made: no copy came with it there, and still it is a
// rename of the folder the view presents, which t2 follows.
func TestARenameBackThroughTheViewMadeWhereTheFolderIsEmpty(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.mkdir("t1", "Shows/D")
	b.write("t2", "Shows/D/e1.mkv", "the episode")
	b.scan(true)
	b.now += 60_000_000_000
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs["t2"]] = func(string, string) error {
		if b.exists("t1", "media/Shows/D") {
			panic(errStopped) //nolint:forbidigo // what stands for kill -9
		}
		return nil
	}
	b.stopped(func() { _ = w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer) })
	b.fail[b.dirs["t2"]] = nil
	if !b.exists("t1", "media/Shows/D") || !b.exists("t2", "media/Shows/E") {
		t.Fatal("the rename back is not made on t1 alone")
	}
	b.gone[b.dirs["t1"]] = true
	b.passes(3)
	if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST('Shows/E/e1.mkv' AS BLOB)"); got != 1 {
		t.Fatal("the passes did not decide the first rename from t2")
	}
	b.gone[b.dirs["t1"]] = false
	b.passes(1)
	if err := b.viewOf().Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.passes(6)
	if !b.exists("t2", "media/Shows/D/e1.mkv") || b.exists("t2", "media/Shows/E/e1.mkv") {
		t.Error("t2 does not hold the file at Shows/D alone, the name the view gave it back")
	}
	if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST('Shows/D/e1.mkv' AS BLOB)"); got != 1 {
		t.Error("the catalog holds the file at another name than Shows/D")
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("the renames are not taken in: %+v", open)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		t.Errorf("%d operations left planned", got)
	}
}

// The same with a copy on t2 below Shows/E deleted directly then, under deletes.direct = "restore": the rename waits
// for that copy's debounce, the copies on t1 staying meanwhile where the view put them, then goes on to t2, the copy
// deleted made again there, at Shows/D.
func TestARenameBackThroughTheViewWaitsForACopyGone(t *testing.T) {
	b := newBenchWith(t, "[deletes]\ndirect = \"restore\"\n", "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Shows/D/e1.mkv", "the episode")
		b.write(id, "Shows/D/e2.mkv", "the next episode")
	}
	b.scan(true)
	b.now += 60_000_000_000
	renamedBackIn(t, b)
	b.remove("t2", "Shows/E/e2.mkv")
	b.pass()
	b.now += 31_000_000_000
	for _, name := range []string{"e1", "e2"} {
		if !b.exists("t1", "media/Shows/D/"+name+".mkv") {
			t.Errorf("t1's %s carried from Shows/D while the rename back waits", name)
		}
	}
	if got := b.text("SELECT CAST(lpath AS TEXT) FROM files WHERE CAST(lpath AS TEXT) LIKE '%e1.mkv'"); got != "Shows/E/e1.mkv" {
		t.Errorf("the file at %s before the copy gone is decided", got)
	}
	b.passes(6)
	b.renamedBackEverywhere("e1", "e2")
}

// A cycle of renames that only an exchange of two copies' names gets round waits, no rename journaled, while an
// operation of the view journaled since the pass began is at one of its names, as a copy's rename does, read as it
// would journal them, past what the pass decided from; the pass after it makes the exchange (specs.md §15.12;
// conformance/models/README.md, flaw 22, the maintainer's remedy, 2026-11-24).
func TestACycleOfRenamesWaitsForTheView(t *testing.T) {
	b := swapPool(t)
	b.at["decided"] = func() {
		b.exec("INSERT INTO view_ops(kind, disk_id, lpath, folder, state, at_ns) VALUES ('create', 't1', CAST('b' AS BLOB), 0, 'done', ?)", b.now)
	}
	b.pass()
	if _, ok := b.at["decided"]; ok {
		t.Fatal("the pass decided nothing")
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND disk_id = 't2'"); got != 0 {
		t.Errorf("%d renames journaled on t2", got)
	}
	if got := b.readMedia("t2", "a"); got != "the file at a" {
		t.Errorf("t2 holds %q at a", got)
	}
	b.exec("UPDATE view_ops SET state = 'undone'")
	b.now += 30_000_000_000
	b.pass()
	if got1, got2 := b.readMedia("t2", "a"), b.readMedia("t2", "b"); got1 != "the file at b, longer" || got2 != "the file at a" {
		t.Errorf("t2 holds %q at a and %q at b: no exchange", got1, got2)
	}
}

// lostAtTheRenameBack runs flaw 23's trace (conformance/models/README.md, FlawViewFolderBackLost): Shows/D with e1 on
// both disks and e2 on t2 alone, renamed through the view to Shows/E on both, its journal naming t1, the first holder;
// a pass finds that rename on both disks; a disk is lost, and the pass with it away decides the rename from the other
// disk's walk; the view renames Shows/E back to Shows/D while that disk is away: made on the other, failed on it (EIO
// here, standing for the lost disk), done. One pass later it is back, its folder at Shows/E, which its entry records at
// Shows/D.
func lostAtTheRenameBack(t *testing.T, lost string) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.write("t1", "Shows/D/e1.mkv", "the episode")
	b.write("t2", "Shows/D/e1.mkv", "the episode")
	b.write("t2", "Shows/D/e2.mkv", "the next episode")
	b.scan(true)
	b.now += 60_000_000_000
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.passes(1)
	b.gone[b.dirs[lost]] = true
	b.passes(1)
	b.fail[b.dirs[lost]] = func(call, name string) error {
		if call == "rename" && name == "D" {
			return syscall.EIO
		}
		return nil
	}
	if err := w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs[lost]] = nil
	b.passes(1)
	b.gone[b.dirs[lost]] = false
	return b
}

// keptBack says what the rename back leaves once the passes took it to the disk that missed it: e1 at Shows/D on both
// disks, e2 there on t2, in the catalog and in the view, nothing at Shows/E, and nothing left to do.
func (b *bench) keptBack() {
	b.t.Helper()
	for _, id := range []string{"t1", "t2"} {
		if !b.exists(id, "media/Shows/D/e1.mkv") || b.exists(id, "media/Shows/E") {
			b.t.Errorf("%s does not hold e1 at Shows/D alone, the name the view gave it back", id)
		}
	}
	if !b.exists("t2", "media/Shows/D/e2.mkv") {
		b.t.Error("t2 does not hold e2 at Shows/D")
	}
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT), ' ') FROM (SELECT lpath FROM files ORDER BY lpath)"); got != "Shows/D/e1.mkv Shows/D/e2.mkv" {
		b.t.Errorf("the catalog holds %s", got)
	}
	for _, name := range []string{"e1", "e2"} {
		if n, err := b.viewOf().Tree().Lookup(b.t.Context(), "Shows/D/"+name+".mkv"); err != nil || n == nil {
			b.t.Errorf("the view presents nothing at Shows/D/%s.mkv (%v)", name, err)
		}
	}
	if open := b.openViewOps(); len(open) != 0 {
		b.t.Errorf("not taken in: %+v", open)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename'"); got != 0 {
		b.t.Errorf("%d renames left to do", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		b.t.Errorf("%d operations left planned", got)
	}
}

// A folder's rename back through the view missed by a disk lost at its turn is kept: on that disk, back with its folder
// at the old name, a folder found renamed and a copy found moved from the rename back's new name to its old wait, and
// the pass renames that folder to the new name, at once, as a folder's job it never queues; the rename back is taken
// in once the catalog holds no file below its old name (specs.md §15.12, §21.2; conformance/models/README.md, flaw 23,
// the maintainer's remedy, 2026-11-24). Before, the lost disk's walk read the first rename as a user's while the other
// disk's walk found the rename back, and the pass, deciding both together, left e1 as two files, at Shows/D and at
// Shows/E, the operations of the view never taken in. The same whichever disk is lost, the one the first rename's
// journal names or the other, with that rename failing once, and with the daemon killed at each point of it.
func TestARenameBackMissedByALostDiskIsKept(t *testing.T) {
	for _, lost := range []string{"t1", "t2"} {
		for _, point := range []string{"", "eio", "decided", "journaled", "moved", "target-synced", "source-synced"} {
			t.Run(lost+"/"+point, func(t *testing.T) {
				b := lostAtTheRenameBack(t, lost)
				switch point {
				case "":
				case "eio":
					failed := false
					b.fail[b.dirs[lost]] = func(call, name string) error {
						if call == "rename" && name == "D" && !failed {
							failed = true
							return syscall.EIO
						}
						return nil
					}
					b.passes(3)
					b.fail[b.dirs[lost]] = nil
					if !failed {
						t.Fatal("no rename of the folder on the disk back")
					}
				default:
					killed := false
					b.at[point] = func() {
						// At each point of the pass's rename on the disk back, and at the first decision.
						if point != "decided" && b.count("SELECT count(*) FROM ops WHERE state = 'planned' AND kind = 'rename' AND disk_id = ? AND CAST(path AS TEXT) = 'Shows/E' AND CAST(path2 AS TEXT) = 'Shows/D'", lost) == 0 {
							return
						}
						killed = true
						panic(errStopped) //nolint:forbidigo // what stands for kill -9
					}
					for range 6 {
						func() {
							defer func() {
								if r := recover(); r != nil && r != errStopped { //nolint:errorlint // the very value
									panic(r) //nolint:forbidigo // not the kill
								}
							}()
							_, _ = Run(t.Context(), b.db, b.config, b, true)
						}()
						b.now += 31_000_000_000
					}
					delete(b.at, point)
					if !killed {
						t.Fatalf("no pass reached %s", point)
					}
				}
				b.passes(8)
				b.keptBack()
			})
		}
	}
}

// A dry run lists the rename a pass makes on a disk that missed a folder's rename of the view, as a folder's job, and
// does nothing: the disk back still holds the folder at the old name (specs.md §15.12; invariant 17).
func TestADryRunListsTheRenameOfADiskThatMissedIt(t *testing.T) {
	for _, lost := range []string{"t1", "t2"} {
		t.Run(lost, func(t *testing.T) {
			b := lostAtTheRenameBack(t, lost)
			dry, err := Run(t.Context(), b.db, b.config, b, false)
			if err != nil {
				t.Fatal(err)
			}
			listed := slices.ContainsFunc(dry.Operations, func(op Operation) bool {
				return op.Disk == lost && op.Kind == KindFolder && op.Path == "Shows/E" && op.To == "Shows/D" && op.Outcome == OutcomeToRename
			})
			if !listed {
				t.Errorf("the dry run does not list the rename of Shows/E to Shows/D on %s: %+v", lost, dry.Operations)
			}
			if !b.exists(lost, "media/Shows/E/e1.mkv") || b.exists(lost, "media/Shows/D") {
				t.Errorf("the dry run moved %s's folder", lost)
			}
			b.passes(8)
			b.keptBack()
		})
	}
}
