//go:build flaws

package jobs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// The flaws the TLA+ model found and that wait for the maintainer's remedy (conformance/models/README.md), each a test
// that fails as the flaw says until then, kept out of go test ./... by the build tag flaws; make model-flaws runs them.

// contentAnywhere says whether a file of that content is anywhere on a disk of the bench: in the library or in a trash.
func (b *bench) contentAnywhere(content string) []string {
	b.t.Helper()
	var at []string
	for id, dir := range b.dirs {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, ".db") {
				return nil //nolint:nilerr // what cannot be read holds nothing for this test
			}
			if data, err := os.ReadFile(path); err == nil && string(data) == content {
				rel, _ := filepath.Rel(dir, path)
				at = append(at, id+":"+filepath.ToSlash(rel))
			}
			return nil
		})
	}
	return at
}

// Flaw 8: the SSD cache's transfer leans on the catalog alone for the copies on the data disks (specs.md §19,
// watch.CacheTransfers, jobs.transfer): it looks at the copy on the cache right before its move, and at nothing else,
// then purges it from the cache's trash at once. A user who deletes the copies on the data disks directly, after the
// pass's walk and before the transfer's move, so loses the file for good: the copy on the cache, its last, goes to the
// trash and is purged there, where a deletion made directly on a disk goes to the trash and stays there (invariants 1
// and 10). The model found it with an edit in place of the copy on the data disk too (Replication.tla, Cache.cfg).
func TestFlawATransferPurgesTheLastCopyOfAFileDeletedMeanwhile(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	const content = "episode"
	b.write("c", "show/e1.mkv", content)
	b.write("t1", "show/e1.mkv", content)
	b.write("t2", "show/e1.mkv", content)
	b.scan(true)
	deleted := false
	b.fail[b.dirs["c"]] = func(call, _ string) error {
		if call == "rename" && !deleted {
			// The user deletes the file on both data disks, directly, right before the transfer moves its copy on the
			// cache to the trash.
			deleted = true
			for _, id := range []string{"t1", "t2"} {
				if err := os.Remove(filepath.Join(b.dirs[id], "media", "show", "e1.mkv")); err != nil {
					t.Error(err)
				}
			}
		}
		return nil
	}
	out := b.pass()
	b.fail[b.dirs["c"]] = nil
	if !deleted {
		t.Fatalf("no move reached the cache: %+v", out.Operations)
	}
	if at := b.contentAnywhere(content); len(at) == 0 {
		t.Errorf("flaw 8: the file is on no disk, in no trash: the transfer purged its last copy (%s), a user's deletion made directly "+
			"on the data disks with no trash", outcomeOf(out, KindTransfer, "show/e1.mkv"))
	}
}

