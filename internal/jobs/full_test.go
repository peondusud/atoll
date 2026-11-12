package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// A disk with no room for a move to its trash takes no more in that pass: each would be journaled and undone for
// nothing, at every pass until room comes back, as a soak of the daemon found with a full disk (specs.md §17.1). Once
// room comes back, the next pass sends them all.
func TestAFullDiskTakesNoMoreForItsTrash(t *testing.T) {
	b := newBench(t, "t1", "t2")
	files := []string{"a", "b", "c"}
	for _, name := range files {
		b.write("t1", name, name)
		b.write("t2", name, name)
	}
	b.scan(true)
	for _, name := range files {
		if err := os.Remove(filepath.Join(b.dirs["t1"], "media", name)); err != nil {
			t.Fatal(err)
		}
	}
	b.fail[b.dirs["t2"]] = func(call, _ string) error {
		if call == "mkdir" {
			return syscall.ENOSPC
		}
		return nil
	}
	out := b.settle()
	var tried int
	for _, op := range out.Operations {
		if op.Kind == KindTrash {
			tried++
			if op.Outcome != trash.OutcomeIOError {
				t.Errorf("%+v", op)
			}
		}
	}
	if tried != 1 || b.count("SELECT count(*) FROM ops WHERE kind = 'trash'") != 1 {
		t.Fatalf("%d tried, %d journaled: %+v", tried, b.count("SELECT count(*) FROM ops WHERE kind = 'trash'"), out.Operations)
	}
	if b.count("SELECT count(*) FROM jobs WHERE kind = 'trash' AND state = 'queued'") != 3 {
		t.Errorf("the jobs did not wait")
	}
	delete(b.fail, b.dirs["t2"])
	out = b.pass()
	for _, name := range files {
		if got := outcomeOf(out, KindTrash, name); got != trash.OutcomeTrashed {
			t.Errorf("%s: %q (%+v)", name, got, out.Operations)
		}
	}
}
