package trash

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Turned off once its links went by other hands, the guard has nothing to remove but the folder, and nothing is left
// unnamed (specs.md §17).
func TestTheGuardOffWithItsLinksGone(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	if err := os.Remove(b.linkOf("t3", 1)); err != nil {
		t.Fatal(err)
	}
	if items, lost := b.guarded("t3", false); lost || len(items) != 0 {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	if _, err := os.Lstat(filepath.Dir(b.linkOf("t3", 1))); !os.IsNotExist(err) {
		t.Errorf("the links folder: %v", err)
	}
	if got := b.count("SELECT links_unnamed FROM disks WHERE id = 't3'"); got != 0 {
		t.Errorf("%d links unnamed", got)
	}
}

// What the links folder holds that Atoll did not make is never removed, the guard on or off: the folder stays with it.
func TestAStrayInTheLinksFolderStays(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	stray := filepath.Join(filepath.Dir(b.linkOf("t3", 1)), "notes.txt")
	if err := os.WriteFile(stray, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{true, false} {
		if items, lost := b.guarded("t3", on); lost || len(items) != 0 {
			t.Fatalf("on %v: %+v, lost %v", on, items, lost)
		}
		if _, err := os.Stat(stray); err != nil {
			t.Errorf("on %v: %v", on, err)
		}
	}
	if inodeOf(t, b.linkOf("t3", 1)) != 0 {
		t.Error("the guard off left its own link")
	}
}

// What the guard cannot read or make waits for the next pass, the disk kept; a disk that fails there is lost (specs.md
// §17).
func TestTheGuardWaitsForWhatItCannotReach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(b *bench)
		call   string
		target string
		err    syscall.Errno
		lost   bool
	}{
		{name: "a links folder that cannot be read", before: func(b *bench) { b.guarded("t3", true) }, call: "opendir", target: "links", err: syscall.EACCES},
		{name: "a links folder on a disk that fails", before: func(b *bench) { b.guarded("t3", true) }, call: "opendir", target: "links", err: syscall.EIO, lost: true},
		{name: "a links folder that cannot be made", call: "mkdir", target: "links", err: syscall.EACCES},
		{name: "a file that cannot be looked at", before: func(b *bench) { b.lostItsName() }, call: "stat", target: "a", err: syscall.EACCES},
		{name: "a file on a disk that fails", before: func(b *bench) { b.lostItsName() }, call: "stat", target: "a", err: syscall.EIO, lost: true},
		{name: "a folder of links that cannot be synced", call: "sync", target: "links", err: syscall.EIO, lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			if tc.before != nil {
				tc.before(b)
			}
			before := b.count("SELECT count(*) FROM ops")
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				if call == tc.call && name == tc.target {
					return tc.err
				}
				return nil
			}
			items, lost := b.guarded("t3", true)
			if lost != tc.lost || len(items) != 0 {
				t.Errorf("%+v, lost %v", items, lost)
			}
			if lost != (b.diskState("t3") == "missing") {
				t.Errorf("t3 is %s", b.diskState("t3"))
			}
			if !tc.lost && b.count("SELECT count(*) FROM ops WHERE state = 'planned'") != 0 {
				t.Error("an operation stays planned")
			}
			// The next pass, nothing failing, ends where one with no failure would.
			delete(b.fail, b.dirs["t3"])
			if _, lost := b.guarded("t3", true); lost {
				t.Fatal("lost again")
			}
			if b.count("SELECT count(*) FROM ops WHERE state = 'planned'") != 0 || b.count("SELECT count(*) FROM ops") < before {
				t.Error("an operation stays planned")
			}
		})
	}
}

// A run that may not write leaves the links as they are, and the guard turned off on a disk it never wrote to, by its
// journal, opens nothing of it (specs.md §17).
func TestTheGuardWithNothingToDo(t *testing.T) {
	b := newBench(t)
	b.confirm = map[string]int{}
	if items, lost := b.guarded("t3", false); lost || len(items) != 0 || b.confirm[b.dirs["t3"]] != 0 {
		t.Errorf("off on a disk it never wrote to: %+v, lost %v, confirmed %d times", items, lost, b.confirm[b.dirs["t3"]])
	}
	dry, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dry.Close()
	if err := dry.Guard(t.Context(), "t3", true, nil); err != nil {
		t.Error(err)
	}
	if _, err := os.Lstat(filepath.Dir(b.linkOf("t3", 1))); !os.IsNotExist(err) || b.count("SELECT count(*) FROM ops") != 0 {
		t.Errorf("a dry run made the links folder: %v", err)
	}
}

