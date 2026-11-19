package watch

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/fsx"
)

// inotifyMask is asked of each folder's watch, which is only ever on a folder. It is put through the link
// /proc/self/fd/N to a folder already opened without following a symlink, so the link is followed.
const inotifyMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_CLOSE_WRITE |
	unix.IN_ATTRIB | unix.IN_ONLYDIR | unix.IN_EXCL_UNLINK

// inotifySource is one inotify watch per folder below the root, where fanotify cannot be had: it gives no
// process (specs.md §15.1).
type inotifySource struct {
	ino     int
	root    string
	mountID uint64
	// rootDev and rootIno are the root's folder as it was first watched, and ended says that its watch went.
	rootDev, rootIno uint64
	ended            bool
	tree             *folderTree[int32]
	// moved holds the folders that left their parent in this read, by cookie, until the other end comes.
	moved map[uint32]*folderNode[int32]
	// read reads the instance, and wait waits for it to have something to read, at most a time, saying whether it
	// has: unix.Read and a poll, but in tests.
	read func(buf []byte) (int, error)
	wait func(d time.Duration) bool
	// opened is WatcherOptions.Opened.
	opened func(path string)
	// marker watches the folders below one, as far as the disk's walks go (specs.md §15.8).
	marker *treeMarker[int32]
}

func openInotify(ctx context.Context, root string, limits Limits) (*inotifySource, error) {
	ino, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify: %w", err)
	}
	s := &inotifySource{ino: ino, root: path.Clean(root), moved: map[uint32]*folderNode[int32]{}}
	s.read = func(buf []byte) (int, error) { return unix.Read(ino, buf) }
	s.wait = func(d time.Duration) bool { return readable(ino, d) }
	dir, err := fsx.OpenDir(root)
	if err != nil {
		_ = unix.Close(ino)
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Info()
	if err != nil {
		_ = unix.Close(ino)
		return nil, fmt.Errorf("%w", err)
	}
	s.mountID, s.rootDev, s.rootIno = info.MountID, info.Dev, info.Ino
	wd, err := s.watch(dir)
	if err != nil {
		_ = unix.Close(ino)
		return nil, err
	}
	s.tree = newFolderTree(wd)
	s.marker = &treeMarker[int32]{root: s.root, mountID: s.mountID, limits: limits, tree: s.tree, mark: s.watch}
	if err := s.marker.all(ctx, dir, s.tree.root, "", nil); err != nil {
		_ = unix.Close(ino)
		return nil, err
	}
	return s, nil
}

// watch puts a watch on a folder through its descriptor, so that no path is followed.
func (s *inotifySource) watch(dir fsx.Dir) (int32, error) {
	fd, err := fsx.Descriptor(dir)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	wd, err := unix.InotifyAddWatch(s.ino, "/proc/self/fd/"+strconv.Itoa(fd), inotifyMask)
	if err != nil {
		return 0, fmt.Errorf("inotify watch: %w", err)
	}
	return int32(wd), nil
}

func (s *inotifySource) fd() int { return s.ino }

// alive: the root is still the folder that was watched first. Its watch going ends the reading by itself.
func (s *inotifySource) alive() bool {
	return sameRoot(s.root, s.rootDev, s.rootIno)
}

// errRootGone says that the watch of a source's root went: nothing below it is heard any more.
var errRootGone = errors.New("the watch of the root is gone")

func (s *inotifySource) close() error { return unix.Close(s.ino) }

// moveWait is how long a read that a folder left by a rename, its other end not come yet, waits for that end: the
// kernel queues the two ends of a rename one after the other, so a read can come between them. Without it, a folder
// renamed within the root was forgotten, then watched again from scratch, listed and every name in it reported as
// created (specs.md §15.1).
const moveWait = 10 * time.Millisecond

