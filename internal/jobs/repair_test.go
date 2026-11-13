package jobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/trash"
)

// film is a file of three chunks, the last one short.
func film() []byte {
	data := make([]byte, 2*integrity.ChunkSize+1000)
	for i := range data {
		data[i] = byte(i*7 + i/4096)
	}
	return data
}

// rotChunks changes the first byte of each chunk given, on one disk, as rot does: the row takes the new times.
func (b *bench) rotChunks(id, path string, data []byte, chunks ...int64) []byte {
	b.t.Helper()
	rotten := bytes.Clone(data)
	for _, i := range chunks {
		rotten[i*integrity.ChunkSize] ^= 0xff
	}
	b.rot(id, path, string(rotten))
	return rotten
}

func (b *bench) inode(id, path string) uint64 {
	b.t.Helper()
	info, err := os.Stat(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		b.t.Fatal(err)
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	return st.Ino
}

func (b *bench) repair(opts RepairOptions) *RepairSummary {
	b.t.Helper()
	out, err := Repair(b.t.Context(), b.db, b.config, b, opts)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// repairBench holds a film on two disks, scanned whole and due for a scrub.
func repairBench(t *testing.T, more string) (*bench, []byte) {
	t.Helper()
	b := newBenchWith(t, more, "t1", "t2")
	data := film()
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a.mkv", string(data))
	}
	b.scan(true)
	b.now += int64(month)
	return b, data
}

func (b *bench) savedChunk(id string, opID, index int64) []byte {
	b.t.Helper()
	entry := b.text("SELECT CAST(trash_path AS TEXT) FROM trash WHERE op_id = ?", opID)
	chunk, err := layout.ChunkName(index)
	if err != nil {
		b.t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b.dirs[id], "Atoll", "trash", filepath.FromSlash(entry), chunk))
	if err != nil {
		b.t.Fatalf("the chunk saved: %v", err)
	}
	return data
}

// A copy that rotted is repaired in place by the scrub, with auto_repair: only its bad chunk is written, from the
// healthy copy, after its bytes reached the trash, and the copy keeps its inode and its time (specs.md §16.1).
func TestScrubRepairsAChunkInPlace(t *testing.T) {
	b, data := repairBench(t, "")
	rotten := b.rotChunks("t2", "a.mkv", data, 1)
	before := b.inode("t2", "a.mkv")
	mtime := b.text("SELECT mtime_ns FROM copies WHERE disk_id = 't2'")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t2 := diskOf(out, "t2")
	if !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t2.Repaired, []string{"a.mkv"}) || len(t2.Quarantined) != 0 || len(t2.Kept) != 0 || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.read("t2", "a.mkv"); got != string(data) {
		t.Error("the copy does not hold the film again")
	}
	if b.inode("t2", "a.mkv") != before {
		t.Error("the copy is another file")
	}
	if got := b.text("SELECT mtime_ns FROM copies WHERE disk_id = 't2'"); got != mtime {
		t.Errorf("the time moved: %s, was %s", got, mtime)
	}
	info, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "a.mkv"))
	if err != nil || strconv.FormatInt(info.ModTime().UnixNano(), 10) != mtime {
		t.Errorf("the time on the disk is not set back: %v", err)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops"); got != "repair:done" {
		t.Errorf("ops %s", got)
	}
	op := b.count("SELECT id FROM ops")
	day := time.Unix(0, b.now).UTC().Format("2006-01-02")
	if got := b.text("SELECT reason || ' ' || size || ' ' || (blake3 IS NULL) || ' ' || CAST(trash_path AS TEXT) FROM trash"); got != "repaired 4194304 1 "+day+"/"+strconv.Itoa(op)+".chunks/a.mkv" {
		t.Errorf("the trash holds %q", got)
	}
	if saved := b.savedChunk("t2", int64(op), 1); !bytes.Equal(saved, rotten[integrity.ChunkSize:2*integrity.ChunkSize]) {
		t.Error("the chunk saved is not what rotted")
	}
	if got := b.text("SELECT state || ' ' || (verified_ns IS NOT NULL) FROM copies WHERE disk_id = 't2'"); got != "ok 1" {
		t.Errorf("the copy is %s", got)
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("the file is %s", got)
	}
	if got := b.text("SELECT group_concat(kind) FROM events WHERE kind IN ('corrupt', 'repair', 'repaired')"); got != "corrupt,repair,repaired" {
		t.Errorf("events %s", got)
	}
	if parts, err := os.ReadDir(filepath.Join(b.dirs["t2"], "Atoll", "tmp")); err == nil && len(parts) != 0 {
		t.Errorf("parts left: %v", parts)
	}
	if got := b.read("t1", "a.mkv"); got != string(data) {
		t.Error("the healthy copy changed")
	}
}

