package watch

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/solver"
)

const testNow = int64(1767225600000000000)

// bench is a pool of real folders, on a machine that says every disk is mounted, and that can make
// any call on a disk fail.
type bench struct {
	t      *testing.T
	db     *sql.DB
	config *config.Config
	dirs   map[string]string
	wrap   func(disk string, d fsx.Dir) fsx.Dir
	now    int64
	// readOnly makes every disk show a read-only mount.
	readOnly bool
	// noSpace says which mounts fail statfs.
	noSpace map[string]error
}

// pastTick lets the clock behind a change time move, which Linux 6.12 can leave the same within a
// clock tick (specs.md §17.1), and at the lowest usual HZ a tick is 10 ms.
func pastTick() { time.Sleep(20 * time.Millisecond) }

func (b *bench) Mount(mount string) disks.MountSeen {
	options := []string{"rw"}
	if b.readOnly {
		options = []string{"ro"}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: "ext4", MountOptions: options, SuperOptions: options,
	}}
}

func (b *bench) Device(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	for i := range b.config.Disks {
		if b.config.Disks[i].Mount == entry.MountPoint {
			return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: b.config.Disks[i].UUID}}
		}
	}
	return disks.DeviceSeen{Device: disks.DeviceGoneAway}
}

func (b *bench) Root(string, string) disks.RootSeen {
	return disks.RootSeen{Folder: disks.FolderOK}
}

func (b *bench) Now() int64 { return b.now }

func (b *bench) OpenTop(mount string) (fsx.Dir, error) {
	return fsx.OpenDir(mount) //nolint:wrapcheck // a test double
}

func (b *bench) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return b.Device(fs, entry)
}

func (b *bench) Checkpoint(string) {}

func (b *bench) Space(mount string) (capacity, free int64, err error) {
	if err := b.noSpace[mount]; err != nil {
		return 0, 0, err
	}
	return 1 << 30, 1 << 29, nil
}

func (b *bench) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := fsx.OpenDir(filepath.Join(mount, root))
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	if b.wrap != nil {
		for id, m := range b.dirs {
			if m == mount {
				return b.wrap(id, dir), nil
			}
		}
	}
	return dir, nil
}

