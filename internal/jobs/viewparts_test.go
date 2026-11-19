package jobs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/view/writer"
)

// parallelPool gives a pool of two disks in immediate mode with a replica each, its clock the real one, t1 with the
// most room, and a view over it.
func parallelPool(t *testing.T, more string) (*bench, *writer.Writer) {
	t.Helper()
	b := newBenchWith(t, "[defaults]\nmode = \"immediate\"\nreplicas = 1\n"+more, "t1", "t2")
	b.now = time.Now().UnixNano()
	if b.spaces == nil {
		b.spaces = map[string][2]int64{}
	}
	b.spaces[b.dirs["t1"]] = [2]int64{1 << 30, 1 << 29}
	b.spaces[b.dirs["t2"]] = [2]int64{1 << 30, 1 << 28}
	b.scan(false)
	return b, b.viewOf()
}

// writeThroughView makes a file through the view, writes it, closes it, and waits until its parts are handed over.
func writeThroughView(b *bench, w *writer.Writer, path, text string) {
	b.t.Helper()
	f, _, err := w.Create(b.t.Context(), path, 0o644, viewer)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(text), 0); err != nil {
		b.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.t.Fatal(err)
	}
	w.Wait()
}

// partInode gives the inode of the one part a disk's tmp/ holds, 0 with none or more.
func (b *bench) partInode(id string) uint64 {
	b.t.Helper()
	names := b.parts(id)
	if len(names) != 1 {
		return 0
	}
	return b.inodeAt(filepath.Join(b.dirs[id], "Atoll", "tmp", names[0]))
}

// A file written in parallel through the view becomes a replica at the next pass, by the copy pipeline from its part:
// the job of its replica adopts the part the view wrote, compares it with the original and writes nothing again, then
// verifies it, renames it into place, and records it (specs.md §12.3, §14). The replica is the part itself, by its
// inode, and nothing is left in tmp/ nor in the journal.
func TestAFileWrittenInParallelBecomesAReplica(t *testing.T) {
	b, w := parallelPool(t, "")
	writeThroughView(b, w, "new.mkv", "a new film")
	part := b.partInode("t2")
	if part == 0 {
		t.Fatalf("tmp/ of t2 holds %v", b.parts("t2"))
	}
	writes := 0
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "write" && strings.HasSuffix(name, ".part") {
			writes++
		}
		return nil
	}
	out := b.pass()
	if writes != 0 {
		t.Errorf("%d writes to the part: its bytes were written again", writes)
	}
	if got := outcomeOf(out, KindReplicate, "new.mkv"); got != OutcomeReplicated {
		t.Fatalf("the copy: %q, operations %+v", got, out.Operations)
	}
	if b.read("t2", "new.mkv") != "a new film" || b.inode("t2", "new.mkv") != part {
		t.Errorf("the replica is not the part: inode %d, part %d", b.inode("t2", "new.mkv"), part)
	}
	if got := b.holders("new.mkv"); got != "t1:user:ok t2:atoll:ok" {
		t.Errorf("copies %s", got)
	}
	if got := b.parts("t2"); len(got) != 0 {
		t.Errorf("tmp/ of t2 holds %v", got)
	}
	if got := b.text("SELECT group_concat(state, ' ') FROM (SELECT state FROM ops WHERE kind = 'create' AND disk_id = 't2' ORDER BY id)"); got != "undone done" {
		t.Errorf("the creates of t2: %s, want the view's undone as the job's took its part, then the job's done", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		b.pass()
		if open = b.openViewOps(); len(open) != 0 {
			t.Errorf("not taken in: %+v", open)
		}
	}
}

// A pass stopped at each step of the copy from a part, as kill -9 stops it, is resumed or undone by the reprise: the
// part a pending job owns is kept, and the next pass makes the replica (specs.md §14.2, §17.2).
func TestACopyFromAPartSurvivesACrashAtEachStep(t *testing.T) {
	for _, point := range []string{"journaled", "copied", "verified", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b, w := parallelPool(t, "")
			writeThroughView(b, w, "new.mkv", "a new film")
			b.crashAt(point)
			for range 3 {
				b.pass()
				b.now += 60_000_000_000
			}
			if b.read("t2", "new.mkv") != "a new film" {
				t.Errorf("the replica holds %q", b.read("t2", "new.mkv"))
			}
			if got := b.holders("new.mkv"); got != "t1:user:ok t2:atoll:ok" {
				t.Errorf("copies %s", got)
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("tmp/ of t2 holds %v", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// A file changed after the program closed it, before the pass: the job compares the part with the original as it is
// then, and writes what differs, so that the replica holds the original's bytes (specs.md §14.2).
func TestAPartOfAFileChangedSinceIsCopiedWhereItDiffers(t *testing.T) {
	b, w := parallelPool(t, "")
	writeThroughView(b, w, "new.mkv", "a new film")
	b.write("t1", "new.mkv", "A NEW film, longer")
	b.pass()
	b.now += 60_000_000_000
	b.pass()
	if b.read("t2", "new.mkv") != "A NEW film, longer" {
		t.Errorf("the replica holds %q", b.read("t2", "new.mkv"))
	}
}

// A part no job takes goes, its operation undone and the view's row taken in: the plan placed no copy there, as with
// replication off or no replica wanted any more, or its file left its path (specs.md §12.3).
func TestAPartNoJobTakesIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(b *bench)
	}{
		{"replication off", func(b *bench) {
			b.reconfigure("[defaults]\nmode = \"immediate\"\nreplicas = 1\n[replication]\nenabled = false\n")
		}},
		{"no replica wanted", func(b *bench) { b.reconfigure("[defaults]\nmode = \"immediate\"\nreplicas = 0\n") }},
		{"renamed on its disk", func(b *bench) { b.rename("t1", "new.mkv", "other.mkv") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, w := parallelPool(t, "")
			writeThroughView(b, w, "new.mkv", "a new film")
			tc.do(b)
			for range 3 {
				b.pass()
				b.now += 60_000_000_000
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("tmp/ of t2 holds %v", got)
			}
			if got := b.count("SELECT count(*) FROM view_ops WHERE kind = 'part' AND taken_ns IS NULL"); got != 0 {
				t.Errorf("%d parts not taken in", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}
