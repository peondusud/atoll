package jobs

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/trash"
)

// A corrupt copy that is its file's first, on the first disk, is mended as any other: by the scrub with --yes that
// finds it, and by the next one with --yes when a scrub without it found it (specs.md §16, §16.1).
func TestACorruptFirstCopyIsMended(t *testing.T) {
	for _, m := range mendings {
		for _, again := range []bool{false, true} {
			b := newBenchWith(t, m.config, "t1", "t2")
			b.write("t1", "a.mkv", healthyFilm)
			b.write("t2", "a.mkv", healthyFilm)
			b.scan(true)
			b.now += int64(month)
			b.rot("t1", "a.mkv", rottenFilm)
			if again {
				t1 := diskOf(b.scrub(ScrubOptions{Apply: true}), "t1")
				if !slices.Equal(t1.Corrupt, []string{"a.mkv"}) || b.read("t1", "a.mkv") != rottenFilm {
					t.Fatalf("%s, without --yes: %+v", m.name, *t1)
				}
			}
			t1 := diskOf(b.scrub(ScrubOptions{Apply: true, Quarantine: true}), "t1")
			switch m.name {
			case "repaired":
				if !slices.Equal(t1.Repaired, []string{"a.mkv"}) || b.read("t1", "a.mkv") != healthyFilm {
					t.Errorf("%s, taken again %v: %+v, the copy holds %q", m.name, again, *t1, b.read("t1", "a.mkv"))
				}
			default:
				if !slices.Equal(t1.Quarantined, []string{"a.mkv"}) || b.exists("t1", "media/a.mkv") {
					t.Errorf("%s, taken again %v: %+v", m.name, again, *t1)
				}
			}
			if got := b.text("SELECT state FROM files"); got == "corrupt" {
				t.Errorf("%s, taken again %v: the file is still corrupt", m.name, again)
			}
		}
	}
}

// A scrub never reads a disk the configuration marks failed, and reads the others (specs.md §16, §18).
func TestAScrubLeavesAFailedDisk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a.mkv", healthyFilm)
	b.write("t2", "a.mkv", healthyFilm)
	b.scan(true)
	b.now += int64(month)
	b.config.Disks[0].State = config.DiskFailed
	out := b.scrub(ScrubOptions{Apply: true})
	if t1, t2 := diskOf(out, "t1"), diskOf(out, "t2"); t1 != nil || t2 == nil || t2.Read != 1 {
		t.Errorf("%+v", out.Disks)
	}
}

// A scrub whose budget is spent to the nanosecond reads no more: it stops at the chunk it reached, as one past it does
// (specs.md §16).
func TestAScrubStopsAtItsBudgetToTheNanosecond(t *testing.T) {
	for _, steps := range []int64{1, 2, 3} {
		b := newBench(t, "t1")
		b.write("t1", "big.mkv", strings.Repeat("x", 2*integrity.ChunkSize+5))
		b.scan(true)
		b.now += int64(month)
		clock := &ticking{bench: b, step: int64(time.Second)}
		out, err := Scrub(t.Context(), b.db, b.config, clock, ScrubOptions{Apply: true, Budget: time.Duration(steps) * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		// The clock moves a second at each look at it: the first after the budget's start finds a second spent.
		if d := diskOf(out, "t1"); !d.Left || d.Read != 0 || d.Bytes != (steps-1)*integrity.ChunkSize {
			t.Errorf("a budget of %d s: %+v", steps, *d)
		}
	}
}

// A file of exactly one chunk has one chunk to read and to repair, and an empty file none: a scrub verifies both, and
// finds the one chunk of the first rotten when it is; atoll repair mends it (specs.md §16, §16.1).
func TestAFileOfOneChunkExactlyAndAnEmptyFile(t *testing.T) {
	b := newBench(t, "t1", "t2")
	data := film()[:integrity.ChunkSize]
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a.mkv", string(data))
		b.write(id, "e", "")
	}
	b.scan(true)
	b.now += int64(month)
	out := b.scrub(ScrubOptions{Apply: true})
	if t1 := diskOf(out, "t1"); t1.Read != 2 || len(t1.Corrupt) != 0 || out.Partial {
		t.Fatalf("all as their rows say: %+v", out)
	}
	b.now += int64(month)
	b.rotChunks("t2", "a.mkv", data, 0)
	if t2 := diskOf(b.scrub(ScrubOptions{Apply: true}), "t2"); !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t2.Repaired, []string{"a.mkv"}) {
		t.Fatalf("the rotten chunk: %+v", *t2)
	}
	done := b.repair(RepairOptions{Apply: true})
	if len(done.Items) != 1 || done.Items[0].Outcome != trash.OutcomeRepaired || done.Items[0].Chunks != 1 {
		t.Fatalf("%+v", done)
	}
	if b.read("t2", "a.mkv") != string(data) {
		t.Error("not repaired")
	}
}
