package jobs

import (
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// The ways a corrupt copy is mended, by integrity.auto_repair: in place by chunk, or sent to the trash and made again
// (specs.md §16, §16.1).
var mendings = []struct{ name, config string }{
	{"repaired", ""},
	{"quarantined", "[integrity]\nauto_repair = false\n"},
}

// rottenOnT2 gives a film on two disks, read whole, due for a scrub, and rotten on t2.
func rottenOnT2(t *testing.T, config string) *bench {
	t.Helper()
	b := newBenchWith(t, config, "t1", "t2")
	b.write("t1", "a.mkv", healthyFilm)
	b.write("t2", "a.mkv", healthyFilm)
	b.scan(true)
	b.now += int64(month)
	b.rot("t2", "a.mkv", rottenFilm)
	return b
}

// leftCorrupt checks that the copy on t2 is still corrupt in the root, the file corrupt, as its alert says, and the
// healthy copy as it was.
func (b *bench) leftCorrupt(healthy uint64) {
	b.t.Helper()
	if got := b.read("t2", "a.mkv"); got != rottenFilm {
		b.t.Fatalf("the rotten copy holds %q", got)
	}
	if got := b.text("SELECT c.state || ' ' || f.state FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2'"); got != "corrupt corrupt" {
		b.t.Fatalf("the copy and its file are %s", got)
	}
	b.healthyKept(healthy)
}

// healthyKept checks that the healthy copy was only ever read: the same file, with the same bytes, ok and verified.
func (b *bench) healthyKept(healthy uint64) {
	b.t.Helper()
	if got := b.read("t1", "a.mkv"); got != healthyFilm || b.inode("t1", "a.mkv") != healthy {
		b.t.Errorf("the healthy copy holds %q, inode %d for %d", got, b.inode("t1", "a.mkv"), healthy)
	}
	if got := b.text("SELECT state || ' ' || (verified_ns IS NOT NULL) FROM copies WHERE disk_id = 't1'"); got != "ok 1" {
		b.t.Errorf("the healthy copy is %s", got)
	}
}

// mendedAgain checks what the scrub that took the copy again did, as with a copy it finds corrupt (specs.md §16): no
// corrupt event of its own, as nothing new was found; repaired in place, or in the trash, and made again by the next
// pass; the file no longer corrupt, so that its alert ends.
func (b *bench) mendedAgain(how string, out *ScrubOutcome, healthy uint64) {
	b.t.Helper()
	t2 := diskOf(out, "t2")
	switch how {
	case "repaired":
		if !slices.Equal(t2.Repaired, []string{"a.mkv"}) || len(t2.Quarantined)+len(t2.Kept) != 0 {
			b.t.Errorf("t2: %+v", *t2)
		}
		if got := b.read("t2", "a.mkv"); got != healthyFilm {
			b.t.Errorf("the copy repaired holds %q", got)
		}
		if got := b.text("SELECT c.state || ' ' || (c.verified_ns IS NOT NULL) || ' ' || f.state FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2'"); got != "ok 1 ok" {
			b.t.Errorf("once repaired, the copy, verified, and its file: %s", got)
		}
	default:
		if !slices.Equal(t2.Quarantined, []string{"a.mkv"}) || len(t2.Repaired)+len(t2.Kept) != 0 {
			b.t.Errorf("t2: %+v", *t2)
		}
		if b.exists("t2", "media/a.mkv") {
			b.t.Error("the corrupt copy is still in the root")
		}
		if got := b.text("SELECT COALESCE(group_concat(reason), 'nothing') FROM trash t JOIN ops o ON o.id = t.op_id WHERE t.disk_id = 't2' AND o.state = 'done' AND o.kind = 'trash'"); got != "quarantined" {
			b.t.Errorf("in t2's trash: %s", got)
		}
		if !b.holds("t2", rottenFilm) {
			b.t.Error("the rotten copy's bytes are lost")
		}
		if got := b.text("SELECT state FROM files"); got != "pending" {
			b.t.Errorf("once its corrupt copy went, the file is %s", got)
		}
	}
	if len(t2.Corrupt) != 0 {
		b.t.Errorf("the scrub that took it again found it corrupt anew: %+v", *t2)
	}
	if n := b.count("SELECT count(*) FROM events WHERE kind = 'corrupt'"); n != 1 {
		b.t.Errorf("%d corrupt events", n)
	}
	b.healthyKept(healthy)
	b.settle()
	if got := b.read("t2", "a.mkv"); got != healthyFilm {
		b.t.Errorf("after a pass the copy on t2 holds %q", got)
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		b.t.Errorf("after a pass the file is %s", got)
	}
	// Nothing is left to take again.
	if t2 := diskOf(b.scrub(ScrubOptions{Apply: true, Quarantine: true}), "t2"); len(t2.Repaired)+len(t2.Quarantined)+len(t2.Kept)+len(t2.Corrupt) != 0 {
		b.t.Errorf("a scrub after that: %+v", *t2)
	}
}

// A corrupt copy that a scrub could neither repair nor send to the trash, its disk failing at that moment, stays in the
// root, where mergerfs may present it (invariant 14). Once its disk is back, the next scrub takes it before the copies
// due, as it would a copy it finds corrupt: the healthy copy read whole first, then repaired from it, or moved to the
// trash (specs.md §16). It stayed corrupt there for good: no scrub reads a copy that is not ok, and no pass moves a
// corrupt one, so that only atoll repair, run by hand, ever ended it.
func TestACorruptCopyADiskFailureKeptGoesAtTheNextScrub(t *testing.T) {
	for _, m := range mendings {
		t.Run(m.name, func(t *testing.T) {
			b := rottenOnT2(t, m.config)
			healthy := b.inode("t1", "a.mkv")
			// Once the scrub has read the copy again past the cache, the disk dies at the first write: every call fails.
			read, dead := false, false
			b.fail[b.dirs["t2"]] = func(call, _ string) error {
				switch {
				case dead:
					return syscall.EIO
				case call == "dropcache":
					read = true
				case read && (call == "mkdir" || call == "create" || call == "openwrite"):
					dead = true
					return syscall.EIO
				}
				return nil
			}
			out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
			if t2 := diskOf(out, "t2"); !dead || !out.Partial || !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
				t.Fatalf("the disk failing: %+v, %+v", out, *t2)
			}
			b.leftCorrupt(healthy)
			// The disk is back.
			delete(b.fail, b.dirs["t2"])
			out = b.scrub(ScrubOptions{Apply: true, Quarantine: true})
			if out.Partial {
				t.Errorf("the disk back: %+v", out)
			}
			b.mendedAgain(m.name, out, healthy)
		})
	}
}

