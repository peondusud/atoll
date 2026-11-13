package jobs

import (
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/trash"
)

func (b *bench) ctimeOf(id, path string) int64 {
	b.t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(b.dirs[id], "media", path), &st); err != nil {
		b.t.Fatal(err)
	}
	return st.Ctim.Nano()
}

// changeAgain writes a file until its change time has moved, which it may not do within a clock tick.
func (b *bench) changeAgain(id, path, text string) {
	b.t.Helper()
	before := b.ctimeOf(id, path)
	for range 2000 {
		b.write(id, path, text)
		if b.ctimeOf(id, path) != before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	b.t.Fatal("the change time did not move")
}

// edited gives a pool whose copy on t1 was edited, to the same size, with its settle passed.
func edited(t *testing.T) *bench {
	t.Helper()
	b := pool(t)
	b.changeAgain("t1", "a", "y")
	b.now = b.ctimeOf("t1", "a") + settleNS
	return b
}

func overwritten(outcome string) Operation {
	return Operation{Disk: "t2", Kind: KindTrash, Path: "a", Reason: catalog.ReasonOverwritten, Outcome: outcome}
}

// A copy changed less than settle ago, as its change time says, waits and counts in waiting, until the first pass
// at that time; that one reads it, finds another content, and refreshes the other copy (specs.md §15.4).
func TestAModificationWaitsForSettle(t *testing.T) {
	b := pool(t)
	b.changeAgain("t1", "a", "y")
	due := b.ctimeOf("t1", "a") + settleNS
	b.now = due - 1
	out := b.pass()
	if len(out.Decisions) != 0 || len(out.Operations) != 0 || out.Waiting != 1 || !out.NextDue.Valid || out.NextDue.Int64 != due {
		t.Fatalf("a nanosecond early: %+v, want one waiting until %d", out, due)
	}
	if got := b.text("SELECT group_concat(state || created_by, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "okuser,okuser" {
		t.Fatalf("the rows changed: %s", got)
	}
	b.now = due
	out = b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) ||
		!slices.Equal(out.Operations, []Operation{overwritten(trash.OutcomeTrashed), {Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: OutcomeReplicated}}) ||
		out.Waiting != 0 || out.NextDue.Valid || out.Partial {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "y" {
		t.Error("not refreshed")
	}
	// The file takes the hash of what was read, and its chunk, which for one chunk is that hash: the refresh read its
	// source against them.
	if got := b.count("SELECT count(*) FROM files f JOIN file_chunks c ON c.file_id = f.id WHERE c.chunks = f.blake3"); got != 1 {
		t.Errorf("%d files with their chunks", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL"); got != 2 {
		t.Errorf("%d copies verified", got)
	}
}

// A copy changed is the user's, whoever made it, and every queued job of its file goes with its pending copies: a
// deletion no longer stands, and a copy queued may read a source now stale (specs.md §15.4).
func TestAModificationTakesItsFileOver(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(true)
	b.exec("UPDATE copies SET created_by = 'atoll' WHERE disk_id = 't2'")
	b.exec("INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (1, 't3', 'atoll', 1, 'pending')")
	b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state) VALUES ('replicate', 1, 't1', 't3', 'queued')")
	b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', 1, 't3', 't1', 'queued', 'deleted')")
	b.changeAgain("t2", "a", "y")
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	refreshed := []Operation{
		{Disk: "t1", Kind: KindTrash, Path: "a", Reason: catalog.ReasonOverwritten, Outcome: trash.OutcomeTrashed},
		{Disk: "t1", Kind: KindReplicate, Path: "a", Source: "t2", Outcome: OutcomeReplicated},
	}
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) || !slices.Equal(out.Operations, refreshed) {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT created_by FROM copies WHERE disk_id = 't2'"); got != "user" {
		t.Errorf("the copy changed is %s's", got)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE reason IS NOT 'overwritten'"); got != 0 {
		t.Errorf("%d jobs of before", got)
	}
	if b.read("t1", "a") != "y" {
		t.Error("not refreshed")
	}
}

// A copy that changes again after its walk, before its read, is decided nothing of: the next pass reads it again.
func TestACopyChangedAgainBeforeItsLook(t *testing.T) {
	b := edited(t)
	armed := true
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if armed && call == "stat" && name == "a" {
			armed = false
			b.changeAgain("t1", "a", "z")
		}
		return nil
	}
	out := b.pass()
	if armed || len(out.Decisions) != 0 || len(out.Operations) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
		t.Errorf("t2 is %s", got)
	}
	b.now = b.ctimeOf("t1", "a") + settleNS
	if out := b.pass(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("then %+v", out)
	}
	if b.read("t2", "a") != "z" {
		t.Error("not refreshed with what it became")
	}
}

