package trash

import (
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/layout"
)

// rot is a copy that rotted: its byte is another, and its row what its disk holds, as rot leaves a copy's times. The
// repair reads from the copy on t1.
type rot struct {
	b       *bench
	file    *catalog.FileRow
	copy    *catalog.CopyRow
	healthy *Healthy
	chunks  []byte
}

// rotted gives a bench whose copy of a on t3 rotted.
func rotted(t *testing.T) *rot {
	t.Helper()
	b := newBench(t)
	path := filepath.Join(b.dirs["t3"], "media", "a")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	f, err := catalog.FileAt(t.Context(), b.db, "a")
	if err != nil || f == nil {
		t.Fatalf("%v", err)
	}
	r := &rot{b: b, file: f, copy: copyOn(f, "t3")}
	r.copy.CtimeNS = sql.NullInt64{Int64: ctimeOf(t, path), Valid: true}
	h := copyOn(f, "t1")
	r.healthy = &Healthy{Disk: "t1", Known: catalog.KnownCopy{Path: "a", Size: h.Size, MtimeNS: h.MtimeNS, CtimeNS: h.CtimeNS, FileKey: h.FileKey}}
	sum := integrity.ChunkSum([]byte("x"))
	r.chunks = sum[:]
	return r
}

// diskState gives the state a disk's row records.
func (b *bench) diskState(id string) string {
	b.t.Helper()
	return b.text("SELECT state FROM disks WHERE id = '" + id + "'")
}

func ctimeOf(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ctim.Nano()
}

// repaired runs the repair of chunk 0 in a session of its own, and gives its outcome.
func (r *rot) repaired(apply bool) string {
	r.b.t.Helper()
	s, err := Open(r.b.t.Context(), r.b.db, r.b.config, r.b, apply)
	if err != nil {
		r.b.t.Fatal(err)
	}
	defer s.Close()
	outcome, err := s.RepairChunks(r.b.t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks)
	if err != nil {
		r.b.t.Fatal(err)
	}
	return outcome
}

func (r *rot) repairState() string {
	r.b.t.Helper()
	return r.b.text("SELECT coalesce(group_concat(state, ','), '') FROM (SELECT state FROM ops WHERE kind = 'repair' ORDER BY id)")
}

// A copy that rotted is written in place from the healthy one, after its bad chunk reached the trash: its inode and
// its time are its own still (specs.md §16.1).
func TestARepairWritesTheChunkInPlace(t *testing.T) {
	r := rotted(t)
	path := filepath.Join(r.b.dirs["t3"], "media", "a")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.repaired(true); got != OutcomeRepaired {
		t.Fatalf("%s", got)
	}
	after, err := os.Stat(path)
	if err != nil || r.b.read("t3", "a") != "x" || !after.ModTime().Equal(before.ModTime()) || inodeOf(t, path) != before.Sys().(*syscall.Stat_t).Ino { //nolint:forcetypeassert // Linux only
		t.Errorf("the copy holds %q, %v", r.b.read("t3", "a"), err)
	}
	if r.repairState() != "done" || r.b.text("SELECT reason FROM trash") != "repaired" {
		t.Errorf("the operation is %s", r.repairState())
	}
}

// What cannot be reached is not repaired, and nothing is journaled: a disk that may only be read, a healthy copy whose
// disk does not confirm, a copy no longer what its row says.
func TestARepairThatCannotStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(r *rot)
		want  string
	}{
		{"read-only", func(r *rot) { r.b.ro[r.b.dirs["t3"]] = true }, outcomeUnavailable},
		{"healthy out of reach", func(r *rot) { r.b.unread[r.b.dirs["t1"]] = true }, outcomeUnavailable},
		{"changed", func(r *rot) { r.b.write("t3", "a", "zz") }, OutcomeChanged},
		{"cannot be opened", func(r *rot) { r.b.failOn("t3", "openwrite", syscall.EACCES) }, OutcomeIOError},
		{"its disk fails", func(r *rot) { r.b.failOn("t3", "openwrite", syscall.EIO) }, OutcomeIOError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rotted(t)
			tc.setup(r)
			if got := r.repaired(true); got != tc.want {
				t.Errorf("%s, want %s", got, tc.want)
			}
			if r.repairState() != "" || r.b.count("SELECT count(*) FROM trash") != 0 {
				t.Errorf("journaled: %s", r.repairState())
			}
			if got := r.b.read("t3", "a"); got == "x" {
				t.Error("written")
			}
		})
	}
}

