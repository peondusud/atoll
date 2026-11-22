package view

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

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
	roots := NewDiskRoots()
	defer roots.Close()
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
		roots.Set(id, root)
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
	// t1's folder, 0700 of 1:1, narrowed by t2's, 0755 of root's: there, its owner is of the group or of the others.
	if err != nil || len(top1) != 1 || top1[0].Name != "Films" || top1[0].Node.Mode != 0o500 || top1[0].Node.Disk != "t1" {
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
	f, err := tree.Open(t.Context(), "Films/a.mkv", &n)
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
	// The original that has not followed its file's rename sits at its old name, and the other copy presents; once
	// found again and its clock running, the same (specs.md §13.4).
	for _, q := range []string{
		"UPDATE copies SET old_lpath = x'612e6d6b76' WHERE file_id = 1 AND disk_id = 't2'",
		"UPDATE copies SET old_lpath = NULL, gone_ns = 3, gone_kind = 'moved' WHERE file_id = 1 AND disk_id = 't2'",
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
		if got, err := tree.Lookup(t.Context(), "Films/a.mkv"); err != nil || got == nil || got.Disk != "t1" {
			t.Errorf("%s: presents %+v, %v", q, got, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE copies SET gone_ns = NULL, gone_kind = NULL WHERE file_id = 1 AND disk_id = 't2'"); err != nil {
		t.Fatal(err)
	}
	if s, err := tree.Statfs(t.Context()); err != nil || s.Blocks != 4 || s.Bfree != 2 {
		t.Errorf("statfs: %+v, %v", s, err)
	}
	roots.Drop("t2")
	if _, err := tree.Open(t.Context(), "Films/a.mkv", &n); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a disk that cannot be read: %v", err)
	}
}

// The view's own operations over the catalog (specs.md §21.2): a file made presents what its disk holds and lists;
// a file written presents its original alone, as its disk holds it; a file deleted presents nothing; below a folder
// renamed, what the catalog holds below its old name presents at the new one and reads there, before and after a pass
// records the rename; and once taken in, the catalog alone decides.
func TestTreeOverTheViewsOperations(t *testing.T) {
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
	roots := NewDiskRoots()
	defer roots.Close()
	var list []catalog.Disk
	for i, id := range []string{"t1", "t2"} {
		dir := filepath.Join(top, id)
		mustWrite(t, filepath.Join(dir, "Films", "a.mkv"), "from "+id)
		root, err := fsx.OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		roots.Set(id, root)
		list = append(list, catalog.Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (1, x'46696c6d732f612e6d6b76', 7, 9, 1, 'deferred', 'ok', 0, 0), (2, x'46696c6d732f622e6d6b76', 7, 9, 1, 'deferred', 'ok', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
			VALUES (1, 't1', 7, 9, 'atoll', 0, 200, 'ok', 420, 1, 2, 1), (1, 't2', 7, 9, 'user', 0, 100, 'ok', 420, 1, 2, 1),
			(2, 't1', 7, 9, 'user', 0, 100, 'ok', 420, 1, 2, 1)`,
		`INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by) VALUES (x'46696c6d73', 't1', 'dir', 493, 5, 'user'), (x'46696c6d73', 't2', 'dir', 493, 5, 'user')`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	tree := NewTree(db, roots, Owner{UID: 7, GID: 8}, 99)
	journal := func(v *catalog.ViewOp) {
		t.Helper()
		if err := catalog.BeginViewOp(t.Context(), db, v); err != nil {
			t.Fatal(err)
		}
	}
	lookup := func(path string) *Node {
		t.Helper()
		n, err := tree.Lookup(t.Context(), path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return n
	}
	names := func(dir string) []string {
		t.Helper()
		children, err := tree.List(t.Context(), dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		var out []string
		for _, c := range children {
			out = append(out, c.Name)
		}
		return out
	}

	mustWrite(t, filepath.Join(top, "t1", "Films", "new.mkv"), "a new film")
	journal(&catalog.ViewOp{Kind: catalog.ViewCreate, Disk: "t1", Path: "Films/new.mkv", AtNS: 10})
	if n := lookup("Films/new.mkv"); n == nil || n.Disk != "t1" || n.Size != 10 || n.Kind != NodeFile {
		t.Errorf("a file made: %+v", n)
	}
	if got := names("Films"); !slices.Equal(got, []string{"a.mkv", "b.mkv", "new.mkv"}) {
		t.Errorf("listed with a file made: %v", got)
	}

	if n := lookup("Films/a.mkv"); n == nil || n.Disk != "t2" {
		t.Fatalf("before the write, the original: %+v", n)
	}
	mustWrite(t, filepath.Join(top, "t1", "Films", "a.mkv"), "rewritten on t1")
	journal(&catalog.ViewOp{Kind: catalog.ViewWrite, Disk: "t1", Path: "Films/a.mkv", AtNS: 11})
	if n := lookup("Films/a.mkv"); n == nil || n.Disk != "t1" || n.Size != 15 {
		t.Errorf("a file written presents the copy written, as its disk holds it: %+v", n)
	}

	journal(&catalog.ViewOp{Kind: catalog.ViewUnlink, Disk: "t1", Path: "Films/b.mkv", AtNS: 12})
	if n := lookup("Films/b.mkv"); n != nil {
		t.Errorf("a file deleted: %+v", n)
	}
	if got := names("Films"); !slices.Equal(got, []string{"a.mkv", "new.mkv"}) {
		t.Errorf("listed with a file deleted: %v", got)
	}

	for _, id := range []string{"t1", "t2"} {
		if err := os.Rename(filepath.Join(top, id, "Films"), filepath.Join(top, id, "Movies")); err != nil {
			t.Fatal(err)
		}
	}
	journal(&catalog.ViewOp{Kind: catalog.ViewRename, Disk: "t1", Path: "Films", Path2: "Movies", Folder: true, AtNS: 13})
	if n := lookup("Films"); n != nil {
		t.Errorf("a folder renamed, at its old name: %+v", n)
	}
	if got := names(""); !slices.Equal(got, []string{"Movies"}) {
		t.Errorf("the root with a folder renamed: %v", got)
	}
	if got := names("Movies"); !slices.Equal(got, []string{"a.mkv", "new.mkv"}) {
		t.Errorf("a folder renamed lists: %v", got)
	}
	n := lookup("Movies/a.mkv")
	if n == nil || n.Disk != "t1" {
		t.Fatalf("below a folder renamed: %+v", n)
	}
	read := func(path string, n *Node) string {
		t.Helper()
		f, err := tree.Open(t.Context(), path, n)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := read("Movies/a.mkv", n); got != "rewritten on t1" {
		t.Errorf("read below a folder renamed: %q", got)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE files SET lpath = CAST('Movies/' || substr(CAST(lpath AS TEXT), 7) AS BLOB)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE entries SET lpath = CAST('Movies' AS BLOB)"); err != nil {
		t.Fatal(err)
	}
	if n := lookup("Movies/b.mkv"); n != nil {
		t.Errorf("once a pass recorded the rename, the file deleted: %+v", n)
	}
	if got := names("Movies"); !slices.Equal(got, []string{"a.mkv", "new.mkv"}) {
		t.Errorf("once a pass recorded the rename: %v", got)
	}

	open, err := catalog.OpenViewOps(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, v := range open {
		ids = append(ids, v.ID)
	}
	if err := catalog.TakeViewOps(t.Context(), db, ids, 20); err != nil {
		t.Fatal(err)
	}
	if n := lookup("Movies/a.mkv"); n == nil || n.Disk != "t2" {
		t.Errorf("taken in, the catalog alone decides: %+v", n)
	}
	if n := lookup("Movies/new.mkv"); n != nil {
		t.Errorf("taken in before the catalog knows it, a file made presents nothing: %+v", n)
	}
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