// A copy that moves during its read gives nothing to decide on: it is still being written.
func TestACopyThatMovesDuringItsRead(t *testing.T) {
	b := edited(t)
	armed := true
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if armed && call == "read" && name == "a" {
			armed = false
			b.changeAgain("t1", "a", "w")
		}
		return nil
	}
	out := b.pass()
	if armed || len(out.Decisions) != 0 || len(out.Operations) != 0 || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'ok' AND verified_ns IS NOT NULL"); got != 2 {
		t.Errorf("%d copies verified", got)
	}
}

// A disk that fails a read is missing, as in a scan, and nothing is decided (specs.md §15.4, §15.10).
func TestADiskThatFailsTheRead(t *testing.T) {
	b := edited(t)
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "read" && name == "a" {
			return syscall.EIO
		}
		return nil
	}
	out := b.pass()
	if len(out.Decisions) != 0 || len(out.Operations) != 0 || !out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't1'"); got != "missing" {
		t.Errorf("t1 is %s", got)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
		t.Errorf("t2 is %s", got)
	}
}

// A read runs at the lowest best-effort level, as a copy does (specs.md §14, §15.4).
func TestTheIOPriorityOfARead(t *testing.T) {
	const lowestBE = 2<<13 | 7
	b := edited(t)
	// The first read of the file is the modification's: the copy that follows reads at that level too.
	var during uintptr
	read := false
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "read" && name == "a" && !read {
			read, during = true, ioprioOfThisThread(t)
		}
		return nil
	}
	b.pass()
	if during != lowestBE {
		t.Errorf("a modification reads at %#x, want %#x", during, lowestBE)
	}
}

// A copy replaced by the file another copy of its disk moved to is an overwriting move, no modification: nothing
// goes stale (specs.md §15.4, §15.12).
func TestAnOverwritingMoveIsNoModification(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "x")
		b.write(id, "b", "y")
	}
	b.scan(true)
	b.rename("t1", "b", "a")
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.settle()
	for _, d := range out.Decisions {
		if d.Kind == "modified" || d.Kind == "metadata" || d.Kind == "conflict" {
			t.Errorf("%+v", d)
		}
	}
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'stale'") + b.count("SELECT count(*) FROM events WHERE kind = 'modified'"); got != 0 {
		t.Errorf("%d stale copies or events", got)
	}
}

// A job running is one a reprise could not conclude: what is at its target may be a copy of the version before,
// and the modification waits.
func TestARunningJobHoldsAModification(t *testing.T) {
	b := edited(t)
	b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state) VALUES ('replicate', 1, 't1', 't2', 'running')")
	out := b.pass()
	if len(out.Decisions) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
		t.Errorf("t2 is %s", got)
	}
}

// A stale copy whose file has no copy left to refresh it from is what is left of the file: its trash job goes, and
// it stays where it is (specs.md §15.4).
func TestAStaleCopyWithNothingToRefreshItFrom(t *testing.T) {
	b := pool(t)
	b.gone[b.dirs["t2"]] = true
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a") + settleNS
	if out := b.pass(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	b.remove("t1", "a")
	out := b.settle()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "kept"}}) {
		t.Fatalf("%+v", out)
	}
	delete(b.gone, b.dirs["t2"])
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{overwritten(OutcomeCanceled)}) {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "x" {
		t.Error("what was left of the file went")
	}
	if got := b.text("SELECT state FROM copies"); got != "stale" {
		t.Errorf("the copy is %s", got)
	}
}

// A stale copy in the trash whose job a later decision dropped has nothing to say where to copy from: its row
// goes, and the plan places the copy (specs.md §15.12).
func TestAnOverwriteFinishedWithoutItsJob(t *testing.T) {
	b := edited(t)
	b.crashAt("moved")
	b.exec("DELETE FROM jobs")
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{{Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: OutcomeReplicated}}) {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "y" {
		t.Error("not copied")
	}
	if got := b.text("SELECT group_concat(created_by || state, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "userok,atollok" {
		t.Errorf("%s", got)
	}
}

