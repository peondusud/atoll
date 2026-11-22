package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
)

// noTrashRoom makes the full disk t1 answer the write to new.mkv with ENOSPC, as a disk with no room left does, and
// what its trash needs, the folders made there or the rename into them, as the trash finds it once the disk is full:
// errno from call.
func (b *bench) noTrashRoom(call string, errno syscall.Errno) {
	b.machine.setFail(b.mount["t1"], func(c, n string) error {
		switch {
		case c == "write" && n == "new.mkv":
			return syscall.ENOSPC
		case c == call && (call == "mkdir" || n == "new.mkv"):
			return errno
		}
		return nil
	})
}

// removedHere gives, for each move of the view's journal, whether the operation it names removes the copy left at its
// path, with no place in the trash.
func (b *bench) removedHere() []bool {
	b.t.Helper()
	rows, err := b.w.db.QueryContext(b.t.Context(), "SELECT o.kind = 'unlink' AND o.path2 IS NULL FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'move' ORDER BY v.id")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []bool
	for rows.Next() {
		var here bool
		if err := rows.Scan(&here); err != nil {
			b.t.Fatal(err)
		}
		out = append(out, here)
	}
	if err := rows.Err(); err != nil {
		b.t.Fatal(err)
	}
	return out
}

// The trash of the full disk with no room for the copy a move left there, its folders or its rename answering ENOSPC
// or EDQUOT (the maintainer, 2026-11-21; specs.md §21.2): the copy is removed at its path, an unlink journaled with
// its purge event, the trash undone; nothing is left in the trash or planned, and the moved copy holds every byte.
func TestACopyLeftWithNoRoomForItsTrashIsRemovedAtItsPath(t *testing.T) {
	for _, tc := range []struct {
		call  string
		errno syscall.Errno
	}{{"mkdir", syscall.ENOSPC}, {"mkdir", syscall.EDQUOT}, {"rename", syscall.ENOSPC}, {"rename", syscall.EDQUOT}} {
		t.Run(tc.call+" "+tc.errno.Error(), func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			b.noTrashRoom(tc.call, tc.errno)
			if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
				t.Fatal(err)
			}
			must(t, f.Close())
			if b.on("t2", "Films/new.mkv") != first+second || b.on("t1", "Films/new.mkv") != "absent" {
				t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
			}
			if got := b.trashed("t1"); len(got) != 0 {
				t.Errorf("t1's trash holds %v", got)
			}
			if got := b.moves(); !slices.Equal(got, []string{"t1>t2:done:unlink:done"}) {
				t.Errorf("the moves: %v", got)
			}
			if got := b.removedHere(); !slices.Equal(got, []bool{true}) {
				t.Errorf("removed at its path: %v", got)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned", b.planned())
			}
			var trash, purge, undone int
			if err := b.w.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM trash), (SELECT count(*) FROM events WHERE kind = 'purge'),
				(SELECT count(*) FROM ops WHERE kind = 'trash' AND state = 'undone')`).Scan(&trash, &purge, &undone); err != nil {
				t.Fatal(err)
			}
			if trash != 0 || purge != 1 || undone != 1 {
				t.Errorf("trash rows %d, purge events %d, trash operations undone %d", trash, purge, undone)
			}
		})
	}
}

// A copy left that changed since it was copied, its size and change time moved, is never removed at its path when its
// trash has no room: both stay, the move done, the unlink undone with an op-undone event (specs.md §21.2).
func TestACopyLeftThatChangedIsNotRemovedAtItsPath(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	var once sync.Once
	b.machine.setFail(b.mount["t1"], func(c, n string) error {
		switch {
		case c == "write" && n == "new.mkv":
			return syscall.ENOSPC
		case c == "mkdir":
			once.Do(func() {
				// Someone writes it on its disk, around the view, while the trash looks for room.
				left, err := os.OpenFile(filepath.Join(b.mount["t1"], "media", "Films", "new.mkv"), os.O_WRONLY|os.O_APPEND, 0)
				if err == nil {
					_, _ = left.WriteString("more")
					_ = left.Close()
				}
			})
			return syscall.ENOSPC
		}
		return nil
	})
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	must(t, f.Close())
	if b.on("t1", "Films/new.mkv") != first+"more" || b.on("t2", "Films/new.mkv") != first+second {
		t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
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
}

// Stopped once its copy was at its name on the other disk, a move whose full disk then has no room for its trash, in the
// reprise of the next start, leaves the copy at its path, never removed there by a reprise, as only its size and
// modification time say it is the moved one's, and the move planned, the other disk's copy presented; a start once the
// trash has room sends it there, unpurged (specs.md §21.2).
func TestAReprisesTrashWithNoRoomLeavesTheMovePlanned(t *testing.T) {
	for _, point := range []string{"move-placed", "move-swapped"} {
		t.Run(point, func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			b.full("t1", "new.mkv", syscall.ENOSPC)
			if !b.stopAt(point, func() { _, _ = f.WriteAt([]byte(second), int64(len(first))) }) {
				t.Fatal("no stop")
			}
			b.noTrashRoom("mkdir", syscall.ENOSPC)
			if err := b.w.Reprise(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := b.moves(); !slices.Equal(got, []string{"t1>t2:planned:trash:planned"}) {
				t.Errorf("the moves after a reprise with no room: %v", got)
			}
			if b.on("t1", "Films/new.mkv") != first || b.on("t2", "Films/new.mkv") != first {
				t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
			}
			if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t2" {
				t.Errorf("the view presents %+v", n)
			}
			if got := b.trashed("t1"); len(got) != 0 {
				t.Errorf("t1's trash holds %v", got)
			}
			b.machine.setFail(b.mount["t1"], nil)
			if err := b.w.Reprise(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := b.moves(); !slices.Equal(got, []string{"t1>t2:done:trash:done"}) {
				t.Errorf("the moves once the trash has room: %v", got)
			}
			if b.on("t1", "Films/new.mkv") != "absent" {
				t.Errorf("t1 holds %q", b.on("t1", "Films/new.mkv"))
			}
			if got := b.trashed("t1"); len(got) != 1 {
				t.Errorf("t1's trash holds %v, want the copy left, unpurged", got)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned", b.planned())
			}
		})
	}
}

// atoll mount stopped at each step of the removal at its path of a copy left on a full disk, then started again: its
// reprise resolves that unlink as a purge's: gone, done; still there, undone, the copy staying beside the moved one,
// which the view presents and which holds every byte written; nothing planned, nothing in the trash (specs.md §17.2,
// §21.2).
func TestARemovalAtItsPathStoppedAtEachStep(t *testing.T) {
	// The crash points a move passes, its trash with no room, counted from the write that ran out of space.
	all := func() []string {
		b := moveBench(t, "", 500, 400)
		f := b.startNew()
		b.noTrashRoom("mkdir", syscall.ENOSPC)
		b.machine.passed()
		if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		return b.machine.passed()
	}()
	from := slices.Index(all, "move-unlink-journaled")
	if from < 0 || !slices.Equal(all[from:], []string{"move-unlink-journaled", "unlinked", "source-synced"}) {
		t.Fatalf("the crash points of a removal at its path: %v", all)
	}
	for at := from; at < len(all); at++ {
		point := all[at]
		t.Run(fmt.Sprintf("%d %s", at+1, point), func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			b.noTrashRoom("mkdir", syscall.ENOSPC)
			b.machine.passed()
			b.machine.mu.Lock()
			b.machine.crash = at + 1
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
				_, _ = f.WriteAt([]byte(second), int64(len(first)))
				return false
			}()
			b.machine.mu.Lock()
			b.machine.crash = 0
			b.machine.mu.Unlock()
			if !stopped {
				t.Fatalf("no stop at %s", point)
			}
			// atoll mount starts again, the full disk's trash still with no room: its reprise, before it mounts.
			if err := b.w.Reprise(t.Context()); err != nil {
				t.Fatal(err)
			}
			if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t2" || b.on("t2", "Films/new.mkv") != first {
				t.Errorf("the view presents %+v, t2 holds %q", n, b.on("t2", "Films/new.mkv"))
			}
			want, left := "t1>t2:done:unlink:done", "absent"
			if point == "move-unlink-journaled" {
				want, left = "t1>t2:done:unlink:undone", first
			}
			if got := b.moves(); !slices.Equal(got, []string{want}) {
				t.Errorf("the moves: %v, want %s", got, want)
			}
			if b.on("t1", "Films/new.mkv") != left {
				t.Errorf("t1 holds %q, want %q", b.on("t1", "Films/new.mkv"), left)
			}
			if got := b.trashed("t1"); len(got) != 0 {
				t.Errorf("t1's trash holds %v", got)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned", b.planned())
			}
		})
	}
}
