package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// wildcardPool gives a pool of three disks, the second NTFS and mounted with the type ntfs, which cannot take a name
// holding a wildcard character out of its folder without setting its whole volume read-only, each disk with the file
// Who?/q?.mkv that a --full verified, and a record of every call that would write to a name holding a wildcard there.
func wildcardPool(t *testing.T, more string) (*bench, *[]string) {
	t.Helper()
	b := newBenchWith(t, more, "t1", "t2", "t3")
	b.config.Disks[1].FS = names.NTFS
	b.types = map[string]string{b.dirs["t2"]: "ntfs"}
	var calls []string
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		switch call {
		case "stat", "opendir", "openfile", "names", "readlink", "xattrs", "sync", "syncentry", "syncfs", "fileinfo", "read", "readback", "dropcache":
		default:
			if strings.ContainsAny(name, names.Wildcards) {
				calls = append(calls, call+" "+name)
			}
		}
		return nil
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "Who?/q?.mkv", "film")
	}
	b.scan(true)
	return b, &calls
}

// leftThere checks that the copy on t2 is still at its name with its content, and that nothing was journaled or asked
// of t2 at a name holding a wildcard.
func leftThere(t *testing.T, b *bench, calls *[]string, at string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", filepath.FromSlash(at))); err != nil || string(data) != "film" {
		t.Errorf("the copy left as it is: %q, %v", data, err)
	}
	if len(*calls) > 0 {
		t.Errorf("asked of t2: %v", *calls)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE disk_id = 't2'"); got != 0 {
		t.Errorf("%d operations journaled on t2", got)
	}
}

// TestADeletionLeavesAWildcardNameOnTheTypeNTFS: a file deleted on t1 goes to the trash of t3, and its copy on t2 stays
// where it is, with its row, its own job of the deletion refused at each pass, nothing journaled: the file stays,
// name-incompatible, with that copy alone, and waits as one whose deletion is decided, so that nothing is copied from it
// again, however many passes go by (specs.md §15.8, §15.12).
func TestADeletionLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.remove("t1", "Who?/q?.mkv")
	b.settle()
	b.settle()
	out := b.settle()
	leftThere(t, b, calls, "Who?/q?.mkv")
	if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "Who?", "q?.mkv")); !os.IsNotExist(err) {
		t.Errorf("the copy on t3: %v", err)
	}
	if got := b.text("SELECT group_concat(disk_id) FROM copies"); got != "t2" {
		t.Errorf("copies on %q", got)
	}
	if got := b.text("SELECT state FROM files"); got != "name-incompatible" {
		t.Errorf("the file is %q", got)
	}
	if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't3'"); got != 1 {
		t.Errorf("%d entries in the trash of t3", got)
	}
	if !slices.ContainsFunc(out.Operations, func(o Operation) bool {
		return o.Disk == "t2" && o.Kind == KindTrash && o.Reason == reasonDeleted && o.Outcome == trash.OutcomeNameRefused
	}) || slices.ContainsFunc(out.Operations, func(o Operation) bool { return o.Kind == KindReplicate }) {
		t.Errorf("the last pass: %+v", out.Operations)
	}
	if got := b.text("SELECT group_concat(kind || ' ' || state || ' ' || dst_disk) FROM jobs"); got != "trash queued t2" {
		t.Errorf("the jobs: %q", got)
	}
}

// TestARenameLeavesAWildcardNameOnTheTypeNTFS: a file renamed on t1 is renamed on t3, and its copy on t2 stays where it
// sits, as a name refused, and never goes to the trash: the file at its new name is name-incompatible (specs.md
// §15.8, §15.12).
func TestARenameLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.rename("t1", "Who?/q?.mkv", "Who/q.mkv")
	b.settle()
	out := b.settle()
	leftThere(t, b, calls, "Who?/q?.mkv")
	if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "Who", "q.mkv")); err != nil {
		t.Errorf("the copy on t3: %v", err)
	}
	if got := outcomeOf(out, KindRename, "Who?/q?.mkv"); got != "name-refused" {
		t.Errorf("the rename on t2: %q, %+v", got, out.Operations)
	}
	if got := b.text("SELECT CAST(old_lpath AS TEXT) FROM copies WHERE disk_id = 't2'"); got != "Who?/q?.mkv" {
		t.Errorf("the copy on t2 sits at %q", got)
	}
	if got := b.text("SELECT state FROM files"); got != "name-incompatible" {
		t.Errorf("the file is %q", got)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE src_disk = 't2' OR dst_disk = 't2'"); got != 0 {
		t.Errorf("%d jobs on t2", got)
	}
}

