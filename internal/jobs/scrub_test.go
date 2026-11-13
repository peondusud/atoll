package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/integrity"
)

const month = 31 * 24 * time.Hour

// rot changes a copy's content as a disk that rots would: same size, and the catalog none the wiser, as its row
// takes the times the rewrite gave (a userspace write cannot keep the change time).
func (b *bench) rot(id, path, text string) {
	b.t.Helper()
	b.write(id, path, text)
	info, err := os.Stat(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		b.t.Fatal(err)
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	b.exec("UPDATE copies SET mtime_ns = ?, ctime_ns = ? WHERE disk_id = ? AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = ?)",
		info.ModTime().UnixNano(), st.Ctim.Nano(), id, path)
}

func (b *bench) scrub(opts ScrubOptions) *ScrubOutcome {
	b.t.Helper()
	out, err := Scrub(b.t.Context(), b.db, b.config, b, opts)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

func diskOf(out *ScrubOutcome, id string) *ScrubDisk {
	for i := range out.Disks {
		if out.Disks[i].ID == id {
			return &out.Disks[i]
		}
	}
	return nil
}

// A copy never verified that does not hold its file's content is another version of the file, not a corrupt copy:
// nothing says it ever held that content. The file becomes a conflict, both versions kept, and nothing is written to
// either (specs.md §15.7, §16). The scrub called it corrupt: with auto_repair it wrote the first version over the
// second, and without it sent the second to the trash.
func TestScrubCallsACopyNeverVerifiedAConflict(t *testing.T) {
	for _, config := range []string{"", "[integrity]\nauto_repair = false\n"} {
		b := newBenchWith(t, config, "t1", "t2")
		b.write("t1", "a.mkv", "a film")
		b.scan(true)
		// Another version, of the same size, comes with the second disk, as when a disk is added: recorded, not read.
		b.write("t2", "a.mkv", "A FILM")
		b.scan(false)
		if n := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok' AND verified_ns IS NULL"); n != 1 {
			t.Fatalf("%q: %d copies of t2 recorded and never verified", config, n)
		}
		out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
		t2 := diskOf(out, "t2")
		if len(t2.Corrupt) != 0 || len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 || len(t2.Kept) != 0 || !slices.Equal(t2.Conflict, []string{"a.mkv"}) {
			t.Errorf("%q: %+v", config, *t2)
		}
		if got := b.read("t2", "a.mkv"); got != "A FILM" {
			t.Errorf("%q: the second version now holds %q", config, got)
		}
		if got := b.read("t1", "a.mkv"); got != "a film" {
			t.Errorf("%q: the first version now holds %q", config, got)
		}
		if state := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv'"); state != "conflict" {
			t.Errorf("%q: the file is %s", config, state)
		}
		if n := b.count("SELECT count(*) FROM trash"); n != 0 {
			t.Errorf("%q: %d entries in the trash", config, n)
		}
		// While the conflict stands, no scrub reads its copies again: nothing tells them apart.
		out = b.scrub(ScrubOptions{Apply: true, Quarantine: true})
		if t1, t2 := diskOf(out, "t1"), diskOf(out, "t2"); t1.Read != 0 || t2.Read != 0 || len(t2.Conflict) != 0 || len(t2.Corrupt) != 0 {
			t.Errorf("%q, a second scrub: %+v, %+v", config, *t1, *t2)
		}
	}
}

// A disk that is readonly is read, and nothing is written to it (specs.md §10): a corrupt copy found there stays
// where it is, with a healthy copy elsewhere or not. The scrub repaired it in place, or moved it to that disk's
// trash, making its work folder on the way.
func TestScrubWritesNothingToAReadonlyDisk(t *testing.T) {
	for _, config := range []string{"", "[integrity]\nauto_repair = false\n"} {
		for _, yes := range []bool{true, false} {
			b := newBenchWith(t, config, "t1", "t2")
			b.write("t1", "a.mkv", "a film")
			b.write("t2", "a.mkv", "a film")
			b.scan(true)
			b.now += int64(month)
			b.rot("t2", "a.mkv", "a FILM")
			b.ro[b.dirs["t2"]] = true
			out := b.scrub(ScrubOptions{Apply: true, Quarantine: yes})
			t2 := diskOf(out, "t2")
			if !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || len(t2.Repaired) != 0 || len(t2.Quarantined) != 0 || !slices.Equal(t2.Kept, []string{"a.mkv"}) {
				t.Errorf("%q, --yes %v: %+v", config, yes, *t2)
			}
			if got := b.read("t2", "a.mkv"); got != "a FILM" {
				t.Errorf("%q, --yes %v: the copy on the readonly disk now holds %q", config, yes, got)
			}
			if b.exists("t2", "Atoll") {
				t.Errorf("%q, --yes %v: a work folder was made on the readonly disk", config, yes)
			}
		}
	}
}

