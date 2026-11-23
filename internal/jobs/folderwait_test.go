package jobs

import (
	"syscall"
	"testing"
)

// A folder renamed on t1, its job run on t2, whose rename of the folder goes through and whose sync fails: the operation
// stays planned, t2 missing. The user renames the folder back on t1. While t2 is away, the files with a copy on t2 below
// the folder's old or new name decide nothing, as a file with a job running does, so that no rename back clears t2's
// rows with no disk operation and drops the job above them (specs.md §15.12; conformance/models/README.md, flaw 16, the
// maintainer's remedy, 2026-11-23). Back, t2's reprise finishes the folder's operation, its rows following, and the
// rename back is then decided, its job carrying the folder back on t2 with all it holds: a file that only t2 holds ends
// at D/y, where the user renamed its folder back. Before, the rename back was decided while t2 was away, the reprise moved
// in the catalog only the copies with another copy at their own path elsewhere, and the next walk read Atoll's own
// rename of the file that only t2 holds as the user's: it took the name E/y, which the user had taken back.
func TestAFolderJobLeftPlannedHoldsTheFilesBelowItUntilItsReprise(t *testing.T) {
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
	// While t2 is away, the file with a copy below its folder's operation decides nothing: its row on t1 still says E/x.
	if path := b.text("SELECT CAST(lpath AS TEXT) FROM files WHERE CAST(lpath AS TEXT) LIKE '%/x'"); path != "E/x" {
		t.Errorf("the file at x was decided while t2 was away: it is at %s", path)
	}
	if n := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename' AND dst_disk = 't2'"); n != 1 {
		t.Errorf("%d folder jobs on t2 while it is away, want the one left planned kept", n)
	}
	b.gone[b.dirs["t2"]] = false
	for range 3 {
		b.now += 30_000_000_000
		b.settle()
	}
	path := b.text("SELECT CAST(f.lpath AS TEXT) FROM files f JOIN copies c ON c.file_id = f.id WHERE c.disk_id = 't2' AND CAST(f.lpath AS TEXT) LIKE '%/y'")
	if path != "D/y" || !b.exists("t2", "media/D/y") {
		t.Errorf("the file t2 alone holds is at %s (D/y there: %v), where the user renamed its folder back to D", path, b.exists("t2", "media/D/y"))
	}
	if !b.exists("t2", "media/D/x") || !b.exists("t1", "media/D/x") {
		t.Errorf("D/x on t1 %v, on t2 %v", b.exists("t1", "media/D/x"), b.exists("t2", "media/D/x"))
	}
	if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
		t.Errorf("%d operations left planned", n)
	}
}
