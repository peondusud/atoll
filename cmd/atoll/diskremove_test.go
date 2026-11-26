package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stopped is what a test's checkpoint panics with, to stop a command there as kill -9 would: what it committed stays,
// and nothing after it runs.
type stopped struct{ point string }

func (b *poolBench) setAt(at func(point string)) {
	b.machine.mu.Lock()
	defer b.machine.mu.Unlock()
	b.machine.at = at
}

// stoppedAt runs a command that stops at a checkpoint, as kill -9 would, and says whether it reached it.
func (b *poolBench) stoppedAt(point string, args ...string) (reached bool) {
	b.t.Helper()
	b.setAt(func(p string) {
		if p == point {
			panic(stopped{p}) //nolint:forbidigo // what stands for kill -9
		}
	})
	defer b.setAt(nil)
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(stopped); !ok {
				panic(r) //nolint:forbidigo // anything else than the stop goes on as it came
			}
			reached = true
		}
	}()
	b.atoll(args...)
	return false
}

// configured says whether the configuration still lists a disk.
func (b *poolBench) configured(id string) bool {
	b.t.Helper()
	text, err := os.ReadFile(b.configPath)
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Contains(string(text), fmt.Sprintf("id = %q", id))
}

// threeCopies makes the file x2 on three disks, each a user's copy, t3's the surplus one, as the plan keeps the first
// disks' (solver.compareKeep).
func threeCopies(t *testing.T) *poolBench {
	t.Helper()
	b := newPoolBench(t, 3, "[defaults]\nreplicas = 1\n")
	for disk := 1; disk <= 3; disk++ {
		b.write(disk, "Films/a.mkv", "a film")
	}
	b.ok(exitOK, "init")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.advance(time.Hour)
	b.ok(exitOK, "daemon", "--until-idle")
	return b
}

// Flaw 29 of the TLA+ models, fixed with remove-looks (conformance/models/README.md): an edit made on a drained disk,
// directly or through mergerfs, which writes to the copy its branch holds, and not yet taken in by a pass, keeps the
// disk in the pool. Once a pass took it in and made it on the other disks, the disk goes.
func TestAnEditOnADrainedDiskStaysInThePool(t *testing.T) {
	// The guards let one file of one change, as the edit is the pool's only file (specs.md §15.6).
	b := newPoolBench(t, 3, "[defaults]\nreplicas = 1\nsettle = \"0s\"\n[modifies.mass_guard]\npercent = \"100%\"\n"+
		"[modifies.day_guard]\npercent = \"100%\"\n")
	b.write(1, "Films/a.mkv", "a film")
	b.ok(exitOK, "init")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.advance(time.Hour)
	b.ok(exitOK, "daemon", "--until-idle")
	b.json(exitOK, "disk", "remove", "t1", "--evacuate")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.advance(time.Hour)
	b.ok(exitOK, "daemon", "--until-idle")
	if got := b.read(2, "Films/a.mkv") + ", " + b.read(3, "Films/a.mkv"); got != "a film, a film" {
		t.Fatalf("t1 not drained to t2 and t3: they hold %q", got)
	}
	b.write(1, "Films/a.mkv", "the user's edit")
	code, stdout, stderr := b.atoll("disk", "remove", "t1", "--yes")
	if code != exitError || !b.configured("t1") || !strings.Contains(stderr, "the catalog does not hold") {
		t.Fatalf("disk remove t1 --yes, the edit not taken in: exit %d, t1 listed %v\n%s%s", code, b.configured("t1"), stdout, stderr)
	}
	// The bench's clock past the edit's modification time, the machine's own, so that the change settles.
	b.machine.mu.Lock()
	b.machine.now = time.Now().Add(time.Hour).UnixNano()
	b.machine.mu.Unlock()
	for range 3 {
		b.machine.advance(time.Hour)
		b.atoll("daemon", "--until-idle")
	}
	b.ok(exitOK, "disk", "remove", "t1", "--yes")
	if got := b.read(2, "Films/a.mkv") + ", " + b.read(3, "Films/a.mkv"); got != "the user's edit, the user's edit" {
		t.Errorf("t1 taken out once its edit was taken in: t2 and t3 hold %q", got)
	}
}

