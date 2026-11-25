//go:build flaws

package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// Flaw 24 of conformance/models/README.md (Collisions.tla, FlawCollisionRename.cfg and FlawCollisionMoved.cfg,
// NoAtollPair; FlawCollisionView.cfg, through the view, which this test leaves out): the plan
// places a file's copy on an NTFS disk, t2, when no name collides there, by the catalog's keys and by what the plan
// holds itself, the folder above each copy not made yet among them (solver.ownNames). The copy waits in the queue. Every
// other writer asks the catalog alone (catalog.KeyTakenExcept), which has no row for that folder until the copy makes
// it; and the copy's job asks nothing, and writes the file's path as it is when it runs. So Atoll writes Films and
// films side by side on t2, which Windows reads as one folder, against the hard constraint of the solver (specs.md
// §12.4, §15.7): a rename of another file on t1 into films, followed on t2 before the copy is made; or the file itself
// renamed into films while its copy waits, t2 holding Films.
func TestFlawACopyNotMadeYetCollidesOnNTFS(t *testing.T) {
	onNTFS := func(t *testing.T) *bench {
		t.Helper()
		b := newBench(t, "t1", "t2")
		b.config.Disks[1].FS = names.NTFS
		return b
	}
	// The copy is queued, and waits, as for its window, while the passes run with replication off: atoll replicate
	// --now runs it, later.
	queued := func(b *bench, path string) {
		b.t.Helper()
		b.replicate("", ReplicateOptions{})
		if got := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2' AND c.state = 'pending' AND f.lpath = CAST(? AS BLOB)", path); got != 1 {
			b.t.Fatalf("no copy of %s queued for t2", path)
		}
		b.reconfigure("[replication]\nenabled = false\n")
		b.config.Disks[1].FS = names.NTFS
	}
	runNow := func(b *bench) {
		b.t.Helper()
		b.reconfigure("")
		b.config.Disks[1].FS = names.NTFS
		b.replicate("", ReplicateOptions{Now: true})
	}
	t.Run("a rename followed before the copy", func(t *testing.T) {
		b := onNTFS(t)
		b.write("t1", "Films/a.mkv", "the film")
		b.write("t1", "Other/b.mkv", "another film")
		b.write("t2", "Other/b.mkv", "another film")
		b.scan(false)
		queued(b, "Films/a.mkv")
		b.rename("t1", "Other/b.mkv", "films/b.mkv")
		b.settle()
		if !b.exists("t2", "media/films/b.mkv") {
			t.Fatal("the rename did not reach t2")
		}
		runNow(b)
		if b.exists("t2", "media/Films") && b.exists("t2", "media/films") {
			t.Error("t2, an NTFS disk, holds Films and films, both written by Atoll")
		}
	})
	t.Run("the file renamed while its copy waits", func(t *testing.T) {
		b := onNTFS(t)
		b.write("t1", "Films/x.mkv", "a film")
		b.write("t2", "Films/x.mkv", "a film")
		b.write("t1", "Other/a.mkv", "the film")
		b.scan(false)
		queued(b, "Other/a.mkv")
		b.rename("t1", "Other/a.mkv", "films/a.mkv")
		b.settle()
		runNow(b)
		if b.exists("t2", "media/Films") && b.exists("t2", "media/films") {
			t.Error("t2, an NTFS disk, holds Films and films, both written by Atoll")
		}
	})
}

