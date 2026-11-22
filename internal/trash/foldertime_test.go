package trash

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// dated gives folders below the root of t3 their modification times, by path from the root, "" for the root itself.
func (b *bench) dated(times map[string]time.Time) {
	b.t.Helper()
	for folder, at := range times {
		if err := os.Chtimes(filepath.Join(b.dirs["t3"], "media", filepath.FromSlash(folder)), at, at); err != nil {
			b.t.Fatal(err)
		}
	}
}

// timeOf gives the modification time of what a path names.
func timeOf(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// folderTimes are times no test run gives a folder by itself, each its own, down to the nanosecond.
var folderTimes = map[string]time.Time{
	"":    time.Unix(1_000_000_000, 111_111_111),
	"d":   time.Unix(1_100_000_000, 222_222_222),
	"d/e": time.Unix(1_200_000_000, 333_333_333),
}

// A folder Atoll makes in the trash takes the modification time of the one it stands for, as it takes its mode and its
// owner (specs.md §17.1, step 3): the operation's folder that of the root, and each folder of the logical path its own,
// the one the copy went in too, once the copy is there. The folders of the root keep theirs.
func TestAFolderOfTheTrashTakesTheTimeOfTheOneItStandsFor(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/e/x")
	b.dated(folderTimes)
	if items, _ := b.prune(); len(items) != 2 || items[1].Path != "d/e/x" || items[1].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	for folder, want := range folderTimes {
		if got := timeOf(t, b.inTrash(2, folder)); !got.Equal(want) {
			t.Errorf("in the trash, %q has %v, want %v", folder, got, want)
		}
		if got := timeOf(t, filepath.Join(b.dirs["t3"], "media", filepath.FromSlash(folder))); !got.Equal(want) {
			t.Errorf("in the root, %q has %v, want %v", folder, got, want)
		}
	}
	// The folders that stand for none keep their own.
	if got := timeOf(t, filepath.Dir(b.inTrash(2, ""))); got.Equal(folderTimes[""]) {
		t.Errorf("the folder of the date took the root's time")
	}
}

// failSetMtime makes the nth time given to a folder of that name on t3 fail with err, or stop the run as kill -9 does
// with stop.
func (b *bench) failSetMtime(name string, n int, err error) {
	seen := 0
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		if call != "setmtime" || folder != name {
			return nil
		}
		seen++
		if seen != n {
			return nil
		}
		if errors.Is(err, errStopped) {
			panic(err) //nolint:forbidigo // what stands for kill -9
		}
		return err
	}
}

var errStopped = errors.New("stopped")

// The time of the folder the copy goes in is the last thing given to the trash before the operation is durable: a
// device that fails then leaves it planned, the disk lost, as any other step does, and the next run finishes it. A
// driver that refuses the time only leaves the folder as the move left it, as a folder's date protects nothing (specs.md
// §17.1, step 5).
func TestTheTimeOfTheFolderTheCopyWentIn(t *testing.T) {
	t.Run("a device that fails", func(t *testing.T) {
		b := newBench(t)
		b.failSetMtime("1", 1, syscall.EIO)
		items, summary := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
		delete(b.fail, b.dirs["t3"])
		b.prune()
		if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "done" {
			t.Errorf("after the reprise, the operation is %q", got)
		}
		if b.sourceThere() || inodeOf(t, b.inTrash(1, "a")) == 0 {
			t.Error("the copy is not in the trash alone")
		}
	})
	t.Run("a driver that refuses", func(t *testing.T) {
		b := newBench(t)
		b.failSetMtime("1", 1, syscall.EPERM)
		if items, summary := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed || summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "done" {
			t.Errorf("the operation is %q", got)
		}
	})
	// Stopped right there, as kill -9 stops it: the copy is in the trash, and the reprise finishes the move. The folder
	// keeps the time the move gave it, as a folder time a crash left moved stays moved (specs.md §17.2).
	t.Run("a run stopped", func(t *testing.T) {
		b := newBench(t)
		b.failSetMtime("1", 1, errStopped)
		func() {
			defer func() {
				if err, ok := recover().(error); !ok || !errors.Is(err, errStopped) {
					t.Fatalf("it did not stop: %v", err)
				}
			}()
			b.prune()
		}()
		delete(b.fail, b.dirs["t3"])
		if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "planned" {
			t.Fatalf("the operation is %q", got)
		}
		s := b.opened()
		s.Close()
		if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "done" {
			t.Errorf("after the reprise, the operation is %q", got)
		}
		if b.sourceThere() || inodeOf(t, b.inTrash(1, "a")) == 0 {
			t.Error("the copy is not in the trash alone")
		}
		if got := b.count("SELECT count(*) FROM trash WHERE op_id = 1"); got != 1 {
			t.Errorf("%d entries", got)
		}
	})
}

