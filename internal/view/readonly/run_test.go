package readonly

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// awayMachine is machine, with disks a test takes away and gives back: a disk away is a mount point with nothing
// mounted. Each disk's mount has an id, which a test moves on to stand for the disk mounted again; a device a test
// fails answers its mount check from the page cache, and fails a read past it; fail makes calls through a handle on a
// disk's top fail, as fsx.Fault does; its clock moves on at each question.
type awayMachine struct {
	machine
	mu      sync.Mutex
	away    map[string]bool
	mountID map[string]uint64
	failing map[string]bool
	gone    map[string]bool
	fail    map[string]func(call, name string) error
	now     int64
}

func (m *awayMachine) remount(mount string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mountID == nil {
		m.mountID = map[string]uint64{}
	}
	m.mountID[mount]++
}

func (m *awayMachine) setFailing(mount string, failing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	m.failing[mount] = failing
}

func (m *awayMachine) setGone(mount string, gone bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gone == nil {
		m.gone = map[string]bool{}
	}
	m.gone[mount] = gone
}

// Device says that the device of a disk a test took away is gone, its mount left as it was.
func (m *awayMachine) Device(fs names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	m.mu.Lock()
	gone := m.gone[e.MountPoint]
	m.mu.Unlock()
	if gone {
		return disks.DeviceSeen{Device: disks.DeviceGoneAway, Note: "no such device"}
	}
	return m.machine.Device(fs, e)
}

func (m *awayMachine) setFail(mount string, fail func(call, name string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail == nil {
		m.fail = map[string]func(call, name string) error{}
	}
	m.fail[mount] = fail
}

func (m *awayMachine) Now() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now += 1_000_000_000
	return m.now
}

func (m *awayMachine) ReadPastCache(fs names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	m.mu.Lock()
	failing := m.failing[e.MountPoint]
	m.mu.Unlock()
	if failing {
		return disks.DeviceSeen{Device: disks.DeviceUnreadable, Note: "input/output error"}
	}
	return m.machine.ReadPastCache(fs, e)
}

func (m *awayMachine) OpenTop(mount string) (fsx.Dir, error) {
	top, err := m.machine.OpenTop(mount)
	if err != nil {
		return nil, err
	}
	return fsx.Fault(top, func(call, name string) error {
		m.mu.Lock()
		fail := m.fail[mount]
		m.mu.Unlock()
		if fail == nil {
			return nil
		}
		return fail(call, name)
	}), nil
}

func (m *awayMachine) setAway(mount string, away bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.away[mount] = away
}

func (m *awayMachine) Mount(mount string) disks.MountSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.away[mount] {
		return disks.MountSeen{Path: disks.PathOK}
	}
	seen := m.machine.Mount(mount)
	seen.Entry.ID = 100 + m.mountID[mount]
	return seen
}

// fakeServed is a view served by no kernel: Done once the test ends it, Unmount answering as the test says.
type fakeServed struct {
	done      chan struct{}
	unmounted bool
	fail      error
}

func (f *fakeServed) Done() <-chan struct{} { return f.done }
func (f *fakeServed) Unmount() error {
	f.unmounted = true
	return f.fail
}

// runBench is a pool of two disks, t1 and t2, each holding Films/a.mkv, its catalog, and what Run is given.
type runBench struct {
	t       *testing.T
	top     string
	config  *config.Config
	machine *awayMachine
	db      interface{ Close() error }
	o       Options
	logs    *bytes.Buffer
	logMu   *sync.Mutex
	// served is what the view's mount gave, tree and writer what it was given.
	served *fakeServed
	tree   *view.Tree
	w      *writer.Writer
	mounts chan struct{}
}

// lockedBuffer is a log's writer that the view's loop and the test read at once.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (l lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p) //nolint:wrapcheck // a test double
}

