package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/watch"
)

// passNames runs a pass of the daemon that looks at some names of one disk alone, those the watcher gave, and spares
// the other disks (specs.md §15.13).
func (b *bench) passNames(id string, names ...watch.Named) *Outcome {
	b.t.Helper()
	if names == nil {
		names = []watch.Named{}
	}
	out, err := RunLaterOn(b.t.Context(), b.db, b.config, b, true, nil, nil, Scope{id: {Names: names}}, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

func (b *bench) hasRow(path string) bool {
	b.t.Helper()
	return b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte(path)) > 0
}

func (b *bench) hasEntry(id, path string) bool {
	b.t.Helper()
	return b.count("SELECT count(*) FROM entries WHERE disk_id = ? AND lpath = ?", id, []byte(path)) > 0
}

// A pass given names looks at those alone: a file new at a name given is recorded, one at a name not given is not, and
// its line says the look was partial, not a walk of the disk.
func TestANamedLookReadsItsNamesAlone(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.write("t1", "b", "new")
	b.write("t1", "c", "unheard")
	out := b.passNames("t1", watch.Named{Path: "b"})
	if !b.hasRow("b") || b.hasRow("c") {
		t.Errorf("b recorded %v, c recorded %v", b.hasRow("b"), b.hasRow("c"))
	}
	if one := lineOf(out, "t1"); one == nil || !one.Walked || !one.Partial || !lineOf(out, "t2").Spared || len(out.Whole) != 0 {
		t.Fatalf("lines %+v, whole %v", out.Disks, out.Whole)
	}
}

// A copy no longer at a name an event said was deleted is gone: its clock runs, and once due, a look at its name again
// decides the deletion, which reaches the other disk (specs.md §15.13).
func TestANamedDeletionIsDecided(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.remove("t1", "a")
	out := b.passNames("t1", watch.Named{Path: "a", Deleted: true})
	if b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND gone_kind = 'gone'") != 1 || len(out.Whole) != 0 {
		t.Fatalf("no clock: whole %v", out.Whole)
	}
	b.now += 30_000_000_000
	// No name given: the clock of the copy brings its name back, and nothing more.
	walks := b.countWalks()
	out = b.passOn()
	if one := lineOf(out, "t1"); one == nil || !one.Partial {
		t.Fatalf("lines %+v", out.Disks)
	}
	if got := outcomeOf(out, KindTrash, "a"); got != "trashed" || b.exists("t2", "media/a") {
		t.Fatalf("the copy on t2: %q, %+v", got, out.Operations)
	}
	if walks["t2"] != 0 {
		t.Errorf("t2 walked: %v", walks)
	}
}

// A copy no longer at a name with no event that said deleted, only moved-from, may be anywhere the look did not go:
// nothing is concluded of it, and the next pass walks the disk whole, which finds it gone (specs.md §15.13).
func TestANamedAbsenceWithoutDeletionWaitsForAWalk(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.remove("t1", "a")
	out := b.passNames("t1", watch.Named{Path: "a"})
	if !slices.Equal(out.Whole, []string{"t1"}) || b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL") != 0 {
		t.Fatalf("whole %v, clocks %d", out.Whole, b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL"))
	}
	b.now += 30_000_000_000
	out = b.passNames("t1", watch.Named{Path: "a"})
	if len(out.Operations) != 0 || !b.exists("t2", "media/a") {
		t.Fatalf("decided on a look by names: %+v", out.Operations)
	}
	b.passOn("t1")
	b.now += 30_000_000_000
	if got := outcomeOf(b.passOn("t1"), KindTrash, "a"); got != "trashed" {
		t.Fatalf("after walks of the whole disk: %q", got)
	}
}

// A file at a name given, with no row, whose inode a row elsewhere on the disk holds: the look reads that name too, and
// finds a rename, though no event gave the old name. The new name is left to look at again, and the rename, once due,
// reaches the other disk (specs.md §15.13).
func TestANamedRenameIsReadByItsInode(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.rename("t1", "a", "b")
	out := b.passNames("t1", watch.Named{Path: "b"})
	if b.hasRow("b") || b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND gone_kind = 'moved'") != 1 {
		t.Fatalf("b recorded %v, as a new file", b.hasRow("b"))
	}
	again := out.Again["t1"]
	if !slices.ContainsFunc(again, func(n watch.Named) bool { return n.Path == "b" }) || len(out.Whole) != 0 {
		t.Fatalf("again %+v, whole %v", out.Again, out.Whole)
	}
	b.now += 30_000_000_000
	out = b.passNames("t1", again...)
	if got := outcomeOf(out, KindRename, "a"); got != OutcomeRenamed || !b.exists("t2", "media/b") || b.exists("t2", "media/a") {
		t.Fatalf("the rename on t2: %q, %+v", got, out.Operations)
	}
}

