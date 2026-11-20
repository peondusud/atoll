package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/trash"
)

// The points of an operation create after which its copy is at its name in the library, before its row is written
// (specs.md §14.2).
var placedPoints = []string{"moved", "target-synced", "source-synced"}

// propagated says whether a write into the copy on t2 reached t1, the version before in t1's trash.
func (b *bench) propagated(text string) {
	b.t.Helper()
	if got := b.read("t1", "a"); got != text {
		b.t.Fatalf("the edit was not propagated: t1 holds %q", got)
	}
	if got := b.text("SELECT group_concat(disk_id || ' ' || created_by, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "t1 atoll,t2 user" {
		b.t.Errorf("the copies are %s", got)
	}
	if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't1' AND reason = 'overwritten'"); got != 1 {
		b.t.Errorf("%d versions before in the trash of t1", got)
	}
}

// A user's write into a new copy once its name is in the library, before its row is written, is an edit: the row takes
// what the part was when it was verified, and the next look reads the write as a modification, propagated to the
// other copy. The row took what a look found after the syncs, recorded as verified against the version before: no
// look ever saw the edit, and a scrub would have repaired it away (conformance/models/README.md, flaw 1).
func TestAWriteIntoANewCopyBeforeItsRowIsAnEdit(t *testing.T) {
	for _, point := range placedPoints {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.at[point] = func() { b.changeAgain("t2", "a", "CONTENT!!") }
			items, _ := b.replicate("a", ReplicateOptions{Now: true})
			only(t, items, OutcomeReplicated)
			if got := b.count("SELECT size FROM copies WHERE disk_id = 't2'"); got != len("content") {
				t.Errorf("the row records %d bytes, not the part's", got)
			}
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT!!")
		})
	}
}

