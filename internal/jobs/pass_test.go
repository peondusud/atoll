package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// bench is a pool of real folders, on a machine that says each is a disk, with a clock a test moves,
// and that can make any call on a disk fail, or stop at any point.
type bench struct {
	t      *testing.T
	db     *sql.DB
	config *config.Config
	dirs   map[string]string
	fail   map[string]func(call, name string) error
	// walkFail makes a call fail in a walk, which goes through its own handle.
	walkFail map[string]func(call, name string) error
	unread   map[string]bool
	ro       map[string]bool
	gone     map[string]bool
	at       map[string]func()
	// precision is how finely a disk holds a time, by id, as its head would say; 0 says nothing.
	precision map[string]int64
	points    []string
	now       int64
	// base is the configuration's text up to the tables a test adds after the disks.
	base string
}

func (b *bench) Mount(mount string) disks.MountSeen {
	if b.gone[mount] {
		return disks.MountSeen{Path: disks.PathOK}
	}
	options := []string{"rw"}
	if b.ro[mount] {
		options = []string{"ro"}
	}
	fsType, super := "ext4", options
	for i := range b.config.Disks {
		if d := &b.config.Disks[i]; d.Mount == mount && d.FS == names.NTFS {
			fsType, super = "ntfs3", append(slices.Clone(options), "iocharset=utf8")
		}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: fsType, MountOptions: options, SuperOptions: super,
	}}
}

func (b *bench) identity(entry *disks.MountEntry) disks.DeviceSeen {
	for i := range b.config.Disks {
		if b.config.Disks[i].Mount == entry.MountPoint {
			d := &b.config.Disks[i]
			return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: d.UUID, TimePrecisionNS: b.precision[d.ID]}}
		}
	}
	return disks.DeviceSeen{Device: disks.DeviceGoneAway}
}

func (b *bench) Device(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return b.identity(entry)
}

func (b *bench) ReadPastCache(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	if b.unread[entry.MountPoint] {
		return disks.DeviceSeen{Device: disks.DeviceUnreadable, Note: "EIO"}
	}
	return b.identity(entry)
}

func (b *bench) Root(string, string) disks.RootSeen { return disks.RootSeen{Folder: disks.FolderOK} }
func (b *bench) Now() int64                         { return b.now }
func (b *bench) Space(string) (capacity, free int64, err error) {
	return 1 << 30, 1 << 29, nil
}

func (b *bench) Checkpoint(point string) {
	b.points = append(b.points, point)
	if fire := b.at[point]; fire != nil {
		delete(b.at, point)
		fire()
	}
}

func (b *bench) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := fsx.OpenDir(filepath.Join(mount, root))
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	if fail := b.walkFail[mount]; fail != nil {
		return fsx.Fault(dir, fail), nil
	}
	return dir, nil
}

func (b *bench) OpenTop(mount string) (fsx.Dir, error) {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return fsx.Fault(top, func(call, name string) error {
		if fail := b.fail[mount]; fail != nil {
			return fail(call, name)
		}
		return nil
	}), nil
}

func newBench(t *testing.T, ids ...string) *bench {
	t.Helper()
	return newBenchWith(t, "", ids...)
}

// newBenchWith is newBench with more of the configuration: tables that come after the disks.
func newBenchWith(t *testing.T, more string, ids ...string) *bench {
	t.Helper()
	top := t.TempDir()
	var text strings.Builder
	fmt.Fprintf(&text, "pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", filepath.Join(top, "catalog.db"), filepath.Join(top, "pool"))
	b := &bench{t: t, dirs: map[string]string{}, fail: map[string]func(string, string) error{}, walkFail: map[string]func(string, string) error{}, unread: map[string]bool{}, ro: map[string]bool{},
		gone: map[string]bool{}, at: map[string]func(){}, precision: map[string]int64{}, now: 1767225600000000000}
	for i, id := range ids {
		dir := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(dir, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		b.dirs[id] = dir
		fmt.Fprintf(&text, "[[disk]]\nid = %q\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d\"\nmount = %q\nfs = \"ext4\"\n", id, i, dir)
	}
	text.WriteString("[deletes.mass_guard]\npercent = \"100%\"\n")
	b.base = text.String()
	text.WriteString(more)
	c, err := config.Parse([]byte(text.String()))
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
	b.db = db
	return b
}

func (b *bench) write(id, path, text string) {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) rename(id, from, to string) {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", to)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(b.dirs[id], "media", from), full); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) remove(id, path string) {
	b.t.Helper()
	if err := os.Remove(filepath.Join(b.dirs[id], "media", path)); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) exists(id, path string) bool {
	b.t.Helper()
	_, err := os.Lstat(filepath.Join(b.dirs[id], path))
	return err == nil
}

func (b *bench) count(query string, args ...any) int {
	b.t.Helper()
	var n int
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

func (b *bench) text(query string, args ...any) string {
	b.t.Helper()
	var s string
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&s); err != nil {
		b.t.Fatal(err)
	}
	return s
}

func (b *bench) exec(query string, args ...any) {
	b.t.Helper()
	if _, err := b.db.ExecContext(b.t.Context(), query, args...); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) scan(full bool) {
	b.t.Helper()
	if _, err := watch.Scan(b.t.Context(), b.db, b.config, b, watch.Options{Full: full}); err != nil {
		b.t.Fatal(err)
	}
	b.now += 60_000_000_000
}

func (b *bench) pass() *Outcome {
	b.t.Helper()
	out, err := Run(b.t.Context(), b.db, b.config, b, true)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// crashAt runs a pass that stops at a point, as kill -9 would: nothing after it runs, and nothing is
// cleaned up.
func (b *bench) crashAt(point string) {
	b.t.Helper()
	stop := errors.New("stopped")
	b.at[point] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
			b.t.Fatalf("it did not stop at %s: %v", point, err)
		}
	}()
	_, _ = Run(b.t.Context(), b.db, b.config, b, true)
}

// settle runs a pass, lets the debounce go by, and runs another.
func (b *bench) settle() *Outcome {
	b.t.Helper()
	b.pass()
	b.now += 30_000_000_000
	return b.pass()
}

func outcomeOf(out *Outcome, kind, path string) string {
	for i := range out.Operations {
		if op := &out.Operations[i]; op.Kind == kind && op.Path == path {
			return op.Outcome
		}
	}
	return ""
}

// pool gives a pool of two disks, each with a file that a --full has verified.
func pool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(true)
	return b
}

func TestARenameReachesTheOtherDisk(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "b")
	out := b.settle()
	if got := outcomeOf(out, KindRename, "a"); got != OutcomeRenamed {
		t.Fatalf("outcome %q", got)
	}
	if !b.exists("t2", "media/b") || b.exists("t2", "media/a") {
		t.Fatal("the other copy did not follow")
	}
	if got := b.text("SELECT state FROM ops"); got != "done" {
		t.Errorf("operation %s", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL OR gone_ns IS NOT NULL"); got != 0 {
		t.Errorf("%d rows still behind", got)
	}
}

// Every error from a step of a rename on a disk leaves what it should: the file is at one name at least,
// and an operation that may have happened stays planned on a disk that cannot be relied on.
func TestAFaultDuringARename(t *testing.T) {
	cases := []struct {
		name string
		call string
		err  error
		// what the pass says, and whether the disk may go on
		outcome string
		lost    bool
	}{
		{"the move, with EIO", "rename", syscall.EIO, "io-error", true},
		{"the move, out of room", "rename", syscall.ENOSPC, "io-error", true},
		{"the move, with no such file", "rename", syscall.ENOENT, "changed", false},
		{"the move, with a file there", "rename", syscall.EEXIST, "path-taken", false},
		{"the sync", "sync", syscall.EIO, "io-error", true},
		{"the sync of the file", "syncentry", syscall.EIO, "io-error", true},
		{"a look", "stat", syscall.EIO, "io-error", true},
		{"a look, on a device gone", "stat", syscall.ENODEV, "io-error", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := pool(t)
			b.rename("t1", "a", "b")
			b.pass()
			b.now += 30_000_000_000
			mount := b.dirs["t2"]
			armed := true
			b.fail[mount] = func(call, name string) error {
				if armed && call == tc.call && name != filepath.Base(mount) {
					return tc.err
				}
				return nil
			}
			out, err := Run(t.Context(), b.db, b.config, b, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomeOf(out, KindRename, "a"); got != tc.outcome {
				t.Errorf("outcome %q, want %q (%+v)", got, tc.outcome, out.Operations)
			}
			if out.Partial != tc.lost {
				t.Errorf("partial %v, want %v", out.Partial, tc.lost)
			}
			if !b.exists("t2", "media/a") && !b.exists("t2", "media/b") {
				t.Fatal("the copy has no name left")
			}
			// Back to normal, it ends as if nothing had happened.
			armed = false
			if out := b.pass(); out.Partial {
				t.Fatalf("still partial: %+v", out)
			}
			if !b.exists("t2", "media/b") || b.exists("t2", "media/a") {
				t.Fatal("the rename did not finish")
			}
			if got := b.count("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL"); got != 0 {
				t.Errorf("%d rows behind", got)
			}
		})
	}
}

// A crash at each point: what the next pass leaves is what a pass with no crash leaves.
func TestACrashAtEachPointOfARename(t *testing.T) {
	for _, point := range []string{"decided", "journaled", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := pool(t)
			b.rename("t1", "a", "b")
			b.pass()
			b.now += 30_000_000_000
			b.crashAt(point)
			b.pass()
			if !b.exists("t2", "media/b") || b.exists("t2", "media/a") {
				t.Fatal("the rename did not finish")
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL"); got != 0 {
				t.Errorf("%d rows behind", got)
			}
		})
	}
}

// recordSyncs records the syncs on one disk: a folder's by its name, and a file's or a folder's own as
// "entry" and the name.
func (b *bench) recordSyncs(id string) *[]string {
	syncs := &[]string{}
	b.fail[b.dirs[id]] = func(call, name string) error {
		switch call {
		case "sync":
			*syncs = append(*syncs, name)
		case "syncentry":
			*syncs = append(*syncs, "entry "+name)
		}
		return nil
	}
	return syncs
}

// A file or a folder renamed is synced at its new name before its folder: on NTFS its own record holds its
// names too, and ntfs3 does not write it when it syncs the folder, so that a device failing then left
// neither name (measured, specs.md §15.12, §17.1).
func TestTheMovedEntryIsSyncedBeforeItsFolder(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		kind           string
	}{
		{"a file", "a", "b", KindRename},
		{"a folder", "d", "e", KindFolder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			for _, id := range []string{"t1", "t2"} {
				b.write(id, "a", "x")
				b.write(id, "d/x", "x")
			}
			b.scan(true)
			b.rename("t1", tc.from, tc.to)
			b.pass()
			b.now += 30_000_000_000
			syncs := b.recordSyncs("t2")
			out := b.pass()
			if got := outcomeOf(out, tc.kind, tc.from); got != OutcomeRenamed {
				t.Fatalf("outcome %q (%+v)", got, out.Operations)
			}
			at := slices.Index(*syncs, "entry "+tc.to)
			if at < 0 || at+1 >= len(*syncs) || (*syncs)[at+1] != "media" {
				t.Errorf("syncs %q", *syncs)
			}
		})
	}
}

// A reprise that finds the move done syncs the file at its new name too, as the crash may have come before
// that sync (specs.md §17.2).
func TestAReprisesSyncsTheMovedEntry(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "b")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	syncs := b.recordSyncs("t2")
	b.pass()
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'done'"); got != 1 {
		t.Fatalf("%d operations done", got)
	}
	if !slices.Contains(*syncs, "entry b") {
		t.Errorf("syncs %q", *syncs)
	}
}

// A disk that fails its confirmation says nothing, whatever its walk found: an absence on it decides
// nothing, and its verdicts are unknown.
func TestAnAbsenceOnADiskThatFailsItsConfirmation(t *testing.T) {
	b := pool(t)
	b.remove("t1", "a")
	mount := b.dirs["t1"]
	b.unread[mount] = true
	out := b.settle()
	if !out.Partial {
		t.Error("not partial")
	}
	if len(out.Decisions) != 0 {
		t.Errorf("decided %+v", out.Decisions)
	}
	if !b.exists("t2", "media/a") {
		t.Fatal("the other copy left")
	}
	if got := b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL"); got != 0 {
		t.Errorf("%d clocks started on what the disk could not confirm", got)
	}
}

// A readonly disk is walked, and what it says counts once it is confirmed: its absence reaches the
// others, and a job for it waits.
func TestAReadonlyDisk(t *testing.T) {
	b := pool(t)
	b.ro[b.dirs["t2"]] = true
	b.remove("t1", "a")
	out := b.settle()
	if got := outcomeOf(out, KindTrash, "a"); got != OutcomeUnavailable {
		t.Errorf("trash on a readonly disk: %q", got)
	}
	if !out.Partial {
		t.Error("not partial")
	}
	if !b.exists("t2", "media/a") {
		t.Fatal("a readonly disk was written to")
	}
	b.ro[b.dirs["t2"]] = false
	if got := outcomeOf(b.pass(), KindTrash, "a"); got != "trashed" {
		t.Errorf("once writable: %q", got)
	}
}

func TestADeletionFromAReadonlyDisk(t *testing.T) {
	b := pool(t)
	b.ro[b.dirs["t1"]] = true
	b.unread[b.dirs["t1"]] = true
	b.remove("t1", "a")
	out := b.settle()
	if len(out.Decisions) != 0 {
		t.Fatalf("a readonly disk that fails its confirmation decided %+v", out.Decisions)
	}
	b.unread[b.dirs["t1"]] = false
	if out := b.settle(); len(out.Decisions) != 1 || out.Decisions[0].Kind != "deleted" {
		t.Fatalf("confirmed: %+v", out.Decisions)
	}
}

