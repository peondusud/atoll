package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/config"
)

// The watcher by inotify, which needs no privilege: what changes below the root comes, with no process, a
// folder made is watched below, and a disk stopped, then all, stop sending (specs.md §15.1). The race detector
// runs it in CI; the fanotify ways need root, and test/loopdev runs them in qemu.
func TestWatcherByInotify(t *testing.T) {
	top := t.TempDir()
	for _, dir := range []string{"media", "Atoll"} {
		if err := os.Mkdir(filepath.Join(top, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := NewWatcher(WatcherOptions{Now: func() int64 { return time.Now().UnixNano() }, Inotify: true})
	defer w.Close()
	d := &config.Disk{ID: "t1", Mount: top, Root: "media"}
	how, err := w.Watch(t.Context(), d)
	if err != nil || how != HowInotify {
		t.Fatalf("%s, %v", how, err)
	}
	next := func(path, kind string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e := <-w.Events():
				if e.Disk != "t1" || e.Origin != OriginUnknown || e.Path == "x" {
					t.Errorf("%+v", e)
				}
				if e.Path == path && e.Kind == kind {
					return
				}
			case <-deadline:
				t.Fatalf("no %s at %q", kind, path)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(top, "Atoll", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "media", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	next("d", KindCreated)
	if err := os.WriteFile(filepath.Join(top, "media", "d", "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	next("d/a", KindWritten)
	if err := os.Rename(filepath.Join(top, "media", "d"), filepath.Join(top, "media", "e")); err != nil {
		t.Fatal(err)
	}
	next("e", KindMovedTo)
	if err := os.Remove(filepath.Join(top, "media", "e", "a")); err != nil {
		t.Fatal(err)
	}
	next("e/a", KindDeleted)

	// Stopped, the disk sends nothing more, and watching it again works.
	w.Stop("t1")
	if err := os.WriteFile(filepath.Join(top, "media", "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-w.Events():
		t.Errorf("after Stop: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := w.Watch(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "media", "g"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	next("g", KindWritten)
	w.Close()
	if _, err := w.Watch(t.Context(), d); err == nil {
		t.Error("a closed watcher watches")
	}
}

// A root that is not there cannot be watched.
func TestWatcherWithoutRoot(t *testing.T) {
	w := NewWatcher(WatcherOptions{Now: func() int64 { return 0 }, Inotify: true})
	defer w.Close()
	if _, err := w.Watch(t.Context(), &config.Disk{ID: "t1", Mount: t.TempDir(), Root: "media"}); err == nil {
		t.Error("watched a root that is not there")
	}
}

// brokenSource is a source whose reading fails as soon as it is asked: what a group the kernel closed, or a read
// that answers an error, leaves.
type brokenSource struct {
	pipe   [2]int
	closed chan struct{}
}

func newBrokenSource(t *testing.T) *brokenSource {
	t.Helper()
	s := &brokenSource{closed: make(chan struct{})}
	if err := unix.Pipe2(s.pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(s.pipe[1], []byte{0}); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *brokenSource) fd() int { return s.pipe[0] }

func (s *brokenSource) drain([]byte) ([]change, []int32, error) {
	return nil, nil, unix.EBADF
}

func (s *brokenSource) alive() bool { return true }

func (s *brokenSource) close() error {
	close(s.closed)
	closePipe(s.pipe)
	return nil
}

// A watch whose source fails ends, with an overflow for what it may have missed, and is alive no more: the daemon
// asks, and watches the disk again (specs.md §15.1). Nothing said that a watch had ended, and the disk then sent no
// event until the daemon started again.
func TestAWatchThatEndedIsNotAlive(t *testing.T) {
	w := NewWatcher(WatcherOptions{Now: func() int64 { return 7 }})
	defer w.Close()
	if w.Alive("t1") {
		t.Error("a disk never watched is alive")
	}
	src := newBrokenSource(t)
	if err := w.arm("t1", src); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-w.Events():
		if e != (Event{Disk: "t1", Kind: KindOverflow, Origin: OriginUnknown, AtNS: 7}) {
			t.Errorf("%+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no overflow from a watch that ended")
	}
	select {
	case <-src.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the source of a watch that ended stays open")
	}
	// The watch is over once its reader has returned, a moment after it closed its source: whoever asks in between
	// is still told alive, and asks again, as the daemon does at each of its checks.
	for deadline := time.Now().Add(5 * time.Second); w.Alive("t1") && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if w.Alive("t1") {
		t.Error("a watch that ended is alive")
	}
	// Stopping it after is nothing, and the disk can be watched again.
	w.Stop("t1")
	if w.Alive("t1") {
		t.Error("a watch stopped is alive")
	}
}

// The root's own watch going is the end of an inotify watch: its folder was removed, or its filesystem unmounted,
// and what is made at that path after is another folder, which nothing watches (specs.md §15.1). Watched again, the
// disk is heard again.
func TestAnInotifyWatchEndsWithItsRoot(t *testing.T) {
	top := t.TempDir()
	root := filepath.Join(top, "media")
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(WatcherOptions{Now: func() int64 { return time.Now().UnixNano() }, Inotify: true})
	defer w.Close()
	d := &config.Disk{ID: "t1", Mount: top, Root: "media"}
	if _, err := w.Watch(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if !w.Alive("t1") {
		t.Fatal("a disk just watched is not alive")
	}
	// A folder below the root going is the watch's daily life.
	if err := os.Remove(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	wait := func(kind string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e := <-w.Events():
				if e.Kind == kind {
					return
				}
			case <-deadline:
				t.Fatalf("no %s", kind)
			}
		}
	}
	wait(KindDeleted)
	if !w.Alive("t1") {
		t.Fatal("the watch ended with a folder below its root")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	wait(KindOverflow)
	for deadline := time.Now().Add(5 * time.Second); w.Alive("t1"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the watch of a root that went is still alive")
		}
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Watch(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if !w.Alive("t1") {
		t.Error("watched again, the disk is not alive")
	}
	if err := os.WriteFile(filepath.Join(root, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wait(KindWritten)
}

// A fanotify group whose filesystem was unmounted holds no mark any more, and says nothing of it: what the kernel
// lists of the group tells (specs.md §15.1). The first lines are the descriptor's and the group's own.
func TestHasMarks(t *testing.T) {
	const head = "pos:\t0\nflags:\t04002\nmnt_id:\t16\nino:\t2075\nfanotify flags:1c00 event-flags:8002\n"
	for text, want := range map[string]bool{
		"":   false,
		head: false,
		head + "fanotify sdev:800011 mflags:0 mask:100003ce ignored_mask:0\n":                                              true,
		head + "fanotify ino:1b sdev:800011 mflags:0 mask:3ce ignored_mask:0 fhandle-bytes:8 fhandle-type:1 f_handle:1b\n": true,
		head + "fanotify mnt_id:1a mflags:0 mask:3ce ignored_mask:0\n":                                                     true,
		// A line cut short by the size of the read still names a mark.
		head + "fanotify sdev:8000": true,
		// Another kind of descriptor lists no mark of fanotify.
		"pos:\t0\nflags:\t02004000\nmnt_id:\t16\nino:\t2075\ninotify wd:1 ino:1b sdev:800011 mask:3ce ignored_mask:0\n": false,
		"pos:\t0\nflags:\t0100002\nmnt_id:\t30\nino:\t5\n":                                                              false,
	} {
		if got := hasMarks([]byte(text)); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

// A folder that appears is opened some time after its event, to be watched and listed. Opened by its path, with a
// folder above it traded for a symbolic link meanwhile, it was a folder anywhere on the machine: watched, and what it
// holds told of as new below the root, which a media server is then asked to scan. It is opened from the root, one
// name at a time, following no link (specs.md §15.1).
func TestAnArrivingFolderIsOpenedBelowTheRoot(t *testing.T) {
	top := t.TempDir()
	root, outside := filepath.Join(top, "media"), filepath.Join(top, "elsewhere")
	for _, dir := range []string{filepath.Join(root, "d"), filepath.Join(outside, "new")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "new", "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := openInotify(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close() }()
	var d *folderNode[int32]
	for _, n := range s.tree.nodes {
		if p, ok := n.path(); ok && p == "d" {
			d = n
		}
	}
	if d == nil {
		t.Fatal("the folder d is not watched")
	}
	watched := len(s.tree.nodes)
	// d is traded for a link that leads elsewhere, between the event of a folder made in it and the look at it.
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "d2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if found := s.arrive(d, "new"); len(found) != 0 || len(s.tree.nodes) != watched {
		t.Errorf("a folder a link leads to was taken: %+v, %d folders watched for %d", found, len(s.tree.nodes), watched)
	}
	// The folder itself, once it is there, is taken, with what it holds.
	if err := os.Remove(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "d2"), filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "d", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "new", "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	found := s.arrive(d, "new")
	if len(found) != 1 || found[0] != (change{path: "d/new/a", kind: KindCreated}) || len(s.tree.nodes) != watched+1 {
		t.Errorf("the folder made there: %+v, %d folders watched for %d", found, len(s.tree.nodes), watched+1)
	}
}