// The same with a crash at each of those points: the reprise records what the part was when verified, from what its
// job recorded then, however long Atoll was down, and the next look reads the write as a modification. The reprise
// recorded what was there when it ran: the edit was taken for the copy, verified, and never propagated.
func TestAWriteIntoANewCopyWhileAtollIsDown(t *testing.T) {
	for _, point := range placedPoints {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.replicateStopping("a", point)
			b.changeAgain("t2", "a", "CONTENT!!")
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT!!")
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// A disk lost after the rename, written into while away, and back: its reprise finds the write an edit, as above.
func TestAWriteIntoANewCopyOnADiskLostAfterItsRename(t *testing.T) {
	b := replicatePool(t)
	at("moved", failOn("t2", "sync", syscall.EIO))(b)
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, trash.OutcomeIOError)
	if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "planned" {
		t.Fatalf("the operation is %s", got)
	}
	delete(b.fail, b.dirs["t2"])
	b.changeAgain("t2", "a", "CONTENT!!")
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	b.propagated("CONTENT!!")
}

// A chmod of a new copy while Atoll is down moves its mode and its change time, not its size or its dates: the reprise
// records the part's mode, with the change time it had then, so that the next look reads a change of metadata, given to
// the other copy (specs.md §14.2, §15.4). The reprise recorded the mode it found, and the chmod never reached the other
// copy.
func TestAChmodOfANewCopyWhileAtollIsDown(t *testing.T) {
	b := replicatePool(t)
	b.replicateStopping("a", "source-synced")
	mode := int64(b.chmod("t2", "a", 0o600, 0o640))
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{modeTo("t1", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	if b.modeOf("t1", "a") != mode {
		t.Errorf("t1 holds %o, want the mode given to t2, %o", b.modeOf("t1", "a"), mode)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mode = ?", mode); got != 2 {
		t.Errorf("%d rows say the mode", got)
	}
}

// pastTick waits longer than a clock tick at the lowest usual HZ, so that a change time taken after it is later than
// any taken before.
func pastTick() { time.Sleep(20 * time.Millisecond) }

// keepingDates writes into a file as a program that keeps its size and its dates does, until its change time has moved:
// nothing but the change time tells the write.
func (b *bench) keepingDates(id, path, text string) {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", path)
	st, err := os.Stat(full)
	if err != nil {
		b.t.Fatal(err)
	}
	if st.Size() != int64(len(text)) {
		b.t.Fatalf("%q is not of the size of %s", text, full)
	}
	before := b.ctimeOf(id, path)
	for range 2000 {
		if err := os.WriteFile(full, []byte(text), st.Mode()); err != nil {
			b.t.Fatal(err)
		}
		if err := os.Chtimes(full, st.ModTime(), st.ModTime()); err != nil {
			b.t.Fatal(err)
		}
		if b.ctimeOf(id, path) != before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	b.t.Fatal("the change time did not move")
}

// The points of an operation create after which its job has recorded the change time its copy had right after the
// rename, before its row is written (specs.md §14.2).
var recordedPoints = []string{"placed", "target-synced", "source-synced"}

// A write into a new copy once its name is in the library that keeps its size, its times, its mode and its owner moves
// only its change time. Made after the look right after the rename, it is an edit at the next look, as the row takes
// the change time that look found: what a normal run leaves open is the width of that look (specs.md §14.2).
func TestAWriteThatMovesOnlyTheChangeTimeBeforeItsRowIsAnEdit(t *testing.T) {
	for _, point := range append([]string{"moved"}, recordedPoints...) {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.at[point] = func() { b.keepingDates("t2", "a", "CONTENT") }
			items, _ := b.replicate("a", ReplicateOptions{Now: true})
			only(t, items, OutcomeReplicated)
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT")
		})
	}
}

// The same while Atoll is down, after a crash once the job recorded the change time its copy had right after the rename:
// the reprise finds another, a write since the rename, and the row is the part's, so that the next look reads the edit
// as a modification. The reprise took the change time it found, which only a write had moved past the rename's, and the
// edit was recorded as the copy itself, verified, never propagated, and repaired away by the next scrub (the limit of
// flaw 1, closed as the maintainer decided on 2026-11-20).
func TestAWriteThatMovesOnlyTheChangeTimeWhileAtollIsDown(t *testing.T) {
	for _, point := range recordedPoints {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.replicateStopping("a", point)
			if got := b.count("SELECT count(*) FROM jobs WHERE json_extract(payload, '$.placed.op') = (SELECT id FROM ops WHERE kind = 'create')"); got != 1 {
				t.Fatalf("no change time recorded at %s", point)
			}
			pastTick()
			b.keepingDates("t2", "a", "CONTENT")
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT")
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// unverifiedThenConflict says that the copy on t2 was recorded unverified, which a pass leaves so, and that a full scan
// then reads the write in it as another version, a conflict that keeps both, never a corruption a scrub would repair
// away (specs.md §15.10, §16).
func (b *bench) unverifiedThenConflict(text string) {
	b.t.Helper()
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok' AND created_by = 'atoll' AND verified_ns IS NULL"); got != 1 {
		b.t.Fatalf("the copy on t2 is not recorded unverified: %s", b.text("SELECT group_concat(disk_id || ' ' || state || ' ' || COALESCE(verified_ns, 'unverified'), ',') FROM copies"))
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND verified_ns IS NOT NULL"); got != 1 {
		b.t.Error("the source is not verified")
	}
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 1 {
		b.t.Error("the file did not take the hash its source was read with")
	}
	b.scan(true)
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		b.t.Errorf("the file is %s", got)
	}
	if got := b.read("t2", "a"); got != text {
		b.t.Errorf("t2 holds %q", got)
	}
	if got := b.read("t1", "a"); got != "content" {
		b.t.Errorf("t1 holds %q", got)
	}
}

// After a crash between the rename and the commit of the change time the copy had then, nothing tells a write that moved
// only the change time from the rename's: the reprise records the copy unverified, as its change time is not the one it
// was verified with. A full scan then reads the write as a conflict. The reprise recorded it verified, and a scrub would
// have repaired the write away as a corruption (the maintainer, 2026-11-20).
func TestAWriteThatMovesOnlyTheChangeTimeBeforeItsRecordLeavesTheCopyUnverified(t *testing.T) {
	b := replicatePool(t)
	b.replicateStopping("a", "moved")
	if got := b.count("SELECT count(*) FROM jobs WHERE json_extract(payload, '$.placed') IS NOT NULL"); got != 0 {
		t.Fatal("a change time is recorded before the crash point moved")
	}
	pastTick()
	b.keepingDates("t2", "a", "CONTENT")
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	if len(out.Decisions) != 0 {
		t.Errorf("%+v", out.Decisions)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'") + b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d operations or jobs left", got)
	}
	b.unverifiedThenConflict("CONTENT")
}

// failAfter makes a call on a disk fail once another has run there: the disk lost right after it.
func failAfter(id, first, call string, err error) func(b *bench) {
	return func(b *bench) {
		ran := false
		b.fail[b.dirs[id]] = func(c, _ string) error {
			switch {
			case c == first:
				ran = true
			case ran && c == call:
				return err
			}
			return nil
		}
	}
}

// A disk lost right after the rename, before the look that the change time is recorded from, written into while away so
// that only the change time moved, and back: no change time is recorded, and its reprise records the copy unverified.
// One lost once the change time is recorded: its reprise reads the write as an edit, a modification.
func TestAWriteThatMovesOnlyTheChangeTimeOnADiskLostAfterItsRename(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lose   func(b *bench)
		record int
	}{
		{"before the record", failAfter("t2", "rename", "stat", syscall.EIO), 0},
		{"after the record", at("placed", failOn("t2", "sync", syscall.EIO)), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := replicatePool(t)
			tc.lose(b)
			items, _ := b.replicate("a", ReplicateOptions{Now: true})
			only(t, items, trash.OutcomeIOError)
			if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "planned" {
				t.Fatalf("the operation is %s", got)
			}
			if got := b.count("SELECT count(*) FROM jobs WHERE json_extract(payload, '$.placed') IS NOT NULL"); got != tc.record {
				t.Fatalf("%d change times recorded", got)
			}
			delete(b.fail, b.dirs["t2"])
			pastTick()
			b.keepingDates("t2", "a", "CONTENT")
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if tc.record == 0 {
				if len(out.Decisions) != 0 {
					t.Errorf("%+v", out.Decisions)
				}
				b.unverifiedThenConflict("CONTENT")
				return
			}
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT")
		})
	}
}

