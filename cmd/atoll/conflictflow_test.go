package main

import (
	"strings"
	"testing"
)

// Two versions of a file at its path on two disks are a conflict, listed in text and in JSON; atoll conflicts resolve
// keeps the copy of the disk named, said first without --yes and with --dry-run, and sends the other to its disk's
// trash with --yes; the replica is then made from the kept copy (specs.md §15.10, §25).
func TestAConflictListedAndResolved(t *testing.T) {
	b := newPoolBench(t, 2, "")
	b.write(1, "Films/a.mkv", "a film")
	b.write(2, "Films/a.mkv", "A FILM, the other version")
	b.ok(exitOK, "init")
	b.ok(exitOK, "scan", "--full")
	if out := b.ok(exitOK, "conflicts", "list"); !strings.Contains(out, "Films/a.mkv") {
		t.Errorf("conflicts list:\n%s", out)
	}
	listed := b.json(exitOK, "conflicts", "list")
	t.Logf("conflicts list --json: %v", listed)
	b.json(exitConfirm, "conflicts", "resolve", "Films/a.mkv", "--keep", "t2")
	b.json(exitOK, "conflicts", "resolve", "Films/a.mkv", "--keep=t2", "--dry-run")
	if got := b.read(1, "Films/a.mkv"); got != "a film" {
		t.Errorf("t1 holds %q after a dry run", got)
	}
	if code, _, _ := b.atoll("conflicts", "resolve", "Films/a.mkv", "--keep", "t9", "--yes"); code != exitError {
		t.Errorf("a disk the pool lacks kept: exit %d", code)
	}
	b.ok(exitOK, "conflicts", "resolve", "Films/a.mkv", "--keep", "t2", "--yes")
	if got := b.read(1, "Films/a.mkv"); got != "absent" {
		t.Errorf("t1 still holds %q", got)
	}
	if out := b.ok(exitOK, "conflicts", "list"); strings.Contains(out, "Films/a.mkv") {
		t.Errorf("conflicts list once resolved:\n%s", out)
	}
	if code, _, _ := b.atoll("conflicts", "resolve", "Films/a.mkv", "--keep", "t2", "--yes"); code != exitError {
		t.Errorf("a conflict resolved twice: exit %d", code)
	}
	b.json(exitOK, "replicate", "Films/a.mkv", "--now")
	if got := b.read(1, "Films/a.mkv"); got != "A FILM, the other version" {
		t.Errorf("t1 holds %q once the replica is made", got)
	}
	if entries := b.json(exitOK, "trash", "list")["entries"].([]any); len(entries) != 1 {
		t.Errorf("trash: %v", entries)
	}
}
