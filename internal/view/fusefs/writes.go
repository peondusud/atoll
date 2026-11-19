package fusefs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// What a view that writes answers (specs.md §21.2): each operation goes to the writer, which makes it on one disk, or a
// folder's on every disk that holds it; a view read only answers EROFS.
var (
	_ fs.NodeCreater       = (*node)(nil)
	_ fs.NodeMkdirer       = (*node)(nil)
	_ fs.NodeSymlinker     = (*node)(nil)
	_ fs.NodeUnlinker      = (*node)(nil)
	_ fs.NodeRmdirer       = (*node)(nil)
	_ fs.NodeRenamer       = (*node)(nil)
	_ fs.NodeSetattrer     = (*node)(nil)
	_ fs.NodeLinker        = (*node)(nil)
	_ fs.NodeSetxattrer    = (*node)(nil)
	_ fs.NodeRemovexattrer = (*node)(nil)
)

// errnoOf says what a failed write answers. What says the disk cannot be relied on, or the catalog cannot be read, is
// EIO and is logged; what a disk itself answered, as EACCES or ENOSPC, passes as it is.
func errnoOf(what, p string, err error) syscall.Errno {
	var errno syscall.Errno
	switch {
	case err == nil:
		return 0
	case errors.Is(err, writer.ErrExists), errors.Is(err, ops.ErrPathTaken):
		return syscall.EEXIST
	case errors.Is(err, writer.ErrNotFound), errors.Is(err, ops.ErrGone), fsx.IsAbsent(err):
		return syscall.ENOENT
	case errors.Is(err, writer.ErrNotFolder):
		return syscall.ENOTDIR
	case errors.Is(err, writer.ErrIsFolder):
		return syscall.EISDIR
	case errors.Is(err, writer.ErrNotEmpty), errors.Is(err, ops.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, writer.ErrRefused), errors.Is(err, ops.ErrNameRefused), errors.Is(err, writer.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, writer.ErrCrossDisk), errors.Is(err, ops.ErrNoLink):
		return syscall.EXDEV
	case errors.Is(err, writer.ErrNoSpace):
		return syscall.ENOSPC
	case errors.Is(err, writer.ErrNoHandle):
		return syscall.EROFS
	case errors.Is(err, ops.ErrDiskLost), errors.Is(err, view.ErrUnreadable):
		return failed(what, p, err)
	case errors.As(err, &errno):
		return errno
	}
	return failed(what, p, err)
}

// owner is who asks, as the kernel says.
func owner(ctx context.Context) writer.Owner {
	if c, ok := fuse.FromContext(ctx); ok {
		return writer.Owner{UID: c.Uid, GID: c.Gid}
	}
	return writer.Owner{}
}

func (n *node) Create(ctx context.Context, name string, _ uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if n.w == nil {
		return nil, nil, 0, syscall.EROFS
	}
	p := join(n.logical(), name)
	f, v, err := n.w.Create(whole(ctx), p, mode&0o7777, owner(ctx))
	if err != nil {
		return nil, nil, 0, errnoOf("the view cannot make a file", p, err)
	}
	fill(&out.Attr, v)
	return n.child(ctx, p, v), &wfile{f: f}, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.w == nil {
		return nil, syscall.EROFS
	}
	p := join(n.logical(), name)
	v, err := n.w.Mkdir(whole(ctx), p, mode&0o7777, owner(ctx))
	if err != nil {
		return nil, errnoOf("the view cannot make a folder", p, err)
	}
	fill(&out.Attr, v)
	return n.child(ctx, p, v), 0
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.w == nil {
		return nil, syscall.EROFS
	}
	p := join(n.logical(), name)
	v, err := n.w.Symlink(whole(ctx), p, target, owner(ctx))
	if err != nil {
		return nil, errnoOf("the view cannot make a symbolic link", p, err)
	}
	fill(&out.Attr, v)
	return n.child(ctx, p, v), 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	if n.w == nil {
		return syscall.EROFS
	}
	p := join(n.logical(), name)
	return errnoOf("the view cannot delete a file", p, n.w.Unlink(whole(ctx), p))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if n.w == nil {
		return syscall.EROFS
	}
	p := join(n.logical(), name)
	return errnoOf("the view cannot remove a folder", p, n.w.Rmdir(whole(ctx), p))
}

// The flags of renameat2 that reach a FUSE server.
const (
	renameNoReplace = 1
	renameExchange  = 2
)

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if n.w == nil {
		return syscall.EROFS
	}
	to, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	if flags&renameExchange != 0 {
		return syscall.EINVAL
	}
	from := join(n.logical(), name)
	return errnoOf("the view cannot rename", from, n.w.Rename(whole(ctx), from, join(to.logical(), newName), flags&renameNoReplace != 0))
}