// Without --yes, the scrub only marks the copy and says it is to be repaired; atoll repair then says the same, and
// repairs it with Apply (specs.md §16.1, §25).
func TestRepairSaysThenRepairs(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 0, 2)
	out := b.scrub(ScrubOptions{Apply: true})
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Repaired, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	said := b.repair(RepairOptions{Record: true})
	if len(said.Items) != 1 || said.Items[0] != (RepairItem{Disk: "t2", Path: "a.mkv", Mode: RepairChunk, Chunks: 2, Outcome: OutcomeToRepair}) || said.Applied {
		t.Fatalf("%+v", said)
	}
	if b.count("SELECT count(*) FROM ops") != 0 || b.read("t2", "a.mkv") == string(data) {
		t.Fatal("something was written")
	}
	done := b.repair(RepairOptions{Apply: true})
	if len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired || done.Items[0].Chunks != 2 {
		t.Fatalf("%+v", done)
	}
	if b.read("t2", "a.mkv") != string(data) || b.text("SELECT state FROM files") != "ok" {
		t.Error("not repaired")
	}
	if again := b.repair(RepairOptions{Apply: true}); len(again.Items) != 0 {
		t.Errorf("repaired twice: %+v", again)
	}
}

// An edit made while the chunks were being saved is never overwritten: the repair stops, the chunks it saved stay in
// the trash, and the copy is left to the next pass (specs.md §16.1).
func TestARepairNeverOverwritesAnEdit(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 1)
	edited := strings.Repeat("e", len(data))
	b.at["chunks-saved"] = func() { b.write("t2", "a.mkv", edited) }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if b.read("t2", "a.mkv") != edited {
		t.Error("the edit was overwritten")
	}
	if got := b.text("SELECT state FROM ops"); got != "done" {
		t.Errorf("the repair is %s, with a chunk in the trash", got)
	}
	if got := b.text("SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'repair-stopped'"); got != "changed" {
		t.Errorf("stopped for %q", got)
	}
	if got := b.text("SELECT size FROM trash"); got != "4194304" {
		t.Errorf("the entry holds %s bytes", got)
	}
}

// A repair stopped at each step, as kill -9 would stop it, is resolved by the next command that writes: finished
// once its writes were all done, its time set back otherwise, never another write over the copy (specs.md §16.1).
func TestARepairStoppedAtEachStep(t *testing.T) {
	for _, tc := range []struct {
		point string
		bad   []int64
		// what the copy holds after the reprise, and the state of the repair and of the copy
		repaired bool
		op, copy string
		entry    bool
	}{
		{"journaled", []int64{1}, false, "undone", "corrupt", false},
		{"chunk-saved", []int64{1}, false, "done", "corrupt", true},
		{"chunks-saved", []int64{0, 2}, false, "done", "corrupt", true},
		{"chunk-written", []int64{1}, true, "done", "ok", true},
		{"chunk-written", []int64{0, 2}, false, "done", "corrupt", true},
		{"times-set", []int64{0, 2}, true, "done", "ok", true},
	} {
		t.Run(tc.point+"/"+strconv.Itoa(len(tc.bad)), func(t *testing.T) {
			b, data := repairBench(t, "")
			b.rotChunks("t2", "a.mkv", data, tc.bad...)
			b.scrub(ScrubOptions{Apply: true})
			stop := errors.New("stopped")
			b.at[tc.point] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop at %s: %v", tc.point, err)
					}
				}()
				_, _ = Repair(t.Context(), b.db, b.config, b, RepairOptions{Apply: true})
			}()
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Fatalf("the repair is %s", got)
			}
			b.pass()
			if got := b.text("SELECT state FROM ops"); got != tc.op {
				t.Errorf("the repair is %s, want %s", got, tc.op)
			}
			if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != tc.copy {
				t.Errorf("the copy is %s, want %s", got, tc.copy)
			}
			if got := b.count("SELECT count(*) FROM trash"); (got == 1) != tc.entry {
				t.Errorf("%d entries", got)
			}
			if (b.read("t2", "a.mkv") == string(data)) != tc.repaired {
				t.Errorf("repaired: %v", !tc.repaired)
			}
			// Whatever the crash left, the copy's row says again what its disk holds: a repair now finishes.
			if !tc.repaired {
				if done := b.repair(RepairOptions{Apply: true}); len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired {
					t.Fatalf("the next repair: %+v", done)
				}
				if b.read("t2", "a.mkv") != string(data) {
					t.Error("not repaired by the next repair")
				}
			}
			if got := b.text("SELECT state FROM files"); got != "ok" {
				t.Errorf("the file is %s", got)
			}
		})
	}
}