// Wherever an overwrite stops, the next pass ends as one with no crash: the old copy in the trash, the new one in
// its place, the row a copy of Atoll's.
func TestACrashAtEachPointOfAnOverwrite(t *testing.T) {
	for _, point := range []string{"modified", "journaled", "trash-folders", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := edited(t)
			b.crashAt(point)
			b.pass()
			if b.read("t2", "a") != "y" {
				t.Error("not refreshed")
			}
			if got := b.count("SELECT count(*) FROM trash WHERE reason = 'overwritten' AND blake3 IS NULL"); got != 1 {
				t.Errorf("%d entries in the trash", got)
			}
			if got := b.text("SELECT group_concat(created_by || state, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "userok,atollok" {
				t.Errorf("%s", got)
			}
			if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d jobs or operations left", got)
			}
		})
	}
}

// A copy replaced takes the identity of the file at its name, first seen now, and is the user's.
func TestAReplacedCopyTakesItsNewIdentity(t *testing.T) {
	b := pool(t)
	b.write("t1", "a.tmp", "new!")
	b.rename("t1", "a.tmp", "a")
	b.now = b.ctimeOf("t1", "a") + settleNS
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(b.dirs["t1"], "media", "a"), &st); err != nil {
		t.Fatal(err)
	}
	if out := b.pass(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND file_key = ? AND first_seen_ns = ? AND created_by = 'user' AND size = 4",
		strconv.FormatUint(st.Ino, 10), b.now); got != 1 {
		t.Errorf("the row is %s", b.text("SELECT file_key || ' ' || first_seen_ns || ' ' || created_by FROM copies WHERE disk_id = 't1'"))
	}
	if b.read("t2", "a") != "new!" {
		t.Error("not refreshed")
	}
	// Its creation time is the new file's, where the filesystem gives one (specs.md §7).
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, filepath.Join(b.dirs["t1"], "media", "a"), 0, unix.STATX_BTIME, &stx); err != nil {
		t.Fatal(err)
	}
	if stx.Mask&unix.STATX_BTIME != 0 {
		birth := stx.Btime.Sec*1_000_000_000 + int64(stx.Btime.Nsec)
		if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND btime_ns = ?", birth); got != 1 {
			t.Errorf("the creation time is not the new file's %d", birth)
		}
	}
}

// threeDisks gives a pool of three disks, each with a copy of a that a --full has verified.
func threeDisks(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2", "t3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	return b
}

// awayWhileModified edits a's copy on t1 to another size while the disks given are away: their copies go stale,
// and their trash jobs wait for them.
func (b *bench) awayWhileModified(away ...string) {
	b.t.Helper()
	for _, id := range away {
		b.gone[b.dirs[id]] = true
	}
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a") + settleNS
	if out := b.pass(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		b.t.Fatalf("%+v", out)
	}
}

func overwriteOf(disk, outcome string) Operation {
	return Operation{Disk: disk, Kind: KindTrash, Path: "a", Reason: catalog.ReasonOverwritten, Outcome: outcome}
}

// A file in conflict counts in nothing: none of its changed copies waits for anything (specs.md §15.4).
func TestAFileInConflictWaitsForNothing(t *testing.T) {
	b := pool(t)
	b.exec("UPDATE files SET state = 'conflict'")
	b.changeAgain("t1", "a", "y")
	b.now = b.ctimeOf("t1", "a") + settleNS - 1
	if out := b.pass(); out.Waiting != 0 || out.NextDue.Valid || len(out.Decisions) != 0 {
		t.Fatalf("%+v", out)
	}
}

// A row that says moved, which this look could not get to, is decided nothing of yet: the file waits.
func TestACopyMovedOutOfReachHoldsAModification(t *testing.T) {
	b := pool(t)
	b.exec("UPDATE copies SET gone_kind = 'moved', gone_ns = ? WHERE disk_id = 't2'", b.now)
	b.walkFail[b.dirs["t2"]] = func(call, name string) error {
		if name == "a" {
			return syscall.ENOENT
		}
		return nil
	}
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	if len(out.Decisions) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "ok" {
		t.Errorf("t2 is %s", got)
	}
}