// Link gives a file a second name on the disk it is presented from (specs.md §21.2). A folder answers EPERM, as on any
// filesystem; what cannot be made there answers EXDEV, as across two filesystems, so that a program copies instead.
func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.w == nil {
		return nil, syscall.EROFS
	}
	source, ok := target.(*node)
	if !ok {
		return nil, syscall.EXDEV
	}
	from, to := source.logical(), join(n.logical(), name)
	if from == "" {
		return nil, syscall.EPERM
	}
	v, err := n.w.Link(whole(ctx), from, to)
	switch {
	case errors.Is(err, writer.ErrIsFolder):
		return nil, syscall.EPERM
	case errors.Is(err, writer.ErrCrossDisk):
		// Whatever the disk answered of it: what wraps it would read as another error.
		return nil, syscall.EXDEV
	case err != nil:
		return nil, errnoOf("the view cannot make a hard link", to, err)
	}
	fill(&out.Attr, v)
	return n.child(ctx, to, v), 0
}

// Setxattr and Removexattr are refused: the view keeps no extended attribute (specs.md §21.2).
func (n *node) Setxattr(context.Context, string, []byte, uint32) syscall.Errno {
	return syscall.ENOTSUP
}

func (n *node) Removexattr(context.Context, string) syscall.Errno { return syscall.ENOTSUP }

func (n *node) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if n.w == nil {
		return syscall.EROFS
	}
	p := n.logical()
	if p == "" {
		return syscall.EPERM
	}
	v, errno := n.lookup(ctx, p)
	if errno != 0 {
		return errno
	}
	a := &ops.UserAttrs{}
	if mode, ok := in.GetMode(); ok {
		m := mode & 0o7777
		a.Mode = &m
	}
	if uid, ok := in.GetUID(); ok {
		a.UID = &uid
	}
	if gid, ok := in.GetGID(); ok {
		a.GID = &gid
	}
	if size, ok := in.GetSize(); ok {
		s := int64(size) //nolint:gosec // a size the kernel gives fits
		a.Size = &s
	}
	if t, ok := in.GetATime(); ok {
		a.Atime = timeOf(t)
	}
	if t, ok := in.GetMTime(); ok {
		a.Mtime = timeOf(t)
	}
	got, err := n.w.Setattr(whole(ctx), p, v, a)
	if err != nil {
		return errnoOf("the view cannot change attributes", p, err)
	}
	fill(&out.Attr, got)
	return 0
}

func timeOf(t time.Time) *fsx.Time {
	at := fsx.TimeOf(t.UnixNano())
	return &at
}

// openToWrite opens the file a node presents for writing, through the writer: its original alone (specs.md §21.2).
func (n *node) openToWrite(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.w == nil {
		return nil, 0, syscall.EROFS
	}
	p := n.logical()
	v, errno := n.lookup(ctx, p)
	if errno != 0 {
		return nil, 0, errno
	}
	f, err := n.w.Open(whole(ctx), p, v)
	if err != nil {
		return nil, 0, errnoOf("the view cannot open a file to write it", p, err)
	}
	if flags&syscall.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, 0, errnoOf("the view cannot truncate a file", p, err)
		}
	}
	return &wfile{f: f}, 0, 0
}

// wfile is a file open for writing, on the disk it was made on or presented from.
type wfile struct {
	f *writer.File
}

var (
	_ fs.FileReader    = (*wfile)(nil)
	_ fs.FileWriter    = (*wfile)(nil)
	_ fs.FileFsyncer   = (*wfile)(nil)
	_ fs.FileFlusher   = (*wfile)(nil)
	_ fs.FileReleaser  = (*wfile)(nil)
	_ fs.FileGetattrer = (*wfile)(nil)
)

func (f *wfile) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.f.ReadAt(dest, off)
	if err != nil && n == 0 && !isEOF(err) {
		return nil, errnoOf("the view cannot read a file being written", f.f.Path, err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *wfile) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := f.f.WriteAt(data, off)
	if err != nil {
		return uint32(n), errnoOf("the view cannot write a file", f.f.Path, err) //nolint:gosec // n is at most len(data)
	}
	return uint32(n), 0 //nolint:gosec // n is at most len(data)
}

func (f *wfile) Fsync(context.Context, uint32) syscall.Errno {
	return errnoOf("the view cannot sync a file", f.f.Path, f.f.Sync())
}

func (f *wfile) Flush(context.Context) syscall.Errno { return 0 }

func (f *wfile) Release(context.Context) syscall.Errno {
	if err := f.f.Close(); err != nil {
		slog.Warn("a file written through the view did not close cleanly", "path", f.f.Path, "error", err)
	}
	return 0
}

// Getattr of an open file looks at it, so that its size follows what is written.
func (f *wfile) Getattr(_ context.Context, out *fuse.AttrOut) syscall.Errno {
	st, err := f.f.Info()
	if err != nil {
		return errnoOf("the view cannot look at a file being written", f.f.Path, err)
	}
	fill(&out.Attr, &view.Node{Kind: view.NodeFile, Mode: st.Mode, UID: st.Uid, GID: st.Gid, Size: st.Size,
		MtimeNS: st.Mtime.Sec*1_000_000_000 + st.Mtime.Nsec, Nlink: int64(st.Nlink)})
	return 0
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) }
