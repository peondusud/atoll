package trash

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/watch"
)

// filmsPruned gives a bench whose t3 sent Films/b to its trash, and whose t1 lost its Films folder behind Atoll's back:
// a restore of Films/b to t1 makes the folder, like the one in t3's trash.
func filmsPruned(t *testing.T) *bench {
	t.Helper()
	b := newBench(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "Films/b", "y")
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.prune()
	if err := os.RemoveAll(filepath.Join(b.dirs["t1"], "media", "Films")); err != nil {
		t.Fatal(err)
	}
	return b
}

// createState gives the state of the restore-to operations, by id.
func (b *bench) createState() string {
	b.t.Helper()
	return b.text("SELECT coalesce(group_concat(state, ','), '') FROM (SELECT state FROM ops WHERE kind = 'create' ORDER BY id)")
}

// restoredOnce restores a to t1, and says how it went, failing the test unless the outcome is want.
func (b *bench) restoredOnce(under, want string) *Summary {
	b.t.Helper()
	outcomes, summary := b.restoredTo(under, "t1", true)
	if len(outcomes) != 1 || outcomes[0] != want {
		b.t.Fatalf("%v, want %s", outcomes, want)
	}
	return summary
}

// An entry put back on its own disk is moved back there, not copied: it leaves the trash (specs.md §17.4).
func TestARestoreToItsOwnDiskMovesItBack(t *testing.T) {
	b := pruned(t)
	outcomes, _ := b.restoredTo("a", "t3", true)
	if len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%v", outcomes)
	}
	if b.read("t3", "a") != "x" || b.count("SELECT count(*) FROM trash") != 0 || b.createState() != "" {
		t.Errorf("t3 holds %q, the trash %d entries, copies %q", b.read("t3", "a"), b.count("SELECT count(*) FROM trash"), b.createState())
	}
}

// What stands at the path on the target, or a copy of that file there that still sits elsewhere, keeps the entry where
// it is, and nothing is journaled (specs.md §17.4).
func TestARestoreToAPathTaken(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.restoredOnce("a", OutcomePathTaken)
	if b.read("t1", "a") != "x" || b.createState() != "" {
		t.Error("the file at the path was touched")
	}
	b = pruned(t)
	b.exec("UPDATE copies SET old_lpath = x'7a' WHERE disk_id = 't1'")
	b.restoredOnce("a", OutcomePathTaken)
	if b.read("t1", "a") != "" || b.createState() != "" {
		t.Error("a copy was made where the file's copy on that disk is to come back")
	}
}

// A name the target's filesystem refuses, or one that collides there with another, is never copied: Windows would see
// one of the two (specs.md §15.7, §15.8).
func TestARestoreToAnNTFSDiskOfANameItRefuses(t *testing.T) {
	b := newBenchOf(t, "ntfs3")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "x:y", "z")
		b.write(id, "c", "z")
	}
	b.write("t3", "C", "w")
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	s := b.opened()
	for _, path := range []string{"x:y", "c"} {
		f, err := catalog.FileAt(t.Context(), b.db, path)
		if err != nil || f == nil {
			t.Fatalf("%s: %v", path, err)
		}
		if outcome, err := s.Send(t.Context(), f, copyOn(f, "t1"), "deleted"); err != nil || outcome != OutcomeTrashed {
			t.Fatalf("%s: %s, %v", path, outcome, err)
		}
	}
	s.Close()
	for path, want := range map[string]string{"x:y": OutcomeNameRefused, "c": OutcomeNameCollision} {
		outcomes, _ := b.restoredTo(path, "t3", true)
		if len(outcomes) != 1 || outcomes[0] != want {
			t.Errorf("%s: %v, want %s", path, outcomes, want)
		}
	}
	if b.createState() != "" || b.count("SELECT count(*) FROM trash") != 2 {
		t.Error("something was journaled, or left the trash")
	}
}