// A disk with no room for the chunks a repair saves: nothing is written to the copy, the repair leaves nothing in
// the trash, and the scrub quarantines the copy, as a move to the trash takes no room but its folders (specs.md
// §16.1).
func TestARepairWithNoRoomQuarantines(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 1)
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "create" && strings.HasSuffix(name, ".part") {
			return syscall.ENOSPC
		}
		return nil
	}
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || !slices.Equal(t2.Quarantined, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops ORDER BY id"); got != "repair:undone,trash:done" {
		t.Errorf("ops %s", got)
	}
	if got := b.text("SELECT reason FROM trash"); got != "quarantined" {
		t.Errorf("the trash holds %q", got)
	}
}

// badWrites is a disk whose writes in place do not take: a file opened to be written in place holds, after a write,
// the bytes written with the last one flipped, as a sector that cannot be written again would leave them. That is
// neither what the write held nor what was there before.
type badWrites struct{ fsx.Dir }

func (s *badWrites) Unwrap() fsx.Dir { return s.Dir }

func (s *badWrites) OpenDir(name string) (fsx.Dir, error) {
	d, err := s.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badWrites{Dir: d}, nil
}

func (s *badWrites) OpenWritable(name string) (fsx.WritableFile, error) {
	f, err := s.Dir.OpenWritable(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badWritten{WritableFile: f}, nil
}

type badWritten struct{ fsx.WritableFile }

func (f *badWritten) WriteAt(p []byte, off int64) (int, error) {
	wrong := bytes.Clone(p)
	if len(wrong) > 0 {
		wrong[len(wrong)-1] ^= 0xff
	}
	return f.WritableFile.WriteAt(wrong, off) //nolint:wrapcheck // a test double
}

// A repair that fails otherwise than for want of room leaves the copy corrupt, and the scrub that found it sends it to
// the trash, as it does without auto_repair: the view never serves it, and the next pass makes it again from the
// healthy copy (specs.md §16, §16.1, invariant 14). It stayed in media/, listed as kept, and no scrub read it again.
func TestARepairThatFailsQuarantines(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 1)
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &badWrites{Dir: top} }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || !slices.Equal(t2.Quarantined, []string{"a.mkv"}) || len(t2.Kept) != 0 {
		t.Fatalf("%+v", t2)
	}
	if got := b.text("SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'repair-stopped'"); got != "io-error" {
		t.Errorf("stopped for %q", got)
	}
	if b.exists("t2", "media/a.mkv") {
		t.Error("the copy a repair could not mend is still in media/")
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops ORDER BY id"); got != "repair:done,trash:done" {
		t.Errorf("ops %s", got)
	}
	if got := b.text("SELECT group_concat(reason) FROM trash ORDER BY id"); got != "repaired,quarantined" {
		t.Errorf("the trash holds %q", got)
	}
	if n := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2'"); n != 0 {
		t.Errorf("%d copies recorded on the disk whose copy went to the trash", n)
	}
	// The healthy copy is the one the next pass copies from.
	delete(b.tops, b.dirs["t2"])
	b.settle()
	if b.read("t2", "a.mkv") != string(data) {
		t.Error("the copy is not made again from the healthy one")
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("the file is %s", got)
	}
}

