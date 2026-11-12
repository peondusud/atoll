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