func newBench(t *testing.T, ids ...string) *bench {
	t.Helper()
	top := t.TempDir()
	var text strings.Builder
	file := filepath.Join(top, "catalog.db")
	fmt.Fprintf(&text, "pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", file, filepath.Join(top, "pool"))
	dirs := map[string]string{}
	for i, id := range ids {
		mount := filepath.Join(top, id)
		dirs[id] = mount
		if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&text, "[[disk]]\nid = %q\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d\"\nmount = %q\nfs = \"ext4\"\n", id, i, mount)
	}
	c, err := config.Parse([]byte(text.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &bench{t: t, db: db, config: c, dirs: dirs, now: testNow}
}

func (b *bench) write(disk, path, text string) {
	b.t.Helper()
	full := filepath.Join(b.dirs[disk], "media", path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) scan(opts Options) *Outcome {
	b.t.Helper()
	out, err := Scan(b.t.Context(), b.db, b.config, b, opts)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

func (b *bench) count(query string, args ...any) int {
	b.t.Helper()
	var n int
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// failing makes one call on one name fail, on any disk.
func failing(call, name string, with error) func(string, fsx.Dir) fsx.Dir {
	return func(_ string, d fsx.Dir) fsx.Dir {
		return fsx.Fault(d, func(c, n string) error {
			if c == call && (name == "" || n == name) {
				return with
			}
			return nil
		})
	}
}

// TestAnErrorConcludesNothing: an I/O error during a walk means "disk unknown", never "file absent"
// (invariant 4). Nothing is written, and no copy is reported absent, wherever the error comes.
// TestCorruptOutlivesAnOutage: only the state remembers it, and a scenario cannot corrupt a disk.
// conflict is in the scenario scan-original-kept.
func TestCorruptOutlivesAnOutage(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	if _, err := b.db.ExecContext(t.Context(), "UPDATE files SET state = 'corrupt'"); err != nil {
		t.Fatal(err)
	}
	b.wrap = failing("names", "", syscall.EIO)
	if out := b.scan(Options{}); !out.Partial() {
		t.Fatalf("the disk answered: %+v", out.Disks[0])
	}
	if got := b.count("SELECT count(*) FROM disks WHERE state = 'missing'"); got != 1 {
		t.Fatalf("%d disks missing", got)
	}
	if got := b.text("SELECT state FROM files"); got != "corrupt" {
		t.Errorf("through the outage: %q", got)
	}
	b.wrap = nil
	if out := b.scan(Options{}); out.Partial() {
		t.Fatalf("the disk did not come back: %+v", out.Disks[0])
	}
	if got := b.text("SELECT state FROM files"); got != "corrupt" {
		t.Errorf("after the outage: %q", got)
	}
}

// TestCorruptStaysWhenCopiesDiffer: both apply, and it comes first (specs.md §4).
func TestCorruptStaysWhenCopiesDiffer(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		opts       Options
	}{
		{"a new copy of another size", "yy", Options{}},
		{"a new copy of the same size, with other content", "y", Options{Full: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.write("t1", "a", "x")
			b.scan(Options{})
			if _, err := b.db.ExecContext(t.Context(), "UPDATE files SET state = 'corrupt'"); err != nil {
				t.Fatal(err)
			}
			b.write("t2", "a", tc.text)
			b.scan(tc.opts)
			if got := b.text("SELECT state FROM files"); got != "corrupt" {
				t.Errorf("%q", got)
			}
		})
	}
}

func TestAnErrorConcludesNothing(t *testing.T) {
	for _, tc := range []struct {
		call, name string
		err        error
	}{
		{"names", "", syscall.EIO},
		{"stat", "b", syscall.EIO},
		{"stat", "sub", syscall.ENOTCONN},
		{"stat", "c", syscall.ENODEV},
		{"opendir", "sub", syscall.EIO},
		{"readlink", "link", syscall.EIO},
		{"stat", "b", syscall.EACCES},
	} {
		t.Run(tc.call+"/"+tc.name+"/"+tc.err.Error(), func(t *testing.T) {
			b := newBench(t, "t1")
			b.write("t1", "a", "x")
			b.write("t1", "b", "y")
			b.write("t1", "sub/c", "z")
			if err := os.Symlink("a", filepath.Join(b.dirs["t1"], "media", "link")); err != nil {
				t.Fatal(err)
			}
			first := b.scan(Options{})
			if first.Partial() || first.Disks[0].New != 3 {
				t.Fatalf("the first scan: %+v", first.Disks[0])
			}
			if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "a")); err != nil {
				t.Fatal(err)
			}
			b.write("t1", "new", "w")
			b.wrap = failing(tc.call, tc.name, tc.err)
			out := b.scan(Options{})
			r := out.Disks[0]
			if r.Walked || r.Verdict.State != disks.Missing || r.Verdict.Reason != disks.IOError {
				t.Errorf("verdict: %+v", r)
			}
			if !out.Partial() {
				t.Error("not reported as partial")
			}
			if r.New != 0 || r.Known != 0 || r.Files != 0 || len(r.Absent) != 0 || len(r.Changed) != 0 {
				t.Errorf("concluded from a walk that failed: %+v", r)
			}
			if got := b.count("SELECT count(*) FROM copies"); got != 3 {
				t.Errorf("%d copies, want the 3 of before", got)
			}
			if got := b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("new")); got != 0 {
				t.Error("a file of a walk that failed was written")
			}
			if got := b.count("SELECT count(*) FROM entries"); got != 2 {
				t.Errorf("%d entries, want the 2 of before", got)
			}
			if got := b.count("SELECT count(*) FROM disks WHERE state = 'missing'"); got != 1 {
				t.Error("the disk is not recorded as missing")
			}
		})
	}
}

