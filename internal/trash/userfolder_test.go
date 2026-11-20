package trash

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// removedUserFolder removes a folder a user removed through Atoll's own view, in a session that may write or not.
func (b *bench) removedUserFolder(id, folder string, apply bool) {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	if err := s.RemoveUserFolder(b.t.Context(), id, folder); err != nil {
		b.t.Fatal(err)
	}
}

// A folder a user removed through Atoll's own view goes from a disk once the catalog holds nothing of that disk below
// it, whoever made it; one that still holds something stays for a later pass, and so does every folder in a run that
// may not write, or on a disk that cannot be written to (specs.md §21.2).
func TestAUserFolderGoesOnceEmpty(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.prune()
	if !b.there("t3", "d") || b.there("t3", "d/x") {
		t.Fatal("the prune did not leave d empty on t3")
	}
	b.removedUserFolder("t3", "d", false)
	b.removedUserFolder("t3", "none", true)
	b.removedUserFolder("t1", "d", true)
	b.ro[b.dirs["t3"]] = true
	b.removedUserFolder("t3", "d", true)
	delete(b.ro, b.dirs["t3"])
	if !b.there("t3", "d") || !b.there("t1", "d/x") || b.count("SELECT count(*) FROM ops WHERE kind = 'rmdir'") != 0 {
		t.Fatal("a folder went that had to stay")
	}
	b.removedUserFolder("t3", "d", true)
	if b.there("t3", "d") || b.text("SELECT state FROM ops WHERE kind = 'rmdir'") != "done" {
		t.Error("the empty folder stays")
	}
	if got := b.count("SELECT count(*) FROM entries WHERE disk_id = 't3' AND lpath = x'64'"); got != 0 {
		t.Errorf("%d rows for the folder", got)
	}
}

// A disk lost while the session opens it takes no removal: the folder stays for a later pass.
func TestAUserFolderOnADiskLostWhileOpened(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.prune()
	b.failTmp()
	b.removedUserFolder("t3", "d", true)
	if !b.there("t3", "d") || b.count("SELECT count(*) FROM ops WHERE kind = 'rmdir'") != 0 || b.diskState("t3") != "missing" {
		t.Errorf("the folder there %v, t3 %s", b.there("t3", "d"), b.diskState("t3"))
	}
}

// The folders Atoll's own operation left on a disk lost before they were looked at wait for the next run that writes,
// which looks at them from the journal (specs.md §17.5).
func TestTheFoldersOfADiskLostWaitForTheNextRun(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.own("t3", "d")
	s := b.opened()
	f, err := catalog.FileAt(t.Context(), b.db, "d/x")
	if err != nil || f == nil {
		t.Fatalf("%v", err)
	}
	if got, err := s.Send(t.Context(), f, copyOn(f, "t3"), "surplus"); err != nil || got != OutcomeTrashed {
		t.Fatalf("%s, %v", got, err)
	}
	if err := s.Lose(t.Context(), "t3", errors.New("gone")); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if !b.there("t3", "d") {
		t.Fatal("a folder of a disk lost was removed")
	}
	b.prune()
	if b.there("t3", "d") || b.text("SELECT state FROM ops WHERE kind = 'rmdir'") != "done" {
		t.Error("the next run left the folder")
	}
}

// A folder that cannot be removed for another reason than the disk stays, its operation undone, with a line of the
// log; a disk that fails there is lost, its operation planned (specs.md §17.5).
func TestAUserFolderThatCannotBeRemoved(t *testing.T) {
	for _, tc := range []struct {
		err   syscall.Errno
		state string
		disk  string
	}{{syscall.EACCES, "undone", "online"}, {syscall.EIO, "planned", "missing"}} {
		b := newBench(t)
		b.addFiles("d/x")
		b.prune()
		b.failOn("t3", "rmdir", tc.err)
		b.removedUserFolder("t3", "d", true)
		if !b.there("t3", "d") || b.text("SELECT state FROM ops WHERE kind = 'rmdir'") != tc.state || b.diskState("t3") != tc.disk {
			t.Errorf("%v: the operation is %s, t3 %s", tc.err, b.text("SELECT state FROM ops WHERE kind = 'rmdir'"), b.diskState("t3"))
		}
	}
	// One that holds what the catalog does not know yet stays, and its operation is undone.
	b := newBench(t)
	b.addFiles("d/x")
	b.prune()
	if err := os.WriteFile(filepath.Join(b.dirs["t3"], "media", "d", "new"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.removedUserFolder("t3", "d", true)
	if !b.there("t3", "d/new") || b.text("SELECT state FROM ops WHERE kind = 'rmdir'") != "undone" {
		t.Error("a folder that holds something went")
	}
}
