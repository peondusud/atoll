//go:build loopdev

package loopdev

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// TestViewMovesAFileThatRunsOutOfSpace: a new file written through Atoll's own view, served as atoll mount serves it
// with view.move_on_enospc, lands on a small ext4 disk, the first of the order, and is written past what that disk
// holds (specs.md §21.2): the write the disk answers with ENOSPC moves the file to the other disk, and the program never
// sees the error. Once it closed the file, the other disk holds every byte it wrote, as the view presents it; the small
// disk holds nothing at its path, nor in its trash, its copy purged from there; the move's operations are done; and the
// pass that follows records the file on the disk it went to and takes the view's operations in.
func TestViewMovesAFileThatRunsOutOfSpace(t *testing.T) {
	needRoot(t)
	small := newVolumeSized(t, names.Ext4, 24)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchKept(t, small, v, at, benchGuards, "[placement]\nnew_files = \"ordered\"\nfill_to = \"99%\"\nmin_free = [\"1MiB\"]\n")
	text := strings.Replace(b.configText, "[view]\n", "[view]\nmove_on_enospc = true\n", 1)
	c, err := config.Parse([]byte(text))
	mustDo(t, err)
	b.config, b.configText = c, text
	b.scan(false)
	point := filepath.Join(t.TempDir(), "pool")
	mustDo(t, os.MkdirAll(point, 0o755))
	served, err := serveView(t, b, point, true)
	mustDo(t, err)
	defer func() {
		served.stop()
		mustDo(t, served.wait())
	}()

	// 40 MiB, past the 24 of the small disk, written a MiB at a time as a program writes a download.
	chunk := bytes.Repeat([]byte("a film that does not fit, "), (1<<20)/26+1)[:1<<20]
	f, err := os.Create(filepath.Join(point, "Big.mkv"))
	mustDo(t, err)
	var want []byte
	for range 40 {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("a write through the view, at %d MiB: %v", len(want)>>20, err)
		}
		want = append(want, chunk...)
	}
	mustDo(t, f.Close())

	through, err := os.ReadFile(filepath.Join(point, "Big.mkv"))
	mustDo(t, err)
	if !bytes.Equal(through, want) {
		t.Fatalf("the view gives %d bytes of other content, want %d", len(through), len(want))
	}
	moved, err := os.ReadFile(filepath.Join(b.at, "media", "Big.mkv"))
	mustDo(t, err)
	if !bytes.Equal(moved, want) {
		t.Fatalf("the disk it moved to holds %d bytes of other content", len(moved))
	}
	if _, err := os.Stat(filepath.Join(b.keep, "media", "Big.mkv")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the small disk still holds it: %v", err)
	}
	left := 0
	_ = filepath.WalkDir(filepath.Join(b.keep, "Atoll", "trash"), func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			left++
		}
		return nil
	})
	if left != 0 {
		t.Errorf("the small disk's trash holds %d files", left)
	}
	if got := b.text("SELECT group_concat(v.disk2_id || '>' || v.disk_id || ' ' || v.state || ' ' || o.kind || ' ' || o.state, ', ') FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'move'"); got != "k>v done unlink done" {
		t.Errorf("the moves: %q", got)
	}

	b.settle()
	if got := b.text("SELECT group_concat(c.disk_id || ' ' || c.created_by, ', ') FROM copies c JOIN files f ON f.id = c.file_id WHERE CAST(f.lpath AS TEXT) = 'Big.mkv'"); got != "v user" {
		t.Errorf("the copies the pass recorded: %q", got)
	}
	open, err := catalog.OpenViewOps(t.Context(), b.db)
	mustDo(t, err)
	if len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
}
