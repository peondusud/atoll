package watch

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/fsx"
)

// What a source reads through is a descriptor: these tests read fanotify's records through a pipe, written as the
// kernel writes them, as a group needs privileges a test has not (test/loopdev runs the groups themselves).

// newPipe gives a pipe whose reads do not block.
func newPipe(t *testing.T) [2]int {
	t.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeAll(t *testing.T, fd int, data []byte) {
	t.Helper()
	if n, err := unix.Write(fd, data); err != nil || n != len(data) {
		t.Fatalf("wrote %d of %d: %v", n, len(data), err)
	}
}

// A group is still heard while the kernel lists a mark of it, and is deaf once it lists none: a descriptor that holds
// no mark is deaf. One whose list cannot be read is taken as heard, as nothing then says that its marks went
// (specs.md §15.1).
func TestAGroupWithNoMarkIsDeaf(t *testing.T) {
	p := newPipe(t)
	defer closePipe(p)
	if marked(p[0]) {
		t.Error("a descriptor that holds no mark is said to hold one")
	}
	if !marked(-1) {
		t.Error("a group whose list cannot be read is said to have lost its marks")
	}
	root := t.TempDir()
	dir, err := fsx.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := dir.Info()
	_ = dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	fs := &filesystemSource{group: p[0], root: root}
	if fs.fd() != p[0] || fs.alive() {
		t.Errorf("a filesystem mark that went: descriptor %d, alive %v", fs.fd(), fs.alive())
	}
	folders := &foldersSource{group: p[0], root: root, rootDev: info.Dev, rootIno: info.Ino}
	if folders.fd() != p[0] || folders.alive() {
		t.Errorf("marks by folder that went: descriptor %d, alive %v", folders.fd(), folders.alive())
	}
}

// A read of a group gives its records, and closes the descriptor each carries, which would otherwise be lost to the
// process; what cannot be read is an overflow, which looks at the disk whole; nothing waiting is EAGAIN, and a group
// that cannot be read any more is an error, which ends its watch (specs.md §15.1).
func TestReadingAGroup(t *testing.T) {
	p := newPipe(t)
	defer func() { _ = unix.Close(p[0]) }()
	buf := make([]byte, 4096)
	if _, err := readFanotify(p[0], buf); !errors.Is(err, unix.EAGAIN) {
		t.Errorf("nothing waits: %v", err)
	}

	carried := newPipe(t)
	event := fanEvent(fanCloseWrite, 42, fanHandle(infoDFIDName, "root", "a"))
	binary.NativeEndian.PutUint32(event[16:], uint32(carried[0]))
	writeAll(t, p[1], event)
	records, err := readFanotify(p[0], buf)
	if err != nil || len(records) != 1 || records[0].pid != 42 || records[0].mask != fanCloseWrite || len(records[0].infos) != 1 || records[0].infos[0].name != "a" {
		t.Fatalf("%+v, %v", records, err)
	}
	if _, err := unix.Write(carried[1], []byte{0}); !errors.Is(err, unix.EPIPE) {
		t.Errorf("the descriptor the event carried is still open: %v", err)
	}
	_ = unix.Close(carried[1])

	good := fanEvent(fanCreate, 7, fanHandle(infoDFIDName, "root", "b"))
	writeAll(t, p[1], append(good, good[:len(good)-1]...))
	records, err = readFanotify(p[0], buf)
	if err != nil || len(records) != 2 || records[0].pid != 7 || records[1].mask != fanQOverflow {
		t.Errorf("a record cut short: %+v, %v", records, err)
	}

	if _, err := readFanotify(-1, buf); err == nil || errors.Is(err, unix.EAGAIN) {
		t.Errorf("a group that cannot be read: %v", err)
	}
	_ = unix.Close(p[1])
	if _, err := readFanotify(p[0], buf); !errors.Is(err, unix.EAGAIN) {
		t.Errorf("a group whose other end went: %v", err)
	}
}

// A filesystem mark finds each name through its folder's handle. A handle that leads nowhere any more, or a root that
// cannot be opened, is a folder that cannot be told: the whole disk is looked at, never nothing. A file that no longer
// exists says nothing, its folder's event saying it went (specs.md §15.1).
func TestAFilesystemMarkGivesWhatWaits(t *testing.T) {
	p := newPipe(t)
	defer func() { _ = unix.Close(p[1]) }()
	s := &filesystemSource{group: p[0], root: t.TempDir()}
	defer func() { _ = s.close() }()
	var batch []byte
	for _, e := range [][]byte{
		fanEvent(fanCreate, 42, fanHandle(infoDFIDName, "gone", "a")),
		fanEvent(fanAttrib, 43, fanHandle(infoFID, "gone", "")),
		fanEvent(fanQOverflow, 0),
	} {
		batch = append(batch, e...)
	}
	writeAll(t, p[1], batch)
	buf := make([]byte, 4096)
	out, pids, err := s.drain(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []change{{kind: KindCreated}, {kind: KindOverflow}}
	if !slices.Equal(out, want) || !slices.Equal(pids, []int32{42, 0}) {
		t.Errorf("got %+v from %v, want %+v", out, pids, want)
	}
	if path, where := s.resolve(&fanInfo{kind: infoDFIDName, handleType: 1, handle: []byte("gone"), name: "a"}); path != "" || where != unknown {
		t.Errorf("a handle that leads nowhere: %q, %v", path, where)
	}
	gone := &filesystemSource{group: -1, root: filepath.Join(s.root, "nowhere")}
	if path, where := gone.resolve(&fanInfo{kind: infoDFIDName, handleType: 1, handle: []byte("x"), name: "a"}); path != "" || where != unknown {
		t.Errorf("a root that cannot be opened: %q, %v", path, where)
	}
	if out, _, err := gone.drain(buf); err == nil || len(out) != 0 {
		t.Errorf("a group that cannot be read: %+v, %v", out, err)
	}
}

// startMergerfs starts a process whose executable is named mergerfs, with args on its command line, as the view's
// mergerfs is told by those: a copy of a shell under that name, that says it runs, then waits for its input to end.
func startMergerfs(t *testing.T, args ...string) int {
	t.Helper()
	return startNamed(t, shellNamed(t, "mergerfs", false), nil, "run", args...)
}

// shellNamed gives the path of a shell under another name, in a folder every account may enter and run from: a copy
// of it, whose executable /proc names by that name, or a symbolic link to it, run by which the process takes that name
// alone, as its comm, its executable staying the shell's.
func shellNamed(t *testing.T, name string, link bool) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to stand for another program")
	}
	dir, err := os.MkdirTemp("", "atoll-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := filepath.Join(dir, name)
	if link {
		if err := os.Symlink(sh, at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	data, err := os.ReadFile(sh)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return at
}

// startNamed starts the shell at program on the script named script, with args after it, as an account when as is set,
// so that its command line reads program, script, args: the script, in a folder of its own, says it runs, then waits
// for its input to end. Start returns as soon as the child let go of the parent's memory, before the kernel has laid
// out its arguments: its name is already the new one there and its command line still empty, so the shell's word is
// waited for.
func startNamed(t *testing.T, program string, as *syscall.Credential, script string, args ...string) int {
	t.Helper()
	dir, err := os.MkdirTemp("", "atoll-script-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, script), []byte("echo\nread x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), program, append([]string{script}, args...)...)
	cmd.Dir = dir
	if as != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: as}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	if _, err := stdout.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the shell did not start: %v", err)
	}
	pid := cmd.Process.Pid
	if comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err != nil || strings.TrimSpace(string(comm)) != filepath.Base(program) {
		t.Fatalf("the process stands as %q: %v", comm, err)
	}
	return pid
}

