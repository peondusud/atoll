package trash

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// own marks a disk's folders as made by Atoll, as a copy or a restore makes them.
func (b *bench) own(id string, folders ...string) {
	b.t.Helper()
	for _, f := range folders {
		res, err := b.db.ExecContext(b.t.Context(), "UPDATE entries SET created_by = 'atoll' WHERE disk_id = ? AND lpath = ? AND file_key IS NOT NULL", id, []byte(f))
		if err != nil {
			b.t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			b.t.Fatalf("no folder row %q on %s", f, id)
		}
	}
}

func (b *bench) there(id, path string) bool {
	_, err := os.Lstat(filepath.Join(b.dirs[id], "media", filepath.FromSlash(path)))
	return err == nil
}

// The folders Atoll made, which its own move to the trash left empty, go, the deepest first, each with its parent's
// time given back; a folder the user made stays (specs.md §17.5).
func TestAtollsEmptyFoldersGo(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/e/x", "u/x")
	b.own("t3", "d", "d/e")
	root := filepath.Join(b.dirs["t3"], "media")
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(root, at, at); err != nil {
		t.Fatal(err)
	}
	items, summary := b.prune()
	if len(items) != 3 || summary.DiskLost {
		t.Fatalf("%+v, %+v", items, summary)
	}
	if b.there("t3", "d") || !b.there("t3", "u") {
		t.Errorf("d there: %v, u there: %v", b.there("t3", "d"), b.there("t3", "u"))
	}
	if got := b.text("SELECT group_concat(CAST(path AS TEXT) || ' ' || state, ', ') FROM (SELECT path, state FROM ops WHERE kind = 'rmdir' ORDER BY id)"); got != "d/e done, d done" {
		t.Errorf("%q", got)
	}
	if got := b.count("SELECT count(*) FROM entries WHERE disk_id = 't3' AND lpath IN (x'64', x'642f65')"); got != 0 {
		t.Errorf("%d rows left", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'rmdir'"); got != 2 {
		t.Errorf("%d events", got)
	}
	if st, err := os.Stat(root); err != nil || !st.ModTime().Equal(at) {
		t.Errorf("the root's time: %v, %v", st.ModTime(), err)
	}
	// The other disks' folders are the user's.
	if !b.there("t1", "d/e") || !b.there("t2", "d/e") {
		t.Error("a folder of another disk went")
	}
}

// A folder that holds what the catalog does not know yet stays: only an empty folder can be removed, and the
// operation is undone.
func TestAFolderWithSomethingNewStays(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.own("t3", "d")
	if err := os.WriteFile(filepath.Join(b.dirs["t3"], "media", "d", "new"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.prune()
	if !b.there("t3", "d/new") {
		t.Error("the folder went")
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'rmdir'"); got != "undone" {
		t.Errorf("%q", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 0 {
		t.Errorf("%d events", got)
	}
}

// A folder whose name now holds another folder, made since the catalog recorded the first, is not Atoll's.
func TestAnotherFolderAtTheNameStays(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.own("t3", "d")
	// Between the journal and the look: the folder went, and another came at its name.
	b.at["rmdir-journaled"] = func() {
		folder := filepath.Join(b.dirs["t3"], "media", "d")
		if err := os.Rename(folder, folder+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(folder, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.prune()
	if !b.there("t3", "d") {
		t.Error("the other folder went")
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'rmdir'"); got != "undone" {
		t.Errorf("%q", got)
	}
}

// EIO from the removal makes the disk lost, and the operation stays planned; the next run finds the folder there and
// undoes it, and does not try again what it did not empty.
func TestARemovalThatFailsLosesTheDisk(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.own("t3", "d")
	b.failOn("t3", "rmdir", syscall.EIO)
	_, summary := b.prune()
	if !summary.DiskLost {
		t.Errorf("%+v", summary)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'rmdir'"); got != "planned" {
		t.Errorf("%q", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
		t.Errorf("the disk is %q", got)
	}
	delete(b.fail, b.dirs["t3"])
	b.prune()
	if got := b.text("SELECT state FROM ops WHERE kind = 'rmdir'"); got != "undone" {
		t.Errorf("after the reprise: %q", got)
	}
	if !b.there("t3", "d") {
		t.Error("the folder went")
	}
}

// A sync of the parent that fails once the folder is gone leaves the operation planned, and the next run finishes it.
func TestASyncAfterTheRemovalFails(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.own("t3", "d")
	removed := false
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		switch {
		case call == "rmdir":
			removed = true
		case call == "sync" && removed:
			return syscall.EIO
		}
		return nil
	}
	_, summary := b.prune()
	if !summary.DiskLost || b.there("t3", "d") {
		t.Errorf("%+v, folder there: %v", summary, b.there("t3", "d"))
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'rmdir'"); got != "planned" {
		t.Errorf("%q", got)
	}
	delete(b.fail, b.dirs["t3"])
	b.prune()
	if got := b.text("SELECT state FROM ops WHERE kind = 'rmdir'"); got != "done" {
		t.Errorf("after the reprise: %q", got)
	}
	if got := b.count("SELECT count(*) FROM entries WHERE disk_id = 't3' AND lpath = x'64'"); got != 0 {
		t.Errorf("%d rows", got)
	}
}

// A run that may not write removes nothing.
func TestADryRunRemovesNoFolder(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.own("t3", "d")
	s, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Prune(t.Context(), func(*PruneItem) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s.Left("t3", "d/x")
	if err := s.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !b.there("t3", "d/x") || b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("a dry run wrote")
	}
}
