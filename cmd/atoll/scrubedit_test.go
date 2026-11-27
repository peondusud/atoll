package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// A user's edit and rot of the copies newCopiedPool writes, each of its size: a.mkv holds "a film, long enough to be
// read whole", b.mkv "another film".
const (
	theFilm   = "a film, long enough to be read whole"
	userEdit  = "a FILM, long enough to be read whole"
	rottedOne = "a fil#, long enough to be read whole"
)

// inPlace writes text over a copy of a file on a disk, the same size, and gives the copy back its modification time,
// as touch -r does: only its change time moves.
func (b *poolBench) inPlace(disk int, name, text string) {
	b.t.Helper()
	file := filepath.Join(b.mounts[disk-1], "media", filepath.FromSlash(name))
	st, err := os.Stat(file)
	if err != nil {
		b.t.Fatal(err)
	}
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(text), 0); err != nil {
		b.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chtimes(file, st.ModTime(), st.ModTime()); err != nil {
		b.t.Fatal(err)
	}
}

// sameTick gives the row of a file on a disk the change time its copy has now, as a filesystem that holds the change
// time in whole seconds shows a change made in the second of the one the row recorded, and as rot leaves it: no look
// can tell it. No program can set a change time.
func (b *poolBench) sameTick(disk int, name string) {
	b.t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(b.mounts[disk-1], "media", filepath.FromSlash(name)), &st); err != nil {
		b.t.Fatal(err)
	}
	db, err := catalog.OpenPool(context.Background(), filepath.Join(b.dir, "catalog.db"), "p")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), `UPDATE copies SET ctime_ns = ? WHERE disk_id = ?
		AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = ?)`, st.Ctim.Sec*1e9+st.Ctim.Nsec, b.diskID(disk), name); err != nil {
		b.t.Fatal(err)
	}
}

func (b *poolBench) diskID(disk int) string { return "t" + string(rune('0'+disk)) }

// coarse makes a disk one that holds a time to the second, as an ext4 made with 128-byte inodes is (specs.md §10).
func (b *poolBench) coarse(disk int) {
	b.machine.mu.Lock()
	defer b.machine.mu.Unlock()
	if b.machine.precision == nil {
		b.machine.precision = map[string]int64{}
	}
	b.machine.precision[b.mounts[disk-1]] = int64(time.Second)
}

// heldBy says whether a disk holds text, at a file's path or anywhere in its trash.
func (b *poolBench) heldBy(disk int, name, text string) bool {
	b.t.Helper()
	if b.read(disk, name) == text {
		return true
	}
	found := false
	_ = filepath.Walk(filepath.Join(b.mounts[disk-1], "Atoll", "trash"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), text) {
				found = true
			}
		}
		return nil
	})
	return found
}

// Flaw 31 of the TLA+ models (conformance/models/README.md, Scrub.tla): on a filesystem whose change time holds whole
// seconds, as an ext4 with 128-byte inodes (time_precision_ns 1e9, specs.md §10), an edit in place that keeps the size
// and the modification time, in the second of the copy's last change, leaves every time the row recorded as it was:
// the scrub read another content with nothing changed, called the copy corrupt, and repaired the edit away. A mismatch
// on such a disk is a conflict now, every version kept (specs.md §16).
func TestAnEditInTheSecondOfTheLastChangeIsNotRepairedAway(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.coarse(2)
	b.inPlace(2, "Films/a.mkv", userEdit)
	b.sameTick(2, "Films/a.mkv")
	out := b.json(exitPartial, "scrub", "--yes")
	if got := b.read(2, "Films/a.mkv"); got != userEdit {
		t.Errorf("the user's edit on t2 was repaired away: t2 holds %q", got)
	}
	if got := paths(t, out, "t2", "conflict"); !slices.Equal(got, []string{"Films/a.mkv"}) || len(paths(t, out, "t2", "corrupt")) != 0 {
		t.Errorf("conflict %q, corrupt %q", got, paths(t, out, "t2", "corrupt"))
	}
	if got := b.read(1, "Films/a.mkv"); got != theFilm {
		t.Errorf("t1 holds %q", got)
	}
}