// A guard tripped on the disk of the copy that changed holds what a modification would send to the trash, in a dry
// run as in a real pass (specs.md §15.6, §15.12).
func TestAGuardHoldsAnOverwrite(t *testing.T) {
	b := newBenchWith(t, "files = 0\n", "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "x")
		b.write(id, "b", "y")
	}
	b.scan(true)
	b.remove("t1", "b")
	b.changeAgain("t1", "a", "z")
	b.now = b.ctimeOf("t1", "a") + settleNS
	held := Operation{Disk: "t2", Kind: KindTrash, Path: "a", Reason: catalog.ReasonOverwritten, Outcome: OutcomeHeld}
	for _, apply := range []bool{false, true} {
		out, err := Run(t.Context(), b.db, b.config, b, apply)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(out.Operations, held) || !slices.Contains(out.Decisions, Decision{Path: "a", Kind: "modified"}) {
			t.Errorf("applied %v: %+v", apply, out)
		}
	}
	if b.read("t2", "a") != "x" {
		t.Error("overwritten under the guard")
	}
}

// A copy that must wait for its source after the stale one went to the trash is a pending copy: no look reads it as
// gone, and no decision is made of it (specs.md §15.4).
func TestARefreshThatWaitsIsNoCopyGone(t *testing.T) {
	b := edited(t)
	opened := 0
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "openfile" && name == "a" {
			opened++
			if opened == 2 {
				return syscall.ENOENT
			}
		}
		return nil
	}
	out := b.pass()
	if !slices.Contains(out.Operations, overwriteOf("t2", trash.OutcomeTrashed)) || outcomeOf(out, KindReplicate, "a") != trash.OutcomeChanged {
		t.Fatalf("%+v", out)
	}
	delete(b.fail, b.dirs["t1"])
	if out := b.settle(); len(out.Decisions) != 0 {
		t.Fatalf("then %+v", out)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "pending" {
		t.Errorf("t2 is %s", got)
	}
}

// A file deleted while one of its copies waits stale: the deletion takes that copy to the trash, and nothing is
// refreshed (specs.md §15.4, §15.12).
func TestADeletionOfAFileWithAStaleCopy(t *testing.T) {
	b := threeDisks(t)
	b.exec("UPDATE copies SET created_by = 'atoll' WHERE disk_id <> 't1'")
	b.awayWhileModified("t2")
	b.remove("t1", "a")
	out := b.settle()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "deleted"}}) {
		t.Fatalf("%+v", out)
	}
	delete(b.gone, b.dirs["t2"])
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{{Disk: "t2", Kind: KindTrash, Path: "a", Reason: reasonDeleted, Outcome: trash.OutcomeTrashed}}) {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM files"); got != 0 {
		t.Errorf("%d files", got)
	}
}

// A file in conflict overwrites nothing: a stale copy's trash job goes when the file turns out in conflict
// (specs.md §15.4).
func TestAConflictKeepsAStaleCopy(t *testing.T) {
	b := threeDisks(t)
	b.awayWhileModified("t2")
	b.changeAgain("t1", "a", "first!")
	b.changeAgain("t3", "a", "second")
	b.now = max(b.ctimeOf("t1", "a"), b.ctimeOf("t3", "a")) + settleNS
	if out := b.pass(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "conflict"}}) {
		t.Fatalf("%+v", out)
	}
	delete(b.gone, b.dirs["t2"])
	if out := b.pass(); !slices.Equal(out.Operations, []Operation{overwriteOf("t2", OutcomeCanceled)}) {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "x" {
		t.Error("overwritten in a conflict")
	}
}

// A surplus copy is a source a stale copy can be refreshed from (specs.md §13.4, §15.4).
func TestAStaleCopyRefreshedFromASurplusCopy(t *testing.T) {
	b := pool(t)
	b.awayWhileModified("t2")
	b.exec("UPDATE copies SET state = 'surplus' WHERE disk_id = 't1'")
	delete(b.gone, b.dirs["t2"])
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{overwriteOf("t2", trash.OutcomeTrashed), {Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: OutcomeReplicated}}) {
		t.Fatalf("%+v", out)
	}
}

// Stale copies whose file has nothing left to refresh them from each keep what they hold: a job that goes takes no
// other with it, in a dry run as in a real pass.
func TestStaleCopiesWithNothingToRefreshThemFrom(t *testing.T) {
	b := threeDisks(t)
	b.awayWhileModified("t2", "t3")
	b.remove("t1", "a")
	if out := b.settle(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "kept"}}) {
		t.Fatalf("%+v", out)
	}
	delete(b.gone, b.dirs["t2"])
	delete(b.gone, b.dirs["t3"])
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Operations, []Operation{overwriteOf("t2", OutcomeCanceled), overwriteOf("t3", OutcomeCanceled)}) {
		t.Fatalf("a dry run: %+v", out)
	}
	b.gone[b.dirs["t3"]] = true
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{overwriteOf("t2", OutcomeCanceled), overwriteOf("t3", OutcomeUnavailable)}) {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 1 {
		t.Errorf("%d jobs: t3's waits for it", got)
	}
	if b.read("t2", "a") != "x" {
		t.Error("what was left of the file went")
	}
}

