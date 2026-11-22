package main

import (
	"strings"
	"testing"
)

// A pool of two disks through the commands that look at it and the copies they make, each in the test's process on
// disks as folders, as the scenarios run them in a conformance build (specs.md §25): init, a scan, the status, the
// copies a replicate makes, then what reads the catalog and the disks again.
func TestAPoolThroughItsCommands(t *testing.T) {
	b := newPoolBench(t, 2, "")
	b.write(1, "Films/a.mkv", "a film")
	b.write(1, "Films/b.mkv", "another film")
	b.write(2, "Series/c.mkv", "a series")
	b.ok(exitOK, "init")
	scanned := b.json(exitOK, "scan", "--full")
	if list, _ := scanned["disks"].([]any); len(list) != 2 {
		t.Fatalf("scan: %v", scanned)
	}
	if out := b.ok(exitOK, "scan"); !strings.Contains(out, "t1: online") || !strings.Contains(out, "t2: online") {
		t.Errorf("scan:\n%s", out)
	}
	if out := b.ok(exitOK, "scan", "--disk", "t2"); strings.Contains(out, "t1:") {
		t.Errorf("scan of t2:\n%s", out)
	}
	if code, _, stderr := b.atoll("scan", "--disk", "t9"); code != exitUsage && code != exitError {
		t.Errorf("a scan of a disk the pool lacks: %d, %s", code, stderr)
	}

	b.json(exitOK, "status")
	if out := b.ok(exitOK, "status"); !strings.Contains(out, "t1") {
		t.Errorf("status:\n%s", out)
	}
	b.json(exitOK, "status", "--path", "Films")
	b.ok(exitOK, "status", "--path", "Films/a.mkv")

	// Each file gets a copy on the other disk.
	b.json(exitOK, "replicate", "Films", "--dry-run")
	b.ok(exitOK, "replicate", "Films", "--now")
	b.json(exitOK, "replicate", "Series", "--now")
	for disk, path := range map[int]string{2: "Films/a.mkv", 1: "Series/c.mkv"} {
		if got, want := b.read(disk, path), map[string]string{"Films/a.mkv": "a film", "Series/c.mkv": "a series"}[path]; got != want {
			t.Errorf("t%d's %s: %q", disk, path, got)
		}
	}
	b.ok(exitOK, "scan")

	// What reads the catalog, and the disks again.
	b.json(exitOK, "scrub", "--dry-run")
	b.json(exitOK, "scrub", "--yes")
	b.ok(exitOK, "scrub", "--yes", "--disk", "t1", "--path", "Films")
	b.json(exitOK, "dupes")
	b.ok(exitOK, "dupes", "--min-size", "1B")
	b.json(exitOK, "capacity")
	b.ok(exitOK, "capacity")
	b.json(exitOK, "conflicts", "list")
	b.ok(exitOK, "conflicts", "list")
	b.json(exitOK, "names", "check")
	b.ok(exitOK, "names", "check", "--path", "Films")
	b.json(exitOK, "rules", "check")
	b.ok(exitOK, "rules", "explain", "Films/a.mkv")
	b.json(exitOK, "rules", "explain", "Films/z.mkv", "--size", "1GB")
	b.json(exitOK, "trash", "list")
	b.ok(exitOK, "trash", "list")
	b.json(exitOK, "prune", "--dry-run")
	b.json(exitOK, "balance", "--dry-run")
	b.json(exitOK, "guard", "list")
	b.ok(exitOK, "guard", "list")
	b.json(exitOK, "catalog", "verify")
	b.ok(exitOK, "catalog", "verify")
	b.json(exitOK, "catalog", "backup")
	if out := b.ok(exitOK, "catalog", "backup"); !strings.HasPrefix(out, "backup ") {
		t.Errorf("catalog backup:\n%s", out)
	}
	b.ok(exitConfirm, "catalog", "rebuild")
	b.json(exitOK, "disk", "list")
	b.ok(exitOK, "disk", "show", "t1")
	b.json(exitOK, "disk", "show", "t2")
	b.ok(exitOK, "mergerfs-config")
	b.json(exitOK, "mergerfs-config", "--omv")

	// t2 taken away: an empty mount point is a missing disk, never one whose files went (invariant 4).
	b.machine.setAway(b.mounts[1], true)
	if got := b.diskStates(); got != "t1 online, t2 missing" {
		t.Errorf("disks with t2 away: %s", got)
	}
	if code, _, stderr := b.atoll("scan"); code != exitOK && code != exitPartial {
		t.Errorf("scan with t2 away: exit %d, %s", code, stderr)
	}
	if out := b.ok(exitOK, "status", "--path", "Series/c.mkv"); !strings.Contains(out, "Series/c.mkv") {
		t.Errorf("status of a file whose disk is away:\n%s", out)
	}
	b.machine.setAway(b.mounts[1], false)
	if got := b.diskStates(); got != "t1 online, t2 online" {
		t.Errorf("disks with t2 back: %s", got)
	}
}