// A repair that stopped where nothing could resolve it stays planned, and only a reprise can tell what its copy holds
// (specs.md §17.2): the scrub moves nothing that an operation still planned names, and the copy stays where it is.
func TestARepairLeftPlannedIsNotQuarantined(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 1)
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &badWrites{Dir: top} }
	written := false
	b.at["chunk-written"] = func() { written = true }
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		// What the repair saved cannot be looked at any more, and nothing says the disk is gone.
		if written && call == "opendir" && strings.HasSuffix(name, ".chunks") {
			return syscall.EACCES
		}
		return nil
	}
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true, Disk: "t2"})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops ORDER BY id"); got != "repair:planned" {
		t.Errorf("ops %s", got)
	}
	if !b.exists("t2", "media/a.mkv") {
		t.Error("a copy an operation still planned names was moved")
	}
	if !out.Partial {
		t.Error("an operation stays planned, and the scrub does not say so")
	}
}

// droppedWrites is a disk that drops a write in place, as a bridge that says a write is done and did none: the file
// holds what it held, and its times have moved.
type droppedWrites struct{ fsx.Dir }

func (s *droppedWrites) Unwrap() fsx.Dir { return s.Dir }

func (s *droppedWrites) OpenDir(name string) (fsx.Dir, error) {
	d, err := s.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &droppedWrites{Dir: d}, nil
}

func (s *droppedWrites) OpenWritable(name string) (fsx.WritableFile, error) {
	f, err := s.Dir.OpenWritable(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &droppedWrite{WritableFile: f}, nil
}

type droppedWrite struct{ fsx.WritableFile }

// WriteAt writes what was there, until the change time has moved, which it may not do within a clock tick.
func (f *droppedWrite) WriteAt(p []byte, off int64) (int, error) {
	was := make([]byte, len(p))
	if _, err := f.ReadAt(was, off); err != nil {
		return 0, err //nolint:wrapcheck // a test double
	}
	before, err := f.Info()
	if err != nil {
		return 0, err //nolint:wrapcheck // a test double
	}
	for range 2000 {
		if _, err := f.WritableFile.WriteAt(was, off); err != nil {
			return 0, err //nolint:wrapcheck // a test double
		}
		after, err := f.Info()
		if err != nil {
			return 0, err //nolint:wrapcheck // a test double
		}
		if after.Ctime != before.Ctime {
			return len(p), nil
		}
		time.Sleep(time.Millisecond)
	}
	return 0, errors.New("the change time did not move")
}

// A write that the disk drops leaves the copy as the repair found it, but for its times, which the repair's own write
// moved: that is no change of anyone else's. The repair stops, the copy's row says again what its disk holds, and the
// next repair begins again (specs.md §16.1). Its row kept the times from before the write, so that every repair
// after it found a changed copy, which it left to the next pass, and a pass leaves a corrupt file to a repair: the
// copy stayed corrupt in media/ for good.
func TestARepairWhoseWriteIsDroppedBeginsAgain(t *testing.T) {
	b, data := repairBench(t, "")
	rotten := b.rotChunks("t2", "a.mkv", data, 1)
	b.scrub(ScrubOptions{Apply: true})
	mtime := b.mtimeOf("t2", "a.mkv")
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &droppedWrites{Dir: top} }
	if done := b.repair(RepairOptions{Apply: true}); len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeIOError {
		t.Fatalf("%+v", done)
	}
	if b.read("t2", "a.mkv") != string(rotten) {
		t.Fatal("the double wrote something")
	}
	if got := b.mtimeOf("t2", "a.mkv"); got != mtime {
		t.Errorf("the modification time is not set back: %d, was %d", got, mtime)
	}
	if got := b.text("SELECT ctime_ns FROM copies WHERE disk_id = 't2'"); got != strconv.FormatInt(b.ctimeOf("t2", "a.mkv"), 10) {
		t.Error("the row of the copy does not say the change time the repair's own write left")
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops ORDER BY id"); got != "repair:done" {
		t.Errorf("ops %s", got)
	}
	// On a disk that writes again, the next repair finds the copy its row says, and repairs it.
	delete(b.tops, b.dirs["t2"])
	if done := b.repair(RepairOptions{Apply: true}); len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired {
		t.Fatalf("the next repair: %+v", done)
	}
	if b.read("t2", "a.mkv") != string(data) {
		t.Error("not repaired by the next repair")
	}
}

