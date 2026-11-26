package trash

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/watch"
)

// bigRot is a copy of two chunks on t3, the second the last and shorter, rotted in the chunks bad says, its row what
// its disk holds, as rot leaves a copy's times. The repair reads from the copy on t1.
type bigRot struct {
	b       *bench
	file    *catalog.FileRow
	copy    *catalog.CopyRow
	healthy *Healthy
	chunks  []byte
	good    []byte
	rotten  []byte
	path    string
	bad     []int64
	// row is the copy's times as its row holds them before the repair.
	row string
}

func rottedBig(t *testing.T, bad ...int64) *bigRot {
	t.Helper()
	b := newBench(t)
	good := append(bytes.Repeat([]byte("a"), integrity.ChunkSize), "xyz"...)
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "big", string(good))
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.dirs["t3"], "media", "big")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rotten := bytes.Clone(good)
	for _, i := range bad {
		rotten[i*integrity.ChunkSize] = 'b'
	}
	if err := os.WriteFile(path, rotten, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	f, err := catalog.FileAt(t.Context(), b.db, "big")
	if err != nil || f == nil {
		t.Fatalf("%v", err)
	}
	c := copyOn(f, "t3")
	c.CtimeNS = sql.NullInt64{Int64: ctimeOf(t, path), Valid: true}
	b.exec("UPDATE copies SET ctime_ns = ? WHERE disk_id = 't3'", c.CtimeNS.Int64)
	h := copyOn(f, "t1")
	first, last := integrity.ChunkSum(good[:integrity.ChunkSize]), integrity.ChunkSum(good[integrity.ChunkSize:])
	r := &bigRot{b: b, file: f, copy: c, chunks: append(first[:], last[:]...), good: good, rotten: rotten, path: path, bad: bad,
		healthy: &Healthy{Disk: "t1", Known: catalog.KnownCopy{Path: "big", Size: h.Size, MtimeNS: h.MtimeNS, CtimeNS: h.CtimeNS, FileKey: h.FileKey}}}
	r.row = r.rowTimes()
	return r
}

func (r *bigRot) rowTimes() string {
	r.b.t.Helper()
	return r.b.text("SELECT mtime_ns || ' ' || ctime_ns || ' ' || state FROM copies WHERE disk_id = 't3'")
}

// repair runs the repair in a session of its own, and gives its outcome.
func (r *bigRot) repair() string {
	r.b.t.Helper()
	s := r.b.opened()
	defer s.Close()
	outcome, err := s.RepairChunks(r.b.t.Context(), r.file, r.copy, r.bad, nil, r.healthy, r.chunks)
	if err != nil {
		r.b.t.Fatal(err)
	}
	return outcome
}

// edit writes text at an offset of t3's copy, in place, as a program that edits a file does.
func (r *bigRot) edit(at int64, text string) {
	r.b.t.Helper()
	f, err := os.OpenFile(r.path, os.O_WRONLY, 0)
	if err != nil {
		r.b.t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(text), at); err != nil {
		r.b.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		r.b.t.Fatal(err)
	}
}

func (r *bigRot) content() []byte {
	r.b.t.Helper()
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.b.t.Fatal(err)
	}
	return data
}

// with gives b with text written at an offset.
func with(b []byte, at int64, text string) []byte {
	out := bytes.Clone(b)
	copy(out[at:], text)
	return out
}

func (r *bigRot) repairState() string {
	r.b.t.Helper()
	return r.b.text("SELECT coalesce(group_concat(state, ','), '') FROM (SELECT state FROM ops WHERE kind = 'repair' ORDER BY id)")
}

// whileHealthyRead runs fire at the first read of the healthy copy on t1, while the repair reads the chunk it is to
// write: the window between its look right before its first write and that write was there (flaw 32 of the TLA+
// models).
func (r *bigRot) whileHealthyRead(fire func()) {
	done := false
	r.b.fail[r.b.dirs["t1"]] = func(call, _ string) error {
		if call == "read" && !done {
			done = true
			fire()
		}
		return nil
	}
}

