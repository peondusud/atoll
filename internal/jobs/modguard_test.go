package jobs

import (
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// guarded gives a pool of files on two disks whose modification guard lets files through, with more for the rest of
// the configuration.
func guarded(t *testing.T, guard string, names ...string) *bench {
	t.Helper()
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[modifies.mass_guard]\n"+guard, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		for _, n := range names {
			b.write(id, n, "x")
		}
	}
	b.scan(true)
	return b
}

// rewrite gives copies on t1 another content of the same size, so that each must be read, and makes them due.
func (b *bench) rewrite(names ...string) {
	b.t.Helper()
	for _, n := range names {
		b.changeAgain("t1", n, "z")
	}
	b.now = max(b.now, b.ctimeOf("t1", names[len(names)-1])+2_000_000)
}

func decisionsOf(out *Outcome) []Decision {
	return slices.Clone(out.Decisions)
}

// The guard lets through as many files as it allows, then holds the one that would trip it and every one after it:
// nothing of those is read again while it stands, and the other copies stay what they were (specs.md §15.6).
func TestTheModifyGuardStopsAtItsThreshold(t *testing.T) {
	b := guarded(t, "files = 1\npercent = \"100%\"\n", "a", "b", "c")
	b.rewrite("a", "b", "c")
	out := b.pass()
	if want := []Decision{{Path: "a", Kind: "modified"}}; !slices.Equal(decisionsOf(out), want) || !out.Held {
		t.Fatalf("%+v", out)
	}
	if want := []GuardLine{{Disk: "t1", Kind: GuardModifications, Count: 1}}; !slices.Equal(out.Guards, want) {
		t.Errorf("guards %+v", out.Guards)
	}
	if b.read("t2", "b") != "x" || b.read("t2", "c") != "x" {
		t.Error("a held file reached the other copy")
	}
	opened := 0
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "openfile" && (name == "b" || name == "c") {
			opened++
		}
		return nil
	}
	out = b.pass()
	if len(out.Decisions) != 0 || !out.Held || opened != 0 {
		t.Errorf("then %+v, %d reads", out, opened)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'modify-guard' AND level = 'warning'"); got != 1 {
		t.Errorf("%d guard events", got)
	}
}

// The guard trips on its share of a disk's copies too, counting the copies the disk had when the pass began.
func TestTheModifyGuardTripsOnItsPercent(t *testing.T) {
	b := guarded(t, "files = 100\npercent = \"50%\"\n", "a", "b", "c")
	b.rewrite("a", "b", "c")
	out := b.pass()
	// 1 of 3 is not past 50%; 2 of 3 would be.
	if want := []Decision{{Path: "a", Kind: "modified"}}; !slices.Equal(decisionsOf(out), want) {
		t.Errorf("%+v", out.Decisions)
	}
}

// A file with no other copy loses nothing to a modification: it counts for nothing, and has no modified event.
func TestAModificationWithNothingStaleDoesNotCount(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[modifies.mass_guard]\nfiles = 0\npercent = \"100%\"\n", "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(true)
	b.rewrite("a")
	out := b.pass()
	if want := []Decision{{Path: "a", Kind: "modified"}}; !slices.Equal(decisionsOf(out), want) || out.Held || len(out.Guards) != 0 {
		t.Errorf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind IN ('modified', 'modify-guard')"); got != 0 {
		t.Errorf("%d events", got)
	}
}

// A confirm answers what changed by the time the guard tripped: those are decided and count for nothing. What changed
// since is asked about, and counts.
func TestAConfirmAsksAgainAboutWhatChangedSince(t *testing.T) {
	b := guarded(t, "files = 1\npercent = \"100%\"\n", "a", "b", "c", "d")
	b.rewrite("a", "b", "c")
	b.pass()
	tripped := b.now
	if _, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardConfirm, b.now); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	b.rewrite("d")
	if b.ctimeOf("t1", "d") <= tripped {
		t.Skip("the clock did not move past the guard")
	}
	out := b.pass()
	want := []Decision{{Path: "b", Kind: "modified"}, {Path: "c", Kind: "modified"}, {Path: "d", Kind: "modified"}}
	if !slices.Equal(decisionsOf(out), want) || out.Held || len(out.Guards) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT group_concat(COALESCE(json_extract(data, '$.answered'), 0), ',') FROM (SELECT data FROM events WHERE kind = 'modified' ORDER BY id)"); got != "0,1,1,0" {
		t.Errorf("answered: %s", got)
	}
	// d counted: one more is past the guard again.
	b.write("t1", "a", "w")
	b.now = b.ctimeOf("t1", "a") + 2_000_000
	if out := b.pass(); !out.Held || len(out.Guards) != 1 {
		t.Errorf("then %+v", out)
	}
}