// A copy in the disk's own trash is not a user's deletion: its row goes, and nothing propagates.
func TestACopyFoundInItsOwnTrash(t *testing.T) {
	b := pool(t)
	trash := filepath.Join(b.dirs["t1"], "Atoll", "trash", "2026-01-01", "9")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(b.dirs["t1"], "media", "a"), filepath.Join(trash, "a")); err != nil {
		t.Fatal(err)
	}
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "kept" {
		t.Fatalf("decided %+v", out.Decisions)
	}
	if !b.exists("t2", "media/a") {
		t.Fatal("the other copy left")
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind IN ('copy-gone', 'copy-kept')"); got != 0 {
		t.Errorf("%d events for an operation of Atoll's", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1'"); got != 0 {
		t.Errorf("%d rows left", got)
	}
}

// A file with the inode of the copy, in the trash, is not that copy: an inode alone says nothing.
func TestAnotherFileWithTheInodeInItsOwnTrash(t *testing.T) {
	b := pool(t)
	if b.count("SELECT count(*) FROM copies WHERE btime_ns IS NULL") > 0 {
		t.Skip("this filesystem gives no creation time")
	}
	trash := filepath.Join(b.dirs["t1"], "Atoll", "trash", "2026-01-01", "9")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(b.dirs["t1"], "media", "a"), filepath.Join(trash, "a")); err != nil {
		t.Fatal(err)
	}
	// What the row says of the file is what a file that took its inode would not match.
	b.exec("UPDATE copies SET btime_ns = btime_ns + 1 WHERE disk_id = 't1'")
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "deleted" {
		t.Fatalf("decided %+v", out.Decisions)
	}
}

// What a walk cannot reach is not absent: a pass decides nothing from it, whatever goes by.
func TestWhatAWalkCannotReachIsNotGone(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "d/a", "x")
	b.write("t2", "d/a", "x")
	b.scan(true)
	mount := b.dirs["t1"]
	if err := os.RemoveAll(filepath.Join(mount, "media", "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(mount, "media", "e")); err != nil {
		t.Fatal(err)
	}
	if out := b.settle(); len(out.Decisions) != 1 {
		t.Fatalf("the file that is gone is decided about: %+v", out.Decisions)
	}
}

// A deletion's job, with each kind of fault: nothing leaves the library without its operation done.
func TestAFaultDuringADeletion(t *testing.T) {
	cases := []struct {
		name    string
		call    string
		err     error
		outcome string
	}{
		{"the move, with EIO", "rename", syscall.EIO, "io-error"},
		{"the move, out of room", "rename", syscall.ENOSPC, "io-error"},
		{"the sync", "sync", syscall.EIO, "io-error"},
		{"a folder of the trash", "mkdir", syscall.ENOSPC, "io-error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := pool(t)
			b.remove("t1", "a")
			b.pass()
			b.now += 30_000_000_000
			armed := true
			b.fail[b.dirs["t2"]] = func(call, name string) error {
				if armed && call == tc.call {
					return tc.err
				}
				return nil
			}
			out, err := Run(t.Context(), b.db, b.config, b, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomeOf(out, KindTrash, "a"); got != tc.outcome {
				t.Errorf("outcome %q, want %q", got, tc.outcome)
			}
			if !out.Partial {
				t.Error("not partial")
			}
			armed = false
			if got := outcomeOf(b.pass(), KindTrash, "a"); got != "trashed" && got != "" {
				t.Errorf("then: %q", got)
			}
			if b.exists("t2", "media/a") {
				t.Fatal("the job did not finish")
			}
			if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
				t.Errorf("%d jobs left", got)
			}
			if got := b.count("SELECT count(*) FROM files"); got != 0 {
				t.Errorf("%d files left", got)
			}
		})
	}
}

// A crash at each point of a deletion's job: the next pass ends as a pass with no crash does, with no
// row, no job and no file left behind.
func TestACrashAtEachPointOfADeletion(t *testing.T) {
	for _, point := range []string{"decided", "journaled", "trash-folders", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := pool(t)
			b.remove("t1", "a")
			b.pass()
			b.now += 30_000_000_000
			b.crashAt(point)
			b.pass()
			if b.exists("t2", "media/a") {
				t.Fatal("the deletion did not finish")
			}
			for _, table := range []string{"files", "copies", "jobs"} {
				if got := b.count("SELECT count(*) FROM " + table); got != 0 {
					t.Errorf("%d rows in %s", got, table)
				}
			}
			if got := b.count("SELECT count(*) FROM trash"); got != 1 {
				t.Errorf("%d entries in the trash", got)
			}
		})
	}
}

// The same state reached with a crash and without one ends the same way: a file imported again at the
// name while the job's operation was left planned is a new file, and stays.
func TestAReimportDuringAReprise(t *testing.T) {
	b := pool(t)
	b.remove("t1", "a")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	b.write("t2", "a", "x")
	b.pass()
	if !b.exists("t2", "media/a") {
		t.Fatal("the file imported again left")
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d jobs left", got)
	}
}

// A rename left planned, a disk away, and the user's next rename: the row says where the copy really is,
// and the next run renames it from there.
func TestARepriseAfterAnotherRename(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "b")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	b.gone[b.dirs["t2"]] = true
	b.rename("t1", "b", "c")
	b.settle()
	b.gone[b.dirs["t2"]] = false
	out := b.pass()
	if got := outcomeOf(out, KindRename, "b"); got != OutcomeRenamed {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/c") || b.exists("t2", "media/b") || b.exists("t2", "media/a") {
		t.Fatal("the copy is not at the user's last name")
	}
}

// The guard, once more than one thing counts: a copy a walk confirmed gone counts however old, and only
// a confirm clears it.
func TestAGuardHoldsOldAbsences(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.config = reconfigured(t, b, "files = 1\npercent = \"100%\"\n")
	b.scan(true)
	for _, name := range []string{"a", "b", "c"} {
		b.remove("t1", name)
	}
	b.pass()
	b.now += 365 * 24 * 3600 * 1_000_000_000
	out := b.pass()
	if !out.Held || len(out.Decisions) != 0 {
		t.Fatalf("a year later: %+v", out)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'guard'"); got != 1 {
		t.Errorf("%d guard events", got)
	}
}

// ntfsPool gives a pool whose second disk is NTFS, so that it refuses names the first accepts, with a file
// on both that a --full has verified and the first renamed to such a name. The refused rename has run.
func ntfsPool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.write("t1", "c", "y")
	b.write("t2", "c", "y")
	b.scan(true)
	b.rename("t1", "a", "a:b")
	out := b.settle()
	if got := outcomeOf(out, KindRename, "a"); got != "name-refused" {
		t.Fatalf("outcome %q (%+v)", got, out)
	}
	return b
}

// A refused copy goes only while the copy at the target can be relied on: a disk lost in this very pass
// says nothing any more.
func TestARefusedCopyWhoseGoodCopyIsLost(t *testing.T) {
	b := ntfsPool(t)
	b.rename("t2", "c", "d")
	b.pass()
	b.scan(true)
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "stat" && name == "c" {
			return syscall.EIO
		}
		return nil
	}
	out := b.pass()
	if !out.Partial {
		t.Errorf("not partial: %+v", out)
	}
	if !b.exists("t2", "media/a") {
		t.Fatalf("the copy went to the trash with its good copy lost: %+v", out.Operations)
	}
	if got := outcomeOf(out, KindTrash, "a"); got != OutcomeCanceled {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d jobs left", got)
	}
}

// A copy with a job does not ask again: RefusedCopies says what a guard command has yet to speak of.
func TestARefusedCopyWithAJobIsNotAskedAgain(t *testing.T) {
	b := ntfsPool(t)
	b.scan(true)
	list, err := RefusedCopies(t.Context(), b.db, &b.config.Disks[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Place != "a" {
		t.Fatalf("refused %+v", list)
	}
	if err := catalog.QueueRenamedJob(t.Context(), b.db, list[0].FileID, "t2"); err != nil {
		t.Fatal(err)
	}
	if list, err = RefusedCopies(t.Context(), b.db, &b.config.Disks[1]); err != nil || len(list) != 0 {
		t.Fatalf("with a job: %+v, %v", list, err)
	}
}

// An operation left planned on a disk that is only readable is Atoll's, so what a walk finds at its names
// is no user's rename: once the disk can be written to, the reprise finishes it and the user's name wins.
func TestAPlannedRenameOnAReadonlyDisk(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "b")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	b.ro[b.dirs["t2"]] = true
	b.rename("t1", "b", "c")
	b.settle()
	b.pass()
	if got := b.count("SELECT count(*) FROM files"); got != 1 {
		t.Fatalf("%d files for one", got)
	}
	if got := b.text("SELECT CAST(old_lpath AS TEXT) FROM copies WHERE disk_id = 't2'"); got != "a" {
		t.Errorf("the row says %q", got)
	}
	b.ro[b.dirs["t2"]] = false
	out := b.pass()
	if got := outcomeOf(out, KindRename, "b"); got != OutcomeRenamed {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/c") || b.exists("t2", "media/b") || b.exists("t2", "media/a") {
		t.Fatal("the copy is not at the user's last name")
	}
}

// A parent that cannot be made is an error of that disk, for now, and not a name taken: the exit code says
// that something failed, and the next pass makes it.
func TestAParentThatCannotBeMade(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "d/e/a")
	b.pass()
	b.now += 30_000_000_000
	mount := b.dirs["t2"]
	b.fail[mount] = func(call, _ string) error {
		if call == "mkdir" {
			return syscall.EACCES
		}
		return nil
	}
	out := b.pass()
	if got := outcomeOf(out, KindRename, "a"); got != "io-error" {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !out.Partial {
		t.Error("not partial")
	}
	b.fail[mount] = nil
	if got := outcomeOf(b.pass(), KindRename, "a"); got != OutcomeRenamed {
		t.Errorf("outcome %q", got)
	}
	if !b.exists("t2", "media/d/e/a") {
		t.Fatal("the rename did not reach the disk")
	}
}

// deletes.direct changed while a job waits: nothing propagates any more, and that job goes too.
func TestAJobWhenDeletesStopPropagating(t *testing.T) {
	b := pool(t)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.remove("t1", "a")
	b.settle()
	if got := b.count("SELECT count(*) FROM jobs WHERE state = 'queued'"); got != 1 {
		t.Fatalf("%d jobs", got)
	}
	b.gone[mount] = false
	b.config = reconfigured(t, b, "percent = \"100%\"\n[deletes]\ndirect = \"restore\"\n")
	out := b.pass()
	if got := outcomeOf(out, KindTrash, "a"); got != OutcomeCanceled {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/a") {
		t.Fatal("the copy went to the trash")
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d jobs left", got)
	}
}

// A job the guard holds is held on a disk that cannot be written to as well: nothing is missing.
func TestAJobHeldOnAReadonlyDisk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.config = reconfigured(t, b, "files = 1\npercent = \"100%\"\n")
	b.scan(true)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.remove("t1", "a")
	b.settle()
	b.gone[mount] = false
	b.ro[mount] = true
	b.remove("t1", "b")
	out := b.pass()
	if got := outcomeOf(out, KindTrash, "a"); got != OutcomeHeld {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if out.Partial || !out.Held {
		t.Errorf("partial %v, held %v", out.Partial, out.Held)
	}
}

// Each sync of a rename says so when it fails, whichever it is: a failing disk does not go on, and the
// next pass ends as if nothing had happened.
func TestEachSyncOfARename(t *testing.T) {
	for nth := 1; ; nth++ {
		b := pool(t)
		b.rename("t1", "a", "b")
		b.pass()
		b.now += 30_000_000_000
		mount := b.dirs["t2"]
		syncs := 0
		b.fail[mount] = func(call, _ string) error {
			if call == "sync" {
				syncs++
				if syncs == nth {
					return syscall.EIO
				}
			}
			return nil
		}
		out := b.pass()
		if syncs < nth {
			if nth < 3 {
				t.Fatalf("only %d syncs", syncs)
			}
			return
		}
		if !out.Partial {
			t.Errorf("sync %d: not partial (%+v)", nth, out.Operations)
		}
		b.fail[mount] = nil
		if out := b.pass(); out.Partial {
			t.Errorf("sync %d: still partial (%+v)", nth, out)
		}
		if !b.exists("t2", "media/b") || b.exists("t2", "media/a") {
			t.Errorf("sync %d: the rename did not finish", nth)
		}
	}
}

// --full names a copy where it sits, also one behind its file's rename.
func TestAFullScanNamesACopyBehindWhereItSits(t *testing.T) {
	b := pool(t)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "a", "b")
	b.settle()
	b.gone[mount] = false
	// What only a read can tell: the copy is not what the file's hash says.
	b.exec("UPDATE files SET blake3 = zeroblob(32)")
	out, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Disks[1].Changed; len(got) != 1 || got[0] != "a" {
		t.Errorf("changed %q", got)
	}
}

// A copy edited and renamed follows, and keeps the change time its row had, so that the edit still reads as
// one.
func TestARenamedCopyEditedKeepsItsChangeTime(t *testing.T) {
	b := pool(t)
	before := b.count("SELECT ctime_ns FROM copies WHERE disk_id = 't1'")
	b.rename("t1", "a", "b")
	b.write("t1", "b", "longer")
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "renamed" {
		t.Skipf("no creation time here: %+v", out.Decisions)
	}
	if got := b.count("SELECT ctime_ns FROM copies WHERE disk_id = 't1'"); got != before {
		t.Errorf("change time %d, was %d", got, before)
	}
}