func newRunBench(t *testing.T) *runBench {
	t.Helper()
	top := t.TempDir()
	b := &runBench{t: t, top: top, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}, served: &fakeServed{done: make(chan struct{})}, mounts: make(chan struct{}, 1)}
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", filepath.Join(top, "catalog.db"), filepath.Join(top, "pool"))
	b.machine = &awayMachine{machine: machine{uuid: map[string]string{}}, away: map[string]bool{}}
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(mount, "media", "Films"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mount, "media", "Films", "a.mkv"), []byte("from "+id), 0o644); err != nil {
			t.Fatal(err)
		}
		uuid := fmt.Sprintf("3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d", i)
		b.machine.uuid[mount] = uuid
		text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = \"ext4\"\n", id, uuid, mount)
	}
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	b.config = c
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
	b.db = db
	point := filepath.Join(top, "view")
	if err := os.Mkdir(point, 0o755); err != nil {
		t.Fatal(err)
	}
	b.o = Options{Config: c, DB: db, Machine: b.machine, Point: point, Every: 10 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(lockedBuffer{mu: b.logMu, buf: b.logs}, nil)),
		mount: func(tree *view.Tree, _ string, w *writer.Writer) (served, error) {
			b.tree, b.w = tree, w
			b.mounts <- struct{}{}
			return b.served, nil
		}}
	return b
}

func (b *runBench) said() string {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	return b.logs.String()
}