// What the caller's function returns stops the guard, with it.
func TestTheGuardStops(t *testing.T) {
	b := newBench(t)
	b.lostItsName()
	s := b.opened()
	stop := errors.New("stop")
	if err := s.Guard(t.Context(), "t3", true, func(*LinkItem) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("%v", err)
	}
}

// A purge that cannot look at whether links lead to its entry, on a disk that fails, loses the disk and purges nothing:
// the links go first, and nothing of the purged data may stay behind one (specs.md §17).
func TestAPurgeWhoseLinksCannotBeLookedFor(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	b.prune()
	// The entry's folder in the trash, the operation's.
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "opendir" && name == "2" {
			return syscall.EIO
		}
		return nil
	}
	outcomes, summary := b.purgedAll()
	if len(outcomes) != 1 || outcomes[0] != OutcomeIOError || !summary.DiskLost {
		t.Fatalf("%v, %+v", outcomes, summary)
	}
	if _, err := os.Stat(b.inTrash(2, "a")); err != nil || b.count("SELECT count(*) FROM trash") != 1 {
		t.Errorf("the entry: %v", err)
	}
	delete(b.fail, b.dirs["t3"])
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomePurged {
		t.Errorf("once readable: %v", outcomes)
	}
	if inodeOf(t, b.linkOf("t3", 1)) != 0 {
		t.Error("the link stays")
	}
}

// A purge that cannot read the links folder purges nothing of its entry: a link there may keep the entry's data, which
// would stay behind it once purged, and go back to the trash at the next pass. A folder that fails on a disk that fails
// loses the disk, the entry left for later (invariant 4); any other error leaves the entry for the next purge, the disk
// kept, and so does another mount over the folder, which hides the links below it (specs.md §17).
func TestAPurgeThatCannotReadTheLinksFolder(t *testing.T) {
	failing := func(call string, err syscall.Errno) func(b *bench) {
		return func(b *bench) {
			b.fail[b.dirs["t3"]] = func(c, name string) error {
				if c == call && name == "links" {
					return err
				}
				return nil
			}
		}
	}
	for _, tc := range []struct {
		name string
		fail func(b *bench)
		lost bool
	}{
		{"read on a disk that fails", failing("names", syscall.EIO), true},
		{"opened on a disk gone", failing("opendir", syscall.ENODEV), true},
		{"read with no right to", failing("names", syscall.EACCES), false},
		{"opened with no file left to open", failing("opendir", syscall.EMFILE), false},
		{"another mount over it", func(b *bench) { b.otherMount[b.dirs["t3"]] = "links" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			if _, lost := b.guarded("t3", true); lost {
				t.Fatal("lost")
			}
			b.prune()
			tc.fail(b)
			outcomes, summary := b.purgedAll()
			if len(outcomes) != 1 || outcomes[0] != OutcomeIOError || summary.DiskLost != tc.lost {
				t.Fatalf("%v, %+v", outcomes, summary)
			}
			if tc.lost != (b.diskState("t3") == "missing") {
				t.Errorf("t3 is %s", b.diskState("t3"))
			}
			if _, err := os.Stat(b.inTrash(2, "a")); err != nil || b.count("SELECT count(*) FROM trash") != 1 {
				t.Errorf("the entry: %v", err)
			}
			if inodeOf(t, b.linkOf("t3", 1)) == 0 {
				t.Error("the link went")
			}
			if b.count("SELECT count(*) FROM ops WHERE kind = 'unlink'") != 0 {
				t.Error("an unlink was journaled")
			}
			delete(b.fail, b.dirs["t3"])
			delete(b.otherMount, b.dirs["t3"])
			if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomePurged {
				t.Errorf("once readable: %v", outcomes)
			}
			if inodeOf(t, b.linkOf("t3", 1)) != 0 {
				t.Error("the link stays")
			}
			if b.count("SELECT count(*) FROM trash") != 0 || b.count("SELECT count(*) FROM ops WHERE state = 'planned'") != 0 {
				t.Error("the purge did not end")
			}
		})
	}
}