// The view's and Atoll's own events are told by the account the process runs as, and by the executable /proc names,
// never by the name a process gives itself, where /proc lets the watcher read it (specs.md §15.1;
// docs/threat-model.md, G17). A shell named mergerfs by a link, whose comm says mergerfs and whose command line names
// the view, is a user's; so is a mergerfs, or a process of Atoll's own executable, run by an account none of Atoll's; a
// process gone by the time /proc is read is a user's, the reading that loses nothing, as a user's change wakes a pass.
// The accounts are root and the one Atoll runs as, or those the watcher is given. A process of one of them whose
// executable /proc does not let the watcher read, as root's mergerfs to a daemon of the account atoll, is told by its
// comm: in a container, root cannot read the executable of 65534's processes either.
func TestOnlyTheViewAndAtollOfTheirAccountsMakeTheirEvents(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("starts processes as another account: root only")
	}
	view := "/srv/pool"
	nobody := &syscall.Credential{Uid: 65534, Gid: 65534}
	mergerfs := shellNamed(t, "mergerfs", false)
	real := startNamed(t, mergerfs, nil, "run", "/mnt/a:/mnt/b", view)
	named := startNamed(t, shellNamed(t, "mergerfs", true), nil, "run", "/mnt/a:/mnt/b", view)
	other := startNamed(t, mergerfs, nobody, "run", "/mnt/a:/mnt/b", view)
	atoll := shellNamed(t, "atoll", false)
	own := startNamed(t, atoll, nil, "scan")
	ownMount := startNamed(t, atoll, nil, "mount")
	foreign := startNamed(t, atoll, nobody, "scan")
	foreignMount := startNamed(t, atoll, nobody, "mount")
	ended := exec.CommandContext(t.Context(), "true")
	if err := ended.Run(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		accounts []uint32
		pid      int
		want     string
	}{
		{"mergerfs of root, serving the view", nil, real, OriginView},
		{"a shell that calls itself mergerfs", nil, named, OriginUser},
		{"mergerfs of another account", nil, other, OriginUser},
		{"mergerfs of an account the watcher is given", []uint32{0, 65534}, other, OriginView},
		{"Atoll's executable, of root", nil, own, OriginAtoll},
		{"Atoll's executable, of root, atoll mount", nil, ownMount, OriginView},
		{"Atoll's executable, of another account", nil, foreign, OriginUser},
		{"Atoll's executable, of another account, atoll mount", nil, foreignMount, OriginUser},
		{"Atoll's executable, of root, when the watcher is given root alone", []uint32{0}, own, OriginAtoll},
		{"Atoll's executable, of root, when the watcher is given another", []uint32{65534}, own, OriginUser},
		{"mergerfs of root, when the watcher is given another", []uint32{65534}, real, OriginUser},
		{"a process gone", nil, ended.Process.Pid, OriginUser},
	} {
		w := NewWatcher(WatcherOptions{ViewMount: view, Now: func() int64 { return 1 }, Accounts: tc.accounts})
		w.exe = atoll
		if got := w.origin(tc.pid); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		w.Close()
	}
}

