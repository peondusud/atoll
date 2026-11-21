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

// A scrub that finds a copy corrupt, and another never verified that holds another version, finds a file corrupt
// whose copies also differ: it shows corrupt, and holds the conflict, which is the file's state once no copy of it is
// corrupt, the corrupt copy repaired, sent to the trash, or kept until atoll repair (specs.md §4, §16). The conflict
// was lost: the file was ok once the corrupt copy was repaired, until a scrub read the copy never verified again.
func TestAScrubHoldsAConflictFoundOnACorruptFile(t *testing.T) {
	for _, tc := range []struct {
		config     string
		quarantine bool
		after      string
	}{
		{"", true, "conflict 0"},
		{"[integrity]\nauto_repair = false\n", true, "conflict 0"},
		{"[integrity]\nauto_repair = false\n", false, "corrupt 1"},
	} {
		b := newBenchWith(t, tc.config, "t1", "t2", "t3")
		b.write("t1", "a.mkv", "a film")
		b.write("t2", "a.mkv", "a film")
		b.scan(true)
		b.write("t3", "a.mkv", "A FILM")
		b.scan(false)
		b.now += int64(month)
		b.rot("t2", "a.mkv", "a FILM")
		out := b.scrub(ScrubOptions{Apply: true, Quarantine: tc.quarantine})
		if t2, t3 := diskOf(out, "t2"), diskOf(out, "t3"); !slices.Equal(t2.Corrupt, []string{"a.mkv"}) || !slices.Equal(t3.Conflict, []string{"a.mkv"}) {
			t.Errorf("%q, --yes %v: %+v, %+v", tc.config, tc.quarantine, *t2, *t3)
		}
		if got := b.text("SELECT state || ' ' || conflict_held FROM files"); got != tc.after {
			t.Errorf("%q, --yes %v: the file is %s", tc.config, tc.quarantine, got)
		}
		if got := b.read("t3", "a.mkv"); got != "A FILM" {
			t.Errorf("%q, --yes %v: the other version now holds %q", tc.config, tc.quarantine, got)
		}
		if tc.after != "corrupt 1" {
			continue
		}
		// While the file holds the conflict, no scrub reads its copies again, as for a conflict: the one never
		// verified was read first by every scrub, to find the same.
		again := b.scrub(ScrubOptions{Apply: true})
		if t3 := diskOf(again, "t3"); t3.Read != 0 || len(t3.Conflict) != 0 {
			t.Errorf("%q, a second scrub: %+v", tc.config, *t3)
		}
		b.repair(RepairOptions{Apply: true})
		if got := b.text("SELECT state || ' ' || conflict_held FROM files"); got != "conflict 0" {
			t.Errorf("%q, repaired: the file is %s", tc.config, got)
		}
	}
}

