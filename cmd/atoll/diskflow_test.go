package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spare is a disk the machine has mounted, healthy, that the pool's configuration does not name yet: the third
// identifier of poolUUIDs.
func (b *poolBench) spare(id string) string {
	b.t.Helper()
	mount := filepath.Join(b.dir, "disks", id)
	mustMkdir(b.t, filepath.Join(mount, "media"))
	b.machine.mu.Lock()
	defer b.machine.mu.Unlock()
	b.machine.uuid[mount] = poolUUIDs[2]
	return mount
}

// diskStates gives each disk's id and state, as disk list --json says them, in its order.
func (b *poolBench) diskStates() string {
	b.t.Helper()
	var parts []string
	for _, d := range b.json(exitOK, "disk", "list")["disks"].([]any) {
		d := d.(map[string]any)
		parts = append(parts, d["id"].(string)+" "+d["state"].(string))
	}
	return strings.Join(parts, ", ")
}

// A disk declared failed is never read again, and its copies are made again elsewhere by the next pass; once taken
// out, the pool goes on without it (specs.md §18, §25).
func TestADiskFailedThenRemoved(t *testing.T) {
	b := newPoolBench(t, 3, "[defaults]\nreplicas = 2\n")
	b.write(1, "Films/a.mkv", "a film")
	b.ok(exitOK, "init")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.advance(time.Hour)
	b.ok(exitOK, "daemon", "--until-idle")
	if got := b.read(2, "Films/a.mkv") + ", " + b.read(3, "Films/a.mkv"); !strings.Contains(got, "a film") {
		t.Errorf("t2 and t3 hold %q", got)
	}
	b.json(exitConfirm, "disk", "fail", "t1")
	b.ok(exitOK, "disk", "fail", "t1", "--dry-run")
	b.json(exitOK, "disk", "fail", "t1", "--yes")
	b.ok(exitOK, "disk", "fail", "t1", "--yes")
	b.ok(exitOK, "daemon", "--until-idle")
	if got := b.read(2, "Films/a.mkv") + ", " + b.read(3, "Films/a.mkv"); got != "a film, a film" {
		t.Errorf("t2 and t3 hold %q", got)
	}
	if out := b.ok(exitOK, "disk", "show", "t1"); !strings.Contains(out, "failed") {
		t.Errorf("disk show t1:\n%s", out)
	}
	if code, _, _ := b.atoll("disk", "remove", "t1", "--evacuate"); code != exitError {
		t.Errorf("a failed disk evacuated: exit %d", code)
	}
	b.ok(exitConfirm, "disk", "remove", "t1")
	b.json(exitOK, "disk", "remove", "t1", "--yes")
	if got := b.diskStates(); got != "t2 online, t3 online" {
		t.Errorf("disks: %s", got)
	}
	if got := b.read(1, "Films/a.mkv"); got != "a film" {
		t.Errorf("a disk taken out lost its file: %q", got)
	}
}

// A disk evacuated has its copies made elsewhere before it goes; a disk replaced brings the new one in, and its
// copies are made on the disks that count (specs.md §18, §25).
func TestADiskEvacuatedAndOneReplaced(t *testing.T) {
	b := newPoolBench(t, 2, "")
	b.write(1, "Films/a.mkv", "a film")
	b.write(2, "Series/b.mkv", "a series")
	b.ok(exitOK, "init")
	b.ok(exitOK, "daemon", "--until-idle")
	spare := b.spare("t3")
	b.mounts = append(b.mounts, spare)
	for _, args := range [][]string{
		{"disk", "add", spare, "--id", "t1"}, {"disk", "add", b.mounts[0], "--id", "t9"}, {"disk", "add", filepath.Join(b.dir, "disks", "t4")},
	} {
		if code, _, _ := b.atoll(args...); code != exitError {
			t.Errorf("atoll %s: exit %d", strings.Join(args, " "), code)
		}
	}
	if out := b.ok(exitOK, "disk", "add", spare, "--dry-run"); !strings.Contains(out, "t3") {
		t.Errorf("disk add --dry-run:\n%s", out)
	}
	b.json(exitOK, "disk", "add", spare, "--dry-run")
	b.json(exitOK, "disk", "add", spare, "--id", "t3", "--domain", "shelf", "--tags", "big")
	if got := b.diskStates(); got != "t1 online, t2 online, t3 online" {
		t.Errorf("disks once one is added: %s", got)
	}
	b.ok(exitOK, "daemon", "--until-idle")
	b.json(exitOK, "disk", "remove", "t2", "--evacuate", "--dry-run")
	b.json(exitOK, "disk", "remove", "t2", "--evacuate", "--yes")
	b.ok(exitOK, "daemon", "--until-idle")
	b.machine.advance(time.Hour)
	b.ok(exitOK, "daemon", "--until-idle")
	b.ok(exitOK, "disk", "remove", "t2", "--yes")
	if got := b.read(1, "Series/b.mkv") + ", " + b.read(3, "Series/b.mkv"); !strings.Contains(got, "a series") {
		t.Errorf("t1 and t3 hold %q once t2 is evacuated", got)
	}
	if code, _, _ := b.atoll("disk", "show", "t2"); code != exitError {
		t.Errorf("disk show of a disk taken out: exit %d", code)
	}
	code, stdout, stderr := b.atoll("disk", "replace", "t1", "--with", filepath.Join(b.dir, "disks", "t4"), "--json")
	t.Logf("disk replace t1 with a disk the machine lacks: %d\n%s%s", code, stdout, stderr)
	if code == exitOK {
		t.Error("a disk replaced by one not mounted")
	}
	b.json(exitOK, "status")
	b.json(exitOK, "capacity")
}
