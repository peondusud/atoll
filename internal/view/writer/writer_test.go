package writer

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
)

// machine is the disks as folders, with the free space a test gives each, mounted with the type a test gives each,
// ext4 when it gives none.
type machine struct {
	free   map[string]int64
	fsType map[string]string
	now    int64
	// fail makes calls on a disk fail, by mount, as fsx.Fault does; a part's writer reads it as a test sets it.
	mu   sync.Mutex
	fail map[string]func(call, name string) error
}

func (m *machine) setFail(mount string, fail func(call, name string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail == nil {
		m.fail = map[string]func(string, string) error{}
	}
	m.fail[mount] = fail
}

func (m *machine) failOf(mount string) func(call, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fail[mount]
}

func (m *machine) Mount(mount string) disks.MountSeen {
	fsType := m.fsType[mount]
	if fsType == "" {
		fsType = "ext4"
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{MountPoint: mount, Root: "/", FSType: fsType}}
}
func (m *machine) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (m *machine) Root(string, string) disks.RootSeen { return disks.RootSeen{} }
func (m *machine) Hiberfil(string, *disks.MountEntry) disks.HiberfilSeen {
	return disks.HiberfilSeen{File: disks.HiberfilAbsent}
}
func (m *machine) ReadPastCache(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (m *machine) Checkpoint(string) {}
func (m *machine) Now() int64 {
	// A part's end is written apart from the program, and asks the time too.
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now += 1_000_000_000
	return m.now
}
func (m *machine) Space(mount string) (int64, int64, error) { return 1000, m.free[mount], nil }
func (m *machine) OpenRoot(mount, root string) (fsx.Dir, error) {
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}
func (m *machine) OpenTop(mount string) (fsx.Dir, error) {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return fsx.Fault(top, func(call, name string) error {
		if fail := m.failOf(mount); fail != nil {
			return fail(call, name)
		}
		return nil
	}), nil
}

type bench struct {
	t       *testing.T
	top     string
	db      interface{ Close() error }
	tree    *view.Tree
	w       *Writer
	mount   map[string]string
	machine *machine
}

func newBench(t *testing.T, t2fs string) *bench {
	t.Helper()
	return newBenchWith(t, t2fs, "", 500, 50)
}

// newBenchWith is newBench with more keys of [placement], and the free space of each disk, of 1000 bytes.
func newBenchWith(t *testing.T, t2fs, placement string, free1, free2 int64) *bench {
	t.Helper()
	return newBenchEdited(t, t2fs, placement, free1, free2, nil)
}

// newBenchEdited is newBenchWith, whose configuration edit changes once its disks are in it.
func newBenchEdited(t *testing.T, t2fs, placement string, free1, free2 int64, edit func(b *bench, text string) string) *bench {
	t.Helper()
	top := t.TempDir()
	b := &bench{t: t, top: top, mount: map[string]string{}}
	m := &machine{free: map[string]int64{}, fsType: map[string]string{}}
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = \"/srv/pool\"\n[placement]\nmin_free = [\"5%%\"]\n", filepath.Join(top, "catalog.db")) + placement
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
	m.free[b.mount["t1"]], m.free[b.mount["t2"]] = free1, free2
	if edit != nil {
		text = edit(b, text)
	}
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
		rows = append(rows, catalog.Disk{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role, Position: i, State: "online"})
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
	settings, err := c.Cache()
	if err != nil {
		t.Fatal(err)
	}
	b.tree = view.NewTree(db, roots, view.Owner{UID: 0, GID: 0}, 1).WithCache(settings.InView)
	if b.w, err = New(db, c, m, b.tree, handles, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	b.db = db
	b.machine = m
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
	f, _, err := b.w.Create(b.t.Context(), path, 0o644, root)
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
	if _, _, err := b.w.Create(ctx, "Films/new.mkv", 0o644, root); !errors.Is(err, ErrExists) {
		t.Errorf("made twice: %v", err)
	}
	if _, _, err := b.w.Create(ctx, "Nowhere/x.mkv", 0o644, root); !errors.Is(err, ErrNotFound) {
		t.Errorf("made in a folder that presents nothing: %v", err)
	}

	f, err := b.w.Open(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), root)
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

	if _, err := b.w.Mkdir(ctx, "Series", 0o750, root); err != nil {
		t.Fatal(err)
	}
	b.create("Series/e01.mkv", "e01")
	if err := b.w.Rmdir(ctx, "Series", root); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("a folder that presents something removed: %v", err)
	}

	if err := b.w.Rename(ctx, "Films/new.mkv", "Films/renamed.mkv", false, root); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Films/new.mkv"); n != nil {
		t.Errorf("a file renamed, at its old name: %+v", n)
	}
	if err := b.w.Rename(ctx, "Films", "Movies", false, root); err != nil {
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

	if err := b.w.Unlink(ctx, "Movies/renamed.mkv", root); err != nil {
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
	if err := b.w.Rename(ctx, "Movies/a.nfo.tmp", "Movies/a.nfo", true, root); !errors.Is(err, ErrExists) {
		t.Errorf("a rename that must replace nothing: %v", err)
	}
	if err := b.w.Rename(ctx, "Movies/a.nfo.tmp", "Movies/a.nfo", false, root); err != nil {
		t.Fatal(err)
	}
	if b.on("t1", "Movies/a.nfo") != "new nfo" || b.on("t1", "Movies/a.nfo.tmp") != "absent" {
		t.Errorf("an atomic save: %q, %q", b.on("t1", "Movies/a.nfo"), b.on("t1", "Movies/a.nfo.tmp"))
	}
	if entries, err = catalog.TrashEntries(ctx, b.w.db); err != nil || len(entries) != 2 {
		t.Errorf("the trash after a replacement: %+v, %v", entries, err)
	}
	if err := b.w.Rename(ctx, "Movies/a.nfo", "Movies/a.mkv", false, root); !errors.Is(err, ErrCrossDisk) {
		t.Errorf("onto a name presented from another disk: %v", err)
	}

	if err := b.w.Unlink(ctx, "Series/e01.mkv", root); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Rmdir(ctx, "Series", root); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Series"); n != nil || b.on("t1", "Series") != "absent" {
		t.Errorf("an empty folder removed: %+v", n)
	}
	if err := b.w.Rename(ctx, "Movies", "Movies/Inner", false, root); !errors.Is(err, ErrInvalid) {
		t.Errorf("a folder into itself: %v", err)
	}
}

