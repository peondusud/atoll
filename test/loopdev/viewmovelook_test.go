//go:build loopdev

package loopdev

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// TestViewMoveKeepsTheCopyLeftWhenTheTargetGoes: as TestViewMovesAFileThatRunsOutOfSpace, a new file written through
// Atoll's own view past what its small ext4 disk holds moves to the other disk; but a user deletes the moved copy on
// that disk directly, once the move sent the copy left on the small disk to its trash, or tried to, and before it is
// purged or removed at its path. The look at the target right before finds the moved copy gone, and the copy left is
// kept, in the small disk's trash or at its path, its unlink undone, the move done (specs.md §21.2; flaw 17 of
// conformance/models/README.md, the maintainer's remedy, 2026-11-23): on a real filesystem, the copy left holds what
// the program wrote before the move, and the moved copy's change time, read right after its rename, is what the look
// compares.
func TestViewMoveKeepsTheCopyLeftWhenTheTargetGoes(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name string
		mkfs []string
		// noInode fills the small disk's inodes but one, which the file takes.
		noInode bool
		// here says that the copy left would be removed at its path, not from the trash; at is the call on the small
		// disk before which the user deletes the moved copy: the trash's mkdir, or its rename.
		here bool
		at   string
	}{
		{name: "at its path", mkfs: []string{"-N", "128"}, noInode: true, here: true, at: "mkdir"},
		{name: "in the trash", mkfs: []string{"-O", "inline_data", "-I", "256"}, at: "rename"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := newVolumeSized(t, names.Ext4, 24, tc.mkfs...)
			v := newVolume(t, names.Ext4)
			at := mountPoint(t)
			mountVolume(t, v, "", at)
			b := newPassBenchKept(t, small, v, at, benchGuards, "[placement]\nnew_files = \"ordered\"\nfill_to = \"99%\"\nmin_free = [\"1MiB\"]\n")
			text := strings.Replace(b.configText, "[view]\n", "[view]\nmove_on_enospc = true\n", 1)
			c, err := config.Parse([]byte(text))
			mustDo(t, err)
			b.config, b.configText = c, text
			b.scan(false)
			var mu sync.Mutex
			armed, deleted := false, false
			moved := filepath.Join(b.at, "media", "Big.mkv")
			b.machine = &faultTop{Machine: b.machine, fail: map[string]func(string, string) error{b.keep: func(call, name string) error {
				mu.Lock()
				defer mu.Unlock()
				if armed && !deleted && call == tc.at && (call == "mkdir" || name == "Big.mkv") {
					// The moved copy is at its name on the other disk by now: a user deletes it there.
					if _, err := os.Stat(moved); err == nil {
						deleted = true
						mustDo(t, os.Remove(moved))
					}
				}
				return nil
			}}}
			point := filepath.Join(t.TempDir(), "pool")
			mustDo(t, os.MkdirAll(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			defer func() {
				served.stop()
				mustDo(t, served.wait())
			}()
			if tc.noInode {
				fillInodes(t, b.keep, 1)
			}

			chunk := bytes.Repeat([]byte("a film that does not fit, "), (1<<20)/26+1)[:1<<20]
			f, err := os.Create(filepath.Join(point, "Big.mkv"))
			mustDo(t, err)
			var want []byte
			for i := range 40 {
				if i == 1 {
					mu.Lock()
					armed = true
					mu.Unlock()
				}
				if _, err := f.Write(chunk); err != nil {
					t.Fatalf("a write through the view, at %d MiB: %v", i, err)
				}
				want = append(want, chunk...)
			}
			mustDo(t, f.Close())
			mu.Lock()
			gone := deleted
			mu.Unlock()
			if !gone {
				t.Fatal("the moved copy was never deleted on the other disk")
			}
			// The copy left, as the program wrote it before the move: what the kernel passed on of it by then, in pieces of
			// its own size, the start of what the program wrote.
			var left []byte
			if tc.here {
				left, err = os.ReadFile(filepath.Join(b.keep, "media", "Big.mkv"))
				mustDo(t, err)
			} else {
				_ = filepath.WalkDir(filepath.Join(b.keep, "Atoll", "trash"), func(path string, e fs.DirEntry, err error) error {
					if err == nil && !e.IsDir() && e.Name() == "Big.mkv" {
						left, err = os.ReadFile(path)
						mustDo(t, err)
					}
					return nil
				})
			}
			if len(left) == 0 || len(left) >= len(want) || !bytes.Equal(left, want[:len(left)]) {
				t.Errorf("the copy left holds %d bytes, want the start of the %d the program wrote", len(left), len(want))
			}
			if _, err := os.Stat(moved); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the other disk holds the file the user deleted: %v", err)
			}
			moves := "k>v done unlink undone in the trash"
			if tc.here {
				moves = "k>v done unlink undone at its path"
			}
			if got := b.text("SELECT group_concat(v.disk2_id || '>' || v.disk_id || ' ' || v.state || ' ' || o.kind || ' ' || o.state || ' ' || CASE WHEN o.path2 IS NULL THEN 'at its path' ELSE 'in the trash' END, ', ') FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'move'"); got != moves {
				t.Errorf("the moves: %q, want %q", got, moves)
			}
		})
	}
}