// What a guard counts from events alone holds nothing: what they counted has gone, so the exit code does
// not say that something waits for a confirm.
func TestAGuardTrippedByEventsAlone(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(true)
	for _, name := range []string{"a", "b", "c"} {
		b.remove("t1", name)
	}
	b.settle()
	b.config = reconfigured(t, b, "files = 1\npercent = \"100%\"\n")
	out := b.pass()
	if len(out.Guards) != 1 || out.Held {
		t.Errorf("guards %+v, held %v", out.Guards, out.Held)
	}
	// A rename on that disk holds nothing either.
	b.write("t1", "n", "x")
	b.write("t2", "n", "x")
	b.pass()
	b.now += 60_000_000_000
	b.rename("t1", "n", "m")
	b.pass()
	b.now += 30_000_000_000
	out = b.pass()
	if len(out.Guards) != 1 || out.Held || len(out.Decisions) != 1 {
		t.Errorf("guards %+v, held %v, decided %+v", out.Guards, out.Held, out.Decisions)
	}
}

// A queued job for a refused copy is looked at when it runs, as it may have waited long.
func TestAQueuedRefusedJobThatNoLongerStands(t *testing.T) {
	cases := []struct {
		name   string
		change string
	}{
		{"a file in conflict", "UPDATE files SET state = 'conflict'"},
		{"a corrupt file", "UPDATE files SET state = 'corrupt'"},
		{"the copy no longer verified", "UPDATE copies SET verified_ns = NULL WHERE disk_id = 't2'"},
		{"the good copy no longer verified", "UPDATE copies SET verified_ns = NULL WHERE disk_id = 't1'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := ntfsPool(t)
			b.scan(true)
			id := b.count("SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'a:b'")
			if err := catalog.QueueRenamedJob(t.Context(), b.db, int64(id), "t2"); err != nil {
				t.Fatal(err)
			}
			b.exec(tc.change)
			out := b.pass()
			if got := outcomeOf(out, KindTrash, "a"); got != OutcomeCanceled {
				t.Errorf("outcome %q (%+v)", got, out.Operations)
			}
			if !b.exists("t2", "media/a") {
				t.Fatal("the copy went to the trash")
			}
			if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
				t.Errorf("%d jobs left", got)
			}
		})
	}
}

// A copy that has not followed the rename either is not at the target, verified or not.
func TestAQueuedRefusedJobWithOnlyACopyBehind(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.config.Disks[1].FS = names.NTFS
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	b.gone[b.dirs["t3"]] = true
	b.rename("t1", "a", "a:b")
	b.settle()
	b.gone[b.dirs["t3"]] = false
	b.write("t3", "a:b", "zz")
	id := b.count("SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'a:b'")
	if err := catalog.QueueRenamedJob(t.Context(), b.db, int64(id), "t2"); err != nil {
		t.Fatal(err)
	}
	out := b.pass()
	if got := outcomeOf(out, KindRename, "a"); got == "" {
		t.Errorf("nothing said of the renames: %+v", out.Operations)
	}
	if got := outcomeOf(out, KindTrash, "a"); got != OutcomeCanceled {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/a") {
		t.Fatal("the copy went to the trash")
	}
}

// A file in conflict no longer has its deletion: any copy may be the good one.
func TestADeletionJobOfAFileInConflict(t *testing.T) {
	b := pool(t)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.remove("t1", "a")
	b.settle()
	b.gone[mount] = false
	b.exec("UPDATE files SET state = 'conflict'")
	out := b.pass()
	if got := outcomeOf(out, KindTrash, "a"); got != OutcomeCanceled {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/a") {
		t.Fatal("the copy went to the trash")
	}
}

// A folder move that fails for now keeps its job, and holds what it will carry: on an NTFS disk a case-only
// rename reads as refused for each copy below it, which must not send them to the trash.
func TestAFolderMoveThatFailsKeepsItsJob(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	b.write("t1", "d/x", "x")
	b.write("t2", "d/x", "x")
	b.scan(true)
	b.rename("t1", "d", "D")
	b.pass()
	b.now += 30_000_000_000
	mount := b.dirs["t2"]
	b.fail[mount] = func(call, _ string) error {
		if call == "rename" {
			return syscall.ENOSPC
		}
		return nil
	}
	for try := range 2 {
		out := b.pass()
		if got := outcomeOf(out, KindFolder, "d"); got != "io-error" {
			t.Errorf("try %d: outcome %q (%+v)", try, got, out.Operations)
		}
		if !out.Partial {
			t.Errorf("try %d: not partial", try)
		}
		if got := outcomeOf(out, KindTrash, "d/x"); got != "" {
			t.Errorf("try %d: trash %q", try, got)
		}
		if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename'"); got != 1 {
			t.Errorf("try %d: %d folder jobs", try, got)
		}
		if !b.exists("t2", "media/d/x") {
			t.Fatalf("try %d: the copy left", try)
		}
	}
	b.fail[mount] = nil
	out := b.pass()
	if got := outcomeOf(out, KindFolder, "d"); got != OutcomeRenamed {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/D/x") {
		t.Fatal("the folder did not move")
	}
}

// A copy below a job that waits does not go one by one, as that would make the job's target and end it.
func TestACopyBelowAJobThatWaits(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "d/x", "x")
	b.write("t2", "d/x", "x")
	b.scan(false)
	b.rename("t1", "d", "e")
	b.pass()
	b.now += 30_000_000_000
	mount := b.dirs["t2"]
	b.fail[mount] = func(call, name string) error {
		if call == "rename" && name == "e" {
			return syscall.ENOSPC
		}
		return nil
	}
	out := b.pass()
	if got := outcomeOf(out, KindRename, "d/x"); got != "" {
		t.Errorf("the copy went on its own: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/d/x") || b.exists("t2", "media/e") {
		t.Fatal("the job's target was made")
	}
	b.fail[mount] = nil
	if got := outcomeOf(b.pass(), KindFolder, "d"); got != OutcomeRenamed {
		t.Errorf("outcome %q", got)
	}
}

// A planned rename whose old name holds something new is no more a user's rename than one whose old name
// holds nothing.
func TestAPlannedRenameWithANewFileAtItsOldName(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "b")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	b.write("t2", "a", "new")
	b.ro[b.dirs["t2"]] = true
	b.rename("t1", "b", "c")
	b.settle()
	b.pass()
	if got := b.count("SELECT count(*) FROM files"); got != 1 {
		t.Errorf("%d files for one", got)
	}
}

// A file on a page after what holds its target is told of once, however the page ends. So is a file pulled
// into an earlier page by what wanted its name.
func TestAChainAcrossAPage(t *testing.T) {
	cases := []struct {
		name   string
		apply  bool
		blocks bool
	}{
		{"a dry run", false, false},
		{"a real pass, with the chain's end taken", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			for i := range 1002 {
				name := fmt.Sprintf("y%04d", i)
				b.write("t1", name, "x")
				b.write("t2", name, "x")
			}
			if tc.blocks {
				b.write("t2", "zz", "other")
			}
			b.scan(false)
			b.rename("t1", "y1000", "zz")
			b.rename("t1", "y0999", "y1000")
			for i := range 999 {
				b.rename("t1", fmt.Sprintf("y%04d", i), fmt.Sprintf("w%04d", i))
			}
			// On the second page, and wanting a name on the first: a dry run's rows do not say that it left.
			b.rename("t1", "y1001", "y0000")
			b.pass()
			b.now += 30_000_000_000
			out, err := Run(t.Context(), b.db, b.config, b, tc.apply)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for i := range out.Decisions {
				seen[out.Decisions[i].Path]++
			}
			for _, path := range []string{"y0000", "y0999", "y1000", "y1001"} {
				if seen[path] != 1 {
					t.Errorf("%s told of %d times", path, seen[path])
				}
			}
		})
	}
}

// A failure of the catalog is no failure of the disk: the pass stops, and exits with 1.
func TestACatalogFailureMakingAParent(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "d/a")
	b.pass()
	b.now += 30_000_000_000
	b.exec("CREATE TRIGGER no_mkdir BEFORE INSERT ON ops WHEN NEW.kind = 'mkdir' BEGIN SELECT RAISE(ABORT, 'no'); END")
	if _, err := Run(t.Context(), b.db, b.config, b, true); err == nil {
		t.Fatal("no error")
	}
}

// A missing parent is made like the folder the file's own copy sits in, and not like any folder of that
// name.
func TestAParentIsMadeLikeTheFolderOfACopy(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "E"), 0o700); err != nil {
		t.Fatal(err)
	}
	b.write("t2", "a", "x")
	b.write("t3", "a", "x")
	b.scan(false)
	b.rename("t2", "a", "E/x")
	b.settle()
	st, err := os.Stat(filepath.Join(b.dirs["t3"], "media", "E"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o755 {
		t.Errorf("mode %o", got)
	}
}

// A folder that cannot be looked at gives no mode, so the parent waits: made 0755, a private folder would
// be open on the other disk for good.
func TestAParentWhoseModelCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"refused", syscall.EACCES}, {"on a disk that fails", syscall.EIO}} {
		t.Run(tc.name, func(t *testing.T) {
			b := pool(t)
			b.rename("t1", "a", "P/a")
			if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "P"), 0o700); err != nil {
				t.Fatal(err)
			}
			b.pass()
			b.now += 30_000_000_000
			mount := b.dirs["t1"]
			b.fail[mount] = func(call, name string) error {
				if call == "stat" && name == "P" {
					return tc.err
				}
				return nil
			}
			out := b.pass()
			if got := outcomeOf(out, KindRename, "a"); got != "io-error" {
				t.Errorf("outcome %q (%+v)", got, out.Operations)
			}
			if b.exists("t2", "media/P") {
				t.Fatal("the parent was made with no model")
			}
			b.fail[mount] = nil
			b.pass()
			st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "P"))
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Mode().Perm(); got != 0o700 {
				t.Errorf("mode %o", got)
			}
		})
	}
}

// A folder's rename left planned on a disk that is only readable: its rows stay, as only the reprise can
// tell of those names, and they follow once it can.
func TestAPlannedFolderRenameOnAReadonlyDisk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "d/s/x", "x")
	b.write("t2", "d/s/x", "x")
	b.scan(false)
	b.rename("t1", "d", "e")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	b.ro[b.dirs["t2"]] = true
	b.pass()
	entries := "SELECT count(*) FROM entries WHERE disk_id = 't2' AND CAST(lpath AS TEXT) = ?"
	if got := b.count(entries, "d"); got != 1 {
		t.Errorf("%d rows at the old name", got)
	}
	if got := b.count(entries, "e") + b.count(entries, "e/s"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
	if got := b.count(entries, "d/s"); got != 1 {
		t.Errorf("%d rows below the old name", got)
	}
	b.ro[b.dirs["t2"]] = false
	b.pass()
	if got := b.count(entries, "e"); got != 1 {
		t.Errorf("%d rows at the new name, once it could finish", got)
	}
	if got := b.count(entries, "d"); got != 0 {
		t.Errorf("%d rows at the old name, once it could finish", got)
	}
}

// A dry run says of a folder's job what a real pass says when a look fails, and what it would carry waits
// with it.
func TestADryRunWhoseFolderLookFails(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "d/x", "x")
	b.write("t2", "d/x", "x")
	b.scan(false)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "d", "e")
	b.settle()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "stat" && name == "e" {
			return syscall.EACCES
		}
		return nil
	}
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(out, KindFolder, "d"); got != "io-error" {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if got := outcomeOf(out, KindRename, "d/x"); got != "" {
		t.Errorf("the copy: %q", got)
	}
}

// What a walk did not reach keeps what its row says, also for a dry run's guard.
func TestADryRunOfAGuardOverWhatAWalkCannotReach(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"s/a", "s/b", "s/c", "s/d"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.config = reconfigured(t, b, "files = 1\npercent = \"100%\"\n")
	b.scan(true)
	for _, name := range []string{"s/a", "s/b", "s/c"} {
		b.remove("t1", name)
	}
	b.pass()
	// The walk no longer reaches them: three rows say that they went, and one does not.
	b.walkFail[b.dirs["t1"]] = func(call, name string) error {
		if call == "opendir" && name == "s" {
			return syscall.ELOOP // a name it cannot reach, and not a disk that fails
		}
		return nil
	}
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	real := b.pass()
	if len(dry.Guards) != 1 || len(real.Guards) != 1 || dry.Guards[0].Count != real.Guards[0].Count {
		t.Errorf("a dry run says %+v, and a real pass %+v", dry.Guards, real.Guards)
	}
}

// A dry run writes nothing, also when a walk fails.
func TestADryRunThatFailsToWalkWritesNoState(t *testing.T) {
	b := pool(t)
	media := filepath.Join(b.dirs["t2"], "media")
	if err := os.Rename(media, media+".away"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), b.db, b.config, b, false); err != nil {
		t.Fatal(err)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "online" {
		t.Errorf("state %s", got)
	}
}