// A look at the target that fails: the disk is lost when it fails, and the entry stays otherwise; a trash that cannot
// be read leaves the entry unavailable.
func TestARestoreToWhoseLookFails(t *testing.T) {
	b := pruned(t)
	b.failOn("t1", "stat", syscall.EACCES)
	b.restoredOnce("a", OutcomeIOError)
	if b.diskState("t1") != "online" {
		t.Errorf("t1 is %s", b.diskState("t1"))
	}
	b.failOn("t1", "stat", syscall.EIO)
	if summary := b.restoredOnce("a", outcomeUnavailable); !summary.DiskLost || b.diskState("t1") != "missing" {
		t.Errorf("%+v, t1 is %s", summary, b.diskState("t1"))
	}
	b = pruned(t)
	b.unread[b.dirs["t3"]] = true
	b.restoredOnce("a", outcomeUnavailable)
	if b.createState() != "" || b.read("t1", "a") != "" {
		t.Error("copied from a trash that does not confirm")
	}
	// Nor to a disk the pool does not have.
	if outcomes, _ := b.restoredTo("a", "t9", true); len(outcomes) != 1 || outcomes[0] != outcomeUnavailable {
		t.Errorf("to a disk the pool does not have: %v", outcomes)
	}
}

// The part that cannot be made: nothing was made, so the operation is undone, unless the disk failed.
func TestARestoreToWhosePartCannotBeMade(t *testing.T) {
	b := pruned(t)
	b.failOn("t1", "create", syscall.ENOSPC)
	b.restoredOnce("a", OutcomeIOError)
	if b.createState() != "undone" || b.diskState("t1") != "online" || b.partsOn("t1") != 0 {
		t.Errorf("the operation is %s, t1 %s, %d parts", b.createState(), b.diskState("t1"), b.partsOn("t1"))
	}
	b.failOn("t1", "create", syscall.EIO)
	b.restoredOnce("a", OutcomeIOError)
	if b.createState() != "undone,planned" || b.diskState("t1") != "missing" {
		t.Errorf("the operations are %s, t1 %s", b.createState(), b.diskState("t1"))
	}
}

// The file of the trash that cannot be opened or read: a disk that fails there is the trash's, never the target's; the
// part goes and the operation is undone either way, and the entry stays.
func TestARestoreToWhoseEntryCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		call string
		err  syscall.Errno
		lost bool
	}{
		{"openfile", syscall.EIO, true},
		{"openfile", syscall.EACCES, false},
		{"read", syscall.EACCES, false},
	} {
		t.Run(tc.call+" "+tc.err.Error(), func(t *testing.T) {
			b := pruned(t)
			b.failOn("t3", tc.call, tc.err)
			b.restoredOnce("a", OutcomeIOError)
			if b.createState() != "undone" || b.partsOn("t1") != 0 || b.diskState("t1") != "online" {
				t.Errorf("the operation is %s, %d parts, t1 %s", b.createState(), b.partsOn("t1"), b.diskState("t1"))
			}
			if lost := b.diskState("t3") == "missing"; lost != tc.lost {
				t.Errorf("t3 is %s", b.diskState("t3"))
			}
			if b.count("SELECT count(*) FROM trash") != 1 {
				t.Error("the entry left the trash")
			}
		})
	}
}

// A file of the trash that changes while it is copied, shorter than when it was opened or another file once read, is
// no longer the entry: changed, with an op-undone event, and nothing reaches the target.
func TestARestoreToOfAnEntryThatChangesWhileCopied(t *testing.T) {
	b := pruned(t)
	shortened := false
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "read" && !shortened {
			shortened = true
			if err := os.Truncate(b.inTrash(1, "a"), 0); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	b.restoredOnce("a", OutcomeChanged)
	if b.createState() != "undone" || b.partsOn("t1") != 0 || b.read("t1", "a") != "" {
		t.Errorf("the operation is %s, %d parts", b.createState(), b.partsOn("t1"))
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events", got)
	}
	b = pruned(t)
	b.at["copied"] = func() { b.replace(b.inTrash(1, "a"), "x") }
	b.restoredOnce("a", OutcomeChanged)
	if b.createState() != "undone" || b.read("t1", "a") != "" {
		t.Errorf("the operation is %s", b.createState())
	}
	// A trash that fails at that look is the one lost.
	b = pruned(t)
	late := false
	b.at["copied"] = func() { late = true }
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if late && call == "stat" {
			return syscall.EIO
		}
		return nil
	}
	b.restoredOnce("a", OutcomeIOError)
	if b.diskState("t3") != "missing" || b.diskState("t1") != "online" || b.partsOn("t1") != 0 {
		t.Errorf("t3 %s, t1 %s, %d parts", b.diskState("t3"), b.diskState("t1"), b.partsOn("t1"))
	}
}