// armable sets a fault on t3, before the run opens its handles as the bench's faults must be: once the function it
// gives is called, each call of that kind fails with EIO.
func (r *bigRot) armable(kind string) func() {
	var armed atomic.Bool
	r.b.fail[r.b.dirs["t3"]] = func(call, _ string) error {
		if call == kind && armed.Load() {
			return syscall.EIO
		}
		return nil
	}
	return func() { armed.Store(true) }
}

// A user's write in place that lands while a repair reads the healthy chunk, after its look right before the write,
// was written over, never saved to the trash (flaw 32). The repair looks at the copy again right before each write now:
// its times moved, someone else wrote, and the repair stops changed, nothing more written, its time not set back, its
// row as it was, so that the write stays a change; what it saved stays in the trash (specs.md §16.1).
func TestARepairLeavesAWriteBeforeItsFirstWrite(t *testing.T) {
	r := rottedBig(t, 0)
	r.whileHealthyRead(func() { r.edit(0, "U") })
	if got := r.repair(); got != OutcomeChanged {
		t.Fatalf("%s", got)
	}
	if !bytes.Equal(r.content(), with(r.rotten, 0, "U")) {
		t.Error("the user's write is not at its path")
	}
	if r.repairState() != "done" || r.rowTimes() != r.row {
		t.Errorf("the operation is %s, the row %s, was %s", r.repairState(), r.rowTimes(), r.row)
	}
	saved, err := os.ReadFile(filepath.Join(r.b.dirs["t3"], "Atoll", "trash", filepath.FromSlash(r.b.text("SELECT CAST(trash_path AS TEXT) FROM trash")), "00000000"))
	if err != nil || !bytes.Equal(saved, r.rotten[:integrity.ChunkSize]) {
		t.Errorf("the chunk saved: %v", err)
	}
	if n := r.b.count("SELECT count(*) FROM events WHERE kind = 'repair-stopped' AND json_extract(data, '$.reason') = 'changed'"); n != 1 {
		t.Errorf("%d events repair-stopped, changed", n)
	}
}

// The look right before each write, and the one before the time is set back and the one before the commit: a write
// landing between two writes of the repair, after its last write, or after its time is set back, is a change it
// leaves, each, nothing it wrote undone and nothing more written; its time not set back by the repair when it had not
// set it yet, and its row as it was, so that the next look of the copy finds the change (specs.md §16.1).
func TestARepairLeavesAWriteAtEachLook(t *testing.T) {
	end := int64(integrity.ChunkSize)
	for _, tc := range []struct {
		name  string
		bad   []int64
		point string
		// wrote is what the copy holds after: the chunks the repair wrote, then the user's write.
		wrote func(r *bigRot) []byte
		set   bool
	}{
		{"between two writes", []int64{0, 1}, "chunk-written", func(r *bigRot) []byte { return with(with(r.rotten, 0, "a"), end, "U") }, false},
		{"after the last write", []int64{0}, "chunk-written", func(r *bigRot) []byte { return with(r.good, end, "U") }, false},
		{"after the time is set back", []int64{0}, "times-set", func(r *bigRot) []byte { return with(r.good, end, "U") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rottedBig(t, tc.bad...)
			r.b.at[tc.point] = func() { r.edit(end, "U") }
			if got := r.repair(); got != OutcomeChanged {
				t.Fatalf("%s", got)
			}
			if !bytes.Equal(r.content(), tc.wrote(r)) {
				t.Error("the copy does not hold the chunks written, then the user's write")
			}
			if r.repairState() != "done" || r.rowTimes() != r.row {
				t.Errorf("the operation is %s, the row %s, was %s", r.repairState(), r.rowTimes(), r.row)
			}
			info, err := os.Stat(r.path)
			if err != nil {
				t.Fatal(err)
			}
			if back := info.ModTime().UnixNano() == r.copy.MtimeNS.Int64; back {
				t.Error("the user's write has the row's time: hidden from the next look")
			}
		})
	}
}

