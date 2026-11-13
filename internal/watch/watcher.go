package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// How a disk is watched (specs.md §15.1).
const (
	HowFilesystem = "filesystem" // one fanotify mark on the whole filesystem
	HowFolders    = "folders"    // one fanotify mark per folder, where a filesystem mark is refused
	HowInotify    = "inotify"    // one inotify watch per folder, where fanotify cannot be had
)

// WatcherOptions says what a watcher needs besides the disks.
type WatcherOptions struct {
	// ViewMount is view.mount, whose mergerfs process makes the view's events.
	ViewMount string
	// Now gives the time of an event, in Unix nanoseconds.
	Now func() int64
	// Inotify takes inotify on every disk, as where fanotify cannot be had: for tests.
	Inotify bool
}

// Watcher watches disks and says what changed on them: hints of where to look, never decisions (specs.md
// §15.1). Its methods are safe for concurrent use.
type Watcher struct {
	opt    WatcherOptions
	events chan Event
	self   int
	exe    string

	mu      sync.Mutex
	disks   map[string]*diskWatch
	origins map[int]originSeen
	closed  bool
}

// originSeen is what a process was found to be, and when.
type originSeen struct {
	origin string
	atNS   int64
}

// originKept is how long what a process is stays known by its number: the events of one burst, not the life of the
// daemon. The kernel gives the number of a process that ended to another, and an answer kept for good counted a
// user's changes as Atoll's own once a command of Atoll that wrote to a disk had ended.
const originKept = 10 * time.Second

type diskWatch struct {
	src     source
	stop    chan struct{}
	wake    [2]int // a pipe whose write wakes the reader's poll
	done    chan struct{}
	stopped sync.Once
}

// source is one way of watching one disk.
type source interface {
	fd() int
	// drain reads what waits, and gives what it says; an error ends the watch of that disk.
	drain(buf []byte) ([]change, []int32, error)
	// alive says whether the source can still hear its disk: false once what it listens through went, with a
	// filesystem that was unmounted or a root that is another folder now. It is asked from another goroutine than
	// the one that drains, and reads nothing that one writes.
	alive() bool
	close() error
}

// NewWatcher makes a watcher of no disk yet.
func NewWatcher(opt WatcherOptions) *Watcher {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	return &Watcher{opt: opt, events: make(chan Event, 4096), self: os.Getpid(), exe: exe, disks: map[string]*diskWatch{}, origins: map[int]originSeen{}}
}

// SetViewMount gives the watcher another view.mount, as a configuration read again does. What each process was
// found to be is forgotten with it: the view's process is told by that path.
func (w *Watcher) SetViewMount(mount string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opt.ViewMount = mount
	w.origins = map[int]originSeen{}
}

// ViewMount gives the view.mount the watcher tells the view's process by.
func (w *Watcher) ViewMount() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opt.ViewMount
}

// Events gives what changed. A reader that stops reading lets the kernel's queue fill, and an overflow is a
// rescan of that disk.
func (w *Watcher) Events() <-chan Event {
	return w.events
}

// Watch starts watching a disk that passed its check, below <mount>/<root>, and says how. Watching a disk
// again stops the earlier watch first.
func (w *Watcher) Watch(ctx context.Context, d *config.Disk) (string, error) {
	w.Stop(d.ID)
	root := path.Join(d.Mount, d.Root)
	src, how, err := openSource(ctx, root, w.opt.Inotify)
	if err != nil {
		return "", fmt.Errorf("watch %s: %w", d.ID, err)
	}
	if err := w.arm(d.ID, src); err != nil {
		return "", err
	}
	return how, nil
}

