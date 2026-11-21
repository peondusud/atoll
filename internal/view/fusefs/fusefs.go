package fusefs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// node is one path of the view; the root's path is "". w writes on a user's behalf, nil in a view read only.
type node struct {
	fs.Inode
	tree *view.Tree
	w    *writer.Writer
}

var (
	_ fs.NodeLookuper   = (*node)(nil)
	_ fs.NodeGetattrer  = (*node)(nil)
	_ fs.NodeReaddirer  = (*node)(nil)
	_ fs.NodeOpener     = (*node)(nil)
	_ fs.NodeReadlinker = (*node)(nil)
	_ fs.NodeStatfser   = (*node)(nil)
)

func (n *node) logical() string {
	if p := n.Path(n.Root()); p != "." {
		return p
	}
	return ""
}

// join gives the logical path of a name in a folder, byte for byte: never cleaned, as a name is never altered.
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func kindMode(k view.NodeKind) uint32 {
	switch k {
	case view.NodeDir:
		return syscall.S_IFDIR
	case view.NodeSymlink:
		return syscall.S_IFLNK
	case view.NodeFile:
		return syscall.S_IFREG
	}
	return syscall.S_IFREG
}

func fill(a *fuse.Attr, v *view.Node) {
	a.Mode = kindMode(v.Kind) | v.Mode&0o7777
	a.Nlink = uint32(max(v.Nlink, 1))
	a.Owner = fuse.Owner{Uid: v.UID, Gid: v.GID}
	a.Size = uint64(max(v.Size, 0))
	a.Blocks = (a.Size + 511) / 512
	a.Blksize = 4096
	t := time.Unix(0, v.MtimeNS)
	a.SetTimes(&t, &t, &t)
}

// whole gives the work of a request a context the kernel's interrupts do not cancel: a request runs to its end, as
// mergerfs and libfuse's high-level API do by default. go-fuse cancels the context of a request the kernel interrupts,
// as it does when a signal reaches the process waiting on it, and the Go runtime preempts its threads with SIGURG.
// Canceled, the catalog's query failed, and the open answered EIO failed for good; answered EINTR, the call fails
// with EINTR in the process, as the kernel does not start it again, whatever SA_RESTART says. The catalog's queries
// are short.
func whole(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// failed says what a request that failed answers, and logs why.
func failed(what, p string, err error) syscall.Errno {
	slog.Warn(what, "path", p, "error", err)
	return syscall.EIO
}

func (n *node) lookup(ctx context.Context, p string) (*view.Node, syscall.Errno) {
	v, err := n.tree.Lookup(whole(ctx), p)
	switch {
	case err != nil:
		return nil, failed("the view cannot read the catalog", p, err)
	case v == nil:
		return nil, syscall.ENOENT
	}
	return v, 0
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	v, errno := n.lookup(ctx, join(n.logical(), name))
	if errno != 0 {
		return nil, errno
	}
	fill(&out.Attr, v)
	p := join(n.logical(), name)
	return n.child(ctx, p, v), 0
}

// child is the node of a path below this one, which presents v.
func (n *node) child(ctx context.Context, p string, v *view.Node) *fs.Inode {
	return n.NewInode(ctx, &node{tree: n.tree, w: n.w}, fs.StableAttr{Mode: kindMode(v.Kind), Ino: inoOf(p)})
}

// inoOf numbers a path the same at each lookup, so that go-fuse gives the kernel the node it already knows once its
// 1 s are over, rather than a new one at each lookup: FNV-1a of the logical path, past the numbers FUSE reserves.
func inoOf(p string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(p))
	ino := h.Sum64()
	if ino <= fuse.FUSE_ROOT_ID || ino == ^uint64(0) {
		ino += 2
	}
	return ino
}