// A chunk the repair could not read, that reads right before it is written: someone wrote there, as a sector that
// cannot be read reads again only once written over. The repair leaves it (specs.md §16.1).
func TestARepairLeavesASectorThatReadsAgain(t *testing.T) {
	r := rotted(t)
	s := r.b.opened()
	outcome, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, []int64{0}, r.healthy, r.chunks)
	s.Close()
	if err != nil || outcome != OutcomeChanged || r.b.read("t3", "a") != "y" {
		t.Errorf("%s, %v, the copy holds %q", outcome, err, r.b.read("t3", "a"))
	}
}

// A look or a read again that fails right before a write, or a look that fails before the time is set back or before
// the commit: an I/O error the disk's, which loses it, the operation planned for the reprise (invariant 4); a chunk that
// cannot be read again on a disk that confirms, the chunk's, the disk kept. The next run's reprise ends it.
func TestARepairWhoseLookFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bad   []int64
		arm   func(r *bigRot)
		lost  bool
		after []byte // what the copy holds once the reprise ran: nil for the rot, as before
	}{
		{"the look before a write", []int64{0}, func(r *bigRot) { r.whileHealthyRead(r.armable("stat")) }, true, nil},
		{"the read again before a write", []int64{0}, func(r *bigRot) { r.whileHealthyRead(r.armable("readback")) }, false, nil},
		{"the look before the time is set back", []int64{0}, func(r *bigRot) { r.b.at["chunk-written"] = r.armable("stat") }, true, []byte{}},
		{"the look before the commit", []int64{0}, func(r *bigRot) { r.b.at["times-set"] = r.armable("stat") }, true, []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rottedBig(t, tc.bad...)
			tc.arm(r)
			if got := r.repair(); got != OutcomeIOError {
				t.Errorf("%s", got)
			}
			if lost := r.b.diskState("t3") == "missing"; lost != tc.lost || r.repairState() != "planned" {
				t.Errorf("t3 is %s, the operation %s", r.b.diskState("t3"), r.repairState())
			}
			delete(r.b.fail, r.b.dirs["t3"])
			delete(r.b.fail, r.b.dirs["t1"])
			r.b.opened().Close()
			want := r.rotten
			if tc.after != nil {
				want = r.good
			}
			if r.repairState() == "planned" || !bytes.Equal(r.content(), want) {
				t.Errorf("after the reprise, the operation is %s, the copy the rot %v", r.repairState(), bytes.Equal(r.content(), r.rotten))
			}
		})
	}
}

// A repair stopped as by kill -9 at each look it takes now, right before a write, before its time is set back and
// before its commit: the reprise of the next run ends it from what the copy holds, nothing written before the first,
// every chunk written at the others (specs.md §16.1, §17.2).
func TestARepairStoppedAtEachLook(t *testing.T) {
	for _, tc := range []struct {
		point    string
		repaired bool
	}{
		{"chunk-looked", false},
		{"times-looked", true},
		{"commit-looked", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			r := rottedBig(t, 0)
			stop := errors.New("stopped")
			r.b.at[tc.point] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop: %v", err)
					}
				}()
				r.repair()
			}()
			if r.repairState() != "planned" {
				t.Fatalf("the operation is %s", r.repairState())
			}
			r.b.opened().Close()
			want, state := r.rotten, "done"
			if tc.repaired {
				want = r.good
			}
			if !bytes.Equal(r.content(), want) || r.repairState() != state {
				t.Errorf("after the reprise, the operation is %s, the copy repaired %v", r.repairState(), bytes.Equal(r.content(), r.good))
			}
			info, err := os.Stat(r.path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.repaired && info.ModTime().UnixNano() != r.copy.MtimeNS.Int64 {
				t.Error("the time is not set back")
			}
		})
	}
}
