package writer

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
)

// missedRename renames the folder Films to Movies through the view while t2 has no handle: the rename reaches t1
// alone, and t2 holds the folder at its old name until a pass takes the rename there. a.mkv is on t2 alone.
func missedRename(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "ext4")
	if err := os.Remove(filepath.Join(b.mount["t1"], "media", "Films", "a.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.w.db.ExecContext(t.Context(), "DELETE FROM copies WHERE disk_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	b.w.handles.Drop("t2")
	if err := b.w.Rename(t.Context(), "Films", "Movies", false, root); err != nil {
		t.Fatal(err)
	}
	if b.on("t2", "Films/a.mkv") != "from t2" || b.on("t2", "Movies/a.mkv") != "absent" {
		t.Fatalf("t2 holds %q at the old name, %q at the new", b.on("t2", "Films/a.mkv"), b.on("t2", "Movies/a.mkv"))
	}
	return b
}

// readThrough opens a file the view presents for reading, as a read through the kernel does, and gives what it holds.
func (b *bench) readThrough(path string) (string, error) {
	b.t.Helper()
	n := b.lookup(path)
	if n == nil {
		b.t.Fatalf("the view presents nothing at %s", path)
	}
	f, err := b.tree.Open(b.t.Context(), path, n)
	if err != nil {
		return "", err //nolint:wrapcheck // what the view answers
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	return string(data), err //nolint:wrapcheck // what the view answers
}

// A file the view presents below a folder it renamed opens for reading on a disk that still holds it at the old name,
// as a disk with no handle as the rename ran holds it until a pass takes the rename there (specs.md §21.2; observation
// 4 of conformance/models/README.md, the maintainer's remedy, 2026-11-28). It answered ENOENT for the window of a pass.
// An open to write gets what it got: the copy is written where the view presents it, and the folder is not there yet.
func TestAFileBelowAFolderRenamedOpensOnADiskThatMissedTheRename(t *testing.T) {
	b := missedRename(t)
	if got, err := b.readThrough("Movies/a.mkv"); err != nil || got != "from t2" {
		t.Errorf("read below the folder renamed, from the disk that missed it: %q, %v", got, err)
	}
	// t2 writable again, as a check of its disks gives it back its handle, and no pass since.
	h, err := ops.Open(b.machine, &b.w.config.Disks[1], 0)
	if err != nil {
		t.Fatal(err)
	}
	b.w.handles.Set("t2", h)
	n := b.lookup("Movies/a.mkv")
	if f, err := b.w.Open(t.Context(), "Movies/a.mkv", n, root); err == nil || !fsx.IsAbsent(err) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("an open to write through the old name: %v", err)
	}
	open, err := catalog.OpenViewOps(t.Context(), b.w.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range open {
		if v.Kind == "write" {
			t.Errorf("an open to write that opened nothing left its write: %+v", v)
		}
	}
	if b.on("t2", "Films/a.mkv") != "from t2" {
		t.Errorf("t2's copy at the old name: %q", b.on("t2", "Films/a.mkv"))
	}
}

// The copy gone from the old name too: what the presented name answered.
func TestAFileBelowAFolderRenamedWhoseOldNameIsGone(t *testing.T) {
	b := missedRename(t)
	if err := os.Remove(filepath.Join(b.mount["t2"], "media", "Films", "a.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.readThrough("Movies/a.mkv"); !fsx.IsAbsent(err) {
		t.Errorf("gone from both names: %v", err)
	}
}
