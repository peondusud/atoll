package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// newCopiedPool is a pool of two disks holding Films/a.mkv and Films/b.mkv on each, verified, with keys of [view] and
// more tables added to its configuration; its copies are due for a scrub.
func newCopiedPool(t *testing.T, view, more string) *poolBench {
	t.Helper()
	b := newPoolBenchViewing(t, 2, view, more)
	b.write(1, "Films/a.mkv", "a film, long enough to be read whole")
	b.write(1, "Films/b.mkv", "another film")
	b.ok(exitOK, "init")
	b.ok(exitOK, "scan", "--full")
	b.ok(exitOK, "replicate", "Films", "--now")
	b.ok(exitOK, "scan", "--full")
	b.machine.advance(31 * 24 * time.Hour)
	return b
}

// unreadable makes every read of Films/a.mkv on t2 fail with EIO, through any handle, on a disk its check confirms:
// the chunk's fault, a corruption (invariant 4); once written over, as a repair writes it, the sector reads again.
func (b *poolBench) unreadable() {
	b.machine.mu.Lock()
	defer b.machine.mu.Unlock()
	var written atomic.Bool
	b.machine.fail = func(mount, call, name string) error {
		if mount != b.mounts[1] || !strings.HasSuffix(name, "a.mkv") {
			return nil
		}
		switch call {
		case "write":
			written.Store(true)
		case "read", "readback":
			if !written.Load() {
				return syscall.EIO
			}
		}
		return nil
	}
}

func (b *poolBench) readable() {
	b.machine.mu.Lock()
	defer b.machine.mu.Unlock()
	b.machine.fail = nil
}

// paths gives the paths of a list of a disk's output, as the scrub's --json gives it.
func paths(t *testing.T, out map[string]any, disk, list string) []string {
	t.Helper()
	var got []string
	for _, d := range out["disks"].([]any) {
		d := d.(map[string]any)
		if d["id"] != disk {
			continue
		}
		for _, p := range d[list].([]any) {
			got = append(got, p.(map[string]any)["path"].(string))
		}
	}
	return got
}

// A scrub finds the chunk of a copy that cannot be read, and repairs it in place from the other copy, as
// integrity.auto_repair asks by default (specs.md §16).
func TestAScrubRepairsAChunkThatCannotBeRead(t *testing.T) {
	b := newCopiedPool(t, "", "")
	b.json(exitOK, "scrub", "--dry-run")
	b.unreadable()
	out := b.json(exitOK, "scrub", "--yes")
	b.readable()
	if got := paths(t, out, "t2", "corrupt"); !slices.Equal(got, []string{"Films/a.mkv"}) {
		t.Errorf("corrupt: %q", got)
	}
	if got := paths(t, out, "t2", "repaired"); !slices.Equal(got, []string{"Films/a.mkv"}) {
		t.Errorf("repaired: %q", got)
	}
	if got := b.read(2, "Films/a.mkv"); got != "a film, long enough to be read whole" {
		t.Errorf("t2 holds %q", got)
	}
}

// Without integrity.auto_repair, a scrub sends the corrupt copy to its disk's trash, quarantined, and the file waits
// for the copy a replicate makes again from the other; atoll repair then finds no corrupt copy there (specs.md §16,
// §16.1, §17).
func TestAScrubWithoutAutoRepairQuarantines(t *testing.T) {
	b := newCopiedPool(t, "", "[integrity]\nauto_repair = false\n")
	b.unreadable()
	out := b.json(exitOK, "scrub", "--yes")
	b.readable()
	if got := paths(t, out, "t2", "corrupt"); !slices.Equal(got, []string{"Films/a.mkv"}) {
		t.Errorf("corrupt: %q", got)
	}
	if got := paths(t, out, "t2", "quarantined"); !slices.Equal(got, []string{"Films/a.mkv"}) {
		t.Errorf("quarantined: %q", got)
	}
	if got := paths(t, out, "t2", "repaired"); len(got) != 0 {
		t.Errorf("repaired without auto_repair: %q", got)
	}
	if got := b.read(2, "Films/a.mkv"); got != "absent" {
		t.Errorf("t2 still holds %q at its path", got)
	}
	if out := b.ok(exitOK, "status", "--path", "Films/a.mkv"); !strings.Contains(out, "pending") {
		t.Errorf("status of a file quarantined:\n%s", out)
	}
	trash := b.json(exitOK, "trash", "list")
	if entries, _ := trash["entries"].([]any); len(entries) != 1 {
		t.Errorf("trash: %v", trash)
	}
	if out := b.ok(exitOK, "repair", "Films/a.mkv"); !strings.Contains(out, "no corrupt copy") {
		t.Errorf("repair:\n%s", out)
	}
	b.json(exitOK, "replicate", "Films/a.mkv", "--now")
	if got := b.read(2, "Films/a.mkv"); got != "a film, long enough to be read whole" {
		t.Errorf("t2 holds %q once made again", got)
	}
}

// Files deleted on one disk go, at the daemon's pass once its debounce is over, to the trash of the other; one comes
// back from there, the other is purged (specs.md §15.12, §17); a rebuild of the catalog then finds the pool as it is.
func TestADeletionThroughTheTrashAndBack(t *testing.T) {
	b := newCopiedPool(t, "", "[deletes.mass_guard]\npercent = \"100%\"\n")
	for _, name := range []string{"a.mkv", "b.mkv"} {
		if err := os.Remove(filepath.Join(b.mounts[0], "media", "Films", name)); err != nil {
			t.Fatal(err)
		}
	}
	if out := b.json(exitOK, "daemon", "--until-idle"); out["waiting"] != float64(2) {
		t.Errorf("before its debounce: %v", out)
	}
	b.machine.advance(time.Minute)
	b.json(exitOK, "daemon", "--until-idle")
	if got := b.read(2, "Films/a.mkv") + ", " + b.read(2, "Films/b.mkv"); got != "absent, absent" {
		t.Fatalf("t2 still holds %q", got)
	}
	listed := b.json(exitOK, "trash", "list")
	if entries, _ := listed["entries"].([]any); len(entries) != 2 {
		t.Fatalf("trash: %v", listed)
	}
	if out := b.ok(exitOK, "trash", "list"); !strings.Contains(out, "Films/a.mkv") || !strings.Contains(out, "Films/b.mkv") {
		t.Errorf("trash list:\n%s", out)
	}
	b.ok(exitOK, "trash", "restore", "Films/a.mkv", "--dry-run")
	b.ok(exitConfirm, "trash", "restore", "Films/a.mkv")
	b.json(exitOK, "trash", "restore", "Films/a.mkv", "--yes")
	if got := b.read(2, "Films/a.mkv"); got != "a film, long enough to be read whole" {
		t.Errorf("t2 holds %q once restored", got)
	}
	b.ok(exitOK, "trash", "purge", "--older-than", "0s", "--dry-run")
	b.ok(exitConfirm, "trash", "purge", "--older-than", "0s")
	purged := b.json(exitOK, "trash", "purge", "--older-than", "0s", "--yes")
	if entries, _ := purged["entries"].([]any); len(entries) != 1 {
		t.Errorf("purge: %v", purged)
	}
	if listed := b.json(exitOK, "trash", "list"); len(listed["entries"].([]any)) != 0 {
		t.Errorf("trash once purged: %v", listed)
	}
	b.ok(exitOK, "scan")
	b.json(exitOK, "catalog", "rebuild", "--dry-run")
	if out := b.json(exitOK, "catalog", "rebuild", "--yes"); out["done"] != true {
		t.Errorf("rebuild: %v", out)
	}
	b.json(exitOK, "catalog", "verify")
}