// A crash between the scrub that found a copy corrupt and its move to the trash, or its repair, leaves the copy in the
// root: the reprise undoes what was journaled, and the next scrub takes the copy again (specs.md §16, §17.2).
func TestACorruptCopyACrashKeptGoesAtTheNextScrub(t *testing.T) {
	for _, m := range mendings {
		t.Run(m.name, func(t *testing.T) {
			b := rottenOnT2(t, m.config)
			healthy := b.inode("t1", "a.mkv")
			stop := errors.New("stopped")
			b.at["journaled"] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop once journaled: %v", err)
					}
				}()
				_, _ = Scrub(t.Context(), b.db, b.config, b, ScrubOptions{Apply: true, Quarantine: true})
			}()
			b.leftCorrupt(healthy)
			b.mendedAgain(m.name, b.scrub(ScrubOptions{Apply: true, Quarantine: true}), healthy)
		})
	}
}

// A scrub not told --yes records a copy it finds corrupt, and says what --yes would do with it; a scrub told --yes then
// does it (specs.md §16, §25). It did nothing: the copy, corrupt already, was no longer due, and stayed in the root.
func TestACorruptCopyFoundWithoutYesGoesWithIt(t *testing.T) {
	for _, m := range mendings {
		t.Run(m.name, func(t *testing.T) {
			b := rottenOnT2(t, m.config)
			healthy := b.inode("t1", "a.mkv")
			out := b.scrub(ScrubOptions{Apply: true})
			t2 := diskOf(out, "t2")
			if said := append(slices.Clone(t2.Repaired), t2.Quarantined...); !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(said, []string{"a.mkv"}) {
				t.Fatalf("without --yes: %+v", *t2)
			}
			b.leftCorrupt(healthy)
			// Asked again without --yes, it says the same, and still writes nothing.
			t2 = diskOf(b.scrub(ScrubOptions{Apply: true}), "t2")
			if said := append(slices.Clone(t2.Repaired), t2.Quarantined...); len(t2.Corrupt) != 0 || !slices.Equal(said, []string{"a.mkv"}) {
				t.Errorf("again without --yes: %+v", *t2)
			}
			b.leftCorrupt(healthy)
			b.mendedAgain(m.name, b.scrub(ScrubOptions{Apply: true, Quarantine: true}), healthy)
		})
	}
}