// One scrub over a disk that holds a time to the second and one that holds it to the nanosecond, each with a copy
// whose content changed and whose times did not (specs.md §16): on the first, an edit cannot be told from rot, and its
// file is a conflict, nothing written; on the second, the same is rot, corrupt and repaired from the first, in the
// same run.
func TestAScrubOverACoarseDiskAndAFineOne(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.coarse(2)
	b.inPlace(2, "Films/a.mkv", userEdit)
	b.sameTick(2, "Films/a.mkv")
	b.inPlace(1, "Films/b.mkv", "another fil#")
	b.sameTick(1, "Films/b.mkv")
	out := b.json(exitPartial, "scrub", "--yes")
	if got := paths(t, out, "t2", "conflict"); !slices.Equal(got, []string{"Films/a.mkv"}) || len(paths(t, out, "t2", "repaired")) != 0 {
		t.Errorf("t2: conflict %q, repaired %q", got, paths(t, out, "t2", "repaired"))
	}
	if got := paths(t, out, "t1", "repaired"); !slices.Equal(got, []string{"Films/b.mkv"}) || !slices.Equal(paths(t, out, "t1", "corrupt"), got) {
		t.Errorf("t1: corrupt %q, repaired %q", paths(t, out, "t1", "corrupt"), got)
	}
	if b.read(2, "Films/a.mkv") != userEdit || b.read(1, "Films/a.mkv") != theFilm {
		t.Errorf("a.mkv: t1 %q, t2 %q", b.read(1, "Films/a.mkv"), b.read(2, "Films/a.mkv"))
	}
	if b.read(1, "Films/b.mkv") != "another film" || b.read(2, "Films/b.mkv") != "another film" {
		t.Errorf("b.mkv: t1 %q, t2 %q", b.read(1, "Films/b.mkv"), b.read(2, "Films/b.mkv"))
	}
	// The conflict is the user's to settle, every version kept: the next scrub reads neither copy of it again.
	again := b.json(exitOK, "scrub", "--yes")
	if len(paths(t, again, "t2", "conflict")) != 0 || len(paths(t, again, "t2", "corrupt")) != 0 || b.read(2, "Films/a.mkv") != userEdit {
		t.Errorf("a second scrub: %v", again)
	}
}

// A chunk that cannot be read, on a disk that confirms and holds a time to the second, is a mismatch like another
// there: its file is a conflict, nothing written, as nothing tells what else of the copy is an edit (specs.md §16).
func TestAnUnreadableChunkOnACoarseDiskIsAConflict(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.coarse(2)
	b.unreadable()
	out := b.json(exitPartial, "scrub", "--yes")
	b.readable()
	if got := paths(t, out, "t2", "conflict"); !slices.Equal(got, []string{"Films/a.mkv"}) || len(paths(t, out, "t2", "repaired")) != 0 {
		t.Errorf("conflict %q, repaired %q", got, paths(t, out, "t2", "repaired"))
	}
	if got := b.diskStates(); !strings.Contains(got, "t2 online") {
		t.Errorf("the disks: %s", got)
	}
}

// Flaw 32 of the TLA+ models (conformance/models/README.md, Scrub.tla): a repair by chunk looked at the copy right
// before its first write, then read each bad chunk from the healthy copy, then wrote it. A user's write in place that
// landed after that look, while the healthy chunk was read, was written over, and never saved to the trash. The test
// rots t2's copy, the row left as it was, and writes the user's edit into it when the repair reads from t1. The repair
// looks at the copy again and reads the chunk again right before it writes it now, and stops changed (specs.md §16.1).
func TestAnEditWhileARepairWritesIsNotLost(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.inPlace(2, "Films/a.mkv", rottedOne)
	b.sameTick(2, "Films/a.mkv")
	var once sync.Once
	armed := false
	b.machine.mu.Lock()
	b.machine.fail = func(mount, call, name string) error {
		switch {
		case mount == b.mounts[1] && call == "openwrite" && strings.HasSuffix(name, "a.mkv"):
			armed = true
		case armed && mount == b.mounts[0] && call == "read" && strings.HasSuffix(name, "a.mkv"):
			once.Do(func() { b.inPlace(2, "Films/a.mkv", userEdit) })
		}
		return nil
	}
	b.machine.mu.Unlock()
	out := b.json(exitPartial, "scrub", "--yes")
	b.machine.mu.Lock()
	b.machine.fail = nil
	b.machine.mu.Unlock()
	if !armed {
		t.Fatal("no repair opened t2's copy to write")
	}
	if got := b.read(2, "Films/a.mkv"); got != userEdit {
		t.Errorf("the user's edit on t2, written while the repair wrote, is not at its path: t2 holds %q, in its trash %v", got,
			b.heldBy(2, "Films/a.mkv", userEdit))
	}
	if got := paths(t, out, "t2", "kept"); !slices.Equal(got, []string{"Films/a.mkv"}) || len(paths(t, out, "t2", "repaired")) != 0 {
		t.Errorf("kept %q, repaired %q", got, paths(t, out, "t2", "repaired"))
	}
	// The write stays a change: once it is settle old, the next scrub takes the copy again, reads it as it is, and makes
	// its file a conflict, every version kept (specs.md §16.1).
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(b.mounts[1], "media", "Films", "a.mkv"), &st); err != nil {
		t.Fatal(err)
	}
	b.machine.mu.Lock()
	b.machine.now = max(b.machine.now, st.Ctim.Nano()+int64(time.Hour))
	b.machine.mu.Unlock()
	again := b.json(exitPartial, "scrub", "--yes")
	if got := paths(t, again, "t2", "conflict"); !slices.Equal(got, []string{"Films/a.mkv"}) || b.read(2, "Films/a.mkv") != userEdit {
		t.Errorf("the next scrub: conflict %q, t2 holds %q", got, b.read(2, "Films/a.mkv"))
	}
}
