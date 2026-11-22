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
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// awayMachine is machine, with disks a test takes away and gives back: a disk away is a mount point with nothing
// mounted.
type awayMachine struct {
	machine
	mu   sync.Mutex
	away map[string]bool
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
	return m.machine.Mount(mount)
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