// A folder whose name fits a branch, and what is below it does not: a dry run says so as a real pass does,
// from what it has decided.
func TestADryRunOfAFolderThatDoesNotFit(t *testing.T) {
	b := newBench(t, "t1", "t2")
	longer := filepath.Join(b.dirs["t2"], strings.Repeat("m", 100))
	if err := os.MkdirAll(filepath.Join(longer, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.dirs["t2"], b.config.Disks[1].Mount = longer, longer
	name := strings.Repeat("x", 150)
	b.write("t1", "d/"+name, "x")
	b.write("t2", "d/"+name, "x")
	b.scan(false)
	room := names.MaxBranchPathBytes - len(b.dirs["t1"]) - len("/media/") - 1 - len(name)
	var deep strings.Builder
	for deep.Len()+201 < room {
		deep.WriteString(strings.Repeat("e", 200) + "/")
	}
	deep.WriteString(strings.Repeat("e", room-deep.Len()))
	target := deep.String()
	if err := os.MkdirAll(filepath.Join(b.dirs["t1"], "media", filepath.Dir(target)), 0o755); err != nil {
		t.Skipf("this filesystem takes no path that long: %v", err)
	}
	b.rename("t1", "d", target)
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(out, KindRename, "d/"+name); got != "name-refused" {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if got := outcomeOf(out, KindFolder, "d"); got != "" {
		t.Errorf("the folder: %q", got)
	}
	// A real pass says the same of the copy behind.
	real := b.settle()
	if got := outcomeOf(real, KindRename, "d/"+name); got != "name-refused" {
		t.Errorf("a real pass: %q (%+v)", got, real.Operations)
	}
	if got := outcomeOf(real, KindFolder, "d"); got != "" {
		t.Errorf("a real pass, the folder: %q", got)
	}
	if !b.exists("t2", "media/d/"+name) {
		t.Fatal("the copy went past what its branch can reach")
	}
}

// A name a disk's table refuses is refused before anything asks that disk about it: a driver can answer a
// look at such a name with an error, and that must not read as a disk that fails.
func TestAFolderNameRefusedThatCannotBeLookedAt(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	b.write("t1", "d/x", "x")
	b.write("t2", "d/x", "x")
	b.scan(false)
	b.rename("t1", "d", "a:b")
	b.pass()
	b.now += 30_000_000_000
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "stat" && name == "a:b" {
			return syscall.EINVAL
		}
		return nil
	}
	out := b.pass()
	if got := outcomeOf(out, KindFolder, "d"); got != "" {
		t.Errorf("the folder: %q (%+v)", got, out.Operations)
	}
	if got := outcomeOf(out, KindRename, "d/x"); got != "name-refused" {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if out.Partial {
		t.Error("partial")
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename'"); got != 0 {
		t.Errorf("%d folder jobs", got)
	}
}

// A disk whose own folders cannot be opened is that disk's trouble: the others go on.
func TestAnOwnFolderThatCannotBeOpened(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	b.write("t2", "c", "y")
	b.write("t3", "c", "y")
	b.scan(true)
	if err := os.MkdirAll(filepath.Join(b.dirs["t1"], "Atoll", "trash"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.remove("t1", "a")
	b.rename("t2", "c", "d")
	b.pass()
	b.now += 30_000_000_000
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "opendir" && name == "trash" {
			return syscall.EACCES
		}
		return nil
	}
	out, err := Run(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatalf("the pass stopped: %v", err)
	}
	if !out.Partial {
		t.Error("not partial")
	}
	if got := outcomeOf(out, KindRename, "c"); got != OutcomeRenamed {
		t.Errorf("the other disks: %q (%+v)", got, out.Operations)
	}
}

// A file renamed while the walk goes, from a folder it has read to one it has not: two names with one link
// are one file. The new name gets no row, so the next look reads the rename, and no deletion comes of it.
func TestARenameAWalkStraddles(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "A/x", "x")
	b.write("t2", "A/x", "x")
	b.write("t1", "B/o", "o")
	b.write("t2", "B/o", "o")
	b.scan(true)
	mount := b.dirs["t1"]
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "opendir" && name == "B" && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", "A", "x"), filepath.Join(mount, "media", "B", "x")); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'B/x'"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
	delete(b.walkFail, mount)
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "renamed" || out.Decisions[0].To != "B/x" {
		t.Errorf("decided %+v", out.Decisions)
	}
	if !b.exists("t2", "media/B/x") {
		t.Fatal("the other copy did not follow")
	}
}

// The same, with the rename between the walk's reading of a folder and its look at that name.
func TestARenameAWalkStraddlesAtItsLook(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "A/x", "x")
	b.write("t2", "A/x", "x")
	b.write("t1", "B/o", "o")
	b.write("t2", "B/o", "o")
	b.scan(true)
	mount := b.dirs["t1"]
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "stat" && name == "x" && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", "A", "x"), filepath.Join(mount, "media", "B", "x")); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'B/x'"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
	delete(b.walkFail, mount)
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "renamed" || out.Decisions[0].To != "B/x" {
		t.Errorf("decided %+v", out.Decisions)
	}
}

// A file moved to a name an operation left planned covers reads as moved, not gone.
func TestAFileMovedBelowAPlannedName(t *testing.T) {
	b := pool(t)
	b.exec("INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES ('t2', 'mkdir', CAST('P' AS BLOB), '', 1, 'planned')")
	b.ro[b.dirs["t2"]] = true
	b.rename("t2", "a", "P/a")
	out := b.settle()
	if got := outcomeOf(out, KindTrash, "a"); got != "" {
		t.Errorf("the other copy: %q (%+v)", got, out)
	}
	if !b.exists("t1", "media/a") && !b.exists("t1", "media/P/a") {
		t.Fatal("the other copy left")
	}
}

// A dry run reads a file it decided as still there when it did not leave, across its pages.
func TestADryRunOfAFileThatStayed(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for i := range 1002 {
		name := fmt.Sprintf("y%04d", i)
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.write("t2", "zz", "other")
	b.scan(false)
	b.rename("t1", "y0000", "zz")
	b.rename("t1", "y1001", "y0000")
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for i := range out.Decisions {
		kinds[out.Decisions[i].Path] = out.Decisions[i].Kind
	}
	for _, path := range []string{"y0000", "y1001"} {
		if kinds[path] != "path-taken" {
			t.Errorf("%s: %q", path, kinds[path])
		}
	}
}

// A copy says nothing of itself: what a dry run counts toward the share is what a real pass does.
func TestADryRunOfAGuardOverACopyNothingElseHolds(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "s/a", "x")
	b.write("t2", "k", "y")
	b.config = reconfigured(t, b, "percent = \"5%\"\n")
	b.scan(true)
	b.remove("t1", "s/a")
	b.pass()
	b.walkFail[b.dirs["t1"]] = func(call, name string) error {
		if call == "opendir" && name == "s" {
			return syscall.ELOOP // a name it cannot reach, and not a disk that fails
		}
		return nil
	}
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	real := b.pass()
	if len(dry.Guards) != len(real.Guards) {
		t.Errorf("a dry run says %+v, and a real pass %+v", dry.Guards, real.Guards)
	}
}

// A file moved onto a name an operation left planned holds: that row says nothing of what else came there, and
// the row the file left reads as moved, never as gone.
func TestAFileMovedOntoAPlannedName(t *testing.T) {
	b := pool(t)
	b.write("t1", "x", "y")
	b.write("t2", "x", "y")
	b.scan(true)
	b.rename("t1", "a", "c")
	b.pass()
	b.now += 30_000_000_000
	b.crashAt("moved")
	b.ro[b.dirs["t2"]] = true
	b.rename("t2", "x", "a")
	out := b.settle()
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'copy-gone'"); got != 0 {
		t.Errorf("%d copies gone", got)
	}
	if got := outcomeOf(out, KindTrash, "x"); got != "" {
		t.Errorf("the other copy: %q (%+v)", got, out.Operations)
	}
	if !slices.ContainsFunc(out.Decisions, func(d Decision) bool { return d.Path == "x" && d.Kind == "renamed" && d.To == "a" }) {
		t.Errorf("decided %+v", out.Decisions)
	}
}

// A disk lost to a look at a model says nothing of the next folder either: made with no model, it would be
// open there for good.
func TestAParentWhoseModelWasLost(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(true)
	b.rename("t1", "a", "P/a")
	b.rename("t1", "b", "P/b")
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "P"), 0o700); err != nil {
		t.Fatal(err)
	}
	b.pass()
	b.now += 30_000_000_000
	mount := b.dirs["t1"]
	b.fail[mount] = func(call, name string) error {
		if call == "stat" && name == "P" {
			return syscall.EIO
		}
		return nil
	}
	b.pass()
	if b.exists("t2", "media/P") {
		t.Fatal("the parent was made with no model")
	}
	b.fail[mount] = nil
	b.pass()
	st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "P"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("mode %o", got)
	}
}

// A dry run says of a disk it cannot rely on what a real pass says: not walked, and nothing concluded from it.
func TestADryRunOfAnOwnFolderThatCannotBeOpened(t *testing.T) {
	b := pool(t)
	if err := os.MkdirAll(filepath.Join(b.dirs["t1"], "Atoll", "trash"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.remove("t1", "a")
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "opendir" && name == "trash" {
			return syscall.EACCES
		}
		return nil
	}
	out, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Disks[0].Walked {
		t.Errorf("walked: %+v", out.Disks[0])
	}
	if len(out.Decisions) != 0 || len(out.Operations) != 0 {
		t.Errorf("concluded %+v and %+v", out.Decisions, out.Operations)
	}
}

// A deletion decided over a copy the walk could not reach: a dry run says what a real pass will find.
func TestADryRunOfADeletionOverACopyNotReached(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "s/a", "x")
	b.write("t2", "s/a", "x")
	b.scan(true)
	b.remove("t1", "s/a")
	mount := b.dirs["t2"]
	failing := func(err error) func(call, name string) error {
		return func(_, name string) error {
			if name == "s" {
				return err
			}
			return nil
		}
	}
	// A name the walk cannot reach, which its own handle then finds gone.
	b.walkFail[mount], b.fail[mount] = failing(syscall.ELOOP), failing(syscall.ENOENT)
	b.pass()
	b.now += 30_000_000_000
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(dry, KindTrash, "s/a"); got != "changed" {
		t.Errorf("a dry run: %q (%+v)", got, dry.Operations)
	}
	real := b.pass()
	if got := outcomeOf(real, KindTrash, "s/a"); got != "changed" {
		t.Errorf("a real pass: %q (%+v)", got, real.Operations)
	}
	if !b.exists("t2", "media/s/a") {
		t.Fatal("the copy went")
	}
}

// A job queued while a target was truly refused, when a folder renamed since leaves a target that differs only
// as NTFS reads one name: only a folder's operation could rename that copy, so it stays, and never goes to the
// trash (specs.md §15.3).
func TestAQueuedRefusedJobThatOnlyAFolderCanRename(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.config.Disks[1].FS = names.NTFS
	b.write("t1", "X:Y/g", "g")
	b.write("t2", "Show/a", "x")
	b.write("t3", "Show/a", "x")
	b.scan(true)
	b.rename("t3", "Show/a", "X:Y/a")
	b.settle()
	b.scan(true)
	id := b.count("SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'X:Y/a'")
	if err := catalog.QueueRenamedJob(t.Context(), b.db, int64(id), "t2"); err != nil {
		t.Fatal(err)
	}
	b.gone[b.dirs["t2"]] = true
	b.rename("t1", "X:Y", "SHOW")
	b.settle()
	b.gone[b.dirs["t2"]] = false
	out := b.pass()
	if got := outcomeOf(out, KindRename, "Show/a"); got != "name-refused" {
		t.Errorf("the rename: %q (%+v)", got, out.Operations)
	}
	if got := outcomeOf(out, KindTrash, "Show/a"); got != OutcomeCanceled {
		t.Errorf("the job: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/Show/a") {
		t.Fatal("the copy went to the trash")
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d jobs left", got)
	}
}

// The same with no job yet: what the guard was told comes from before the decisions, and the target has moved
// since.
func TestAWishOnACopyOnlyAFolderCanRenameNow(t *testing.T) {
	b := newBench(t, "t3", "t2", "t1")
	b.config.Disks[1].FS = names.NTFS
	b.write("t3", "Show/x", "x")
	b.write("t2", "Show/x", "x")
	b.write("t1", "Show/g", "g")
	b.scan(true)
	b.rename("t1", "Show", "Sh:ow")
	b.settle()
	b.gone[b.dirs["t3"]] = true
	b.rename("t1", "Sh:ow", "show")
	b.pass()
	b.gone[b.dirs["t3"]] = false
	b.now += 30_000_000_000
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(dry, KindTrash, "Show/x"); got != "" {
		t.Errorf("a dry run says of the trash: %q (%+v)", got, dry.Operations)
	}
	out := b.pass()
	if got := outcomeOf(out, KindRename, "Show/x"); got != "name-refused" {
		t.Errorf("the rename: %q (%+v)", got, out.Operations)
	}
	if got := outcomeOf(out, KindTrash, "Show/x"); got != "" {
		t.Errorf("the trash: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/Show/x") {
		t.Fatal("the copy went to the trash")
	}
	list, err := RefusedCopies(t.Context(), b.db, &b.config.Disks[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("refused %+v", list)
	}
}

// What a page says may be gone by the time its turn comes, as a guard command takes no lock.
func TestARefusedCopyGoneSinceItsPageWasRead(t *testing.T) {
	b := ntfsPool(t)
	b.rename("t1", "c", "c:d")
	b.settle()
	seen := 0
	err := eachRefused(t.Context(), b.db, &b.config.Disks[1], nil, func(r *catalog.RenameRow, f *catalog.FileRow) {
		seen++
		if !slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == "t2" }) {
			t.Errorf("%s has no copy there", f.Path)
		}
		b.exec("DELETE FROM copies WHERE disk_id = 't2' AND file_id <> ?", r.FileID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("%d copies", seen)
	}
}

// A disk lost while it is worked on: what it had left is listed as for a disk lost before its turn, and not as
// an error of each look on a handle that is closed.
func TestADiskLostWhileItIsWorkedOn(t *testing.T) {
	t.Run("at a folder", func(t *testing.T) {
		b := newBench(t, "t1", "t2")
		for _, name := range []string{"d/x", "a"} {
			b.write("t1", name, "x")
			b.write("t2", name, "x")
		}
		b.scan(false)
		b.rename("t1", "d", "e")
		b.rename("t1", "a", "b")
		b.pass()
		b.now += 30_000_000_000
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "stat" && name == "d" {
				return syscall.EIO
			}
			return nil
		}
		out := b.pass()
		if got := outcomeOf(out, KindFolder, "d"); got != OutcomeUnavailable {
			t.Errorf("the folder: %q (%+v)", got, out.Operations)
		}
		if got := outcomeOf(out, KindRename, "a"); got != OutcomeUnavailable {
			t.Errorf("the rename: %q (%+v)", got, out.Operations)
		}
	})
	t.Run("in a round", func(t *testing.T) {
		b := newBench(t, "t1", "t2")
		list := []string{"a", "c", "e"}
		for _, name := range list {
			b.write("t1", name, "x")
			b.write("t2", name, "x")
		}
		b.scan(false)
		for _, name := range list {
			b.rename("t1", name, name+"2")
		}
		b.pass()
		b.now += 30_000_000_000
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			if call == "stat" && name == "c" {
				return syscall.EIO
			}
			return nil
		}
		out := b.pass()
		for name, want := range map[string]string{"a": OutcomeRenamed, "c": "io-error", "e": OutcomeUnavailable} {
			if got := outcomeOf(out, KindRename, name); got != want {
				t.Errorf("%s: %q (%+v)", name, got, out.Operations)
			}
		}
	})
}

// A folder renamed after its parent was listed and before the walk reached it: its row stays for that walk, so
// the next look finds the rename, and the other disk gets its one operation.
func TestAFolderRenamedDuringItsWalk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		later bool
	}{
		{"seen by the next look", false},
		{"found later in the same walk", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			from, to := "d", "e"
			if tc.later {
				first, second := walkOrder(t, b, "t1", "t2")
				from, to = first+"/d", second+"/e"
			}
			b.write("t1", from+"/x", "x")
			b.write("t2", from+"/x", "x")
			b.write("t2", from+"/y", "y")
			b.scan(false)
			mount := b.dirs["t1"]
			base := filepath.Base(from)
			moved := false
			b.walkFail[mount] = func(call, name string) error {
				if call == "stat" && name == base && !moved {
					moved = true
					if err := os.Rename(filepath.Join(mount, "media", from), filepath.Join(mount, "media", to)); err != nil {
						t.Error(err)
					}
				}
				return nil
			}
			b.pass()
			if !moved {
				t.Fatal("the walk did not get there")
			}
			delete(b.walkFail, mount)
			out := b.settle()
			if got := outcomeOf(out, KindFolder, from); got != OutcomeRenamed {
				t.Errorf("the folder: %q (%+v, %+v)", got, out.Decisions, out.Operations)
			}
			if !b.exists("t2", "media/"+to+"/y") {
				t.Fatal("what only the other disk held stayed behind")
			}
		})
	}
}