// TestAnUnreachableNameIsNoAbsence: a listed name that cannot be reached is reported as such, not
// absent, and the rest of the disk is still read.
func TestAnUnreachableNameIsNoAbsence(t *testing.T) {
	for _, tc := range []struct {
		call, name string
		err        error
		want       string
	}{
		{"stat", "b", syscall.ENOENT, "b"},
		{"stat", "b", syscall.ELOOP, "b"},
		{"stat", "b", syscall.ENOTDIR, "b"},
		{"opendir", "sub", syscall.ENOENT, "sub"},
		{"opendir", "sub", syscall.ELOOP, "sub"},
		{"opendir", "sub", syscall.ENOTDIR, "sub"},
		{"readlink", "link", syscall.ENOENT, "link"},
	} {
		t.Run(tc.call+"/"+tc.err.Error(), func(t *testing.T) {
			b := newBench(t, "t1")
			b.write("t1", "a", "x")
			b.write("t1", "b", "y")
			b.write("t1", "sub/c", "z")
			if err := os.Symlink("a", filepath.Join(b.dirs["t1"], "media", "link")); err != nil {
				t.Fatal(err)
			}
			b.wrap = failing(tc.call, tc.name, tc.err)
			r := b.scan(Options{}).Disks[0]
			if !r.Walked || r.Verdict.State != disks.Online {
				t.Errorf("verdict: %+v", r)
			}
			if !slices.Equal(r.Unreadable, []string{tc.want}) {
				t.Errorf("unreadable: %v, want %s", r.Unreadable, tc.want)
			}
			if got := b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("a")); got != 1 {
				t.Error("the rest of the disk was not read")
			}
			if got := b.count("SELECT count(*) FROM entries WHERE lpath = ?", []byte(tc.want)); got != 0 {
				t.Error("a row for a name it could not reach")
			}
			if tc.call == "opendir" && r.Folders != 0 {
				t.Errorf("%d folders counted", r.Folders)
			}
		})
	}
}

// TestAFailedStatfs: a disk that passed its check and cannot say how full it is counts as failed, keeps
// what the last check measured, and the others go on (specs.md §15.10, §25).
func TestAFailedStatfs(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(Options{})
	b.noSpace = map[string]error{b.dirs["t1"]: syscall.ENOTCONN}
	out := b.scan(Options{})
	first, second := out.Disks[0], out.Disks[1]
	if first.Walked || first.Verdict.State != disks.Missing || first.Verdict.Reason != disks.IOError {
		t.Errorf("%+v", first)
	}
	if !second.Walked || second.Verdict.State != disks.Online {
		t.Errorf("the other disk: %+v", second)
	}
	if !out.Partial() {
		t.Error("nothing says that a disk failed")
	}
	if got := b.count("SELECT count(*) FROM disks WHERE id = 't1' AND state = 'missing' AND capacity_bytes = ?", int64(1<<30)); got != 1 {
		t.Error("the row of the disk is not what it should be")
	}
}

// TestAnIgnoredFileKeepsItsMode: what a rule ignores is an entry like any other.
func TestAnIgnoredFileKeepsItsMode(t *testing.T) {
	b := newBench(t, "t1")
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", filepath.Join(t.TempDir(), "catalog.db"), filepath.Join(t.TempDir(), "pool")) +
		fmt.Sprintf("[[disk]]\nid = \"t1\"\nuuid = %q\nmount = %q\nfs = \"ext4\"\n", b.config.Disks[0].UUID, b.dirs["t1"]) +
		"[[rule]]\nlabel = \"skip\"\next = [\"nfo\"]\naction = \"ignore\"\n"
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	b.config = c
	b.write("t1", "x.nfo", "n")
	full := filepath.Join(b.dirs["t1"], "media", "x.nfo")
	for _, mode := range []os.FileMode{0o640, 0o600} {
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
		b.scan(Options{})
		if got := b.count("SELECT mode FROM entries WHERE lpath = ?", []byte("x.nfo")); got != int(mode) {
			t.Errorf("mode %o, want %o", got, mode)
		}
	}
}

