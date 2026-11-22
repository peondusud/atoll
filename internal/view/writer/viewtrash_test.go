package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// atoll mount stopped between the link and the unlink of a deletion, on a disk without RENAME_NOREPLACE, as ntfs-3g
// is: its next start resolves the deletion by the table of §17.2 (specs.md §21.2), whose row target same, source same
// removes the copy's name in the library and finishes it. The reprise once called it done on the inode the trash's
// name held, and left the copy at its name (conformance/models/README.md, flaw 3).
func TestTheRepriseOfALinkedDeletionRemovesTheCopysName(t *testing.T) {
	b := newBench(t, "ext4")
	const path = "Films/a.mkv"
	disk := b.lookup(path).Disk
	// The copy's row is what a walk recorded of it: its inode, its size and its time.
	info, err := os.Stat(filepath.Join(b.mount[disk], "media", path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.w.db.ExecContext(t.Context(), "UPDATE copies SET file_key = ?, size = ?, mtime_ns = ? WHERE disk_id = ?",
		fmt.Sprintf("%d", b.inode(disk, path)), info.Size(), info.ModTime().UnixNano(), disk); err != nil {
		t.Fatal(err)
	}
	// renameat2 refuses its flag there, as ntfs-3g's does: link, then unlink (specs.md §17.1).
	b.machine.setFail(b.mount[disk], func(call, _ string) error {
		if call == "rename" {
			return syscall.EINVAL
		}
		return nil
	})
	b.machine.mu.Lock()
	b.machine.crashAt = "linked"
	b.machine.mu.Unlock()
	stopped := func() (stopped bool) {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(crashed); !ok {
					panic(r) //nolint:forbidigo // not a stop the test asked for: as it came
				}
				stopped = true
			}
		}()
		_ = b.w.Unlink(t.Context(), path, root)
		return false
	}()
	if !stopped {
		t.Fatal("the deletion never reached its link")
	}
	b.machine.setFail(b.mount[disk], nil)
	// atoll mount starts again: its reprise, before it mounts.
	if err := b.w.Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.on(disk, path); got != "absent" {
		t.Errorf("the copy is still at its name on %s, holding %q", disk, got)
	}
	if got := b.trashed(disk); len(got) != 1 {
		t.Errorf("the trash of %s holds %v", disk, got)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
	}
	open, err := catalog.OpenViewOps(t.Context(), b.w.db)
	if err != nil || len(open) != 1 || open[0].Kind != catalog.ViewUnlink || open[0].State != catalog.OpDone {
		t.Errorf("the view's journal: %+v, %v", open, err)
	}
}

// A deletion through the view whose copy a user edits directly on its disk between the look right before the move and
// the move: the look after the move finds another size and time at the trash's name, and the move is resolved at once
// by the table of §17.2 (target other, source absent), as the daemon resolves one (specs.md §17.1, §21.2): the edit goes
// back to its path, the operation undone with an op-undone event, and nothing is left in the trash. It once was undone
// as it was, the edit left in the trash with no row there (conformance/models/README.md, flaw 4).
func TestADeletionEditedAtTheMoveGoesBack(t *testing.T) {
	b := newBench(t, "ext4")
	const path = "Films/a.mkv"
	const edited = "edited by hand, at the move"
	disk := b.lookup(path).Disk
	file := filepath.Join(b.mount[disk], "media", path)
	once := false
	b.machine.setFail(b.mount[disk], func(call, _ string) error {
		if call == "rename" && !once {
			// A user writes into the copy in place, right before the move: same inode, another size and time.
			once = true
			if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	err := b.w.Unlink(t.Context(), path, root)
	b.machine.setFail(b.mount[disk], nil)
	if !once {
		t.Fatal("the deletion never reached its move")
	}
	if err == nil {
		t.Error("the deletion said it was made")
	}
	if got := b.on(disk, path); got != edited {
		t.Errorf("the user's edit is not back at its path on %s: %q", disk, got)
	}
	if got := b.trashed(disk); len(got) != 0 {
		t.Errorf("the trash of %s holds %v", disk, got)
	}
	entries, err := catalog.TrashEntries(t.Context(), b.w.db)
	if err != nil || len(entries) != 0 {
		t.Errorf("the trash lists %+v, %v", entries, err)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
	}
	var undone int
	if err := b.w.db.QueryRowContext(t.Context(), "SELECT count(*) FROM events WHERE kind = 'op-undone'").Scan(&undone); err != nil || undone != 1 {
		t.Errorf("op-undone events: %d, %v", undone, err)
	}
}