// And the scrub that found the copy sends it to the trash, as after any repair that fails (specs.md §16).
func TestARepairWhoseWriteIsDroppedQuarantines(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 1)
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &droppedWrites{Dir: top} }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || !slices.Equal(t2.Quarantined, []string{"a.mkv"}) || len(t2.Kept) != 0 {
		t.Fatalf("%+v", t2)
	}
	if b.exists("t2", "media/a.mkv") {
		t.Error("the copy a repair could not mend is still in media/")
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops ORDER BY id"); got != "repair:done,trash:done" {
		t.Errorf("ops %s", got)
	}
	delete(b.tops, b.dirs["t2"])
	b.settle()
	if b.read("t2", "a.mkv") != string(data) {
		t.Error("the copy is not made again from the healthy one")
	}
}

// The disk of the healthy copy is the one lost when it fails while a repair reads from it, never the disk repaired,
// whose repair is resolved at once: nothing was written from a copy that could not be read. And the corrupt copy
// stays where it is, as no healthy copy can be read any more (specs.md §16, §16.1, invariant 4). The disk repaired
// was lost in its place, its repair left planned, and the disk that failed stayed in service.
func TestAHealthyDiskThatFailsIsTheOneLost(t *testing.T) {
	b, data := repairBench(t, "")
	rotten := b.rotChunks("t2", "a.mkv", data, 1)
	late := false
	b.at["chunks-saved"] = func() { late = true }
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if late && call == "openfile" && name == "a.mkv" {
			return syscall.EIO
		}
		return nil
	}
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true, Disk: "t2"})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if !out.Partial {
		t.Error("a disk failed, and the scrub does not say so")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got == "missing" {
		t.Error("the disk repaired is lost for an error of the healthy copy's")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't1'"); got != "missing" {
		t.Errorf("the disk of the healthy copy, which failed, is %s", got)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops ORDER BY id"); got != "repair:done" {
		t.Errorf("ops %s", got)
	}
	if b.read("t2", "a.mkv") != string(rotten) {
		t.Error("the corrupt copy is not what it was")
	}
}

// With repair_mode = "file", the corrupt copy goes to the trash, repaired, and is made again at once from the
// healthy one, as a new file (specs.md §16.1).
func TestScrubRepairsAWholeFile(t *testing.T) {
	b, data := repairBench(t, "[integrity]\nrepair_mode = \"file\"\n[defaults]\nsettle = \"0s\"\n")
	b.rotChunks("t2", "a.mkv", data, 1)
	before := b.inode("t2", "a.mkv")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Repaired, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if b.read("t2", "a.mkv") != string(data) {
		t.Fatal("not made again")
	}
	if b.inode("t2", "a.mkv") == before {
		t.Error("the same file: a repair by file makes a new one")
	}
	if got := b.text("SELECT reason || ' ' || (blake3 IS NULL) FROM trash"); got != "repaired 1" {
		t.Errorf("the trash holds %q", got)
	}
}

// A copy marked corrupt that reads right after all, read whole now, is verified, and nothing is written.
func TestRepairOfACopyThatReadsRight(t *testing.T) {
	b, _ := repairBench(t, "")
	b.exec("UPDATE copies SET state = 'corrupt' WHERE disk_id = 't2'")
	b.exec("UPDATE files SET state = 'corrupt'")
	out := b.repair(RepairOptions{Apply: true})
	if len(out.Items) != 1 || out.Items[0].Outcome != OutcomeVerified || out.Items[0].Chunks != 0 {
		t.Fatalf("%+v", out)
	}
	if b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("something was journaled")
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
		t.Errorf("the copy is %s", got)
	}
}

// With no healthy copy, a corrupt copy stays as it is.
func TestRepairWithNoHealthyCopy(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t1", "a.mkv", data, 0)
	b.rotChunks("t2", "a.mkv", data, 1)
	b.scrub(ScrubOptions{Apply: true})
	out := b.repair(RepairOptions{Apply: true})
	if len(out.Items) != 2 || out.Items[0].Outcome != OutcomeNoHealthyCopy || out.Items[1].Outcome != OutcomeNoHealthyCopy {
		t.Fatalf("%+v", out)
	}
	if b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("something was journaled")
	}
}