// arm starts reading a source for a disk, and closes it if it cannot.
func (w *Watcher) arm(id string, src source) error {
	dw := &diskWatch{src: src, stop: make(chan struct{}), done: make(chan struct{})}
	if err := unix.Pipe2(dw.wake[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		_ = src.close()
		return fmt.Errorf("watch %s: %w", id, err)
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		_ = src.close()
		closePipe(dw.wake)
		return fmt.Errorf("watch %s: the watcher is closed", id)
	}
	w.disks[id] = dw
	w.mu.Unlock()
	go w.run(id, src, dw)
	return nil
}

// Alive says whether a disk is watched and its watch still hears it (specs.md §15.1). A watch ends by itself when
// its source fails, which it tells with an overflow. And it goes deaf without a word when its disk is unmounted: the
// kernel drops the marks with the filesystem, and a disk mounted there again is another filesystem to it, which
// nothing listens to. Whoever keeps a disk watched asks, and watches it again.
func (w *Watcher) Alive(id string) bool {
	w.mu.Lock()
	dw := w.disks[id]
	w.mu.Unlock()
	if dw == nil {
		return false
	}
	select {
	case <-dw.done:
		return false
	default:
	}
	return dw.src.alive()
}

// marked says whether a fanotify group still holds a mark, from the head of what the kernel lists of it. It says
// true when that cannot be read: nothing then says that the marks went.
func marked(group int) bool {
	fd, err := unix.Open("/proc/self/fdinfo/"+strconv.Itoa(group), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return true
	}
	defer func() { _ = unix.Close(fd) }()
	// The descriptor's own lines, the group's, then one line a mark: the first mark is within the first lines,
	// however many follow.
	buf := make([]byte, 4096)
	n := 0
	for n < len(buf) {
		got, err := unix.Read(fd, buf[n:])
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return true
		}
		if got <= 0 {
			break
		}
		n += got
	}
	return hasMarks(buf[:n])
}

// hasMarks says whether what /proc/self/fdinfo lists of a fanotify group names a mark: a line of fanotify's that is
// not the group's own, "fanotify flags:". A mark on an inode, a mount or a filesystem each has such a line.
func hasMarks(fdinfo []byte) bool {
	for line := range bytes.SplitSeq(fdinfo, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("fanotify ")) && !bytes.HasPrefix(line, []byte("fanotify flags:")) {
			return true
		}
	}
	return false
}

// openBelow opens the folder at a logical path below a root, one name at a time and following no symbolic link, and
// gives it only when it is on the mount the root was watched on. A folder that appears is opened some time after its
// event: by its path, with a folder above it traded for a link meanwhile, it would be a folder anywhere on the
// machine, watched and listed as one of the library's (specs.md §15.1).
func openBelow(root, at string, mountID uint64) (fsx.Dir, error) {
	dir, err := fsx.OpenDir(root)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	for name := range strings.SplitSeq(at, "/") {
		sub, err := dir.OpenDir(name)
		_ = dir.Close()
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		dir = sub
	}
	info, err := dir.Info()
	if err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("%w", err)
	}
	if info.MountID != mountID {
		_ = dir.Close()
		return nil, fmt.Errorf("%q is on another mount than its root", at)
	}
	return dir, nil
}

