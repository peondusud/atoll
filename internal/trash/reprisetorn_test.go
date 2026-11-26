package trash

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/integrity"
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

// rowTimes gives what t3's row of a records.
func (r *rot) rowTimes() string {
	r.b.t.Helper()
	return r.b.text("SELECT mtime_ns || ' ' || ctime_ns || ' ' || state FROM copies WHERE disk_id = 't3'")
}

// Flaw 33 of the TLA+ models (conformance/models/README.md, Scrub.tla): the reprise of a repair stopped by a crash took
// a bad chunk that holds neither the file's content nor what was saved of it for the repair's own write, torn by the
// crash, set the modification time back and gave the row the change time the copy had: a user's write into that chunk
// before the reprise was taken so, and the next repair wrote over it, saved to the trash first. Such a chunk is someone
// else's change now (torn-foreign): nothing is written, the row stays, and the next look settles the copy as a conflict
// (specs.md §16.1). On a disk that holds its times to the second as on one that holds them to the nanosecond.
func TestAUsersWriteIsNotTakenForATornChunk(t *testing.T) {
	for _, precision := range []int64{0, 1_000_000_000} {
		r := rotted(t)
		if precision > 0 {
			r.b.exec("UPDATE disks SET time_precision_ns = ? WHERE id = 't3'", precision)
		}
		r.stoppedAt("chunk-looked", nil)
		if r.repairState() != "planned" {
			t.Fatalf("%d ns: the operation is %s", precision, r.repairState())
		}
		before := r.rowTimes()
		if err := os.WriteFile(filepath.Join(r.b.dirs["t3"], "media", "a"), []byte("u"), 0o644); err != nil {
			t.Fatal(err)
		}
		r.b.opened().Close()
		if r.repairState() == "planned" || r.rowTimes() != before {
			t.Errorf("%d ns: the reprise left the operation %s, the row %s, was %s", precision, r.repairState(), r.rowTimes(), before)
		}
		f, err := catalog.FileAt(t.Context(), r.b.db, "a")
		if err != nil || f == nil {
			t.Fatalf("%v", err)
		}
		s := r.b.opened()
		outcome, err := s.RepairChunks(t.Context(), f, copyOn(f, "t3"), []int64{0}, nil, r.healthy, r.chunks)
		s.Close()
		if got := r.b.read("t3", "a"); got != "u" || err != nil || outcome != OutcomeChanged {
			t.Errorf("%d ns: the user's write was taken for a torn chunk and repaired away: t3 holds %q; the next repair %s, %v", precision, got, outcome, err)
		}
	}
}

// A true tear, part the healthy bytes and part the rot, holds neither too: the reprise takes it for someone else's
// change, nothing written, the row as it was, so that the next look finds the copy changed and settles it as a conflict,
// every version kept: a tear and a user's write are never told apart (specs.md §16.1, the maintainer, 2026-11-26).
func TestATrueTearEndsInAConflict(t *testing.T) {
	r := rottedBig(t, 0)
	stop := errors.New("stopped")
	r.b.at["chunk-looked"] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
	func() {
		defer func() {
			if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
				t.Fatalf("it did not stop: %v", err)
			}
		}()
		r.repair()
	}()
	// The write the crash tore: its first half the healthy bytes, its second the rot.
	r.edit(0, string(r.good[:integrity.ChunkSize/2]))
	torn := r.content()
	r.b.opened().Close()
	if r.repairState() == "planned" || r.rowTimes() != r.row || !bytes.Equal(r.content(), torn) {
		t.Errorf("the reprise left the operation %s, the row %s, was %s; written %v", r.repairState(), r.rowTimes(), r.row, !bytes.Equal(r.content(), torn))
	}
}

// Flaw 34 of the TLA+ models (conformance/models/README.md, Scrub.tla): a repair stopped by a crash once journaled,
// before it saved an empty file for a bad chunk it could not read, left its operation planned for as long as the chunk
// could not be read, and no scrub took the copy again. Once the disk confirms, the chunk's fault: as the repair found it,
// nothing saved of it yet, the repair is resolved, nothing written (unread-found, specs.md §16.1).
func TestAnUnreadableChunkLeavesNoRepairPlannedForGood(t *testing.T) {
	r := rotted(t)
	r.b.failOn("t3", "readback", syscall.EIO)
	r.stoppedAt("journaled", []int64{0})
	if r.repairState() != "planned" {
		t.Fatalf("the operation is %s", r.repairState())
	}
	r.b.opened().Close()
	if got := r.repairState(); got == "planned" || r.b.diskState("t3") != "online" {
		t.Errorf("the repair stays %s while the chunk cannot be read, t3 %s", got, r.b.diskState("t3"))
	}
}