// A corrupt copy with nothing to mend it from stays as it is, marked and alerted, and the scrubs after the one that
// found it say nothing more of it (specs.md §16): a file whose only other copy is corrupt too, or on a disk away, and a
// corrupt copy on a disk that may only be read, which nothing writes to.
func TestACorruptCopyWithNothingToMendItFromIsLeftAsItIs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(b *bench)
	}{
		{"no healthy copy", func(b *bench) {
			b.exec("UPDATE copies SET state = 'corrupt' WHERE disk_id = 't1'")
		}},
		{"its disk away", func(b *bench) { b.gone[b.dirs["t1"]] = true }},
		{"on a readonly disk", func(b *bench) { b.ro[b.dirs["t2"]] = true }},
	} {
		for _, m := range mendings {
			t.Run(tc.name+", "+m.name, func(t *testing.T) {
				b := rottenOnT2(t, m.config)
				b.scrub(ScrubOptions{Apply: true})
				tc.after(b)
				b.now += int64(month)
				out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
				t2 := diskOf(out, "t2")
				if len(t2.Corrupt)+len(t2.Repaired)+len(t2.Quarantined)+len(t2.Kept) != 0 {
					t.Errorf("%+v", *t2)
				}
				if got := b.read("t2", "a.mkv"); got != rottenFilm {
					t.Errorf("the corrupt copy holds %q", got)
				}
				if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "corrupt" {
					t.Errorf("the copy is %s", got)
				}
				if n := b.count("SELECT count(*) FROM ops"); n != 0 {
					t.Errorf("%d operations journaled", n)
				}
			})
		}
	}
}

// A corrupt copy taken again that still cannot go, its healthy copy rotten too though its row says ok, is kept where it
// is. The daemon takes each such copy once a stretch of integrity.window, leaving out those an earlier slice took:
// one taken at each slice of 30 s would read its other copy whole at each (specs.md §15.13, §16). Left out, and with
// nothing due, its disk is not opened.
func TestACorruptCopyTakenAgainIsLeftOutOfTheStretch(t *testing.T) {
	b := rottenOnT2(t, "")
	b.scrub(ScrubOptions{Apply: true})
	b.rot("t1", "a.mkv", "A film, of one short chunk")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t1, t2 := diskOf(out, "t1"), diskOf(out, "t2")
	if !slices.Equal(t2.Again, []string{"a.mkv"}) || !slices.Equal(t2.Kept, []string{"a.mkv"}) || len(t2.Corrupt)+len(t2.Repaired)+len(t2.Quarantined) != 0 || t1.Read != 0 {
		t.Fatalf("taken again: %+v, %+v", *t1, *t2)
	}
	id := int64(b.count("SELECT id FROM files"))
	if !slices.Equal(out.Taken, []catalog.CorruptCopy{{FileID: id, Disk: "t2"}}) {
		t.Fatalf("taken: %+v", out.Taken)
	}
	clear(b.pastCache)
	taken := map[catalog.CorruptCopy]bool{out.Taken[0]: true}
	out = b.scrub(ScrubOptions{Apply: true, Quarantine: true, Taken: taken})
	if t2 := diskOf(out, "t2"); len(t2.Again)+len(t2.Kept) != 0 || len(out.Taken) != 0 || !t2.Scrubbed {
		t.Errorf("left out: %+v, %+v", out.Taken, *t2)
	}
	if len(b.pastCache) != 0 {
		t.Errorf("a disk was opened for nothing: %v", b.pastCache)
	}
	// The next stretch takes it again.
	if t2 := diskOf(b.scrub(ScrubOptions{Apply: true, Quarantine: true}), "t2"); !slices.Equal(t2.Again, []string{"a.mkv"}) {
		t.Errorf("the next stretch: %+v", *t2)
	}
	if got := b.read("t2", "a.mkv"); got != rottenFilm {
		t.Errorf("the corrupt copy holds %q", got)
	}
}

// The budget counts the corrupt copies taken again as it counts the copies due: one that runs out leaves the others to
// the next scrub, which takes them first (specs.md §16).
func TestTheBudgetCountsTheCorruptCopiesTakenAgain(t *testing.T) {
	b := newBenchWith(t, "[integrity]\nauto_repair = false\n", "t1", "t2")
	for _, name := range []string{"a.mkv", "b.mkv"} {
		b.write("t1", name, healthyFilm)
		b.write("t2", name, healthyFilm)
	}
	b.scan(true)
	b.now += int64(month)
	b.rot("t2", "a.mkv", rottenFilm)
	b.rot("t2", "b.mkv", rottenFilm)
	b.scrub(ScrubOptions{Apply: true})
	// The first move to the trash takes an hour.
	b.at["journaled"] = func() { b.now += int64(time.Hour) }
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true, Budget: time.Minute})
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Again, []string{"a.mkv"}) || !slices.Equal(t2.Quarantined, []string{"a.mkv"}) || !t2.Left {
		t.Fatalf("the budget spent: %+v", *t2)
	}
	if !b.exists("t2", "media/b.mkv") {
		t.Fatal("b.mkv went too")
	}
	if t2 := diskOf(b.scrub(ScrubOptions{Apply: true, Quarantine: true}), "t2"); !slices.Equal(t2.Again, []string{"b.mkv"}) || !slices.Equal(t2.Quarantined, []string{"b.mkv"}) || t2.Left {
		t.Errorf("the next scrub: %+v", *t2)
	}
	if b.exists("t2", "media/b.mkv") {
		t.Error("b.mkv is still in the root")
	}
}