// A part whose write, metadata or read back fails: a failure of the disk keeps the operation planned for the reprise,
// and any other ends it, the part gone (specs.md §14.2, §17.4).
func TestARestoreToWhosePartFailsEachStep(t *testing.T) {
	for _, tc := range []struct {
		call  string
		err   syscall.Errno
		state string
		// nth is the call of the part that fails, every one when 0: its first sync is that of the part just made.
		nth int
	}{
		{"write", syscall.ENOSPC, "undone", 0},
		{"settimes", syscall.EPERM, "undone", 0},
		{"setowner", syscall.EIO, "planned", 0},
		{"setmode", syscall.EIO, "planned", 0},
		{"setmode", syscall.EPERM, "undone", 0},
		{"filesync", syscall.EIO, "planned", 2},
		{"dropcache", syscall.EIO, "planned", 0},
		{"readback", syscall.EIO, "planned", 0},
		{"rename", syscall.ENOSPC, "undone", 0},
	} {
		t.Run(tc.call+" "+tc.err.Error(), func(t *testing.T) {
			b := pruned(t)
			seen := 0
			b.fail[b.dirs["t1"]] = func(call, name string) error {
				// The part's calls alone, but for the move, which is about the name it makes.
				if call != tc.call || (call != "rename" && !strings.HasSuffix(name, ".part")) {
					return nil
				}
				if seen++; tc.nth == 0 || seen == tc.nth {
					return tc.err
				}
				return nil
			}
			summary := b.restoredOnce("a", OutcomeIOError)
			if b.createState() != tc.state || summary.DiskLost != (tc.state == "planned") {
				t.Errorf("the operation is %s, %+v", b.createState(), summary)
			}
			if b.read("t1", "a") != "" {
				t.Error("a copy took the path")
			}
			if tc.state == "undone" && (b.partsOn("t1") != 0 || b.diskState("t1") != "online") {
				t.Errorf("%d parts, t1 %s", b.partsOn("t1"), b.diskState("t1"))
			}
			// The next run resolves what stayed planned, and makes the copy.
			delete(b.fail, b.dirs["t1"])
			b.restoredOnce("a", OutcomeRestored)
			if b.read("t1", "a") != "x" || b.partsOn("t1") != 0 {
				t.Errorf("t1 holds %q, with %d parts", b.read("t1", "a"), b.partsOn("t1"))
			}
		})
	}
}

