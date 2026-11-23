package writer

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// The move of a file whose write ran out of space looks at the file's path on the disk it went to right before it purges
// the copy it left on the full disk, or removes that copy at its path when its trash had no room for it: the target must
// still hold the copy the move placed there as it placed it, its inode, size, modification time and change time
// (specs.md §21.2; conformance/models/README.md, flaw 17, the maintainer's remedy, 2026-11-23). A copy a user or a program
// deleted, wrote into, or rewrote keeping its size and times directly on the target, once the handle was its, or a target
// that cannot be looked at, keeps the copy left, in the trash unpurged or at its path: its unlink undone with an
// op-undone event, the move done, nothing planned. Before, the copy left was purged, and a file deleted on the target
// meanwhile was on no disk and in no trash.
func TestAMoveKeepsTheCopyLeftWhenTheTargetNoLongerHoldsItsCopy(t *testing.T) {
	type change func(b *bench, moved string)
	deleted := func(b *bench, moved string) {
		if err := os.Remove(moved); err != nil {
			b.t.Error(err)
		}
	}
	written := func(b *bench, moved string) {
		f, err := os.OpenFile(moved, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			b.t.Fatal(err)
		}
		_, _ = f.WriteString("more")
		_ = f.Close()
	}
	rewritten := func(b *bench, moved string) {
		// The same size and modification time: only the change time says it.
		st, err := os.Stat(moved)
		if err != nil {
			b.t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		if err := os.WriteFile(moved, []byte("FIRST PART "), 0); err != nil {
			b.t.Fatal(err)
		}
		if err := os.Chtimes(moved, st.ModTime(), st.ModTime()); err != nil {
			b.t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// at is the call on the full disk t1 before which the target's copy changes: the trash's rename, or its mkdir
		// when that trash has no room.
		at      string
		noRoom  bool
		change  change
		removed bool
	}{
		{name: "deleted", at: "rename", change: deleted},
		{name: "written", at: "rename", change: written},
		{name: "rewritten keeping its size and times", at: "rename", change: rewritten},
		{name: "deleted, the trash with no room", at: "mkdir", noRoom: true, change: deleted, removed: true},
		{name: "written, the trash with no room", at: "mkdir", noRoom: true, change: written, removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			moved := filepath.Join(b.mount["t2"], "media", "Films", "new.mkv")
			once := false
			b.machine.setFail(b.mount["t1"], func(call, name string) error {
				switch {
				case call == "write" && name == "new.mkv":
					return syscall.ENOSPC
				case call == tc.at && (call == "mkdir" || name == "new.mkv"):
					if !once {
						once = true
						tc.change(b, moved)
					}
					if tc.noRoom {
						return syscall.ENOSPC
					}
				}
				return nil
			})
			_, _ = f.WriteAt([]byte(second), int64(len(first)))
			_ = f.Close()
			if !once {
				t.Fatal("the copy left never went to the trash")
			}
			if tc.removed {
				if got := b.on("t1", "Films/new.mkv"); got != first {
					t.Errorf("the copy left at its path holds %q, want %q", got, first)
				}
				if got := b.removedHere(); !slices.Equal(got, []bool{true}) {
					t.Errorf("removed at its path: %v", got)
				}
			} else {
				held := false
				for _, data := range b.trashed("t1") {
					held = held || data == first
				}
				if !held {
					t.Errorf("the copy left is in no trash: t1 %q, t2 %q, t1's trash %v",
						b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"), b.trashed("t1"))
				}
			}
			if got := b.moves(); !slices.Equal(got, []string{"t1>t2:done:unlink:undone"}) {
				t.Errorf("the moves: %v", got)
			}
			var undone int
			if err := b.w.db.QueryRowContext(t.Context(), "SELECT count(*) FROM events WHERE kind = 'op-undone'").Scan(&undone); err != nil || undone != 1 {
				t.Errorf("op-undone events: %d, %v", undone, err)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned", b.planned())
			}
		})
	}
}

// A target that cannot be looked at right before the purge, its stat failing with EIO, keeps the copy left in the full
// disk's trash, unpurged, the unlink undone: nothing says the target still holds the file (specs.md §21.2, invariant 4).
func TestAMoveWhoseTargetCannotBeLookedAtKeepsTheCopyLeft(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	trashed, failed := false, false
	b.machine.setFail(b.mount["t1"], func(call, name string) error {
		switch {
		case call == "write" && name == "new.mkv":
			return syscall.ENOSPC
		case call == "rename" && name == "new.mkv":
			trashed = true
		}
		return nil
	})
	b.machine.setFail(b.mount["t2"], func(call, name string) error {
		if trashed && !failed && call == "stat" && name == "new.mkv" {
			failed = true
			return syscall.EIO
		}
		return nil
	})
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if !failed {
		t.Fatal("the target was never looked at after the trash")
	}
	if got := b.trashed("t1"); len(got) != 1 {
		t.Errorf("t1's trash holds %v, want the copy left, unpurged", got)
	}
	for _, data := range b.trashed("t1") {
		if data != first {
			t.Errorf("t1's trash holds %q", data)
		}
	}
	if got := b.moves(); !slices.Equal(got, []string{"t1>t2:done:unlink:undone"}) {
		t.Errorf("the moves: %v", got)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
	}
}