// A disk with no room for a saved chunk takes no other repair in the session, as each would be journaled and undone for
// nothing (specs.md §16.1, §17.1).
func TestARepairOnAFullDisk(t *testing.T) {
	r := rotted(t)
	r.b.failOn("t3", "create", syscall.ENOSPC)
	s := r.b.opened()
	outcome, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks)
	if err != nil || outcome != OutcomeNoRoom || !s.Full("t3") {
		t.Fatalf("%s, %v, full %v", outcome, err, s.Full("t3"))
	}
	if r.repairState() != "undone" || r.b.read("t3", "a") != "y" {
		t.Errorf("the operation is %s", r.repairState())
	}
	delete(r.b.fail, r.b.dirs["t3"])
	outcome, err = s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks)
	if err != nil || outcome != OutcomeNoRoom || r.repairState() != "undone" {
		t.Errorf("again in the session: %s, %v, %s", outcome, err, r.repairState())
	}
}

// A copy that cannot be read to save its chunk, or that changes before the first write, is left as it was. One that
// cannot be read either when the repair looks at it after keeps its operation planned, for the reprise: the session
// says so, and the next run undoes it, as nothing was saved.
func TestARepairThatStopsBeforeItWrites(t *testing.T) {
	r := rotted(t)
	r.b.failOn("t3", "readback", syscall.EIO)
	s := r.b.opened()
	outcome, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks)
	if err != nil || outcome != OutcomeIOError || !s.Lost() {
		t.Errorf("unreadable: %s, %v, lost %v", outcome, err, s.Lost())
	}
	s.Close()
	if r.repairState() != "planned" || r.b.read("t3", "a") != "y" {
		t.Errorf("the operation is %s", r.repairState())
	}
	delete(r.b.fail, r.b.dirs["t3"])
	r.b.opened().Close()
	if r.repairState() != "undone" || r.b.read("t3", "a") != "y" || r.b.count("SELECT count(*) FROM trash") != 0 {
		t.Errorf("after the reprise: %s", r.repairState())
	}
	r = rotted(t)
	r.b.at["chunks-saved"] = func() {
		path := filepath.Join(r.b.dirs["t3"], "media", "a")
		if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.repaired(true); got != OutcomeChanged {
		t.Errorf("edited before the write: %s", got)
	}
	if r.b.read("t3", "a") != "y" {
		t.Error("written over an edit")
	}
}

// A healthy copy that cannot be read whole as its hashes say holds no version to write: the repair says changed, and
// writes nothing. Its disk failing is that disk's loss, not the repaired one's (invariant 4).
func TestARepairWhoseHealthyCopyFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(r *rot)
		want  string
		t1Out bool
	}{
		{"cannot be opened", func(r *rot) { r.b.failOn("t1", "openfile", syscall.EACCES) }, OutcomeChanged, false},
		{"its disk fails", func(r *rot) { r.b.failOn("t1", "openfile", syscall.EIO) }, OutcomeIOError, true},
		{"cannot be read", func(r *rot) { r.b.failOn("t1", "read", syscall.EIO) }, OutcomeChanged, false},
		{"shorter once opened", func(r *rot) {
			r.b.fail[r.b.dirs["t1"]] = func(call, _ string) error {
				if call == "read" {
					if err := os.Truncate(filepath.Join(r.b.dirs["t1"], "media", "a"), 0); err != nil {
						r.b.t.Fatal(err)
					}
				}
				return nil
			}
		}, OutcomeChanged, false},
		{"another version", func(r *rot) { r.chunks = make([]byte, integrity.HashSize) }, OutcomeChanged, false},
		{"changed since its look", func(r *rot) { r.b.write("t1", "a", "z") }, OutcomeChanged, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rotted(t)
			tc.fail(r)
			if got := r.repaired(true); got != tc.want {
				t.Errorf("%s, want %s", got, tc.want)
			}
			if r.b.read("t3", "a") != "y" {
				t.Error("written")
			}
			if lost := r.b.diskState("t1") == "missing"; lost != tc.t1Out || r.b.diskState("t3") != "online" {
				t.Errorf("t1 %s, t3 %s", r.b.diskState("t1"), r.b.diskState("t3"))
			}
		})
	}
}

