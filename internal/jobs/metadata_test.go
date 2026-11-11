package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/trash"
)

const touchedNS = 1_700_000_000_123_456_789

// touched gives a pool whose copy on t1 has a new modification time, its settle passed.
func touched(t *testing.T) *bench {
	t.Helper()
	b := pool(t)
	b.touch("t1", "a", touchedNS)
	b.now = b.ctimeOf("t1", "a") + settleNS
	return b
}

// touch sets a modification time, until the change time has moved, which it may not do within a clock tick.
func (b *bench) touch(id, path string, ns int64) {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", path)
	before := b.ctimeOf(id, path)
	for range 2000 {
		if err := os.Chtimes(full, time.Unix(0, ns), time.Unix(0, ns)); err != nil {
			b.t.Fatal(err)
		}
		if b.ctimeOf(id, path) != before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	b.t.Fatal("the change time did not move")
}

func (b *bench) mtimeOf(id, path string) int64 {
	b.t.Helper()
	st, err := os.Stat(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		b.t.Fatal(err)
	}
	return st.ModTime().UnixNano()
}

func timeTo(disk, outcome string) Operation {
	return Operation{Disk: disk, Kind: KindUtimens, Path: "a", Outcome: outcome}
}

// A touch on one copy reaches the other through one operation, listed before the work of the disks, and the rows
// of both say what their disks hold (specs.md §15.4).
func TestATouchReachesTheOtherCopy(t *testing.T) {
	b := touched(t)
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{timeTo("t2", OutcomeApplied)}) || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.mtimeOf("t2", "a"); got != touchedNS {
		t.Errorf("t2 holds %d", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 2 {
		t.Errorf("%d rows say the new time", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND ctime_ns = ?", b.ctimeOf("t2", "a")); got != 1 {
		t.Error("t2's row does not say its change time")
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'utimens' AND state = 'done'"); got != 1 {
		t.Errorf("%d operations done", got)
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}

// A chmod alone moves no modification time: nothing to apply, and only the changed copy's row follows it.
func TestAChmodAloneAppliesNothing(t *testing.T) {
	b := pool(t)
	before := b.ctimeOf("t1", "a")
	full := filepath.Join(b.dirs["t1"], "media", "a")
	for mode := os.FileMode(0o600); b.ctimeOf("t1", "a") == before; mode ^= 0o040 {
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM ops"); got != 0 {
		t.Errorf("%d operations", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND ctime_ns = ?", b.ctimeOf("t1", "a")); got != 1 {
		t.Error("the changed copy's row does not follow it")
	}
}

// A copy that changed since its look gets no time: the operation is undone, nothing is decided, and the next pass
// sees both changes.
func TestACopyChangedBeforeItsTime(t *testing.T) {
	b := touched(t)
	armed := true
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if armed && call == "stat" && name == "a" {
			armed = false
			b.changeAgain("t2", "a", "z")
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t2", trash.OutcomeChanged)}) || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM ops"); got != "undone" {
		t.Errorf("the operation is %s", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS) + b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 0 {
		t.Errorf("%d rows or events", got)
	}
	if b.read("t2", "a") != "z" {
		t.Error("the copy changed since was written to")
	}
}

// A disk lost while the time is set keeps its operation planned, for the reprise; the next pass undoes it, decides
// again and applies the time.
func TestADiskLostDuringATime(t *testing.T) {
	b := touched(t)
	b.fail[b.dirs["t2"]] = func(call, _ string) error {
		if call == "settimes" {
			return syscall.EIO
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t2", trash.OutcomeIOError)}) || !out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM ops"); got != "planned" {
		t.Errorf("the operation is %s", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "missing" {
		t.Errorf("t2 is %s", got)
	}
	delete(b.fail, b.dirs["t2"])
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t2", OutcomeApplied)}) || out.Partial {
		t.Fatalf("then %+v", out)
	}
	if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM ops ORDER BY id)"); got != "undone,done" {
		t.Errorf("the operations are %s", got)
	}
	if b.mtimeOf("t2", "a") != touchedNS {
		t.Error("not applied")
	}
}

// Any other failure is resolved at once, as the reprise would: here nothing happened, and the file waits for the
// next pass.
func TestATimeThatFails(t *testing.T) {
	b := touched(t)
	b.fail[b.dirs["t2"]] = func(call, _ string) error {
		if call == "settimes" {
			return syscall.EPERM
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t2", trash.OutcomeIOError)}) || !out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM ops"); got != "undone" {
		t.Errorf("the operation is %s", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 0 {
		t.Errorf("%d rows decided", got)
	}
}

// Wherever a time stops, the next pass ends as one with no crash: the time on both copies, each row saying it.
func TestACrashAtEachPointOfATime(t *testing.T) {
	for _, point := range []string{"journaled", "times-set"} {
		t.Run(point, func(t *testing.T) {
			b := touched(t)
			b.crashAt(point)
			b.pass()
			if b.mtimeOf("t2", "a") != touchedNS {
				t.Error("not applied")
			}
			if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 2 {
				t.Errorf("%d rows say the time", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'") + b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 0 {
				t.Errorf("%d planned or undone with a warning", got)
			}
			if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
				t.Errorf("then %+v", out)
			}
		})
	}
}

// A copy changed between a crash and the reprise is no time applied: the operation is undone with a warning, and
// the next look says how the copy changed (specs.md §17.2).
func TestARepriseOfATimeOnACopyChangedSince(t *testing.T) {
	b := touched(t)
	b.crashAt("journaled")
	b.changeAgain("t2", "a", "z")
	b.pass()
	if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM ops ORDER BY id)"); got != "undone" {
		t.Errorf("the operations are %s", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d warnings", got)
	}
	if b.read("t2", "a") != "z" {
		t.Error("the copy changed since was written to")
	}
}

// Two copies touched to two times: the last change gives the time, and the other changed copy's row says what its
// operation left, not what its look found.
func TestTheLastTouchGivesTheTime(t *testing.T) {
	b := pool(t)
	b.touch("t1", "a", touchedNS)
	// Later by its change time, which one clock tick may not tell from the first.
	for b.touch("t2", "a", touchedNS+5); b.ctimeOf("t2", "a") <= b.ctimeOf("t1", "a"); {
		b.touch("t2", "a", touchedNS+5)
	}
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t1", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	if b.mtimeOf("t1", "a") != touchedNS+5 {
		t.Error("t1 has not the last time")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND mtime_ns = ? AND ctime_ns = ?", touchedNS+5, b.ctimeOf("t1", "a")); got != 1 {
		t.Error("t1's row does not say what its operation left")
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}

// A time applied to one copy and not to another is not applied twice: the next pass decides again, and only the
// copy left behind gets an operation.
func TestATimeIsNotAppliedTwice(t *testing.T) {
	b := threeDisks(t)
	b.touch("t1", "a", touchedNS)
	b.now = b.ctimeOf("t1", "a") + settleNS
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "settimes" {
			return syscall.EPERM
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t2", OutcomeApplied), timeTo("t3", trash.OutcomeIOError)}) {
		t.Fatalf("%+v", out)
	}
	delete(b.fail, b.dirs["t3"])
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t3", OutcomeApplied)}) {
		t.Fatalf("then %+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 3 {
		t.Errorf("%d rows say the time", got)
	}
}

// A copy on a disk this pass cannot write to goes stale: it is refreshed once its disk is writable, its time with it
// (specs.md §15.4).
func TestATouchWithACopyOnAReadonlyDisk(t *testing.T) {
	b := touched(t)
	b.ro[b.dirs["t2"]] = true
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{overwriteOf("t2", OutcomeUnavailable)}) {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT group_concat(disk_id || state, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "t1ok,t2stale" {
		t.Errorf("%s", got)
	}
}

// A copy its look did not reach goes stale too, as no operation can be decided on what it holds.
func TestATouchWithACopyTheLookDidNotReach(t *testing.T) {
	b := touched(t)
	b.walkFail[b.dirs["t2"]] = func(_, name string) error {
		if name == "a" {
			return syscall.ENOENT
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || slices.ContainsFunc(out.Operations, func(op Operation) bool { return op.Kind == KindUtimens }) {
		t.Fatalf("%+v", out)
	}
	if !slices.Contains(out.Operations, overwriteOf("t2", trash.OutcomeTrashed)) {
		t.Errorf("%+v", out.Operations)
	}
}

// The copy that gives the time is the one a stale copy is refreshed from.
func TestAStaleCopyRefreshedFromTheCopyThatGivesTheTime(t *testing.T) {
	b := threeDisks(t)
	b.gone[b.dirs["t2"]] = true
	b.touch("t1", "a", touchedNS)
	for b.touch("t3", "a", touchedNS+5); b.ctimeOf("t3", "a") <= b.ctimeOf("t1", "a"); {
		b.touch("t3", "a", touchedNS+5)
	}
	b.now = b.ctimeOf("t3", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{timeTo("t1", OutcomeApplied), overwriteOf("t2", OutcomeUnavailable)}) {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT src_disk FROM jobs"); got != "t3" {
		t.Errorf("refreshed from %s", got)
	}
}

// An operation left planned at a copy's name, on a disk the reprise could not write to, holds the file: only a
// reprise can tell what is there (specs.md §15.4, §17.2).
func TestAnOperationLeftPlannedHoldsAMetadataChange(t *testing.T) {
	b := touched(t)
	b.crashAt("times-set")
	b.ro[b.dirs["t2"]] = true
	// Past settle for the time the operation set too: only the operation holds the file.
	b.now = b.ctimeOf("t2", "a") + settleNS
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 || out.Waiting != 0 {
		t.Fatalf("%+v", out)
	}
	delete(b.ro, b.dirs["t2"])
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Fatalf("then %+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mtime_ns = ?", touchedNS); got != 2 {
		t.Errorf("%d rows say the time", got)
	}
}

// The time applied leaves the access time as it was.
func TestATimeKeepsTheAccessTime(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	// Past the change time and the modification time, so that no read moves it (relatime).
	atime := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	full := filepath.Join(b.dirs["t2"], "media", "a")
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, atime, st.ModTime()); err != nil {
		t.Fatal(err)
	}
	b.scan(true)
	b.touch("t1", "a", touchedNS)
	b.now = b.ctimeOf("t1", "a") + settleNS
	if out := b.pass(); !slices.Equal(out.Operations, []Operation{timeTo("t2", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	var sys syscall.Stat_t
	if err := syscall.Stat(full, &sys); err != nil {
		t.Fatal(err)
	}
	if got := sys.Atim.Nano(); got != atime.UnixNano() {
		t.Errorf("access time %d, want %d", got, atime.UnixNano())
	}
}

// A copy whose size changed since a crash, its time put back as its row says, changed all the same: the reprise
// undoes the operation with a warning.
func TestARepriseOfATimeOnACopyOfAnotherSize(t *testing.T) {
	b := touched(t)
	b.crashAt("journaled")
	full := filepath.Join(b.dirs["t2"], "media", "a")
	b.changeAgain("t2", "a", "zz")
	row := time.Unix(0, int64(b.count("SELECT mtime_ns FROM copies WHERE disk_id = 't2'")))
	if err := os.Chtimes(full, row, row); err != nil {
		t.Fatal(err)
	}
	b.pass()
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d warnings", got)
	}
}

// ntfsTick rounds a time down to what NTFS holds of it, 100 ns.
func ntfsTick(ns int64) int64 {
	return ns / 100 * 100
}

// A reprise finds what NTFS holds of the time its event holds, which is all of it that disk can hold: the
// operation finishes, with no warning, and the next decision applies nothing more (specs.md §9, §17.2).
func TestARepriseOfATimeThatNTFSHoldsLessFinely(t *testing.T) {
	b := touched(t)
	b.precision["t2"] = 100
	b.crashAt("times-set")
	// What NTFS would have kept: the bench's folders hold nanoseconds.
	held := ntfsTick(touchedNS)
	if err := os.Chtimes(filepath.Join(b.dirs["t2"], "media", "a"), time.Unix(0, held), time.Unix(0, held)); err != nil {
		t.Fatal(err)
	}
	out := b.pass()
	if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM ops ORDER BY id)"); got != "done" {
		t.Errorf("the operations are %s", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 0 {
		t.Errorf("%d warnings", got)
	}
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Errorf("%+v, %+v", out.Decisions, out.Operations)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND mtime_ns = ?", held); got != 1 {
		t.Error("the row does not say what the disk holds")
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}

// A copy on NTFS that holds, within its 100 ns, the time a touch gave another copy gets no operation: it holds that
// time as finely as it can (specs.md §9, §15.4). Its disk's precision is what its row recorded.
func TestATimeNTFSHoldsAlreadyIsNotApplied(t *testing.T) {
	b := pool(t)
	b.precision["t2"] = 100
	before := b.mtimeOf("t2", "a")
	within := ntfsTick(before) + 99
	if within == before {
		within--
	}
	b.touch("t1", "a", within)
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Errorf("%+v, %+v", out.Decisions, out.Operations)
	}
	if got := b.count("SELECT count(*) FROM disks WHERE id = 't2' AND time_precision_ns = 100"); got != 1 {
		t.Error("the precision is not recorded")
	}
	if b.mtimeOf("t2", "a") != before || b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("the copy was written to")
	}
	if got := b.text("SELECT group_concat(mtime_ns, ',') FROM (SELECT mtime_ns FROM copies ORDER BY disk_id)"); got != fmt.Sprintf("%d,%d", within, before) {
		t.Errorf("the rows say %s", got)
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}
