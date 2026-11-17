package jobs

import (
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/watch"
)

// passScoped runs a pass of the daemon with a scope, on battery or not, so that a copy it plans stays to make.
func (b *bench) passScoped(scope Scope, onBattery bool) *Outcome {
	b.t.Helper()
	out, err := RunLaterOn(b.t.Context(), b.db, b.config, b, true, nil, nil, scope, always(onBattery))
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

func (b *bench) stateOf(path string) string {
	b.t.Helper()
	return b.text("SELECT state FROM files WHERE lpath = ?", []byte(path))
}

// surplusPool is a file copied to t2, then given no replica by a rule: a plan that takes it marks its copy on t2
// surplus, one that does not leaves it as the last plan did.
func surplusPool(t *testing.T) *bench {
	t.Helper()
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	b.pass()
	if got := b.holders("a"); got != "t1:user:ok t2:atoll:ok" {
		t.Fatalf("copies of a: %q", got)
	}
	b.reconfigure("[defaults]\nsettle = \"0s\"\n[[rule]]\nlabel = \"none\"\npath = \"a\"\nreplicas = 0\n")
	return b
}

// A pass that looked at names alone plans what may have changed: a file no name led to, at its level, keeps what the
// last plan gave it, here a copy the configuration no longer wants. A pass for any other reason plans every file, and
// marks it (specs.md §15.13).
func TestAPlanOfSomeFilesLeavesTheOthers(t *testing.T) {
	b := surplusPool(t)
	b.write("t1", "b", "new")
	b.passNames("t1", watch.Named{Path: "b"})
	if got := b.holders("a"); got != "t1:user:ok t2:atoll:ok" {
		t.Errorf("a, which no name led to: %q", got)
	}
	if b.stateOf("b") == "" || b.holders("b") == "" {
		t.Errorf("b was not planned: state %q, copies %q", b.stateOf("b"), b.holders("b"))
	}
	b.passLater()
	if got := b.holders("a"); !strings.Contains(got, "t2:atoll:surplus") {
		t.Errorf("a, after a plan of every file: %q", got)
	}
}

// A pass that walked a disk whole plans every file, as every file of that disk may have changed.
func TestAPlanAfterAWalkTakesEveryFile(t *testing.T) {
	b := surplusPool(t)
	b.passScoped(Scope{"t1": Whole}, false)
	if got := b.holders("a"); !strings.Contains(got, "t2:atoll:surplus") {
		t.Errorf("a: %q", got)
	}
}

// A disk in any state but online or standby changes what its files are, with time too: every file is planned.
func TestAPlanWithADiskAwayTakesEveryFile(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	b.pass()
	if b.stateOf("a") != "ok" {
		t.Fatalf("a is %q", b.stateOf("a"))
	}
	b.gone[b.dirs["t2"]] = true
	b.passNames("t1")
	if got := b.stateOf("a"); got == "ok" {
		t.Errorf("a, with a copy on a missing disk, is %q", got)
	}
}

// A plan of some files holds the room of the copies other files wait for: a file with a copy to make is planned with
// them, so that a new file does not take that room.
func TestAPlanOfSomeFilesHoldsTheRoomOfPendingCopies(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[placement]\nnew_files = \"ordered\"\nfill_to = \"100%\"\nmin_free = [\"0%\"]\n", "t1", "t2", "t3")
	b.spaces = map[string][2]int64{b.dirs["t1"]: {1000, 900}, b.dirs["t2"]: {1000, 160}, b.dirs["t3"]: {100000, 90000}}
	b.write("t1", "p", strings.Repeat("p", 100))
	b.scan(false)
	b.passScoped(nil, true)
	if got := b.holders("p"); got != "t1:user:ok t2:atoll:pending" {
		t.Fatalf("copies of p: %q", got)
	}
	b.write("t1", "n", strings.Repeat("n", 100))
	b.passScoped(Scope{"t1": {Names: []watch.Named{{Path: "n"}}}}, true)
	if got := b.holders("n"); got != "t1:user:ok t3:atoll:pending" {
		t.Errorf("copies of n: %q, with 60 bytes left on t2 once p's copy is made", got)
	}
}

// A file's copies go with its group of colocate: the plan of a new file takes the other files of its folder, whose
// copies decide where its own go.
func TestAPlanOfSomeFilesPlacesWithTheirGroup(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[placement]\ncolocate = \"folder\"\nmin_free = [\"0%\"]\n", "t1", "t2", "t3")
	b.spaces = map[string][2]int64{b.dirs["t1"]: {1000, 900}, b.dirs["t2"]: {1000, 500}, b.dirs["t3"]: {1000, 900}}
	b.write("t1", "s/1", "one")
	b.write("t2", "s/1", "one")
	b.scan(true)
	b.passScoped(nil, true)
	b.write("t1", "s/2", "two")
	b.passScoped(Scope{"t1": {Names: []watch.Named{{Path: "s/2"}}}}, true)
	if got := b.holders("s/2"); got != "t1:user:ok t2:atoll:pending" {
		t.Errorf("copies of s/2: %q, its folder's other file being on t2", got)
	}
}

// A file at a name above another's stands in the way of its copies on that disk, where no folder of that name can be
// made: the plan of the new file takes the file above it.
func TestAPlanOfSomeFilesSeesTheFileAbove(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[placement]\nmin_free = [\"0%\"]\n", "t1", "t2", "t3")
	b.write("t2", "x", "a file")
	b.write("t3", "x", "a file")
	b.scan(true)
	b.passScoped(nil, true)
	if b.stateOf("x") != "ok" {
		t.Fatalf("x is %q", b.stateOf("x"))
	}
	b.write("t1", "x/y", "below")
	b.passScoped(Scope{"t1": {Names: []watch.Named{{Path: "x/y"}}}}, true)
	if got := b.holders("x/y"); strings.Contains(got, "t2") || strings.Contains(got, "t3") {
		t.Errorf("copies of x/y: %q, where t2 and t3 hold a file at x", got)
	}
}

// The guard of a disk compares what went with the copies it had before the look: the new files the same walk found
// count for nothing, or a mass deletion hidden among new files would pass under it (specs.md §15.6).
func TestTheGuardCountsTheCopiesBeforeTheLook(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c", "d"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.config = reconfigured(t, b, "files = 100\npercent = \"40%\"\n")
	b.scan(true)
	b.remove("t1", "a")
	b.remove("t1", "b")
	for i := range 6 {
		b.write("t1", "new"+strings.Repeat("x", i), "new")
	}
	if out := b.pass(); !out.Held {
		t.Fatalf("2 of 4 copies gone, among 6 new files, and nothing held: %+v", out)
	}
}
