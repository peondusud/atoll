//go:build flaws

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// A user's edit and rot of the copy newCopiedPool writes, "a film, long enough to be read whole", each of its size.
const (
	userEdit  = "a FILM, long enough to be read whole"
	rottedOne = "a fil#, long enough to be read whole"
)

// inPlace writes text over a copy of Films/a.mkv on a disk, the same size, and gives the copy back its modification
// time, as touch -r does: only its change time moves.
func (b *poolBench) inPlace(disk int, text string) {
	b.t.Helper()
	file := filepath.Join(b.mounts[disk-1], "media", "Films", "a.mkv")
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

// sameTick gives the row of Films/a.mkv on a disk the change time its copy has now, as a filesystem that holds the
// change time in whole seconds shows a change made in the second of the one the row recorded: no look can tell it.
func (b *poolBench) sameTick(disk int) {
	b.t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(b.mounts[disk-1], "media", "Films", "a.mkv"), &st); err != nil {
		b.t.Fatal(err)
	}
	db, err := catalog.OpenPool(context.Background(), filepath.Join(b.dir, "catalog.db"), "p")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), `UPDATE copies SET ctime_ns = ? WHERE disk_id = ?
		AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'Films/a.mkv')`, st.Ctim.Sec*1e9+st.Ctim.Nsec, b.diskID(disk)); err != nil {
		b.t.Fatal(err)
	}
}

func (b *poolBench) diskID(disk int) string { return "t" + string(rune('0'+disk)) }

// heldBy says whether a disk holds text, at Films/a.mkv or anywhere in its trash.
func (b *poolBench) heldBy(disk int, text string) bool {
	b.t.Helper()
	if b.read(disk, "Films/a.mkv") == text {
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
// the scrub reads another content with nothing changed, calls the copy corrupt, and repairs the edit away. No program
// can set a change time, so the test gives the row the change time the edit left, as such a filesystem shows it. It
// passes once the flaw is fixed, whatever the remedy: the edit stays at its path.
func TestFlawAnEditInTheSecondOfTheLastChangeIsNotRepairedAway(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.machine.mu.Lock()
	b.machine.precision = map[string]int64{b.mounts[1]: int64(time.Second)}
	b.machine.mu.Unlock()
	b.inPlace(2, userEdit)
	b.sameTick(2)
	code, stdout, stderr := b.atoll("scrub", "--yes", "--json")
	t.Logf("atoll scrub --yes: exit %d\n%s%s", code, stdout, stderr)
	if got := b.read(2, "Films/a.mkv"); got != userEdit {
		t.Errorf("the user's edit on t2 was repaired away: t2 holds %q", got)
	}
}

// Flaw 32 of the TLA+ models (conformance/models/README.md, Scrub.tla): a repair by chunk looks at the copy right
// before its first write, then reads each bad chunk from the healthy copy, then writes it. A user's write in place that
// lands after that look, while the healthy chunk is read, is written over, and was never saved to the trash. The test
// rots t2's copy, the row left as it was, and writes the user's edit into it when the repair reads from t1. It passes once
// the flaw is fixed, whatever the remedy: the edit is at its path or in the trash.
func TestFlawAnEditWhileARepairWritesIsNotLost(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.inPlace(2, rottedOne)
	b.sameTick(2)
	var once sync.Once
	armed := false
	b.machine.mu.Lock()
	b.machine.fail = func(mount, call, name string) error {
		switch {
		case mount == b.mounts[1] && call == "openwrite" && strings.HasSuffix(name, "a.mkv"):
			armed = true
		case armed && mount == b.mounts[0] && call == "read" && strings.HasSuffix(name, "a.mkv"):
			once.Do(func() { b.inPlace(2, userEdit) })
		}
		return nil
	}
	b.machine.mu.Unlock()
	code, stdout, stderr := b.atoll("scrub", "--yes", "--json")
	t.Logf("atoll scrub --yes: exit %d\n%s%s", code, stdout, stderr)
	b.machine.mu.Lock()
	b.machine.fail = nil
	b.machine.mu.Unlock()
	if !armed {
		t.Fatal("no repair opened t2's copy to write")
	}
	if !b.heldBy(2, userEdit) {
		t.Errorf("the user's edit on t2, written while the repair wrote, is neither at its path nor in the trash: t2 holds %q", b.read(2, "Films/a.mkv"))
	}
}