// A file decided path-taken on one page, and wanted by a file on a later one: it stays, and is told of once.
func TestAFileThatStayedIsToldOfOnce(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for i := range 1002 {
		name := fmt.Sprintf("y%04d", i)
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.write("t2", "zz", "other")
	b.scan(false)
	b.rename("t1", "y0000", "zz")
	for i := 1; i < 1000; i++ {
		b.rename("t1", fmt.Sprintf("y%04d", i), fmt.Sprintf("w%04d", i))
	}
	b.rename("t1", "y1001", "y0000")
	b.pass()
	b.now += 30_000_000_000
	out := b.pass()
	seen := map[string]int{}
	for i := range out.Decisions {
		seen[out.Decisions[i].Path]++
	}
	if seen["y0000"] != 1 {
		t.Errorf("told of %d times", seen["y0000"])
	}
}

// A folder's job would carry a copy at its own file's path past what its branch can reach: the job goes, what
// was told to follow goes one by one, and that copy stays.
func TestAFolderJobWhoseCopyAtItsOwnPathWouldNotFit(t *testing.T) {
	b := newBench(t, "t1", "t2")
	longer := filepath.Join(b.dirs["t2"], strings.Repeat("m", 100))
	if err := os.MkdirAll(filepath.Join(longer, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.dirs["t2"], b.config.Disks[1].Mount = longer, longer
	b.write("t1", "d/x", "x")
	b.write("t2", "d/x", "x")
	b.scan(false)
	room := names.MaxBranchPathBytes - len(longer) - len("/media/") - len("/x")
	var deep strings.Builder
	for deep.Len()+201 < room {
		deep.WriteString(strings.Repeat("e", 200) + "/")
	}
	deep.WriteString(strings.Repeat("e", room-deep.Len()))
	target := deep.String()
	if err := os.MkdirAll(filepath.Join(b.dirs["t1"], "media", filepath.Dir(target)), 0o755); err != nil {
		t.Skipf("this filesystem takes no path that long: %v", err)
	}
	b.gone[longer] = true
	b.rename("t1", "d", target)
	b.settle()
	b.gone[longer] = false
	name := strings.Repeat("n", 150)
	b.write("t2", "d/"+name, "n")
	out := b.pass()
	if got := outcomeOf(out, KindFolder, "d"); got != "" {
		t.Errorf("the folder: %q", got)
	}
	if got := outcomeOf(out, KindRename, "d/x"); got != OutcomeRenamed {
		t.Errorf("the copy told to follow: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/d/"+name) {
		t.Fatal("the copy went past what its branch can reach")
	}
}

// walkOrder makes two folders on each disk, and gives them in the order a walk of the first disk takes them.
func walkOrder(t *testing.T, b *bench, ids ...string) (first, second string) {
	t.Helper()
	for _, id := range ids {
		b.write(id, "p1/o", "o")
		b.write(id, "p2/o", "o")
	}
	dir, err := os.Open(filepath.Join(b.dirs[ids[0]], "media"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	list, err := dir.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return list[0], list[1]
}

// linesOf gives what a pass says of a path on one disk, one outcome for each line.
func linesOf(out *Outcome, disk, kind, path string) []string {
	var list []string
	for i := range out.Operations {
		if op := &out.Operations[i]; op.Disk == disk && op.Kind == kind && op.Path == path {
			list = append(list, op.Outcome)
		}
	}
	return list
}

// A folder found at both its names in one walk, renamed once it was walked and before the walk reached its new
// parent: the new name gets no row, whatever links lead to what is below it, and the next look reads the rename.
func TestAFolderFoundAtBothItsNames(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("linked %v", linked), func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			first, second := walkOrder(t, b, "t1", "t2")
			from, to := first+"/d", second+"/e"
			b.write("t1", from+"/x", "x")
			b.write("t2", from+"/x", "x")
			b.write("t2", from+"/y", "y")
			mount := b.dirs["t1"]
			if linked {
				if err := os.Link(filepath.Join(mount, "media", from, "x"), filepath.Join(mount, "outside")); err != nil {
					t.Fatal(err)
				}
			}
			b.scan(false)
			moved := false
			b.walkFail[mount] = func(call, name string) error {
				if call == "opendir" && name == second && !moved {
					moved = true
					if err := os.Rename(filepath.Join(mount, "media", from), filepath.Join(mount, "media", to)); err != nil {
						t.Error(err)
					}
				}
				return nil
			}
			b.pass()
			if !moved {
				t.Fatal("the walk did not get there")
			}
			if got := b.count("SELECT count(*) FROM entries WHERE CAST(lpath AS TEXT) = ?", to); got != 0 {
				t.Errorf("%d rows at the new name", got)
			}
			if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = ?", to+"/x"); got != 0 {
				t.Errorf("%d files at the new name", got)
			}
			delete(b.walkFail, mount)
			out := b.settle()
			if got := outcomeOf(out, KindFolder, from); got != OutcomeRenamed {
				t.Errorf("the folder: %q (%+v, %+v)", got, out.Decisions, out.Operations)
			}
			if !b.exists("t2", "media/"+to+"/y") {
				t.Fatal("what only the other disk held stayed behind")
			}
		})
	}
}

// A row the walk could not reach can still leave, so its folder waits for it, whatever became of its file.
func TestAFolderWaitsForACopyItCouldNotReach(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	from, to := first+"/d", second+"/e"
	b.write("t1", from+"/x", "x")
	b.write("t2", from+"/x", "x")
	b.write("t2", from+"/y", "y")
	b.scan(false)
	b.rename("t2", from+"/x", "q")
	b.pass()
	b.now += 30_000_000_000
	mount := b.dirs["t1"]
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "stat" && name == "d" && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", from), filepath.Join(mount, "media", to)); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM entries WHERE disk_id = 't1' AND CAST(lpath AS TEXT) = ?", from); got != 1 {
		t.Errorf("%d rows at the old name", got)
	}
}

// A holder of a file on a disk not looked at may hold the folder to make one like: made with no model, it would
// be open there for good.
func TestAParentWhoseHolderIsAway(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "P/a")
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "P"), 0o700); err != nil {
		t.Fatal(err)
	}
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.settle()
	b.gone[mount] = false
	b.gone[b.dirs["t1"]] = true
	out := b.pass()
	if got := outcomeOf(out, KindRename, "a"); got != "io-error" {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
	if b.exists("t2", "media/P") {
		t.Fatal("the parent was made with no model")
	}
	b.gone[b.dirs["t1"]] = false
	b.pass()
	st, err := os.Stat(filepath.Join(mount, "media", "P"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("mode %o", got)
	}
}

// A folder job has one line, whatever became of it and of the disk.
func TestAFolderJobHasOneLine(t *testing.T) {
	t.Run("its move loses the disk", func(t *testing.T) {
		b := newBench(t, "t1", "t2")
		b.write("t1", "d/x", "x")
		b.write("t2", "d/x", "x")
		b.scan(false)
		b.rename("t1", "d", "e")
		b.pass()
		b.now += 30_000_000_000
		b.fail[b.dirs["t2"]] = func(call, _ string) error {
			if call == "rename" {
				return syscall.EIO
			}
			return nil
		}
		out := b.pass()
		if got := linesOf(out, "t2", KindFolder, "d"); !slices.Equal(got, []string{"io-error"}) {
			t.Errorf("%v", got)
		}
	})
	t.Run("a later one loses the disk", func(t *testing.T) {
		b := newBench(t, "t1", "t2")
		for _, name := range []string{"a/x", "c/x", "f/x"} {
			b.write("t1", name, "x")
			b.write("t2", name, "x")
		}
		b.scan(false)
		b.rename("t1", "a", "a2")
		b.rename("t1", "c", "c2")
		b.rename("t1", "f", "f2")
		b.pass()
		b.now += 30_000_000_000
		b.fail[b.dirs["t2"]] = func(call, name string) error {
			switch {
			case call == "rename" && name == "a2":
				return syscall.ENOSPC
			case call == "stat" && name == "c":
				return syscall.EIO
			}
			return nil
		}
		out := b.pass()
		for name, want := range map[string]string{"a": "io-error", "c": OutcomeUnavailable, "f": OutcomeUnavailable} {
			if got := linesOf(out, "t2", KindFolder, name); !slices.Equal(got, []string{want}) {
				t.Errorf("%s: %v", name, got)
			}
		}
	})
}

// A disk lost at one trash job: what its other jobs would say no longer counts, and they wait as it says.
func TestADiskLostAtATrashJob(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(true)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.remove("t1", "a")
	b.remove("t1", "b")
	b.settle()
	b.write("t1", "b", "x")
	b.pass()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "stat" && name == "a" {
			return syscall.EIO
		}
		return nil
	}
	out := b.pass()
	if got := linesOf(out, "t2", KindTrash, "a"); !slices.Equal(got, []string{"io-error"}) {
		t.Errorf("a: %v", got)
	}
	if got := linesOf(out, "t2", KindTrash, "b"); !slices.Equal(got, []string{OutcomeUnavailable}) {
		t.Errorf("b: %v", got)
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 2 {
		t.Errorf("%d jobs left", got)
	}
}

// Only NTFS reads a folder and a case-only other as one: a disk that merely has no room for the name does not,
// and its copy goes to the trash as any refused copy does.
func TestACaseOnlyFolderOnAnotherFilesystem(t *testing.T) {
	b := newBench(t, "t1", "t2")
	longer := filepath.Join(b.dirs["t2"], strings.Repeat("m", 100))
	if err := os.MkdirAll(filepath.Join(longer, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.dirs["t2"], b.config.Disks[1].Mount = longer, longer
	b.write("t1", "D/x", "x")
	b.write("t2", "D/x", "x")
	b.scan(true)
	room := names.MaxBranchPathBytes - len(b.dirs["t1"]) - len("/media/") - len("d/") - len("/x")
	var deep strings.Builder
	for deep.Len()+201 < room {
		deep.WriteString(strings.Repeat("e", 200) + "/")
	}
	deep.WriteString(strings.Repeat("e", room-deep.Len()))
	target := "d/" + deep.String() + "/x"
	if err := os.MkdirAll(filepath.Join(b.dirs["t1"], "media", filepath.Dir(target)), 0o755); err != nil {
		t.Skipf("this filesystem takes no path that long: %v", err)
	}
	b.rename("t1", "D/x", target)
	b.settle()
	b.scan(true)
	out := b.pass()
	if got := outcomeOf(out, KindTrash, "D/x"); got != "trashed" {
		t.Errorf("outcome %q (%+v)", got, out.Operations)
	}
}

// A job held says held in a dry run too, whatever its copy looks like.
func TestADryRunOfAJobHeldOverACopyNotReached(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"s/a", "k"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.config = reconfigured(t, b, "files = 0\npercent = \"100%\"\n")
	b.scan(true)
	b.remove("t1", "s/a")
	b.pass()
	if _, err := catalog.ConfirmGuard(t.Context(), b.db, "t1", b.now, nil); err != nil {
		t.Fatal(err)
	}
	b.now += 60_000_000_000
	b.remove("t1", "k")
	b.walkFail[b.dirs["t2"]] = func(call, name string) error {
		if call == "opendir" && name == "s" {
			return syscall.ELOOP
		}
		return nil
	}
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(dry, KindTrash, "s/a"); got != OutcomeHeld {
		t.Errorf("outcome %q (%+v)", got, dry.Operations)
	}
}

// A refused copy's job goes alone: a dry run says of each what a real pass does.
func TestADryRunOfTwoRenamedJobsThatGo(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.config.Disks[1].FS = names.NTFS
	b.config.Disks[2].FS = names.NTFS
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	b.rename("t1", "a", "a:b")
	b.settle()
	b.scan(true)
	id := b.count("SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'a:b'")
	for _, disk := range []string{"t2", "t3"} {
		if err := catalog.QueueRenamedJob(t.Context(), b.db, int64(id), disk); err != nil {
			t.Fatal(err)
		}
	}
	b.exec("UPDATE files SET state = 'conflict'")
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	real := b.pass()
	for _, disk := range []string{"t2", "t3"} {
		for name, out := range map[string]*Outcome{"dry": dry, "real": real} {
			if got := linesOf(out, disk, KindTrash, "a"); !slices.Equal(got, []string{OutcomeCanceled}) {
				t.Errorf("%s, %s: %v", name, disk, got)
			}
		}
	}
}

// A deletion no longer stands, and its first disk can only be read: nothing is dropped there, so a dry run says
// of the next disk what a real pass does.
func TestADryRunOfAJobThatGoesOnALaterDisk(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	b.gone[b.dirs["t2"]] = true
	b.gone[b.dirs["t3"]] = true
	b.remove("t1", "a")
	b.settle()
	b.write("t1", "a", "x")
	b.pass()
	b.gone[b.dirs["t2"]] = false
	b.gone[b.dirs["t3"]] = false
	b.ro[b.dirs["t2"]] = true
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	real := b.pass()
	for name, out := range map[string]*Outcome{"dry": dry, "real": real} {
		if got := linesOf(out, "t2", KindTrash, "a"); !slices.Equal(got, []string{OutcomeUnavailable}) {
			t.Errorf("%s, t2: %v", name, got)
		}
		if got := linesOf(out, "t3", KindTrash, "a"); !slices.Equal(got, []string{OutcomeCanceled}) {
			t.Errorf("%s, t3: %v", name, got)
		}
	}
}

// Two names with two links are no rename: the new one is a name of its own.
func TestAHardLinkIsNoRenameAWalkStraddles(t *testing.T) {
	b := pool(t)
	b.write("t1", "o", "o")
	b.write("t2", "o", "o")
	b.scan(true)
	media := filepath.Join(b.dirs["t1"], "media")
	if err := os.Link(filepath.Join(media, "a"), filepath.Join(media, "b")); err != nil {
		t.Fatal(err)
	}
	b.pass()
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'b'"); got != 1 {
		t.Errorf("%d rows at the new name", got)
	}
}

// A folder made again with a file below it that is what its row says, edited or not: no folder went with all it
// held, so the new name has a row at once.
func TestAFolderMadeAgainWithAFileAtHome(t *testing.T) {
	for _, there := range []bool{false, true} {
		t.Run(fmt.Sprintf("there %v", there), func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			for _, name := range []string{"d/x", "d/y"} {
				b.write("t1", name, "x")
				b.write("t2", name, "x")
			}
			b.scan(false)
			b.rename("t1", "d", "e")
			if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "d"), 0o755); err != nil {
				t.Fatal(err)
			}
			b.rename("t1", "e/x", "d/x")
			if there {
				var st syscall.Stat_t
				if err := syscall.Stat(filepath.Join(b.dirs["t1"], "media", "d", "x"), &st); err != nil {
					t.Fatal(err)
				}
				b.exec("UPDATE copies SET ctime_ns = ? WHERE disk_id = 't1' AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'd/x')", st.Ctim.Nano())
			}
			b.pass()
			if got := b.count("SELECT count(*) FROM entries WHERE disk_id = 't1' AND CAST(lpath AS TEXT) = 'e'"); got != 1 {
				t.Errorf("%d rows at the new name", got)
			}
		})
	}
}

