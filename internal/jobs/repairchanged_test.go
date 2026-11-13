package jobs

import (
	"fmt"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// foundCorrupt gives a film on two disks whose copy on t2 rotted in its second chunk, found corrupt by a scrub that
// may not write: the copy and its file are corrupt, and nothing was repaired or moved. It gives the film and what
// the copy holds.
func foundCorrupt(t *testing.T, more string) (*bench, []byte, []byte) {
	t.Helper()
	b, data := repairBench(t, more)
	rotten := b.rotChunks("t2", "a.mkv", data, 1)
	b.scrub(ScrubOptions{Apply: true})
	if got := b.text("SELECT c.state || ' ' || f.state FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2'"); got != "corrupt corrupt" {
		t.Fatalf("the copy and its file are %s", got)
	}
	return b, data, rotten
}

// rowOf gives what a copy's row says of it: its state, whether it is verified, and its size and times.
func (b *bench) rowOf(id string) string {
	b.t.Helper()
	return b.text("SELECT state || ' ' || (verified_ns IS NOT NULL) || ' ' || size || ' ' || mtime_ns || ' ' || ctime_ns FROM copies WHERE disk_id = ?", id)
}

// heldBy gives what a copy's row would say were it state, verified or not, and as its disk holds it now.
func (b *bench) heldBy(id, path, state string, verified bool) string {
	b.t.Helper()
	text := state + " 0 "
	if verified {
		text = state + " 1 "
	}
	st := b.read(id, path)
	return text + fmt.Sprintf("%d %d %d", len(st), b.mtimeOf(id, path), b.ctimeOf(id, path))
}

// A corrupt copy that someone put right by hand, writing the film back over it, moved its times: the repair left it
// to the next pass as a change, and a pass leaves a corrupt file alone, so it stayed corrupt for good, each repair
// answering changed. Read whole as it is now, it holds its file's content: it is ok and verified, its row what was
// read, and nothing is written to a disk (specs.md §16.1).
func TestARepairVerifiesACorruptCopyPutRightByHand(t *testing.T) {
	b, data, _ := foundCorrupt(t, "")
	b.changeAgain("t2", "a.mkv", string(data))
	out := b.repair(RepairOptions{Apply: true})
	if len(out.Items) != 1 || out.Items[0] != (RepairItem{Disk: "t2", Path: "a.mkv", Mode: RepairChunk, Outcome: OutcomeVerified}) || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got, want := b.rowOf("t2"), b.heldBy("t2", "a.mkv", "ok", true); got != want {
		t.Errorf("the copy's row says %s, for %s", got, want)
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("the file is %s", got)
	}
	if b.count("SELECT count(*) FROM ops") != 0 || b.count("SELECT count(*) FROM trash") != 0 {
		t.Error("something was written to a disk")
	}
	// The row says what the disk holds: the next pass has nothing to decide of it.
	b.now = b.ctimeOf("t2", "a.mkv") + settleNS
	if pass := b.pass(); len(pass.Decisions) != 0 || len(pass.Operations) != 0 {
		t.Errorf("the next pass decides %+v, and runs %+v", pass.Decisions, pass.Operations)
	}
	if b.read("t1", "a.mkv") != string(data) || b.read("t2", "a.mkv") != string(data) {
		t.Error("a copy no longer holds the film")
	}
}

// A corrupt copy that someone only touched still holds what rotted, and nothing tells that from another version of
// the file: the file becomes a conflict, both kept, for conflicts resolve (specs.md §15.7, §16.1). The copy is then
// ok and never verified, its row what its disk holds, and nothing is written to a disk.
func TestARepairCallsACorruptCopyThatChangedAConflict(t *testing.T) {
	b, data, rotten := foundCorrupt(t, "")
	b.touch("t2", "a.mkv", touchedNS)
	b.now = b.ctimeOf("t2", "a.mkv") + settleNS
	t1 := b.rowOf("t1")
	out := b.repair(RepairOptions{Apply: true})
	if len(out.Items) != 1 || out.Items[0] != (RepairItem{Disk: "t2", Path: "a.mkv", Mode: RepairChunk, Outcome: OutcomeConflict}) || out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
	if got, want := b.rowOf("t2"), b.heldBy("t2", "a.mkv", "ok", false); got != want {
		t.Errorf("the copy's row says %s, for %s", got, want)
	}
	if got := b.rowOf("t1"); got != t1 {
		t.Errorf("the other copy's row says %s, was %s", got, t1)
	}
	if b.count("SELECT count(*) FROM ops") != 0 || b.count("SELECT count(*) FROM trash") != 0 {
		t.Error("something was written to a disk")
	}
	// Nothing is decided of a file in conflict, and both versions stay.
	if pass := b.pass(); len(pass.Decisions) != 0 || len(pass.Operations) != 0 {
		t.Errorf("the next pass decides %+v, and runs %+v", pass.Decisions, pass.Operations)
	}
	if b.read("t1", "a.mkv") != string(data) || b.read("t2", "a.mkv") != string(rotten) {
		t.Error("a version is gone")
	}
	if again := b.repair(RepairOptions{Apply: true}); len(again.Items) != 0 {
		t.Errorf("a copy is still to repair: %+v", again)
	}
	// conflicts resolve ends it, keeping the film: what rotted goes to the trash.
	session, err := trash.Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := session.Resolve(t.Context(), "a.mkv", "t1", func(*trash.ResolveItem) error { return nil })
	session.Close()
	if err != nil || !sum.Resolved {
		t.Fatalf("the conflict is not resolved: %+v, %v", sum, err)
	}
	if b.exists("t2", "media/a.mkv") || b.read("t1", "a.mkv") != string(data) {
		t.Error("the copy kept is not the film alone")
	}
}

// A copy of another size cannot hold its file's content, and needs no read to say so.
func TestARepairCallsACorruptCopyOfAnotherSizeAConflict(t *testing.T) {
	b, _, _ := foundCorrupt(t, "")
	b.changeAgain("t2", "a.mkv", "another film")
	b.now = b.ctimeOf("t2", "a.mkv") + settleNS
	if out := b.repair(RepairOptions{Apply: true}); len(out.Items) != 1 || out.Items[0].Outcome != OutcomeConflict {
		t.Fatalf("%+v", out)
	}
	if got, want := b.rowOf("t2"), b.heldBy("t2", "a.mkv", "ok", false); got != want {
		t.Errorf("the copy's row says %s, for %s", got, want)
	}
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
	if b.read("t2", "a.mkv") != "another film" {
		t.Error("the version is gone")
	}
}

// A copy that changed less than settle ago may still be being written, as one someone is writing the film back
// over: it is no conflict yet, and waits, as a changed copy does in a pass (specs.md §15.4). One that holds the film
// already waits for nothing.
func TestARepairWaitsForACorruptCopyStillChanging(t *testing.T) {
	b, data, _ := foundCorrupt(t, "")
	b.changeAgain("t2", "a.mkv", string(data[:len(data)/2]))
	b.now = b.ctimeOf("t2", "a.mkv") + settleNS - 1
	row := b.rowOf("t2")
	if out := b.repair(RepairOptions{Apply: true}); len(out.Items) != 1 || out.Items[0].Outcome != trash.OutcomeChanged {
		t.Fatalf("%+v", out)
	}
	if got := b.rowOf("t2"); got != row || b.text("SELECT state FROM files") != "corrupt" {
		t.Errorf("the copy's row says %s, was %s", got, row)
	}
	// The film is all there now, a moment ago: nothing to wait for.
	b.changeAgain("t2", "a.mkv", string(data))
	b.now = b.ctimeOf("t2", "a.mkv") + 1
	if out := b.repair(RepairOptions{Apply: true}); len(out.Items) != 1 || out.Items[0].Outcome != OutcomeVerified {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("the file is %s", got)
	}
}

// Without --yes a repair says what it found and records it, as it records a copy that reads right after all; with
// --dry-run it records nothing (specs.md §16.1, §25).
func TestARepairThatMayNotWriteSaysWhatACopyThatChangedIs(t *testing.T) {
	b, _, _ := foundCorrupt(t, "")
	b.touch("t2", "a.mkv", touchedNS)
	b.now = b.ctimeOf("t2", "a.mkv") + settleNS
	row := b.rowOf("t2")
	if out := b.repair(RepairOptions{}); len(out.Items) != 1 || out.Items[0].Outcome != OutcomeConflict {
		t.Fatalf("%+v", out)
	}
	if got := b.rowOf("t2"); got != row || b.text("SELECT state FROM files") != "corrupt" {
		t.Errorf("a dry run recorded something: %s, was %s", got, row)
	}
	if out := b.repair(RepairOptions{Record: true}); len(out.Items) != 1 || out.Items[0].Outcome != OutcomeConflict {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
}

// With no healthy copy, a copy put right by hand is the way out: it is verified with no other copy to compare with,
// and the other corrupt copy is then repaired from it, in the same run, whichever comes first (specs.md §16.1).
func TestACopyPutRightByHandRepairsTheOthers(t *testing.T) {
	for _, fixed := range []string{"t1", "t2"} {
		b, data := repairBench(t, "")
		b.rotChunks("t1", "a.mkv", data, 0)
		b.rotChunks("t2", "a.mkv", data, 1)
		b.scrub(ScrubOptions{Apply: true})
		if got := b.count("SELECT count(*) FROM copies WHERE state = 'corrupt'"); got != 2 {
			t.Fatalf("%d corrupt copies", got)
		}
		if out := b.repair(RepairOptions{Apply: true}); len(out.Items) != 2 || out.Items[0].Outcome != OutcomeNoHealthyCopy || out.Items[1].Outcome != OutcomeNoHealthyCopy {
			t.Fatalf("%+v", out)
		}
		b.changeAgain(fixed, "a.mkv", string(data))
		out := b.repair(RepairOptions{Apply: true})
		if len(out.Items) != 2 {
			t.Fatalf("%s put right: %+v", fixed, out)
		}
		for _, item := range out.Items {
			want := trash.OutcomeRepaired
			if item.Disk == fixed {
				want = OutcomeVerified
			}
			if item.Outcome != want {
				t.Errorf("%s put right: the copy on %s is %s, for %s", fixed, item.Disk, item.Outcome, want)
			}
		}
		if b.read("t1", "a.mkv") != string(data) || b.read("t2", "a.mkv") != string(data) {
			t.Errorf("%s put right: a copy does not hold the film", fixed)
		}
		if got := b.text("SELECT group_concat(state || ' ' || (verified_ns IS NOT NULL), ',') FROM copies"); got != "ok 1,ok 1" {
			t.Errorf("%s put right: the copies are %s", fixed, got)
		}
		if got := b.text("SELECT state FROM files"); got != "ok" {
			t.Errorf("%s put right: the file is %s", fixed, got)
		}
	}
}

// A file is corrupt while a copy of it is (specs.md §16): with another copy still corrupt, the one that changed is
// no longer, and the conflict is recorded once the other is repaired, in the same run.
func TestAConflictFoundBesideAnotherCorruptCopy(t *testing.T) {
	b := newBenchWith(t, "", "t1", "t2", "t3")
	data := film()
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a.mkv", string(data))
	}
	b.scan(true)
	b.now += int64(month)
	b.rotChunks("t2", "a.mkv", data, 1)
	b.rotChunks("t3", "a.mkv", data, 2)
	b.scrub(ScrubOptions{Apply: true})
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'corrupt'"); got != 2 {
		t.Fatalf("%d corrupt copies", got)
	}
	b.touch("t2", "a.mkv", touchedNS)
	b.now = b.ctimeOf("t2", "a.mkv") + settleNS
	out := b.repair(RepairOptions{Apply: true})
	if len(out.Items) != 2 || out.Items[0].Outcome != OutcomeConflict || out.Items[1].Outcome != trash.OutcomeRepaired {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
	if got := b.text("SELECT group_concat(disk_id || ' ' || (state <> 'corrupt') || ' ' || (verified_ns IS NOT NULL), ',') FROM copies"); got != "t1 1 1,t2 1 0,t3 1 1" {
		t.Errorf("the copies are %s", got)
	}
}
