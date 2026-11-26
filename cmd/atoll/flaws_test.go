//go:build flaws

package main

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// Flaw 29 of the TLA+ models (conformance/models/README.md, Disks.tla): disk remove counts what depends on a disk from
// the catalog alone. A user's edit made on a drained disk, directly or through mergerfs, which writes to the copy its
// branch holds, is not in the catalog until a pass takes it in, and the disk goes with it: the pool keeps the version
// before. The test passes once the flaw is fixed, whatever the remedy: the disk stays, or the edit is in the pool.
func TestFlawAnEditOnADrainedDiskStaysInThePool(t *testing.T) {
	b := newPoolBench(t, 3, "[defaults]\nreplicas = 1\n")
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
	t.Logf("disk remove t1 --yes, the edit not taken in: exit %d\n%s%s", code, stdout, stderr)
	if code != exitOK {
		return
	}
	b.machine.advance(time.Hour)
	b.atoll("daemon", "--until-idle")
	if got := b.read(2, "Films/a.mkv") + ", " + b.read(3, "Films/a.mkv"); !strings.Contains(got, "the user's edit") {
		t.Errorf("t1 taken out with the user's edit, which the pool no longer holds: t2 and t3 hold %q", got)
	}
}

// Flaw 29 through a disk gone: the file x1, a user's copy on t1 and t2, t2's the surplus one, as the plan keeps the
// first disk's (solver.compareKeep). With t1's mount gone and no pass since, disk remove takes t2 out, as a surplus
// copy makes no file depend on its disk (specs.md §25): the file is left with no copy the pool can read.
func TestFlawASurplusCopyTakenOutWhileTheKeptOneIsGone(t *testing.T) {
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
	code, stdout, stderr := b.atoll("disk", "remove", "t2", "--yes")
	t.Logf("disk remove t2 --yes, t1 not mounted: exit %d\n%s%s", code, stdout, stderr)
	if code == exitOK {
		t.Error("t2 taken out while the file's only other copy is on a disk not mounted")
	}
}

// Flaw 30 of the TLA+ models (conformance/models/README.md, Disks.tla): a disk lost while a copy to it is planned,
// then declared failed, can never be taken out. Nothing reads or writes a failed disk again, so what is planned there
// stays planned, and disk remove refuses a disk with an operation planned on it. The test passes once the flaw is
// fixed, whatever the remedy: the failed disk can be taken out.
func TestFlawAFailedDiskWithAnOperationPlannedCanBeTakenOut(t *testing.T) {
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
	shown := b.json(exitOK, "disk", "show", "t2")
	if planned, _ := shown["planned_operations"].(float64); planned == 0 {
		t.Fatalf("no operation left planned on t2 by a copy that failed there: %v", shown)
	}
	b.json(exitOK, "disk", "fail", "t2", "--yes")
	b.machine.mu.Lock()
	b.machine.fail = nil
	b.machine.mu.Unlock()
	b.machine.advance(time.Hour)
	b.atoll("daemon", "--until-idle")
	code, stdout, stderr := b.atoll("disk", "remove", "t2", "--yes")
	if code != exitOK {
		t.Errorf("a failed disk is not taken out: exit %d\n%s%s", code, stdout, stderr)
	}
}
