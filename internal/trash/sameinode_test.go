package trash

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// What a file at a copy's name, or at its entry's in the trash, looks like with the copy's inode and not its content:
// another size, or another modification time, each alone, as a file that took the copy's name with the inode number of
// the copy just removed does, or the copy edited in place (specs.md §17.1, §17.2).
var notTheCopy = []struct {
	name, text string
	change     func(t *testing.T, full string)
}{
	{"another size", "edited", func(t *testing.T, full string) {
		t.Helper()
		at := modifiedAt(t, full)
		writeInPlace(t, full, "edited")
		setModifiedAt(t, full, at)
	}},
	{"another modification time", "y", func(t *testing.T, full string) {
		t.Helper()
		at := modifiedAt(t, full)
		writeInPlace(t, full, "y")
		setModifiedAt(t, full, at.Add(time.Second))
	}},
}

// A file that takes the copy's name between the look right before the move and the move, with the inode number the
// copy had, is moved to the trash in its place: the look after the move compares the inode, the size and the
// modification time, finds that it is not the copy, and the operation is resolved at once as a reprise would, by the
// table of §17.2: the file goes back to its name, untouched, the operation undone with an event that says so, and
// nothing of it stays in the trash (specs.md §17.1, step 4). By the inode alone, the move was done, and the user's file
// went to the trash as the copy.
func TestAFileWithTheCopysInodeGoesBackFromTheTrash(t *testing.T) {
	for _, tc := range notTheCopy {
		for _, driver := range []struct {
			name string
			hook func(b *bench, fire func()) func(call, name string) error
		}{
			{"renamed", func(_ *bench, fire func()) func(string, string) error {
				done := false
				return func(call, _ string) error {
					if call == "rename" && !done {
						done = true
						fire()
					}
					return nil
				}
			}},
			// Without RENAME_NOREPLACE, both names lead to the file between the link and the look at the source: an edit
			// through the one in the library, then that name removed, leave the link its only name.
			{"linked, the source edited then removed", func(b *bench, fire func()) func(string, string) error {
				syncs := 0
				return func(call, _ string) error {
					switch call {
					case "rename":
						return syscall.EINVAL
					case "sync":
						syncs++
						if syncs == syncsOfAFirstMove-1 {
							fire()
							if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "a")); err != nil {
								b.t.Fatal(err)
							}
						}
					}
					return nil
				}
			}},
		} {
			t.Run(driver.name+"/"+tc.name, func(t *testing.T) {
				b := newBench(t)
				source := filepath.Join(b.dirs["t3"], "media", "a")
				b.fail[b.dirs["t3"]] = driver.hook(b, func() { tc.change(t, source) })
				items, summary := b.prune()
				if len(items) != 1 || items[0].Outcome != OutcomeChanged || summary.DiskLost {
					t.Fatalf("%+v, %+v", items, summary)
				}
				delete(b.fail, b.dirs["t3"])
				if got := b.sourceText(); got != tc.text {
					t.Errorf("the file at the copy's name holds %q", got)
				}
				wantBack(t, b)
			})
		}
	}
}

// A reprise compares the file the trash's name holds with the copy's row in the same way, when that name alone has the
// copy's inode: another size or modification time is not the copy, and it goes back to its path, the operation undone
// with an event; with that path taken since, it stays where it is, the operation undone with an event, and the file at
// the path is left as it is. What both names lead to is one file, which a reprise still reads as edited in place
// (TestAnEditDuringTheSyncsOfAReprise); and nothing changed but the inode's place, it finishes (specs.md §17.2).
func TestARepriseKnowsTheCopyByMoreThanItsInode(t *testing.T) {
	// The fifth sync is the target folder's, right after the move: the disk is lost, the operation stays planned with
	// the copy in the trash, as a crash there leaves it.
	stopAfterTheMove := func(b *bench) {
		b.t.Helper()
		b.failNth("t3", "sync", syncsOfAFirstMove-1, syscall.EIO)
		if items, summary := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
			b.t.Fatalf("%+v, %+v", items, summary)
		}
		delete(b.fail, b.dirs["t3"])
		if b.sourceThere() {
			b.t.Fatal("the move did not go through")
		}
	}
	for _, tc := range notTheCopy {
		t.Run("back to its path/"+tc.name, func(t *testing.T) {
			b := newBench(t)
			stopAfterTheMove(b)
			tc.change(t, b.inTrash(1, "a"))
			items, _ := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeChanged {
				t.Fatalf("after the reprise: %+v", items)
			}
			if got := b.sourceText(); got != tc.text {
				t.Errorf("the file at the copy's name holds %q", got)
			}
			wantBack(t, b)
		})
		t.Run("its path taken since/"+tc.name, func(t *testing.T) {
			b := newBench(t)
			stopAfterTheMove(b)
			tc.change(t, b.inTrash(1, "a"))
			b.write("t3", "a", "theirs")
			b.prune()
			if got := b.sourceText(); got != "theirs" {
				t.Errorf("the file at the copy's name holds %q", got)
			}
			data, err := os.ReadFile(b.inTrash(1, "a"))
			if err != nil || string(data) != tc.text {
				t.Errorf("the file in the trash: %q, %v", data, err)
			}
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != catalog.OpUndone {
				t.Errorf("the operation is %q", got)
			}
			if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
				t.Errorf("%d events op-undone", got)
			}
			if got := b.count("SELECT count(*) FROM trash WHERE op_id = 1"); got != 0 {
				t.Errorf("%d entries", got)
			}
		})
	}
	t.Run("the copy, its change time alone moved", func(t *testing.T) {
		b := newBench(t)
		stopAfterTheMove(b)
		time.Sleep(20 * time.Millisecond) // the change time must be able to move, within a clock tick
		if err := os.Chmod(b.inTrash(1, "a"), 0o600); err != nil {
			t.Fatal(err)
		}
		if items, _ := b.prune(); len(items) != 0 {
			t.Errorf("after the reprise: %+v", items)
		}
		if got := b.text("SELECT state FROM ops WHERE id = 1"); got != catalog.OpDone {
			t.Errorf("the operation is %q", got)
		}
		if b.sourceThere() {
			t.Error("the copy came back")
		}
	})
}

// wantBack says that the operation of a move to the trash was undone with an event, and left nothing in the trash.
func wantBack(t *testing.T, b *bench) {
	t.Helper()
	if got := b.text("SELECT state FROM ops WHERE id = 1"); got != catalog.OpUndone {
		t.Errorf("the operation is %q", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events op-undone", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		t.Errorf("%d operations left planned", got)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries", got)
	}
	if _, err := os.Lstat(b.inTrash(1, "a")); !os.IsNotExist(err) {
		t.Errorf("the name in the trash: %v", err)
	}
}

// writeInPlace writes a file's content again, keeping its inode, as a program that writes into it does.
func writeInPlace(t *testing.T, full, text string) {
	t.Helper()
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func modifiedAt(t *testing.T, full string) time.Time {
	t.Helper()
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func setModifiedAt(t *testing.T, full string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(full, at, at); err != nil {
		t.Fatal(err)
	}
}