// Each event says who made it, from the process the kernel names: none, Atoll itself, the view's mergerfs, told by
// the view's mount point among its arguments, or a user (specs.md §15.1).
func TestEventsSayWhoMadeThem(t *testing.T) {
	view := "/srv/pool"
	mergerfs := startMergerfs(t, "-o", "category.create=epmfs", "/mnt/a:/mnt/b", view+"/")
	if !servesView(mergerfs, view) || servesView(mergerfs, "/srv/other") || servesView(os.Getpid(), view) {
		t.Error("the view's process is not told by its mount point")
	}
	p := newPipe(t)
	defer func() { _ = unix.Close(p[1]) }()
	w := NewWatcher(WatcherOptions{ViewMount: view, Now: func() int64 { return 5 }})
	defer w.Close()
	if err := w.arm("t1", &filesystemSource{group: p[0], root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	var batch []byte
	for _, pid := range []int32{0, int32(os.Getpid()), int32(mergerfs)} {
		batch = append(batch, fanEvent(fanCreate, pid, fanHandle(infoDFIDName, "gone", "a"))...)
	}
	writeAll(t, p[1], batch)
	var got []string
	for len(got) < 3 {
		select {
		case e := <-w.Events():
			if e.Disk != "t1" || e.Kind != KindCreated || e.Path != "" || e.AtNS != 5 {
				t.Errorf("%+v", e)
			}
			got = append(got, e.Origin)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %q came", got)
		}
	}
	if want := []string{OriginUnknown, OriginAtoll, OriginView}; !slices.Equal(got, want) {
		t.Errorf("origins %q, want %q", got, want)
	}
	w.Stop("t1")
	if w.Alive("t1") {
		t.Error("a watch stopped is alive")
	}
}

// What each process was is kept for a few processes only: past 1024, it is forgotten, so that a burst of short
// processes does not grow the daemon for good.
func TestWhatProcessesWereIsBounded(t *testing.T) {
	w := NewWatcher(WatcherOptions{Now: func() int64 { return 1 }})
	defer w.Close()
	for i := range 1025 {
		w.origins[1<<22+i] = originSeen{origin: OriginAtoll, atNS: 1}
	}
	if o := w.origin(1); o != OriginUser {
		t.Errorf("a process not of Atoll nor of the view: %q", o)
	}
	if len(w.origins) != 1 {
		t.Errorf("%d processes known", len(w.origins))
	}
}

// A folder that arrives is opened from the root one name at a time: never through a link, and never on another mount
// than the root's, which would be a folder anywhere on the machine (specs.md §15.1).
func TestOpenBelowStaysOnTheRootsMount(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "d", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "d"), filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	top, err := fsx.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := top.Info()
	_ = top.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := openBelow(root, "d/e", info.MountID)
	if err != nil {
		t.Fatal(err)
	}
	_ = dir.Close()
	for what, open := range map[string]func() (fsx.Dir, error){
		"through a link":   func() (fsx.Dir, error) { return openBelow(root, "l/e", info.MountID) },
		"on another mount": func() (fsx.Dir, error) { return openBelow(root, "d/e", info.MountID+1) },
		"below no root":    func() (fsx.Dir, error) { return openBelow(filepath.Join(root, "none"), "d", info.MountID) },
	} {
		if dir, err := open(); err == nil {
			_ = dir.Close()
			t.Errorf("%s: opened", what)
		}
	}
}

// listingFault makes the listing of the folders a marker holds fail to open some names, as when a folder goes between
// its listing and its open.
func listingFault(root fsx.Dir, gone ...string) fsx.Dir {
	return fsx.Fault(root, func(call, name string) error {
		if call == "opendir" && slices.Contains(gone, name) {
			return unix.ENOENT
		}
		return nil
	})
}

// A marker marks every folder below one: a folder gone between its listing and its open is not marked, and the others
// are; a mark that cannot be put fails the whole watch, which is then had another way; and one canceled stops
// (specs.md §15.1).
func TestAMarkerMarksWhatIsThere(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/x", "b/y", "c"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	open := func() (fsx.Dir, uint64) {
		t.Helper()
		dir, err := fsx.OpenDir(root)
		if err != nil {
			t.Fatal(err)
		}
		info, err := dir.Info()
		if err != nil {
			t.Fatal(err)
		}
		return dir, info.MountID
	}
	paths := func(tree *folderTree[int]) []string {
		var out []string
		for _, n := range tree.nodes {
			if p, ok := n.path(); ok && p != "" {
				out = append(out, p)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, maxOpen := range []int{0, 2} {
		dir, mountID := open()
		next := 0
		m := &treeMarker[int]{root: root, mountID: mountID, limits: Limits{MaxPath: NoBound, MaxOpen: maxOpen}, tree: newFolderTree(0),
			mark: func(fsx.Dir) (int, error) { next++; return next, nil }}
		var found []change
		err := m.all(t.Context(), listingFault(dir, "b"), m.tree.root, "", &found)
		_ = dir.Close()
		if err != nil {
			t.Fatalf("max_open %d: %v", maxOpen, err)
		}
		want := []string{"a", "a/x", "b", "b/y", "c"}
		if maxOpen == 0 {
			// b went between its listing and its open: what was below it is not marked, and is not listed either.
			want = []string{"a", "a/x", "c"}
		}
		if got := paths(m.tree); !slices.Equal(got, want) {
			t.Errorf("max_open %d: marked %q, want %q", maxOpen, got, want)
		}
		if len(found) < 3 {
			t.Errorf("max_open %d: listed %+v", maxOpen, found)
		}
	}

	// The folders left for later that went before their turn are skipped; the one marked first, and what is below it,
	// are marked.
	dir, mountID := open()
	calls, survivor := 0, ""
	m := &treeMarker[int]{root: root, mountID: mountID, limits: Limits{MaxPath: NoBound, MaxOpen: 2}, tree: newFolderTree(0),
		mark: func(d fsx.Dir) (int, error) {
			calls++
			if calls == 1 {
				info, err := d.Info()
				if err != nil {
					return 0, err
				}
				for _, name := range []string{"a", "b", "c"} {
					st, err := fsx.StatPath(filepath.Join(root, name))
					if err == nil && st.Ino == info.Ino {
						survivor = name
						continue
					}
					_ = os.RemoveAll(filepath.Join(root, name))
				}
			}
			return calls, nil
		}}
	if err := m.all(t.Context(), dir, m.tree.root, "", nil); err != nil {
		t.Errorf("folders gone before their turn: %v", err)
	}
	_ = dir.Close()
	got := paths(m.tree)
	if survivor == "" || len(got) == 0 || slices.ContainsFunc(got, func(p string) bool { return p != survivor && !strings.HasPrefix(p, survivor+"/") }) {
		t.Errorf("marked %q, want %q and what is below it", got, survivor)
	}

	for _, d := range []string{"a/x", "b"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	refused := errors.New("no mark")
	for _, maxOpen := range []int{0, 2} {
		dir, mountID := open()
		m := &treeMarker[int]{root: root, mountID: mountID, limits: Limits{MaxPath: NoBound, MaxOpen: maxOpen}, tree: newFolderTree(0),
			mark: func(fsx.Dir) (int, error) { return 0, refused }}
		err := m.all(t.Context(), dir, m.tree.root, "", nil)
		_ = dir.Close()
		if !errors.Is(err, refused) || len(m.later) != 0 {
			t.Errorf("max_open %d: a mark refused gave %v, %d left for later", maxOpen, err, len(m.later))
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir, mountID = open()
	m = &treeMarker[int]{root: root, mountID: mountID, limits: Unbounded, tree: newFolderTree(0), mark: func(fsx.Dir) (int, error) { return 1, nil }}
	err := m.all(ctx, dir, m.tree.root, "", nil)
	_ = dir.Close()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	if _, err := openInotify(ctx, root, Unbounded); !errors.Is(err, context.Canceled) {
		t.Errorf("an inotify watch canceled while it marks: %v", err)
	}
}

// readStep is one read of an inotify instance: what it gives, or its error.
type readStep struct {
	data []byte
	err  error
}

// script is what the reads of a scripted source give, in turn, and then EAGAIN.
type script struct{ steps []readStep }

// scriptedInotify is an inotify source of a real root, whose reads come from its script. Its instance is a real one,
// so that a folder that arrives is watched.
func scriptedInotify(t *testing.T, root string) (*inotifySource, *script) {
	t.Helper()
	s, err := openInotify(t.Context(), root, Unbounded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	sc := &script{}
	s.read = func(buf []byte) (int, error) {
		if len(sc.steps) == 0 {
			return -1, unix.EAGAIN
		}
		step := sc.steps[0]
		sc.steps = sc.steps[1:]
		if step.err != nil {
			return -1, step.err
		}
		return copy(buf, step.data), nil
	}
	s.wait = func(time.Duration) bool { return len(sc.steps) > 0 }
	return s, sc
}

func events(list ...[]byte) []byte {
	var out []byte
	for _, e := range list {
		out = append(out, e...)
	}
	return out
}

func nodeAt[K comparable](tree *folderTree[K], path string) *folderNode[K] {
	for _, n := range tree.nodes {
		if p, ok := n.path(); ok && p == path {
			return n
		}
	}
	return nil
}

// What one read of inotify says: an interrupted read is read again; an overflow, and what cannot be parsed, look at the
// whole disk; a read that fails ends the watch with what it read; the watch of a folder that went goes with it, and the
// root's ends the watch; a watch no longer known says nothing (specs.md §15.1).
func TestWhatAnInotifyReadSays(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, sc := scriptedInotify(t, root)
	a, b, c := nodeAt(s.tree, "a"), nodeAt(s.tree, "b"), nodeAt(s.tree, "c")
	if a == nil || b == nil || c == nil {
		t.Fatal("the folders are not watched")
	}
	rootWD := s.tree.root.key
	buf := make([]byte, 4096)
	batch := events(
		inotifyEvent(a.key, unix.IN_CLOSE_WRITE, 0, "f"),
		inotifyEvent(999, unix.IN_CREATE, 0, "lost"),
		inotifyEvent(999, unix.IN_IGNORED, 0, ""),
		inotifyEvent(b.key, unix.IN_IGNORED, 0, ""),
		inotifyEvent(rootWD, unix.IN_DELETE|unix.IN_ISDIR, 0, "c"),
		inotifyEvent(0, unix.IN_Q_OVERFLOW, 0, ""),
	)
	sc.steps = []readStep{{err: unix.EINTR}, {data: batch}, {data: inotifyEvent(rootWD, unix.IN_CREATE, 0, "g")[:20]}}
	out, pids, err := s.drain(buf)
	if err != nil || pids != nil {
		t.Fatalf("%+v, %v, %v", out, pids, err)
	}
	want := []change{{path: "a/f", kind: KindWritten}, {path: "c", folder: true, kind: KindDeleted}, {kind: KindOverflow}, {kind: KindOverflow}}
	if !slices.Equal(out, want) {
		t.Errorf("got %+v\nwant %+v", out, want)
	}
	if s.tree.nodes[b.key] != nil || s.tree.nodes[c.key] != nil || s.tree.nodes[a.key] != a {
		t.Error("the tree does not follow the folders that went")
	}

	sc.steps = []readStep{{data: inotifyEvent(rootWD, unix.IN_CREATE, 0, "h")}, {err: unix.EIO}}
	out, _, err = s.drain(buf)
	if !errors.Is(err, unix.EIO) || !slices.Equal(out, []change{{path: "h", kind: KindCreated}}) {
		t.Errorf("a read that fails: %+v, %v", out, err)
	}

	sc.steps = []readStep{{data: inotifyEvent(rootWD, unix.IN_IGNORED, 0, "")}}
	if _, _, err := s.drain(buf); !errors.Is(err, errRootGone) {
		t.Errorf("the root's watch went: %v", err)
	}
}

// A folder that comes in from outside the root, with no other end of its rename, is watched with what it holds, which
// is listed as created; a folder that left and is not back gives nothing of what happens in it (specs.md §15.1).
func TestAFolderMovedInIsWatched(t *testing.T) {
	top := t.TempDir()
	root := filepath.Join(top, "media")
	if err := os.MkdirAll(filepath.Join(root, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(top, "Film"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "Film", "f.mkv"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, sc := scriptedInotify(t, root)
	rootWD := s.tree.root.key
	show := nodeAt(s.tree, "Show")
	if show == nil {
		t.Fatal("Show is not watched")
	}
	if err := os.Rename(filepath.Join(top, "Film"), filepath.Join(root, "Film")); err != nil {
		t.Fatal(err)
	}
	sc.steps = []readStep{{data: events(
		inotifyEvent(rootWD, unix.IN_MOVED_FROM|unix.IN_ISDIR, 7, "Show"),
		inotifyEvent(show.key, unix.IN_CREATE, 0, "x"),
		inotifyEvent(rootWD, unix.IN_MOVED_TO|unix.IN_ISDIR, 8, "Film"),
	)}}
	var opened []string
	s.opened = func(path string) { opened = append(opened, path) }
	out, _, err := s.drain(make([]byte, 4096))
	if err != nil {
		t.Fatal(err)
	}
	want := []change{{path: "Show", folder: true, kind: KindMovedFrom}, {path: "Film", folder: true, kind: KindMovedTo}, {path: "Film/f.mkv", kind: KindCreated}}
	if !slices.Equal(out, want) {
		t.Errorf("got %+v\nwant %+v", out, want)
	}
	if !slices.Equal(opened, []string{"Film"}) {
		t.Errorf("opened %q", opened)
	}
	if nodeAt(s.tree, "Show") != nil || len(s.moved) != 0 {
		t.Error("a folder that left is still watched")
	}
	if detached := s.tree.detach(s.tree.root, "Film"); detached == nil || s.arrive(detached, "y") != nil {
		t.Error("a folder below one that left is watched")
	}
}

// A source that cannot put a watch watches nothing, and says so: through a folder that is no folder of a disk, or an
// instance that is gone. A folder that cannot be watched when it arrives gives nothing.
func TestAWatchThatCannotBePut(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := scriptedInotify(t, root)
	if _, err := s.watch(fakeDir{}); err == nil {
		t.Error("a watch through a folder of no disk")
	}
	ino := s.ino
	s.ino = -1
	defer func() { s.ino = ino }()
	dir, err := fsx.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if _, err := s.watch(dir); err == nil {
		t.Error("a watch through an instance that is gone")
	}
	before := len(s.tree.nodes)
	if found := s.arrive(s.tree.root, "d"); found != nil || len(s.tree.nodes) != before {
		t.Errorf("a folder that cannot be watched: %+v, %d watched for %d", found, len(s.tree.nodes), before)
	}
}

// fakeDir is a folder of no disk.
type fakeDir struct{ fsx.Dir }

// An instance is readable once the kernel queued an event in it, and not before; a read that waits for the other end
// of a rename asks the instance itself, and forgets the folder that left when nothing comes.
func TestAnInstanceIsReadableOnceAnEventWaits(t *testing.T) {
	root := t.TempDir()
	s, err := openInotify(t.Context(), root, Unbounded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close() }()
	if readable(s.ino, 0) {
		t.Error("readable with nothing queued")
	}
	gone := s.tree.add(s.tree.root, "gone", 1<<30)
	s.tree.detach(s.tree.root, "gone")
	s.moved[3] = gone
	if out, _, err := s.drain(make([]byte, 4096)); err != nil || len(out) != 0 || len(s.moved) != 0 || s.tree.nodes[gone.key] != nil {
		t.Errorf("a folder that left and did not come back: %+v, %v, %d moves", out, err, len(s.moved))
	}
	if err := os.WriteFile(filepath.Join(root, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !readable(s.ino, 5*time.Second) {
		t.Error("not readable with an event queued")
	}
}