// failReadsFrom makes every read of t3's copy through a writable handle fail from the nth on: the first is the save
// of the chunk, the second the read back of what was written, and the others the repair's look at the copy whole.
func (r *rot) failReadsFrom(n int) {
	reads := 0
	r.b.fail[r.b.dirs["t3"]] = func(call, _ string) error {
		if call == "readback" {
			if reads++; reads >= n {
				return syscall.EIO
			}
		}
		return nil
	}
}

// A chunk written that cannot be read back is the disk's fault only when the disk no longer confirms (specs.md §16.1,
// step 5): the disk is lost then, and the next run that can read the copy finishes the repair.
func TestARepairWhoseChunkCannotBeReadBackOnADiskThatFails(t *testing.T) {
	r := rotted(t)
	r.failReadsFrom(2)
	r.b.at["chunk-written"] = func() { r.b.unread[r.b.dirs["t3"]] = true }
	if got := r.repaired(true); got != OutcomeIOError {
		t.Errorf("%s", got)
	}
	if r.b.diskState("t3") != "missing" || r.repairState() != "planned" {
		t.Errorf("t3 is %s, the operation %s", r.b.diskState("t3"), r.repairState())
	}
	delete(r.b.fail, r.b.dirs["t3"])
	delete(r.b.unread, r.b.dirs["t3"])
	r.b.opened().Close()
	if r.repairState() != "done" || r.b.read("t3", "a") != "x" {
		t.Errorf("after the reprise, the operation is %s", r.repairState())
	}
	// A read back that fails once, then a whole read that finds every chunk written, is a repair done.
	r = rotted(t)
	r.b.failNth("t3", "readback", 2, syscall.EIO)
	if got := r.repaired(true); got != OutcomeRepaired || r.repairState() != "done" {
		t.Errorf("a read back that failed once: %s, %s", got, r.repairState())
	}
}

// A chunk written that cannot be read back, on a disk that confirms, is the chunk's fault, never the disk's (invariant
// 4, specs.md §16.1): the repair stops with io-error, its operation planned for a reprise that can read it, and the
// disk stays one that is written to, in that run and the next, while the chunk still cannot be read. The disk was lost
// instead, by the repair's own look at the copy right after, then by the reprise of every later run, for as long as the
// sector could not be read: the copy could never reach the trash, nor anything else be written there.
func TestARepairWhoseChunkCannotBeReadBackLosesNoDiskThatConfirms(t *testing.T) {
	r := rotted(t)
	r.failReadsFrom(2)
	s := r.b.opened()
	outcome, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks)
	if err != nil || outcome != OutcomeIOError || !s.Lost() {
		t.Errorf("%s, %v, lost %v", outcome, err, s.Lost())
	}
	if !s.Writable("t3") || r.b.diskState("t3") != "online" || r.repairState() != "planned" {
		t.Errorf("writable %v, t3 is %s, the operation %s", s.Writable("t3"), r.b.diskState("t3"), r.repairState())
	}
	s.Close()
	// The next run, the chunk still unreadable: its reprise leaves the operation planned, and the disk to be written to.
	s = r.b.opened()
	if !s.Writable("t3") || !s.Lost() || r.b.diskState("t3") != "online" || r.repairState() != "planned" {
		t.Errorf("the next run: writable %v, lost %v, t3 is %s, the operation %s", s.Writable("t3"), s.Lost(), r.b.diskState("t3"), r.repairState())
	}
	s.Close()
	// Once it can be read, the reprise finds every chunk written, and finishes it.
	delete(r.b.fail, r.b.dirs["t3"])
	r.b.opened().Close()
	if r.repairState() != "done" || r.b.read("t3", "a") != "x" {
		t.Errorf("once readable, the operation is %s", r.repairState())
	}
}

