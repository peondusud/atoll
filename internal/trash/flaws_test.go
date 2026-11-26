//go:build flaws

package trash

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// stoppedAt runs a repair of chunk 0 that stops at a point, as kill -9 would: what it committed stays, nothing after it
// runs.
func (r *rot) stoppedAt(point string, unreadable []int64) {
	r.b.t.Helper()
	stop := errors.New("stopped")
	r.b.at[point] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
			r.b.t.Fatalf("it did not stop at %s: %v", point, err)
		}
	}()
	s := r.b.opened()
	defer s.Close()
	_, _ = s.RepairChunks(r.b.t.Context(), r.file, r.copy, []int64{0}, unreadable, r.healthy, r.chunks)
}

// Flaw 33 of the TLA+ models (conformance/models/README.md, Scrub.tla): the reprise of a repair stopped by a crash takes
// a bad chunk that holds neither the file's content nor what was saved of it for the repair's own write, torn by the
// crash, sets the modification time back and gives the row the change time the copy has: a user's write into that chunk
// before the reprise is taken so, and the next repair writes over it, saved to the trash first. The test stops a repair
// right before its write, writes the user's bytes into the bad chunk, runs the reprise, then the repair the next scrub
// runs. It passes once the flaw is fixed, whatever the remedy: the user's write stays at its path.
func TestFlawAUsersWriteIsNotTakenForATornChunk(t *testing.T) {
	r := rotted(t)
	r.stoppedAt("chunk-looked", nil)
	if r.repairState() != "planned" {
		t.Fatalf("the operation is %s", r.repairState())
	}
	if err := os.WriteFile(filepath.Join(r.b.dirs["t3"], "media", "a"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.b.opened().Close()
	f, err := catalog.FileAt(t.Context(), r.b.db, "a")
	if err != nil || f == nil {
		t.Fatalf("%v", err)
	}
	s := r.b.opened()
	outcome, err := s.RepairChunks(t.Context(), f, copyOn(f, "t3"), []int64{0}, nil, r.healthy, r.chunks)
	s.Close()
	t.Logf("the reprise left the operation %s; the next repair: %s, %v", r.repairState(), outcome, err)
	if got := r.b.read("t3", "a"); got != "u" {
		t.Errorf("the user's write was taken for a torn chunk and repaired away: t3 holds %q", got)
	}
}

// Flaw 34 of the TLA+ models (conformance/models/README.md, Scrub.tla): a repair stopped by a crash once journaled, before
// it saved an empty file for a bad chunk it could not read, leaves its operation planned: the reprise finds that chunk
// still unreadable with nothing saved of it, and leaves the operation for a reprise that can read it, which never comes
// while the sector cannot be read; the scrub takes no copy with an operation planned at its name, and the corrupt copy
// stays in the root for good. It passes once the flaw is fixed, whatever the remedy: the operation is resolved.
func TestFlawAnUnreadableChunkLeavesNoRepairPlannedForGood(t *testing.T) {
	r := rotted(t)
	r.b.failOn("t3", "readback", syscall.EIO)
	r.stoppedAt("journaled", []int64{0})
	if r.repairState() != "planned" {
		t.Fatalf("the operation is %s", r.repairState())
	}
	for range 2 {
		r.b.opened().Close()
	}
	if got := r.repairState(); got == "planned" {
		t.Errorf("the repair stays planned while the chunk cannot be read, the corrupt copy taken again by no scrub")
	}
}
