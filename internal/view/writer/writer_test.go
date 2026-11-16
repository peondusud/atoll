package writer

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
)

// machine is the disks as folders, with the free space a test gives each.
type machine struct {
	free map[string]int64
	now  int64
}

func (m *machine) Mount(string) disks.MountSeen { return disks.MountSeen{} }
func (m *machine) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (m *machine) Root(string, string) disks.RootSeen { return disks.RootSeen{} }
func (m *machine) ReadPastCache(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (m *machine) Checkpoint(string) {}
func (m *machine) Now() int64 {
	m.now += 1_000_000_000
	return m.now
}
func (m *machine) Space(mount string) (int64, int64, error) { return 1000, m.free[mount], nil }
func (m *machine) OpenRoot(mount, root string) (fsx.Dir, error) {
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}
func (m *machine) OpenTop(mount string) (fsx.Dir, error) {
	return fsx.OpenDir(mount) //nolint:wrapcheck // a test double
}

type bench struct {
	t     *testing.T
	top   string
	db    interface{ Close() error }
	tree  *view.Tree
	w     *Writer
	mount map[string]string
}

func newBench(t *testing.T, t2fs string) *bench {
	t.Helper()
	top := t.TempDir()
	b := &bench{t: t, top: top, mount: map[string]string{}}
	m := &machine{free: map[string]int64{}}
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = \"/srv/pool\"\n[placement]\nmin_free = [\"5%%\"]\n", filepath.Join(top, "catalog.db"))
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(top, id)
		b.mount[id] = mount
		mustWrite(t, filepath.Join(mount, "media", "Films", "a.mkv"), "from "+id)
		fs := "ext4"
		if id == "t2" {
			fs = t2fs
		}
		uuid := fmt.Sprintf("3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d", i)
		if fs == "ntfs" {
			uuid = fmt.Sprintf("0123456789ABCDE%d", i)
		}
		text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = %q\n", id, uuid, mount, fs)
	}
	m.free[b.mount["t1"]], m.free[b.mount["t2"]] = 500, 50
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(top, "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var rows []catalog.Disk
	for i := range c.Disks {
		d := &c.Disks[i]
		rows = append(rows, catalog.Disk{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: "data", Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, rows); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE disks SET state = 'online'",
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, CAST('Films/a.mkv' AS BLOB), 7, 9, 1, 'deferred', 'ok', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
			VALUES (1, 't1', 7, 9, 'atoll', 0, 200, 'ok', 420, 0, 0, 1), (1, 't2', 7, 9, 'user', 0, 100, 'ok', 420, 0, 0, 1)`,
		`INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by) VALUES (CAST('Films' AS BLOB), 't1', 'dir', 493, 5, 'user'), (CAST('Films' AS BLOB), 't2', 'dir', 493, 5, 'user')`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	roots := view.NewDiskRoots()
	t.Cleanup(roots.Close)
	handles := NewHandles()
	t.Cleanup(handles.Close)
	for i := range c.Disks {
		d := &c.Disks[i]
		root, err := m.OpenRoot(d.Mount, d.Root)
		if err != nil {
			t.Fatal(err)
		}
		roots.Set(d.ID, root)
		h, err := ops.Open(m, d, 0)
		if err != nil {
			t.Fatal(err)
		}
		handles.Set(d.ID, h)
	}
	b.tree = view.NewTree(db, roots, view.Owner{UID: 0, GID: 0}, 1)
	if b.w, err = New(db, c, m, b.tree, handles, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	b.db = db
	return b
}

func (b *bench) lookup(path string) *view.Node {
	b.t.Helper()
	n, err := b.tree.Lookup(b.t.Context(), path)
	if err != nil {
		b.t.Fatalf("%s: %v", path, err)
	}
	return n
}

func (b *bench) on(disk, path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.mount[disk], "media", path))
	if err != nil {
		return "absent"
	}
	return string(data)
}

func (b *bench) create(path, text string) {
	b.t.Helper()
	f, _, err := b.w.Create(b.t.Context(), path, 0o644, Owner{})
	if err != nil {
		b.t.Fatalf("create %s: %v", path, err)
	}
	if _, err := f.WriteAt([]byte(text), 0); err != nil {
		b.t.Fatal(err)
	}
	_ = f.Close()
}

// The writer (specs.md §21.2): what is new goes to the disk with the most room past min_free; a file written changes
// its original alone; a rename is made on the file's disk, and a folder's on every disk that holds it; a deletion
// sends the copy presented to the trash; a rename onto a name of the same disk replaces it, onto another's is
// refused as across two filesystems; an empty folder goes; and each is what the view then presents.
func TestWriter(t *testing.T) {
	b := newBench(t, "ext4")
	ctx := t.Context()

	b.create("Films/new.mkv", "new")
	if b.on("t1", "Films/new.mkv") != "new" {
		t.Errorf("a new file is not on the disk with the most room: t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t1" || n.Size != 3 {
		t.Errorf("a new file presents: %+v", n)
	}
	if _, _, err := b.w.Create(ctx, "Films/new.mkv", 0o644, Owner{}); !errors.Is(err, ErrExists) {
		t.Errorf("made twice: %v", err)
	}
	if _, _, err := b.w.Create(ctx, "Nowhere/x.mkv", 0o644, Owner{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("made in a folder that presents nothing: %v", err)
	}

	f, err := b.w.Open(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("FROM"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if b.on("t2", "Films/a.mkv") != "FROM t2" || b.on("t1", "Films/a.mkv") != "from t1" {
		t.Errorf("a write reaches the original alone: t1 %q, t2 %q", b.on("t1", "Films/a.mkv"), b.on("t2", "Films/a.mkv"))
	}

	if _, err := b.w.Mkdir(ctx, "Series", 0o750, Owner{}); err != nil {
		t.Fatal(err)
	}
	b.create("Series/e01.mkv", "e01")
	if err := b.w.Rmdir(ctx, "Series"); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("a folder that presents something removed: %v", err)
	}

	if err := b.w.Rename(ctx, "Films/new.mkv", "Films/renamed.mkv", false); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Films/new.mkv"); n != nil {
		t.Errorf("a file renamed, at its old name: %+v", n)
	}
	if err := b.w.Rename(ctx, "Films", "Movies", false); err != nil {
		t.Fatal(err)
	}
	for _, disk := range []string{"t1", "t2"} {
		if b.on(disk, "Movies/a.mkv") == "absent" {
			t.Errorf("a folder renamed is not renamed on %s", disk)
		}
	}
	if n := b.lookup("Movies/a.mkv"); n == nil || n.Disk != "t2" {
		t.Errorf("below a folder renamed: %+v", n)
	}
	if n := b.lookup("Movies/renamed.mkv"); n == nil || n.Disk != "t1" {
		t.Errorf("a file renamed, then its folder: %+v", n)
	}

	if err := b.w.Unlink(ctx, "Movies/renamed.mkv"); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Movies/renamed.mkv"); n != nil || b.on("t1", "Movies/renamed.mkv") != "absent" {
		t.Errorf("a file deleted: %+v, %q", n, b.on("t1", "Movies/renamed.mkv"))
	}
	entries, err := catalog.TrashEntries(ctx, b.w.db)
	if err != nil || len(entries) != 1 || entries[0].Reason != "deleted" || entries[0].Disk != "t1" {
		t.Errorf("the trash: %+v, %v", entries, err)
	}

	b.create("Movies/a.nfo", "old nfo")
	b.create("Movies/a.nfo.tmp", "new nfo")
	if err := b.w.Rename(ctx, "Movies/a.nfo.tmp", "Movies/a.nfo", true); !errors.Is(err, ErrExists) {
		t.Errorf("a rename that must replace nothing: %v", err)
	}
	if err := b.w.Rename(ctx, "Movies/a.nfo.tmp", "Movies/a.nfo", false); err != nil {
		t.Fatal(err)
	}
	if b.on("t1", "Movies/a.nfo") != "new nfo" || b.on("t1", "Movies/a.nfo.tmp") != "absent" {
		t.Errorf("an atomic save: %q, %q", b.on("t1", "Movies/a.nfo"), b.on("t1", "Movies/a.nfo.tmp"))
	}
	if entries, err = catalog.TrashEntries(ctx, b.w.db); err != nil || len(entries) != 2 {
		t.Errorf("the trash after a replacement: %+v, %v", entries, err)
	}
	if err := b.w.Rename(ctx, "Movies/a.nfo", "Movies/a.mkv", false); !errors.Is(err, ErrCrossDisk) {
		t.Errorf("onto a name presented from another disk: %v", err)
	}

	if err := b.w.Unlink(ctx, "Series/e01.mkv"); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Rmdir(ctx, "Series"); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Series"); n != nil || b.on("t1", "Series") != "absent" {
		t.Errorf("an empty folder removed: %+v", n)
	}
	if err := b.w.Rename(ctx, "Movies", "Movies/Inner", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("a folder into itself: %v", err)
	}
}

// A rename that a disk holding a copy refuses is refused, and nothing changes (specs.md §15.8, §21.2): t2 is NTFS,
// which refuses ':'.
func TestWriterRefusesANameADiskRefuses(t *testing.T) {
	b := newBench(t, "ntfs")
	if err := b.w.Rename(t.Context(), "Films/a.mkv", "Films/a:b.mkv", false); !errors.Is(err, ErrRefused) {
		t.Errorf("a name NTFS refuses: %v", err)
	}
	if b.on("t2", "Films/a.mkv") != "from t2" {
		t.Errorf("something changed: %q", b.on("t2", "Films/a.mkv"))
	}
	if err := b.w.Rename(t.Context(), "Films", "Fi:lms", false); !errors.Is(err, ErrRefused) {
		t.Errorf("a folder name NTFS refuses: %v", err)
	}
	// Written through the view, the file presents its original alone, while its other copy, on NTFS, is still on its
	// disk until a pass: that copy refuses the name all the same. The original is t1's, on ext4.
	if _, err := b.w.db.ExecContext(t.Context(), "UPDATE copies SET btime_ns = 50 WHERE disk_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Films/a.mkv"); n == nil || n.Disk != "t1" {
		t.Fatalf("the original: %+v", n)
	}
	f, err := b.w.Open(t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := b.w.Rename(t.Context(), "Films/a.mkv", "Films/a:b.mkv", false); !errors.Is(err, ErrRefused) {
		t.Errorf("a name NTFS refuses, once written through the view: %v", err)
	}
}

// What the view left planned is resolved before it mounts (specs.md §21.2): a file made is done, one that never was
// is undone.
func TestWriterReprise(t *testing.T) {
	b := newBench(t, "ext4")
	ctx := t.Context()
	mustWrite(t, filepath.Join(b.mount["t1"], "media", "Films", "made.mkv"), "x")
	made := &catalog.ViewOp{Kind: catalog.ViewCreate, Disk: "t1", Path: "Films/made.mkv"}
	never := &catalog.ViewOp{Kind: catalog.ViewCreate, Disk: "t1", Path: "Films/never.mkv"}
	for _, v := range []*catalog.ViewOp{made, never} {
		if err := catalog.BeginViewOp(ctx, b.w.db, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.w.Reprise(ctx); err != nil {
		t.Fatal(err)
	}
	open, err := catalog.OpenViewOps(ctx, b.w.db)
	if err != nil || len(open) != 1 || open[0].ID != made.ID || open[0].State != catalog.OpDone {
		t.Errorf("after the reprise: %+v, %v", open, err)
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