// A scrub that was not told --yes writes to no disk, and so resolves nothing of what a crash left (specs.md §17.2):
// an operation stays planned, for a run that may write. It did resolve them, as its session was opened to write so
// that its disks were confirmed.
func TestAScrubWithoutYesResolvesNothing(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a.mkv", "a film")
	b.write("t2", "a.mkv", "a film")
	b.scan(true)
	// A move to the trash journaled, and stopped before it was made.
	b.exec("INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state) VALUES (900, 't2', 'trash', CAST('a.mkv' AS BLOB), CAST('2026-01-01/900/a.mkv' AS BLOB), ?, 1, 'planned')",
		strconv.FormatUint(b.inode("t2", "a.mkv"), 10))
	b.exec("INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, reason, deleted_ns, expires_ns) VALUES (CAST('a.mkv' AS BLOB), 't2', CAST('2026-01-01/900/a.mkv' AS BLOB), 900, 6, 'surplus', 1, 2)")
	state := func() string { return b.text("SELECT state FROM ops WHERE id = 900") }
	b.scrub(ScrubOptions{Apply: true})
	if got := state(); got != "planned" {
		t.Errorf("after a scrub without --yes the operation is %s", got)
	}
	if b.exists("t2", "Atoll") {
		t.Error("a scrub without --yes made a work folder")
	}
	// With --yes it may write, and the reprise comes first.
	b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if got := state(); got == "planned" {
		t.Error("after a scrub with --yes the operation is still planned")
	}
}

// A copy that rotted, read again past the cache, is corrupt: with a healthy copy elsewhere it goes to the trash,
// and the next pass makes it again from the healthy one (specs.md §16).
func TestScrubQuarantines(t *testing.T) {
	// Without auto_repair, a corrupt copy goes to the trash (specs.md §16).
	b := newBenchWith(t, "[integrity]\nauto_repair = false\n", "t1", "t2")
	b.write("t1", "a.mkv", "a film")
	b.write("t2", "a.mkv", "a film")
	b.write("t1", "b.mkv", "fine")
	b.write("t2", "b.mkv", "fine")
	b.scan(true)
	// Not due yet: verified a minute ago.
	if out := b.scrub(ScrubOptions{Apply: true, Quarantine: true}); diskOf(out, "t1").Read != 0 || diskOf(out, "t2").Read != 0 {
		t.Errorf("read before due: %+v", out)
	}
	b.now += int64(month)
	b.rot("t2", "a.mkv", "a FILM")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	t1, t2 := diskOf(out, "t1"), diskOf(out, "t2")
	if t1.Read != 2 || t2.Read != 1 || !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t2.Quarantined, []string{"a.mkv"}) || len(t2.Kept) != 0 || out.Partial {
		t.Fatalf("%+v", out)
	}
	if n := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2' AND CAST(f.lpath AS TEXT) = 'a.mkv'"); n != 0 {
		t.Errorf("the corrupt copy is still recorded")
	}
	if b.exists("t2", "media/a.mkv") {
		t.Error("the corrupt copy is still in media/")
	}
	if got := b.text("SELECT reason FROM trash WHERE disk_id = 't2'"); got != "quarantined" {
		t.Errorf("trash reason %q", got)
	}
	if n := b.count("SELECT count(*) FROM events WHERE kind = 'corrupt' AND json_extract(data, '$.disk') = 't2' AND json_extract(data, '$.path') = 'a.mkv'"); n != 1 {
		t.Errorf("%d corrupt events", n)
	}
	// Corrupt while a copy is, and no longer: once the copy went, the file takes the state its copies give it.
	if got := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv'"); got != "pending" {
		t.Errorf("the file is %s once its corrupt copy went", got)
	}
	b.settle()
	data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", "a.mkv"))
	if err != nil || string(data) != "a film" {
		t.Errorf("made again: %q, %v", data, err)
	}
	if got := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = 'a.mkv'"); got != "ok" {
		t.Errorf("the file is %s once its copy is made again", got)
	}
}

// With no healthy copy, a corrupt copy stays where it is, and so does every other: nothing is lost to a scrub
// (specs.md §16).
func TestScrubKeepsTheLastCopy(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a.mkv", "a film")
	b.write("t2", "a.mkv", "a film")
	b.scan(true)
	b.now += int64(month)
	b.rot("t1", "a.mkv", "A film")
	b.rot("t2", "a.mkv", "a filM")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	for _, id := range []string{"t1", "t2"} {
		d := diskOf(out, id)
		if !slices.Equal(d.Corrupt, []string{"a.mkv"}) || !slices.Equal(d.Kept, []string{"a.mkv"}) || len(d.Quarantined) != 0 {
			t.Errorf("%s: %+v", id, d)
		}
		if !b.exists(id, "media/a.mkv") {
			t.Errorf("%s: the copy went", id)
		}
	}
	if got := b.text("SELECT state FROM files"); got != "corrupt" {
		t.Errorf("file %s", got)
	}
	if n := b.count("SELECT count(*) FROM trash"); n != 0 {
		t.Errorf("%d in the trash", n)
	}
	b.settle()
	if got := b.text("SELECT state FROM files"); got != "corrupt" {
		t.Errorf("file %s after a pass, its copies still corrupt", got)
	}
}