// A copy found moved by a walk of the whole disk has a new name the catalog does not hold: a pass given no names for its
// disk walks it whole, as a look at the old name could not find it.
func TestAMoveAWalkFoundIsLookedForByAWalk(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.rename("t1", "a", "b")
	b.passOn("t1")
	b.now += 30_000_000_000
	out := b.passOn()
	if one := lineOf(out, "t1"); one == nil || one.Partial || !one.Walked {
		t.Fatalf("lines %+v", out.Disks)
	}
}

// A folder given is walked below: what it holds is recorded, with its own entry, and a folder no name led to keeps its
// entry.
func TestANamedFolderIsWalkedBelow(t *testing.T) {
	b := pool(t)
	b.write("t1", "g/h", "sibling")
	b.passLater()
	b.write("t1", "d/e", "one")
	b.write("t1", "d/f/g", "two")
	b.passNames("t1", watch.Named{Path: "d", Folder: true})
	if !b.hasRow("d/e") || !b.hasRow("d/f/g") || !b.hasEntry("t1", "d") || !b.hasEntry("t1", "d/f") {
		t.Fatalf("rows %v %v, entries %v %v", b.hasRow("d/e"), b.hasRow("d/f/g"), b.hasEntry("t1", "d"), b.hasEntry("t1", "d/f"))
	}
	if !b.hasEntry("t1", "g") || !b.hasRow("g/h") {
		t.Errorf("g lost its entry or its row: %v %v", b.hasEntry("t1", "g"), b.hasRow("g/h"))
	}
}

// The folder that holds a name given is read too, as Atoll's own view presents it: its entry takes its new time.
func TestANamedLookReadsTheFolderOfItsName(t *testing.T) {
	b := pool(t)
	b.write("t1", "d/e", "one")
	b.passLater()
	b.write("t1", "d/x", "two")
	info, err := os.Stat(filepath.Join(b.dirs["t1"], "media", "d"))
	if err != nil {
		t.Fatal(err)
	}
	b.passNames("t1", watch.Named{Path: "d/x"})
	if got := b.count("SELECT count(*) FROM entries WHERE disk_id = 't1' AND lpath = ? AND mtime_ns = ?", []byte("d"), info.ModTime().UnixNano()); got != 1 {
		t.Errorf("the entry of d holds another time than %d", info.ModTime().UnixNano())
	}
	if !b.hasRow("d/e") || !b.hasRow("d/x") {
		t.Errorf("rows %v %v", b.hasRow("d/e"), b.hasRow("d/x"))
	}
}

// A folder renamed, both names given: read as a folder renamed, and the rename, once due, reaches the other disk.
func TestANamedFolderRenameIsDecided(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "s/1", "one")
		b.write(id, "s/2", "two")
	}
	b.scan(true)
	b.passLater()
	b.rename("t1", "s", "s2")
	out := b.passNames("t1", watch.Named{Path: "s", Folder: true}, watch.Named{Path: "s2", Folder: true})
	if len(out.Whole) != 0 {
		t.Fatalf("whole %v", out.Whole)
	}
	for range 3 {
		b.now += 30_000_000_000
		names := append(slices.Clone(out.Again["t1"]), watch.Named{Path: "s", Folder: true}, watch.Named{Path: "s2", Folder: true})
		out = b.passNames("t1", names...)
	}
	if !b.exists("t2", "media/s2/1") || !b.exists("t2", "media/s2/2") || b.exists("t2", "media/s") {
		t.Fatalf("t2 holds s2/1 %v, s2/2 %v, s %v: %+v", b.exists("t2", "media/s2/1"), b.exists("t2", "media/s2/2"), b.exists("t2", "media/s"), out.Operations)
	}
}