// A guard counts a copy gone beside a kind moved on a disk out of the configuration: that says nothing now, and
// the decision would be a deletion.
func TestAGuardOverAMoveOnARetiredDisk(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	guard := "files = 0\npercent = \"100%\"\n"
	b.config = reconfigured(t, b, guard)
	b.scan(true)
	b.rename("t2", "a", "q")
	b.pass()
	b.config.Disks = slices.DeleteFunc(b.config.Disks, func(d config.Disk) bool { return d.ID == "t2" })
	b.config = reconfigured(t, b, guard)
	b.scan(false)
	b.remove("t1", "a")
	out := b.settle()
	if len(out.Guards) != 1 || out.Guards[0].Disk != "t1" {
		t.Errorf("guards %+v", out.Guards)
	}
	if got := outcomeOf(out, KindTrash, "a"); got != "" {
		t.Errorf("the trash: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t3", "media/a") {
		t.Fatal("the copy left")
	}
}

// A copy found moved, and out of reach at the next look: what becomes of its file waits for a look that can say,
// in a real pass and in a dry run, and the guard, which does not count it, is not gone around.
func TestADeletionWaitsForAMovedCopyItCannotReach(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "s/a", "x")
	}
	guard := "files = 0\npercent = \"100%\"\n"
	b.config = reconfigured(t, b, guard)
	b.scan(true)
	b.rename("t2", "s/a", "s/b")
	b.pass()
	b.walkFail[b.dirs["t2"]] = func(call, name string) error {
		if call == "opendir" && name == "s" {
			return syscall.ELOOP
		}
		return nil
	}
	b.remove("t1", "s/a")
	b.pass()
	b.now += 30_000_000_000
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	out := b.pass()
	for name, o := range map[string]*Outcome{"dry": dry, "real": out} {
		if len(o.Decisions) != 0 || len(o.Operations) != 0 || len(o.Guards) != 0 {
			t.Errorf("%s: decided %+v, ran %+v, guards %+v", name, o.Decisions, o.Operations, o.Guards)
		}
	}
	delete(b.walkFail, b.dirs["t2"])
	out = b.pass()
	if len(out.Decisions) != 1 || out.Decisions[0].To != "s/b" {
		t.Errorf("once it can look: %+v", out.Decisions)
	}
	if !b.exists("t3", "media/s/b") {
		t.Fatal("the rename did not reach the other copy")
	}
}

// A copy a queued job would carry to its target is no refused copy, whatever the table says of a name that disk
// holds: the guard says nothing of it, and a job from before goes once it is no longer behind.
func TestACopyAFolderJobWillCarry(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued %v", queued), func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			for _, name := range []string{"S/a:b", "S/c"} {
				b.write("t1", name, "x")
				b.write("t2", name, "x")
			}
			b.config = reconfigured(t, b, "percent = \"5%\"\nfiles = 100\n")
			b.config.Disks[1].FS = names.NTFS
			b.scan(true)
			mount := b.dirs["t2"]
			b.ro[mount] = true
			b.rename("t1", "S", "T")
			b.pass()
			b.now += 30_000_000_000
			b.pass()
			out := b.pass()
			if len(out.Guards) != 0 {
				t.Errorf("guards %+v", out.Guards)
			}
			list, err := RefusedCopies(t.Context(), b.db, &b.config.Disks[1])
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 0 {
				t.Errorf("refused %+v", list)
			}
			if queued {
				id := b.count("SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'T/a:b'")
				if err := catalog.QueueRenamedJob(t.Context(), b.db, int64(id), "t2"); err != nil {
					t.Fatal(err)
				}
			}
			b.ro[mount] = false
			out = b.pass()
			if got := outcomeOf(out, KindFolder, "S"); got != OutcomeRenamed {
				t.Errorf("the folder: %q (%+v)", got, out.Operations)
			}
			if !b.exists("t2", "media/T/a:b") {
				t.Fatal("the copy left")
			}
			if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
				t.Errorf("%d jobs left", got)
			}
		})
	}
}

// The same as TestARenameAWalkStraddles on a disk where no row reads as there, as after a chown of all it holds.
func TestARenameAWalkStraddlesWithNoRowThere(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "A/x", "x")
	b.write("t2", "A/x", "x")
	b.write("t1", "B/o", "o")
	b.write("t2", "B/o", "o")
	b.scan(true)
	mount := b.dirs["t1"]
	time.Sleep(20 * time.Millisecond) // past a clock tick, for the change time to move
	for _, name := range []string{"A/x", "B/o"} {
		if err := os.Chmod(filepath.Join(mount, "media", name), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "opendir" && name == "B" && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", "A", "x"), filepath.Join(mount, "media", "B", "x")); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'B/x'"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
	delete(b.walkFail, mount)
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "renamed" || out.Decisions[0].To != "B/x" {
		t.Errorf("decided %+v", out.Decisions)
	}
}

// A guard tripped by its events alone, whose only hold is a copy gone beside a sibling that moved: that row stays
// for the guard, so the exit code says so.
func TestAHoldOnlyBesideARename(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c", "f"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(true)
	for _, name := range []string{"a", "b", "c"} {
		b.remove("t1", name)
	}
	b.settle()
	b.rename("t2", "f", "g")
	b.remove("t1", "f")
	b.pass()
	b.now += 30_000_000_000
	b.config = reconfigured(t, b, "files = 1\npercent = \"100%\"\n")
	out := b.pass()
	if len(out.Guards) != 1 || !out.Held {
		t.Errorf("guards %+v, held %v", out.Guards, out.Held)
	}
}

// A follower whose job went, with no copy at its path: any disk's folder is the model, as for a folder job, or the
// result would depend on the order of the names.
func TestAParentOfAFollowerIsMadeLikeTheFolderOfAnyDisk(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "d/a", "a")
	b.write("t2", "d/a", "a")
	b.write("t2", "d/0", "0")
	b.write("t2", "d/p", "p")
	b.write("t3", "e/p", "other")
	for _, id := range []string{"t1", "t2"} {
		if err := os.Chmod(filepath.Join(b.dirs[id], "media", "d"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b.scan(false)
	b.rename("t1", "d", "e")
	b.settle()
	st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "e"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("mode %o", got)
	}
}

// A row at a name an operation left planned, with its own file there: that says nothing of another row with its
// inode, which is gone when its own name is.
func TestAHardLinkAtAPlannedName(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t1", "b", "x")
	b.write("t2", "a", "x")
	media := filepath.Join(b.dirs["t2"], "media")
	if err := os.Link(filepath.Join(media, "a"), filepath.Join(media, "b")); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	b.exec("INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES ('t2', 'mkdir', CAST('a' AS BLOB), '', 1, 'planned')")
	b.ro[b.dirs["t2"]] = true
	b.remove("t2", "b")
	b.pass()
	if got := b.text("SELECT c.gone_kind FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't2' AND CAST(f.lpath AS TEXT) = 'b'"); got != "gone" {
		t.Errorf("read as %q", got)
	}
}

// A copy's clock is kept while its verdict stays among those that have one, but for a moved that turns gone.
func TestAClockThroughItsVerdicts(t *testing.T) {
	clock := func(b *bench) (int64, string) {
		t.Helper()
		var ns int64
		var kind string
		if err := b.db.QueryRowContext(t.Context(), "SELECT gone_ns, gone_kind FROM copies WHERE disk_id = 't1'").Scan(&ns, &kind); err != nil {
			t.Fatal(err)
		}
		return ns, kind
	}
	t.Run("moved to uncertain", func(t *testing.T) {
		b := pool(t)
		b.rename("t1", "a", "q")
		b.pass()
		first, _ := clock(b)
		b.now += 10_000_000_000
		media := filepath.Join(b.dirs["t1"], "media")
		if err := os.Link(filepath.Join(media, "q"), filepath.Join(media, "r")); err != nil {
			t.Fatal(err)
		}
		b.pass()
		if ns, kind := clock(b); ns != first || kind != "uncertain" {
			t.Errorf("%d, %s, and first %d", ns, kind, first)
		}
	})
	t.Run("own to gone", func(t *testing.T) {
		b := pool(t)
		trash := filepath.Join(b.dirs["t1"], "Atoll", "trash", "2026-01-01", "9")
		if err := os.MkdirAll(trash, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(b.dirs["t1"], "media", "a"), filepath.Join(trash, "a")); err != nil {
			t.Fatal(err)
		}
		b.pass()
		first, kind := clock(b)
		if kind != "own" {
			t.Fatalf("read as %q", kind)
		}
		b.now += 10_000_000_000
		if err := os.Remove(filepath.Join(trash, "a")); err != nil {
			t.Fatal(err)
		}
		b.pass()
		if ns, kind := clock(b); ns != first || kind != "gone" {
			t.Errorf("%d, %s, and first %d", ns, kind, first)
		}
	})
}

// What a copy on a disk away says of a folder renamed elsewhere: only a row the guard holds stops its file following.
func TestAFollowerBesideARowOnADiskAway(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard string
		gone  bool
	}{
		{"gone, with no guard to hold it", "percent = \"100%\"\n", true},
		{"moved, under a guard tripped by others", "files = 1\npercent = \"100%\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2", "t3")
			b.write("t1", "d/k", "k")
			for _, name := range []string{"d/x", "f", "g", "h"} {
				b.write("t2", name, name)
				b.write("t3", name, name)
			}
			b.config = reconfigured(t, b, tc.guard)
			b.scan(true)
			if tc.gone {
				b.remove("t2", "d/x")
			} else {
				b.rename("t2", "d/x", "d/q")
				for _, name := range []string{"f", "g", "h"} {
					b.remove("t2", name)
				}
			}
			b.pass()
			b.gone[b.dirs["t2"]] = true
			b.rename("t1", "d", "e")
			out := b.settle()
			if got := linesOf(out, "t3", KindFolder, "d"); !slices.Equal(got, []string{OutcomeRenamed}) {
				t.Errorf("the folder on t3: %v (%+v, %+v)", got, out.Decisions, out.Operations)
			}
		})
	}
}