// A part that does not read back as written is no copy of the entry: io-error, the part gone, the operation undone.
func TestARestoreToWhosePartReadsBackWrong(t *testing.T) {
	b := pruned(t)
	b.at["copied"] = func() {
		entries, err := os.ReadDir(filepath.Join(b.dirs["t1"], "Atoll", "tmp"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("%v, %v", entries, err)
		}
		if err := os.WriteFile(filepath.Join(b.dirs["t1"], "Atoll", "tmp", entries[0].Name()), []byte("z"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b.restoredOnce("a", OutcomeIOError)
	if b.createState() != "undone" || b.partsOn("t1") != 0 || b.read("t1", "a") != "" || b.diskState("t1") != "online" {
		t.Errorf("the operation is %s, %d parts, t1 %s", b.createState(), b.partsOn("t1"), b.diskState("t1"))
	}
}

// An owner the target refuses leaves the copy its own, and then without the setuid and setgid bits, as a copy that
// could not take its source's owner never keeps them (specs.md §14).
func TestARestoreToWhoseOwnerIsRefused(t *testing.T) {
	for _, refused := range []bool{false, true} {
		b := pruned(t)
		if err := os.Chmod(b.inTrash(1, "a"), 0o755|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
		if refused {
			b.failOn("t1", "setowner", syscall.EPERM)
		}
		b.restoredOnce("a", OutcomeRestored)
		info, err := os.Stat(filepath.Join(b.dirs["t1"], "media", "a"))
		if err != nil {
			t.Fatal(err)
		}
		if setuid := info.Mode()&os.ModeSetuid != 0; setuid == refused || info.Mode().Perm() != 0o755 {
			t.Errorf("refused %v: the copy is %v", refused, info.Mode())
		}
	}
}

// The folders a copy from the trash needs on the target: each made like the one in the trash, as an operation of its
// own; one someone made meanwhile is theirs; one that cannot be made ends the copy, the disk lost only when it fails.
func TestARestoreToWhoseFolderIsMissing(t *testing.T) {
	t.Run("made", func(t *testing.T) {
		b := filmsPruned(t)
		b.restoredOnce("Films/b", OutcomeRestored)
		if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "done" || b.read("t1", "Films/b") != "y" {
			t.Errorf("the folder's operation is %s", got)
		}
	})
	t.Run("made meanwhile", func(t *testing.T) {
		b := filmsPruned(t)
		b.at["verified"] = func() {
			if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "Films"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		b.restoredOnce("Films/b", OutcomeRestored)
		if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "undone" || b.read("t1", "Films/b") != "y" {
			t.Errorf("the folder's operation is %s", got)
		}
	})
	for _, tc := range []struct {
		err   syscall.Errno
		state string
	}{{syscall.ENOSPC, "undone"}, {syscall.EIO, "planned"}} {
		t.Run("mkdir "+tc.err.Error(), func(t *testing.T) {
			b := filmsPruned(t)
			b.fail[b.dirs["t1"]] = func(call, name string) error {
				if call == "mkdir" && name == "Films" {
					return tc.err
				}
				return nil
			}
			b.restoredOnce("Films/b", OutcomeIOError)
			if b.createState() != tc.state || b.there("t1", "Films") {
				t.Errorf("the operation is %s, the folder there: %v", b.createState(), b.there("t1", "Films"))
			}
		})
	}
	t.Run("its model cannot be looked at", func(t *testing.T) {
		b := filmsPruned(t)
		late := false
		b.at["verified"] = func() { late = true }
		b.fail[b.dirs["t3"]] = func(call, name string) error {
			if late && call == "stat" && name == "Films" {
				return syscall.EACCES
			}
			return nil
		}
		b.restoredOnce("Films/b", OutcomeIOError)
		if b.createState() != "undone" || b.diskState("t3") != "online" || b.diskState("t1") != "online" || b.there("t1", "Films") {
			t.Errorf("the operation is %s, t3 %s, t1 %s", b.createState(), b.diskState("t3"), b.diskState("t1"))
		}
	})
}

// A path taken between the look and the placing of the part keeps what took it: path-taken, the part gone.
func TestARestoreToWhosePathIsTakenMeanwhile(t *testing.T) {
	b := pruned(t)
	b.at["verified"] = func() { b.write("t1", "a", "theirs") }
	b.restoredOnce("a", OutcomePathTaken)
	if b.read("t1", "a") != "theirs" || b.partsOn("t1") != 0 || b.createState() != "undone" {
		t.Errorf("t1 holds %q, %d parts, the operation is %s", b.read("t1", "a"), b.partsOn("t1"), b.createState())
	}
}

// A part that cannot be removed when the copy ends stays with its operation planned, for the reprise to remove; a disk
// that fails there is lost.
func TestARestoreToWhosePartCannotBeRemoved(t *testing.T) {
	for _, tc := range []struct {
		err  syscall.Errno
		disk string
	}{{syscall.EACCES, "online"}, {syscall.EIO, "missing"}} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			b := pruned(t)
			b.at["copied"] = func() { b.replace(b.inTrash(1, "a"), "x") }
			b.failOn("t1", "unlink", tc.err)
			b.restoredOnce("a", OutcomeIOError)
			if b.createState() != "planned" || b.partsOn("t1") != 1 || b.diskState("t1") != tc.disk {
				t.Errorf("the operation is %s, %d parts, t1 %s", b.createState(), b.partsOn("t1"), b.diskState("t1"))
			}
			delete(b.fail, b.dirs["t1"])
			s := b.opened()
			s.Close()
			if b.createState() != "undone" || b.partsOn("t1") != 0 {
				t.Errorf("after the reprise: %s, %d parts", b.createState(), b.partsOn("t1"))
			}
		})
	}
}
