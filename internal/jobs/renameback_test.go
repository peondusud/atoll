package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A pass takes a rename to the other copy with rename(2), which moves whatever the old name holds then. A file a
// program saves at that name between the look right before the move and the move arrives at the new name in the copy's
// place: the look after finds it is not the copy, and puts it back at its old name at once, without replacing anything
// (specs.md §15.12, step 4, §17.2). It once stayed at the new name, at a name Atoll gave it (invariant 15;
// conformance/models/README.md, flaw 7).
func TestAFileSavedAtTheOldNameGoesBackAfterTheRename(t *testing.T) {
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
		t.Errorf("the file saved at a on t2 is not there (%s), and b holds %q", got, at("b"))
	}
	if got := at("b"); got == saved {
		t.Errorf("the file saved at a is at b too")
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned'"); n != 0 {
		t.Errorf("%d renames left planned", n)
	}
}

// Killed once the file is put back, before anything is synced: the reprise of the rename finds nothing at the target
// and another file where the copy sat, undone with an op-undone event (specs.md §17.2), the file left at its name.
func TestAPassKilledOnceAFileIsPutBack(t *testing.T) {
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
	stop := errors.New("stopped")
	b.at["put-back"] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
	func() {
		defer func() {
			if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
				t.Fatalf("it did not stop at put-back: %v", err)
			}
		}()
		b.pass()
		b.now += 30_000_000_000
		b.pass()
	}()
	delete(b.at, "put-back")
	b.fail[b.dirs["t2"]] = nil
	b.settle()
	data, err := os.ReadFile(old)
	if err != nil || string(data) != saved {
		t.Errorf("the file put back: %q, %v", data, err)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned'"); n != 0 {
		t.Errorf("%d renames left planned", n)
	}
}