// A folder an event said was deleted: what it held is gone, and once due its copies on the other disk go to the trash.
func TestANamedFolderDeletionIsDecided(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "s/1", "one")
		b.write(id, "s/2", "two")
	}
	b.scan(true)
	b.passLater()
	if err := os.RemoveAll(filepath.Join(b.dirs["t1"], "media", "s")); err != nil {
		t.Fatal(err)
	}
	// With no event that said deleted, the folder may have gone anywhere: its entry stays, and the disk is walked whole.
	if out := b.passNames("t1", watch.Named{Path: "s", Folder: true}); !slices.Equal(out.Whole, []string{"t1"}) || !b.hasEntry("t1", "s") {
		t.Fatalf("whole %v, entry of s %v", out.Whole, b.hasEntry("t1", "s"))
	}
	b.passNames("t1", watch.Named{Path: "s", Folder: true, Deleted: true})
	b.now += 30_000_000_000
	out := b.passOn()
	if b.exists("t2", "media/s/1") || b.exists("t2", "media/s/2") {
		t.Fatalf("the copies on t2 stayed: %+v", out.Operations)
	}
}

// A second name of a file whose first name is still there is a file of its own, as a walk would read it: the look reads
// the first name too, by the inode, and finds it there, so nothing moved.
func TestANamedLinkIsAFileOfItsOwn(t *testing.T) {
	b := pool(t)
	b.passLater()
	if err := os.Link(filepath.Join(b.dirs["t1"], "media", "a"), filepath.Join(b.dirs["t1"], "media", "b")); err != nil {
		t.Fatal(err)
	}
	out := b.passNames("t1", watch.Named{Path: "b"})
	if !b.hasRow("b") || b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL") != 0 || len(out.Whole) != 0 {
		t.Fatalf("b recorded %v, clocks %d, whole %v", b.hasRow("b"), b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL"), out.Whole)
	}
}

// A file moved over another, as mv a b over b does, loses b's file to the overwrite. No event said b was deleted, so a
// look limited to names concludes nothing of it, and the next pass walks the disk whole, as nothing tells b's file did
// not move first.
func TestANamedOverwriteWaitsForAWalk(t *testing.T) {
	b := pool(t)
	b.write("t1", "b", "other")
	b.write("t2", "b", "other")
	b.scan(true)
	b.passLater()
	b.rename("t1", "a", "b")
	out := b.passNames("t1", watch.Named{Path: "a"}, watch.Named{Path: "b"})
	if !slices.Equal(out.Whole, []string{"t1"}) || b.count("SELECT count(*) FROM copies WHERE gone_kind = 'gone'") != 0 {
		t.Fatalf("whole %v, gone %d", out.Whole, b.count("SELECT count(*) FROM copies WHERE gone_kind = 'gone'"))
	}
}

// An empty folder no longer at a name, with no event that said deleted, may have gone anywhere: its entry stays, as no
// copy below it keeps it, and the disk is walked whole.
func TestANamedEmptyFolderGoneWaitsForAWalk(t *testing.T) {
	b := pool(t)
	if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.passLater()
	if !b.hasEntry("t1", "e") {
		t.Fatal("no entry for e")
	}
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "e")); err != nil {
		t.Fatal(err)
	}
	if out := b.passNames("t1", watch.Named{Path: "e", Folder: true}); !slices.Equal(out.Whole, []string{"t1"}) || !b.hasEntry("t1", "e") {
		t.Fatalf("whole %v, entry of e %v", out.Whole, b.hasEntry("t1", "e"))
	}
	if b.passNames("t1", watch.Named{Path: "e", Folder: true, Deleted: true}); b.hasEntry("t1", "e") {
		t.Error("the entry of e stayed, though an event said it was deleted")
	}
}

// A name below a symbolic link is no name of the library: a look never follows one.
func TestANamedLookFollowsNoLink(t *testing.T) {
	b := pool(t)
	b.write("t1", "real/x", "x")
	b.passLater()
	if err := os.Symlink("real", filepath.Join(b.dirs["t1"], "media", "l")); err != nil {
		t.Fatal(err)
	}
	b.passNames("t1", watch.Named{Path: "l/x"})
	if b.hasRow("l/x") {
		t.Error("a name below a link got a row")
	}
}

// A copy edited at a name given waits for settle, and its name is left to look at again while its row does not hold
// what the look found; once settled, a look at that name decides it, and leaves nothing to look at again.
func TestANamedEditIsFound(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a")
	out := b.passNames("t1", watch.Named{Path: "a"})
	if out.Waiting == 0 || len(out.WaitDisks) != 0 || !slices.Equal(out.Again["t1"], []watch.Named{{Path: "a"}}) {
		t.Fatalf("waiting for settle: %+v", out)
	}
	b.now += settleNS
	out = b.passNames("t1", out.Again["t1"]...)
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) || len(out.Again) != 0 {
		t.Fatalf("decided: %+v", out)
	}
}
