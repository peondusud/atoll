package view

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// TestAFileTakenInWhenItsOriginalMovedToAnotherCopy: a file the view made on t1, whose copy on t2, written in parallel
// as the program wrote it, has the older creation time, or the only one known (ntfs-3g gives none through statx), is
// presented from t2 by the catalog once a pass recorded both. Its operation is taken in all the same once the catalog
// holds the copy on t1 in sync at the size and time a look finds there, as nothing presented changes in a byte then; it
// was held for good before, holding back every later operation at that path (found on ntfs-3g by GitHub's qemu job,
// 2026-11-19).
func TestAFileTakenInWhenItsOriginalMovedToAnotherCopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		row   string // the copy on t1, as the catalog holds it
		size  int64  // what a look finds on t1
		taken bool
	}{
		{"in sync on its disk", "(1, 't1', 10, 9, 'user', 300, NULL, 'ok', 420, 1, 2, 1)", 10, true},
		{"its row behind what the disk holds", "(1, 't1', 10, 9, 'user', 300, NULL, 'ok', 420, 1, 2, 1)", 12, false},
		{"stale on its disk", "(1, 't1', 10, 9, 'user', 300, NULL, 'stale', 420, 1, 2, 1)", 10, false},
		{"not recorded on its disk", "", 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db := takenCatalog(t)
			copies := "(1, 't2', 10, 9, 'atoll', 300, 100, 'ok', 420, 1, 2, 1)"
			if tc.row != "" {
				copies += ", " + tc.row
			}
			for _, q := range []string{
				`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
					VALUES (1, x'46696c6d732f6e65772e6d6b76', 10, 9, 1, 'immediate', 'ok', 300, 300)`,
				`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
					VALUES ` + copies,
				`INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by) VALUES (x'46696c6d73', 't1', 'dir', 493, 5, 'user'), (x'46696c6d73', 't2', 'dir', 493, 5, 'user')`,
			} {
				if _, err := db.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			v := &catalog.ViewOp{Kind: catalog.ViewCreate, Disk: "t1", Path: "Films/new.mkv", AtNS: 10}
			if err := catalog.BeginViewOp(ctx, db, v); err != nil {
				t.Fatal(err)
			}
			if err := catalog.FinishViewOp(ctx, db, v, catalog.OpDone, false, 11); err != nil {
				t.Fatal(err)
			}
			open, err := catalog.OpenViewOps(ctx, db)
			if err != nil || len(open) != 1 {
				t.Fatalf("the view's operations: %+v, %v", open, err)
			}
			look := func(_ context.Context, disk, path string) (*Node, bool, error) {
				if disk != "t1" || path != "Films/new.mkv" {
					return nil, true, nil
				}
				return &Node{Kind: NodeFile, Disk: "t1", Size: tc.size, MtimeNS: 9, Mode: 0o644, Nlink: 1}, true, nil
			}
			taken, err := TakenIn(ctx, db, open, true, false, look, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(taken, open[0].ID); got != tc.taken {
				t.Errorf("taken in: %v, want %v", got, tc.taken)
			}
		})
	}
}

// takenCatalog makes a catalog of two online data disks, t1 and t2.
func takenCatalog(t *testing.T) *sql.DB {
	t.Helper()
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var list []catalog.Disk
	for i, id := range []string{"t1", "t2"} {
		list = append(list, catalog.Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	return db
}