// TestWhatWasNotReachedIsNotAbsent: a known copy behind a name that cannot be reached, or below a
// folder that cannot, or below another mount. Nothing can be said about it, and it is not read: a
// path through another mount would even lead to a file of that mount.
func TestWhatWasNotReachedIsNotAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(string, fsx.Dir) fsx.Dir
		want []string
		mnt  []string
	}{
		{"a name", failing("stat", "b", syscall.ENOENT), []string{"b"}, nil},
		{"a folder", failing("opendir", "sub", syscall.ENOENT), []string{"sub"}, nil},
		{"another mount", func(_ string, d fsx.Dir) fsx.Dir { return &otherMount{Dir: d, t: t, name: "sub"} }, nil, []string{"sub"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1")
			b.write("t1", "a", "x")
			b.write("t1", "b", "y")
			b.write("t1", "sub/c", "z")
			b.write("t1", "sub/deep/d", "w")
			b.write("t1", "sub2", "v")
			b.scan(Options{})
			b.wrap = tc.wrap
			r := b.scan(Options{Full: true}).Disks[0]
			if len(r.Absent) != 0 {
				t.Errorf("absent: %v", r.Absent)
			}
			if !slices.Equal(r.Unreadable, tc.want) && len(r.Unreadable)+len(tc.want) > 0 {
				t.Errorf("unreadable: %v, want %v", r.Unreadable, tc.want)
			}
			if !slices.Equal(r.OtherMounts, tc.mnt) && len(r.OtherMounts)+len(tc.mnt) > 0 {
				t.Errorf("other mounts: %v, want %v", r.OtherMounts, tc.mnt)
			}
			if r.Hashed == 0 || r.Verdict.State != disks.Online {
				t.Errorf("%+v", r)
			}
			if got := b.count("SELECT count(*) FROM copies"); got != 5 {
				t.Errorf("%d copies", got)
			}
		})
	}
}

func TestPastGrace(t *testing.T) {
	const grace = 24 * time.Hour
	now := testNow
	for _, tc := range []struct {
		name     string
		lastSeen int64
		want     bool
	}{
		{"just seen", now, false},
		{"exactly the grace ago", now - int64(grace), false},
		{"a nanosecond more", now - int64(grace) - 1, true},
		{"long ago", 1, true},
		{"never seen", 0, false},
	} {
		got := planDisks([]catalog.Disk{{ID: "t1", State: "missing", LastSeenNS: tc.lastSeen, FS: "ext4"}}, now, grace)
		if got[0].PastGrace != tc.want {
			t.Errorf("%s: got %v", tc.name, got[0].PastGrace)
		}
	}
}