// The copies of a file corrupt that holds a conflict renamed to two names, on their own disks: the one that moves to
// the other name makes a file of its own, which takes the file's hash only when it was read against it, as in a
// conflict, as the hash may be another version's (specs.md §4, §15.12); and the file, left with one version, lets go of
// the conflict it held. The other version took the hash, and each copy made from it failed its check, again and
// again; the file held the conflict, and would have been one once repaired.
func TestACopyOfAHeldConflictRenamedTakesNoHash(t *testing.T) {
	b := newBenchWith(t, "[integrity]\nauto_repair = false\n", "t1", "t2", "t3")
	b.write("t1", "a.mkv", "a film")
	b.write("t2", "a.mkv", "a film")
	b.scan(true)
	b.write("t3", "a.mkv", "A FILM")
	b.scan(false)
	b.now += int64(month)
	b.rot("t2", "a.mkv", "a FILM")
	b.scrub(ScrubOptions{Apply: true})
	if got := b.text("SELECT state || ' ' || conflict_held FROM files"); got != "corrupt 1" {
		t.Fatalf("the file is %s", got)
	}
	b.rename("t1", "a.mkv", "x.mkv")
	b.rename("t3", "a.mkv", "y.mkv")
	b.settle()
	// x.mkv keeps the file and version A; y.mkv, where t3's copy went, is version B, a file of its own, read afresh, its
	// copy made and verified. With the other version's hash, it stayed pending, its copy failing its check at each try.
	rows := func(name string) string {
		t.Helper()
		return b.text(`SELECT f.state || ' ' || f.conflict_held || ' ' || (SELECT group_concat(d, ' ') FROM (SELECT c.disk_id || '/' || c.state || '/' ||
			(c.verified_ns IS NOT NULL) AS d FROM copies c WHERE c.file_id = f.id ORDER BY c.disk_id)) FROM files f WHERE CAST(f.lpath AS TEXT) = ?`, name)
	}
	if got := rows("y.mkv"); got != "ok 0 t1/ok/1 t3/ok/1" {
		t.Errorf("y.mkv, version B: %s", got)
	}
	if got := b.read("t3", "y.mkv"); got != "A FILM" {
		t.Errorf("y.mkv on t3 holds %q", got)
	}
	// x.mkv, left with version A alone, holds no conflict any more, as a conflict would clear, and gets its replica.
	if got := rows("x.mkv"); got != "corrupt 0 t1/ok/1 t2/corrupt/0 t3/ok/1" {
		t.Errorf("x.mkv, version A: %s", got)
	}
	if got := b.read("t3", "x.mkv"); got != "a film" {
		t.Errorf("x.mkv on t3 holds %q", got)
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

// A scrub asks the catalog what is due on a disk before it opens the disk: a disk with nothing due is left asleep
// (specs.md §16, "one disk at a time to limit wake-ups", §22). Each scrub confirmed every disk first, which reads its
// head past the cache and so wakes it: the daemon's slices, 30 s each all night, kept every disk awake for a scrub of
// one, and woke them all on a night with nothing due.
func TestScrubLeavesADiskWithNothingDueAsleep(t *testing.T) {
	b := pool(t)
	for _, tc := range []struct {
		name string
		opts ScrubOptions
	}{
		{"told --yes", ScrubOptions{Apply: true, Quarantine: true}},
		{"not told --yes", ScrubOptions{Apply: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every copy was verified a minute ago: nothing is due.
			clear(b.pastCache)
			out := b.scrub(tc.opts)
			if out.Partial || len(out.Disks) != 2 {
				t.Fatalf("%+v", out)
			}
			if len(b.pastCache) != 0 {
				t.Errorf("with nothing due, disks were read past the cache: %v", b.pastCache)
			}
			// A month on, one disk's copy alone is due: that disk alone is opened.
			b.exec("UPDATE copies SET verified_ns = 1 WHERE disk_id = 't2'")
			clear(b.pastCache)
			out = b.scrub(tc.opts)
			if t2 := diskOf(out, "t2"); out.Partial || t2 == nil || t2.Read != 1 {
				t.Fatalf("%+v", out)
			}
			if b.pastCache[b.dirs["t1"]] != 0 || b.pastCache[b.dirs["t2"]] == 0 {
				t.Errorf("with one disk due, read past the cache: %v", b.pastCache)
			}
		})
	}
	// A disk that is away is still one the scrub could not read, as the mount check says without waking anything.
	b.gone[b.dirs["t1"]] = true
	b.pass()
	if out := b.scrub(ScrubOptions{Apply: true, Quarantine: true}); !out.Partial {
		t.Errorf("a scrub with one disk away: %+v", out)
	}
}

// A copy the scrub cannot open, for a reason that says nothing of its disk, is left as it is, neither verified nor
// corrupt, and the scrub of its disk goes on (specs.md §16). It lost the disk, as an I/O error does, at each slice and
// at the same copy: never verified, such a copy is the first due of its disk.
func TestAScrubLeavesACopyItCannotOpen(t *testing.T) {
	for _, refusal := range []error{syscall.EACCES, syscall.ENOKEY} {
		b := newBench(t, "t1", "t2")
		for _, id := range []string{"t1", "t2"} {
			b.write(id, "a.mkv", "a film")
			b.write(id, "b.mkv", "another")
		}
		b.scan(true)
		b.now += int64(month)
		b.fail[b.dirs["t1"]] = func(call, name string) error {
			if call == "openfile" && name == "a.mkv" {
				return refusal
			}
			return nil
		}
		out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
		t1, t2 := diskOf(out, "t1"), diskOf(out, "t2")
		if !t1.Scrubbed || t1.Read != 1 || !slices.Equal(t1.Refused, []string{"a.mkv"}) || len(t1.Corrupt)+len(t1.Changed)+len(t1.Kept) != 0 {
			t.Fatalf("%v: t1 %+v", refusal, t1)
		}
		if !t2.Scrubbed || t2.Read != 2 || len(t2.Refused) != 0 {
			t.Errorf("%v: t2 %+v", refusal, t2)
		}
		// Not everything due was read: the command says so by its exit, as for a disk it could not read.
		if !out.Partial {
			t.Errorf("%v: a scrub that could not open a copy says it read all that was due", refusal)
		}
		if got := b.count("SELECT count(*) FROM disks WHERE state = 'missing'"); got != 0 {
			t.Errorf("%v: %d disks recorded as missing", refusal, got)
		}
		if got := b.count("SELECT count(*) FROM copies WHERE state <> 'ok'"); got != 0 {
			t.Errorf("%v: %d copies are no longer ok", refusal, got)
		}
		if got := b.count("SELECT count(*) FROM copies WHERE verified_ns >= ?", b.now); got != 3 {
			t.Errorf("%v: %d copies verified by this scrub, want every one but the one refused", refusal, got)
		}
	}
}

// A scrub that only verified copies changed no state, and works none out (specs.md §16). It ended by a plan of the
// whole pool whatever it had done, and the daemon scrubs by slices of 30 s: a night of plans for nothing. One that
// found a corrupt copy, as one that repaired or moved a copy, called a conflict or lost a disk, works them out.
func TestAScrubThatOnlyVerifiedWorksNoStateOut(t *testing.T) {
	b := newBench(t, "t1", "t2")
	data := film()
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a.mkv", string(data))
		b.write(id, "b.mkv", "another")
	}
	b.scan(true)
	b.now += int64(month)
	// A state that only working the states out puts right.
	b.exec("UPDATE files SET state = 'degraded'")
	out := b.scrub(ScrubOptions{Apply: true, Quarantine: true})
	if t1 := diskOf(out, "t1"); t1.Read != 2 || len(t1.Corrupt) != 0 || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM files WHERE state = 'degraded'"); got != 2 {
		t.Errorf("%d files left as they were: a scrub that verified worked the states out", got)
	}
	b.now += int64(month)
	b.rotChunks("t2", "a.mkv", data, 1)
	out = b.scrub(ScrubOptions{Apply: true})
	if t2 := diskOf(out, "t2"); !slices.Equal(t2.Corrupt, []string{"a.mkv"}) {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM files ORDER BY lpath)"); got != "corrupt,ok" {
		t.Errorf("the files are %s: a scrub that found a corrupt copy works the states out", got)
	}
}
