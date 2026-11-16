package jobs

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/trash"
)

// passOn runs a pass of the daemon that walks only the disks given, and those whose rows say a walk must see them again
// (specs.md §15.13).
func (b *bench) passOn(ids ...string) *Outcome {
	b.t.Helper()
	scope := Scope{}
	for _, id := range ids {
		scope[id] = true
	}
	out, err := RunLaterOn(b.t.Context(), b.db, b.config, b, true, nil, nil, scope)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// countWalks counts, by disk, what the walks of each disk asked of it.
func (b *bench) countWalks() map[string]int {
	calls := map[string]int{}
	for id, dir := range b.dirs {
		b.walkFail[dir] = func(string, string) error {
			calls[id]++
			return nil
		}
	}
	return calls
}

func lineOf(out *Outcome, id string) *DiskLine {
	at := slices.IndexFunc(out.Disks, func(l DiskLine) bool { return l.ID == id })
	if at < 0 {
		return nil
	}
	return &out.Disks[at]
}

// A pass given some disks walks those only. The others are spared, with a line that says so and the verdict of their
// mount check, and nothing reads them: no walk and no confirmation, so that a disk asleep stays so (specs.md §15.13).
func TestALightPassWalksOnlyTheDisksItIsGiven(t *testing.T) {
	b := pool(t)
	b.passLater()
	walks := b.countWalks()
	confirmed := b.pastCache[b.dirs["t2"]]
	out := b.passOn("t1")
	if walks["t1"] == 0 || walks["t2"] != 0 || b.pastCache[b.dirs["t2"]] != confirmed {
		t.Fatalf("walks %v, t2 confirmed %d times", walks, b.pastCache[b.dirs["t2"]]-confirmed)
	}
	one, two := lineOf(out, "t1"), lineOf(out, "t2")
	if one == nil || !one.Walked || one.Spared || two == nil || two.Walked || !two.Spared || two.Verdict.State != disks.Online {
		t.Fatalf("lines %+v", out.Disks)
	}
	if len(out.Decisions)+len(out.Operations) != 0 || out.Partial {
		t.Errorf("%+v", out)
	}
	// A nil scope walks every disk, as before.
	walks = b.countWalks()
	if out := b.passLater(); walks["t2"] == 0 || lineOf(out, "t2").Spared {
		t.Errorf("a pass of every disk: walks %v, lines %+v", walks, out.Disks)
	}
}

// A copy on a disk spared reads as its row says, as a walk of a disk that changed in nothing would find it, and not as
// one on a disk unknown: a deletion on the disk walked reaches it. A disk whose rows say a copy went is walked whatever
// the scope, as the next walk must see it again for its clock to run out.
func TestASparedDiskReadsAsItsRowsSay(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.remove("t1", "a")
	out := b.passOn("t1")
	if len(out.Decisions) != 0 {
		t.Fatalf("decided before the debounce: %+v", out.Decisions)
	}
	b.now += 30_000_000_000
	walks := b.countWalks()
	out = b.passOn()
	if walks["t1"] == 0 || !lineOf(out, "t1").Walked || !lineOf(out, "t2").Spared {
		t.Fatalf("walks %v, lines %+v", walks, out.Disks)
	}
	if got := outcomeOf(out, KindTrash, "a"); got != trash.OutcomeTrashed {
		t.Fatalf("the deletion reached t2: %q, %+v", got, out)
	}
	if b.exists("t2", "media/a") {
		t.Error("the copy on t2 is still there")
	}
}

// A modification on the disk walked refreshes the copy on the disk spared, which is decided on as it is in sync.
func TestAModificationReachesASparedDisk(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.passOn("t1")
	if !lineOf(out, "t2").Spared || !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	if b.read("t2", "a") != "longer" {
		t.Error("the copy on t2 was not refreshed")
	}
}

// A stale copy on a disk spared, with no job, gets its overwrite queued and run, as its row says where it is: the pass
// opens the disk for that work alone.
func TestAStaleCopyOnASparedDiskIsOverwritten(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.exec("UPDATE copies SET state = 'stale' WHERE disk_id = 't2'")
	out := b.passOn("t1")
	if !lineOf(out, "t2").Spared {
		t.Fatalf("lines %+v", out.Disks)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'stale'"); got != 0 {
		t.Fatalf("%d copies still stale: %+v", got, out)
	}
}

// So does a rename: the copy on the disk spared follows its file to the new name, as after a pass of every disk.
func TestARenameReachesASparedDisk(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.rename("t1", "a", "b")
	b.passOn("t1")
	b.now += 30_000_000_000
	out := b.passOn()
	if !lineOf(out, "t2").Spared {
		t.Fatalf("lines %+v", out.Disks)
	}
	if got := outcomeOf(out, KindRename, "a"); got != OutcomeRenamed {
		t.Fatalf("the rename on t2: %q, %+v", got, out)
	}
	if !b.exists("t2", "media/b") || b.exists("t2", "media/a") {
		t.Error("the copy on t2 did not follow")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL OR gone_ns IS NOT NULL"); got != 0 {
		t.Errorf("%d rows still behind", got)
	}
}

// A disk the mount check finds missing is no disk to spare: its line says missing, and the pass is partial, as in a
// pass of every disk.
func TestALightPassStillFindsADiskMissing(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.gone[b.dirs["t2"]] = true
	out := b.passOn("t1")
	if two := lineOf(out, "t2"); two == nil || two.Spared || two.Verdict.State != disks.Missing || !out.Partial {
		t.Fatalf("%+v", out)
	}
}

// What a pass's look found and did not take in, a later look must see again: a changed copy that waits for settle, or
// whose read is left to the slices. Its disk is in the outcome's WaitDisks, which the daemon walks in its next pass. A
// change decided and recorded leaves nothing there.
func TestWhatALookLeftWaitsForTheNextLook(t *testing.T) {
	b := pool(t)
	b.passLater()
	b.changeAgain("t1", "a", "longer")
	b.now = b.ctimeOf("t1", "a")
	if out := b.passLater(); !slices.Equal(out.WaitDisks, []string{"t1"}) || out.Waiting == 0 {
		t.Fatalf("waiting for settle: %+v", out)
	}
	b.now += settleNS
	if out := b.passLater(); !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) || len(out.WaitDisks) != 0 {
		t.Fatalf("decided: %+v", out)
	}
	b = touched(t)
	out := b.passLater()
	if len(out.ToRead) != 1 || !slices.Equal(out.WaitDisks, []string{"t1"}) {
		t.Fatalf("left to read: %+v", out)
	}
	b.readChanged(out.ToRead, farNS)
	walks := b.countWalks()
	if out := b.passOn(); walks["t1"] != 0 || len(out.Decisions)+len(out.Operations)+len(out.ToRead)+len(out.WaitDisks) != 0 {
		t.Errorf("then, with walks %v: %+v", walks, out)
	}
}