// waitSaid waits until the log holds the text.
func (b *runBench) waitSaid(text string) {
	b.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(b.said(), text) {
		if time.Now().After(deadline) {
			b.t.Fatalf("the log never said %q:\n%s", text, b.said())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs the view in a goroutine of its own, until the context the test ends; it gives what Run ends with.
func (b *runBench) start(ctx context.Context) <-chan error {
	ended := make(chan error, 1)
	go func() { ended <- Run(ctx, b.o) }()
	select {
	case <-b.mounts:
	case err := <-ended:
		b.t.Fatalf("the view ended before it was mounted: %v", err)
	case <-time.After(10 * time.Second):
		b.t.Fatal("the view was never mounted")
	}
	return ended
}

// The view read only (specs.md §21.2): mounted once its first check read every disk, each file read from its
// original; a disk that goes away is read from no more within a check, and once back is read again; a stop unmounts it.
func TestTheViewReadOnlyServesUntilItsStop(t *testing.T) {
	b := newRunBench(t)
	ready := make(chan struct{})
	b.o.Ready = func() { close(ready) }
	ctx, stop := context.WithCancel(t.Context())
	ended := b.start(ctx)
	<-ready
	if b.w != nil {
		t.Error("a view read only was given a writer")
	}
	read := func() (string, error) {
		n, err := b.tree.Lookup(t.Context(), "Films/a.mkv")
		if err != nil || n == nil {
			return "", fmt.Errorf("lookup: %v, %w", n, err)
		}
		f, err := b.tree.Open(t.Context(), "Films/a.mkv", n)
		if err != nil {
			return "", err //nolint:wrapcheck // the test's own
		}
		defer func() { _ = f.Close() }()
		buf := make([]byte, 16)
		got, _ := f.ReadAt(buf, 0)
		return string(buf[:got]), nil
	}
	if got, err := read(); err != nil || got != "from t2" {
		t.Errorf("Films/a.mkv reads %q, %v", got, err)
	}
	b.machine.setAway(b.config.Disks[1].Mount, true)
	b.waitSaid("a disk cannot be read")
	if got, err := read(); got == "from t2" {
		t.Errorf("t2 away, Films/a.mkv reads %q from it, %v", got, err)
	}
	b.machine.setAway(b.config.Disks[1].Mount, false)
	b.waitSaid("a disk can be read again")
	if got, err := read(); err != nil || got != "from t2" {
		t.Errorf("t2 back, Films/a.mkv reads %q, %v", got, err)
	}
	stop()
	if err := <-ended; err != nil {
		t.Errorf("the view ended with %v", err)
	}
	if !b.served.unmounted {
		t.Error("the stop did not unmount the view")
	}
	for _, said := range []string{"the view is mounted, read only", "the view is unmounted"} {
		if !strings.Contains(b.said(), said) {
			t.Errorf("the log does not say %q:\n%s", said, b.said())
		}
	}
}

// The view that writes is given its writer once what a stop left planned is resolved; unmounted by anything else, it
// ends with ErrUnmounted.
func TestTheViewThatWritesEndsWhenUnmountedByAnother(t *testing.T) {
	b := newRunBench(t)
	b.o.Writes = true
	ended := b.start(t.Context())
	if b.w == nil {
		t.Fatal("a view that writes was given no writer")
	}
	close(b.served.done)
	if err := <-ended; !errors.Is(err, ErrUnmounted) {
		t.Errorf("the view ended with %v", err)
	}
	if !strings.Contains(b.said(), "the view is mounted, and writes") {
		t.Errorf("the log:\n%s", b.said())
	}
}

// A view that cannot be unmounted at its stop says so; one that cannot be mounted says where.
func TestTheViewSaysWhatItsMountAndUnmountMet(t *testing.T) {
	b := newRunBench(t)
	b.served.fail = errors.New("busy")
	ctx, stop := context.WithCancel(t.Context())
	ended := b.start(ctx)
	stop()
	if err := <-ended; err == nil || !strings.Contains(err.Error(), "unmount") || !strings.Contains(err.Error(), "busy") {
		t.Errorf("the view ended with %v", err)
	}

	b = newRunBench(t)
	b.o.mount = func(*view.Tree, string, *writer.Writer) (served, error) { return nil, errors.New("no device") }
	if err := Run(t.Context(), b.o); err == nil || !strings.Contains(err.Error(), "mount at "+b.o.Point) {
		t.Errorf("%v", err)
	}
}

// The point the view is mounted at must be a folder that exists, which Atoll never makes, and where nothing else is
// mounted.
func TestTheViewsPoint(t *testing.T) {
	b := newRunBench(t)
	b.o.Point = filepath.Join(b.top, "nowhere")
	if err := Run(t.Context(), b.o); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a point that does not exist: %v", err)
	}
	b.o.Point = filepath.Join(b.top, "t1", "media", "Films", "a.mkv")
	if err := Run(t.Context(), b.o); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("a point that is a file: %v", err)
	}
	// The root of the machine, where a filesystem other than the view's is mounted, whatever it is.
	b.o.Point = "/"
	if err := Run(t.Context(), b.o); err == nil || !strings.Contains(err.Error(), "is mounted there") {
		t.Errorf("a point where another filesystem is mounted: %v", err)
	}
	if gone(b.top) {
		t.Error("a folder is taken for a view no process serves")
	}
}

// A disk the configuration says failed is presented from no root, and gets no handle to write through.
func TestAFailedDiskIsNeitherReadNorWritten(t *testing.T) {
	b := newRunBench(t)
	b.config.Disks[1].State = config.DiskFailed
	roots := view.NewDiskRoots()
	defer roots.Close()
	handles := writer.NewHandles()
	defer handles.Close()
	ch := &checker{o: b.o, roots: roots, handles: handles, readable: map[string]bool{}}
	ch.check(t.Context())
	if handles.Has("t2") || !handles.Has("t1") {
		t.Errorf("handles: t1 %v, t2 %v", handles.Has("t1"), handles.Has("t2"))
	}
	if !strings.Contains(b.said(), "reason=failed") {
		t.Errorf("the log:\n%s", b.said())
	}
	// Once the catalog cannot be read, the check takes the markers it read last, and goes on.
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	ch.check(t.Context())
	if !strings.Contains(b.said(), "which disks' own marker was seen is not read now") {
		t.Errorf("the log:\n%s", b.said())
	}
}

// plannedOnT2 deletes Films/a.mkv through the view's writer, its move to the trash meeting EIO on t2, which presents
// it: a disk that cannot be relied on, the deletion left planned (specs.md §17.1, §21.2).
func (b *runBench) plannedOnT2() {
	b.t.Helper()
	t2 := b.config.Disks[1].Mount
	b.machine.setFail(t2, func(call, _ string) error {
		if call == "rename" {
			return syscall.EIO
		}
		return nil
	})
	root := writer.Caller{Creds: &view.Credentials{Caps: ^view.Caps(0)}}
	if err := b.w.Unlink(b.t.Context(), "Films/a.mkv", root); err == nil {
		b.t.Fatal("the deletion went through a disk that fails")
	}
	b.machine.setFail(t2, nil)
	if got := b.planned(); got != 1 {
		b.t.Fatalf("%d operations of the view planned, want the deletion's", got)
	}
}

// planned counts the operations of the view left planned.
func (b *runBench) planned() int {
	b.t.Helper()
	var n int
	if err := b.o.DB.QueryRowContext(b.t.Context(), "SELECT count(*) FROM view_ops WHERE state = 'planned'").Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// checks lets the view's check run some twenty times.
func (b *runBench) checks() {
	time.Sleep(20 * b.o.Every)
}

// The view's check every 30 s resolves what a disk lost left planned only once a check found that disk lost, missing
// or on another mount, and a later one finds it there again on a mount of its own, confirmed past the page cache, as a
// session of the daemon confirms a disk (the maintainer, 2026-11-23; specs.md §10, §21.2). A disk whose device fails
// while it stays mounted answers its mount check from the page cache: never found lost, it resolves nothing; found
// lost and back, but failing a read past the cache, it resolves nothing either; back on a mount of its own and
// confirmed, it resolves the deletion, nothing having moved, as undone, the copy at its name.
func TestTheViewsCheckResolvesOnlyADiskBackAndConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lose takes t2 away and gives it back, as the test says, once the deletion is planned.
		lose func(b *runBench, t2 string)
		want int
	}{
		{"failing while still mounted, never found lost", func(b *runBench, t2 string) {
			b.machine.setFailing(t2, true)
			b.checks()
		}, 1},
		{"found missing, back on a mount of its own, failing a read past the cache", func(b *runBench, t2 string) {
			b.machine.setAway(t2, true)
			b.waitSaid("a disk cannot be read")
			b.machine.setFailing(t2, true)
			b.machine.remount(t2)
			b.machine.setAway(t2, false)
			b.waitSaid("a disk can be read again")
			b.checks()
		}, 1},
		{"found missing, its device gone, back on the same mount", func(b *runBench, t2 string) {
			b.machine.setGone(t2, true)
			b.waitSaid("a disk cannot be read")
			b.machine.setGone(t2, false)
			b.waitSaid("a disk can be read again")
			b.checks()
		}, 1},
		{"found missing, back on a mount of its own, confirmed", func(b *runBench, t2 string) {
			b.machine.setAway(t2, true)
			b.waitSaid("a disk cannot be read")
			b.machine.remount(t2)
			b.machine.setAway(t2, false)
			b.waitSaid("a disk can be read again")
			b.checks()
		}, 0},
		{"on another mount between two checks, confirmed", func(b *runBench, t2 string) {
			b.machine.remount(t2)
			b.checks()
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newRunBench(t)
			b.o.Writes = true
			ctx, stop := context.WithCancel(t.Context())
			ended := b.start(ctx)
			b.plannedOnT2()
			t2 := b.config.Disks[1].Mount
			tc.lose(b, t2)
			if got := b.planned(); got != tc.want {
				t.Errorf("%d operations of the view planned once its check ran, want %d:\n%s", got, tc.want, b.said())
			}
			if _, err := os.Stat(filepath.Join(t2, "media", "Films", "a.mkv")); err != nil {
				t.Errorf("the copy is not at its name on t2: %v", err)
			}
			stop()
			if err := <-ended; err != nil {
				t.Errorf("the view ended with %v", err)
			}
		})
	}
}
