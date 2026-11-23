package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// An operation mkdir left planned, as a disk that fails at the mkdir leaves it, finishes at its reprise only on the
// folder of the inode it journaled right after the mkdir (specs.md §17.2, §17.4): a folder a user made at that name
// meanwhile, private, is theirs, and stays as they made it, the operation undone and no row saying it is Atoll's. The
// reprise once finished on any folder there, gave it its model's mode and recorded it as Atoll's own
// (conformance/models/README.md, flaw 6).
func TestAFolderAUserMadeIsNotTakenByTheReprise(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	// t2 fails as its folder p is made: the disk cannot be relied on, and the operation stays planned, with no inode.
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "mkdir" && name == "p" {
			return syscall.EIO
		}
		return nil
	}
	b.replicate("p", ReplicateOptions{Now: true})
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "planned" {
		t.Fatalf("the operation mkdir: %q", got)
	}
	b.fail[b.dirs["t2"]] = nil
	// Meanwhile a user makes that folder on t2, private, their own.
	mine := filepath.Join(b.dirs["t2"], "media", "p")
	if err := os.Mkdir(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	b.exec("UPDATE disks SET state = 'online'")
	b.replicate("p", ReplicateOptions{Now: true})
	if got := b.modeOf("t2", "p"); got != 0o700 {
		t.Errorf("the folder a user made private on t2 has the mode %04o after the reprise", got)
	}
	if n := b.count("SELECT count(*) FROM entries WHERE disk_id = 't2' AND CAST(lpath AS TEXT) = 'p' AND created_by = 'atoll'"); n != 0 {
		t.Errorf("the folder a user made on t2 is recorded as Atoll's own (%d rows)", n)
	}
	if got := b.text("SELECT group_concat(state) FROM ops WHERE kind = 'mkdir'"); got != "undone" {
		t.Errorf("the operation mkdir: %q", got)
	}
	if got := b.read("t2", "p/a"); got != "content" {
		t.Errorf("the copy in the user's folder: %q", got)
	}
}

// What the remedy leaves: a stop between the mkdir and the journal of its inode leaves a folder nothing says is
// Atoll's, which the reprise leaves as mkdir made it, its operation undone with an op-undone event (specs.md §17.2),
// and the copy then goes into it as into any folder there.
func TestAFolderMadeBeforeItsInodeIsJournaledIsLeftAsMade(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	b.scan(false)
	withUmask(t, 0o027)
	b.replicateStopping("p", "mkdir-unkeyed")
	if got := b.text("SELECT state || ' ' || COALESCE(file_key, '') FROM ops WHERE kind = 'mkdir'"); got != "planned " {
		t.Fatalf("the operation mkdir: %q", got)
	}
	b.replicate("p", ReplicateOptions{Now: true})
	if got := b.modeOf("t2", "p"); got != 0o750 {
		t.Errorf("the folder after the reprise: %04o, as mkdir made it", got)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "undone" {
		t.Errorf("the operation mkdir: %q", got)
	}
	if n := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); n != 1 {
		t.Errorf("op-undone events: %d", n)
	}
	if got := b.read("t2", "p/a"); got != "content" {
		t.Errorf("the copy: %q", got)
	}
}
