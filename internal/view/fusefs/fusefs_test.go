package fusefs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// machine is two disks as folders, each mounted healthy as ext4, with room.
type machine struct{}

func (machine) Mount(mount string) disks.MountSeen {
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{MountPoint: mount, Root: "/", FSType: "ext4"}}
}
func (machine) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (machine) Root(string, string) disks.RootSeen { return disks.RootSeen{} }
func (machine) Hiberfil(string, *disks.MountEntry) disks.HiberfilSeen {
	return disks.HiberfilSeen{File: disks.HiberfilAbsent}
}
func (machine) LogFile(string, *disks.MountEntry) disks.LogFileSeen {
	return disks.LogFileSeen{File: disks.LogFileFile, Journal: disks.JournalEmpty}
}
func (machine) ReadPastCache(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{}
}
func (machine) Checkpoint(string)                  {}
func (machine) Now() int64                         { return time.Now().UnixNano() }
func (machine) Space(string) (int64, int64, error) { return 1 << 30, 1 << 29, nil }
func (machine) OpenRoot(mount, root string) (fsx.Dir, error) {
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}
func (machine) OpenTop(mount string) (fsx.Dir, error) {
	return fsx.OpenDir(mount) //nolint:wrapcheck // a test double
}

// bench is a pool of two disks, t1 and t2, each holding Films/a.mkv, and the view's root node, served through go-fuse's
// bridge as the kernel would ask it, with no mount: read only, or writing through a writer.
type bench struct {
	t     *testing.T
	mount map[string]string
	db    interface{ Close() error }
	root  *node
	raw   fuse.RawFileSystem
}