// The chunks a repair saved make an entry of the trash that is listed, never restored, and purged whole, its folders
// with it (specs.md §17).
func TestTheChunksOfARepairInTheTrash(t *testing.T) {
	b, data := repairBench(t, "")
	b.rotChunks("t2", "a.mkv", data, 1)
	b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	session, err := trash.Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	if _, err := session.Restore(t.Context(), "a.mkv", func(e *trash.EntryItem) error {
		outcomes = append(outcomes, e.Outcome)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	if !slices.Equal(outcomes, []string{trash.OutcomeNotACopy}) || b.read("t2", "a.mkv") != string(data) {
		t.Fatalf("restored: %v", outcomes)
	}
	session, err = trash.Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	none := time.Duration(0)
	outcomes = nil
	if _, err := session.Purge(t.Context(), &none, func(e *trash.EntryItem) error {
		outcomes = append(outcomes, e.Outcome)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	if !slices.Equal(outcomes, []string{trash.OutcomePurged}) || b.count("SELECT count(*) FROM trash") != 0 {
		t.Fatalf("purged: %v", outcomes)
	}
	if entries, err := os.ReadDir(filepath.Join(b.dirs["t2"], "Atoll", "trash")); err != nil || len(entries) != 0 {
		t.Errorf("the trash still holds %v, %v", entries, err)
	}
}

// A copy with more than one link is repaired by chunk whatever repair_mode says, so that every name of it stays the
// same file (specs.md §16.1).
func TestAHardLinkedCopyIsRepairedByChunk(t *testing.T) {
	b := newBenchWith(t, "[integrity]\nrepair_mode = \"file\"\n", "t1", "t2")
	data := film()
	b.write("t1", "a.mkv", string(data))
	b.write("t2", "a.mkv", string(data))
	if err := os.Link(filepath.Join(b.dirs["t2"], "media", "a.mkv"), filepath.Join(b.dirs["t2"], "media", "b.mkv")); err != nil {
		t.Fatal(err)
	}
	b.write("t1", "b.mkv", string(data))
	b.scan(true)
	b.now += int64(month)
	before := b.inode("t2", "a.mkv")
	b.rotChunks("t2", "a.mkv", data, 1)
	b.exec("UPDATE copies SET mtime_ns = (SELECT mtime_ns FROM copies o WHERE o.disk_id = 't2' AND o.file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv')), ctime_ns = (SELECT ctime_ns FROM copies o WHERE o.disk_id = 't2' AND o.file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv')) WHERE disk_id = 't2'")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true, Under: "a.mkv"})
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Repaired, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if b.inode("t2", "a.mkv") != before || b.inode("t2", "b.mkv") != before {
		t.Error("a name of the copy is another file now")
	}
	if b.read("t2", "b.mkv") != string(data) {
		t.Error("the other name does not hold the film again")
	}
	if got := b.text("SELECT CAST(trash_path AS TEXT) LIKE '%.chunks/a.mkv' FROM trash"); got != "1" {
		t.Error("not repaired by chunk")
	}
}

// A healthy copy that no longer holds what its hashes say once read stops the repair before anything is written
// from it (specs.md §16.1).
func TestARepairStopsWhenTheHealthyCopyChanges(t *testing.T) {
	b, data := repairBench(t, "")
	rotten := b.rotChunks("t2", "a.mkv", data, 1)
	b.at["chunks-saved"] = func() { b.rot("t1", "a.mkv", strings.Repeat("x", len(data))) }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true, Disk: "t2"})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if b.read("t2", "a.mkv") != string(rotten) {
		t.Error("something was written from a copy that changed")
	}
	if got := b.text("SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'repair-stopped'"); got != "changed" {
		t.Errorf("stopped for %q", got)
	}
}

// badReads is a disk that reads its files wrong while on is set: the first byte of each read flipped, as a disk that
// starts to fail would read them, with no change to their times.
type badReads struct {
	fsx.Dir
	on *bool
}

func (s *badReads) Unwrap() fsx.Dir { return s.Dir }

func (s *badReads) OpenDir(name string) (fsx.Dir, error) {
	d, err := s.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badReads{Dir: d, on: s.on}, nil
}