// A rename that a disk holding a copy refuses is refused, and nothing changes (specs.md §15.8, §21.2): t2 is NTFS,
// which refuses ':'.
func TestWriterRefusesANameADiskRefuses(t *testing.T) {
	b := newBench(t, "ntfs")
	if err := b.w.Rename(t.Context(), "Films/a.mkv", "Films/a:b.mkv", false, root); !errors.Is(err, ErrRefused) {
		t.Errorf("a name NTFS refuses: %v", err)
	}
	if b.on("t2", "Films/a.mkv") != "from t2" {
		t.Errorf("something changed: %q", b.on("t2", "Films/a.mkv"))
	}
	if err := b.w.Rename(t.Context(), "Films", "Fi:lms", false, root); !errors.Is(err, ErrRefused) {
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
	f, err := b.w.Open(t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), root)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := b.w.Rename(t.Context(), "Films/a.mkv", "Films/a:b.mkv", false, root); !errors.Is(err, ErrRefused) {
		t.Errorf("a name NTFS refuses, once written through the view: %v", err)
	}
}

// inode gives the inode of a path on a disk, 0 when nothing is there.
func (b *bench) inode(disk, path string) uint64 {
	b.t.Helper()
	info, err := os.Lstat(filepath.Join(b.mount[disk], "media", path))
	if err != nil {
		return 0
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// A hard link through the view is made on the disk the file is presented from, its original, as a second name of the
// same file there, after the folders that disk lacks on the way; a folder is refused, as on any filesystem, and what
// that disk cannot hold answers as across two filesystems, with nothing journaled (specs.md §21.2). t2, the original
// of Films/a.mkv, is NTFS, which refuses ':'.
func TestWriterLinks(t *testing.T) {
	b := newBench(t, "ntfs")
	ctx := t.Context()
	if n := b.lookup("Films/a.mkv"); n == nil || n.Disk != "t2" {
		t.Fatalf("the original: %+v", n)
	}
	if _, err := b.w.Mkdir(ctx, "Series", 0o750, root); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Series"); n == nil || n.Disk != "t1" {
		t.Fatalf("a folder made on t1 alone: %+v", n)
	}
	n, err := b.w.Link(ctx, "Films/a.mkv", "Series/a.mkv", root)
	if err != nil {
		t.Fatal(err)
	}
	if n.Disk != "t2" || n.Nlink != 2 || b.inode("t2", "Series/a.mkv") != b.inode("t2", "Films/a.mkv") {
		t.Errorf("the link: %+v, inodes %d and %d", n, b.inode("t2", "Series/a.mkv"), b.inode("t2", "Films/a.mkv"))
	}
	if b.on("t1", "Series/a.mkv") != "absent" {
		t.Errorf("a link reached another disk: %q", b.on("t1", "Series/a.mkv"))
	}
	if info, err := os.Stat(filepath.Join(b.mount["t2"], "media", "Series")); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("the folder made on t2 on the way: %v, %v", info, err)
	}
	if got := b.lookup("Series/a.mkv"); got == nil || got.Disk != "t2" || got.Size != int64(len("from t2")) {
		t.Errorf("the new name presents %+v", got)
	}
	if got := b.lookup("Films/a.mkv"); got == nil || got.Disk != "t2" {
		t.Errorf("the name it was made from presents %+v", got)
	}
	open, err := catalog.OpenViewOps(ctx, b.w.db)
	if err != nil {
		t.Fatal(err)
	}
	last := open[len(open)-1]
	if last.Kind != catalog.ViewLink || last.Path != "Films/a.mkv" || last.Path2 != "Series/a.mkv" || last.State != catalog.OpDone ||
		last.FileKey != fmt.Sprintf("%d", b.inode("t2", "Films/a.mkv")) {
		t.Errorf("the journal: %+v", last)
	}
	for _, tc := range []struct {
		name, from, to string
		want           error
	}{
		{"onto a name presented", "Films/a.mkv", "Series/a.mkv", ErrExists},
		{"a folder", "Films", "Series/Films", ErrIsFolder},
		{"in a folder that presents nothing", "Films/a.mkv", "Nowhere/a.mkv", ErrNotFound},
		{"of nothing", "Films/none.mkv", "Series/none.mkv", ErrNotFound},
		{"a name its disk refuses", "Films/a.mkv", "Films/a:b.mkv", ErrCrossDisk},
	} {
		if _, err := b.w.Link(ctx, tc.from, tc.to, root); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if after, err := catalog.OpenViewOps(ctx, b.w.db); err != nil || len(after) != len(open) {
		t.Errorf("a link refused was journaled: %+v, %v", after, err)
	}
	// Something at the new name on the disk that the view does not present, as a stale copy, takes it there too.
	mustWrite(t, filepath.Join(b.mount["t2"], "media", "Films", "hidden.mkv"), "stale")
	if _, err := b.w.Link(ctx, "Films/a.mkv", "Films/hidden.mkv", root); !errors.Is(err, ErrCrossDisk) {
		t.Errorf("onto what the disk holds and the view does not present: %v", err)
	}
	if b.on("t2", "Films/hidden.mkv") != "stale" {
		t.Errorf("what the disk held there: %q", b.on("t2", "Films/hidden.mkv"))
	}
	// The disk of the original is mounted with the kernel's ntfs driver, whose link moves the file's modification time:
	// a program copies instead, and the file keeps its time.
	b.machine.fsType[b.mount["t2"]] = "ntfs"
	if _, err := b.w.Link(ctx, "Films/a.mkv", "Series/b.mkv", root); !errors.Is(err, ErrCrossDisk) {
		t.Errorf("a disk of the type ntfs: %v", err)
	}
	if b.on("t2", "Series/b.mkv") != "absent" {
		t.Error("a link made on a disk of the type ntfs")
	}
	b.machine.fsType[b.mount["t2"]] = "ntfs3"
	// The disk of the original may not be written now: a program copies instead.
	b.w.handles.Drop("t2")
	if _, err := b.w.Link(ctx, "Films/a.mkv", "Series/b.mkv", root); !errors.Is(err, ErrCrossDisk) {
		t.Errorf("its disk not writable: %v", err)
	}
}

// xattrsOn gives the user. extended attributes of a path on a disk, and skips a test on a filesystem that keeps none.
func (b *bench) xattrsOn(disk, path string) map[string]string {
	b.t.Helper()
	full := filepath.Join(b.mount[disk], "media", path)
	size, err := syscall.Listxattr(full, nil)
	if errors.Is(err, syscall.ENOTSUP) {
		b.t.Skip("this filesystem keeps no extended attribute")
	}
	if err != nil {
		b.t.Fatal(err)
	}
	list := make([]byte, size)
	if size, err = syscall.Listxattr(full, list); err != nil {
		b.t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range strings.Split(string(list[:size]), "\x00") {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		value := make([]byte, 1024)
		n, err := syscall.Getxattr(full, name, value)
		if err != nil {
			b.t.Fatal(err)
		}
		out[name] = string(value[:n])
	}
	return out
}

// Extended attributes through the view (specs.md §21.2): of the user. namespace alone, set and removed on the copy a
// file is presented from, its original, alone, and on every disk that holds a folder, each journaled; read from what
// the view presents; what the disk answers of one passes as it is; a symbolic link and the root take none.
func TestWriterXattrs(t *testing.T) {
	b := newBench(t, "ext4")
	ctx := t.Context()
	_ = b.xattrsOn("t2", "Films/a.mkv")
	n := b.lookup("Films/a.mkv")
	if n.Disk != "t2" {
		t.Fatalf("the original: %+v", n)
	}
	if err := b.w.SetXattr(ctx, "Films/a.mkv", n, "user.DOSATTRIB", []byte("0x20"), 0, root); err != nil {
		t.Fatal(err)
	}
	if got := b.xattrsOn("t2", "Films/a.mkv"); got["user.DOSATTRIB"] != "0x20" || len(b.xattrsOn("t1", "Films/a.mkv")) != 0 {
		t.Errorf("t2 %v, t1 %v: set on the original alone", got, b.xattrsOn("t1", "Films/a.mkv"))
	}
	attrs, err := b.tree.Xattrs("Films/a.mkv", b.lookup("Films/a.mkv"))
	if err != nil || string(attrs["user.DOSATTRIB"]) != "0x20" || len(attrs) != 1 {
		t.Errorf("read back: %v, %v", attrs, err)
	}
	open, err := catalog.OpenViewOps(ctx, b.w.db)
	if err != nil {
		t.Fatal(err)
	}
	last := open[len(open)-1]
	if last.Kind != catalog.ViewXattr || last.Disk != "t2" || last.Path != "Films/a.mkv" || last.State != catalog.OpDone ||
		last.FileKey != fmt.Sprintf("%d", b.inode("t2", "Films/a.mkv")) {
		t.Errorf("the journal: %+v", last)
	}
	if err := b.w.SetXattr(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), "user.DOSATTRIB", []byte("0x21"), fsx.XattrCreate, root); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("created twice: %v, want EEXIST as the disk answers", err)
	}
	if err := b.w.RemoveXattr(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), "user.DOSATTRIB", root); err != nil {
		t.Fatal(err)
	}
	if got := b.xattrsOn("t2", "Films/a.mkv"); len(got) != 0 {
		t.Errorf("removed: %v", got)
	}
	if err := b.w.RemoveXattr(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), "user.DOSATTRIB", root); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("removed twice: %v, want ENODATA", err)
	}

	// A folder the catalog holds on both disks takes it on each.
	if err := b.w.SetXattr(ctx, "Films", b.lookup("Films"), "user.DOSATTRIB", []byte("0x10"), 0, root); err != nil {
		t.Fatal(err)
	}
	for _, disk := range []string{"t1", "t2"} {
		if got := b.xattrsOn(disk, "Films"); got["user.DOSATTRIB"] != "0x10" {
			t.Errorf("the folder on %s: %v", disk, got)
		}
	}
	if _, err := b.w.Symlink(ctx, "Films/l", "a.mkv", root); err != nil {
		t.Fatal(err)
	}
	if err := b.w.SetXattr(ctx, "Films/l", b.lookup("Films/l"), "user.x", []byte("1"), 0, root); !errors.Is(err, ErrXattrOnLink) {
		t.Errorf("a symbolic link: %v", err)
	}
	if err := b.w.SetXattr(ctx, "", b.tree.Root(), "user.x", []byte("1"), 0, root); !errors.Is(err, ErrXattrOfRoot) {
		t.Errorf("the root: %v", err)
	}
	if attrs, err := b.tree.Xattrs("", b.tree.Root()); err != nil || len(attrs) != 0 {
		t.Errorf("the root presents %v, %v", attrs, err)
	}
}