// Flaw 8, an edit: a user who writes into the copy on a data disk directly in the same window, the file at its level of
// one copy, leaves the version the catalog vouches for on no disk once the transfer purges the copy on the cache,
// where the next pass would have sent that copy to the trash as overwritten, and kept it there (invariant 1).
func TestFlawATransferPurgesTheVersionACopyOnADataDiskLostMeanwhile(t *testing.T) {
	// cacheBench's more begins in [defaults]: one replica fewer, the file at its level of one copy.
	b := cacheBench(t, "replicas = 0\n[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	const content = "episode"
	b.write("c", "show/e1.mkv", content)
	b.write("t1", "show/e1.mkv", content)
	b.scan(true)
	edited := false
	b.fail[b.dirs["c"]] = func(call, _ string) error {
		if call == "rename" && !edited {
			edited = true
			if err := os.WriteFile(filepath.Join(b.dirs["t1"], "media", "show", "e1.mkv"), []byte("a better release"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	out := b.pass()
	b.fail[b.dirs["c"]] = nil
	if !edited {
		t.Fatalf("no move reached the cache: %+v", out.Operations)
	}
	if at := b.contentAnywhere(content); len(at) == 0 {
		t.Errorf("flaw 8: the version the catalog vouches for is on no disk, in no trash: the transfer purged it (%s)",
			outcomeOf(out, KindTransfer, "show/e1.mkv"))
	}
}

// Flaw 9: Atoll's own view presents a file from its original (specs.md §7, §21.2), which the catalog says, and reads it
// there alone with read.policy = "oldest". The SSD cache's transfer moves the copy on the cache, the file's original, to
// the trash, then syncs, then commits its row's removal (§17.1): between the move and the commit, the view presents the
// file from the cache, where nothing is at its path any more, while the data disks hold it in sync, and an open through
// the view finds nothing (invariant 14). The model found it as Atoll's own view presenting nothing of a file a user wrote
// through it (Replication.tla, CacheView.cfg).
func TestFlawTheViewOpensNothingWhileATransferMovesTheOriginal(t *testing.T) {
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
		t.Errorf("flaw 9: while the transfer moved the original, the view read %q of a file the data disks hold in sync", read)
	}
}

// Flaw 10: a pass reads the view's operations once, before its transfers run (jobs.transfers, touchedByView), and the
// view journals a write when it opens a file, done at once, then writes into the copy it presents (writer.Open). A
// program that opens the file to write through the view after that read and before the transfer's move holds the copy
// on the cache open, which the look right before the move finds as its row says, as nothing was written yet: the copy
// goes to the trash and is purged, and what the program writes then goes to a file no name holds any more, lost once it
// closes it, while the view presents the file from the cache, where nothing is, for good (invariants 1 and 14).
func TestFlawAWriteOpenedThroughTheViewDuringATransferIsLost(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	const content = "episode"
	b.write("c", "show/e1.mkv", content)
	b.write("t1", "show/e1.mkv", content)
	b.write("t2", "show/e1.mkv", content)
	b.scan(true)
	w := b.viewOf()
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
	var f *writer.File
	b.at["journaled"] = func() {
		// Right after the transfer journaled its move: a program opens the file to write through the view.
		n, err := tree.Lookup(t.Context(), "show/e1.mkv")
		if err != nil || n == nil || n.Disk != "c" {
			t.Errorf("the view presents %+v (%v)", n, err)
			return
		}
		if f, err = w.Open(t.Context(), "show/e1.mkv", n, viewer); err != nil {
			t.Errorf("open to write: %v", err)
		}
	}
	out := b.pass()
	if f == nil {
		t.Fatalf("nothing opened: %+v", out.Operations)
	}
	const better = "a better release"
	if _, err := f.WriteAt([]byte(better), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if at := b.contentAnywhere(better); len(at) == 0 {
		t.Errorf("flaw 10: what the program wrote through the view is on no disk, in no trash (transfer %s)",
			outcomeOf(out, KindTransfer, "show/e1.mkv"))
	}
}

// Flaw 11: the reprise of an operation link of the link guard gives the names of its file the change time it finds,
// "as what it had before is gone with the crash" (specs.md §17.2, ops.recoverMakeLink, and recoverDropLink for its
// removal). A program that writes into the copy, keeping its size and its modification time, as a tagger may, while
// Atoll is down after a link it made and before its commit, moves only the change time, which the copy's row then takes
// for the link's: the write is never seen, never reaches the other copy, and a scrub would read it as corruption and
// repair it away (invariant 9, G4). The model found it with Atoll killed right after the link (Replication.tla,
// GuardRewrite.cfg), as flaw 1's limit was for a copy's rename.
func TestFlawAWriteKeepingTheDatesIsHiddenByTheRepriseOfALink(t *testing.T) {
	b := newBenchWith(t, "[trash]\nlink_guard = true\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.write("t2", "a", "content")
	b.scan(true)
	b.crashAt("linked")
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'link' AND state = 'planned'"); n == 0 {
		t.Fatal("no link left planned")
	}
	// While Atoll is down, a program rewrites the copy on t1 to the same size, and sets its modification time back.
	full := filepath.Join(b.dirs["t1"], "media", "a")
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	b.changeAgain("t1", "a", "CONTENT")
	if err := os.Chtimes(full, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	b.now = b.ctimeOf("t1", "a") + settleNS
	b.settle()
	b.now += settleNS
	b.settle()
	data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if state := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); string(data) != "CONTENT" && state == "ok" {
		t.Errorf("flaw 11: the write on t1 never reached t2, whose copy is %s with %q: the reprise of the link took its "+
			"change time for the link's", state, data)
	}
}
