package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/view/writer"
)

// onNTFS gives a bench of two disks, t2 an NTFS disk.
func onNTFS(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	return b
}

// asNTFS gives the bench its configuration again with more, t2 an NTFS disk as before.
func (b *bench) asNTFS(more string) {
	b.t.Helper()
	b.reconfigure(more)
	b.config.Disks[1].FS = names.NTFS
}

// queuedOnT2 has the plan queue the copy of a path on t2, which then waits, as for its window, while the passes run
// with replication off: runNowOnT2 runs it later, as atoll replicate --now.
func (b *bench) queuedOnT2(path string) {
	b.t.Helper()
	b.replicate("", ReplicateOptions{})
	if got := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2' AND c.state = 'pending' AND f.lpath = CAST(? AS BLOB)", path); got != 1 {
		b.t.Fatalf("no copy of %s queued for t2", path)
	}
	b.asNTFS("[replication]\nenabled = false\n")
}

func (b *bench) runNowOnT2() []ReplicateItem {
	b.t.Helper()
	b.asNTFS("")
	items, _ := b.replicate("", ReplicateOptions{Now: true})
	return items
}

// outcomeFor gives the outcome of the copy of a path among a run's.
func outcomeFor(items []ReplicateItem, path string) string {
	for i := range items {
		if items[i].Path == path {
			return items[i].Outcome
		}
	}
	return ""
}

// bothCases says whether t2 holds Films and films side by side, which Windows reads as one folder.
func (b *bench) bothCases() bool {
	b.t.Helper()
	return b.exists("t2", "media/Films") && b.exists("t2", "media/films")
}

// Flaw 24 of conformance/models/README.md (Collisions.tla, NoAtollPair: FlawCollisionRename, FlawCollisionView,
// FlawCollisionMoved). The plan checks a copy's names once, by the catalog's keys and by what it holds itself, and the
// copy waits in the queue; meanwhile another writer brings onto t2, an NTFS disk, a name of the same key, which no row
// says yet: a rename of another file followed there, a new file through Atoll's own view not taken in yet, or the file
// itself renamed into a folder of the other case while its copy waits. Right before Atoll makes a name on an NTFS disk,
// it lists the folder the name goes in, and each folder on the way, and refuses it when another name there has its key,
// whoever made it (disk-key-look, the maintainer's remedy, 2026-11-25): the copy ends name-refused, and t2 never holds
// Films and films, both Atoll's (specs.md §12.4, §15.7). Atoll made both side by side, in each of the three ways.
func TestNoNameBroughtOnNTFSBeforeACopyCollides(t *testing.T) {
	t.Run("a rename followed before the copy", func(t *testing.T) {
		b := onNTFS(t)
		b.write("t1", "Films/a.mkv", "the film")
		b.write("t1", "Other/b.mkv", "another film")
		b.write("t2", "Other/b.mkv", "another film")
		b.scan(false)
		b.queuedOnT2("Films/a.mkv")
		b.rename("t1", "Other/b.mkv", "films/b.mkv")
		b.settle()
		if !b.exists("t2", "media/films/b.mkv") {
			t.Fatal("the rename did not reach t2")
		}
		items := b.runNowOnT2()
		if b.bothCases() {
			t.Error("t2, an NTFS disk, holds Films and films, both written by Atoll")
		}
		if got := outcomeFor(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
			t.Errorf("the copy of Films/a.mkv ended %q, want name-refused: %+v", got, items)
		}
		if !b.exists("t2", "media/films/b.mkv") || len(b.parts("t2")) != 0 {
			t.Errorf("t2 lost the rename, or keeps a part: %v", b.parts("t2"))
		}
		// The next plan knows films on t2, which Atoll made and recorded: the file is name-collision, as §15.7 says.
		b.pass()
		if got := b.text("SELECT state FROM files WHERE lpath = CAST('Films/a.mkv' AS BLOB)"); got != "name-collision" {
			t.Errorf("Films/a.mkv is %s", got)
		}
	})
	t.Run("the file renamed while its copy waits", func(t *testing.T) {
		b := onNTFS(t)
		b.write("t1", "Films/x.mkv", "a film")
		b.write("t2", "Films/x.mkv", "a film")
		b.write("t1", "Other/a.mkv", "the film")
		b.scan(false)
		b.queuedOnT2("Other/a.mkv")
		b.rename("t1", "Other/a.mkv", "films/a.mkv")
		b.settle()
		items := b.runNowOnT2()
		if b.bothCases() {
			t.Error("t2, an NTFS disk, holds Films and films, both written by Atoll")
		}
		if got := outcomeFor(items, "films/a.mkv"); got != trash.OutcomeNameRefused {
			t.Errorf("the copy of films/a.mkv ended %q, want name-refused: %+v", got, items)
		}
	})
	t.Run("a new file through the view before the plan", func(t *testing.T) {
		b := onNTFS(t)
		b.write("t1", "Films/a.mkv", "the film")
		b.scan(false)
		// t2 has more room: a new file of the view lands there.
		b.spaces = map[string][2]int64{b.dirs["t1"]: {1 << 30, 1 << 28}}
		w := b.viewOf()
		if _, err := w.Mkdir(t.Context(), "films", 0o755, viewer); err != nil {
			t.Fatal(err)
		}
		f, _, err := w.Create(t.Context(), "films/x.mkv", 0o644, viewer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("a new film"), 0); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if !b.exists("t2", "media/films/x.mkv") {
			t.Fatal("the view's new file is not on t2")
		}
		// No pass took it in: atoll replicate plans from what the catalog says, which has no row of films on t2.
		items, _ := b.replicate("", ReplicateOptions{Now: true})
		if b.bothCases() {
			t.Error("t2, an NTFS disk, holds Films and films, both written by Atoll")
		}
		if got := outcomeFor(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
			t.Errorf("the copy of Films/a.mkv ended %q, want name-refused: %+v", got, items)
		}
	})
}