// A hard link left planned is done when its new name leads to the file it linked, and undone otherwise (specs.md §21.2).
func TestWriterRepriseOfALink(t *testing.T) {
	b := newBench(t, "ext4")
	ctx := t.Context()
	key := fmt.Sprintf("%d", b.inode("t2", "Films/a.mkv"))
	if err := os.Link(filepath.Join(b.mount["t2"], "media", "Films", "a.mkv"), filepath.Join(b.mount["t2"], "media", "Films", "made.mkv")); err != nil {
		t.Fatal(err)
	}
	made := &catalog.ViewOp{Kind: catalog.ViewLink, Disk: "t2", Path: "Films/a.mkv", Path2: "Films/made.mkv", FileKey: key}
	never := &catalog.ViewOp{Kind: catalog.ViewLink, Disk: "t2", Path: "Films/a.mkv", Path2: "Films/never.mkv", FileKey: key}
	other := &catalog.ViewOp{Kind: catalog.ViewLink, Disk: "t2", Path: "Films/a.mkv", Path2: "Films/other.mkv", FileKey: key}
	mustWrite(t, filepath.Join(b.mount["t2"], "media", "Films", "other.mkv"), "another file")
	for _, v := range []*catalog.ViewOp{made, never, other} {
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
	if b.on("t2", "Films/other.mkv") != "another file" {
		t.Errorf("what took the new name: %q", b.on("t2", "Films/other.mkv"))
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

// What is new through the view goes where its group is with colocate, then as placement.new_files says (specs.md
// §12.4): with ordered, t1, the first disk below fill_to, though t2 has more room; a new episode of a season whose
// files are on t2 goes to t2.
func TestWriterPlacesAsTheStrategiesSay(t *testing.T) {
	b := newBenchWith(t, "ext4", "new_files = \"ordered\"\ncolocate = \"season\"\n", 200, 300)
	ctx := t.Context()
	b.create("Films/new.mkv", "new")
	if b.on("t1", "Films/new.mkv") != "new" {
		t.Errorf("ordered: a new film is not on t1: t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	mustWrite(t, filepath.Join(b.mount["t2"], "media", "Series", "S01", "e01.mkv"), "e01")
	for _, q := range []string{
		"INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (2, CAST('Series/S01/e01.mkv' AS BLOB), 3, 9, 1, 'deferred', 'ok', 0, 0)",
		"INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, state, mode, uid, gid, nlink) VALUES (2, 't2', 3, 9, 'user', 0, 'ok', 420, 0, 0, 1)",
		"INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by) VALUES (CAST('Series' AS BLOB), 't2', 'dir', 493, 5, 'user'), (CAST('Series/S01' AS BLOB), 't2', 'dir', 493, 5, 'user')",
	} {
		if _, err := b.w.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	b.create("Series/S01/e02.mkv", "e02")
	if b.on("t2", "Series/S01/e02.mkv") != "e02" {
		t.Errorf("colocate: a new episode is not with its season on t2: t1 %q, t2 %q", b.on("t1", "Series/S01/e02.mkv"), b.on("t2", "Series/S01/e02.mkv"))
	}
}

// balance.equalize places Atoll's copies, not what is new: the view lands a new file as placement.new_files says, ordered
// here, where the most free share would be t2 (specs.md §12.4).
func TestTheViewLandsAsNewFilesSaysWhateverEqualize(t *testing.T) {
	b := newBenchWith(t, "ext4", "new_files = \"ordered\"\n[balance]\nequalize = \"percent\"\n", 200, 300)
	b.create("Films/new.mkv", "new")
	if b.on("t1", "Films/new.mkv") != "new" {
		t.Errorf("a new film is not on t1: t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
}

// cacheBench is a bench whose t2 is a cache disk, with Atoll's own view, and more after the disks.
func cacheBench(t *testing.T, more string, free2 int64) *bench {
	t.Helper()
	return newBenchEdited(t, "ext4", "", 500, free2, func(b *bench, text string) string {
		text = strings.Replace(text, "[view]\n", "[view]\nengine = \"atoll\"\n", 1)
		disk := fmt.Sprintf("mount = %q\nfs = \"ext4\"\n", b.mount["t2"])
		if !strings.Contains(text, disk) {
			t.Fatal("no disk t2")
		}
		return strings.Replace(text, disk, disk+"role = \"cache\"\n", 1) + more
	})
}

// A new file lands on the SSD cache, with Atoll's own view and cache.enabled, though a data disk has more room; a folder
// does not, as it has no residency; nor does a file a rule keeps off the cache, any file with the cache not enabled,
// or one the cache has no room for past min_free, which go to a data disk (specs.md §19).
func TestANewFileLandsOnTheCache(t *testing.T) {
	for _, tc := range []struct {
		name, more, path string
		free2            int64
		want             string
	}{
		{"on the cache", "[cache]\nenabled = true\n", "Films/new.mkv", 100, "t2"},
		{"the cache not enabled", "[cache]\nenabled = false\n", "Films/new.mkv", 100, "t1"},
		{"a rule keeps it off", "[cache]\nenabled = true\n[[rule]]\nlabel = \"iso\"\next = [\"iso\"]\ncache = false\n", "Films/new.iso", 100, "t1"},
		{"a rule keeps another off", "[cache]\nenabled = true\n[[rule]]\nlabel = \"iso\"\next = [\"iso\"]\ncache = false\n", "Films/new.mkv", 100, "t2"},
		{"the cache full", "[cache]\nenabled = true\n", "Films/new.mkv", 50, "t1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := cacheBench(t, tc.more, tc.free2)
			b.create(tc.path, "new")
			other := map[string]string{"t1": "t2", "t2": "t1"}[tc.want]
			if b.on(tc.want, tc.path) != "new" || b.on(other, tc.path) != "absent" {
				t.Errorf("t1 %q, t2 %q, want it on %s", b.on("t1", tc.path), b.on("t2", tc.path), tc.want)
			}
			if n := b.lookup(tc.path); n == nil || n.Disk != tc.want {
				t.Errorf("it presents %+v", n)
			}
		})
	}
	b := cacheBench(t, "[cache]\nenabled = true\n", 100)
	if n, err := b.w.Mkdir(t.Context(), "Shows", 0o755, root); err != nil || n.Disk != "t1" {
		t.Errorf("a folder: %+v, %v", n, err)
	}
}