// staleState says what the catalog holds of the copies and of the trash jobs, for the tests of a stale copy.
func (b *bench) staleState() string {
	b.t.Helper()
	return b.text("SELECT group_concat(line, ', ') FROM (SELECT disk_id || ' ' || state AS line FROM copies ORDER BY disk_id)") + " | " +
		b.text("SELECT COALESCE(group_concat(line, ', '), '') FROM (SELECT dst_disk || ' ' || reason || ' ' || state AS line FROM jobs WHERE kind = 'trash' ORDER BY id)")
}

// A stale copy whose file is renamed while its disk is away still reaches the trash, and is made again: the rename
// drops every trash job of the file, as a deletion no longer stands, and the stale copy's with them (specs.md §15.4,
// §15.12). Nothing queued it again: the copy followed the rename and stayed in media/ with the version that came
// before, for good, and no replica of the current version was made.
func TestAStaleCopyRenamedWhileAwayIsMadeAgain(t *testing.T) {
	for _, how := range []string{"away", "read-only"} {
		t.Run(how, func(t *testing.T) {
			b := pool(t)
			if how == "away" {
				b.awayWhileModified("t2")
			} else {
				b.ro[b.dirs["t2"]] = true
				b.awayWhileModified()
			}
			b.rename("t1", "a", "b")
			b.settle()
			delete(b.gone, b.dirs["t2"])
			b.ro[b.dirs["t2"]] = false
			b.settle()
			b.settle()
			if b.exists("t2", "media/a") || !b.exists("t2", "media/b") || b.read("t2", "b") != "longer" {
				t.Errorf("the stale copy is not made again: %s", b.staleState())
			}
			if got := b.staleState(); got != "t1 ok, t2 ok | " {
				t.Errorf("%s", got)
			}
			if got := b.text("SELECT group_concat(disk_id || ' ' || reason, ', ') FROM trash"); got != "t2 overwritten" {
				t.Errorf("the trash: %s", got)
			}
		})
	}
}

// A guard rejected takes the deletions it asked about back, and only those (specs.md §15.6): the overwrite of a stale
// copy it held is no deletion, and runs once the guard no longer stands. It went with them, and the copy stayed stale.
func TestAGuardRejectedLeavesAnOverwrite(t *testing.T) {
	b := newBenchWith(t, "files = 0\n", "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "x")
		b.write(id, "b", "y")
	}
	b.scan(true)
	b.remove("t1", "b")
	b.changeAgain("t1", "a", "z")
	b.now = b.ctimeOf("t1", "a") + settleNS
	held := Operation{Disk: "t2", Kind: KindTrash, Path: "a", Reason: catalog.ReasonOverwritten, Outcome: OutcomeHeld}
	if out := b.pass(); !slices.Contains(out.Operations, held) {
		t.Fatalf("%+v", out)
	}
	if _, err := catalog.RejectGuard(t.Context(), b.db, "t1", b.now, nil); err != nil {
		t.Fatal(err)
	}
	b.settle()
	if b.read("t2", "a") != "z" {
		t.Errorf("the stale copy is not made again: %s", b.staleState())
	}
	if !b.exists("t2", "media/b") {
		t.Error("a deletion the guard was told to take back went through")
	}
}

// A stale copy that someone changed has lost its trash job to that change, and gets none again while it is not what
// its row says: what becomes of it is for a decision, and no pass tries the move again meanwhile (specs.md §15.4).
func TestAStaleCopyThatChangedGetsNoJobAgain(t *testing.T) {
	b := pool(t)
	b.awayWhileModified("t2")
	delete(b.gone, b.dirs["t2"])
	b.changeAgain("t2", "a", "mine")
	b.exec("DELETE FROM jobs")
	before := b.count("SELECT count(*) FROM ops")
	b.pass()
	b.pass()
	if got := b.count("SELECT count(*) FROM ops"); got != before {
		t.Errorf("%d operations for a copy that changed", got-before)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'trash'"); got != 0 {
		t.Errorf("%d trash jobs for a copy that changed", got)
	}
	if b.read("t2", "a") != "mine" {
		t.Error("an edit went")
	}
}
