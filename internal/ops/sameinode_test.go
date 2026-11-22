package ops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// What a file at a copy's name can be once a move has gone through it, compared with what its row says: the inode
// stays, and so it is no new inode that tells, but its size or its modification time. That is what a file that took the
// copy's name just before the move, with the inode number of the copy just removed, as ext4 gives the inode of a file
// removed to the next one made, looks like to a look; or the copy edited in place in that moment. Each of the two
// must tell alone.
var notTheCopy = []struct {
	name   string
	change func(t *testing.T, full string)
}{
	{"another size, the same modification time", func(t *testing.T, full string) {
		t.Helper()
		at := mtimeAt(t, full)
		writeInPlace(t, full, "another film")
		setMtimeAt(t, full, at)
	}},
	{"the same size, another modification time", func(t *testing.T, full string) {
		t.Helper()
		at := mtimeAt(t, full)
		writeInPlace(t, full, "mlif")
		setMtimeAt(t, full, at.Add(time.Second))
	}},
	{"another size and modification time", func(t *testing.T, full string) {
		t.Helper()
		writeInPlace(t, full, "another film")
	}},
}

// The look right after a move compares what is at the new name with the copy as the link path's look does, the inode,
// the size and the modification time, and never by the inode alone: a file that took the copy's name and its inode
// number before the move is not the copy, and ErrGone says so, for the caller to resolve as a reprise would, which
// puts it back where it was (specs.md §17.1, step 4, §17.2). By its inode alone, a move to the trash took it for the
// copy, and sent a user's file there in its place. A rename, and the exchange of two names, compare the identity of
// §15.1 there, as their link path and their reprise do.
func TestTheLookAfterAMoveKnowsMoreThanTheInode(t *testing.T) {
	const trashed = "Atoll/trash/2026-01-01/7/Films/a"
	for _, tc := range notTheCopy {
		t.Run("to the trash, renamed/"+tc.name, func(t *testing.T) {
			mount := newMount(t)
			put(t, mount, "media/Films/a", "film")
			row := rowAt(t, mount, "Films/a")
			op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/a", Path2: "2026-01-01/7/Films/a", FileKey: row.FileKey.String}
			source := filepath.Join(mount, "media", "Films", "a")
			d := openAt(t, mount, once("rename", func() { tc.change(t, source) }, nil))
			_, err := d.ToTrash(op, row)
			if !errors.Is(err, ErrGone) {
				t.Fatalf("what moved was taken for the copy: %v", err)
			}
			// The move went through: the caller resolves it, as a reprise would (specs.md §17.2).
			if keyAt(t, mount, trashed) != row.FileKey.String || !absent(mount, "media/Films/a") {
				t.Error("the move did not go through")
			}
		})
		t.Run("to the trash, linked, the source edited then removed/"+tc.name, func(t *testing.T) {
			mount := newMount(t)
			put(t, mount, "media/Films/a", "film")
			row := rowAt(t, mount, "Films/a")
			op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/a", Path2: "2026-01-01/7/Films/a", FileKey: row.FileKey.String}
			source := filepath.Join(mount, "media", "Films", "a")
			// Between the link and the look at the source's name, both names lead to the file: an edit through the one
			// in the library, then that name removed, leaves the link the file's only name, and the look finds none.
			d := openAt(t, mount, onceAfter("link", "syncentry", func() {
				tc.change(t, source)
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			}, asNTFS3G))
			_, err := d.ToTrash(op, row)
			if !errors.Is(err, ErrGone) {
				t.Fatalf("what the link holds was taken for the copy: %v", err)
			}
			if keyAt(t, mount, trashed) != row.FileKey.String {
				t.Error("the link is gone")
			}
		})
		t.Run("a file renamed/"+tc.name, func(t *testing.T) {
			mount := newMount(t)
			put(t, mount, "media/a", "film")
			put(t, mount, "media/Films/other", "x")
			row := rowAt(t, mount, "a")
			op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRename, Path: "a", Path2: "Films/b", FileKey: row.FileKey.String}
			d := openAt(t, mount, once("rename", func() { tc.change(t, filepath.Join(mount, "media", "a")) }, nil))
			if _, _, err := d.RenameFile(op, row); !errors.Is(err, ErrGone) {
				t.Fatalf("what moved was taken for the copy: %v", err)
			}
		})
		t.Run("two files exchanged/"+tc.name, func(t *testing.T) {
			mount := newMount(t)
			put(t, mount, "media/a", "A")
			put(t, mount, "media/Films/b", "B")
			rowA, rowB := rowAt(t, mount, "a"), rowAt(t, mount, "Films/b")
			a := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRename, Path: "a", Path2: "Films/b", FileKey: rowA.FileKey.String}
			b := &Op{ID: 8, Disk: "t1", Kind: catalog.OpRename, Path: "Films/b", Path2: "a", FileKey: rowB.FileKey.String}
			d := openAt(t, mount, once("exchange", func() { tc.change(t, filepath.Join(mount, "media", "a")) }, nil))
			if _, _, _, _, err := d.ExchangeFiles(a, b, rowA, rowB); !errors.Is(err, ErrGone) {
				t.Fatalf("what moved was taken for the copy: %v", err)
			}
		})
	}
	// A move moves the change time, and so does a link: a change of nothing else is still the copy.
	t.Run("to the trash, the change time alone", func(t *testing.T) {
		mount := newMount(t)
		put(t, mount, "media/Films/a", "film")
		row := rowAt(t, mount, "Films/a")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/a", Path2: "2026-01-01/7/Films/a", FileKey: row.FileKey.String}
		source := filepath.Join(mount, "media", "Films", "a")
		time.Sleep(20 * time.Millisecond) // the change time must be able to move, within a clock tick
		d := openAt(t, mount, once("rename", func() {
			if err := os.Chmod(source, 0o600); err != nil {
				t.Fatal(err)
			}
		}, nil))
		if _, err := d.ToTrash(op, row); err != nil {
			t.Fatalf("the copy, its change time moved, was not taken for itself: %v", err)
		}
		if !holding(mount, trashed, "film") {
			t.Error("the copy is not in the trash")
		}
	})
}

// newMount is a new disk's top, with its root.
func newMount(t *testing.T) string {
	t.Helper()
	mount := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	return mount
}

// openAt opens an ext4 disk at mount whose calls go through fail, closed with the test.
func openAt(t *testing.T, mount string, fail func(call, name string) error) *Disk {
	t.Helper()
	d, err := Open(&onDisk{fail: fail}, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll", FS: names.Ext4}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// once runs fire right before the first call of a kind, then answers each call as driver does, when one is given.
func once(call string, fire func(), driver func(call, name string) error) func(string, string) error {
	return onceAfter("", call, fire, driver)
}

// onceAfter is once, for the first call of a kind that comes after one of another kind, when after is not empty.
func onceAfter(after, call string, fire func(), driver func(call, name string) error) func(string, string) error {
	armed, done := after == "", false
	return func(c, name string) error {
		switch {
		case !armed && c == after:
			armed = true
		case armed && !done && c == call:
			done = true
			fire()
		}
		if driver != nil {
			return driver(c, name)
		}
		return nil
	}
}

// writeInPlace writes a file's content again, keeping its inode, as a program that writes into it does.
func writeInPlace(t *testing.T, full, text string) {
	t.Helper()
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mtimeAt(t *testing.T, full string) time.Time {
	t.Helper()
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func setMtimeAt(t *testing.T, full string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(full, at, at); err != nil {
		t.Fatal(err)
	}
}