// Flaw 29 through a disk gone: the file x1, a user's copy on t1 and t2, t2's the surplus one. With t1's mount gone
// and no pass since, t2 stays: the mounts are checked first, and a surplus copy counts as an ok one. Once t1 is back,
// t2 goes.
func TestASurplusCopyStaysWhileTheKeptOneIsGone(t *testing.T) {
	b := newPoolBench(t, 2, "[defaults]\nreplicas = 0\n")
	b.write(1, "Films/a.mkv", "a film")
	b.write(2, "Films/a.mkv", "a film")
	b.ok(exitOK, "init")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.advance(time.Hour)
	b.ok(exitOK, "daemon", "--until-idle")
	shown := b.json(exitOK, "disk", "show", "t2")
	if copies, _ := shown["copies"].(map[string]any); copies["surplus"] != float64(1) {
		t.Fatalf("t2's copy is not the surplus one: %v", shown["copies"])
	}
	b.machine.setAway(b.mounts[0], true)
	code, stdout, stderr := b.atoll("disk", "remove", "t2", "--yes", "--json")
	if code != exitError || !b.configured("t2") {
		t.Fatalf("disk remove t2 --yes, t1 not mounted: exit %d\n%s%s", code, stdout, stderr)
	}
	b.machine.setAway(b.mounts[0], false)
	b.ok(exitOK, "disk", "remove", "t2", "--yes")
	if b.configured("t2") {
		t.Error("t2 still listed")
	}
}

// remove-looks: a surplus copy counts as an ok one elsewhere too, so a disk whose file has it beside one ok copy, x2,
// goes.
func TestASurplusCopyCountsForARemoval(t *testing.T) {
	b := threeCopies(t)
	out := b.json(exitOK, "disk", "remove", "t1", "--yes")
	if out["done"] != true || b.configured("t1") {
		t.Errorf("t1 not taken out though t2 and t3 hold the file: %v", out)
	}
}

// remove-looks: a disk missing is taken out only once declared failed, which gives up what only it holds: it cannot be
// looked at.
func TestAMissingDiskGoesOnceFailed(t *testing.T) {
	b := threeCopies(t)
	b.machine.setAway(b.mounts[2], true)
	code, stdout, stderr := b.atoll("disk", "remove", "t3", "--yes")
	if code != exitError || !b.configured("t3") || !strings.Contains(stderr, "atoll disk fail t3") {
		t.Fatalf("disk remove of a disk missing: exit %d\n%s%s", code, stdout, stderr)
	}
	b.json(exitOK, "disk", "fail", "t3", "--yes")
	b.ok(exitOK, "disk", "remove", "t3", "--yes")
	if b.configured("t3") {
		t.Error("t3 still listed once failed and taken out")
	}
}

// remove-looks: a look at the disk that fails with EIO says nothing of what it holds (invariant 4): the disk stays.
func TestARemovalWhoseLookFails(t *testing.T) {
	b := threeCopies(t)
	b.machine.mu.Lock()
	b.machine.fail = func(mount, call, _ string) error {
		if mount == b.mounts[2] && (call == "names" || call == "stat") {
			return syscall.EIO
		}
		return nil
	}
	b.machine.mu.Unlock()
	code, stdout, stderr := b.atoll("disk", "remove", "t3", "--yes")
	if code != exitError || !b.configured("t3") {
		t.Fatalf("disk remove of a disk whose look fails: exit %d\n%s%s", code, stdout, stderr)
	}
	b.machine.mu.Lock()
	b.machine.fail = nil
	b.machine.mu.Unlock()
	b.ok(exitOK, "disk", "remove", "t3", "--yes")
}

// remove-looks: disk remove stopped, as by kill -9, once the mounts are checked and recorded, and once its look is over,
// leaves the configuration as it was; run again, it goes to its end.
func TestARemovalStoppedAtEachStep(t *testing.T) {
	for _, point := range []string{"remove-checked", "remove-looked"} {
		t.Run(point, func(t *testing.T) {
			b := threeCopies(t)
			if !b.stoppedAt(point, "disk", "remove", "t3", "--yes") {
				t.Fatalf("disk remove never reached %s", point)
			}
			if !b.configured("t3") {
				t.Fatalf("stopped at %s, t3 is out of the configuration", point)
			}
			b.ok(exitOK, "disk", "remove", "t3", "--yes")
			if b.configured("t3") {
				t.Error("t3 still listed")
			}
		})
	}
}