// A copy changed since its row is a modification, for the next pass, and is never called corrupt; without Apply
// nothing is written, and without Quarantine a corrupt copy is only said to be one to move (specs.md §16).
func TestScrubLeavesWhatChanged(t *testing.T) {
	// Without auto_repair, a corrupt copy goes to the trash (specs.md §16).
	b := newBenchWith(t, "[integrity]\nauto_repair = false\n", "t1", "t2")
	b.write("t1", "a.mkv", "a film")
	b.write("t2", "a.mkv", "a film")
	b.write("t1", "b.mkv", "b")
	b.write("t2", "b.mkv", "b")
	b.scan(true)
	b.now += int64(month)
	b.write("t1", "a.mkv", "a fiLm")
	b.rot("t2", "b.mkv", "B")
	before := b.text("SELECT group_concat(disk_id || ':' || state || ':' || COALESCE(verified_ns, 0), ',') FROM copies ORDER BY file_id, disk_id")
	out := b.scrub(ScrubOptions{})
	if t1 := diskOf(out, "t1"); !slices.Equal(t1.Changed, []string{"a.mkv"}) || len(t1.Corrupt) != 0 {
		t.Errorf("t1: %+v", t1)
	}
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Corrupt, []string{"b.mkv"}) || !slices.Equal(t2.Quarantined, []string{"b.mkv"}) {
		t.Errorf("t2: %+v", t2)
	}
	if after := b.text("SELECT group_concat(disk_id || ':' || state || ':' || COALESCE(verified_ns, 0), ',') FROM copies ORDER BY file_id, disk_id"); after != before {
		t.Errorf("a dry scrub wrote: %s, then %s", before, after)
	}
	out = b.scrub(ScrubOptions{Apply: true})
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Quarantined, []string{"b.mkv"}) || !b.exists("t2", "media/b.mkv") {
		t.Errorf("t2 without quarantine: %+v", t2)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2' AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'b.mkv')"); got != "corrupt" {
		t.Errorf("t2's copy of b.mkv: %s", got)
	}
}

// ticking is the bench with a clock that moves at each look, so that a budget runs out.
type ticking struct {
	*bench
	step int64
}

func (m *ticking) Now() int64 {
	m.now += m.step
	return m.now
}

// A scrub whose budget runs out in a copy says where it stopped, and the next one resumes there: the chunks before
// are not read again. A copy that changed meanwhile is read again from its start (specs.md §16).
func TestScrubResumes(t *testing.T) {
	b := newBench(t, "t1")
	big := bytes.Repeat([]byte("x"), 2*integrity.ChunkSize+5)
	b.write("t1", "big.mkv", string(big))
	b.scan(true)
	b.now += int64(month)
	clock := &ticking{bench: b, step: int64(time.Second)}
	out, err := Scrub(t.Context(), b.db, b.config, clock, ScrubOptions{Apply: true, Budget: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if d := diskOf(out, "t1"); !d.Left || d.Read != 0 || d.Bytes != integrity.ChunkSize {
		t.Fatalf("%+v", d)
	}
	if got := b.text("SELECT chunk || ' ' || file_id FROM scrub_cursor WHERE disk_id = 't1'"); !strings.HasPrefix(got, "1 ") {
		t.Errorf("cursor %q", got)
	}
	out = b.scrub(ScrubOptions{Apply: true})
	if d := diskOf(out, "t1"); d.Left || d.Read != 1 || d.Bytes != integrity.ChunkSize+5 {
		t.Errorf("resumed: %+v", d)
	}
	if n := b.count("SELECT count(*) FROM scrub_cursor"); n != 0 {
		t.Errorf("%d cursors left", n)
	}
	if n := b.count("SELECT count(*) FROM copies WHERE verified_ns = ?", b.now); n != 1 {
		t.Errorf("not verified now")
	}
}

// --path keeps the files at that path and below it, by whole components.
func TestScrubUnder(t *testing.T) {
	b := newBench(t, "t1")
	for _, p := range []string{"Films/a.mkv", "Films2/b.mkv", "Films", "c.mkv"} {
		if p == "Films" {
			continue
		}
		b.write("t1", p, "x")
	}
	b.scan(true)
	b.now += int64(month)
	if d := diskOf(b.scrub(ScrubOptions{Apply: true, Under: "Films"}), "t1"); d.Read != 1 {
		t.Errorf("under Films: %+v", d)
	}
	if d := diskOf(b.scrub(ScrubOptions{Apply: true}), "t1"); d.Read != 2 {
		t.Errorf("the rest: %+v", d)
	}
	if !underPath("a/b", "a") || underPath("ab", "a") || !underPath("a", "a") || !underPath("x", "") {
		t.Error("underPath")
	}
}