// The time of a folder above the copy's is given once the next folder is made in it, before that folder is synced: a
// device that fails then leaves the operation planned, nothing moved, and the next run undoes it and moves the copy.
func TestTheTimeOfAFolderAboveTheCopys(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.failSetMtime("2", 1, syscall.EIO)
	items, summary := b.prune()
	if len(items) != 2 || items[1].Outcome != OutcomeIOError || !summary.DiskLost {
		t.Fatalf("%+v, %+v", items, summary)
	}
	if got := b.text("SELECT state FROM ops WHERE id = 2"); got != "planned" {
		t.Errorf("the operation is %q", got)
	}
	if !b.there("t3", "d/x") {
		t.Fatal("the copy moved")
	}
	delete(b.fail, b.dirs["t3"])
	b.prune()
	if got := b.text("SELECT group_concat(state, ' ') FROM (SELECT state FROM ops WHERE kind = 'trash' AND CAST(path AS TEXT) = 'd/x' ORDER BY id)"); got != "undone done" {
		t.Errorf("the operations are %q", got)
	}
	if b.there("t3", "d/x") {
		t.Error("the copy stays")
	}
}

// What the link guard sends to the trash: the folders that stand for one still there take its time, as for a copy
// (specs.md §17, §17.1).
func TestAFolderOfTheTrashFromALinkTakesTheTime(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/e/x")
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d", "e", "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(t.Context(), "DELETE FROM copies WHERE disk_id = 't3' AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'd/e/x')"); err != nil {
		t.Fatal(err)
	}
	b.dated(folderTimes)
	items, lost := b.guarded("t3", true)
	if lost || len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	entry := filepath.Join(b.dirs["t3"], "Atoll", "trash", filepath.FromSlash(b.text("SELECT CAST(trash_path AS TEXT) FROM trash")))
	top := filepath.Dir(filepath.Dir(filepath.Dir(entry)))
	for folder, want := range folderTimes {
		if got := timeOf(t, filepath.Join(top, filepath.FromSlash(folder))); !got.Equal(want) {
			t.Errorf("in the trash, %q has %v, want %v", folder, got, want)
		}
	}
}

// The folders of the chunks a repair saves: the operation's folder takes the time of the root; the one named like the
// file stands for no folder, and keeps its own (specs.md §16.1, §17).
func TestTheFolderOfTheChunksTakesTheTime(t *testing.T) {
	r := rotted(t)
	r.b.dated(map[string]time.Time{"": folderTimes[""]})
	if got := r.repaired(true); got != OutcomeRepaired {
		t.Fatalf("%s", got)
	}
	var trashPath string
	if err := r.b.db.QueryRowContext(t.Context(), "SELECT CAST(trash_path AS TEXT) FROM trash").Scan(&trashPath); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(r.b.dirs["t3"], "Atoll", "trash", filepath.FromSlash(trashPath))
	if got := timeOf(t, filepath.Dir(named)); !got.Equal(folderTimes[""]) {
		t.Errorf("the operation's folder has %v, want %v", got, folderTimes[""])
	}
	if got := timeOf(t, named); got.Equal(folderTimes[""]) {
		t.Errorf("the folder named like the file took the root's time")
	}
	if got := timeOf(t, filepath.Join(r.b.dirs["t3"], "media")); !got.Equal(folderTimes[""]) {
		t.Errorf("the root has %v", got)
	}
}