// Atoll killed at each step of an operation create, and nothing else: the next run makes or finishes the copy, its row
// what its disk holds, so that a pass finds nothing to do, and a full scan reads both copies as the file. The copy is
// verified, but after a crash at moved, between the rename and the commit of the change time it gave: a change time
// past the one the part was verified with may be a write's too, and the copy is unverified until the full scan reads it
// (specs.md §14.2). The rename comes past a clock tick here, so that it moves the change time.
func TestACrashAtEachPointOfACopy(t *testing.T) {
	for _, point := range []string{"journaled", "copied", "verified", "moved", "placed", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.at["verified"] = pastTick
			if point == "verified" {
				delete(b.at, "verified")
			}
			b.replicateStopping("a", point)
			b.replicate("a", ReplicateOptions{Now: true})
			if got := b.read("t2", "a"); got != "content" {
				t.Fatalf("%q", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'") + b.count("SELECT count(*) FROM jobs"); got != 0 {
				t.Errorf("%d operations or jobs left", got)
			}
			if parts := b.parts("t2"); len(parts) != 0 {
				t.Errorf("tmp/ holds %q", parts)
			}
			verified := 1
			if point == "moved" {
				verified = 0
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok' AND verified_ns IS NOT NULL"); got != verified {
				t.Errorf("%d copies on t2 verified, want %d", got, verified)
			}
			out := b.settle()
			if len(out.Decisions) != 0 || len(out.Operations) != 0 || out.Waiting != 0 {
				t.Errorf("a pass after: %+v", out)
			}
			b.scan(true)
			if got := b.count("SELECT count(*) FROM copies WHERE state = 'ok' AND verified_ns IS NOT NULL"); got != 2 {
				t.Errorf("%d copies verified", got)
			}
		})
	}
}

// A copy made with no crash and no write is verified at once, its row the part's with the change time the rename
// gave, which the next look finds as it is.
func TestACopyMadeIsVerifiedAndThere(t *testing.T) {
	b := replicatePool(t)
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns IS NOT NULL AND ctime_ns = ?", b.ctimeOf("t2", "a")); got != 1 {
		t.Error("the copy is not verified with the change time its disk holds")
	}
	st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND mtime_ns = ? AND size = ?", st.ModTime().UnixNano(), st.Size()); got != 1 {
		t.Error("the row is not what the disk holds")
	}
	b.now += int64(time.Hour)
	if out := b.pass(); len(out.Decisions) != 0 || len(out.Operations) != 0 {
		t.Errorf("%+v", out)
	}
}