// What Atoll's own view makes or renames on an NTFS disk is looked at by its key there too, right before, the disk
// being the truth for a name no walk recorded yet, a user's made there directly: a new folder or file of the view goes
// to the next disk placement gives, its operation on the first undone, as the plan places a copy on another disk than
// one whose catalog knows such a name; a rename to such a name is refused, EINVAL, before any disk renames, so that it
// goes on every disk that holds the file or on none (specs.md §15.7, §21.2; conformance/models/README.md, flaw 24,
// FlawCollisionView). The view made Films beside films, and renamed a.mkv beside B.mkv.
func TestTheViewMakesNothingBesideANameOfItsKey(t *testing.T) {
	b := onNTFS(t)
	b.write("t2", "a.mkv", "a film")
	b.scan(false)
	b.spaces = map[string][2]int64{b.dirs["t1"]: {1 << 30, 1 << 28}}
	if err := os.Mkdir(filepath.Join(b.dirs["t2"], "media", "films"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.write("t2", "B.mkv", "a user's film")
	w := b.viewOf()
	if _, err := w.Mkdir(t.Context(), "Films", 0o755, viewer); err != nil {
		t.Fatal(err)
	}
	f, _, err := w.Create(t.Context(), "Films/a.mkv", 0o644, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if f.Disk != "t1" {
		t.Errorf("the new file is on %s", f.Disk)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b.exists("t2", "media/Films") || !b.exists("t1", "media/Films/a.mkv") {
		t.Errorf("t2 holds Films %v; t1 holds Films/a.mkv %v", b.exists("t2", "media/Films"), b.exists("t1", "media/Films/a.mkv"))
	}
	if got := b.text("SELECT group_concat(disk_id || ':' || state, ' ') FROM (SELECT disk_id, state FROM view_ops WHERE kind = 'mkdir' ORDER BY id)"); got != "t2:undone t1:done" {
		t.Errorf("the view's folders: %s", got)
	}
	if err := w.Rename(t.Context(), "a.mkv", "b.mkv", true, viewer); !errors.Is(err, writer.ErrRefused) {
		t.Errorf("the rename beside B.mkv: %v", err)
	}
	if !b.exists("t2", "media/a.mkv") || b.exists("t2", "media/b.mkv") {
		t.Error("the rename went on t2")
	}
}

// foldingDir finds a name by its ASCII case, as a driver that folds case finds a name of an NTFS volume, ntfs3 and the
// type ntfs mounted with nocase and lowntfs-3g with ignore_case, which the mount check refuses (specs.md §10, §15.8):
// a look at, or a folder opened by, a name it does not hold but holds in another case gets that one, and a folder made
// at such a name already exists.
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

// Flaw 25 of conformance/models/README.md (Collisions.tla, FlawCollisionFold, CopiesAtTheirNames), and §15.7's stated
// limit it starts from: a folder films a user made directly on t2, an NTFS disk, since its last walk, which no row
// says, and a copy queued for Films/a.mkv. On a driver that finds a name by its bytes, as ntfs3, the type ntfs and
// ntfs-3g do, the copy made Films beside films; on one that finds it by its case, it landed in films, at films/a.mkv,
// while the catalog said Films/a.mkv. Right before it makes Films, Atoll lists t2's folder, finds films there with its
// key, and refuses the copy, name-refused: nothing is written, and nothing lands in films (disk-key-look). The same holds
// for a folder on the way, Films there already, recorded, and films beside it, a user's since the last walk.
func TestACopyNeverMakesANameBesideAnotherOfItsKey(t *testing.T) {
	for _, folding := range []bool{false, true} {
		name := "a driver that finds a name by its bytes"
		if folding {
			name = "a driver that finds a name by its case"
		}
		t.Run(name, func(t *testing.T) {
			b := onNTFS(t)
			if folding {
				b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &foldingDir{Dir: top} }
			}
			b.write("t1", "Films/a.mkv", "the film")
			b.scan(false)
			b.queuedOnT2("Films/a.mkv")
			if err := os.Mkdir(filepath.Join(b.dirs["t2"], "media", "films"), 0o755); err != nil {
				t.Fatal(err)
			}
			items := b.runNowOnT2()
			if got := outcomeFor(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
				t.Errorf("the copy ended %q, want name-refused: %+v", got, items)
			}
			if b.exists("t2", "media/Films") || b.exists("t2", "media/films/a.mkv") {
				t.Errorf("t2 holds Films %v, films/a.mkv %v", b.exists("t2", "media/Films"), b.exists("t2", "media/films/a.mkv"))
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2'"); got != 0 {
				t.Errorf("the catalog has %d copies on t2", got)
			}
			// The walk that finds films tells the plan, which reports the file.
			b.asNTFS("")
			b.pass()
			if got := b.text("SELECT state FROM files"); got != "name-collision" {
				t.Errorf("the file is %s", got)
			}
		})
	}
	t.Run("a folder on the way", func(t *testing.T) {
		b := onNTFS(t)
		b.write("t1", "Films/a.mkv", "the film")
		b.write("t1", "Films/b.mkv", "another film")
		b.write("t2", "Films/b.mkv", "another film")
		b.scan(false)
		b.queuedOnT2("Films/a.mkv")
		if err := os.Mkdir(filepath.Join(b.dirs["t2"], "media", "films"), 0o755); err != nil {
			t.Fatal(err)
		}
		items := b.runNowOnT2()
		if got := outcomeFor(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
			t.Errorf("the copy ended %q, want name-refused: %+v", got, items)
		}
		if b.exists("t2", "media/Films/a.mkv") {
			t.Error("the copy is in Films, beside the user's films")
		}
	})
}

// eioOnNames fails, with EIO, a listing of a folder of t2 named name: the n-th one from the moment the bench reaches the
// point from, or from the start when from is empty.
func (b *bench) eioOnNames(from, name string, n int) {
	b.t.Helper()
	armed, seen := from == "", 0
	if from != "" {
		b.at[from] = func() { armed = true }
	}
	b.fail[b.dirs["t2"]] = func(call, folder string) error {
		if !armed || call != "names" || folder != name {
			return nil
		}
		seen++
		if seen == n {
			return syscall.EIO
		}
		return nil
	}
}

// A listing of an NTFS disk's folder that fails with EIO, the look before the copy, the one before the copy makes its
// folder, or the one right before its part takes its name, says the disk unknown, never that no name collides there
// (invariant 4): the disk is lost for the run, nothing is written at the copy's name, and the copy waits; the next run,
// the disk back, makes it.
func TestAKeyLookThatFailsWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, from, folder string
		n                  int
		there              bool // t2 holds Films already
	}{
		{"the look before the copy", "", "media", 1, false},
		{"the look before its folder is made", "mkdir-journaled", "media", 1, false},
		{"the look right before the part takes its name", "verified", "Films", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := onNTFS(t)
			b.write("t1", "Films/a.mkv", "the film")
			if tc.there {
				b.write("t1", "Films/b.mkv", "another film")
				b.write("t2", "Films/b.mkv", "another film")
			}
			b.scan(false)
			b.eioOnNames(tc.from, tc.folder, tc.n)
			items, sum := b.replicate("Films/a.mkv", ReplicateOptions{Now: true})
			if got := outcomeFor(items, "Films/a.mkv"); got != trash.OutcomeIOError || !sum.DiskLost {
				t.Errorf("the copy ended %q, lost %v: %+v", got, sum.DiskLost, items)
			}
			if b.exists("t2", "media/Films/a.mkv") {
				t.Fatal("the copy took its name past a look that failed")
			}
			delete(b.fail, b.dirs["t2"])
			items, _ = b.replicate("Films/a.mkv", ReplicateOptions{Now: true})
			if got := outcomeFor(items, "Films/a.mkv"); got != OutcomeReplicated {
				t.Errorf("the next run ended %q: %+v", got, items)
			}
			if b.read("t2", "Films/a.mkv") != "the film" {
				t.Error("t2's copy is not the film")
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// Atoll stopped, as by kill -9, right before a look of disk-key-look, a copy's or that of the folder it makes, while a
// user makes films on t2: the reprise ends what the stop left, and the next run refuses the copy, name-refused, with
// nothing written beside films and nothing left in t2's tmp/.
func TestAStopBeforeAKeyLook(t *testing.T) {
	for _, point := range []string{"journaled", "verified", "mkdir-journaled"} {
		t.Run(point, func(t *testing.T) {
			b := onNTFS(t)
			b.write("t1", "Films/a.mkv", "the film")
			b.scan(false)
			stop := errors.New("stopped")
			b.at[point] = func() {
				if err := os.Mkdir(filepath.Join(b.dirs["t2"], "media", "films"), 0o755); err != nil {
					t.Error(err)
				}
				panic(stop) //nolint:forbidigo // what stands for kill -9
			}
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop at %s: %v", point, err)
					}
				}()
				_, _ = Replicate(t.Context(), b.db, b.config, b, ReplicateOptions{Now: true}, func(*ReplicateItem) error { return nil })
			}()
			items, _ := b.replicate("", ReplicateOptions{Now: true})
			if got := outcomeFor(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
				t.Errorf("the run after the stop ended %q: %+v", got, items)
			}
			// What the job it ended kept in tmp/ goes the next time a command that writes opens the disk (§14.1), and
			// the walk of that pass tells the plan of films, which reports the file.
			b.pass()
			if got := b.text("SELECT state FROM files"); got != "name-collision" {
				t.Errorf("the file is %s", got)
			}
			if b.exists("t2", "media/Films") || len(b.parts("t2")) != 0 {
				t.Errorf("t2 holds Films %v, parts %v", b.exists("t2", "media/Films"), b.parts("t2"))
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// Atoll stopped, as by kill -9, as the rename of a copy behind on t2, an NTFS disk, or a folder's job there, is
// journaled, right before its key look; then, in the pass after the start, a user makes there a name of the new name's
// key once that pass walked t2, which no row says: the reprise undoes the rename, and the pass refuses it, name-refused,
// its copy or folder kept where it was, nothing made beside the user's name. It made the name beside it.
func TestAStopBeforeARenamesKeyLook(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, users, kept string
	}{
		{"a copy's rename", "Other/a.mkv", "Other/b.mkv", "media/Other/B.mkv", "media/Other/a.mkv"},
		{"a folder's job", "Other", "Shows", "media/shows", "media/Other/a.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := onNTFS(t)
			b.write("t1", "Other/a.mkv", "a film")
			b.write("t2", "Other/a.mkv", "a film")
			b.scan(false)
			b.rename("t1", tc.from, tc.to)
			stop := errors.New("stopped")
			b.at["journaled"] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop: %v", err)
					}
				}()
				b.settle()
			}()
			// Right as the rename first looks at its new name on t2, once the pass walked t2: after the reprise's look
			// at it, and the walk's listing of t2's root.
			walked, made, last := false, false, tc.to[strings.LastIndexByte(tc.to, '/')+1:]
			b.fail[b.dirs["t2"]] = func(call, name string) error {
				walked = walked || call == "names" && name == "media"
				if walked && !made && call == "stat" && name == last {
					made = true
					if err := os.MkdirAll(filepath.Join(b.dirs["t2"], tc.users), 0o755); err != nil {
						t.Error(err)
					}
				}
				return nil
			}
			out := b.pass()
			refused := false
			for _, op := range out.Operations {
				refused = refused || op.Disk == "t2" && op.Outcome == trash.OutcomeNameRefused
			}
			if !refused {
				t.Errorf("no rename on t2 refused: %+v", out.Operations)
			}
			if !b.exists("t2", tc.kept) || b.exists("t2", "media/"+tc.to) {
				t.Errorf("t2 holds %s %v, %s %v", tc.kept, b.exists("t2", tc.kept), tc.to, b.exists("t2", "media/"+tc.to))
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// A listing of t2's folder that fails with EIO as a rename there looks at its new name's key, the rename of a copy
// behind or a folder's job, says the disk unknown (invariant 4): nothing is renamed, and the next pass, the disk back,
// renames it.
func TestARenamesKeyLookThatFails(t *testing.T) {
	for _, tc := range []struct{ name, from, to, folder, renamed string }{
		{"a copy's rename", "Other/a.mkv", "Other/b.mkv", "Other", "media/Other/b.mkv"},
		{"a folder's job", "Other", "Shows", "media", "media/Shows/a.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := onNTFS(t)
			b.write("t1", "Other/a.mkv", "a film")
			b.write("t2", "Other/a.mkv", "a film")
			b.scan(false)
			b.rename("t1", tc.from, tc.to)
			// Once the pass walked t2 and decided the rename: each listing of the folder the new name goes in fails.
			decided := false
			b.at["decided"] = func() { decided = true }
			b.fail[b.dirs["t2"]] = func(call, name string) error {
				if decided && call == "names" && name == tc.folder {
					return syscall.EIO
				}
				return nil
			}
			b.settle()
			if b.exists("t2", tc.renamed) || !b.exists("t2", "media/Other/a.mkv") {
				t.Fatal("t2 renamed past a look that failed")
			}
			delete(b.fail, b.dirs["t2"])
			b.settle()
			if !b.exists("t2", tc.renamed) {
				t.Error("the next passes did not rename it")
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}