func (s *inotifySource) drain(buf []byte) ([]change, []int32, error) {
	var out []change
	waited := false
	for {
		n, err := s.read(buf)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EAGAIN) || err == nil && n <= 0:
			if len(s.moved) > 0 && !waited {
				waited = true
				if s.wait(moveWait) {
					continue
				}
			}
			s.settleMoves()
			return out, nil, nil
		case err != nil:
			return out, nil, err
		}
		records, perr := parseInotify(buf[:n])
		for i := range records {
			out = append(out, s.changes(&records[i])...)
		}
		if perr != nil {
			out = append(out, change{kind: KindOverflow})
		}
		if s.ended {
			// The root's folder was removed, or its filesystem unmounted: what is at that path now is watched by
			// nothing, and the watch ends, which whoever asked for it finds out (Watcher.Alive).
			return out, nil, errRootGone
		}
	}
}

// changes gives what a record says, and keeps the tree as the folders move.
func (s *inotifySource) changes(r *inotifyRecord) []change {
	switch {
	case r.mask&unix.IN_Q_OVERFLOW != 0:
		return []change{{kind: KindOverflow}}
	case r.mask&unix.IN_IGNORED != 0:
		switch n := s.tree.nodes[r.wd]; n {
		case nil:
		case s.tree.root:
			s.ended = true
		default:
			s.tree.remove(n)
		}
		return nil
	}
	node := s.tree.nodes[r.wd]
	if node == nil {
		// A watch already forgotten, of a folder that left the root.
		return nil
	}
	dir, ok := node.path()
	if !ok {
		return nil
	}
	p, folder := join(dir, r.name), r.mask&unix.IN_ISDIR != 0
	var out []change
	for _, k := range []struct {
		bit  uint32
		kind string
	}{{unix.IN_CREATE, KindCreated}, {unix.IN_CLOSE_WRITE, KindWritten}, {unix.IN_ATTRIB, KindAttrib}, {unix.IN_DELETE, KindDeleted}, {unix.IN_MOVED_FROM, KindMovedFrom}, {unix.IN_MOVED_TO, KindMovedTo}} {
		if r.mask&k.bit != 0 {
			out = append(out, change{path: p, folder: folder, kind: k.kind})
		}
	}
	if folder && r.name != "" {
		switch {
		case r.mask&unix.IN_MOVED_FROM != 0:
			if n := s.tree.detach(node, r.name); n != nil {
				s.moved[r.cookie] = n
			}
		case r.mask&unix.IN_DELETE != 0:
			if n := s.tree.detach(node, r.name); n != nil {
				s.tree.remove(n)
			}
		case r.mask&unix.IN_MOVED_TO != 0:
			if n := s.moved[r.cookie]; n != nil {
				delete(s.moved, r.cookie)
				s.tree.attach(n, node, r.name)
			} else {
				out = append(out, s.arrive(node, r.name)...)
			}
		case r.mask&unix.IN_CREATE != 0:
			out = append(out, s.arrive(node, r.name)...)
		}
	}
	return out
}

// arrive watches a folder that appeared, with everything below it, and lists what it holds, as it may have
// been filled first.
func (s *inotifySource) arrive(parent *folderNode[int32], name string) []change {
	dirPath, ok := parent.path()
	if !ok {
		return nil
	}
	at := join(dirPath, name)
	if !s.marker.limits.Fits(at) {
		// Past what the disk's walks read: nothing to watch there (specs.md §15.8).
		return nil
	}
	sub, err := openBelow(s.root, at, s.mountID)
	if err != nil {
		return nil
	}
	defer func() { _ = sub.Close() }()
	if s.opened != nil {
		s.opened(at)
	}
	wd, err := s.watch(sub)
	if err != nil {
		return nil
	}
	var found []change
	_ = s.marker.all(context.Background(), sub, s.tree.add(parent, name, wd), at, &found)
	return found
}

// readable waits at most d for an inotify instance to have something to read.
func readable(ino int, d time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(ino), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, int(d.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && n > 0
	}
}

// settleMoves forgets the folders that left and did not come back below the root: their watches go.
func (s *inotifySource) settleMoves() {
	for cookie, n := range s.moved {
		delete(s.moved, cookie)
		for _, wd := range s.tree.remove(n) {
			_, _ = unix.InotifyRmWatch(s.ino, uint32(wd))
		}
	}
}
