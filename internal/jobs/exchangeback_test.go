package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// swapPool makes two disks, each with a at "the file at a" and b at "the file at b, longer", scanned; the user swaps a
// and b on t1, through a third name; the next pass renames both copies of t2, each path-taken at the other's name, which
// only an exchange of a and b there gets round (specs.md §15.12, A cycle).
func swapPool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "the file at a")
		b.write(id, "b", "the file at b, longer")
	}
	b.scan(true)
	b.rename("t1", "a", "c")
	b.rename("t1", "b", "a")
	b.rename("t1", "c", "b")
	b.pass()
	b.now += 30_000_000_000
	return b
}

const savedAtB = "a file a program saved at b, on t2"

// saveAtB saves a file at b on t2, atomically, as a program does: a new name, then a rename onto b.
func (b *bench) saveAtB() {
	at := filepath.Join(b.dirs["t2"], "media", "b")
	if err := os.WriteFile(at+".new", []byte(savedAtB), 0o644); err != nil {
		b.t.Error(err)
	}
	if err := os.Rename(at+".new", at); err != nil {
		b.t.Error(err)
	}
}

func (b *bench) readMedia(id, path string) string {
	data, err := os.ReadFile(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		return "absent"
	}
	return string(data)
}

// A file a program saves at one of the two names between the look right before an exchange and the exchange arrives
// at the other name; the look after finds it there, and the two names are exchanged back at once, each synced, and both
// renames undone, with an op-undone event each (specs.md §15.12, §17.2; conformance/models/README.md, flaw 13, the
// maintainer's remedy, 2026-11-23): the file saved stays at the name it was saved at, the copy at its own. Before, the
// table finished one rename and undid the other, leaving the file saved at a name the exchange gave it (invariant 15).
func TestAFileSavedBeforeAnExchangeStaysAtItsName(t *testing.T) {
	b := swapPool(t)
	exchanges := 0
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "exchange" && name == "b" {
			if exchanges++; exchanges == 1 {
				// An atomic save at one of the two names, right before the exchange.
				b.saveAtB()
			}
		}
		return nil
	}
	b.pass()
	b.fail[b.dirs["t2"]] = nil
	if exchanges != 2 {
		t.Fatalf("%d exchanges on t2, want the exchange and the one back", exchanges)
	}
	if got := b.readMedia("t2", "b"); got != savedAtB {
		t.Errorf("the file saved at b on t2 is not there (b holds %q), and a holds %q", got, b.readMedia("t2", "a"))
	}
	if got := b.readMedia("t2", "a"); got != "the file at a" {
		t.Errorf("a on t2 holds %q, want its copy", got)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned'"); n != 0 {
		t.Errorf("%d renames left planned", n)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND disk_id = 't2' AND state = 'undone'"); n != 2 {
		t.Errorf("%d renames undone on t2, want both", n)
	}
	if n := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); n != 2 {
		t.Errorf("%d op-undone events, want one for each rename", n)
	}
	// The passes after it, the copies' change times moved by the two exchanges: every version is still on a disk or in a
	// trash, and nothing is left planned.
	for range 3 {
		b.now += 30_000_000_000
		b.settle()
	}
	held := map[string]bool{}
	for _, id := range []string{"t1", "t2"} {
		_ = filepath.WalkDir(b.dirs[id], func(path string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if data, err := os.ReadFile(path); err == nil {
					held[string(data)] = true
				}
			}
			return nil
		})
	}
	for _, text := range []string{"the file at a", "the file at b, longer", savedAtB} {
		if !held[text] {
			t.Errorf("%q is on no disk and in no trash after the passes that follow", text)
		}
	}
	if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
		t.Errorf("%d operations left planned after the passes that follow", n)
	}
}

// The exchange back stopped as kill -9 stops it, once the two names are exchanged back and before either is synced,
// then the next pass's reprise: each rename found undone by the table of §17.2, the file saved at b, the copy at a, as
// with nothing stopped; the exchange back failing with EIO at its sync, the table then says what became of each, the
// names as they are, both undone; and the exchange back itself failing, the names left as the exchange gave them, the
// table finishing one and undoing the other, nothing lost (specs.md §15.12, §17.2).
func TestAnExchangeBackStoppedOrFailing(t *testing.T) {
	t.Run("stopped at put-back", func(t *testing.T) {
		b := swapPool(t)
		saved := false
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "exchange" && name == "b" && !saved {
				saved = true
				b.saveAtB()
			}
			return nil
		}
		b.crashAt("put-back")
		b.fail[b.dirs["t2"]] = nil
		b.pass()
		if got := b.readMedia("t2", "b"); got != savedAtB {
			t.Errorf("b on t2 holds %q, a %q", got, b.readMedia("t2", "a"))
		}
		if got := b.readMedia("t2", "a"); got != "the file at a" {
			t.Errorf("a on t2 holds %q", got)
		}
		if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND disk_id = 't2' AND state = 'undone'"); n != 2 {
			t.Errorf("%d renames undone on t2 after the reprise, want both", n)
		}
		if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
			t.Errorf("%d operations left planned", n)
		}
	})
	t.Run("its sync failing", func(t *testing.T) {
		b := swapPool(t)
		exchanges, failed := 0, false
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			switch {
			case call == "exchange" && name == "b":
				if exchanges++; exchanges == 1 {
					b.saveAtB()
				}
			case call == "syncentry" && exchanges == 2 && !failed:
				failed = true
				return syscall.EIO
			}
			return nil
		}
		b.pass()
		b.fail[b.dirs["t2"]] = nil
		b.pass()
		if !failed {
			t.Fatal("no sync after the exchange back")
		}
		if got := b.readMedia("t2", "b"); got != savedAtB {
			t.Errorf("b on t2 holds %q, a %q", got, b.readMedia("t2", "a"))
		}
		if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
			t.Errorf("%d operations left planned", n)
		}
	})
	t.Run("the exchange back failing", func(t *testing.T) {
		b := swapPool(t)
		exchanges := 0
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "exchange" && name == "b" {
				exchanges++
				switch exchanges {
				case 1:
					b.saveAtB()
				case 2:
					return syscall.EIO
				}
			}
			return nil
		}
		b.pass()
		b.fail[b.dirs["t2"]] = nil
		b.pass()
		if exchanges != 2 {
			t.Fatalf("%d exchanges on t2", exchanges)
		}
		held := map[string]bool{}
		for _, path := range []string{"a", "b"} {
			held[b.readMedia("t2", path)] = true
		}
		if !held[savedAtB] || !held["the file at a"] {
			t.Errorf("t2 holds a %q, b %q: something is lost", b.readMedia("t2", "a"), b.readMedia("t2", "b"))
		}
		if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
			t.Errorf("%d operations left planned", n)
		}
	})
}