// A pass killed once a modification is recorded: the next pass counts it from its event, and the guard trips on the
// next file, as it would have in one pass.
func TestTheModifyGuardCountsWhatACrashLeft(t *testing.T) {
	b := guarded(t, "files = 1\npercent = \"100%\"\n", "a", "b", "c")
	b.rewrite("a", "b", "c")
	b.crashAt("modified")
	out := b.pass()
	if want := []GuardLine{{Disk: "t1", Kind: GuardModifications, Count: 1}}; len(out.Decisions) != 0 || !out.Held || !slices.Equal(out.Guards, want) {
		t.Errorf("%+v", out)
	}
}

// What left the window counts for nothing: a modification an hour ago does not bring the guard closer now.
func TestTheModifyGuardForgetsWhatLeftItsWindow(t *testing.T) {
	b := guarded(t, "files = 1\npercent = \"100%\"\nwindow = \"10m\"\n", "a", "b")
	b.rewrite("a")
	if out := b.pass(); out.Held {
		t.Fatalf("%+v", out)
	}
	b.rewrite("b")
	b.now += int64(time.Hour)
	out := b.pass()
	if want := []Decision{{Path: "b", Kind: "modified"}}; !slices.Equal(decisionsOf(out), want) || out.Held {
		t.Errorf("%+v", out)
	}
}

// The guards tripped come by disk position, the mass-deletion guard of a disk before its mass-modification guard,
// whichever tripped first in the pass (specs.md §15.12).
func TestTheGuardsComeByDiskThenKind(t *testing.T) {
	b := newBenchWith(t, "files = 0\n[modifies.mass_guard]\nfiles = 0\npercent = \"100%\"\n", "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "x")
		b.write(id, "b", "y")
		b.write(id, "c", "w")
	}
	b.scan(true)
	b.changeAgain("t1", "a", "z")
	b.remove("t1", "c")
	b.remove("t2", "b")
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	want := []GuardLine{{Disk: "t1", Kind: GuardDeletions, Count: 1}, {Disk: "t1", Kind: GuardModifications, Count: 0}, {Disk: "t2", Kind: GuardDeletions, Count: 1}}
	if !slices.Equal(out.Guards, want) {
		t.Errorf("%+v", out.Guards)
	}
}

// The guard is that of the disk of the first changed copy, wherever that is: a change on another disk than the first
// is held there, with nothing read.
func TestTheModifyGuardOfTheDiskThatChanged(t *testing.T) {
	b := guarded(t, "files = 0\npercent = \"100%\"\n", "a")
	b.changeAgain("t2", "a", "z")
	b.now = b.ctimeOf("t2", "a") + 2_000_000
	if out := b.pass(); !slices.Equal(out.Guards, []GuardLine{{Disk: "t2", Kind: GuardModifications}}) {
		t.Fatalf("%+v", out)
	}
	opened := 0
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "openfile" && name == "a" {
			opened++
		}
		return nil
	}
	if out := b.pass(); !out.Held || opened != 0 {
		t.Errorf("%+v, %d reads", out, opened)
	}
}

// What a confirm answered is decided whatever trips the guard again in the same pass, before or after it.
func TestAnAnsweredChangeAfterANewTrip(t *testing.T) {
	b := guarded(t, "files = 1\npercent = \"100%\"\n", "a", "b", "d", "e", "y")
	b.rewrite("a", "b", "y")
	b.pass()
	tripped := b.now
	if _, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardConfirm, b.now); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	b.rewrite("d", "e")
	if b.ctimeOf("t1", "d") <= tripped {
		t.Skip("the clock did not move past the guard")
	}
	// b answered, d counted, e trips the guard again, and y, answered, is decided after it.
	out := b.pass()
	want := []Decision{{Path: "b", Kind: "modified"}, {Path: "d", Kind: "modified"}, {Path: "y", Kind: "modified"}}
	if !slices.Equal(decisionsOf(out), want) || !out.Held {
		t.Errorf("%+v", out)
	}
}