func TestAFifoIsIgnored(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	if err := syscall.Mkfifo(filepath.Join(b.dirs["t1"], "media", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := b.scan(Options{Full: true}).Disks[0]
	if r.Ignored != 1 || r.Files != 1 || r.Hashed != 1 {
		t.Errorf("%+v", r)
	}
	if got := b.count("SELECT count(*) FROM entries WHERE lpath = ? AND kind = 'ignored'", []byte("pipe")); got != 1 {
		t.Error("the FIFO is no ignored entry")
	}
}

// otherMount makes a folder look as if it were on another mount, and fails if it is entered.
type otherMount struct {
	fsx.Dir
	t    *testing.T
	name string
}

func (d *otherMount) Stat(name string) (fsx.Stat, error) {
	st, err := d.Dir.Stat(name)
	if name == d.name {
		st.MountID++
	}
	return st, err //nolint:wrapcheck // a test double
}

func (d *otherMount) OpenDir(name string) (fsx.Dir, error) {
	if name == d.name {
		d.t.Errorf("a walk entered %q, on another mount", name)
	}
	return d.Dir.OpenDir(name) //nolint:wrapcheck // a test double
}

func TestAnotherMountIsNotEntered(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.write("t1", "inner/not-ours", "y")
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir { return &otherMount{Dir: d, t: t, name: "inner"} }
	r := b.scan(Options{}).Disks[0]
	if !slices.Equal(r.OtherMounts, []string{"inner"}) {
		t.Errorf("other mounts: %v", r.OtherMounts)
	}
	if r.Files != 1 || r.Folders != 1 {
		t.Errorf("%+v", r)
	}
	if got := b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("inner/not-ours")); got != 0 {
		t.Error("a file of another mount was recorded")
	}
}

func TestACanceledScanIsAnError(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Scan(ctx, b.db, b.config, b, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

// TestReadingStopsAtAnError: an I/O error during --full stops that disk, keeps what it hashed, and
// leaves the other disk to go on.
func TestReadingStopsAtAnError(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t1", "b", "y")
	b.write("t1", "c", "z")
	b.write("t2", "d", "w")
	b.scan(Options{})
	b.wrap = func(disk string, d fsx.Dir) fsx.Dir {
		if disk != "t1" {
			return d
		}
		return fsx.Fault(d, func(call, name string) error {
			if call == "read" && name == "b" {
				return syscall.EIO
			}
			return nil
		})
	}
	out := b.scan(Options{Full: true})
	t1, t2 := out.Disks[0], out.Disks[1]
	if t1.Verdict.State != disks.Missing || t1.Verdict.Reason != disks.IOError || !t1.Walked || t1.Hashed != 1 {
		t.Errorf("t1: %+v", t1)
	}
	if t2.Verdict.State != disks.Online || t2.Hashed != 1 {
		t.Errorf("t2: %+v", t2)
	}
	if !out.Partial() {
		t.Error("not reported as partial")
	}
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 2 {
		t.Errorf("%d hashes, want a and d", got)
	}
	if got := b.count("SELECT count(*) FROM disks WHERE id = 't1' AND state = 'missing'"); got != 1 {
		t.Error("t1 is not recorded as missing")
	}
	// The plan runs again, knowing that: a replica cannot go to t1 any more, and a file on it has
	// no source.
	if got := b.text("SELECT state FROM files WHERE lpath = ?", []byte("d")); got != "degraded" {
		t.Errorf("d is %s", got)
	}
	if got := b.text("SELECT state FROM files WHERE lpath = ?", []byte("a")); got != "unavailable" {
		t.Errorf("a is %s", got)
	}
}

func TestReadingAnUnreachableName(t *testing.T) {
	for _, tc := range []struct {
		call string
		err  error
	}{
		{"openfile", syscall.ENOENT},
		{"openfile", syscall.ELOOP},
	} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.write("t1", "b", "y")
		b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
			return fsx.Fault(d, func(call, name string) error {
				if call == tc.call && name == "a" {
					return tc.err
				}
				return nil
			})
		}
		r := b.scan(Options{Full: true}).Disks[0]
		if !slices.Equal(r.Unreadable, []string{"a"}) || r.Hashed != 1 || r.Verdict.State != disks.Online {
			t.Errorf("%v: %+v", tc.err, r)
		}
	}
}

// changing changes a file as it is read, as a user might, and serves what the disk holds.
type changing struct {
	fsx.Dir
	name   string
	atOpen func()
	during func()
	serve  []byte // when given, what a read gets instead of the file: corruption
}

func (d *changing) OpenFile(name string) (fsx.File, error) {
	if name == d.name && d.atOpen != nil {
		d.atOpen()
	}
	f, err := d.Dir.OpenFile(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	if name != d.name {
		return f, nil
	}
	file := &changingFile{File: f}
	if d.during != nil {
		file.during = d.during
	}
	if d.serve != nil {
		file.reader = bytes.NewReader(d.serve)
	}
	return file, nil
}

type changingFile struct {
	fsx.File
	during func()
	reader io.Reader
	done   atomic.Bool
}

func (f *changingFile) Read(p []byte) (int, error) {
	if f.during != nil && !f.done.Swap(true) {
		f.during()
	}
	if f.reader != nil {
		return f.reader.Read(p) //nolint:wrapcheck // io.EOF must stay as it is
	}
	return f.File.Read(p) //nolint:wrapcheck // io.EOF must stay as it is
}

func TestAFileThatMovesDuringTheRead(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	pastTick()
	full := filepath.Join(b.dirs["t1"], "media", "a")
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return &changing{Dir: d, name: "a", during: func() {
			if err := os.WriteFile(full, []byte("y"), 0o644); err != nil {
				t.Fatal(err)
			}
		}}
	}
	r := b.scan(Options{Full: true}).Disks[0]
	if !slices.Equal(r.Changed, []string{"a"}) {
		t.Errorf("changed: %v", r.Changed)
	}
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 0 {
		t.Error("the hash of a file that moved was kept")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL"); got != 0 {
		t.Error("a file that moved was verified")
	}
}

// TestAVerifiedCopyThatDiffers: same size and times, another hash. The scrub decides what that is
// (specs.md §16), so no conflict yet, and no scenario can reach this. But nothing may rely on it, or
// prune would count it as a good copy and send the good one to the trash (specs.md §15.10).
func TestAVerifiedCopyThatDiffers(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{Full: true})
	if got := b.count("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL"); got != 1 {
		t.Fatalf("%d verified", got)
	}
	serve := func(_ string, d fsx.Dir) fsx.Dir { return &changing{Dir: d, name: "a", serve: []byte("y")} }
	b.wrap = serve
	r := b.scan(Options{Full: true}).Disks[0]
	if !slices.Equal(r.Changed, []string{"a"}) {
		t.Errorf("changed: %v", r.Changed)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL"); got != 0 {
		t.Error("it stays verified")
	}
	if got := b.text("SELECT state FROM files"); got == "conflict" {
		t.Error("a conflict, on a first mismatch")
	}
	// Read again with the same answer, it is an unverified copy that differs.
	b.scan(Options{Full: true})
	if got := b.text("SELECT state FROM files"); got != "conflict" {
		t.Errorf("the state is %q", got)
	}
	// A wrong read heals itself.
	c := newBench(t, "t1")
	c.write("t1", "a", "x")
	c.scan(Options{Full: true})
	c.wrap = serve
	c.scan(Options{Full: true})
	c.wrap = nil
	c.scan(Options{Full: true})
	if got := c.count("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL"); got != 1 {
		t.Error("it did not come back")
	}
	if got := c.text("SELECT state FROM files"); got == "conflict" {
		t.Error("a conflict, from one wrong read")
	}
}