func (s *badReads) OpenFile(name string) (fsx.File, error) {
	f, err := s.Dir.OpenFile(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badFile{File: f, on: s.on}, nil
}

type badFile struct {
	fsx.File
	on *bool
}

func (f *badFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	if *f.on && n > 0 {
		p[0] ^= 0xff
	}
	return n, err //nolint:wrapcheck // a test double
}

// A healthy copy that its disk reads wrong once the repair reads it again is never written from: each chunk read from
// it is checked against its hash before it is written (specs.md §16.1).
func TestARepairChecksWhatItWrites(t *testing.T) {
	b, data := repairBench(t, "")
	rotten := b.rotChunks("t2", "a.mkv", data, 1)
	on := false
	b.tops[b.dirs["t1"]] = func(top fsx.Dir) fsx.Dir { return &badReads{Dir: top, on: &on} }
	b.at["chunks-saved"] = func() { on = true }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true, Disk: "t2"})
	if t2 := diskOf(out, "t2"); len(t2.Repaired) != 0 || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
		t.Fatalf("%+v", t2)
	}
	if b.read("t2", "a.mkv") != string(rotten) {
		t.Error("something was written from a chunk read wrong")
	}
	if got := b.text("SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'repair-stopped'"); got != "changed" {
		t.Errorf("stopped for %q", got)
	}
}

// What a repair leaves in tmp/ goes like any other part: the chunk it was saving when it stopped,
// <op-id>.<index>.part, is removed when a command that writes next opens the disk, once the repair is done or undone,
// and nothing else there is touched (specs.md §14.2, §16.1). Nothing knew those names: each repair that failed on a
// disk with no room, which every scrub tries again, left up to 4 MiB there for good.
func TestTheChunkARepairWasSavingGoesFromTmp(t *testing.T) {
	t.Run("after a failure", func(t *testing.T) {
		b, data := repairBench(t, "")
		b.rotChunks("t2", "a.mkv", data, 1)
		b.scrub(ScrubOptions{Apply: true})
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "write" && strings.HasSuffix(name, ".part") {
				return syscall.ENOSPC
			}
			return nil
		}
		if failed := b.repair(RepairOptions{Apply: true}); len(failed.Items) != 1 || failed.Items[0].Outcome == trash.OutcomeRepaired {
			t.Fatalf("%+v", failed)
		}
		op := b.text("SELECT id FROM ops WHERE kind = 'repair' AND state = 'undone'")
		left := op + ".1.part"
		if got := b.parts("t2"); !slices.Equal(got, []string{left}) {
			t.Fatalf("the repair that failed left %q in tmp/, want %q", got, left)
		}
		// What no repair of this disk names stays, however like a repair's its name is.
		others := []string{op + ".01.part", op + ".1.part.old", "999.1.part"}
		for _, name := range others {
			if err := os.WriteFile(filepath.Join(b.dirs["t2"], "Atoll", "tmp", name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		delete(b.fail, b.dirs["t2"])
		if done := b.repair(RepairOptions{Apply: true}); len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired {
			t.Fatalf("%+v", done)
		}
		slices.Sort(others)
		if got := b.parts("t2"); !slices.Equal(got, others) {
			t.Errorf("tmp/ holds %q, want %q", got, others)
		}
	})
	t.Run("after a crash", func(t *testing.T) {
		b, data := repairBench(t, "")
		b.rotChunks("t2", "a.mkv", data, 1)
		b.scrub(ScrubOptions{Apply: true})
		stop := errors.New("stopped")
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			// The chunk is written and synced in tmp/, and the machine stops as it is moved to the trash.
			if (call == "rename" || call == "link") && name == "00000001" {
				panic(stop) //nolint:forbidigo // what stands for kill -9
			}
			return nil
		}
		func() {
			defer func() {
				if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
					t.Fatalf("it did not stop as the chunk was moved: %v", err)
				}
			}()
			_, _ = Repair(t.Context(), b.db, b.config, b, RepairOptions{Apply: true})
		}()
		op := b.text("SELECT id FROM ops WHERE kind = 'repair' AND state = 'planned'")
		if got := b.parts("t2"); !slices.Equal(got, []string{op + ".1.part"}) {
			t.Fatalf("the crash left %q in tmp/", got)
		}
		delete(b.fail, b.dirs["t2"])
		// The next command that writes resolves the repair, then cleans tmp/: the part goes with it.
		if done := b.repair(RepairOptions{Apply: true}); len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired {
			t.Fatalf("%+v", done)
		}
		if got := b.text("SELECT state FROM ops WHERE id = ?", op); got != "undone" {
			t.Errorf("the repair the crash left is %s", got)
		}
		if got := b.parts("t2"); len(got) != 0 {
			t.Errorf("tmp/ holds %q", got)
		}
		if b.read("t2", "a.mkv") != string(data) {
			t.Error("not repaired")
		}
	})
}
