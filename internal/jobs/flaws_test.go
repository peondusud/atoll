//go:build flaws

package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The flaws the TLA+ model found and that wait for the maintainer's remedy (conformance/models/README.md), each a test
// that fails as the flaw says until then, kept out of go test ./... by the build tag flaws; make model-flaws runs them.

// Flaw 6: an operation mkdir left planned, as a disk that fails at the mkdir leaves it, finishes at the reprise on any
// folder found at its name (specs.md §17.2, ops.recoverMkdir), and gives it the mode and owner of its model while it
// holds nothing: a folder a user made there meanwhile, private, comes out with the model's mode, and is recorded as
// Atoll's own, which §17.5 removes once Atoll leaves it empty. The model found it through a rename (Rename.tla); a
// copy's missing parent is made, and resumed, the same way.
func TestFlawAFolderAUserMadeIsTakenByTheReprise(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	// t2 fails as its folder p is made: the disk cannot be relied on, and the operation stays planned.
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
		t.Errorf("flaw 6: the folder a user made private on t2 has the mode %04o after the reprise, its model's", got)
	}
	if by := b.text("SELECT COALESCE(created_by, '') FROM entries WHERE disk_id = 't2' AND CAST(lpath AS TEXT) = 'p'"); by == "atoll" {
		t.Errorf("flaw 6: the folder a user made on t2 is recorded as Atoll's own, %q, for §17.5 to remove once empty", by)
	}
}

// Flaw 7: a pass takes a rename to the other copy with rename(2), which moves whatever the old name holds then. A file a
// program saves at that name between the look right before the move and the move goes to the new name in the copy's
// place; the look after finds it is not the copy, and the operation is undone with it left there, as §17.2's table
// leaves what a rename's target holds (target other, source absent), where a move to the trash puts it back. The user's
// file is then at a name Atoll gave it (invariant 15).
func TestFlawAFileSavedAtTheOldNameFollowsTheRename(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "content")
	b.write("t2", "a", "content")
	b.scan(true)
	b.rename("t1", "a", "b")
	old := filepath.Join(b.dirs["t2"], "media", "a")
	const saved = "a new file, saved at the old name"
	once := false
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "rename" && name == "b" && !once {
			// An atomic save at the copy's old name, right before Atoll's move.
			once = true
			if err := os.WriteFile(old+".new", []byte(saved), 0o644); err != nil {
				t.Error(err)
			}
			if err := os.Rename(old+".new", old); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.settle()
	b.fail[b.dirs["t2"]] = nil
	if !once {
		t.Fatal("no rename reached t2")
	}
	at := func(path string) string {
		data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", path))
		if err != nil {
			return "absent"
		}
		return string(data)
	}
	if got := at("a"); got != saved {
		t.Errorf("flaw 7: the file saved at a on t2 is not there (%s), and b holds %q: Atoll's rename moved it", got, at("b"))
	}
}