func TestAnUnverifiedCopyThatDiffersIsAConflict(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(Options{Full: true})
	b.write("t2", "a", "y")
	out := b.scan(Options{Full: true})
	if !slices.Equal(out.Conflicts, []string{"a"}) {
		t.Errorf("conflicts: %v", out.Conflicts)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns IS NULL"); got != 1 {
		t.Error("the copy that differs was verified")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND verified_ns IS NOT NULL"); got != 1 {
		t.Error("the first copy is no longer verified")
	}
}

// altered changes what statx says about one file.
type altered struct {
	fsx.Dir
	name   string
	change func(*fsx.Stat)
}

func (d *altered) Stat(name string) (fsx.Stat, error) {
	st, err := d.Dir.Stat(name)
	if name == d.name {
		d.change(&st)
	}
	return st, err //nolint:wrapcheck // a test double
}

// TestWhatMakesACopyChanged: each of the four, alone. A change of the change time alone is what
// chmod makes, and no scenario has it.
func TestWhatMakesACopyChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fsx.Stat)
	}{
		{"size", func(st *fsx.Stat) { st.Size++ }},
		{"modification time", func(st *fsx.Stat) { st.Mtime.Nsec = (st.Mtime.Nsec + 1) % 1000000000 }},
		{"change time", func(st *fsx.Stat) { st.Ctime.Nsec = (st.Ctime.Nsec + 1) % 1000000000 }},
		{"inode", func(st *fsx.Stat) { st.Ino++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1")
			b.write("t1", "a", "x")
			b.write("t1", "b", "y")
			b.scan(Options{})
			b.wrap = func(_ string, d fsx.Dir) fsx.Dir { return &altered{Dir: d, name: "a", change: tc.change} }
			r := b.scan(Options{}).Disks[0]
			if !slices.Equal(r.Changed, []string{"a"}) || r.Known != 1 {
				t.Errorf("%+v", r)
			}
		})
	}
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	pastTick()
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := b.scan(Options{}).Disks[0]; !slices.Equal(r.Changed, []string{"a"}) {
		t.Errorf("after a chmod: %+v", r)
	}
}

func (b *bench) text(query string, args ...any) string {
	b.t.Helper()
	var s sql.NullString
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&s); err != nil {
		b.t.Fatal(err)
	}
	return s.String
}

