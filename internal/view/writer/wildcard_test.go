package writer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/ops"
)

func (b *bench) count(query string) int {
	b.t.Helper()
	var n int
	if err := b.w.db.QueryRowContext(b.t.Context(), query).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// reopen opens a disk's handle again, as the view does at a mount of its own: it reads the type the disk is mounted with.
func (b *bench) reopen(id string) {
	b.t.Helper()
	h, err := ops.Open(b.machine, b.w.disk(id), 0)
	if err != nil {
		b.t.Fatal(err)
	}
	b.w.handles.Set(id, h)
}

// TestTheViewLeavesAWildcardNameOnTheTypeNTFS: through Atoll's own view, a file presented from a disk of the type ntfs,
// at a name holding a wildcard character, which that driver cannot move or remove without setting its whole volume
// read-only, is neither deleted nor renamed, and an empty folder of such a name is not removed: each is refused, EINVAL,
// before anything is journaled or asked of the disk, and stays (specs.md §15.8, §21.2). On ntfs3 the same deletion goes.
func TestTheViewLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	for _, fstype := range []string{"ntfs", "ntfs3"} {
		t.Run(fstype, func(t *testing.T) {
			b := newBench(t, "ntfs")
			ctx := t.Context()
			for _, disk := range []string{"t1", "t2"} {
				mustWrite(t, filepath.Join(b.mount[disk], "media", "Films", "q?.mkv"), "from "+disk)
			}
			if err := os.Mkdir(filepath.Join(b.mount["t2"], "media", "Films", "Empty*"), 0o755); err != nil {
				t.Fatal(err)
			}
			b.exec(`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (2, CAST('Films/q?.mkv' AS BLOB), 7, 9, 1, 'deferred', 'ok', 0, 0)`)
			b.exec(`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
				VALUES (2, 't1', 7, 9, 'atoll', 0, 200, 'ok', 420, 0, 0, 1), (2, 't2', 7, 9, 'user', 0, 100, 'ok', 420, 0, 0, 1)`)
			b.exec(`INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by) VALUES (CAST('Films/Empty*' AS BLOB), 't2', 'dir', 493, 5, 'user')`)
			b.machine.fsType[b.mount["t2"]] = fstype
			b.reopen("t2")
			if fstype == "ntfs3" {
				if err := b.w.Unlink(ctx, "Films/q?.mkv", root); err != nil {
					t.Errorf("a deletion on ntfs3: %v", err)
				}
				return
			}
			for what, err := range map[string]error{
				"a deletion":       b.w.Unlink(ctx, "Films/q?.mkv", root),
				"a rename":         b.w.Rename(ctx, "Films/q?.mkv", "Films/q.mkv", true, root),
				"a folder removed": b.w.Rmdir(ctx, "Films/Empty*", root),
			} {
				if !errors.Is(err, ErrRefused) && !errors.Is(err, ops.ErrNameRefused) {
					t.Errorf("%s: %v, want it refused", what, err)
				}
			}
			if b.on("t2", "Films/q?.mkv") != "from t2" || b.on("t1", "Films/q?.mkv") != "from t1" {
				t.Errorf("the copies: %q, %q", b.on("t2", "Films/q?.mkv"), b.on("t1", "Films/q?.mkv"))
			}
			if got := b.count("SELECT count(*) FROM ops") + b.count("SELECT count(*) FROM view_ops"); got != 0 {
				t.Errorf("%d operations journaled", got)
			}
		})
	}
}