// The reprise of a repair with a chunk that cannot be read, each way: readable again, as the repair found it, nothing
// saved; a bad chunk saved, unreadable since; and on a disk that no longer confirms, lost, the operation planned until a
// run finds it again, which resolves it (invariant 4, specs.md §16.1).
func TestAReprisePastAChunkThatCannotBeRead(t *testing.T) {
	// Unreadable when the repair found it, readable again at the reprise: what the repair found, nothing written.
	r := rotted(t)
	r.b.failOn("t3", "readback", syscall.EIO)
	r.stoppedAt("journaled", []int64{0})
	delete(r.b.fail, r.b.dirs["t3"])
	r.b.opened().Close()
	if r.repairState() == "planned" || r.b.read("t3", "a") != "y" {
		t.Errorf("readable again: the operation is %s, the copy %q", r.repairState(), r.b.read("t3", "a"))
	}
	// A bad chunk saved, then unreadable: neither its hash nor what was saved, someone else's change.
	r = rotted(t)
	arm := false
	r.b.fail[r.b.dirs["t3"]] = func(call, _ string) error {
		if call == "readback" && arm {
			return syscall.EIO
		}
		return nil
	}
	r.stoppedAt("chunk-looked", nil)
	arm = true
	r.b.opened().Close()
	if r.repairState() != "done" || r.b.diskState("t3") != "online" {
		t.Errorf("saved, then unreadable: the operation is %s, t3 %s", r.repairState(), r.b.diskState("t3"))
	}
	// The disk no longer confirms at the reprise: lost, the operation planned; once it confirms, resolved.
	r = rotted(t)
	r.b.failOn("t3", "readback", syscall.EIO)
	r.stoppedAt("journaled", []int64{0})
	mount := r.b.dirs["t3"]
	r.b.beforeRead[mount] = func() { r.b.beforeRead[mount] = func() { r.b.unread[mount] = true } }
	r.b.opened().Close()
	if r.repairState() != "planned" || r.b.diskState("t3") != "missing" {
		t.Errorf("a disk that does not confirm: the operation is %s, t3 %s", r.repairState(), r.b.diskState("t3"))
	}
	delete(r.b.unread, mount)
	r.b.opened().Close()
	if r.repairState() == "planned" {
		t.Errorf("once it confirms, the operation is %s", r.repairState())
	}
}

// The reprise of a repair whose chunk cannot be read, stopped as by kill -9, or failing, at each of its new steps: the
// disk's confirmation, then the repair's resolution once it confirmed, its look at the copy's folder and name, its open
// and its read. The operation stays planned, nothing written, the disk lost only by a failure, and the next run
// resolves it (specs.md §16.1, §17.2, invariant 4).
func TestAnUnreadableChunksRepriseStoppedAtEachStep(t *testing.T) {
	stop := errors.New("stopped")
	for _, tc := range []struct {
		name string
		call string // the call of the resolution that stops or fails, or "" for the confirmation itself
		err  error  // nil: stopped as by kill -9
	}{
		{"kill -9 at the confirmation", "", nil},
		{"kill -9 at the folder", "opendir", nil},
		{"kill -9 at the look", "stat", nil},
		{"kill -9 at the open", "openwrite", nil},
		{"kill -9 at the read", "readback", nil},
		{"EIO at the folder", "opendir", syscall.EIO},
		{"EIO at the look", "stat", syscall.EIO},
		{"EIO at the open", "openwrite", syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rotted(t)
			mount := r.b.dirs["t3"]
			armed := false
			r.b.fail[mount] = func(call, _ string) error {
				if armed && call == tc.call {
					armed = false
					if tc.err == nil {
						panic(stop) //nolint:forbidigo // what stands for kill -9
					}
					return tc.err
				}
				if call == "readback" {
					return syscall.EIO
				}
				return nil
			}
			r.stoppedAt("journaled", []int64{0})
			// The next run checks the disk, then confirms it once more after the read that fails: that step stops, or
			// arms the next.
			r.b.beforeRead[mount] = func() {
				r.b.beforeRead[mount] = func() {
					if tc.call == "" {
						panic(stop) //nolint:forbidigo // what stands for kill -9
					}
					armed = true
				}
			}
			func() {
				defer func() {
					p := recover()
					if got, _ := p.(error); (tc.err == nil) != errors.Is(got, stop) || p != nil && !errors.Is(got, stop) {
						t.Fatalf("it stopped with %v", p)
					}
				}()
				r.b.opened().Close()
			}()
			want := "online"
			if tc.err != nil {
				want = "missing"
			}
			if r.repairState() != "planned" || r.b.diskState("t3") != want || r.b.read("t3", "a") != "y" {
				t.Errorf("the operation is %s, t3 %s, the copy %q", r.repairState(), r.b.diskState("t3"), r.b.read("t3", "a"))
			}
			r.b.opened().Close()
			if r.repairState() == "planned" || r.b.diskState("t3") != "online" || r.b.read("t3", "a") != "y" {
				t.Errorf("the next run: the operation is %s, t3 %s, the copy %q", r.repairState(), r.b.diskState("t3"), r.b.read("t3", "a"))
			}
		})
	}
}