func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	v, errno := n.lookup(ctx, n.logical())
	if errno != 0 {
		return errno
	}
	fill(&out.Attr, v)
	return 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, err := n.tree.List(whole(ctx), n.logical())
	if err != nil {
		return nil, failed("the view cannot list a folder", n.logical(), err)
	}
	entries := make([]fuse.DirEntry, 0, len(children))
	for i := range children {
		entries = append(entries, fuse.DirEntry{Name: children[i].Name, Mode: kindMode(children[i].Node.Kind)})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	v, errno := n.lookup(ctx, n.logical())
	switch {
	case errno != 0:
		return nil, errno
	case v.Kind != view.NodeSymlink:
		return nil, syscall.EINVAL
	}
	return []byte(v.Target), 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	s, err := n.tree.Statfs(whole(ctx))
	if err != nil {
		return failed("the view cannot read the catalog", "", err)
	}
	*out = fuse.StatfsOut{Blocks: s.Blocks, Bfree: s.Bfree, Bavail: s.Bavail, Bsize: uint32(s.Bsize), Frsize: uint32(s.Bsize), NameLen: uint32(s.Namemax)}
	return 0
}

// file is a file open for reading, on the disk whose copy it presents.
type file struct {
	f fsx.File
}

var (
	_ fs.FileReader   = (*file)(nil)
	_ fs.FileReleaser = (*file)(nil)
)

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&syscall.O_TRUNC != 0 {
		return n.openToWrite(ctx, flags)
	}
	p := n.logical()
	v, errno := n.lookup(ctx, p)
	switch {
	case errno != 0:
		return nil, 0, errno
	case v.Kind == view.NodeDir:
		return nil, 0, syscall.EISDIR
	}
	f, err := n.tree.Open(ctx, p, v)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return nil, 0, syscall.ENOENT
	case err != nil:
		return nil, 0, failed("the view cannot open a copy", p, err)
	}
	return &file{f: f}, 0, 0
}

func (f *file) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.f.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *file) Release(context.Context) syscall.Errno {
	_ = f.f.Close()
	return 0
}

// Mounted is a view being served.
type Mounted struct {
	server *fuse.Server
	point  string
	done   chan struct{}
}

// Type is the type of the view's mounts in the mount table.
const Type = "fuse.atoll"

// device is the kernel's FUSE device, which mount(2) and fusermount both open as the account that mounts.
const device = "/dev/fuse"

// Mount serves the view at mount until Unmount, read only when w is nil, and writing through w otherwise: mounted by
// mount(2) when the process holds CAP_SYS_ADMIN, as its units give it, and by fusermount otherwise.
func Mount(tree *view.Tree, mount string, w *writer.Writer) (*Mounted, error) {
	// Said here: go-fuse would say only that fusermount failed, once mount(2) had.
	if err := fsx.CanReadWrite(device); err != nil {
		return nil, fmt.Errorf("%w: FUSE needs it readable and writable by this account, as udev makes it (mode 0666)", err)
	}
	second := time.Second
	none := time.Duration(0)
	options := []string{"ro", "default_permissions"}
	if w != nil {
		options = options[1:]
	}
	server, err := fs.Mount(mount, &node{tree: tree, w: w}, &fs.Options{
		EntryTimeout: &second, AttrTimeout: &second, NegativeTimeout: &none,
		MountOptions: fuse.MountOptions{
			AllowOther: true, FsName: "atoll", Name: "atoll", DirectMount: true, Options: options,
		},
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // go-fuse says what failed
	}
	m := &Mounted{server: server, point: mount, done: make(chan struct{})}
	go func() {
		server.Wait()
		close(m.done)
	}()
	return m, nil
}

// Done is closed once the view is served no more: unmounted by Unmount, or by anything else.
func (m *Mounted) Done() <-chan struct{} {
	return m.done
}

// Unmount stops serving the view, and waits until it has. A view a process still reads from, which the kernel does
// not unmount, is detached instead, as umount -l does: what is open there fails from then on, and Unmount waits for
// nothing.
func (m *Mounted) Unmount() error {
	err := m.server.Unmount()
	if err == nil {
		<-m.done
		return nil
	}
	if detached := fsx.DetachMount(m.point); detached != nil {
		return errors.Join(err, detached)
	}
	return nil
}
