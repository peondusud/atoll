package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// contentAnywhere says where a file of that content is on a disk of the bench: in the library or in a trash.
func (b *bench) contentAnywhere(content string) []string {
	b.t.Helper()
	var at []string
	for id, dir := range b.dirs {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, ".db") {
				return nil //nolint:nilerr // what cannot be read holds nothing for these tests
			}
			if data, err := os.ReadFile(path); err == nil && string(data) == content {
				rel, _ := filepath.Rel(dir, path)
				at = append(at, id+":"+filepath.ToSlash(rel))
			}
			return nil
		})
	}
	return at
}

// deleteOnMove deletes the file on the data disks directly, right before the transfer moves its copy on the cache to
// the trash: from the fault hook of the cache's rename.
func (b *bench) deleteOnMove(path string, from ...string) *bool {
	b.t.Helper()
	deleted := false
	b.fail[b.dirs["c"]] = func(call, _ string) error {
		if call == "rename" && !deleted {
			deleted = true
			for _, id := range from {
				if err := os.Remove(filepath.Join(b.dirs[id], "media", path)); err != nil {
					b.t.Error(err)
				}
			}
		}
		return nil
	}
	return &deleted
}

// The SSD cache's transfer looks at the copies on the data disks it leans on right before it purges its copy on the
// cache from the trash: a user who deleted them directly, after the pass's walk and before the move, no longer loses
// the file for good, as nothing is purged and the copy stays in the cache's trash, and the next pass decides from what
// the data disks hold (specs.md §19; conformance/models/README.md, flaw 8, the maintainer's remedy).
func TestATransferKeepsItsCopyWhenTheDataCopiesWentMeanwhile(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	const content = "episode"
	b.write("c", "show/e1.mkv", content)
	b.write("t1", "show/e1.mkv", content)
	b.write("t2", "show/e1.mkv", content)
	b.scan(true)
	deleted := b.deleteOnMove("show/e1.mkv", "t1", "t2")
	out := b.pass()
	b.fail[b.dirs["c"]] = nil
	if !*deleted {
		t.Fatalf("no move reached the cache: %+v", out.Operations)
	}
	if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); got == OutcomeTransferred {
		t.Errorf("the transfer says %q", got)
	}
	if at := b.contentAnywhere(content); len(at) != 1 || !strings.HasPrefix(at[0], "c:Atoll/trash/") {
		t.Errorf("the copy on the cache is not in its trash: the file is at %v", at)
	}
	if n := b.count("SELECT count(*) FROM trash WHERE disk_id = 'c'"); n != 1 {
		t.Errorf("%d entries in the cache's trash", n)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'unlink' AND state = 'done'"); n != 0 {
		t.Errorf("%d purges done", n)
	}
	if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
		t.Errorf("%d operations left planned", n)
	}
}

// The same with an edit: a user who writes into the one copy on a data disk of a file of one replica, in that window,
// keeps the version the catalog vouches for, on the cache, where the next pass finds it stale.
func TestATransferKeepsItsCopyWhenADataCopyChangedMeanwhile(t *testing.T) {
	// cacheBench's more begins in [defaults]: one replica fewer, the file at its level of one copy.
	b := cacheBench(t, "replicas = 0\n[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	const content = "episode"
	b.write("c", "show/e1.mkv", content)
	b.write("t1", "show/e1.mkv", content)
	b.scan(true)
	edited := false
	b.fail[b.dirs["c"]] = func(call, _ string) error {
		if call == "rename" && !edited {
			edited = true
			if err := os.WriteFile(filepath.Join(b.dirs["t1"], "media", "show", "e1.mkv"), []byte("a better release"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	out := b.pass()
	b.fail[b.dirs["c"]] = nil
	if !edited {
		t.Fatalf("no move reached the cache: %+v", out.Operations)
	}
	if at := b.contentAnywhere(content); len(at) == 0 {
		t.Errorf("the version the catalog vouches for is on no disk, in no trash (transfer %s)", outcomeOf(out, KindTransfer, "show/e1.mkv"))
	}
}

// A data disk that fails the look right before the purge says nothing of the copy there: nothing is purged, as for
// for a copy that changed, and the disk is lost (invariant 4).
func TestATransferKeepsItsCopyWhenADataDiskFailsTheLook(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.write("c", "show/e1.mkv", "episode")
	b.write("t1", "show/e1.mkv", "episode")
	b.write("t2", "show/e1.mkv", "episode")
	b.scan(true)
	moved := false
	b.fail[b.dirs["c"]] = func(call, _ string) error {
		if call == "rename" {
			moved = true
		}
		return nil
	}
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if moved && call == "stat" && name == "e1.mkv" {
			return syscall.EIO
		}
		return nil
	}
	out := b.pass()
	b.fail[b.dirs["c"]], b.fail[b.dirs["t2"]] = nil, nil
	if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); got == OutcomeTransferred || b.count("SELECT count(*) FROM trash WHERE disk_id = 'c'") != 1 {
		t.Errorf("transfer %q, the copy on the cache at %v", got, b.contentAnywhere("episode"))
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "missing" {
		t.Errorf("t2 is %s", got)
	}
}

// Atoll killed at each point of a transfer whose data copies a user deleted directly as it moved its copy on the
// cache: the reprise finishes or undoes the move, nothing is purged, the copy is at its path on the cache or in its
// trash, never lost, and a pass after leaves nothing planned.
func TestATransferKilledWhileItsDataCopiesGo(t *testing.T) {
	for _, point := range []string{"journaled", "trash-folders", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
			const content = "episode"
			b.write("c", "show/e1.mkv", content)
			b.write("t1", "show/e1.mkv", content)
			b.write("t2", "show/e1.mkv", content)
			b.scan(true)
			b.deleteOnMove("show/e1.mkv", "t1", "t2")
			b.crashAtNth(point, 1)
			b.fail[b.dirs["c"]] = nil
			b.pass()
			if at := b.contentAnywhere(content); len(at) == 0 {
				t.Errorf("the file is on no disk, in no trash")
			}
			if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
				t.Errorf("%d operations left planned", n)
			}
		})
	}
}