// TestAFolderRenameLeavesAWildcardNameOnTheTypeNTFS: a folder whose name holds a wildcard character renamed on t1 is
// renamed on t3 as one operation; on t2 it cannot go as one, and its copy, below it, stays where it sits, as a name
// refused (specs.md §15.8, §15.12).
func TestAFolderRenameLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.rename("t1", "Who?", "Who")
	b.settle()
	b.settle()
	leftThere(t, b, calls, "Who?/q?.mkv")
	if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "Who", "q?.mkv")); err != nil {
		t.Errorf("the copy on t3: %v", err)
	}
	if got := b.text("SELECT CAST(old_lpath AS TEXT) FROM copies WHERE disk_id = 't2'"); got != "Who?/q?.mkv" {
		t.Errorf("the copy on t2 sits at %q", got)
	}
	if got := b.text("SELECT state FROM files"); got != "name-incompatible" {
		t.Errorf("the file is %q", got)
	}
}

// TestAnEditLeavesAWildcardNameOnTheTypeNTFS: a file edited on t1 is refreshed on t3, and its copy on t2 goes stale and
// stays as it is: never sent to the trash, never written to (specs.md §15.4, §15.8).
func TestAnEditLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.write("t1", "Who?/q?.mkv", "a longer film")
	b.now = b.ctimeOf("t1", "Who?/q?.mkv") + settleNS
	b.settle()
	b.settle()
	leftThere(t, b, calls, "Who?/q?.mkv")
	if data, err := os.ReadFile(filepath.Join(b.dirs["t3"], "media", "Who?", "q?.mkv")); err != nil || string(data) != "a longer film" {
		t.Errorf("the copy on t3: %q, %v", data, err)
	}
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "stale" {
		t.Errorf("the copy on t2 is %q", got)
	}
	if got := b.text("SELECT state FROM files"); got != "name-incompatible" {
		t.Errorf("the file is %q", got)
	}
}

// TestAnEditThereRefreshesTheOthers: the copy on t2 edited, the others go stale, to the trash as overwritten, and are
// made again from it, the source of a copy as any (the maintainer, 2026-11-23): every version kept, nothing written,
// moved or journaled on t2, the file name-incompatible (specs.md §15.4, §15.8).
func TestAnEditThereRefreshesTheOthers(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.write("t2", "Who?/q?.mkv", "a longer film")
	b.now = b.ctimeOf("t2", "Who?/q?.mkv") + settleNS
	b.settle()
	b.settle()
	if data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", "Who?", "q?.mkv")); err != nil || string(data) != "a longer film" {
		t.Errorf("the copy left as it is: %q, %v", data, err)
	}
	if len(*calls) > 0 {
		t.Errorf("asked of t2: %v", *calls)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE disk_id = 't2'"); got != 0 {
		t.Errorf("%d operations journaled on t2", got)
	}
	for _, id := range []string{"t1", "t3"} {
		if data, err := os.ReadFile(filepath.Join(b.dirs[id], "media", "Who?", "q?.mkv")); err != nil || string(data) != "a longer film" {
			t.Errorf("the copy on %s: %q, %v", id, data, err)
		}
	}
	if got := b.count("SELECT count(*) FROM trash WHERE reason = 'overwritten'"); got != 2 {
		t.Errorf("%d versions in the trash", got)
	}
	if got := b.text("SELECT state FROM files"); got != "name-incompatible" {
		t.Errorf("the file is %q", got)
	}
}

// TestATimeLeavesAWildcardNameOnTheTypeNTFS: a time set on t1 goes to t3, and the copy on t2, which no operation writes
// to, goes stale (specs.md §15.4, §15.8).
func TestATimeLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.touch("t1", "Who?/q?.mkv", 1500000000_000000000)
	b.now = b.ctimeOf("t1", "Who?/q?.mkv") + settleNS
	b.settle()
	b.settle()
	leftThere(t, b, calls, "Who?/q?.mkv")
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "stale" {
		t.Errorf("the copy on t2 is %q", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'utimens' AND disk_id = 't3'"); got != 1 {
		t.Errorf("%d times set on t3", got)
	}
}