// A repair that stops as a chunk cannot be read back, stopped as by kill -9 at the disk's confirmation right after: the
// operation stays planned, and the next run's reprise resolves it, the chunk still unreadable (specs.md §16.1).
func TestARepairStoppedAtTheConfirmationOfAnUnreadableChunk(t *testing.T) {
	r := rotted(t)
	mount := r.b.dirs["t3"]
	failing := false
	reads := 0
	r.b.fail[mount] = func(call, _ string) error {
		if call == "readback" {
			if reads++; reads >= 3 {
				failing = true
				return syscall.EIO
			}
		}
		return nil
	}
	stop := errors.New("stopped")
	var confirm func()
	confirm = func() {
		if failing {
			panic(stop) //nolint:forbidigo // what stands for kill -9
		}
		r.b.beforeRead[mount] = confirm
	}
	r.b.beforeRead[mount] = confirm
	func() {
		defer func() {
			if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
				t.Fatalf("it did not stop: %v", err)
			}
		}()
		s := r.b.opened()
		defer s.Close()
		_, _ = s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks)
	}()
	delete(r.b.beforeRead, mount)
	if r.repairState() != "planned" || r.b.diskState("t3") != "online" {
		t.Fatalf("the operation is %s, t3 %s", r.repairState(), r.b.diskState("t3"))
	}
	r.b.opened().Close()
	if r.repairState() == "planned" || r.b.diskState("t3") != "online" {
		t.Errorf("the next run: the operation is %s, t3 %s", r.repairState(), r.b.diskState("t3"))
	}
}

// Both remedies at once: a repair of two bad chunks stopped by a crash before its second write, a user's write into that
// chunk, and every read of the copy failing since, on a disk that confirms: the reprise ends the operation, nothing
// more written, the time not set back, the user's write where it was.
func TestARepriseWithBothRemedies(t *testing.T) {
	r := rottedBig(t, 0, 1)
	arm := false
	r.b.fail[r.b.dirs["t3"]] = func(call, _ string) error {
		if call == "readback" && arm {
			return syscall.EIO
		}
		return nil
	}
	stop := errors.New("stopped")
	looks := 0
	r.b.at["chunk-written"] = func() {
		r.b.at["chunk-looked"] = func() {
			if looks++; looks == 1 {
				panic(stop) //nolint:forbidigo // what stands for kill -9
			}
		}
	}
	func() {
		defer func() {
			if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
				t.Fatalf("it did not stop: %v", err)
			}
		}()
		r.repair()
	}()
	r.edit(int64(integrity.ChunkSize), "U")
	want := r.content()
	arm = true
	r.b.opened().Close()
	arm = false
	if r.repairState() != "done" || !bytes.Equal(r.content(), want) || r.b.diskState("t3") != "online" {
		t.Errorf("the operation is %s, t3 %s, the user's write kept %v", r.repairState(), r.b.diskState("t3"), bytes.Equal(r.content(), want))
	}
}