// What a command does not take is a usage error, exit 2, said on standard error, with nothing on standard output and
// nothing done (specs.md §25, §29.3).
func TestEachCommandRefusesWhatItDoesNotTake(t *testing.T) {
	b := newPoolBench(t, 1, "")
	b.ok(exitOK, "init")
	for _, args := range [][]string{
		{"scan", "--bogus"}, {"scan", "--disk"}, {"scan", "--full", "--full"}, {"scan", "x"},
		{"status", "--path"}, {"status", "--bogus"},
		{"replicate"}, {"replicate", "a", "b"}, {"replicate", "a", "--now", "--force"}, {"replicate", "a", "--bogus"},
		{"scrub", "--budget", "soon"}, {"scrub", "--disk"}, {"scrub", "--path"}, {"scrub", "x"},
		{"repair"}, {"repair", "a", "--mode", "half"}, {"repair", "a", "--disk"}, {"repair", "a", "b"},
		{"dupes", "--min-size", "big"}, {"dupes", "--path"}, {"dupes", "x"},
		{"trash"}, {"trash", "empty"}, {"trash", "restore"}, {"trash", "restore", "a", "--to"}, {"trash", "purge", "--older-than", "soon"},
		{"trash", "list", "x"},
		{"names"}, {"names", "fix"}, {"names", "check", "--path"},
		{"guard"}, {"guard", "confirm"}, {"guard", "confirm", "--disk", "t1", "--kind", "renames"}, {"guard", "list", "x"},
		{"balance", "--bogus"}, {"balance", "x"},
		{"capacity", "x"}, {"capacity", "--bogus"},
		{"prune", "x"}, {"prune", "--bogus"},
		{"conflicts"}, {"conflicts", "resolve", "a"}, {"conflicts", "resolve", "a", "--keep"}, {"conflicts", "list", "x"},
		{"catalog"}, {"catalog", "mend"}, {"catalog", "verify", "x"}, {"catalog", "backup", "--bogus"}, {"catalog", "rebuild", "x"},
		{"rules"}, {"rules", "explain"}, {"rules", "explain", "a", "--size", "big"}, {"rules", "check", "x"},
		{"disk"}, {"disk", "show"}, {"disk", "list", "x"}, {"disk", "remove"}, {"disk", "fail"}, {"disk", "replace", "t1"},
		{"disk", "add"}, {"disk", "add", "relative"}, {"disk", "add", "/a", "/b"}, {"disk", "add", "/a", "--id"},
		{"disk", "add", "/a", "--id", "not one"}, {"disk", "add", "/a", "--domain", "not one"}, {"disk", "add", "/a", "--tags", "a,a"},
		{"disk", "add", "/a", "--id", "x", "--id=y"}, {"disk", "add", "/a", "--bogus"}, {"disk", "add", "/a", "--dry-run", "--dry-run"},
		{"disk", "add", "/a", "--json", "--json"},
		{"mergerfs-config", "x"}, {"polkit-rule", "x"}, {"notify"}, {"notify", "send"},
		{"daemon", "--bogus"}, {"daemon", "--json"},
	} {
		code, stdout, stderr := b.atoll(args...)
		if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "atoll: ") {
			t.Errorf("atoll %s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout, stderr)
		}
	}
}
