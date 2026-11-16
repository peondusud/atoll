//go:build linux && cgo && fuse3

package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/view"
)

// Built says whether this build holds the view.
const Built = true

// options are those of the mount: read only, as the read path comes first; the kernel checks permissions against
// what the view presents, and keeps attributes and names for 1 s and no absent name, as for mergerfs (specs.md §21.1,
// §21.2). Every user may read it, as a media server does.
var options = []string{"-o", "ro,fsname=atoll,subtype=atoll,default_permissions,allow_other,attr_timeout=1,entry_timeout=1,negative_timeout=0"}

// fileSystem answers FUSE from the tree.
type fileSystem struct {
	fuse.FileSystemBase
	tree *view.Tree
	// ready is closed once the kernel has the mount.
	ready chan struct{}
	once  sync.Once

	mu   sync.Mutex
	next uint64
	open map[uint64]fsx.File
}

// Mounted is a view being served.
type Mounted struct {
	host *fuse.FileSystemHost
	done chan bool
	fs   *fileSystem
	// unmounted makes Unmount run once; err is what it gave.
	unmounted sync.Once
	err       error
}

// Mount serves the view at mount until Unmount, once the kernel has it.
func Mount(tree *view.Tree, mount string) (*Mounted, error) {
	fs := &fileSystem{tree: tree, ready: make(chan struct{}), open: map[uint64]fsx.File{}}
	host := fuse.NewFileSystemHost(fs)
	host.SetCapReaddirPlus(true)
	m := &Mounted{host: host, done: make(chan bool, 1), fs: fs}
	go func() { m.done <- host.Mount(mount, options) }()
	select {
	case <-fs.ready:
		return m, nil
	case ok := <-m.done:
		m.done <- ok
		return nil, fmt.Errorf("fuse: the view could not be mounted at %q", mount)
	}
}

// Unmount stops serving the view, and waits until it has. A second call gives what the first gave.
func (m *Mounted) Unmount() error {
	m.unmounted.Do(func() {
		if !m.host.Unmount() {
			m.err = errors.New("fuse: the view could not be unmounted")
			return
		}
		<-m.done
		m.fs.mu.Lock()
		defer m.fs.mu.Unlock()
		for fh, f := range m.fs.open {
			_ = f.Close()
			delete(m.fs.open, fh)
		}
	})
	return m.err
}

func (fs *fileSystem) Init() {
	fs.once.Do(func() { close(fs.ready) })
}

// logical gives the logical path of a FUSE path, "" for the root.
func logical(path string) string {
	return strings.TrimPrefix(path, "/")
}

func (fs *fileSystem) lookup(path string) (*view.Node, int) {
	n, err := fs.tree.Lookup(context.Background(), logical(path))
	switch {
	case err != nil:
		slog.Warn("the view cannot read the catalog", "path", logical(path), "error", err)
		return nil, -fuse.EIO
	case n == nil:
		return nil, -fuse.ENOENT
	}
	return n, 0
}

func fill(st *fuse.Stat_t, n *view.Node) {
	*st = fuse.Stat_t{}
	switch n.Kind {
	case view.NodeDir:
		st.Mode = fuse.S_IFDIR
	case view.NodeSymlink:
		st.Mode = fuse.S_IFLNK
	case view.NodeFile:
		st.Mode = fuse.S_IFREG
	}
	st.Mode |= n.Mode & 0o7777
	st.Nlink = uint32(max(n.Nlink, 1))
	st.Uid, st.Gid = n.UID, n.GID
	st.Size = n.Size
	t := fuse.NewTimespec(time.Unix(0, n.MtimeNS))
	st.Atim, st.Mtim, st.Ctim = t, t, t
	st.Blksize = 4096
	st.Blocks = (n.Size + 511) / 512
}

func (fs *fileSystem) Getattr(path string, st *fuse.Stat_t, _ uint64) int {
	n, errc := fs.lookup(path)
	if errc != 0 {
		return errc
	}
	fill(st, n)
	return 0
}

func (fs *fileSystem) Readlink(path string) (int, string) {
	n, errc := fs.lookup(path)
	switch {
	case errc != 0:
		return errc, ""
	case n.Kind != view.NodeSymlink:
		return -fuse.EINVAL, ""
	}
	return 0, n.Target
}

func (fs *fileSystem) Opendir(path string) (int, uint64) {
	n, errc := fs.lookup(path)
	switch {
	case errc != 0:
		return errc, ^uint64(0)
	case n.Kind != view.NodeDir:
		return -fuse.ENOTDIR, ^uint64(0)
	}
	return 0, 0
}