// A name only a row whose clock runs still holds, or a folder found renamed and not yet decided: the walk did not
// find it, so a rename waits for it, and nothing goes to the trash for it.
func TestARefusedCopyWhoseCollisionIsLeaving(t *testing.T) {
	for _, tc := range []struct {
		name, holder, target, leftFrom, leftTo string
	}{
		{"a file", "Film", "FILM", "Film", "Other"},
		{"a folder", "Film/x", "FILM/a", "Film", "Other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.config.Disks[1].FS = names.NTFS
			for _, id := range []string{"t1", "t2"} {
				b.write(id, "old", "o")
				b.write(id, tc.holder, "h")
			}
			b.scan(true)
			b.rename("t1", "old", tc.target)
			out := b.settle()
			if got := outcomeOf(out, KindRename, "old"); got != "name-refused" {
				t.Fatalf("the rename: %q (%+v)", got, out.Operations)
			}
			b.scan(true)
			b.rename("t2", tc.leftFrom, tc.leftTo)
			out = b.pass()
			if got := outcomeOf(out, KindTrash, "old"); got != "" {
				t.Errorf("the trash: %q (%+v)", got, out.Operations)
			}
			if !b.exists("t2", "media/old") {
				t.Fatal("the copy went to the trash")
			}
			b.now += 30_000_000_000
			b.pass()
			b.pass()
			if !b.exists("t2", "media/"+tc.target) {
				t.Fatal("the rename did not come once the name was free")
			}
		})
	}
}

// A folder job that lands in the old name of another that failed for now waits with it, as do the copies going
// there one by one, or the other's operation would carry them.
func TestAJobLandingInAFolderThatStays(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a/x", "c/y", "w/z"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(false)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "c", "d")
	b.settle()
	if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.rename("t1", "a", "c/a2")
	b.rename("t1", "w/z", "c/z")
	b.settle()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "rename" && name == "d" {
			return syscall.ENOSPC
		}
		return nil
	}
	out := b.pass()
	if got := outcomeOf(out, KindFolder, "c"); got != "io-error" {
		t.Errorf("the folder: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/a/x") || !b.exists("t2", "media/w/z") {
		t.Fatal("what lands in that folder did not wait")
	}
	if b.exists("t2", "media/c/a2") || b.exists("t2", "media/c/z") {
		t.Fatal("it went into the old folder")
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'rename'"); got != 2 {
		t.Errorf("%d jobs", got)
	}
	b.fail[mount] = nil
	b.pass()
	for _, name := range []string{"media/d/y", "media/c/a2/x", "media/c/z"} {
		if !b.exists("t2", name) {
			t.Errorf("%s is not there", name)
		}
	}
}

// A job goes after any whose old name it lands in, chains included. Of a cycle one goes, and what it held up runs.
func TestTheOrderOfFolderJobs(t *testing.T) {
	jobs := func(pairs ...string) []catalog.FolderJob {
		var list []catalog.FolderJob
		for i := 0; i < len(pairs); i += 2 {
			list = append(list, catalog.FolderJob{From: pairs[i], To: pairs[i+1]})
		}
		return list
	}
	for _, tc := range []struct {
		name       string
		list, want []catalog.FolderJob
	}{
		{"none lands in another", jobs("a", "x", "b", "y"), jobs("a", "x", "b", "y")},
		{"lands below it", jobs("a", "c/a2", "c", "d"), jobs("c", "d", "a", "c/a2")},
		{"lands at it", jobs("a", "c", "c", "d"), jobs("c", "d", "a", "c")},
		{"a chain", jobs("a", "b/x", "b", "c/x", "c", "d"), jobs("c", "d", "b", "c/x", "a", "b/x")},
		{"a name that only starts the same", jobs("a", "cd/x", "c", "d"), jobs("a", "cd/x", "c", "d")},
		{"lands below another's new name", jobs("a", "n/b", "c", "n"), jobs("c", "n", "a", "n/b")},
		{"a new name that only starts the same", jobs("a", "nb", "c", "n"), jobs("a", "nb", "c", "n")},
		{"holds another's old name", jobs("d", "e", "d/s", "f"), jobs("d/s", "f", "d", "e")},
		{"an old name that only starts the same", jobs("d", "e", "ds", "f"), jobs("d", "e", "ds", "f")},
		{"holds one that lands in its old name", jobs("d", "e", "d/s", "d/x"), jobs("d", "e", "d/s", "d/x")},
		{"holds one that lands below its new name", jobs("d", "e", "d/s", "e/x"), jobs("d", "e", "d/s", "e/x")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cycle := folderOrder(tc.list)
			if !slices.Equal(got, tc.want) || len(cycle) != 0 {
				t.Errorf("%+v, and %+v", got, cycle)
			}
		})
	}
	for _, tc := range []struct {
		name              string
		list, want, cycle []catalog.FolderJob
	}{
		{"a cycle", jobs("a", "b/r/p", "b", "a/q", "c", "b/r"), jobs("b", "a/q", "c", "b/r"), jobs("a", "b/r/p")},
		{"one leading into a cycle, sorting before it", jobs("0", "a/z", "a", "b/r/p", "b", "a/q", "c", "b/r"), jobs("0", "a/z", "b", "a/q", "c", "b/r"), jobs("a", "b/r/p")},
		{"a job below its own old name", jobs("0", "a/z", "a", "a/x"), jobs("0", "a/z"), jobs("a", "a/x")},
		{"a cycle through what a folder holds", jobs("d", "e", "d/s", "f", "f", "d/q"), jobs("f", "d/q", "d/s", "f"), jobs("d", "e")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cycle := folderOrder(tc.list)
			if !slices.Equal(got, tc.want) || !slices.Equal(cycle, tc.cycle) {
				t.Errorf("%+v, and %+v", got, cycle)
			}
		})
	}
}

// What a folder's operation brings to its target, and only that.
func TestWhatAFolderJobCarries(t *testing.T) {
	j := &catalog.FolderJob{From: "c", To: "e"}
	for _, tc := range []struct {
		place, target string
		want          bool
	}{
		{"c/x", "e/x", true},
		{"c/s/x", "e/s/x", true},
		{"c/x", "e/y", false},
		{"c/x", "q", false},
		{"cd/x", "ed/x", false},
	} {
		if got := carries(j, tc.place, tc.target); got != tc.want {
			t.Errorf("%s to %s: %v", tc.place, tc.target, got)
		}
	}
}

// What is left beside a copy gone counts on a disk out of the configuration, whatever its row last said.
func TestAGuardOverACopyLeftOnARetiredDisk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	guard := "files = 100\npercent = \"5%\"\n"
	b.config = reconfigured(t, b, guard)
	b.scan(true)
	b.remove("t2", "a")
	b.pass()
	b.config.Disks = slices.DeleteFunc(b.config.Disks, func(d config.Disk) bool { return d.ID == "t2" })
	b.config = reconfigured(t, b, guard)
	b.scan(false)
	b.remove("t1", "a")
	out := b.pass()
	if len(out.Guards) != 1 || out.Guards[0].Disk != "t1" {
		t.Errorf("guards %+v", out.Guards)
	}
}

// A copy moved out of reach holds back what its file would do on other disks too: the name may be about to change,
// and a deletion no longer stands.
func TestWhatACopyMovedOutOfReachHoldsBack(t *testing.T) {
	unreachable := func(call, name string) error {
		if call == "opendir" && name == "s" {
			return syscall.ELOOP
		}
		return nil
	}
	t.Run("a refused copy", func(t *testing.T) {
		b := newBench(t, "t1", "t2", "t3")
		b.config.Disks[1].FS = names.NTFS
		for _, id := range []string{"t1", "t2", "t3"} {
			b.write(id, "s/a", "x")
		}
		b.scan(true)
		b.rename("t1", "s/a", "s/a:b")
		b.settle()
		b.rename("t1", "s/a:b", "s/c")
		b.pass()
		b.walkFail[b.dirs["t1"]] = unreachable
		out := b.pass()
		if got := outcomeOf(out, KindTrash, "s/a"); got != "" {
			t.Errorf("the trash: %q (%+v)", got, out.Operations)
		}
		if !b.exists("t2", "media/s/a") {
			t.Fatal("the copy went to the trash")
		}
		delete(b.walkFail, b.dirs["t1"])
		b.now += 30_000_000_000
		b.pass()
		if !b.exists("t2", "media/s/c") {
			t.Fatal("the rename did not reach it")
		}
	})
	t.Run("a queued deletion", func(t *testing.T) {
		b := newBench(t, "t1", "t2", "t3")
		for _, id := range []string{"t1", "t2", "t3"} {
			b.write(id, "s/a", "x")
		}
		b.scan(true)
		b.gone[b.dirs["t2"]] = true
		b.gone[b.dirs["t3"]] = true
		b.remove("t1", "s/a")
		b.settle()
		b.gone[b.dirs["t3"]] = false
		b.ro[b.dirs["t3"]] = true
		b.rename("t3", "s/a", "s/b")
		b.pass()
		b.walkFail[b.dirs["t3"]] = unreachable
		b.gone[b.dirs["t2"]] = false
		out := b.pass()
		if got := linesOf(out, "t2", KindTrash, "s/a"); !slices.Equal(got, []string{OutcomeCanceled}) {
			t.Errorf("%v (%+v)", got, out.Operations)
		}
		if !b.exists("t2", "media/s/a") {
			t.Fatal("the copy went to the trash")
		}
	})
}

// A rename while the walk goes, of a file with a link outside the root, as an import with hard links leaves it:
// as many links as before and a change time that moved. It reads as a rename, and no deletion comes of it.
func TestARenameAWalkStraddlesWithALinkOutside(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	b.write("t1", first+"/x", "x")
	b.write("t2", first+"/x", "x")
	mount := b.dirs["t1"]
	if err := os.Link(filepath.Join(mount, "media", first, "x"), filepath.Join(mount, "outside")); err != nil {
		t.Fatal(err)
	}
	b.scan(true)
	time.Sleep(20 * time.Millisecond) // past a clock tick, for the change time to move
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "opendir" && name == second && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", first, "x"), filepath.Join(mount, "media", second, "x")); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = ?", second+"/x"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
	delete(b.walkFail, mount)
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "renamed" {
		t.Errorf("decided %+v", out.Decisions)
	}
	if !b.exists("t2", "media/"+second+"/x") {
		t.Fatal("the rename did not reach the other copy")
	}
}

// A guard command goes by what a pass can see: a verified copy on a disk out of the configuration says nothing.
func TestARefusedCopyWhoseGoodCopyIsRetired(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "a", "x")
	}
	b.write("t3", "k", "k")
	b.config.Disks[1].FS = names.NTFS
	b.scan(true)
	b.rename("t1", "a", "a:b")
	b.settle()
	b.scan(true)
	b.config.Disks = slices.DeleteFunc(b.config.Disks, func(d config.Disk) bool { return d.ID == "t1" })
	b.config = reconfigured(t, b, "percent = \"100%\"\n")
	b.config.Disks[0].FS = names.NTFS
	b.scan(false)
	list, err := RefusedCopies(t.Context(), b.db, &b.config.Disks[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("refused %+v", list)
	}
}

// A holder of a file that does not hold the folder, as it was removed there: another disk that does is the model.
func TestAParentWhoseHolderHasNoSuchFolder(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.write("t3", "E/g", "g")
	if err := os.Chmod(filepath.Join(b.dirs["t3"], "media", "E"), 0o700); err != nil {
		t.Fatal(err)
	}
	b.scan(true)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "a", "E/a")
	b.settle()
	b.remove("t1", "E/a")
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "E")); err != nil {
		t.Fatal(err)
	}
	b.gone[mount] = false
	b.pass()
	st, err := os.Stat(filepath.Join(mount, "media", "E"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("mode %o", got)
	}
}

// A name that only the old name of a folder whose job is still queued holds: a rename waits for that job, the guard
// is not asked, and nothing goes to the trash.
func TestACopyRefusedByAFolderWhoseJobStays(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.config.Disks[1].FS = names.NTFS
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "d/k", "k")
		b.write(id, "z/y", "y")
	}
	b.scan(true)
	b.rename("t1", "d", "D")
	b.rename("t1", "z/y", "D/y")
	b.pass()
	b.now += 30_000_000_000
	mount := b.dirs["t2"]
	b.fail[mount] = func(call, name string) error {
		if call == "rename" && name == "D" {
			return syscall.ENOSPC
		}
		return nil
	}
	for try := range 3 {
		out := b.pass()
		if got := outcomeOf(out, KindTrash, "z/y"); got != "" {
			t.Errorf("try %d: the trash: %q (%+v)", try, got, out.Operations)
		}
		if !b.exists("t2", "media/z/y") {
			t.Fatalf("try %d: the copy went to the trash", try)
		}
	}
	list, err := RefusedCopies(t.Context(), b.db, &b.config.Disks[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("refused %+v", list)
	}
	id := b.count("SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'D/y'")
	if err := catalog.QueueRenamedJob(t.Context(), b.db, int64(id), "t2"); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(b.pass(), KindTrash, "z/y"); got != OutcomeCanceled {
		t.Errorf("a job from before: %q", got)
	}
	b.fail[mount] = nil
	b.pass()
	for _, name := range []string{"media/D/k", "media/D/y"} {
		if !b.exists("t2", name) {
			t.Errorf("%s is not there", name)
		}
	}
}

