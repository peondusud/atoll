package writer

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// The copy a move leaves on the full disk goes to that disk's trash, then is purged at once. A file that takes its name
// just before that move to the trash, with the inode number the copy had, as a file made right after the copy was
// removed can, or the copy written to there in that moment, is not that copy: the look after the move compares the
// inode, the size and the modification time with what the move copied, and the file goes back to its path, never
// purged, the trash undone with an event, and the move done, for a pass to find both, as a copy its trash refuses does
// (specs.md §17.1, step 4, §17.2, §21.2). By its inode alone, it was taken for the copy, and purged.
func TestACopyLeftWithItsInodeAndNotItsSizeIsNeverPurged(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	left := filepath.Join(b.mount["t1"], "media", "Films", "new.mkv")
	moved := false
	b.machine.setFail(b.mount["t1"], func(call, n string) error {
		switch {
		case call == "write" && n == "new.mkv":
			return syscall.ENOSPC
		case call == "rename" && n == "new.mkv" && !moved:
			moved = true
			// Written in place: the inode stays, the size does not.
			if err := os.WriteFile(left, []byte("theirs"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	if n, err := f.WriteAt([]byte(second), int64(len(first))); err != nil || n != len(second) {
		t.Fatalf("the write that ran out of space: %d, %v", n, err)
	}
	if !moved {
		t.Fatal("the copy left never went to the trash")
	}
	if f.OnDisk() != "t2" {
		t.Errorf("the file is on %s", f.OnDisk())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b.machine.setFail(b.mount["t1"], nil)
	if got := b.on("t1", "Films/new.mkv"); got != "theirs" {
		t.Errorf("t1 holds %q at the file's path", got)
	}
	if got := b.on("t2", "Films/new.mkv"); got != first+second {
		t.Errorf("t2 holds %q", got)
	}
	if got := b.trashed("t1"); len(got) != 0 {
		t.Errorf("t1's trash holds %v", got)
	}
	if got := b.moves(); !slices.Equal(got, []string{"t1>t2:done:trash:undone"}) {
		t.Errorf("the moves: %v", got)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
	}
	var undone int
	if err := b.w.db.QueryRowContext(t.Context(), "SELECT count(*) FROM events WHERE kind = 'op-undone'").Scan(&undone); err != nil || undone != 1 {
		t.Errorf("%d events op-undone, %v", undone, err)
	}
}
