package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// folderPool makes two disks, each with D/x, scanned; the user renames D to E on t1, and the next pass decides it and
// queues the folder's job on t2, which the pass after runs (specs.md §15.12, Folders).
func folderPool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "D/x", "the file at D/x")
	}
	b.scan(true)
	b.rename("t1", "D", "E")
	b.pass()
	b.now += 30_000_000_000
	return b
}

// rotateD renames the folder D on t2 away to F and makes a new D there with a file in it, as a tool that rotates a
// folder does.
func (b *bench) rotateD() {
	media := filepath.Join(b.dirs["t2"], "media")
	if err := os.Rename(filepath.Join(media, "D"), filepath.Join(media, "F")); err != nil {
		b.t.Error(err)
	}
	if err := os.Mkdir(filepath.Join(media, "D"), 0o755); err != nil {
		b.t.Error(err)
	}
	if err := os.WriteFile(filepath.Join(media, "D", "new"), []byte("a file of the new folder"), 0o644); err != nil {
		b.t.Error(err)
	}
}

// A folder's job renames the folder on another disk as one operation, rename(2) of the folder, which moves whatever the
// old name holds then (specs.md §15.12, Folders). A program that renames that folder away and makes a new one at its
// name, between the look right before the job's rename and the rename, has its new folder carried to the new name; the
// look after finds there a folder that is not the one journaled, and puts it back at the old name, free again, synced,
// the folders' times restored; the table then undoes the operation and the job goes, its copies following one by one
// (conformance/models/README.md, flaw 14, the maintainer's remedy, 2026-11-23). Before, the new folder stayed at the
// name the job gave it, with all it holds (invariant 15).
func TestAFolderMadeBeforeAFolderJobStaysAtItsName(t *testing.T) {
	b := folderPool(t)
	media := filepath.Join(b.dirs["t2"], "media")
	once := false
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "rename" && name == "E" && !once {
			once = true
			b.rotateD()
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
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND disk_id = 't2' AND state = 'undone'"); n != 1 {
		t.Errorf("%d renames undone on t2, want the folder's", n)
	}
	if n := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename'"); n != 0 {
		t.Errorf("%d folder jobs left, want the job gone", n)
	}
	// Nothing is lost: the passes after follow what the program did, the copy of D/x at F/x on t2.
	for range 3 {
		b.now += 30_000_000_000
		b.settle()
	}
	for _, path := range []string{"D/new", "F/x"} {
		if !b.exists("t2", "media/"+path) {
			t.Errorf("t2 no longer holds %s", path)
		}
	}
}

// The folder's put back stopped as kill -9 stops it, once it is back at the old name and before either name is synced,
// then the next pass's reprise: the operation found undone by the table of §17.2, the new folder at D, as with nothing
// stopped; its sync failing, the table says the same of the names as they are; and the put back itself failing, the
// new folder stays at E, for the table, nothing lost (specs.md §15.12, §17.2).
func TestAFolderPutBackStoppedOrFailing(t *testing.T) {
	media := func(b *bench, path string) bool { return b.exists("t2", "media/"+path) }
	for _, tc := range []struct {
		name string
		// fail is what fails once the job's rename ran; crash a point the pass stops at.
		fail  func(call, name string) error
		crash string
		// at is where the new folder's file ends.
		at string
	}{
		{name: "stopped at put-back", crash: "put-back", at: "D/new"},
		{name: "its sync failing", fail: func(call, name string) error {
			if call == "syncentry" && name == "D" {
				return syscall.EIO
			}
			return nil
		}, at: "D/new"},
		{name: "the put back failing", fail: func(call, name string) error {
			if call == "rename" && name == "D" {
				return syscall.EIO
			}
			return nil
		}, at: "E/new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := folderPool(t)
			moved, failed := false, false
			b.fail[b.dirs["t2"]] = func(call, name string) error {
				if call == "rename" && name == "E" && !moved {
					moved = true
					b.rotateD()
					return nil
				}
				if moved && !failed && tc.fail != nil {
					if err := tc.fail(call, name); err != nil {
						failed = true
						return err
					}
				}
				return nil
			}
			if tc.crash != "" {
				b.crashAt(tc.crash)
			} else {
				b.pass()
			}
			b.fail[b.dirs["t2"]] = nil
			b.pass()
			if !moved || tc.fail != nil && !failed {
				t.Fatalf("the job's rename %v, the failure %v", moved, failed)
			}
			if !media(b, tc.at) {
				t.Errorf("the new folder's file is not at %s: D/new %v, E/new %v", tc.at, media(b, "D/new"), media(b, "E/new"))
			}
			if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
				t.Errorf("%d operations left planned", n)
			}
		})
	}
}
