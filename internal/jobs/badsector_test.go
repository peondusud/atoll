package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/trash"
)

// sectors is a disk with sectors that cannot be read: the chunks of a file, by its name, that answer EIO to a read
// until something is written over them, which is when a drive gives a bad sector another place.
type sectors struct {
	mu  sync.Mutex
	bad map[string]map[int64]bool
	// stuck says that writing over one does not mend it: a drive with no spare sector left.
	stuck bool
	// once says that a chunk fails its first read alone, as a cable or a controller makes one.
	once bool
	// failed is called at each read that fails.
	failed func()
	fails  int
}

func (s *sectors) reads(name string, off int64, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := off / integrity.ChunkSize; i*integrity.ChunkSize < off+int64(n); i++ {
		if s.bad[name][i] {
			s.fails++
			if s.once {
				delete(s.bad[name], i)
			}
			if s.failed != nil {
				s.failed()
			}
			return syscall.EIO
		}
	}
	return nil
}

func (s *sectors) writes(name string, off int64, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stuck {
		return
	}
	for i := off / integrity.ChunkSize; i*integrity.ChunkSize < off+int64(n); i++ {
		delete(s.bad[name], i)
	}
}

func (s *sectors) left(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bad[name])
}

// badSectors dresses the top of a disk with them.
type badSectors struct {
	fsx.Dir
	s *sectors
}

func (d *badSectors) Unwrap() fsx.Dir { return d.Dir }

func (d *badSectors) OpenDir(name string) (fsx.Dir, error) {
	sub, err := d.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badSectors{Dir: sub, s: d.s}, nil
}

func (d *badSectors) OpenFile(name string) (fsx.File, error) {
	f, err := d.Dir.OpenFile(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badRead{File: f, name: name, s: d.s}, nil
}

func (d *badSectors) OpenWritable(name string) (fsx.WritableFile, error) {
	f, err := d.Dir.OpenWritable(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &badWritable{WritableFile: f, name: name, s: d.s}, nil
}

type badRead struct {
	fsx.File
	name string
	s    *sectors
	at   int64
}

func (f *badRead) ReadAt(p []byte, off int64) (int, error) {
	if err := f.s.reads(f.name, off, len(p)); err != nil {
		return 0, err
	}
	return f.File.ReadAt(p, off) //nolint:wrapcheck // a test double
}

func (f *badRead) Read(p []byte) (int, error) {
	if err := f.s.reads(f.name, f.at, len(p)); err != nil {
		return 0, err
	}
	n, err := f.File.Read(p)
	f.at += int64(n)
	return n, err //nolint:wrapcheck // a test double
}

type badWritable struct {
	fsx.WritableFile
	name string
	s    *sectors
}

func (f *badWritable) ReadAt(p []byte, off int64) (int, error) {
	if err := f.s.reads(f.name, off, len(p)); err != nil {
		return 0, err
	}
	return f.WritableFile.ReadAt(p, off) //nolint:wrapcheck // a test double
}

func (f *badWritable) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.WritableFile.WriteAt(p, off)
	if err == nil {
		f.s.writes(f.name, off, n)
	}
	return n, err //nolint:wrapcheck // a test double
}

// sectorBench is repairBench with a chunk of the copy on t2 that cannot be read.
func sectorBench(t *testing.T, chunks ...int64) (*bench, []byte, *sectors) {
	t.Helper()
	b, data := repairBench(t, "")
	sec := &sectors{bad: map[string]map[int64]bool{"a.mkv": {}}}
	for _, i := range chunks {
		sec.bad["a.mkv"][i] = true
	}
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir { return &badSectors{Dir: top, s: sec} }
	return b, data, sec
}

// A chunk that cannot be read is the copy's fault when its disk still confirms, not the disk's (specs.md §16, as the
// maintainer decided on 2026-11-13): the copy is corrupt, and repaired in place from a healthy one, which writes over
// the sector, or sent to the trash. The scrub took any read error for the disk's: the disk went missing, came back at
// the next mount check, and the next slice read the same copy first, without end, the copy never named nor mended.
func TestAChunkThatCannotBeReadIsItsCopysFault(t *testing.T) {
	b, data, sec := sectorBench(t, 1)
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t2 := diskOf(out, "t2")
	if out.Partial || t2 == nil || !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t2.Repaired, []string{"a.mkv"}) {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "online" {
		t.Errorf("the disk is %s", got)
	}
	if sec.left("a.mkv") != 0 {
		t.Error("nothing was written over the chunk that could not be read")
	}
	if b.read("t2", "a.mkv") != string(data) {
		t.Error("the copy does not hold the file's content")
	}
	if got := b.text("SELECT group_concat(state, ' ') FROM copies"); got != "ok ok" {
		t.Errorf("copies %s", got)
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("the file is %s", got)
	}
	// What could not be read could not be saved: its place in the trash holds nothing, and says which chunk it was.
	entry := b.text("SELECT CAST(trash_path AS TEXT) FROM trash WHERE reason = 'repaired'")
	info, err := os.Stat(filepath.Join(b.dirs["t2"], "Atoll", "trash", filepath.FromSlash(entry), "00000001"))
	if err != nil || info.Size() != 0 {
		t.Errorf("the chunk in the trash: %v, %v", info, err)
	}
	if got := b.text("SELECT size FROM trash WHERE reason = 'repaired'"); got != "0" {
		t.Errorf("the entry holds %s bytes", got)
	}
}

// A disk that does not confirm after a read failed is lost, as before: nothing is concluded of its copy.
func TestAReadThatFailsOnADiskThatDoesNotConfirmLosesIt(t *testing.T) {
	b, _, sec := sectorBench(t, 1)
	sec.failed = func() { b.unread[b.dirs["t2"]] = true }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t2 := diskOf(out, "t2")
	if !out.Partial || t2 == nil || len(t2.Corrupt) != 0 || len(t2.Repaired) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "missing" {
		t.Errorf("the disk is %s", got)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
		t.Errorf("the copy is %s", got)
	}
	if got := b.count("SELECT count(*) FROM ops"); got != 0 {
		t.Errorf("%d operations", got)
	}
}

// A chunk that reads right the second time was a bad read, of a cable or a controller: the copy is verified.
func TestAReadThatFailsOnceIsNoCorruption(t *testing.T) {
	b, _, sec := sectorBench(t, 1)
	sec.once = true
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t2 := diskOf(out, "t2")
	if out.Partial || t2 == nil || len(t2.Corrupt) != 0 || t2.Read != 1 {
		t.Fatalf("%+v", out)
	}
	if sec.fails != 1 {
		t.Errorf("%d reads failed", sec.fails)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2' AND verified_ns = ?", b.now); got != "ok" {
		t.Errorf("the copy is %q", got)
	}
}

// A copy never verified with a chunk that cannot be read is a conflict, as one that does not match: nothing says it
// ever held its file's content, and a repair would write over whatever else it holds (specs.md §16).
func TestAChunkThatCannotBeReadOfACopyNeverVerified(t *testing.T) {
	b, _, sec := sectorBench(t, 1)
	b.exec("UPDATE copies SET verified_ns = NULL WHERE disk_id = 't2'")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t2 := diskOf(out, "t2")
	if t2 == nil || !slices.Equal(t2.Conflict, []string{"a.mkv"}) || len(t2.Corrupt) != 0 {
		t.Fatalf("%+v", out)
	}
	if sec.left("a.mkv") != 1 || b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("something was written to a copy that may be another version")
	}
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
}

