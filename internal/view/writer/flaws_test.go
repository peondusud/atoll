//go:build flaws

package writer

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// The flaws of the view's code that the TLA+ model found and that wait for the maintainer's remedy
// (conformance/models/README.md, flaws 3 and 4), each a test that fails as the flaw says until then. The conformance
// runner has no FUSE to drive the view through, so they are unit tests of the writer, kept out of go test ./... by the
// build tag flaws; make model-flaws runs them.

// Flaw 3: atoll mount stopped between the link and the unlink of a deletion, on a disk without RENAME_NOREPLACE, as
// ntfs-3g is. At its next start, its reprise must resolve the deletion by the table of §17.2 (specs.md §21.2), whose
// row target same, source same removes the source's name and finishes it; the code calls it done on the inode the
// trash's name holds, and leaves the copy at its name in the library.
func TestFlawViewRepriseOfALinkedDeletion(t *testing.T) {
	b := newBench(t, "ext4")
	const path = "Films/a.mkv"
	disk := b.lookup(path).Disk
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
		t.Errorf("flaw 3: the deletion is resolved, %d operations left planned, and the copy is still at its name on %s, "+
			"holding %q, beside its name in the trash: the source's name was not removed (specs.md §17.2, target same, source same)",
			b.planned(), disk, got)
	}
}

// Flaw 4: a deletion through the view whose copy a user edits directly on its disk between the look right before the
// move and the move. The look after the move finds another size and time at the trash's name, and §17.1 resolves the
// move at once by the table of §17.2, whose row target other, source absent puts the file back at its path; the code
// undoes the operation as it is, and the user's edit stays in the trash, with no row there.
func TestFlawViewDeletionEditedAtTheMove(t *testing.T) {
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
	_ = b.w.Unlink(t.Context(), path, root)
	b.machine.setFail(b.mount[disk], nil)
	if !once {
		t.Fatal("the deletion never reached its move")
	}
	if got := b.on(disk, path); got != edited {
		entries, err := catalog.TrashEntries(t.Context(), b.w.db)
		t.Errorf("flaw 4: the user's edit is not back at its path on %s (%q there), the trash holds %v and lists %d "+
			"entries (%v): the move was undone as it was, not resolved by the table of §17.2 (target other, source absent)",
			disk, got, b.trashed(disk), len(entries), err)
	}
}