// Flaw 26 of conformance/models/README.md (Conflicts.tla, FlawConflictViewWrite.cfg, NoVersionLost): a file in
// conflict, two versions on two disks, Atoll's own view presenting t1's. A program opens it through the view to write;
// atoll conflicts resolve --keep t2 sends t1's copy to the trash, overwritten, its move checked against that copy's row
// alone (trash.Session.Send), never against the view's writes held open, as the SSD cache's transfer is
// (TrashStart.ViewFree, flaw 10); what the program writes then goes into the copy in the trash, a version no resolve
// said it sends away (specs.md §15.7, §21.2).
func TestFlawAWriteOpenedThroughTheViewDuringAResolveIsLost(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a.mkv", "the film")
	b.write("t2", "a.mkv", "another cut")
	b.scan(true)
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Fatalf("the file is %s", got)
	}
	w, tree := b.viewOf(), b.treeOf()
	f, err := b.openThroughView(w, tree, "a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if f.Disk != "t1" {
		t.Fatalf("the view opened the copy on %s", f.Disk)
	}
	session, err := trash.Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := session.Resolve(t.Context(), "a.mkv", "t2", func(*trash.ResolveItem) error { return nil })
	session.Close()
	if err != nil {
		t.Fatal(err)
	}
	const late = "a write the program makes after the resolve"
	if _, err := f.WriteAt([]byte(late), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	at := b.contentAnywhere(late)
	kept := false
	for _, place := range at {
		if !strings.Contains(place, "/trash/") {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the write through the view is only in the trash, %v (resolved: %v)", at, sum.Resolved)
	}
}

// foldingDir finds a name by its ASCII case, as ntfs3 finds a name of an NTFS volume that differs from it in case
// alone: a look at, or a folder opened by, a name it does not hold but holds in another case gets that one, and a folder
// made at such a name already exists.
type foldingDir struct{ fsx.Dir }

func (d *foldingDir) Unwrap() fsx.Dir { return d.Dir }

// asciiEqual compares two names, ASCII letters in either case alike, as the double of a driver.
func asciiEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	lower := func(c byte) byte {
		if c >= 'A' && c <= 'Z' {
			return c + 'a' - 'A'
		}
		return c
	}
	for i := range len(a) {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func (d *foldingDir) fold(name string) string {
	if _, err := d.Dir.Stat(name); err == nil {
		return name
	}
	held, err := d.Names()
	if err != nil {
		return name
	}
	for _, n := range held {
		if asciiEqual(n, name) {
			return n
		}
	}
	return name
}

func (d *foldingDir) Stat(name string) (fsx.Stat, error) {
	return d.Dir.Stat(d.fold(name)) //nolint:wrapcheck // a test double
}

func (d *foldingDir) OpenDir(name string) (fsx.Dir, error) {
	sub, err := d.Dir.OpenDir(d.fold(name))
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &foldingDir{Dir: sub}, nil
}

func (d *foldingDir) Mkdir(name string, mode uint32) error {
	if d.fold(name) != name {
		return syscall.EEXIST
	}
	return d.Dir.Mkdir(name, mode) //nolint:wrapcheck // a test double
}

// Flaw 25 of conformance/models/README.md (Collisions.tla, FlawCollisionFold.cfg, CopiesAtTheirNames): on an NTFS disk
// whose driver finds a name by its case alone (ntfs3), a copy queued for Films/a.mkv, and a folder films a user made
// there directly, which no walk found yet (§15.7's stated limit). The copy's look at its target (ops.LookAtTarget)
// finds films for Films, goes into it, and the copy lands at films/a.mkv, while the catalog records it at Films/a.mkv:
// a replica at another name than its file's, against invariant 6, which the next walk reads as a rename on t2.
func TestFlawACopyLandsInAUsersFolderOfAnotherCase(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &foldingDir{Dir: top} }
	b.write("t1", "Films/a.mkv", "the film")
	b.scan(false)
	b.replicate("", ReplicateOptions{})
	if err := os.Mkdir(filepath.Join(b.dirs["t2"], "media", "films"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.replicate("", ReplicateOptions{Now: true})
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got == "ok" && !b.exists("t2", "media/Films/a.mkv") {
		t.Errorf("t2's copy, ok at Films/a.mkv in the catalog, is not there; it is at films/a.mkv: %v", b.exists("t2", "media/films/a.mkv"))
	}
}