func (fs *fileSystem) Readdir(path string, add func(name string, stat *fuse.Stat_t, ofst int64) bool, _ int64, _ uint64) int {
	children, err := fs.tree.List(context.Background(), logical(path))
	if err != nil {
		return -fuse.EIO
	}
	add(".", nil, 0)
	add("..", nil, 0)
	for i := range children {
		var st fuse.Stat_t
		fill(&st, &children[i].Node)
		if !add(children[i].Name, &st, 0) {
			break
		}
	}
	return 0
}

func (fs *fileSystem) Releasedir(string, uint64) int { return 0 }

func (fs *fileSystem) Open(path string, flags int) (int, uint64) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&syscall.O_TRUNC != 0 {
		return -fuse.EROFS, ^uint64(0)
	}
	n, errc := fs.lookup(path)
	switch {
	case errc != 0:
		return errc, ^uint64(0)
	case n.Kind == view.NodeDir:
		return -fuse.EISDIR, ^uint64(0)
	}
	f, err := fs.tree.Open(logical(path), n)
	switch {
	case errors.Is(err, view.ErrUnreadable), errors.Is(err, syscall.EIO), errors.Is(err, syscall.ENOTCONN), errors.Is(err, syscall.ENODEV):
		slog.Warn("the view cannot read a copy's disk", "path", logical(path), "disk", n.Disk, "error", err)
		return -fuse.EIO, ^uint64(0)
	case errors.Is(err, syscall.ENOENT):
		// The catalog holds a copy its disk no longer has: the next look records it.
		return -fuse.ENOENT, ^uint64(0)
	case err != nil:
		slog.Warn("the view cannot open a copy", "path", logical(path), "disk", n.Disk, "error", err)
		return -fuse.EIO, ^uint64(0)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.next++
	fs.open[fs.next] = f
	return 0, fs.next
}

func (fs *fileSystem) Read(_ string, buff []byte, ofst int64, fh uint64) int {
	fs.mu.Lock()
	f := fs.open[fh]
	fs.mu.Unlock()
	if f == nil {
		return -fuse.EBADF
	}
	n, err := f.ReadAt(buff, ofst)
	if err != nil && !errors.Is(err, io.EOF) {
		return -fuse.EIO
	}
	return n
}

func (fs *fileSystem) Release(_ string, fh uint64) int {
	fs.mu.Lock()
	f := fs.open[fh]
	delete(fs.open, fh)
	fs.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
	return 0
}

func (fs *fileSystem) Statfs(_ string, st *fuse.Statfs_t) int {
	s, err := fs.tree.Statfs(context.Background())
	if err != nil {
		return -fuse.EIO
	}
	*st = fuse.Statfs_t{Bsize: s.Bsize, Frsize: s.Bsize, Blocks: s.Blocks, Bfree: s.Bfree, Bavail: s.Bavail, Namemax: s.Namemax}
	return 0
}

func (fs *fileSystem) Access(path string, mask uint32) int {
	if _, errc := fs.lookup(path); errc != 0 {
		return errc
	}
	if mask&2 != 0 {
		return -fuse.EROFS
	}
	return 0
}

// The read path is mounted read only: nothing writes (specs.md §21.2).

func (fs *fileSystem) Mknod(string, uint32, uint64) int        { return -fuse.EROFS }
func (fs *fileSystem) Mkdir(string, uint32) int                { return -fuse.EROFS }
func (fs *fileSystem) Unlink(string) int                       { return -fuse.EROFS }
func (fs *fileSystem) Rmdir(string) int                        { return -fuse.EROFS }
func (fs *fileSystem) Link(string, string) int                 { return -fuse.EROFS }
func (fs *fileSystem) Symlink(string, string) int              { return -fuse.EROFS }
func (fs *fileSystem) Rename(string, string) int               { return -fuse.EROFS }
func (fs *fileSystem) Chmod(string, uint32) int                { return -fuse.EROFS }
func (fs *fileSystem) Chown(string, uint32, uint32) int        { return -fuse.EROFS }
func (fs *fileSystem) Utimens(string, []fuse.Timespec) int     { return -fuse.EROFS }
func (fs *fileSystem) Truncate(string, int64, uint64) int      { return -fuse.EROFS }
func (fs *fileSystem) Write(string, []byte, int64, uint64) int { return -fuse.EROFS }
func (fs *fileSystem) Setxattr(string, string, []byte, int) int {
	return -fuse.EROFS
}
func (fs *fileSystem) Removexattr(string, string) int { return -fuse.EROFS }
func (fs *fileSystem) Create(string, int, uint32) (int, uint64) {
	return -fuse.EROFS, ^uint64(0)
}