// The reprise of a repair whose chunk still cannot be read, on a disk that no longer confirms, loses the disk, and
// the operation waits for a run that can read it (invariant 4).
func TestARepriseOfARepairOnADiskThatNoLongerConfirms(t *testing.T) {
	r := rotted(t)
	r.failReadsFrom(2)
	if got := r.repaired(true); got != OutcomeIOError || r.repairState() != "planned" {
		t.Fatalf("%s, the operation %s", got, r.repairState())
	}
	// The next run confirms the disk to open it, then once more after the read that fails: that one fails.
	mount := r.b.dirs["t3"]
	r.b.beforeRead[mount] = func() { r.b.beforeRead[mount] = func() { r.b.unread[mount] = true } }
	s := r.b.opened()
	if s.Writable("t3") || !s.Lost() || r.b.diskState("t3") != "missing" || r.repairState() != "planned" {
		t.Errorf("writable %v, lost %v, t3 is %s, the operation %s", s.Writable("t3"), s.Lost(), r.b.diskState("t3"), r.repairState())
	}
}

// A bad chunk that cannot be read has nothing to save: its place in the trash is an empty file, which no chunk is
// otherwise, the entry counts nothing for it, and the healthy copy's bytes are written over it, which is what makes a
// disk give the sector another place (specs.md §16.1).
func TestARepairOfAChunkThatCannotBeRead(t *testing.T) {
	r := rotted(t)
	s := r.b.opened()
	outcome, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, []int64{0}, r.healthy, r.chunks)
	if err != nil || outcome != OutcomeRepaired || r.b.read("t3", "a") != "x" {
		t.Fatalf("%s, %v, the copy holds %q", outcome, err, r.b.read("t3", "a"))
	}
	chunk, err := layout.ChunkName(0)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(r.b.dirs["t3"], "Atoll", "trash", filepath.FromSlash(r.b.text("SELECT CAST(trash_path AS TEXT) FROM trash")), chunk)
	if info, err := os.Stat(saved); err != nil || info.Size() != 0 || r.b.count("SELECT size FROM trash") != 0 {
		t.Errorf("the chunk saved: %v, %v", info, err)
	}
}

// A time that cannot be set back once every chunk was written keeps the operation planned; the reprise finds every
// chunk written, sets the time back and finishes it: the copy keeps its time (specs.md §16.1, invariant 6).
func TestARepairWhoseTimeCannotBeSetBack(t *testing.T) {
	r := rotted(t)
	path := filepath.Join(r.b.dirs["t3"], "media", "a")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	r.b.failOn("t3", "settimes", syscall.EPERM)
	if got := r.repaired(true); got != OutcomeIOError || r.repairState() != "planned" {
		t.Errorf("%s, the operation is %s", got, r.repairState())
	}
	delete(r.b.fail, r.b.dirs["t3"])
	r.b.opened().Close()
	after, err := os.Stat(path)
	if err != nil || r.b.read("t3", "a") != "x" || r.repairState() != "done" || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the copy holds %q from %v, was %v; the operation is %s", r.b.read("t3", "a"), after.ModTime(), before.ModTime(), r.repairState())
	}
}
