//go:build flaws

package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Flaw 13 of conformance/models/README.md (Folders.tla, FlawExchangeSaved.cfg, NamesKept): a pass gets round a
// cycle of renames on a disk by exchanging two copies' names in one step, renameat2 with RENAME_EXCHANGE, which swaps
// whatever the two names hold then (specs.md §15.12, A cycle). A file a program saves at one of the two names between
// the look right before the exchange and the exchange arrives at the other name: the look after finds that name not
// holding the copy, the table of §17.2 finishes the other operation and undoes this one, and what is at its target, the
// file saved, stays there, at a name Atoll gave it (invariant 15), as a file saved at a rename's old name did before
// flaw 7's remedy put it back.
func TestFlawAFileSavedBeforeAnExchangeFollowsIt(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "the file at a")
		b.write(id, "b", "the file at b, longer")
	}
	b.scan(true)
	// The user swaps a and b on t1, through a third name; the next pass renames both copies of t2, each path-taken at
	// the other's name, by an exchange of a and b there.
	b.rename("t1", "a", "c")
	b.rename("t1", "b", "a")
	b.rename("t1", "c", "b")
	b.pass()
	b.now += 30_000_000_000
	at := filepath.Join(b.dirs["t2"], "media", "b")
	const saved = "a file a program saved at b, on t2"
	once := false
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "exchange" && name == "b" && !once {
			// An atomic save at one of the two names, right before the exchange.
			once = true
			if err := os.WriteFile(at+".new", []byte(saved), 0o644); err != nil {
				t.Error(err)
			}
			if err := os.Rename(at+".new", at); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	b.fail[b.dirs["t2"]] = nil
	if !once {
		t.Fatal("no exchange reached t2")
	}
	read := func(path string) string {
		data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", path))
		if err != nil {
			return "absent"
		}
		return string(data)
	}
	if got := read("b"); got != saved {
		t.Errorf("the file saved at b on t2 is not there (b holds %q), and a holds %q", got, read("a"))
	}
	if got := read("a"); got == saved {
		t.Errorf("the file saved at b is at a, a name the exchange gave it")
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned'"); n != 0 {
		t.Errorf("%d renames left planned", n)
	}
}

// Flaw 16 of conformance/models/README.md (Folders.tla, FlawFolderBack.cfg, RenamesAreUsers): a folder renamed on t1,
// its job run on t2, whose rename of the folder goes through and whose sync fails: the operation stays planned, t2
// missing. The user renames the folder back on t1. The next pass, t2 away, decides the rename back: t2's copies get
// old_lpath NULL with no disk operation, and the job above them goes (specs.md §15.12, A disk away). Back, t2's reprise
// finishes the folder's operation, its new name holding the folder: of its copies, only those with another copy at its
// own path elsewhere follow (§17.2), so a file that only t2 holds keeps its row at the old name while it sits at the
// new one, and the next walk reads Atoll's own rename as the user's: the file takes the folder's new name, which the
// user had taken back.
func TestFlawAFolderJobLeftPlannedMovesAFileARenameBackClears(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "D/x", "the file at D/x")
	b.write("t2", "D/x", "the file at D/x")
	b.write("t2", "D/y", "a file t2 alone holds")
	b.scan(true)
	b.rename("t1", "D", "E")
	b.pass()
	b.now += 30_000_000_000
	// The folder's job on t2: the rename goes through, the sync of the folder at its new name fails.
	once := false
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "syncentry" && name == "E" && !once {
			once = true
			return syscall.EIO
		}
		return nil
	}
	b.pass()
	if !once {
		t.Fatal("no folder job reached t2")
	}
	b.fail[b.dirs["t2"]] = nil
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned' AND disk_id = 't2'"); n != 1 {
		t.Fatalf("%d renames planned on t2, want the folder's", n)
	}
	// The user renames the folder back on t1, while t2 is away.
	b.rename("t1", "E", "D")
	b.gone[b.dirs["t2"]] = true
	b.settle()
	b.now += 30_000_000_000
	b.settle()
	b.gone[b.dirs["t2"]] = false
	for range 3 {
		b.now += 30_000_000_000
		b.settle()
	}
	path := b.text("SELECT CAST(f.lpath AS TEXT) FROM files f JOIN copies c ON c.file_id = f.id WHERE c.disk_id = 't2' AND CAST(f.lpath AS TEXT) LIKE '%/y'")
	if path != "D/y" || !b.exists("t2", "media/D/y") {
		t.Errorf("the file t2 alone holds is at %s (D/y there: %v), where the user renamed its folder back to D", path, b.exists("t2", "media/D/y"))
	}
}

// Flaw 14 of conformance/models/README.md (Folders.tla, FlawFolderTaken.cfg, NamesKept): a folder renamed on one disk
// is renamed on the other by a job, as one operation, rename(2) of the folder, which moves whatever the old name holds
// then (specs.md §15.12, Folders). A program that renames that folder away and makes a new one at its name, between
// the look right before the job's rename and the rename, as a tool that rotates a folder does, has its new folder
// carried to the new name: the look after finds there a folder that is not the one journaled, the table of §17.2 undoes
// the operation, and what is at its target stays there, at a name Atoll gave it (invariant 15), with all it holds.
func TestFlawAFolderMadeBeforeAFolderJobFollowsIt(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "D/x", "the file at D/x")
	}
	b.scan(true)
	b.rename("t1", "D", "E")
	b.pass()
	b.now += 30_000_000_000
	media := filepath.Join(b.dirs["t2"], "media")
	once := false
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "rename" && name == "E" && !once {
			// The folder renamed away and a new one made at its name, with a file in it, right before the job's rename.
			once = true
			if err := os.Rename(filepath.Join(media, "D"), filepath.Join(media, "F")); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(filepath.Join(media, "D"), 0o755); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(media, "D", "new"), []byte("a file of the new folder"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	b.fail[b.dirs["t2"]] = nil
	if !once {
		t.Fatal("no folder job reached t2")
	}
	if _, err := os.Stat(filepath.Join(media, "D", "new")); err != nil {
		t.Errorf("the folder made at D on t2 is no longer there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(media, "E", "new")); err == nil {
		t.Errorf("the folder made at D on t2 is at E, a name the folder's job gave it")
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned'"); n != 0 {
		t.Errorf("%d renames left planned", n)
	}
}