// TestARepairLeavesAWildcardNameOnTheTypeNTFS: a copy left as it is that a scrub finds corrupt stays where it is and as
// it is, neither repaired in place nor sent to the trash, its name refused; and a corrupt copy elsewhere is repaired from
// it, the healthy copy, which is only read (specs.md §15.8, §16.1).
func TestARepairLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	setup := func(t *testing.T, ids ...string) (*bench, *[]string, []byte) {
		if len(ids) == 0 {
			ids = []string{"t1", "t2"}
		}
		b := newBench(t, ids...)
		b.config.Disks[1].FS = names.NTFS
		b.types = map[string]string{b.dirs["t2"]: "ntfs"}
		var calls []string
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if (call == "openwrite" || call == "rename" || call == "unlink" || call == "write" || call == "openfile") && strings.ContainsAny(name, names.Wildcards) {
				calls = append(calls, call+" "+name)
			}
			return nil
		}
		data := film()
		for _, id := range ids {
			b.write(id, "Who?/a?.mkv", string(data))
		}
		b.scan(true)
		b.now += int64(month)
		return b, &calls, data
	}
	t.Run("corrupt there", func(t *testing.T) {
		b, calls, data := setup(t)
		rotten := b.rotChunks("t2", "Who?/a?.mkv", data, 1)
		b.scrub(ScrubOptions{Apply: true})
		done := b.repair(RepairOptions{Apply: true})
		if len(done.Items) != 1 || done.Items[0].Disk != "t2" || done.Items[0].Outcome != trash.OutcomeNameRefused {
			t.Errorf("%+v", done)
		}
		if b.read("t2", "Who?/a?.mkv") != string(rotten) || slices.ContainsFunc(*calls, func(c string) bool { return !strings.HasPrefix(c, "openfile ") }) {
			t.Errorf("written to: %v", *calls)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE disk_id = 't2'"); got != 0 {
			t.Errorf("%d operations journaled on t2", got)
		}
		if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "corrupt" {
			t.Errorf("the copy on t2 is %q", got)
		}
	})
	t.Run("the healthy copy there", func(t *testing.T) {
		b, calls, data := setup(t)
		b.rotChunks("t1", "Who?/a?.mkv", data, 1)
		b.scrub(ScrubOptions{Apply: true})
		b.repair(RepairOptions{Apply: true})
		if b.read("t1", "Who?/a?.mkv") != string(data) {
			t.Error("the copy on t1 is not repaired")
		}
		if slices.ContainsFunc(*calls, func(c string) bool { return !strings.HasPrefix(c, "openfile ") }) {
			t.Errorf("asked of t2: %v", *calls)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE disk_id = 't2'"); got != 0 {
			t.Errorf("%d operations journaled on t2", got)
		}
	})
	t.Run("the healthy copy there and another", func(t *testing.T) {
		b, calls, data := setup(t, "t1", "t2", "t3")
		b.rotChunks("t1", "Who?/a?.mkv", data, 1)
		b.scrub(ScrubOptions{Apply: true})
		*calls = nil
		b.repair(RepairOptions{Apply: true})
		if b.read("t1", "Who?/a?.mkv") != string(data) {
			t.Error("the copy on t1 is not repaired")
		}
		// The repair reads the copy on t3, after t2 by position: the copy left as it is is read last.
		if len(*calls) > 0 {
			t.Errorf("asked of t2: %v", *calls)
		}
	})
}

// TestADryRunSaysAWildcardNameIsLeft: a dry run of a pass after an edit on t1 says that the copy on t2 would not go to
// the trash, its name refused, as a real pass leaves it (specs.md §15.8, §15.12).
func TestADryRunSaysAWildcardNameIsLeft(t *testing.T) {
	b, calls := wildcardPool(t, "")
	b.write("t1", "Who?/q?.mkv", "a longer film")
	b.now = b.ctimeOf("t1", "Who?/q?.mkv") + settleNS
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(out.Operations, func(o Operation) bool {
		return o.Disk == "t2" && o.Kind == KindTrash && o.Outcome == "name-refused"
	}) {
		t.Errorf("the dry run: %+v", out.Operations)
	}
	leftThere(t, b, calls, "Who?/q?.mkv")
}