// sameRoot says whether the folder at a root's path is still the one a source put its first watch on. A root
// removed and made again, or renamed away for another, leaves the watches on the folder that went.
func sameRoot(root string, dev, ino uint64) bool {
	dir, err := fsx.OpenDir(root)
	if err != nil {
		return false
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Info()
	return err == nil && info.Dev == dev && info.Ino == ino
}

// Stop stops watching a disk, if it was watched.
func (w *Watcher) Stop(id string) {
	w.mu.Lock()
	dw := w.disks[id]
	delete(w.disks, id)
	w.mu.Unlock()
	if dw != nil {
		dw.halt()
	}
}

// Close stops every watch. Events stops being written to, and is never closed.
func (w *Watcher) Close() {
	w.mu.Lock()
	w.closed = true
	all := w.disks
	w.disks = map[string]*diskWatch{}
	w.mu.Unlock()
	for _, dw := range all {
		dw.halt()
	}
}

func (dw *diskWatch) halt() {
	dw.stopped.Do(func() {
		close(dw.stop)
		_, _ = unix.Write(dw.wake[1], []byte{0})
	})
	<-dw.done
}

func closePipe(p [2]int) {
	_ = unix.Close(p[0])
	_ = unix.Close(p[1])
}

// run reads a disk's events until it is stopped. An error of the source is an overflow: the disk is looked at
// whole, and its watch ends.
func (w *Watcher) run(disk string, src source, dw *diskWatch) {
	defer close(dw.done)
	defer closePipe(dw.wake)
	defer func() { _ = src.close() }()
	buf := make([]byte, 64<<10)
	fds := []unix.PollFd{{Fd: int32(src.fd()), Events: unix.POLLIN}, {Fd: int32(dw.wake[0]), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != nil && !errors.Is(err, unix.EINTR) {
			w.send(dw, Event{Disk: disk, Kind: KindOverflow, Origin: OriginUnknown, AtNS: w.now()})
			return
		}
		select {
		case <-dw.stop:
			return
		default:
		}
		changes, pids, err := src.drain(buf)
		for i, c := range changes {
			e := Event{Disk: disk, Path: c.path, Folder: c.folder, Kind: c.kind, Origin: OriginUnknown, AtNS: w.now()}
			if c.kind != KindOverflow && pids != nil {
				e.Origin = w.origin(int(pids[i]))
			}
			if !w.send(dw, e) {
				return
			}
		}
		if err != nil {
			w.send(dw, Event{Disk: disk, Kind: KindOverflow, Origin: OriginUnknown, AtNS: w.now()})
			return
		}
	}
}

func (w *Watcher) send(dw *diskWatch, e Event) bool {
	select {
	case w.events <- e:
		return true
	case <-dw.stop:
		return false
	}
}

func (w *Watcher) now() int64 {
	return w.opt.Now()
}

// origin tells who made an event, from the process behind it (specs.md §15.1). What a process is changes only
// when its number is used again, so the answer is kept for originKept.
func (w *Watcher) origin(pid int) string {
	if pid <= 0 {
		return OriginUnknown
	}
	if pid == w.self {
		return OriginAtoll
	}
	now := w.now()
	w.mu.Lock()
	known, ok := w.origins[pid]
	view := w.opt.ViewMount
	w.mu.Unlock()
	if ok && now-known.atNS < originKept.Nanoseconds() && now >= known.atNS {
		return known.origin
	}
	o := OriginUser
	switch {
	case w.exe != "" && procExe(pid) == w.exe:
		o = OriginAtoll
	case view != "" && servesView(pid, view):
		o = OriginView
	}
	w.mu.Lock()
	if len(w.origins) > 1024 {
		w.origins = map[int]originSeen{}
	}
	w.origins[pid] = originSeen{origin: o, atNS: now}
	w.mu.Unlock()
	return o
}

func procExe(pid int) string {
	target, err := fsx.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return target
}

// servesView says whether a process is mergerfs with the view's mount point among its arguments.
func servesView(pid int, mount string) bool {
	comm, err := fsx.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil || strings.TrimSpace(string(comm)) != "mergerfs" {
		return false
	}
	cmdline, err := fsx.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	mount = path.Clean(mount)
	for _, arg := range bytes.Split(cmdline, []byte{0}) {
		if a := string(arg); a != "" && path.Clean(a) == mount {
			return true
		}
	}
	return false
}

// openSource watches a disk's root the first way that works: a filesystem mark, a mark per folder, inotify.
func openSource(ctx context.Context, root string, inotifyOnly bool) (source, string, error) {
	if !inotifyOnly {
		src, err := openFilesystem(root)
		if err == nil {
			return src, HowFilesystem, nil
		}
		folders, err := openFolders(ctx, root)
		if err == nil {
			return folders, HowFolders, nil
		}
	}
	src, err := openInotify(ctx, root)
	if err != nil {
		return nil, "", err
	}
	return src, HowInotify, nil
}

// filesystemSource is one fanotify mark on the filesystem that holds the root, which reports every name with
// its folder's handle. It holds no descriptor of the disk between two events: one would keep the disk from
// being unmounted.
type filesystemSource struct {
	group int
	root  string
}

func openFilesystem(root string) (*filesystemSource, error) {
	group, err := unix.FanotifyInit(fanCloexec|fanNonblock|fanReportDFIDName|fanReportFID|fanReportTargetFID, unix.O_RDONLY|unix.O_LARGEFILE|unix.O_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("fanotify: %w", err)
	}
	if err := unix.FanotifyMark(group, fanMarkAdd|fanMarkFilesystem, fsMask, unix.AT_FDCWD, root); err != nil {
		_ = unix.Close(group)
		return nil, fmt.Errorf("fanotify mark %s: %w", root, err)
	}
	return &filesystemSource{group: group, root: path.Clean(root)}, nil
}

func (s *filesystemSource) fd() int { return s.group }

// alive: the mark is on the filesystem, and goes with it when it is unmounted.
func (s *filesystemSource) alive() bool { return marked(s.group) }

func (s *filesystemSource) close() error {
	return unix.Close(s.group)
}

func (s *filesystemSource) drain(buf []byte) ([]change, []int32, error) {
	var out []change
	var pids []int32
	for {
		records, err := readFanotify(s.group, buf)
		if errors.Is(err, unix.EAGAIN) {
			return out, pids, nil
		}
		for i := range records {
			for _, c := range fanChanges(&records[i], s.resolve) {
				out, pids = append(out, c), append(pids, records[i].pid)
			}
		}
		if err != nil {
			return out, pids, err
		}
	}
}

// resolve finds where a handle leads now, through the folder's handle, opening the root for that alone. A root
// that is now another filesystem's opens no handle of this one.
func (s *filesystemSource) resolve(info *fanInfo) (string, place) {
	// Not O_PATH: open_by_handle_at refuses one as the descriptor of the filesystem.
	mountFD, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", unknown
	}
	fd, err := unix.OpenByHandleAt(mountFD, unix.NewFileHandle(info.handleType, info.handle), unix.O_PATH|unix.O_CLOEXEC)
	_ = unix.Close(mountFD)
	if err != nil {
		return "", unknown
	}
	defer func() { _ = unix.Close(fd) }()
	dir, err := fsx.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || strings.HasSuffix(dir, " (deleted)") {
		return "", unknown
	}
	return below(s.root, dir, info.name)
}

// readFanotify reads once, closes the descriptors events carry, and gives the records. EAGAIN says nothing
// waits.
func readFanotify(group int, buf []byte) ([]fanRecord, error) {
	n, err := unix.Read(group, buf)
	switch {
	case errors.Is(err, unix.EINTR):
		return nil, nil
	case err != nil:
		return nil, err
	case n <= 0:
		return nil, unix.EAGAIN
	}
	records, err := parseFanotify(buf[:n])
	for i := range records {
		if records[i].fd >= 0 {
			_ = unix.Close(int(records[i].fd))
		}
	}
	if err != nil {
		// What could not be read is lost: the disk is looked at whole.
		records = append(records, fanRecord{mask: fanQOverflow})
	}
	return records, nil
}

// foldersSource is one fanotify mark per folder below the root, for a filesystem that refuses a filesystem
// mark: ntfs-3g and lowntfs-3g, whose fsid is 0 (docs/research/watcher.md).
type foldersSource struct {
	group   int
	root    string
	mountID uint64
	// rootDev and rootIno are the root's folder as it was marked.
	rootDev, rootIno uint64
	tree             *folderTree[string]
}

func openFolders(ctx context.Context, root string) (*foldersSource, error) {
	group, err := unix.FanotifyInit(fanCloexec|fanNonblock|fanUnlimitedMarks|fanReportDFIDName|fanReportFID, unix.O_RDONLY|unix.O_LARGEFILE|unix.O_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("fanotify: %w", err)
	}
	s := &foldersSource{group: group, root: path.Clean(root)}
	dir, err := fsx.OpenDir(root)
	if err != nil {
		_ = unix.Close(group)
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Info()
	if err != nil {
		_ = unix.Close(group)
		return nil, fmt.Errorf("%w", err)
	}
	s.mountID, s.rootDev, s.rootIno = info.MountID, info.Dev, info.Ino
	key, err := s.mark(dir)
	if err != nil {
		_ = unix.Close(group)
		return nil, err
	}
	s.tree = newFolderTree(key)
	if err := s.markBelow(ctx, dir, s.tree.root, "", nil); err != nil {
		_ = unix.Close(group)
		return nil, err
	}
	return s, nil
}

// mark puts a mark on a folder, and gives its handle's key.
func (s *foldersSource) mark(dir fsx.Dir) (string, error) {
	fd, err := fsx.Descriptor(dir)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if err := unix.FanotifyMark(s.group, fanMarkAdd, foldersMask, fd, ""); err != nil {
		return "", fmt.Errorf("fanotify mark: %w", err)
	}
	h, _, err := unix.NameToHandleAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return "", fmt.Errorf("handle: %w", err)
	}
	return handleKey(h.Type(), h.Bytes()), nil
}

// markBelow marks every folder below one, but those of another mount. With found, it also lists what it
// finds there, at paths below prefix.
func (s *foldersSource) markBelow(ctx context.Context, dir fsx.Dir, node *folderNode[string], prefix string, found *[]change) error {
	return eachEntry(ctx, dir, s.mountID, prefix, found, func(sub fsx.Dir, name, path string) error {
		key, err := s.mark(sub)
		if err != nil {
			return err
		}
		return s.markBelow(ctx, sub, s.tree.add(node, name, key), path, found)
	})
}

// eachEntry calls fn on each folder of a folder, open, but those of another mount, and, with found, lists every
// name there as created: what a folder held before its mark came is known only so (specs.md §15.1). A name that
// went in the meantime is skipped.
func eachEntry(ctx context.Context, dir fsx.Dir, mountID uint64, prefix string, found *[]change, fn func(sub fsx.Dir, name, path string) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	names, err := dir.Names()
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	for _, name := range names {
		st, err := dir.Stat(name)
		if err != nil {
			continue
		}
		path := join(prefix, name)
		if found != nil {
			*found = append(*found, change{path: path, folder: st.Kind == fsx.KindDir, kind: KindCreated})
		}
		if st.Kind != fsx.KindDir || st.MountID != mountID {
			continue
		}
		sub, err := dir.OpenDir(name)
		if err != nil {
			continue
		}
		err = fn(sub, name, path)
		_ = sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *foldersSource) fd() int { return s.group }

// alive: the marks are on the folders, and go with their filesystem when it is unmounted; and the root is still the
// folder that was marked first.
func (s *foldersSource) alive() bool {
	return marked(s.group) && sameRoot(s.root, s.rootDev, s.rootIno)
}

func (s *foldersSource) close() error { return unix.Close(s.group) }

func (s *foldersSource) drain(buf []byte) ([]change, []int32, error) {
	var out []change
	var pids []int32
	for {
		records, err := readFanotify(s.group, buf)
		if errors.Is(err, unix.EAGAIN) {
			return out, pids, nil
		}
		for i := range records {
			r := &records[i]
			for _, c := range fanChanges(r, s.resolve) {
				out, pids = append(out, c), append(pids, r.pid)
			}
			// What a listing found has no process.
			for _, c := range s.follow(r) {
				out, pids = append(out, c), append(pids, 0)
			}
		}
		if err != nil {
			return out, pids, err
		}
	}
}

// resolve finds a name's path from the folder its handle names, as the tree knows it.
func (s *foldersSource) resolve(info *fanInfo) (string, place) {
	node := s.tree.nodes[handleKey(info.handleType, info.handle)]
	if node == nil {
		return "", unknown
	}
	dir, ok := node.path()
	if !ok {
		return "", outside
	}
	return join(dir, info.name), inside
}

// follow keeps the tree as the folders move: one that leaves is detached, one that arrives is attached again
// or, new, marked with everything below it, which it lists, as it may have been filled before its mark.
func (s *foldersSource) follow(r *fanRecord) []change {
	if r.mask&fanOnDir == 0 {
		return nil
	}
	var found []change
	for i := range r.infos {
		info := &r.infos[i]
		if info.kind != infoDFIDName || info.name == "" || info.name == "." {
			continue
		}
		parent := s.tree.nodes[handleKey(info.handleType, info.handle)]
		if parent == nil {
			continue
		}
		switch {
		case r.mask&(fanMovedFrom|fanDelete) != 0:
			if n := s.tree.detach(parent, info.name); n != nil && r.mask&fanDelete != 0 {
				s.tree.remove(n)
			}
		case r.mask&(fanMovedTo|fanCreate) != 0:
			found = append(found, s.arrive(parent, info.name)...)
		}
	}
	return found
}

// arrive takes a folder that appeared below a marked one, and gives what it holds.
func (s *foldersSource) arrive(parent *folderNode[string], name string) []change {
	dirPath, ok := parent.path()
	if !ok {
		return nil
	}
	at := join(dirPath, name)
	sub, err := openBelow(s.root, at, s.mountID)
	if err != nil {
		return nil
	}
	defer func() { _ = sub.Close() }()
	fd, err := fsx.Descriptor(sub)
	if err != nil {
		return nil
	}
	h, _, err := unix.NameToHandleAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return nil
	}
	if n := s.tree.nodes[handleKey(h.Type(), h.Bytes())]; n != nil {
		s.tree.attach(n, parent, name)
		return nil
	}
	key, err := s.mark(sub)
	if err != nil {
		return nil
	}
	var found []change
	_ = s.markBelow(context.Background(), sub, s.tree.add(parent, name, key), at, &found)
	return found
}