func newBench(t *testing.T, writes bool) *bench {
	t.Helper()
	top := t.TempDir()
	b := &bench{t: t, mount: map[string]string{}}
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = \"/srv/pool\"\n", filepath.Join(top, "catalog.db"))
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(top, id)
		b.mount[id] = mount
		mustWrite(t, filepath.Join(mount, "media", "Films", "a.mkv"), "from "+id)
		text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d\"\nmount = %q\nfs = \"ext4\"\n", id, i, mount)
	}
	c, err := config.Parse([]byte(text))
	must(t, err)
	file := filepath.Join(top, "catalog.db")
	_, err = catalog.Create(t.Context(), file, "p")
	must(t, err)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var rows []catalog.Disk
	for i := range c.Disks {
		d := &c.Disks[i]
		rows = append(rows, catalog.Disk{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role, Position: i, State: "online"})
	}
	must(t, catalog.SyncDisks(t.Context(), db, rows))
	for _, q := range []string{
		"UPDATE disks SET state = 'online'",
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, CAST('Films/a.mkv' AS BLOB), 7, 9, 1, 'deferred', 'ok', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
			VALUES (1, 't1', 7, 9, 'atoll', 0, 200, 'ok', 420, 0, 0, 1), (1, 't2', 7, 9, 'user', 0, 100, 'ok', 420, 0, 0, 1)`,
		`INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by) VALUES (CAST('Films' AS BLOB), 't1', 'dir', 493, 5, 'user'), (CAST('Films' AS BLOB), 't2', 'dir', 493, 5, 'user')`,
	} {
		_, err := db.ExecContext(t.Context(), q)
		must(t, err)
	}
	m := machine{}
	roots := view.NewDiskRoots()
	t.Cleanup(roots.Close)
	handles := writer.NewHandles()
	t.Cleanup(handles.Close)
	for i := range c.Disks {
		d := &c.Disks[i]
		root, err := m.OpenRoot(d.Mount, d.Root)
		must(t, err)
		roots.Set(d.ID, root)
		h, err := ops.Open(m, d, 0)
		must(t, err)
		handles.Set(d.ID, h)
	}
	tree := view.NewTree(db, roots, view.Owner{UID: 0, GID: 0}, 1)
	var w *writer.Writer
	if writes {
		w, err = writer.New(db, c, m, tree, handles, slog.New(slog.DiscardHandler))
		must(t, err)
		t.Cleanup(w.Wait)
	}
	b.db = db
	b.root = &node{tree: tree, w: w}
	b.raw = fs.NewNodeFS(b.root, &fs.Options{})
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(text), 0o644))
}

// as is a request's context as go-fuse gives it, with the account and the thread that ask.
func as(t *testing.T) context.Context {
	return fuse.NewContext(t.Context(), &fuse.Caller{Owner: fuse.Owner{Uid: 0, Gid: 0}, Pid: uint32(os.Getpid())})
}

// header is the header of a request on a node, as the kernel sends it, by root.
func header(id uint64) fuse.InHeader {
	return fuse.InHeader{NodeId: id, Caller: fuse.Caller{Owner: fuse.Owner{Uid: 0, Gid: 0}, Pid: uint32(os.Getpid())}}
}

// lookup asks the bridge for a name below a node, as the kernel does, and gives the node it answers with.
func (b *bench) lookup(parent uint64, name string) (uint64, fuse.Status) {
	b.t.Helper()
	var out fuse.EntryOut
	h := header(parent)
	status := b.raw.Lookup(nil, &h, name, &out)
	return out.NodeId, status
}

// node gives the node of a path, each name looked up through the bridge, so that it is in the tree as the kernel's
// lookups put it there.
func (b *bench) node(path string) *node {
	b.t.Helper()
	inode := b.root.EmbeddedInode()
	id := uint64(fuse.FUSE_ROOT_ID)
	for name := range strings.SplitSeq(path, "/") {
		var status fuse.Status
		if id, status = b.lookup(id, name); !status.Ok() {
			b.t.Fatalf("lookup %s in %s: %v", name, path, status)
		}
		inode = inode.GetChild(name)
	}
	return inode.Operations().(*node)
}

// read reads a file through the bridge, open, read and release, as a program's read does.
func (b *bench) read(path string) (string, fuse.Status) {
	b.t.Helper()
	n := b.node(path)
	fh, _, errno := n.Open(as(b.t), syscall.O_RDONLY)
	if errno != 0 {
		return "", fuse.ToStatus(errno)
	}
	buf := make([]byte, 64)
	result, errno := fh.(fs.FileReader).Read(as(b.t), buf, 0)
	if errno != 0 {
		return "", fuse.ToStatus(errno)
	}
	data, status := result.Bytes(buf)
	_ = fh.(fs.FileReleaser).Release(as(b.t))
	return string(data), status
}

func (b *bench) on(disk, path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.mount[disk], "media", path))
	if err != nil {
		return "absent"
	}
	return string(data)
}

// The view as the kernel asks it, read only: a name is found or not, a folder lists its names, a file reads from its
// original, the user copy t2 holds (specs.md §7, §21.2); a folder opened as a file, a file read as a link and every write
// answer as a filesystem does.
func TestTheViewReadOnlyAnswersAsAFilesystem(t *testing.T) {
	b := newBench(t, false)
	if _, status := b.lookup(fuse.FUSE_ROOT_ID, "Series"); status != fuse.ENOENT {
		t.Errorf("a name that is not there: %v", status)
	}
	films := b.node("Films")
	var attr fuse.AttrOut
	if errno := films.Getattr(as(t), nil, &attr); errno != 0 || attr.Mode&syscall.S_IFMT != syscall.S_IFDIR || attr.Mode&0o7777 != 0o755 {
		t.Errorf("Films: %v, mode %o", errno, attr.Mode)
	}
	stream, errno := films.Readdir(as(t))
	if errno != 0 {
		t.Fatal(errno)
	}
	var listed []string
	for stream.HasNext() {
		e, errno := stream.Next()
		if errno != 0 {
			t.Fatal(errno)
		}
		listed = append(listed, e.Name)
	}
	if !slices.Equal(listed, []string{"a.mkv"}) {
		t.Errorf("Films lists %q", listed)
	}
	if got, status := b.read("Films/a.mkv"); !status.Ok() || got != "from t2" {
		t.Errorf("Films/a.mkv reads %q, %v", got, status)
	}
	a := b.node("Films/a.mkv")
	if errno := a.Getattr(as(t), nil, &attr); errno != 0 || attr.Size != 7 || attr.Nlink != 1 || attr.Blocks != 1 {
		t.Errorf("Films/a.mkv: %v, %+v", errno, attr.Attr)
	}
	if _, _, errno := films.Open(as(t), syscall.O_RDONLY); errno != syscall.EISDIR {
		t.Errorf("a folder opened: %v", errno)
	}
	if _, errno := a.Readlink(as(t)); errno != syscall.EINVAL {
		t.Errorf("a file read as a link: %v", errno)
	}
	var statfs fuse.StatfsOut
	if errno := b.root.Statfs(as(t), &statfs); errno != 0 {
		t.Errorf("statfs: %v", errno)
	}
	var entry fuse.EntryOut
	var setattr fuse.SetAttrIn
	for name, errno := range map[string]syscall.Errno{
		"open to write": func() syscall.Errno { _, _, e := a.Open(as(t), syscall.O_WRONLY); return e }(),
		"truncate":      func() syscall.Errno { _, _, e := a.Open(as(t), syscall.O_RDONLY|syscall.O_TRUNC); return e }(),
		"create":        func() syscall.Errno { _, _, _, e := films.Create(as(t), "b.mkv", 0, 0o644, &entry); return e }(),
		"mkdir":         func() syscall.Errno { _, e := films.Mkdir(as(t), "Old", 0o755, &entry); return e }(),
		"symlink":       func() syscall.Errno { _, e := films.Symlink(as(t), "a.mkv", "l", &entry); return e }(),
		"unlink":        films.Unlink(as(t), "a.mkv"),
		"rmdir":         b.root.Rmdir(as(t), "Films"),
		"rename":        films.Rename(as(t), "a.mkv", films, "b.mkv", 0),
		"link":          func() syscall.Errno { _, e := films.Link(as(t), a, "b.mkv", &entry); return e }(),
		"setxattr":      a.Setxattr(as(t), "user.x", []byte("y"), 0),
		"removexattr":   a.Removexattr(as(t), "user.x"),
		"setattr":       a.Setattr(as(t), nil, &setattr, &attr),
	} {
		if errno != syscall.EROFS {
			t.Errorf("%s: %v, want EROFS", name, errno)
		}
	}
	if b.on("t1", "Films/a.mkv") != "from t1" || b.on("t2", "Films/a.mkv") != "from t2" {
		t.Error("a view read only changed a disk")
	}
}

// What the view answers once the catalog cannot be read: EIO, never a name absent.
func TestTheViewWhoseCatalogCannotBeReadAnswersEIO(t *testing.T) {
	b := newBench(t, false)
	films := b.node("Films")
	must(t, b.db.Close())
	var entry fuse.EntryOut
	if _, errno := films.Lookup(as(t), "a.mkv", &entry); errno != syscall.EIO {
		t.Errorf("lookup: %v", errno)
	}
	var attr fuse.AttrOut
	if errno := films.Getattr(as(t), nil, &attr); errno != syscall.EIO {
		t.Errorf("getattr: %v", errno)
	}
	if _, errno := films.Readdir(as(t)); errno != syscall.EIO {
		t.Errorf("readdir: %v", errno)
	}
	var statfs fuse.StatfsOut
	if errno := b.root.Statfs(as(t), &statfs); errno != syscall.EIO {
		t.Errorf("statfs: %v", errno)
	}
	if _, errno := films.Readlink(as(t)); errno != syscall.EIO {
		t.Errorf("readlink: %v", errno)
	}
	if _, _, errno := films.Open(as(t), syscall.O_RDONLY); errno != syscall.EIO {
		t.Errorf("open: %v", errno)
	}
	if _, errno := films.Getxattr(as(t), "user.x", nil); errno != syscall.EIO {
		t.Errorf("getxattr: %v", errno)
	}
}

// The view that writes, as the kernel asks it (specs.md §21.2): a file made, written, read back while open and once
// closed, its size followed while open; a folder, a link, a hard link, a rename, attributes and extended attributes; and
// what a filesystem refuses, refused with its own error.
func TestTheViewThatWritesAnswersAsAFilesystem(t *testing.T) {
	b := newBench(t, true)
	films := b.node("Films")
	var entry fuse.EntryOut
	_, fh, _, errno := films.Create(as(t), "b.mkv", 0, 0o644, &entry)
	if errno != 0 {
		t.Fatalf("create: %v", errno)
	}
	f := fh.(*wfile)
	if n, errno := f.Write(as(t), []byte("a new film"), 0); errno != 0 || n != 10 {
		t.Fatalf("write: %d, %v", n, errno)
	}
	if errno := f.Fsync(as(t), 0); errno != 0 {
		t.Errorf("fsync: %v", errno)
	}
	if errno := f.Flush(as(t)); errno != 0 {
		t.Errorf("flush: %v", errno)
	}
	var attr fuse.AttrOut
	if errno := f.Getattr(as(t), &attr); errno != 0 || attr.Size != 10 {
		t.Errorf("an open file's size: %d, %v", attr.Size, errno)
	}
	buf := make([]byte, 64)
	result, errno := f.Read(as(t), buf, 2)
	if errno != 0 {
		t.Fatal(errno)
	}
	if data, _ := result.Bytes(buf); string(data) != "new film" {
		t.Errorf("an open file reads %q", data)
	}
	if result, errno := f.Read(as(t), buf, 100); errno != 0 || result.Size() != 0 {
		t.Errorf("a read past the end: %v", errno)
	}
	if errno := f.Release(as(t)); errno != 0 {
		t.Errorf("release: %v", errno)
	}
	if got, status := b.read("Films/b.mkv"); !status.Ok() || got != "a new film" {
		t.Errorf("Films/b.mkv reads %q, %v", got, status)
	}
	if _, _, _, errno := films.Create(as(t), "b.mkv", 0, 0o644, &entry); errno != syscall.EEXIST {
		t.Errorf("a file made where one is: %v", errno)
	}

	// Opened again to write, truncated as O_TRUNC asks.
	bn := b.node("Films/b.mkv")
	fh, _, errno = bn.Open(as(t), syscall.O_WRONLY|syscall.O_TRUNC)
	if errno != 0 {
		t.Fatalf("open to write: %v", errno)
	}
	f = fh.(*wfile)
	if _, errno := f.Write(as(t), []byte("shorter"), 0); errno != 0 {
		t.Fatal(errno)
	}
	_ = f.Release(as(t))
	if got, _ := b.read("Films/b.mkv"); got != "shorter" {
		t.Errorf("Films/b.mkv once truncated reads %q", got)
	}

	// Attributes: a mode, a size, and times.
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_MODE | fuse.FATTR_SIZE | fuse.FATTR_MTIME | fuse.FATTR_ATIME
	in.Mode = 0o600
	in.Size = 4
	in.Mtime, in.Atime = 1500000000, 1500000000
	if errno := bn.Setattr(as(t), nil, in, &attr); errno != 0 {
		t.Fatalf("setattr: %v", errno)
	}
	if attr.Mode&0o7777 != 0o600 || attr.Size != 4 {
		t.Errorf("after setattr: mode %o, size %d", attr.Mode, attr.Size)
	}
	if errno := b.root.Setattr(as(t), nil, in, &attr); errno != syscall.EPERM {
		t.Errorf("setattr of the root: %v", errno)
	}

	// A folder, a link, and what reads it.
	if _, errno := films.Mkdir(as(t), "Old", 0o755, &entry); errno != 0 {
		t.Fatalf("mkdir: %v", errno)
	}
	if _, errno := films.Mkdir(as(t), "Old", 0o755, &entry); errno != syscall.EEXIST {
		t.Errorf("a folder made where one is: %v", errno)
	}
	if _, errno := films.Symlink(as(t), "b.mkv", "l", &entry); errno != 0 {
		t.Fatalf("symlink: %v", errno)
	}
	if target, errno := b.node("Films/l").Readlink(as(t)); errno != 0 || string(target) != "b.mkv" {
		t.Errorf("readlink: %q, %v", target, errno)
	}

	// A hard link, then a rename, then what renameat2 asks that the view does not do.
	if _, errno := films.Link(as(t), bn, "c.mkv", &entry); errno != 0 {
		t.Fatalf("link: %v", errno)
	}
	if _, errno := films.Link(as(t), b.root, "d", &entry); errno != syscall.EPERM {
		t.Errorf("a link to the root: %v", errno)
	}
	if _, errno := films.Link(as(t), b.node("Films/Old"), "e", &entry); errno != syscall.EPERM {
		t.Errorf("a link to a folder: %v", errno)
	}
	if _, errno := films.Link(as(t), b.node("Films/l"), "m", &entry); errno != syscall.EXDEV {
		t.Errorf("a link to a symbolic link: %v", errno)
	}
	old := b.node("Films/Old")
	if errno := films.Rename(as(t), "c.mkv", old, "c.mkv", renameNoReplace); errno != 0 {
		t.Fatalf("rename: %v", errno)
	}
	if got, _ := b.read("Films/Old/c.mkv"); got != "shor" {
		t.Errorf("Films/Old/c.mkv reads %q", got)
	}
	if errno := films.Rename(as(t), "b.mkv", films, "a.mkv", renameExchange); errno != syscall.EINVAL {
		t.Errorf("an exchange: %v", errno)
	}
	if errno := films.Rename(as(t), "nothing", films, "z", 0); errno != syscall.ENOENT {
		t.Errorf("a rename of nothing: %v", errno)
	}

	// Extended attributes of the user. namespace alone, read back on the disk the view presents.
	if errno := bn.Setxattr(as(t), "trusted.x", []byte("y"), 0); errno != syscall.ENOTSUP {
		t.Errorf("a trusted. attribute: %v", errno)
	}
	if errno := bn.Removexattr(as(t), "trusted.x"); errno != syscall.ENOTSUP {
		t.Errorf("a trusted. attribute removed: %v", errno)
	}
	if _, errno := bn.Getxattr(as(t), "trusted.x", nil); errno != syscall.ENODATA {
		t.Errorf("a trusted. attribute read: %v", errno)
	}
	errno = bn.Setxattr(as(t), "user.comment", []byte("a note"), 0)
	if errno == syscall.ENOTSUP {
		t.Log("the filesystem of the test keeps no user. attribute")
	} else if errno != 0 {
		t.Errorf("setxattr: %v", errno)
	} else {
		if n, errno := bn.Getxattr(as(t), "user.comment", nil); errno != syscall.ERANGE || n != 6 {
			t.Errorf("getxattr in no room: %d, %v", n, errno)
		}
		value := make([]byte, 16)
		if n, errno := bn.Getxattr(as(t), "user.comment", value); errno != 0 || string(value[:n]) != "a note" {
			t.Errorf("getxattr: %q, %v", value[:n], errno)
		}
		if _, errno := bn.Getxattr(as(t), "user.other", value); errno != syscall.ENODATA {
			t.Errorf("an attribute not there: %v", errno)
		}
		if n, errno := bn.Listxattr(as(t), nil); errno != syscall.ERANGE || n != uint32(len("user.comment\x00")) {
			t.Errorf("listxattr in no room: %d, %v", n, errno)
		}
		list := make([]byte, 64)
		if n, errno := bn.Listxattr(as(t), list); errno != 0 || string(list[:n]) != "user.comment\x00" {
			t.Errorf("listxattr: %q, %v", list[:n], errno)
		}
		if errno := bn.Removexattr(as(t), "user.comment"); errno != 0 {
			t.Errorf("removexattr: %v", errno)
		}
	}
	if errno := b.root.Setxattr(as(t), "user.comment", []byte("x"), 0); errno != syscall.ENOTSUP {
		t.Errorf("an attribute of the root: %v", errno)
	}

	// What goes: a file, then a folder once empty.
	if errno := films.Unlink(as(t), "l"); errno != 0 {
		t.Errorf("unlink: %v", errno)
	}
	if errno := films.Unlink(as(t), "l"); errno != syscall.ENOENT {
		t.Errorf("an unlink of nothing: %v", errno)
	}
	if errno := films.Rmdir(as(t), "Old"); errno != syscall.ENOTEMPTY {
		t.Errorf("rmdir of a folder that holds a file: %v", errno)
	}
	if errno := old.Unlink(as(t), "c.mkv"); errno != 0 {
		t.Errorf("unlink: %v", errno)
	}
	if errno := films.Rmdir(as(t), "Old"); errno != 0 {
		t.Errorf("rmdir: %v", errno)
	}
	if errno := films.Unlink(as(t), "Old"); errno != syscall.ENOENT {
		t.Errorf("a folder gone, unlinked: %v", errno)
	}
}

// Who asks is the caller of the request, as go-fuse gives it; with no request, nobody.
func TestTheCallerIsTheRequests(t *testing.T) {
	ctx := fuse.NewContext(t.Context(), &fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}, Pid: 42})
	if got := caller(ctx); got != (writer.Caller{Owner: writer.Owner{UID: 1000, GID: 100}, PID: 42, InRequest: true}) {
		t.Errorf("%+v", got)
	}
	if got := caller(t.Context()); got != (writer.Caller{}) {
		t.Errorf("no request: %+v", got)
	}
	// A request the kernel interrupts still runs to its end.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := whole(canceled).Err(); err != nil {
		t.Errorf("whole: %v", err)
	}
}

// What each error a write meets answers (specs.md §21.2): what says the disk or the catalog cannot be relied on is EIO,
// what a disk answered passes as it is.
func TestTheErrnoOfAWrite(t *testing.T) {
	for err, want := range map[error]syscall.Errno{
		nil:                    0,
		writer.ErrExists:       syscall.EEXIST,
		ops.ErrPathTaken:       syscall.EEXIST,
		writer.ErrNotFound:     syscall.ENOENT,
		ops.ErrGone:            syscall.ENOENT,
		syscall.ENOENT:         syscall.ENOENT,
		writer.ErrNotFolder:    syscall.ENOTDIR,
		writer.ErrIsFolder:     syscall.EISDIR,
		writer.ErrNotEmpty:     syscall.ENOTEMPTY,
		ops.ErrNotEmpty:        syscall.ENOTEMPTY,
		writer.ErrRefused:      syscall.EINVAL,
		ops.ErrNameRefused:     syscall.EINVAL,
		writer.ErrInvalid:      syscall.EINVAL,
		writer.ErrCrossDisk:    syscall.EXDEV,
		ops.ErrNoLink:          syscall.EXDEV,
		writer.ErrNoSpace:      syscall.ENOSPC,
		writer.ErrNoHandle:     syscall.EROFS,
		writer.ErrXattrOnLink:  syscall.EPERM,
		writer.ErrXattrOfRoot:  syscall.ENOTSUP,
		ops.ErrDiskLost:        syscall.EIO,
		view.ErrUnreadable:     syscall.EIO,
		syscall.EACCES:         syscall.EACCES,
		syscall.ENOSPC:         syscall.ENOSPC,
		errors.New("anything"): syscall.EIO,
	} {
		if got := errnoOf("a test", "p", fmt.Errorf("wrapped: %w", err)); err != nil && got != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
	if got := errnoOf("a test", "p", nil); got != 0 {
		t.Errorf("nil: %v", got)
	}
}

// A node's attributes: its kind, the permission bits of its mode, at least one link, its size in blocks, and its
// modification time for each of the three times; a path numbered the same at each lookup, never as FUSE's own.
func TestAttributesAndNumbers(t *testing.T) {
	var a fuse.Attr
	fill(&a, &view.Node{Kind: view.NodeSymlink, Mode: 0o120777, Size: 513, UID: 7, GID: 8, MtimeNS: 1_500_000_000_000_000_001})
	if a.Mode != syscall.S_IFLNK|0o777 || a.Nlink != 1 || a.Blocks != 2 || a.Owner != (fuse.Owner{Uid: 7, Gid: 8}) ||
		a.Mtime != 1_500_000_000 || a.Mtimensec != 1 || a.Ctime != a.Mtime || a.Atime != a.Mtime {
		t.Errorf("%+v", a)
	}
	fill(&a, &view.Node{Kind: view.NodeFile, Size: -1, Nlink: 3})
	if a.Size != 0 || a.Nlink != 3 || a.Mode != syscall.S_IFREG {
		t.Errorf("%+v", a)
	}
	if kindMode(view.NodeKind("other")) != syscall.S_IFREG {
		t.Error("an unknown kind is not a file")
	}
	if first, again := inoOf("Films/a.mkv"), inoOf("Films/a.mkv"); first != again || first == inoOf("Films/b.mkv") {
		t.Error("the numbers of paths")
	}
	for _, p := range []string{"", "a", "Films", "Films/a.mkv"} {
		if ino := inoOf(p); ino <= fuse.FUSE_ROOT_ID || ino == ^uint64(0) {
			t.Errorf("%q: %d", p, ino)
		}
	}
	if join("", "a") != "a" || join("Films", "a.mkv") != "Films/a.mkv" || join("a", "../b") != "a/../b" {
		t.Error("join")
	}
	if got := timeOf(time.Unix(5, 6)); *got != (fsx.Time{Sec: 5, Nsec: 6}) {
		t.Errorf("timeOf: %v", got)
	}
}

// Mount says why FUSE cannot be used where its device cannot be opened: what a unit test's machine has.
func TestAMountWithoutTheDeviceSaysWhy(t *testing.T) {
	if fsx.CanReadWrite(device) == nil {
		t.Skip("this machine can open /dev/fuse: test/loopdev mounts the view")
	}
	b := newBench(t, false)
	if _, err := Mount(b.root.tree, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "FUSE needs it") {
		t.Errorf("%v", err)
	}
}
