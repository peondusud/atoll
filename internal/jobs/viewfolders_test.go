package jobs

import (
	"errors"
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