// TestEntriesFollowTheDisk: a folder's mode and a symlink's target, as they change.
func TestEntriesFollowTheDisk(t *testing.T) {
	b := newBench(t, "t1")
	media := filepath.Join(b.dirs["t1"], "media")
	b.write("t1", "sub/a", "x")
	if err := os.Chmod(filepath.Join(media, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("old", filepath.Join(media, "link")); err != nil {
		t.Fatal(err)
	}
	b.scan(Options{})
	if got := b.count("SELECT mode FROM entries WHERE lpath = ?", []byte("sub")); got != 0o750 {
		t.Errorf("mode %o", got)
	}
	info, err := os.Stat(filepath.Join(media, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT mtime_ns / 1000000 FROM entries WHERE lpath = ?", []byte("sub")); int64(got) != info.ModTime().UnixMilli() {
		t.Errorf("mtime %d, want %d", got, info.ModTime().UnixMilli())
	}
	if err := os.Chmod(filepath.Join(media, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	when := info.ModTime().Add(-1000000000)
	if err := os.Chtimes(filepath.Join(media, "sub"), when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(media, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new", filepath.Join(media, "link")); err != nil {
		t.Fatal(err)
	}
	b.scan(Options{})
	if got := b.count("SELECT mode FROM entries WHERE lpath = ?", []byte("sub")); got != 0o700 {
		t.Errorf("mode %o, after a chmod", got)
	}
	if got := b.count("SELECT mtime_ns / 1000000 FROM entries WHERE lpath = ?", []byte("sub")); int64(got) != when.UnixMilli() {
		t.Errorf("mtime %d, want %d", got, when.UnixMilli())
	}
	if got := b.text("SELECT CAST(target AS TEXT) FROM entries WHERE lpath = ?", []byte("link")); got != "new" {
		t.Errorf("target %q", got)
	}
}

// TestACancelDuringTheWalk: every folder looks at the context, not only the start.
func TestACancelDuringTheWalk(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "sub/deep/a", "x")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return fsx.Fault(d, func(call, name string) error {
			switch {
			case call == "stat" && name == "sub":
				cancel()
			case call == "names" && ctx.Err() != nil:
				t.Error("a folder was read after the cancel")
			}
			return nil
		})
	}
	if _, err := Scan(ctx, b.db, b.config, b, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
	if got := b.count("SELECT count(*) FROM files"); got != 0 {
		t.Errorf("%d files of a walk that was canceled", got)
	}
}

// TestAReadonlyDiskIsWalked: it can be read, and what a check confirmed is recorded.
func TestAReadonlyDiskIsWalked(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.readOnly = true
	out := b.scan(Options{})
	r := out.Disks[0]
	if !r.Walked || r.Verdict.State != disks.Readonly || r.Verdict.Reason != disks.ReadOnlyMount || r.New != 1 {
		t.Errorf("%+v", r)
	}
	if out.Partial() {
		t.Error("reported as partial")
	}
	if got := b.count("SELECT capacity_bytes FROM disks WHERE id = 't1' AND state = 'readonly' AND last_seen_ns IS NOT NULL"); got != 1<<30 {
		t.Errorf("capacity %d", got)
	}
}

// TestAFileFollowsItsOriginal: its size and date are the original's, the reference is the original's
// hash, and the original moves as the creation times say. The times are set in the catalog, as
// whether a filesystem has them is no part of this.
func TestAFileFollowsItsOriginal(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "y")
	full := filepath.Join(b.dirs["t2"], "media", "a")
	when := time.Unix(1500000000, 0)
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
	b.scan(Options{})
	set := func(disk string, ns int64) {
		t.Helper()
		if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET btime_ns = ? WHERE disk_id = ?", ns, disk); err != nil {
			t.Fatal(err)
		}
	}
	set("t1", 2000)
	set("t2", 1000)
	b.scan(Options{})
	if got := b.text("SELECT original_disk FROM files"); got != "t2" {
		t.Errorf("original %q", got)
	}
	if got := b.count("SELECT count(*) FROM files WHERE mtime_ns = ?", when.UnixNano()); got != 1 {
		t.Error("the file does not have its original's date")
	}
	out := b.scan(Options{Full: true})
	if !slices.Equal(out.Conflicts, []string{"a"}) {
		t.Errorf("conflicts: %v", out.Conflicts)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns IS NOT NULL"); got != 1 {
		t.Error("the reference is not the original's hash")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND verified_ns IS NULL"); got != 1 {
		t.Error("the other copy was verified")
	}
	// In conflict, it keeps its original whatever the times say next.
	set("t1", 500)
	b.scan(Options{})
	if got := b.text("SELECT original_disk FROM files"); got != "t2" {
		t.Errorf("a conflict moved its original to %q", got)
	}
}

// TestAFileThatMovedBeforeTheRead: between the walk and the read, so its look before and after agree,
// and only the row of this scan tells.
func TestAFileThatMovedBeforeTheRead(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	pastTick()
	full := filepath.Join(b.dirs["t1"], "media", "a")
	var opened atomic.Bool
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return &changing{Dir: d, name: "a", atOpen: func() {
			if !opened.Swap(true) {
				if err := os.WriteFile(full, []byte("y"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}}
	}
	r := b.scan(Options{Full: true}).Disks[0]
	if !slices.Equal(r.Changed, []string{"a"}) {
		t.Errorf("changed: %v", r.Changed)
	}
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL"); got != 0 {
		t.Error("the hash of a file that moved was kept")
	}
}

// TestAChmodDuringTheRead: the modification time stays, and the change time says it moved.
func TestAChmodDuringTheRead(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	pastTick()
	full := filepath.Join(b.dirs["t1"], "media", "a")
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return &changing{Dir: d, name: "a", during: func() {
			if err := os.Chmod(full, 0o600); err != nil {
				t.Fatal(err)
			}
		}}
	}
	r := b.scan(Options{Full: true}).Disks[0]
	if !slices.Equal(r.Changed, []string{"a"}) {
		t.Errorf("changed: %v", r.Changed)
	}
}

func TestListsAreSorted(t *testing.T) {
	b := newBench(t, "t1")
	for _, name := range []string{"z", "a", "m", "k", "c"} {
		b.write("t1", name, "x")
	}
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return fsx.Fault(d, func(call, _ string) error {
			if call == "stat" {
				return syscall.ENOENT
			}
			return nil
		})
	}
	r := b.scan(Options{}).Disks[0]
	if !slices.Equal(r.Unreadable, []string{"a", "c", "k", "m", "z"}) {
		t.Errorf("unreadable: %v", r.Unreadable)
	}
}

// TestTheOriginalIsTheOldest: by creation time, and the first_seen of a copy with none stands in
// for it (specs.md §7). A scenario cannot pin this, as the creation time depends on the filesystem
// it runs on.
func TestTheOriginalIsTheOldest(t *testing.T) {
	position := map[string]int{"t1": 0, "t2": 1}
	state := map[string]solver.DiskState{"t1": solver.DiskOnline, "t2": solver.DiskOnline}
	row := func(btime1, btime2 sql.NullInt64) *catalog.FileRow {
		return &catalog.FileRow{Copies: []catalog.CopyRow{
			{Disk: "t1", State: "ok", BtimeNS: btime1, FirstSeenNS: 5},
			{Disk: "t2", State: "ok", BtimeNS: btime2, FirstSeenNS: 5},
		}}
	}
	at := func(ns int64) sql.NullInt64 { return sql.NullInt64{Int64: ns, Valid: true} }
	for _, tc := range []struct {
		f    *catalog.FileRow
		want string
	}{
		{row(at(2), at(1)), "t2"},
		{row(at(1), at(2)), "t1"},
		{row(sql.NullInt64{}, at(1)), "t2"},
		{row(sql.NullInt64{}, at(9)), "t1"},
		{row(sql.NullInt64{}, sql.NullInt64{}), "t1"},
	} {
		got, err := originalOf(tc.f, position, state)
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %q, %v, want %q", tc.f.Copies, got, err, tc.want)
		}
	}
	conflict := row(at(2), at(1))
	conflict.State, conflict.Original = "conflict", sql.NullString{String: "t1", Valid: true}
	if got, err := originalOf(conflict, position, state); err != nil || got != "t1" {
		t.Errorf("a conflict keeps its original: %q, %v", got, err)
	}
	moved := row(at(2), at(1))
	moved.Original = sql.NullString{String: "t1", Valid: true}
	if got, err := originalOf(moved, position, state); err != nil || got != "t2" {
		t.Errorf("outside a conflict the previous one is ignored: %q, %v", got, err)
	}
}

// TestAWalkWrittenWhole: what a failing transaction would have written is not there, and the next
// scan starts from what was.
func TestAWalkWrittenWhole(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	if _, err := b.db.ExecContext(t.Context(), `CREATE TRIGGER stop BEFORE INSERT ON entries BEGIN SELECT RAISE(ABORT, 'stop'); END`); err != nil {
		t.Fatal(err)
	}
	b.write("t1", "sub/b", "y")
	if _, err := Scan(t.Context(), b.db, b.config, b, Options{}); err == nil {
		t.Fatal("no error")
	}
	if got := b.count("SELECT count(*) FROM files"); got != 0 {
		t.Errorf("%d files of a transaction that failed", got)
	}
	if _, err := b.db.ExecContext(t.Context(), `DROP TRIGGER stop`); err != nil {
		t.Fatal(err)
	}
	if got := b.scan(Options{}).Disks[0].New; got != 2 {
		t.Errorf("%d new", got)
	}
}