// A disk lost between the look and the edit of the configuration is not seen by the removal, which goes on: the
// window is the write of the configuration, a user's race with the command (conformance/models/README.md). The pool
// records it missing right after, and what the disk taken out holds stays on it.
func TestADiskLostBetweenTheLookAndTheEdit(t *testing.T) {
	b := threeCopies(t)
	b.setAt(func(point string) {
		if point == "remove-looked" {
			b.machine.setAway(b.mounts[1], true)
		}
	})
	b.ok(exitOK, "disk", "remove", "t3", "--yes")
	b.setAt(nil)
	if b.configured("t3") {
		t.Error("t3 still listed")
	}
	if got := b.diskStates(); got != "t1 online, t2 missing" {
		t.Errorf("disks once t3 is out: %s", got)
	}
	if got := b.read(1, "Films/a.mkv") + ", " + b.read(3, "Films/a.mkv"); got != "a film, a film" {
		t.Errorf("t1 and t3 hold %q", got)
	}
}

// failedWithACopyPlanned makes a file to replicate from t1 to t2, every rename on t2 failing, so that the copy there
// stays planned, then declares t2 failed: what nothing ends, as a failed disk is never written again.
func failedWithACopyPlanned(t *testing.T) *poolBench {
	t.Helper()
	b := newPoolBench(t, 2, "[defaults]\nreplicas = 1\n")
	b.write(1, "Films/a.mkv", "a film")
	b.ok(exitOK, "init")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.mu.Lock()
	b.machine.fail = func(mount, call, _ string) error {
		if mount == b.mounts[1] && (call == "rename" || call == "link" || call == "plainrename") {
			return syscall.EIO
		}
		return nil
	}
	b.machine.mu.Unlock()
	b.machine.advance(time.Hour)
	b.atoll("daemon", "--until-idle")
	if planned, _ := b.json(exitOK, "disk", "show", "t2")["planned_operations"].(float64); planned == 0 {
		t.Fatal("no operation left planned on t2 by a copy that failed there")
	}
	b.json(exitOK, "disk", "fail", "t2", "--yes")
	b.machine.mu.Lock()
	b.machine.fail = nil
	b.machine.mu.Unlock()
	b.machine.advance(time.Hour)
	b.atoll("daemon", "--until-idle")
	return b
}

// Flaw 30 of the TLA+ models, fixed with failed-remove-drops (conformance/models/README.md): a failed disk with an
// operation planned on it is taken out, the operation undone in the catalog alone, as nothing reads or writes the disk
// again; nothing is left planned after it.
func TestAFailedDiskWithAnOperationPlannedIsTakenOut(t *testing.T) {
	b := failedWithACopyPlanned(t)
	out := b.json(exitOK, "disk", "remove", "t2", "--yes")
	if out["done"] != true || out["planned_operations"] != float64(1) || b.configured("t2") {
		t.Fatalf("disk remove of a failed disk with an operation planned: %v", out)
	}
	b.machine.advance(time.Hour)
	if code, stdout, stderr := b.atoll("daemon", "--until-idle"); code != exitOK {
		t.Errorf("a pass once the failed disk is out: exit %d, an operation left planned\n%s%s", code, stdout, stderr)
	}
}

// failed-remove-drops: disk remove stopped, as kill -9 would, once it dropped what is planned on the failed disk and
// before the configuration is edited, leaves the disk listed, failed, with nothing planned; run again, it goes.
func TestARemovalOfAFailedDiskStoppedAfterTheDrop(t *testing.T) {
	b := failedWithACopyPlanned(t)
	if !b.stoppedAt("remove-dropped", "disk", "remove", "t2", "--yes") {
		t.Fatal("disk remove never reached remove-dropped")
	}
	shown := b.json(exitOK, "disk", "show", "t2")
	if shown["configured"] != "failed" || shown["planned_operations"] != float64(0) {
		t.Fatalf("stopped once the operations were dropped: %v", shown)
	}
	b.json(exitOK, "disk", "remove", "t2", "--yes")
	if b.configured("t2") {
		t.Error("t2 still listed")
	}
}
