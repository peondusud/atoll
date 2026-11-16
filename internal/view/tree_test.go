package view

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

type rootsOf map[string]fsx.Dir

func (r rootsOf) Root(disk string) (fsx.Dir, bool) {
	d, ok := r[disk]
	return d, ok
}

// A tree from a catalog: the root, a listing that holds each name once and leaves out what presents nothing, a file
// read from the copy it presents, and statfs; a disk that cannot be read now fails only the read (specs.md §21.2).
func TestTree(t *testing.T) {
	top := t.TempDir()
	file := filepath.Join(top, "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	roots := rootsOf{}
	var list []catalog.Disk
	for i, id := range []string{"t1", "t2"} {
		dir := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(dir, "Films"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Films", "a.mkv"), []byte("from "+id), 0o644); err != nil {
			t.Fatal(err)
		}
		root, err := fsx.OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		roots[id] = root
		list = append(list, catalog.Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE disks SET capacity_bytes = 8192, free_bytes = 4096",
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (1, x'46696c6d732f612e6d6b76', 7, 9, 1, 'deferred', 'ok', 0, 0), (2, x'46696c6d732f7374616c652e6d6b76', 1, 1, 1, 'deferred', 'ok', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
			VALUES (1, 't1', 7, 9, 'user', 0, 200, 'ok', 420, 1, 2, 1), (1, 't2', 7, 9, 'atoll', 0, 100, 'ok', 384, 3, 4, 1), (2, 't1', 1, 1, 'user', 0, NULL, 'stale', NULL, NULL, NULL, NULL)`,
		`INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by, size, uid, gid) VALUES (x'46696c6d73', 't2', 'dir', 493, 5, 'user', 4096, 0, 0),
			(x'46696c6d73', 't1', 'dir', 448, 6, 'user', 4096, 1, 1)`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	tree := NewTree(db, roots, Owner{UID: 7, GID: 8}, 99)
	if root := tree.Root(); root.Kind != NodeDir || root.Mode != 0o755 || root.UID != 7 || root.MtimeNS != 99 {
		t.Errorf("the root: %+v", *root)
	}
	top1, err := tree.List(t.Context(), "")
	if err != nil || len(top1) != 1 || top1[0].Name != "Films" || top1[0].Node.Mode != 0o700 || top1[0].Node.Disk != "t1" {
		t.Errorf("the root's listing: %+v, %v", top1, err)
	}
	films, err := tree.List(t.Context(), "Films")
	if err != nil || len(films) != 1 || films[0].Name != "a.mkv" {
		t.Fatalf("a listing leaves out a file with only a stale copy: %+v, %v", films, err)
	}
	n := films[0].Node
	if n.Disk != "t2" || n.Mode != 0o600 || n.UID != 3 || n.Size != 7 {
		t.Errorf("the file presents its original, t2's copy: %+v", n)
	}
	f, err := tree.Open("Films/a.mkv", &n)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(data) != "from t2" {
		t.Errorf("read %q, %v", data, err)
	}
	if gone, err := tree.Lookup(t.Context(), "Films/stale.mkv"); err != nil || gone != nil {
		t.Errorf("a file with only a stale copy: %+v, %v", gone, err)
	}
	if s, err := tree.Statfs(t.Context()); err != nil || s.Blocks != 4 || s.Bfree != 2 {
		t.Errorf("statfs: %+v, %v", s, err)
	}
	delete(roots, "t2")
	if _, err := tree.Open("Films/a.mkv", &n); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a disk that cannot be read: %v", err)
	}
}