// A drive that cannot give the sector another place: what is written reads back no better. The repair is not done,
// and the copy goes to the trash, which takes no read, for the next pass to make it again (specs.md §16).
func TestASectorThatAWriteDoesNotMend(t *testing.T) {
	b, _, sec := sectorBench(t, 1)
	sec.stuck = true
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t2 := diskOf(out, "t2")
	if out.Partial || t2 == nil || !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t2.Quarantined, []string{"a.mkv"}) || len(t2.Repaired) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "online" {
		t.Errorf("the disk is %s", got)
	}
	if b.exists("t2", "media/a.mkv") {
		t.Error("the copy that could not be mended is still in media/")
	}
}

// atoll repair mends such a copy too, and its reprise knows a chunk it could save nothing of (specs.md §16.1).
func TestARepairOfAChunkThatCannotBeRead(t *testing.T) {
	t.Run("by hand", func(t *testing.T) {
		b, data, sec := sectorBench(t, 0, 2)
		// Found by a scrub that may write nothing.
		if out := b.scrub(ScrubOptions{Apply: true}); !slices.Equal(diskOf(out, "t2").Corrupt, []string{"a.mkv"}) {
			t.Fatalf("%+v", out)
		}
		done := b.repair(RepairOptions{Apply: true})
		if len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired || done.Items[0].Chunks != 2 {
			t.Fatalf("%+v", done)
		}
		if sec.left("a.mkv") != 0 || b.read("t2", "a.mkv") != string(data) {
			t.Error("not repaired")
		}
	})
	for _, point := range []string{"chunks-saved", "chunk-written"} {
		t.Run("stopped at "+point, func(t *testing.T) {
			b, data, sec := sectorBench(t, 1)
			b.scrub(ScrubOptions{Apply: true})
			stop := errors.New("stopped")
			b.at[point] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop at %s: %v", point, err)
					}
				}()
				_, _ = Repair(t.Context(), b.db, b.config, b, RepairOptions{Apply: true})
			}()
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Fatalf("the repair is %s", got)
			}
			// The next command that writes resolves it, and a repair then finishes what is left.
			b.pass()
			if got := b.text("SELECT group_concat(state, ' ') FROM ops WHERE kind = 'repair'"); got != "done" {
				t.Errorf("the repair a crash left is %s", got)
			}
			if b.text("SELECT state FROM copies WHERE disk_id = 't2'") == "corrupt" {
				if done := b.repair(RepairOptions{Apply: true}); len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired {
					t.Fatalf("the next repair: %+v", done)
				}
			}
			if sec.left("a.mkv") != 0 || b.read("t2", "a.mkv") != string(data) {
				t.Error("not repaired")
			}
			if got := b.text("SELECT state FROM files"); got != "ok" {
				t.Errorf("the file is %s", got)
			}
		})
	}
}