// A chain of folder jobs, with the first failing for now: all of it waits, or the last would go into the old folder
// of the second, which would then carry it.
func TestAChainOfJobsBehindOneThatStays(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a/f", "b/g", "c/h"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(false)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	media := filepath.Join(b.dirs["t1"], "media")
	mkdir := func(name string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(media, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.rename("t1", "c", "d")
	b.settle()
	mkdir("c")
	b.rename("t1", "b", "c/x")
	b.settle()
	mkdir("b")
	b.rename("t1", "a", "b/x")
	b.settle()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "rename" && name == "d" {
			return syscall.ENOSPC
		}
		return nil
	}
	b.pass()
	if !b.exists("t2", "media/a/f") || b.exists("t2", "media/b/x") {
		t.Fatal("the last job went into the old folder")
	}
	b.fail[mount] = nil
	b.settle()
	for _, name := range []string{"media/b/x/f", "media/c/x/g", "media/d/h"} {
		if !b.exists("t2", name) {
			t.Errorf("%s is not there", name)
		}
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'b/x/f'"); got != 1 {
		t.Errorf("the file left the name the user gave it: %d rows there", got)
	}
}

// A name a rule ignores is no name a file was renamed to, whatever its change time says: its entry stays.
func TestALinkARuleIgnoresIsNoRenameAWalkStraddles(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	b.config = reconfigured(t, b, "percent = \"100%\"\n[[rule]]\nlabel = \"skip\"\npath = \""+second+"/x\"\naction = \"ignore\"\n")
	mount := b.dirs["t1"]
	media := filepath.Join(mount, "media")
	b.write("t1", first+"/x", "x")
	if err := os.Link(filepath.Join(media, first, "x"), filepath.Join(media, second, "x")); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	entry := "SELECT count(*) FROM entries WHERE disk_id = 't1' AND kind = 'ignored' AND CAST(lpath AS TEXT) = ?"
	if got := b.count(entry, second+"/x"); got != 1 {
		t.Fatalf("%d entries before", got)
	}
	time.Sleep(20 * time.Millisecond) // past a clock tick, for the change time to move
	touched := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "opendir" && name == second && !touched {
			touched = true
			if err := os.Chmod(filepath.Join(media, first, "x"), 0o640); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !touched {
		t.Fatal("the walk did not get there")
	}
	if got := b.count(entry, second+"/x"); got != 1 {
		t.Errorf("%d entries after", got)
	}
}

// The same as TestARenameAWalkStraddlesAtItsLook with a link outside the root.
func TestARenameAWalkStraddlesAtItsLookWithALinkOutside(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	b.write("t1", first+"/x", "x")
	b.write("t2", first+"/x", "x")
	mount := b.dirs["t1"]
	if err := os.Link(filepath.Join(mount, "media", first, "x"), filepath.Join(mount, "outside")); err != nil {
		t.Fatal(err)
	}
	b.scan(true)
	time.Sleep(20 * time.Millisecond) // past a clock tick, for the change time to move
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "stat" && name == "x" && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", first, "x"), filepath.Join(mount, "media", second, "x")); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = ?", second+"/x"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
	delete(b.walkFail, mount)
	out := b.settle()
	if len(out.Decisions) != 1 || out.Decisions[0].Kind != "renamed" {
		t.Errorf("decided %+v", out.Decisions)
	}
}

// A folder moved during the walk from a parent not yet listed to one already read, so found at neither name: its row
// stays while a copy of that disk sits below it, and the next look reads the rename.
func TestAFolderFoundAtNeitherName(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	from, to := second+"/d", first+"/e"
	b.write("t1", from+"/x", "x")
	b.write("t2", from+"/x", "x")
	b.write("t2", from+"/y", "y")
	b.scan(false)
	mount := b.dirs["t1"]
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "opendir" && name == second && !moved {
			moved = true
			if err := os.Rename(filepath.Join(mount, "media", from), filepath.Join(mount, "media", to)); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	delete(b.walkFail, mount)
	b.pass()
	b.now += 30_000_000_000
	out := b.pass()
	if got := outcomeOf(out, KindFolder, from); got != OutcomeRenamed {
		t.Errorf("the folder: %q (%+v, %+v)", got, out.Decisions, out.Operations)
	}
	if !b.exists("t2", "media/"+to+"/y") {
		t.Fatal("what only the other disk held stayed behind")
	}
}

// A hard link made while the walk goes is a name of its own: one more link says it is no rename.
func TestAHardLinkMadeDuringTheWalk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	b.write("t1", first+"/x", "x")
	b.write("t2", first+"/x", "x")
	mount := b.dirs["t1"]
	b.scan(true)
	time.Sleep(20 * time.Millisecond) // past a clock tick, for the change time to move
	linked := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "opendir" && name == second && !linked {
			linked = true
			if err := os.Link(filepath.Join(mount, "media", first, "x"), filepath.Join(mount, "media", second, "y")); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.pass()
	if !linked {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = ?", second+"/y"); got != 1 {
		t.Errorf("%d rows at the new name", got)
	}
}

// One link says enough for a row a walk could not reach, on a driver that keeps the change time through a rename.
func TestARenameAWalkStraddlesAtItsLookWithTheSameChangeTime(t *testing.T) {
	b := newBench(t, "t1", "t2")
	first, second := walkOrder(t, b, "t1", "t2")
	b.write("t1", first+"/x", "x")
	b.write("t2", first+"/x", "x")
	b.scan(true)
	mount := b.dirs["t1"]
	moved := false
	b.walkFail[mount] = func(call, name string) error {
		if call == "stat" && name == "x" && !moved {
			moved = true
			to := filepath.Join(mount, "media", second, "x")
			if err := os.Rename(filepath.Join(mount, "media", first, "x"), to); err != nil {
				t.Error(err)
			}
			var st syscall.Stat_t
			if err := syscall.Stat(to, &st); err != nil {
				t.Error(err)
			}
			b.exec("UPDATE copies SET ctime_ns = ? WHERE disk_id = 't1' AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = ?)", st.Ctim.Nano(), first+"/x")
		}
		return nil
	}
	b.pass()
	if !moved {
		t.Fatal("the walk did not get there")
	}
	if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = ?", second+"/x"); got != 0 {
		t.Errorf("%d rows at the new name", got)
	}
}

// A collision with a copy behind, removed since: the same as with one at its own path.
func TestARefusedCopyWhoseCollisionIsBehindAndGone(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "old", "o")
		b.write(id, "Film", "h")
	}
	b.scan(true)
	mount := b.dirs["t2"]
	b.ro[mount] = true
	b.rename("t1", "Film", "Zed")
	b.settle()
	b.rename("t1", "old", "FILM")
	b.settle()
	b.scan(true)
	b.remove("t2", "Film")
	b.ro[mount] = false
	out := b.pass()
	if got := outcomeOf(out, KindTrash, "old"); got != "" {
		t.Errorf("the trash: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/old") {
		t.Fatal("the copy went to the trash")
	}
}

// A dry run of a job whose look fails: what lands in that folder waits, as what it carries does.
func TestADryRunOfWhatLandsInAFolderWhoseLookFails(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a/x", "c/y", "w/z"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(false)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "c", "d")
	b.settle()
	if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.rename("t1", "a", "c/a2")
	b.rename("t1", "w/z", "c/z")
	b.settle()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "stat" && name == "d" {
			return syscall.EACCES
		}
		return nil
	}
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(dry, KindFolder, "c"); got != "io-error" {
		t.Errorf("the folder: %q (%+v)", got, dry.Operations)
	}
	if got := outcomeOf(dry, KindFolder, "a"); got != "" {
		t.Errorf("what lands in it: %q (%+v)", got, dry.Operations)
	}
	if got := outcomeOf(dry, KindRename, "w/z"); got != "" {
		t.Errorf("the copy that lands in it: %q (%+v)", got, dry.Operations)
	}
}

// A folder job that lands below the new name of another, failed for now, waits with it: run first, it would make that
// name, and the other would find it there and go, leaving its old folder behind. A dry run says the same of one whose
// look fails.
func TestAJobLandingBelowTheNewNameOfOneThatStays(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a/x", "c/y"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(false)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "c", "n")
	b.rename("t1", "a", "n/b")
	b.settle()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "stat" && name == "n" {
			return syscall.EACCES
		}
		return nil
	}
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(dry, KindFolder, "c"); got != "io-error" {
		t.Errorf("dry run, the folder: %q (%+v)", got, dry.Operations)
	}
	if got := outcomeOf(dry, KindFolder, "a"); got != "" {
		t.Errorf("dry run, what lands below its new name: %q (%+v)", got, dry.Operations)
	}
	b.fail[mount] = func(call, name string) error {
		if call == "rename" && name == "n" {
			return syscall.ENOSPC
		}
		return nil
	}
	out := b.pass()
	if got := outcomeOf(out, KindFolder, "c"); got != "io-error" {
		t.Errorf("the folder: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/a/x") || b.exists("t2", "media/n") {
		t.Fatal("what lands below its new name did not wait")
	}
	b.fail[mount] = nil
	b.pass()
	for _, name := range []string{"media/n/y", "media/n/b/x"} {
		if !b.exists("t2", name) {
			t.Errorf("%s is not there", name)
		}
	}
	if b.exists("t2", "media/c") {
		t.Error("the old folder is left behind")
	}
}

// A folder job whose old name holds a folder another job takes out, failed for now, waits for that one: run first,
// it would carry that folder away with it. A dry run says the same of one whose look fails.
func TestAJobHoldingAFolderThatStays(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"D/a", "D/s/x"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(false)
	mount := b.dirs["t2"]
	b.gone[mount] = true
	b.rename("t1", "D/s", "F")
	b.rename("t1", "D", "E")
	b.settle()
	b.gone[mount] = false
	b.fail[mount] = func(call, name string) error {
		if call == "stat" && name == "F" {
			return syscall.EACCES
		}
		return nil
	}
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(dry, KindFolder, "D/s"); got != "io-error" {
		t.Errorf("dry run, the folder inside: %q (%+v)", got, dry.Operations)
	}
	if got := outcomeOf(dry, KindFolder, "D"); got != "" {
		t.Errorf("dry run, the folder that holds it: %q (%+v)", got, dry.Operations)
	}
	b.fail[mount] = func(call, name string) error {
		if call == "rename" && name == "F" {
			return syscall.ENOSPC
		}
		return nil
	}
	out := b.pass()
	if got := outcomeOf(out, KindFolder, "D/s"); got != "io-error" {
		t.Errorf("the folder inside: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/D/a") || !b.exists("t2", "media/D/s/x") {
		t.Fatal("the folder that holds it did not wait")
	}
	b.fail[mount] = nil
	b.pass()
	for _, name := range []string{"media/F/x", "media/E/a"} {
		if !b.exists("t2", name) {
			t.Errorf("%s is not there", name)
		}
	}
	for _, name := range []string{"media/E/s", "media/D"} {
		if b.exists("t2", name) {
			t.Errorf("%s is left", name)
		}
	}
}

// A file row that is not there has no copy anywhere.
func TestNoFileRowHasNoCopy(t *testing.T) {
	if got := copyAt(nil, "t1"); got != -1 {
		t.Errorf("%d", got)
	}
}

// A folder removed, whose row stays for a moment for the copies below it: a name the look did not find is no lasting
// refusal. The rename waits for that row, and nothing goes to the trash.
func TestARefusedCopyWhoseCollisionWasRemoved(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "old", "o")
		b.write(id, "Film/x", "h")
	}
	b.scan(true)
	b.rename("t1", "old", "FILM/a")
	out := b.settle()
	if got := outcomeOf(out, KindRename, "old"); got != "name-refused" {
		t.Fatalf("the rename: %q (%+v)", got, out.Operations)
	}
	b.scan(true)
	b.remove("t2", "Film/x")
	b.remove("t2", "Film")
	out = b.pass()
	if got := outcomeOf(out, KindTrash, "old"); got != "" {
		t.Errorf("the trash: %q (%+v)", got, out.Operations)
	}
	if !b.exists("t2", "media/old") {
		t.Fatal("the copy went to the trash")
	}
	b.now += 30_000_000_000
	b.pass()
	b.pass()
	if !b.exists("t2", "media/FILM/a") {
		t.Fatal("the rename did not come once the name was free")
	}
}

// A dry run counts what is left beside a copy gone as a real pass does: a copy its look could not reach goes by its
// row, on a disk it walked.
func TestADryRunOfAGuardBesideACopyItCannotReach(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "s/a", "x")
	b.write("t2", "s/a", "x")
	b.config = reconfigured(t, b, "percent = \"5%\"\n")
	b.scan(true)
	b.remove("t1", "s/a")
	b.remove("t2", "s/a")
	b.pass()
	b.walkFail[b.dirs["t2"]] = func(call, name string) error {
		if call == "opendir" && name == "s" {
			return syscall.ELOOP
		}
		return nil
	}
	b.now += 30_000_000_000
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	real := b.pass()
	if len(dry.Guards) != len(real.Guards) {
		t.Errorf("a dry run says %+v, and a real pass %+v", dry.Guards, real.Guards)
	}
}

func reconfigured(t *testing.T, b *bench, guard string) *config.Config {
	t.Helper()
	var text strings.Builder
	fmt.Fprintf(&text, "pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", "/x/catalog.db", "/x/pool")
	for i := range b.config.Disks {
		d := &b.config.Disks[i]
		fmt.Fprintf(&text, "[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = \"ext4\"\n", d.ID, d.UUID, d.Mount)
	}
	text.WriteString("[deletes.mass_guard]\n" + guard)
	c, err := config.Parse([]byte(text.String()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
