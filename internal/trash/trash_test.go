package trash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// bench is a pool of real folders, on a machine that says each is a disk, and that can make any call
// on one fail, or its head unreadable.
type bench struct {
	t       *testing.T
	db      *sql.DB
	config  *config.Config
	dirs    map[string]string
	fail    map[string]func(call, name string) error
	unread  map[string]bool
	ro      map[string]bool
	confirm map[string]int
	points  []string
	// at says what happens when an operation reaches a point, once.
	at map[string]func()
	// beforeRead says what happens just before a read past the cache of a mount, once.
	beforeRead map[string]func()
	// otherMount says which folder below the top of a mount is another mount, at any time.
	otherMount map[string]string
	// onMount is told of each look at a mount.
	onMount func(mount string)
}

func (b *bench) Mount(mount string) disks.MountSeen {
	if b.onMount != nil {
		b.onMount(mount)
	}
	options := []string{"rw"}
	if b.ro[mount] {
		options = []string{"ro"}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: "ext4", MountOptions: options, SuperOptions: options,
	}}
}

func (b *bench) identity(entry *disks.MountEntry) disks.DeviceSeen {
	for i := range b.config.Disks {
		if b.config.Disks[i].Mount == entry.MountPoint {
			return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: b.config.Disks[i].UUID}}
		}
	}
	return disks.DeviceSeen{Device: disks.DeviceGoneAway}
}

func (b *bench) Device(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return b.identity(entry)
}

func (b *bench) ReadPastCache(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	b.confirm[entry.MountPoint]++
	if fire := b.beforeRead[entry.MountPoint]; fire != nil {
		delete(b.beforeRead, entry.MountPoint)
		fire()
	}
	if b.unread[entry.MountPoint] {
		return disks.DeviceSeen{Device: disks.DeviceUnreadable, Note: "EIO"}
	}
	return b.identity(entry)
}

func (b *bench) Root(string, string) disks.RootSeen { return disks.RootSeen{Folder: disks.FolderOK} }
func (b *bench) Now() int64                         { return 1767225600000000000 }
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
	return fsx.OpenDir(filepath.Join(mount, root)) //nolint:wrapcheck // a test double
}

func (b *bench) OpenTop(mount string) (fsx.Dir, error) {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	top = &mountBelow{Dir: top, b: b, mount: mount}
	if fail := b.fail[mount]; fail != nil {
		return fsx.Fault(top, fail), nil
	}
	return top, nil
}

// mountBelow is a folder with another mount at one of its names, or further down it.
type mountBelow struct {
	fsx.Dir
	b     *bench
	mount string
}

func (m *mountBelow) Unwrap() fsx.Dir { return m.Dir }

func (m *mountBelow) OpenDir(name string) (fsx.Dir, error) {
	below, err := m.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	if name == m.b.otherMount[m.mount] {
		return &otherMountDir{Dir: below}, nil
	}
	return &mountBelow{Dir: below, b: m.b, mount: m.mount}, nil
}

type otherMountDir struct{ fsx.Dir }

func (o *otherMountDir) Unwrap() fsx.Dir { return o.Dir }

func (o *otherMountDir) Info() (fsx.Stat, error) {
	st, err := o.Dir.Info()
	st.MountID++
	return st, err //nolint:wrapcheck // a test double
}

func newBench(t *testing.T) *bench {
	t.Helper()
	top := t.TempDir()
	var text strings.Builder
	fmt.Fprintf(&text, "pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", filepath.Join(top, "catalog.db"), filepath.Join(top, "pool"))
	b := &bench{t: t, dirs: map[string]string{}, fail: map[string]func(string, string) error{}, unread: map[string]bool{}, ro: map[string]bool{}, confirm: map[string]int{},
		at: map[string]func(){}, beforeRead: map[string]func(){}, otherMount: map[string]string{}}
	for i, id := range []string{"t1", "t2", "t3"} {
		dir := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(dir, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		b.dirs[id] = dir
		fmt.Fprintf(&text, "[[disk]]\nid = %q\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d\"\nmount = %q\nfs = \"ext4\"\n", id, i, dir)
	}
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
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	if _, err := watch.Scan(t.Context(), db, c, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
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

func (b *bench) count(query string) int {
	b.t.Helper()
	var n int
	if err := b.db.QueryRowContext(b.t.Context(), query).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

func (b *bench) text(query string) string {
	b.t.Helper()
	var s string
	if err := b.db.QueryRowContext(b.t.Context(), query).Scan(&s); err != nil {
		b.t.Fatal(err)
	}
	return s
}

func (b *bench) prune() ([]PruneItem, *Summary) {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, true)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	var items []PruneItem
	summary, err := s.Prune(b.t.Context(), func(i *PruneItem) error {
		items = append(items, *i)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := s.Finish(b.t.Context()); err != nil {
		b.t.Fatal(err)
	}
	return items, summary
}

func (b *bench) sourceThere() bool {
	b.t.Helper()
	_, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "a"))
	return err == nil
}

// failOn makes one call on one disk fail.
func (b *bench) failOn(id, call string, err error) {
	b.fail[b.dirs[id]] = func(c, _ string) error {
		if c == call {
			return err
		}
		return nil
	}
}

// failNth makes the nth call of one kind on one disk fail, and only that one.
func (b *bench) failNth(id, call string, n int, err error) {
	seen := 0
	b.fail[b.dirs[id]] = func(c, _ string) error {
		if c == call {
			seen++
			if seen == n {
				return err
			}
		}
		return nil
	}
}

// The first run of a prune: four folders of the trash, each synced through its parent, then the
// target folder and the source's folder.
const syncsOfAFirstMove = 6

// Any fsync that fails, whichever it is: the disk is lost, the operation stays planned, and the next
// run looks at what is there. One that fails before the move leaves it not done, and one after it
// leaves it done, and only a reprise can tell (specs.md §17.1, §17.2).
func TestEverySyncOfAMoveCounts(t *testing.T) {
	for n := 1; n <= syncsOfAFirstMove; n++ {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			b := newBench(t)
			b.failNth("t3", "sync", n, syscall.EIO)
			items, summary := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
				t.Fatalf("%+v, %+v", items, summary)
			}
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Errorf("the operation is %q", got)
			}
			if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
				t.Errorf("the disk is %q", got)
			}
			moved := n > syncsOfAFirstMove-2
			if b.sourceThere() == moved {
				t.Errorf("the source there: %v", !moved)
			}
			delete(b.fail, b.dirs["t3"])
			items, _ = b.prune()
			want := "done"
			if !moved {
				// The reprise undoes it, and this run does the work under a new operation.
				want = "undone"
				if len(items) != 1 || items[0].Outcome != OutcomeTrashed {
					t.Errorf("after the reprise: %+v", items)
				}
				if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "Atoll", "trash", day, "1")); !os.IsNotExist(err) {
					t.Errorf("the undone operation left its folder: %v", err)
				}
			} else if len(items) != 0 {
				t.Errorf("after the reprise: %+v", items)
			}
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != want {
				t.Errorf("the reprise leaves it %q", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 0 {
				t.Errorf("%d rows for a copy in the trash", got)
			}
			if b.sourceThere() {
				t.Error("the copy did not go")
			}
		})
	}
}

const day = "2026-01-01"

func (b *bench) inTrash(op int, path string) string {
	return filepath.Join(b.dirs["t3"], "Atoll", "trash", day, strconv.Itoa(op), filepath.FromSlash(path))
}

// replace gives a name a new file, with a new inode, as an atomic save does.
func (b *bench) replace(full, text string) {
	b.t.Helper()
	next := full + ".new"
	if err := os.WriteFile(next, []byte(text), 0o644); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Rename(next, full); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) sourceText() string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.dirs["t3"], "media", "a"))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

// moveAway takes t3 from under its mount path, and leaves an empty folder there: what a real remount
// does to what a path leads to, while a handle still leads to the old one.
func (b *bench) moveAway() error {
	dir := b.dirs["t3"]
	if err := os.Rename(dir, dir+".away"); err != nil {
		return err //nolint:wrapcheck // a test double
	}
	return os.MkdirAll(filepath.Join(dir, "media"), 0o755) //nolint:wrapcheck // a test double
}

// opened is a session that writes, closed with the test.
func (b *bench) opened() *Session {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, true)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(s.Close)
	return s
}

// restored restores what is at or below a path, and gives the outcomes.
func (b *bench) restored(under string) ([]string, *Summary) {
	b.t.Helper()
	s := b.opened()
	defer s.Close()
	var outcomes []string
	summary, err := s.Restore(b.t.Context(), under, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Outcome)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := s.Finish(b.t.Context()); err != nil {
		b.t.Fatal(err)
	}
	return outcomes, summary
}

// purgedAll purges every entry whatever its age, and gives the outcomes.
func (b *bench) purgedAll() ([]string, *Summary) {
	b.t.Helper()
	s := b.opened()
	defer s.Close()
	var outcomes []string
	var zero time.Duration
	summary, err := s.Purge(b.t.Context(), &zero, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Outcome)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return outcomes, summary
}

// linkedNames gives t3 one file with two names, c and d, and reads everything.
func (b *bench) linkedNames() {
	b.t.Helper()
	b.linkedAs("c", "d")
}

// linkedAs gives t3 one file with several names, and each its own file on the other disks.
func (b *bench) linkedAs(first string, others ...string) {
	b.t.Helper()
	b.write("t1", first, "y")
	b.write("t2", first, "y")
	b.write("t3", first, "y")
	for _, name := range others {
		b.write("t1", name, "y")
		b.write("t2", name, "y")
		if err := os.Link(filepath.Join(b.dirs["t3"], "media", first), filepath.Join(b.dirs["t3"], "media", name)); err != nil {
			b.t.Fatal(err)
		}
	}
	if _, err := watch.Scan(b.t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		b.t.Fatal(err)
	}
}

// addFiles writes files on the three disks, and reads them all.
func (b *bench) addFiles(paths ...string) {
	b.t.Helper()
	for _, path := range paths {
		for _, id := range []string{"t1", "t2", "t3"} {
			b.write(id, path, "z")
		}
	}
	if _, err := watch.Scan(b.t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		b.t.Fatal(err)
	}
}

// failSyncOf makes a fsync of one folder fail, by name, the top having none.
func (b *bench) failSyncOf(name string) {
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		if call == "sync" && folder == name {
			return syscall.EIO
		}
		return nil
	}
}

// failStatAfter makes every look at a name fail, once a point has been reached.
func (b *bench) failStatAfter(point, name string, err error) {
	armed := false
	b.at[point] = func() { armed = true }
	b.fail[b.dirs["t3"]] = func(call, n string) error {
		if armed && call == "stat" && n == name {
			return err
		}
		return nil
	}
}

// plan journals operations with nothing done yet, as a crash right after that would leave them.
func (b *bench) plan(paths ...string) {
	b.t.Helper()
	for _, path := range paths {
		var key string
		if err := b.db.QueryRowContext(b.t.Context(), "SELECT c.file_key FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't3' AND f.lpath = ?", []byte(path)).Scan(&key); err != nil {
			b.t.Fatal(err)
		}
		if _, err := catalog.BeginTrash(b.t.Context(), b.db, &catalog.TrashStart{Disk: "t3", Path: path, FileKey: key, Reason: "surplus", NowNS: b.Now(), ExpiresNS: b.Now() + 1}); err != nil {
			b.t.Fatal(err)
		}
	}
}

// Every folder on the way is synced, each on its own, whether the move finishes or is undone: a way
// synced only in part can leave a name that leads nowhere after a power cut (specs.md §17.2).
func TestARepriseSyncsTheWholeWay(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"it finishes", syncsOfAFirstMove, "done"},
		{"it is undone", syncsOfAFirstMove - 2, "undone"},
	} {
		for _, folder := range []string{"", "Atoll", "trash", day, "1", "media"} {
			t.Run(tc.name+"/"+folder, func(t *testing.T) {
				b := newBench(t)
				b.failNth("t3", "sync", tc.n, syscall.EIO)
				b.prune()
				b.failSyncOf(folder)
				if _, summary := b.prune(); !summary.DiskLost {
					t.Errorf("%+v", summary)
				}
				if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "planned" {
					t.Errorf("%q, with that folder never synced", got)
				}
				delete(b.fail, b.dirs["t3"])
				b.prune()
				if got := b.text("SELECT state FROM ops WHERE id = 1"); got != tc.want {
					t.Errorf("%q, want %q", got, tc.want)
				}
			})
		}
	}
}

// A restore finished by a reprise syncs the way to the path, and the folder it left.
func TestARepriseOfARestoreSyncs(t *testing.T) {
	for _, folder := range []string{"", "media", "1"} {
		t.Run(folder, func(t *testing.T) {
			b := newBench(t)
			b.prune()
			b.failNth("t3", "sync", 1, syscall.EIO)
			b.restored("a")
			b.failSyncOf(folder)
			s := b.opened()
			s.Close()
			if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "planned" {
				t.Errorf("%q, with that folder never synced", got)
			}
			delete(b.fail, b.dirs["t3"])
			s = b.opened()
			s.Close()
			if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "done" {
				t.Errorf("%q", got)
			}
		})
	}
}

// The name a reprise took away from the trash, as the file was edited in place, is durable before the
// operation is called undone: otherwise it could come back with no row to explain it.
func TestANameThatGoesIsDurableBeforeTheUndo(t *testing.T) {
	b := newBench(t)
	b.leaveBothNames()
	if err := os.WriteFile(filepath.Join(b.dirs["t3"], "media", "a"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		if call == "sync" && folder == "1" {
			syncs++
			if syncs == 2 { // the first is on the way, before the look
				return syscall.EIO
			}
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops"); got != "planned" {
		t.Errorf("%q, with the folder it left never synced", got)
	}
}

// A purge finished by a reprise makes the unlink durable first.
func TestARepriseOfAPurgeSyncs(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.failNth("t3", "sync", 1, syscall.EIO)
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%+v", outcomes)
	}
	b.failSyncOf("1")
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "planned" {
		t.Errorf("%q", got)
	}
	delete(b.fail, b.dirs["t3"])
	s = b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "done" {
		t.Errorf("%q", got)
	}
}

// A folder made below the root, which a reprise finishes, is durable first as well.
func TestAMadeFolderIsDurable(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.prune()
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
		t.Fatal(err)
	}
	b.failNth("t3", "sync", 1, syscall.EIO)
	if outcomes, _ := b.restored("d/x"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%+v", outcomes)
	}
	// The restore comes first, by its id, and what comes after it fails.
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "sync" && b.text("SELECT state FROM ops WHERE kind = 'restore'") == "undone" {
			return syscall.EIO
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "planned" {
		t.Errorf("%q, with the way to it never synced", got)
	}
}

// A disk lost in a reprise stops it there: the operations after it are not looked at.
func TestALostDiskStopsTheReprise(t *testing.T) {
	b := newBench(t)
	b.addFiles("b")
	b.plan("a", "b")
	stats := 0
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "stat" {
			stats++
			return syscall.EIO
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if stats != 1 {
		t.Errorf("%d looks at a disk that was lost at the first", stats)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 2 {
		t.Errorf("%d left planned", got)
	}
}

// A folder of the trash someone made between the look and mkdir serves as well.
func TestAFolderOfTheTrashMadeMeanwhile(t *testing.T) {
	b := newBench(t)
	once := true
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "mkdir" && name == "Atoll" && once {
			once = false
			if err := os.Mkdir(filepath.Join(b.dirs["t3"], "Atoll"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Errorf("%+v", items)
	}
}

// A folder takes the exact mode and owner of the one it stands in for, which the umask would change.
func TestAFolderKeepsItsExactModeAndOwner(t *testing.T) {
	b := newBench(t)
	b.addFiles("p/x")
	folder := filepath.Join(b.dirs["t3"], "media", "p")
	if err := os.Chmod(folder, 0o777); err != nil {
		t.Fatal(err)
	}
	root := os.Geteuid() == 0
	if root {
		if err := os.Chown(folder, 1234, 1234); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	check := func(path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o777 {
			t.Errorf("%s: %v", path, info.Mode())
		}
		if st, ok := info.Sys().(*syscall.Stat_t); root && ok && st.Uid != 1234 {
			t.Errorf("%s: owner %d", path, st.Uid)
		}
	}
	b.prune()
	check(filepath.Join(b.dirs["t3"], "Atoll", "trash", day, "2", "p"))
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	if outcomes, _ := b.restored("p/x"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%+v", outcomes)
	}
	check(folder)
}

// The source's folder became another mount since the look: that name no longer leads to the copy.
func TestASourceFolderThatBecomesAMount(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	// The second operation, for d/x: the first is for a.
	b.at["journaled"] = func() {
		b.at["journaled"] = func() { b.otherMount[b.dirs["t3"]] = "d" }
	}
	items, _ := b.prune()
	if len(items) != 2 || items[0].Outcome != OutcomeTrashed || items[1].Outcome != OutcomeChanged {
		t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT state FROM ops WHERE id = 2"); got != "undone" {
		t.Errorf("%q", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
		t.Errorf("the disk is %q", got)
	}
}

// Gone between the look and the unlink, the file of a purge is what the purge wanted.
func TestAFileGoneBeforeItsUnlink(t *testing.T) {
	b := newBench(t)
	b.prune()
	once := true
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "unlink" && once {
			once = false
			if err := os.Remove(b.inTrash(1, "a")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomePurged {
		t.Errorf("%+v", outcomes)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "done" {
		t.Errorf("%q", got)
	}
}

// A lost disk on the look at a folder of the trash stops a restore before it writes anything.
func TestALostDiskBeforeAFolderIsMade(t *testing.T) {
	b := newBench(t)
	b.addFiles("d/x")
	b.prune()
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
		t.Fatal(err)
	}
	b.failStatAfter("journaled", "d", syscall.EIO)
	if outcomes, _ := b.restored("d/x"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%+v", outcomes)
	}
	if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "d")); !os.IsNotExist(err) {
		t.Errorf("a folder was made on a disk that was lost: %v", err)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
		t.Errorf("the disk is %q", got)
	}
}

// A read-only mount that only the confirmation finds: nothing is journaled on it, and what is planned
// there stays, and says so.
func TestAReadOnlyDiskFoundByItsConfirmation(t *testing.T) {
	readOnlyLater := func(b *bench) {
		looks := 0
		b.onMount = func(mount string) {
			if mount == b.dirs["t3"] {
				looks++
				b.ro[mount] = looks > 1
			}
		}
	}
	t.Run("a reprise", func(t *testing.T) {
		b := newBench(t)
		b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
		b.prune()
		delete(b.fail, b.dirs["t3"])
		readOnlyLater(b)
		_, summary := b.purgedAll()
		if !summary.DiskLost {
			t.Errorf("%+v", summary)
		}
		if got := b.text("SELECT state FROM ops"); got != "planned" {
			t.Errorf("%q", got)
		}
	})
	t.Run("a restore", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		readOnlyLater(b)
		if outcomes, _ := b.restored("a"); len(outcomes) != 1 || outcomes[0] != string(solver.PruneDiskUnavailable) {
			t.Errorf("%+v", outcomes)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind <> 'trash'"); got != 0 {
			t.Errorf("%d journaled", got)
		}
	})
	t.Run("a purge", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		readOnlyLater(b)
		if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != string(solver.PruneDiskUnavailable) {
			t.Errorf("%+v", outcomes)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind <> 'trash'"); got != 0 {
			t.Errorf("%d journaled", got)
		}
	})
}

// leaveBothNames stops a move on the path ntfs-3g takes between its link and its unlink, as a crash
// would: both names lead to the file, and the operation is planned.
func (b *bench) leaveBothNames() {
	b.t.Helper()
	syncs := 0
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		switch call {
		case "rename":
			return syscall.EINVAL
		case "sync":
			syncs++
			if syncs == syncsOfAFirstMove-1 {
				return syscall.EIO
			}
		}
		return nil
	}
	b.prune()
	delete(b.fail, b.dirs["t3"])
	if _, err := os.Lstat(b.inTrash(1, "a")); err != nil || !b.sourceThere() {
		b.t.Fatalf("both names should be there: %v", err)
	}
}

// What an operation left is made durable before it is called undone: the folders it made, which the
// next move goes through, and a name that went back (specs.md §17.2).
func TestAnUndoIsDurable(t *testing.T) {
	for n := 1; n <= syncsOfAFirstMove-2; n++ {
		t.Run("a folder it made/"+strconv.Itoa(n), func(t *testing.T) {
			b := newBench(t)
			b.failNth("t3", "sync", n, syscall.EIO)
			b.prune()
			b.failOn("t3", "sync", syscall.EIO)
			if _, summary := b.prune(); !summary.DiskLost {
				t.Errorf("%+v", summary)
			}
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "planned" {
				t.Errorf("undone with nothing synced: %q", got)
			}
			delete(b.fail, b.dirs["t3"])
			b.prune()
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "undone" {
				t.Errorf("%q", got)
			}
		})
	}
	for _, tc := range []struct {
		name string
		hook func(b *bench) func(call, name string) error
	}{
		{"on the rename path", func(b *bench) func(string, string) error {
			done := false
			return func(call, _ string) error {
				switch {
				case call == "rename" && !done:
					done = true
					b.replace(filepath.Join(b.dirs["t3"], "media", "a"), "yy")
				case call == "sync" && done:
					return syscall.EIO
				}
				return nil
			}
		}},
		{"on the link path", func(b *bench) func(string, string) error {
			syncs := 0
			return func(call, _ string) error {
				switch call {
				case "rename":
					return syscall.EINVAL
				case "sync":
					syncs++
					switch {
					case syncs == syncsOfAFirstMove-1:
						b.replace(filepath.Join(b.dirs["t3"], "media", "a"), "yy")
					case syncs >= syncsOfAFirstMove:
						return syscall.EIO
					}
				}
				return nil
			}
		}},
	} {
		t.Run("what went back/"+tc.name, func(t *testing.T) {
			b := newBench(t)
			b.fail[b.dirs["t3"]] = tc.hook(b)
			items, summary := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
				t.Fatalf("%+v, %+v", items, summary)
			}
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Errorf("undone with nothing synced: %q", got)
			}
			delete(b.fail, b.dirs["t3"])
			if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeChanged {
				t.Errorf("%+v", items)
			}
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "undone" {
				t.Errorf("%q", got)
			}
			if got := b.sourceText(); got != "yy" {
				t.Errorf("%q", got)
			}
		})
	}
}

// atTheLookBeforeTheSourceGoes runs what a test wants at the last look before a reprise removes the
// source's name: the third at that name, after one at each of the two places.
func (b *bench) atTheLookBeforeTheSourceGoes(fire func()) {
	looks := 0
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "stat" && name == "a" {
			looks++
			if looks == 3 {
				fire()
			}
		}
		return nil
	}
}

// A reprise looks at the source's name right before it goes: what took it since the first look is a
// new file, and keeps its only name (specs.md §17.2).
func TestAReplacementDuringTheReprise(t *testing.T) {
	b := newBench(t)
	b.leaveBothNames()
	b.atTheLookBeforeTheSourceGoes(func() { b.replace(filepath.Join(b.dirs["t3"], "media", "a"), "yy") })
	b.prune()
	if got := b.sourceText(); got != "yy" {
		t.Errorf("the new file: %q", got)
	}
	if got := b.text("SELECT state FROM ops"); got != "done" {
		t.Errorf("the operation is %q", got)
	}
	data, err := os.ReadFile(b.inTrash(1, "a"))
	if err != nil || string(data) != "x" {
		t.Errorf("in the trash: %q, %v", data, err)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
		t.Errorf("the disk is %q", got)
	}
}

// Likewise for a source removed in that time, which says nothing about the disk.
func TestASourceGoneDuringTheReprise(t *testing.T) {
	b := newBench(t)
	b.leaveBothNames()
	b.atTheLookBeforeTheSourceGoes(func() {
		if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "a")); err != nil {
			t.Fatal(err)
		}
	})
	b.prune()
	if got := b.text("SELECT state FROM ops"); got != "done" {
		t.Errorf("the operation is %q", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
		t.Errorf("the disk is %q", got)
	}
}

// The syncs of a reprise come before its looks, as they can be long: a file edited in place during
// them is seen, and stays live (specs.md §17.2).
func TestAnEditDuringTheSyncsOfAReprise(t *testing.T) {
	b := newBench(t)
	b.leaveBothNames()
	once := true
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "sync" && once {
			once = false
			if err := os.WriteFile(filepath.Join(b.dirs["t3"], "media", "a"), []byte("edited"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	b.prune()
	if got := b.sourceText(); got != "edited" {
		t.Errorf("%q", got)
	}
	if got := b.text("SELECT state FROM ops"); got != "undone" {
		t.Errorf("the operation is %q", got)
	}
	if _, err := os.Lstat(b.inTrash(1, "a")); !os.IsNotExist(err) {
		t.Errorf("the name in the trash: %v", err)
	}
}

// Gone during the fsync after the link, the source's name leaves the link as the file's only name: it
// stays, and the move is done, as a reprise would find it (specs.md §17.1, §17.2).
func TestASourceGoneDuringTheLink(t *testing.T) {
	t.Run("to the trash", func(t *testing.T) {
		b := newBench(t)
		syncs := 0
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			switch call {
			case "rename":
				return syscall.EINVAL
			case "sync":
				syncs++
				if syncs == syncsOfAFirstMove-1 {
					if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "a")); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeTrashed {
			t.Fatalf("%+v", items)
		}
		data, err := os.ReadFile(b.inTrash(1, "a"))
		if err != nil || string(data) != "x" {
			t.Errorf("in the trash: %q, %v", data, err)
		}
		if got := b.text("SELECT state FROM ops"); got != "done" {
			t.Errorf("the operation is %q", got)
		}
	})
	t.Run("back from it", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		once := true
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			switch call {
			case "rename":
				return syscall.EINVAL
			case "sync":
				if once {
					once = false
					if err := os.Remove(b.inTrash(1, "a")); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil
		}
		outcomes, _ := b.restored("a")
		if len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
			t.Fatalf("%+v", outcomes)
		}
		if got := b.sourceText(); got != "x" {
			t.Errorf("the file is gone: %q", got)
		}
	})
}

// A file edited in place after the look of a prune, while the folders of the trash are made, stays
// live, and nothing of it reaches the trash (specs.md §17.1).
func TestAnEditBeforeTheMove(t *testing.T) {
	source := func(b *bench) string { return filepath.Join(b.dirs["t3"], "media", "a") }
	edit := func(t *testing.T, b *bench) {
		t.Helper()
		if err := os.WriteFile(source(b), []byte("edited"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mtime := func(t *testing.T, b *bench) time.Time {
		t.Helper()
		info, err := os.Stat(source(b))
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}
	setMtime := func(t *testing.T, b *bench, at time.Time) {
		t.Helper()
		if err := os.Chtimes(source(b), at, at); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, b *bench, items []PruneItem, text string) {
		t.Helper()
		if len(items) != 1 || items[0].Outcome != OutcomeChanged {
			t.Fatalf("%+v", items)
		}
		if got := b.sourceText(); got != text {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM ops"); got != "undone" {
			t.Errorf("the operation is %q", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 0 {
			t.Errorf("%d entries", got)
		}
		if _, err := os.Lstat(b.inTrash(1, "a")); !os.IsNotExist(err) {
			t.Errorf("the name in the trash: %v", err)
		}
	}
	t.Run("before the rename", func(t *testing.T) {
		b := newBench(t)
		once := true
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call == "mkdir" && once {
				once = false
				edit(t, b)
			}
			return nil
		}
		items, _ := b.prune()
		check(t, b, items, "edited")
	})
	t.Run("a change of nothing but the change time", func(t *testing.T) {
		b := newBench(t)
		time.Sleep(20 * time.Millisecond) // the change time must be able to move, within a clock tick
		once := true
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call == "mkdir" && once {
				once = false
				if err := os.Chmod(source(b), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeChanged {
			t.Fatalf("%+v", items)
		}
		if !b.sourceThere() {
			t.Error("the copy went")
		}
	})
	// The link moved the change time, so between the link and the unlink only the size, the modification
	// time and the inode tell what was linked from what took its place, and each must tell alone.
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, b *bench)
		text   string
	}{
		{"between the link and the unlink", edit, "edited"},
		{"between the link and the unlink, keeping the modification time", func(t *testing.T, b *bench) {
			at := mtime(t, b)
			edit(t, b)
			setMtime(t, b, at)
		}, "edited"},
		{"between the link and the unlink, of the same size", func(t *testing.T, b *bench) {
			at := mtime(t, b)
			if err := os.WriteFile(source(b), []byte("y"), 0o644); err != nil {
				t.Fatal(err)
			}
			setMtime(t, b, at.Add(time.Second))
		}, "y"},
		{"replaced between the link and the unlink, of the same size and modification time", func(t *testing.T, b *bench) {
			at := mtime(t, b)
			b.replace(source(b), "y")
			setMtime(t, b, at)
		}, "y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			syncs := 0
			b.fail[b.dirs["t3"]] = func(call, _ string) error {
				switch call {
				case "rename":
					return syscall.EINVAL
				case "sync":
					syncs++
					if syncs == syncsOfAFirstMove-1 {
						tc.change(t, b)
					}
				}
				return nil
			}
			items, _ := b.prune()
			check(t, b, items, tc.text)
		})
	}
}

// The fsyncs of each kind of operation, in order: a file at the name it now has, or a folder just made,
// before the folder that holds it, as on NTFS its own record holds its names too, and the target before the
// source, so that the file has a name at every moment (specs.md §17.1).
func TestTheOrderOfSyncs(t *testing.T) {
	record := func(b *bench, link bool) *[]string {
		names := &[]string{}
		b.fail[b.dirs["t3"]] = func(call, name string) error {
			switch {
			case call == "sync":
				*names = append(*names, name)
			case call == "syncentry":
				*names = append(*names, "entry "+name)
			case call == "rename" && link:
				return syscall.EINVAL
			}
			return nil
		}
		return names
	}
	check := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "/") != strings.Join(want, "/") {
			t.Errorf("%q, want %q", got, want)
		}
	}
	for _, link := range []bool{false, true} {
		t.Run(fmt.Sprintf("link %v", link), func(t *testing.T) {
			b := newBench(t)
			names := record(b, link)
			b.prune()
			// Each folder made is synced, then its parent, before anything goes through it.
			folders := []string{"entry Atoll", "", "entry trash", "Atoll", "entry " + day, "trash", "entry 1", day}
			want := append(slices.Clone(folders), "entry a", "1", "media")
			if link {
				want = append(slices.Clone(folders), "entry a", "1", "entry a", "1", "media")
			}
			check(t, *names, want...)
			names = record(b, link)
			b.restored("a")
			want = []string{"entry a", "media", "1"}
			if link {
				want = []string{"entry a", "media", "entry a", "media", "1"}
			}
			check(t, *names, want...)
		})
	}
	t.Run("a purge", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		names := record(b, false)
		b.purgedAll()
		check(t, *names, "1")
	})
	t.Run("a restore that makes a folder", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("d/x")
		b.prune()
		if err := os.RemoveAll(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
			t.Fatal(err)
		}
		names := record(b, false)
		if outcomes, _ := b.restored("d/x"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
			t.Fatalf("%+v", outcomes)
		}
		if len(*names) < 4 {
			t.Fatalf("%q", *names)
		}
		check(t, (*names)[:4], "entry d", "media", "entry x", "d")
	})
}

// The sync of the file at its new name counts as any fsync: one that fails leaves the operation planned and
// the disk lost, and the reprise that finishes the move syncs the file again, as the crash or the failure
// may have come before that (specs.md §17.1, §17.2).
func TestTheSyncOfTheMovedFileCounts(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(fmt.Sprintf("link %v", link), func(t *testing.T) {
			b := newBench(t)
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				switch {
				case call == "syncentry" && name == "a":
					return syscall.EIO
				case call == "rename" && link:
					return syscall.EINVAL
				}
				return nil
			}
			items, summary := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
				t.Fatalf("%+v, %+v", items, summary)
			}
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Errorf("the operation is %q", got)
			}
			entries := 0
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				if call == "syncentry" && name == "a" {
					entries++
				}
				return nil
			}
			b.prune()
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "done" {
				t.Errorf("the operation is %q", got)
			}
			if entries == 0 {
				t.Error("the reprise did not sync the file")
			}
			if data, err := os.ReadFile(b.inTrash(1, "a")); err != nil || string(data) != "x" {
				t.Errorf("in the trash: %q, %v", data, err)
			}
		})
	}
}

// A reprise that takes one of two names away syncs the file at the name it keeps before the folder of the
// name that went: its own record lost a name (specs.md §17.2). Whether the move finishes, or is undone as
// the file was edited in place.
func TestAReprisesSyncsTheFileAfterANameGoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  bool
		state string
	}{
		{"finished", false, "done"},
		{"undone", true, "undone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.leaveBothNames()
			if tc.edit {
				if err := os.WriteFile(filepath.Join(b.dirs["t3"], "media", "a"), []byte("edited"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				switch call {
				case "unlink", "syncentry", "sync":
					calls = append(calls, call+" "+name)
				}
				return nil
			}
			b.prune()
			if got := b.text("SELECT state FROM ops WHERE id = 1"); got != tc.state {
				t.Fatalf("the operation is %q", got)
			}
			at := slices.Index(calls, "unlink a")
			if at < 0 || len(calls) < at+3 || !slices.Equal(calls[at+1:at+3], []string{"syncentry a", "sync media"}) {
				t.Errorf("%q", calls)
			}
		})
	}
}

// A name that unlink says is not there, while a look right after still finds the file at it, is a name its
// record no longer holds: ntfs3 leaves that when a device fails between the syncs of a move's two folders,
// and then answers unlink with ENOENT and removes nothing (measured). The reprise concludes nothing, and
// the operation stays planned until a repair of the volume says more (specs.md §17.2).
func TestANameThatUnlinkCannotRemove(t *testing.T) {
	b := newBench(t)
	b.leaveBothNames()
	stuck := true
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "unlink" && name == "a" && stuck {
			return syscall.ENOENT
		}
		return nil
	}
	b.prune()
	if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "planned" {
		t.Errorf("the operation is %q", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 1 {
		t.Errorf("%d rows on t3", got)
	}
	if !b.sourceThere() {
		t.Error("the source's name went")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
		t.Errorf("the disk is %q", got)
	}
	stuck = false
	b.prune()
	if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "done" {
		t.Errorf("once repaired, the operation is %q", got)
	}
}

// What the sync of a moved file cannot open: a name gone, or one Atoll may not open, says nothing about the
// disk, and the move goes on with its folder's sync; any other error from the open is judged as any call's.
func TestAMovedFileThatCannotBeOpenedToSync(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
		lost    bool
	}{
		{"gone", syscall.ENOENT, OutcomeTrashed, false},
		{"not allowed", syscall.EACCES, OutcomeTrashed, false},
		{"a device gone", syscall.EIO, OutcomeIOError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				if call == "syncentry" && name == "a" {
					return fmt.Errorf("%w: %w", fsx.ErrNotOpened, tc.err)
				}
				return nil
			}
			items, summary := b.prune()
			if len(items) != 1 || items[0].Outcome != tc.outcome || summary.DiskLost != tc.lost {
				t.Errorf("%+v, %+v", items, summary)
			}
		})
	}
}

// A move back that fails on its first fsync leaves the file's name in the trash: it never has no name.
func TestAMoveBackIsDurableBeforeItsNameGoes(t *testing.T) {
	b := newBench(t)
	b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
	b.prune()
	b.replace(b.inTrash(1, "a"), "foreign")
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		switch {
		case call == "rename":
			return syscall.EINVAL
		case call == "sync" && folder == "media":
			return syscall.EIO
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if _, err := os.Lstat(b.inTrash(1, "a")); err != nil {
		t.Errorf("its name in the trash went before the other was durable: %v", err)
	}
	if got := b.text("SELECT state FROM ops"); got != "planned" {
		t.Errorf("the operation is %q", got)
	}
}

// A move back syncs the file at the name it gets back before that name's folder, on both paths, as its own
// record holds its names (specs.md §17.2).
func TestAMoveBackSyncsTheFileFirst(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(fmt.Sprintf("link %v", link), func(t *testing.T) {
			b := newBench(t)
			b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
			b.prune()
			b.replace(b.inTrash(1, "a"), "foreign")
			var calls []string
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				switch call {
				case "rename", "link", "unlink", "syncentry", "sync":
					calls = append(calls, call+" "+name)
				}
				if link && call == "rename" {
					return syscall.EINVAL
				}
				return nil
			}
			s := b.opened()
			s.Close()
			want := []string{"rename a", "syncentry a", "sync media", "sync 1"}
			if link {
				want = []string{"rename a", "link a", "syncentry a", "sync media", "unlink a", "syncentry a", "sync media", "sync 1"}
			}
			at := slices.Index(calls, "rename a")
			if at < 0 || len(calls) < at+len(want) || !slices.Equal(calls[at:at+len(want)], want) {
				t.Errorf("%q", calls)
			}
		})
	}
}

// An error that says nothing of the disk, on the way back or on the link that goes: the disk stays.
func TestAFailureOnTheWayBackKeepsTheDisk(t *testing.T) {
	t.Run("the move back", func(t *testing.T) {
		b := newBench(t)
		renames := 0
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call != "rename" {
				return nil
			}
			renames++
			switch renames {
			case 1:
				b.replace(filepath.Join(b.dirs["t3"], "media", "a"), "yy")
			case 2:
				return syscall.EPERM
			}
			return nil
		}
		items, summary := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("the disk is %q", got)
		}
		if got := b.text("SELECT state FROM ops"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
		delete(b.fail, b.dirs["t3"])
		b.prune()
		if got := b.sourceText(); got != "yy" {
			t.Errorf("the new file: %q", got)
		}
	})
	t.Run("the link that goes", func(t *testing.T) {
		b := newBench(t)
		syncs := 0
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			switch call {
			case "rename":
				return syscall.EINVAL
			case "sync":
				syncs++
				if syncs == syncsOfAFirstMove-1 {
					b.replace(filepath.Join(b.dirs["t3"], "media", "a"), "yy")
				}
			case "unlink":
				return syscall.EPERM
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 {
			t.Fatalf("%+v", items)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("the disk is %q", got)
		}
		if got := b.sourceText(); got != "yy" {
			t.Errorf("the new file: %q", got)
		}
	})
}

// What stands in a folder's place in the trash says nothing of the mode a folder should have.
func TestATrashFolderThatIsNoFolder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func(path string) error
	}{
		{"a symlink", func(path string) error { return os.Symlink("elsewhere", path) }},
		{"a file", func(path string) error { return os.WriteFile(path, []byte("f"), 0o600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.addFiles("d/x")
			b.prune()
			media := filepath.Join(b.dirs["t3"], "media", "d")
			if err := os.Remove(media); err != nil {
				t.Fatal(err)
			}
			folder := b.inTrash(2, "d")
			if err := os.Rename(folder, folder+".away"); err != nil {
				t.Fatal(err)
			}
			if err := tc.place(folder); err != nil {
				t.Fatal(err)
			}
			if outcomes, _ := b.restored("d/x"); len(outcomes) != 1 || outcomes[0] != OutcomeChanged {
				t.Fatalf("%+v", outcomes)
			}
			info, err := os.Stat(media)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Errorf("%v", info.Mode())
			}
		})
	}
}

// With a private root, the operation's folder is what keeps a copy private in the trash.
func TestAPrivateRoot(t *testing.T) {
	for _, adopt := range []bool{true, false} {
		t.Run(fmt.Sprintf("adopt %v", adopt), func(t *testing.T) {
			b := newBench(t)
			b.addFiles("d/x")
			if err := os.Chmod(filepath.Join(b.dirs["t3"], "media"), 0o700); err != nil {
				t.Fatal(err)
			}
			if !adopt {
				b.failOn("t3", "adopt", syscall.EPERM)
			}
			b.prune()
			for _, op := range []int{1, 2} {
				folder := filepath.Join(b.dirs["t3"], "Atoll", "trash", day, strconv.Itoa(op))
				info, err := os.Stat(folder)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o700 {
					t.Errorf("%s: %v", folder, info.Mode())
				}
			}
		})
	}
}

// Each level takes the mode of its own folder.
func TestAPrivateFolderTwoLevelsDown(t *testing.T) {
	b := newBench(t)
	b.addFiles("p/q/x")
	media := filepath.Join(b.dirs["t3"], "media")
	if err := os.Chmod(filepath.Join(media, "p"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(media, "p", "q"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.prune()
	mode := func(path string) os.FileMode {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	if got := mode(b.inTrash(2, "p")); got != 0o750 {
		t.Errorf("p in the trash: %v", got)
	}
	if got := mode(b.inTrash(2, "p/q")); got != 0o700 {
		t.Errorf("q in the trash: %v", got)
	}
	if err := os.RemoveAll(filepath.Join(media, "p")); err != nil {
		t.Fatal(err)
	}
	if outcomes, _ := b.restored("p/q/x"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%+v", outcomes)
	}
	if got := mode(filepath.Join(media, "p")); got != 0o750 {
		t.Errorf("p back: %v", got)
	}
	if got := mode(filepath.Join(media, "p", "q")); got != 0o700 {
		t.Errorf("q back: %v", got)
	}
}

// An operation that stays planned leaves the next one to be looked at (specs.md §17.2).
func TestARepriseGoesOnAfterAPuzzle(t *testing.T) {
	b := newBench(t)
	b.addFiles("b")
	b.plan("a", "b")
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "stat" && name == "a" {
			return syscall.EACCES
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE id = 1"); got != "planned" {
		t.Errorf("the first is %q", got)
	}
	if got := b.text("SELECT state FROM ops WHERE id = 2"); got != "undone" {
		t.Errorf("the second is %q", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
		t.Errorf("the disk is %q", got)
	}
}

// A restored copy has the creation time it had, by which it may be the original again (specs.md §7).
func TestARestoredCopyKeepsItsCreationTime(t *testing.T) {
	b := newBench(t)
	var before sql.NullInt64
	if err := b.db.QueryRowContext(t.Context(), "SELECT btime_ns FROM copies WHERE disk_id = 't3'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !before.Valid {
		t.Skip("this filesystem gives no creation time")
	}
	b.prune()
	if outcomes, _ := b.restored("a"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%+v", outcomes)
	}
	var after sql.NullInt64
	if err := b.db.QueryRowContext(t.Context(), "SELECT btime_ns FROM copies WHERE disk_id = 't3'").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("%v, want %v", after, before)
	}
}

// EIO when a new folder takes its mode or owner is a disk that failed, not a driver that refuses.
func TestALostDiskAtAdopt(t *testing.T) {
	t.Run("on the way to the trash", func(t *testing.T) {
		b := newBench(t)
		b.failOn("t3", "adopt", syscall.EIO)
		items, summary := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM ops"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
			t.Errorf("the disk is %q", got)
		}
	})
	t.Run("on the way back", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("d/x")
		b.prune()
		if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
			t.Fatal(err)
		}
		b.failOn("t3", "adopt", syscall.EIO)
		outcomes, summary := b.restored("d/x")
		if len(outcomes) != 1 || outcomes[0] != OutcomeIOError || !summary.DiskLost {
			t.Fatalf("%+v, %+v", outcomes, summary)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "planned" {
			t.Errorf("the folder is %q", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
			t.Errorf("the disk is %q", got)
		}
	})
}

// A folder of the trash that cannot be looked at says nothing of the mode it has: the restore stops
// before it makes anything, so that a private folder never comes back open.
func TestAFolderOfTheTrashThatCannotBeLookedAt(t *testing.T) {
	setUp := func(t *testing.T) *bench {
		t.Helper()
		b := newBench(t)
		b.addFiles("p/x")
		folder := filepath.Join(b.dirs["t3"], "media", "p")
		if err := os.Chmod(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
			t.Fatal(err)
		}
		b.prune()
		if err := os.Remove(folder); err != nil {
			t.Fatal(err)
		}
		return b
	}
	check := func(t *testing.T, b *bench) {
		t.Helper()
		if outcomes, _ := b.restored("p/x"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
			t.Fatalf("%+v", outcomes)
		}
		if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "p")); !os.IsNotExist(err) {
			t.Errorf("a folder was made: %v", err)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
			t.Errorf("%d left planned", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 2 {
			t.Errorf("%d entries", got)
		}
	}
	t.Run("on another mount", func(t *testing.T) {
		b := setUp(t)
		b.otherMount[b.dirs["t3"]] = "Atoll"
		b.fail[b.dirs["t3"]] = nothingMoves(t)
		check(t, b)
	})
	t.Run("refused", func(t *testing.T) {
		b := setUp(t)
		b.failStatAfter("journaled", "p", syscall.EACCES)
		check(t, b)
	})
}

// Undone at the second look of a reprise, a move is durable like any other that is undone: the name
// that went must not come back with no row to explain it (specs.md §17.2).
func TestAnUndoAtTheSecondLookIsDurable(t *testing.T) {
	for _, tc := range []struct {
		name, source, target string
		first                func(b *bench)
		goes                 func(b *bench) string
		kind                 string
	}{
		{
			"to the trash", "media", "1",
			func(b *bench) {
				b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
				b.prune()
			},
			func(b *bench) string { return b.inTrash(1, "a") },
			"trash",
		},
		{
			"back from it", "1", "media",
			func(b *bench) {
				b.prune()
				b.failNth("t3", "sync", 1, syscall.EIO)
				b.restored("a")
			},
			func(b *bench) string { return filepath.Join(b.dirs["t3"], "media", "a") },
			"restore",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			tc.first(b)
			gone := false
			b.fail[b.dirs["t3"]] = func(call, folder string) error {
				switch {
				case call != "sync":
				case folder == tc.source && !gone:
					gone = true
					if err := os.Remove(tc.goes(b)); err != nil {
						t.Fatal(err)
					}
				case folder == tc.target && gone:
					return syscall.EIO
				}
				return nil
			}
			s := b.opened()
			s.Close()
			if got := b.text("SELECT state FROM ops WHERE kind = '" + tc.kind + "' ORDER BY id DESC LIMIT 1"); got != "planned" {
				t.Errorf("%q, with the folder of the name that went never synced", got)
			}
		})
	}
}

// A disk lost at the look of a prune or a restore, outside a reprise: nothing is concluded of what it
// holds, and it is lost for the rest of the run.
func TestALostDiskAtALook(t *testing.T) {
	lostAtTheLook := func(b *bench) {
		b.fail[b.dirs["t3"]] = func(call, name string) error {
			if call == "stat" && name == "a" {
				return syscall.EIO
			}
			return nil
		}
	}
	t.Run("prune", func(t *testing.T) {
		b := newBench(t)
		lostAtTheLook(b)
		items, summary := b.prune()
		if len(items) != 1 || items[0].Outcome != string(solver.PruneDiskUnavailable) || !summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
			t.Errorf("the disk is %q", got)
		}
	})
	t.Run("restore", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		lostAtTheLook(b)
		outcomes, summary := b.restored("a")
		if len(outcomes) != 1 || outcomes[0] != string(solver.PruneDiskUnavailable) || !summary.DiskLost {
			t.Fatalf("%+v, %+v", outcomes, summary)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
			t.Errorf("the disk is %q", got)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'restore'"); got != 0 {
			t.Errorf("%d journaled", got)
		}
	})
}

// A parent that changes after the look gives the same outcome at any depth: a file there takes the path,
// and a folder gone is another error (specs.md §17.4).
func TestAParentThatChangesAfterTheLook(t *testing.T) {
	for _, path := range []string{"d/x", "d/sub/x"} {
		t.Run("a file there/"+path, func(t *testing.T) {
			b := newBench(t)
			b.addFiles(path)
			b.prune()
			if err := os.RemoveAll(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
				t.Fatal(err)
			}
			b.at["journaled"] = func() {
				if err := os.WriteFile(filepath.Join(b.dirs["t3"], "media", "d"), []byte("f"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if outcomes, _ := b.restored(path); len(outcomes) != 1 || outcomes[0] != OutcomePathTaken {
				t.Errorf("%+v", outcomes)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d left planned", got)
			}
		})
	}
	t.Run("a folder gone", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("d/x")
		b.prune()
		b.at["journaled"] = func() {
			if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
				t.Fatal(err)
			}
		}
		if outcomes, _ := b.restored("d/x"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
			t.Errorf("%+v", outcomes)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
			t.Errorf("%q", got)
		}
	})
}

// A time the source's folder takes while the folders of the trash are made is the user's, and stays.
func TestTheFolderTimeIsReadRightBeforeTheMove(t *testing.T) {
	b := newBench(t)
	media := filepath.Join(b.dirs["t3"], "media")
	first := time.Unix(1500000000, 0)
	if err := os.Chtimes(media, first, first); err != nil {
		t.Fatal(err)
	}
	second := time.Unix(1600000000, 0)
	once := true
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "mkdir" && once {
			once = false
			if err := os.Chtimes(media, second, second); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	info, err := os.Stat(media)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(second) {
		t.Errorf("%v", info.ModTime())
	}
}

// A reprise makes the link durable before the source's name goes.
func TestARepriseSyncsBeforeTheSourceGoes(t *testing.T) {
	b := newBench(t)
	b.leaveBothNames()
	b.failOn("t3", "sync", syscall.EIO)
	if _, summary := b.prune(); !summary.DiskLost {
		t.Errorf("%+v", summary)
	}
	if !b.sourceThere() {
		t.Error("the source's name went before the link was durable")
	}
	if got := b.text("SELECT state FROM ops"); got != "planned" {
		t.Errorf("the operation is %q", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 1 {
		t.Errorf("%d rows: the reprise concluded something", got)
	}
}

// What a reprise finished gets the same as what a run did: the original, policy and state of each
// file are worked out again.
func TestARepriseSettles(t *testing.T) {
	b := newBench(t)
	b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
	b.prune()
	delete(b.fail, b.dirs["t3"])
	if _, err := b.db.ExecContext(t.Context(), "UPDATE files SET original_disk = 't3'"); err != nil {
		t.Fatal(err)
	}
	if items, _ := b.prune(); len(items) != 0 {
		t.Fatalf("the reprise is the only thing that may have moved: %+v", items)
	}
	if got := b.text("SELECT state FROM ops"); got != "done" {
		t.Fatalf("%q", got)
	}
	if got := b.text("SELECT original_disk FROM files"); got == "t3" {
		t.Error("the original is still the copy that went")
	}
}

// A failure after the move, that a look shows the move survived: the operation finishes there, and
// the outcome says so (specs.md §17.1).
func TestAFailureThatALookFinishes(t *testing.T) {
	// The nth look after the move fails, once: the first checks the target, and the last says what
	// the move left.
	lookFails := func(b *bench, n int) {
		moved, looks := false, 0
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			switch {
			case call == "rename":
				moved = true
			case call == "stat" && moved:
				looks++
				if looks == n {
					return syscall.EACCES
				}
			}
			return nil
		}
	}
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("a copy going to the trash/%d", n), func(t *testing.T) {
			b := newBench(t)
			if _, err := b.db.ExecContext(t.Context(), "UPDATE files SET original_disk = 't3'"); err != nil {
				t.Fatal(err)
			}
			lookFails(b, n)
			items, summary := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeTrashed || summary.Stayed != 0 || summary.Bytes != 1 || summary.DiskLost {
				t.Fatalf("%+v, %+v", items, summary)
			}
			if got := b.text("SELECT state FROM ops"); got != "done" {
				t.Errorf("the operation is %q", got)
			}
			if got := b.text("SELECT original_disk FROM files"); got == "t3" {
				t.Error("the original was not worked out again")
			}
			if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
				t.Errorf("the disk is %q", got)
			}
		})
		t.Run(fmt.Sprintf("a restore/%d", n), func(t *testing.T) {
			b := newBench(t)
			b.prune()
			lookFails(b, n)
			outcomes, summary := b.restored("a")
			if len(outcomes) != 1 || outcomes[0] != OutcomeRestored || summary.Stayed != 0 || summary.DiskLost {
				t.Fatalf("%+v, %+v", outcomes, summary)
			}
			if got := b.text("SELECT state FROM copies WHERE disk_id = 't3'"); got != string(solver.CopySurplus) {
				t.Errorf("the plan was not worked out again: %q", got)
			}
		})
	}
}

// nothingMoves fails a test on any call that moves or makes something.
func nothingMoves(t *testing.T) func(call, name string) error {
	t.Helper()
	return func(call, name string) error {
		if call == "rename" || call == "link" || call == "mkdir" {
			t.Errorf("%s %q, through another mount", call, name)
		}
		return nil
	}
}

// A folder on another mount stops an operation before anything moves, and no look can go through it:
// the operation is over, and nothing piles up from one run to the next (specs.md §17.1).
func TestAFolderOnAnotherMount(t *testing.T) {
	t.Run("the trash", func(t *testing.T) {
		b := newBench(t)
		if err := os.MkdirAll(filepath.Join(b.dirs["t3"], "Atoll"), 0o755); err != nil {
			t.Fatal(err)
		}
		b.otherMount[b.dirs["t3"]] = "Atoll"
		b.fail[b.dirs["t3"]] = nothingMoves(t)
		for range 2 {
			if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeIOError {
				t.Fatalf("%+v", items)
			}
		}
		if got := b.count("SELECT count(*) FROM ops WHERE state = 'undone'"); got != 2 {
			t.Errorf("%d undone", got)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
			t.Errorf("%d left planned", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 0 {
			t.Errorf("%d rows of the trash", got)
		}
		if !b.sourceThere() {
			t.Error("the copy moved")
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("the disk is %q", got)
		}
	})
	t.Run("the trash, for a restore", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.otherMount[b.dirs["t3"]] = "Atoll"
		b.fail[b.dirs["t3"]] = nothingMoves(t)
		for range 2 {
			if outcomes, _ := b.restored("a"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
				t.Fatalf("%+v", outcomes)
			}
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'restore' AND state = 'undone'"); got != 2 {
			t.Errorf("%d undone", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 1 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("the way to the path", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("d/x")
		b.prune()
		b.otherMount[b.dirs["t3"]] = "d"
		b.fail[b.dirs["t3"]] = nothingMoves(t)
		if outcomes, _ := b.restored("d/x"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
			t.Fatalf("%+v", outcomes)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind IN ('restore', 'mkdir')"); got != 0 {
			t.Errorf("%d journaled", got)
		}
	})
}

// A name that appears at the path after the look: RENAME_NOREPLACE and link are what keep it.
func TestANameThatAppearsBeforeTheMove(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(fmt.Sprintf("link %v", link), func(t *testing.T) {
			b := newBench(t)
			b.prune()
			once := true
			b.fail[b.dirs["t3"]] = func(call, _ string) error {
				if call != "rename" {
					return nil
				}
				if once {
					once = false
					b.write("t3", "a", "new")
				}
				if link {
					return syscall.EINVAL
				}
				return nil
			}
			if outcomes, _ := b.restored("a"); len(outcomes) != 1 || outcomes[0] != OutcomePathTaken {
				t.Fatalf("%+v", outcomes)
			}
			if got := b.sourceText(); got != "new" {
				t.Errorf("%q", got)
			}
			if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
				t.Errorf("%q", got)
			}
			if got := b.count("SELECT count(*) FROM trash"); got != 1 {
				t.Errorf("%d entries", got)
			}
			data, err := os.ReadFile(b.inTrash(1, "a"))
			if err != nil || string(data) != "x" {
				t.Errorf("in the trash: %q, %v", data, err)
			}
		})
	}
}

// Another file in the trash's place just before the move: what reached the path is not the entry, and
// is not recorded as it (specs.md §17.4).
func TestAnEntryReplacedBeforeTheMove(t *testing.T) {
	b := newBench(t)
	b.prune()
	once := true
	b.fail[b.dirs["t3"]] = func(call, _ string) error {
		if call == "rename" && once {
			once = false
			b.replace(b.inTrash(1, "a"), "foreign")
		}
		return nil
	}
	if outcomes, _ := b.restored("a"); len(outcomes) != 1 || outcomes[0] != OutcomeChanged {
		t.Fatalf("%+v", outcomes)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
		t.Errorf("%q", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events", got)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 1 {
		t.Errorf("%d entries", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 0 {
		t.Errorf("%d rows for a file that is not a copy", got)
	}
	if got := b.text("SELECT state FROM files"); got == "conflict" {
		t.Errorf("%q", got)
	}
}

// Any fsync of a restore that fails: the same as for a copy going to the trash.
func TestEverySyncOfARestoreCounts(t *testing.T) {
	for _, tc := range []struct {
		path   string
		folder bool
		syncs  int
	}{{"a", false, 2}, {"d/x", true, 3}} {
		for n := 1; n <= tc.syncs; n++ {
			t.Run(fmt.Sprintf("%s/%d", tc.path, n), func(t *testing.T) {
				b := newBench(t)
				b.addFiles("d/x")
				b.prune()
				if tc.folder {
					if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
						t.Fatal(err)
					}
				}
				b.failNth("t3", "sync", n, syscall.EIO)
				outcomes, summary := b.restored(tc.path)
				if len(outcomes) != 1 || outcomes[0] != OutcomeIOError || !summary.DiskLost {
					t.Fatalf("%+v, %+v", outcomes, summary)
				}
				if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "planned" {
					t.Errorf("the restore is %q", got)
				}
				if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
					t.Errorf("the disk is %q", got)
				}
				delete(b.fail, b.dirs["t3"])
				b.restored(tc.path)
				if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
					t.Errorf("%d left planned", got)
				}
				data, err := os.ReadFile(filepath.Join(b.dirs["t3"], "media", filepath.FromSlash(tc.path)))
				if err != nil || len(data) == 0 {
					t.Errorf("not back: %q, %v", data, err)
				}
				if got := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't3' AND f.lpath = CAST('" + tc.path + "' AS BLOB)"); got != 1 {
					t.Errorf("%d rows", got)
				}
			})
		}
	}
}

// A restore finishing in a reprise whose file went during its last fsync: no copy is recorded for a
// file that is not there (specs.md §17.2).
func TestARestoredFileGoneDuringTheReprise(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.failNth("t3", "sync", 1, syscall.EIO)
	b.restored("a")
	if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "planned" {
		t.Fatalf("%q", got)
	}
	once := true
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		if call == "sync" && folder == "1" && once {
			once = false
			if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "a")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
		t.Errorf("the restore is %q", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 0 {
		t.Errorf("%d rows for a file that is not there", got)
	}
	if got := b.text("SELECT state FROM files"); got == "conflict" {
		t.Errorf("%q", got)
	}
}

// Replaced during the same syncs, what is there now is not what came back either.
func TestARestoredFileReplacedDuringTheReprise(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.failNth("t3", "sync", 1, syscall.EIO)
	b.restored("a")
	once := true
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		if call == "sync" && folder == "1" && once {
			once = false
			b.replace(filepath.Join(b.dirs["t3"], "media", "a"), "yy")
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
		t.Errorf("the restore is %q", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 0 {
		t.Errorf("%d rows for a file that is not a copy", got)
	}
}

// The same for a copy going to the trash: the name in the trash went during the syncs, and the
// operation is undone, so that the entry does not lead nowhere.
func TestATrashedFileGoneDuringTheReprise(t *testing.T) {
	b := newBench(t)
	b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
	b.prune()
	once := true
	b.fail[b.dirs["t3"]] = func(call, folder string) error {
		if call == "sync" && folder == "media" && once {
			once = false
			if err := os.Remove(b.inTrash(1, "a")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops"); got != "undone" {
		t.Errorf("the operation is %q", got)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries lead nowhere", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events", got)
	}
}

// An I/O error on a call that writes: the same, and nothing concluded about the copy.
func TestAnIOErrorLeavesTheOperationPlanned(t *testing.T) {
	for _, tc := range []struct {
		call string
		err  error
	}{
		{"mkdir", syscall.EIO},
		{"rename", syscall.EIO},
		{"rename", syscall.EROFS},
		{"rename", syscall.ENOTCONN},
		{"rename", syscall.ENODEV},
		{"rename", syscall.ENXIO},
		{"rename", syscall.ESTALE},
		{"setmtime", syscall.EIO},
	} {
		t.Run(tc.call+"/"+tc.err.Error(), func(t *testing.T) {
			b := newBench(t)
			b.failOn("t3", tc.call, tc.err)
			items, summary := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
				t.Fatalf("%+v, %+v", items, summary)
			}
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Errorf("the operation is %q", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 1 {
				t.Errorf("%d rows for the copy: nothing is concluded", got)
			}
			delete(b.fail, b.dirs["t3"])
			b.prune()
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 0 {
				t.Errorf("%d rows for a copy in the trash", got)
			}
			if b.sourceThere() {
				t.Error("the copy did not go")
			}
		})
	}
}

// On the path ntfs-3g always takes: the link is made durable before the source's name goes, and a
// failure at each step leaves a state a reprise finishes (specs.md §17.1).
func TestEveryStepOfALink(t *testing.T) {
	fail := func(b *bench, call string, n int, err error) {
		seen := 0
		b.fail[b.dirs["t3"]] = func(c, _ string) error {
			if c == "rename" {
				return syscall.EINVAL
			}
			if c == call {
				seen++
				if seen == n {
					return err
				}
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name     string
		call     string
		n        int
		err      error
		bothKept bool
	}{
		{"link lost", "link", 1, syscall.EIO, false},
		{"the fsync after the link", "sync", 5, syscall.EIO, true},
		{"the unlink", "unlink", 1, syscall.EIO, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			fail(b, tc.call, tc.n, tc.err)
			items, _ := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeIOError {
				t.Fatalf("%+v", items)
			}
			if !b.sourceThere() {
				t.Error("the source's name went before the link was durable")
			}
			inTrash := filepath.Join(b.dirs["t3"], "Atoll", "trash", day, "1", "a")
			if _, err := os.Lstat(inTrash); (err == nil) != tc.bothKept {
				t.Errorf("the name in the trash: %v", err)
			}
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Errorf("the operation is %q", got)
			}
			delete(b.fail, b.dirs["t3"])
			b.prune()
			if b.sourceThere() {
				t.Error("the copy did not go")
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 0 {
				t.Errorf("%d rows for a copy in the trash", got)
			}
		})
	}
	t.Run("a link that is refused for another reason", func(t *testing.T) {
		b := newBench(t)
		fail(b, "link", 1, syscall.EPERM)
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeIOError {
			t.Fatalf("%+v", items)
		}
		if got := b.text("SELECT state FROM ops"); got != "undone" {
			t.Errorf("the operation is %q", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("the disk is %q", got)
		}
	})
	t.Run("an unlink refused once says nothing of the disk", func(t *testing.T) {
		b := newBench(t)
		fail(b, "unlink", 1, syscall.EPERM)
		items, summary := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeTrashed || summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("the disk is %q", got)
		}
	})
	t.Run("an unlink refused for good leaves the operation planned", func(t *testing.T) {
		b := newBench(t)
		b.fail[b.dirs["t3"]] = func(c, _ string) error {
			switch c {
			case "rename":
				return syscall.EINVAL
			case "unlink":
				return syscall.EPERM
			}
			return nil
		}
		items, summary := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeIOError || !summary.DiskLost {
			t.Fatalf("%+v, %+v", items, summary)
		}
		if got := b.text("SELECT state FROM ops"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("the disk is %q", got)
		}
		if !b.sourceThere() {
			t.Error("the copy went")
		}
	})
}

// A write that fails with the disk still there: the copy stays, and the session goes on.
func TestAFailedWriteDoesNotLoseTheDisk(t *testing.T) {
	for _, tc := range []struct {
		call string
		err  error
	}{
		{"mkdir", syscall.ENOSPC},
		{"mkdir", syscall.EACCES},
		{"rename", syscall.ENOSPC},
		{"rename", syscall.EXDEV},
		{"rename", syscall.EPERM},
	} {
		t.Run(tc.call+"/"+tc.err.Error(), func(t *testing.T) {
			b := newBench(t)
			b.failOn("t3", tc.call, tc.err)
			items, _ := b.prune()
			if len(items) != 1 || items[0].Outcome != OutcomeIOError {
				t.Fatalf("%+v", items)
			}
			if got := b.text("SELECT state FROM ops"); got != "undone" {
				t.Errorf("the operation is %q", got)
			}
			if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
				t.Errorf("the disk is %q", got)
			}
			if got := b.count("SELECT count(*) FROM trash"); got != 0 {
				t.Errorf("%d entries", got)
			}
			if !b.sourceThere() {
				t.Error("the copy moved")
			}
		})
	}
}

// A time that cannot be restored is no reason to give up: a folder's date protects nothing.
func TestAFolderTimeIsNotWorthAnError(t *testing.T) {
	b := newBench(t)
	b.failOn("t3", "setmtime", syscall.EPERM)
	items, _ := b.prune()
	if len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
}

// A disk that fails the read past the cache is not written to, though its mount check passes.
func TestConfirmationStopsASession(t *testing.T) {
	b := newBench(t)
	b.unread[b.dirs["t3"]] = true
	items, summary := b.prune()
	if len(items) != 1 || items[0].Outcome != string(solver.PruneDiskUnavailable) || summary.Stayed != 1 {
		t.Fatalf("%+v", items)
	}
	if got := b.count("SELECT count(*) FROM ops"); got != 0 {
		t.Errorf("%d operations on a disk not confirmed", got)
	}
	if !b.sourceThere() {
		t.Error("the copy moved")
	}
}

// A copy that stays on a disk that fails the read does not count.
func TestConfirmationCoversWhatStays(t *testing.T) {
	b := newBench(t)
	b.unread[b.dirs["t1"]] = true
	items, _ := b.prune()
	if len(items) != 1 || items[0].Outcome != string(solver.PruneBelowLevel) {
		t.Fatalf("%+v", items)
	}
	if !b.sourceThere() {
		t.Error("the copy moved")
	}
}

// A session confirms each disk once, and a dry run wakes none.
func TestConfirmedOnce(t *testing.T) {
	b := newBench(t)
	b.write("t1", "b", "x")
	b.write("t2", "b", "x")
	b.write("t3", "b", "x")
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	// A scan confirms what it walks (specs.md §15.10): what counts here is what the sessions read.
	b.confirm = map[string]int{}
	dry, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dry.Prune(t.Context(), func(*PruneItem) error { return nil }); err != nil {
		t.Fatal(err)
	}
	dry.Close()
	for id, n := range b.confirm {
		t.Errorf("a dry run read past the cache of %s %d times", id, n)
	}
	if items, _ := b.prune(); len(items) != 2 || items[1].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if n := b.confirm[b.dirs[id]]; n != 1 {
			t.Errorf("%s was confirmed %d times", id, n)
		}
	}
}

// The points of an operation, in order: a crash test means nothing if one never comes.
func TestCheckpointsInOrder(t *testing.T) {
	b := newBench(t)
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	want := "journaled trash-folders moved target-synced source-synced"
	if got := strings.Join(b.points, " "); got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

// A copy that disappears between the look and the move.
func TestACopyGoneBeforeTheMove(t *testing.T) {
	b := newBench(t)
	b.failOn("t3", "rename", syscall.ENOENT)
	items, _ := b.prune()
	if len(items) != 1 || items[0].Outcome != string(solver.PruneChanged) {
		t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT state FROM ops"); got != "undone" {
		t.Errorf("the operation is %q", got)
	}
}

// What each given to a session returns stops it, and a canceled session stops too.
func TestPruneStops(t *testing.T) {
	b := newBench(t)
	s, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stop := errors.New("stop")
	if _, err := s.Prune(t.Context(), func(*PruneItem) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Prune(ctx, func(*PruneItem) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

// A device that fails during a restore or a purge: the same rules.
func TestRestoreAndPurgeFaults(t *testing.T) {
	session := func(b *bench) *Session {
		t.Helper()
		s, err := Open(t.Context(), b.db, b.config, b, true)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	run := func(t *testing.T, b *bench, restore bool) string {
		t.Helper()
		s := session(b)
		defer s.Close()
		outcome := ""
		each := func(i *EntryItem) error {
			outcome = i.Outcome
			return nil
		}
		var err error
		if restore {
			_, err = s.Restore(t.Context(), "a", each)
		} else {
			var d time.Duration
			_, err = s.Purge(t.Context(), &d, each)
		}
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	t.Run("restore lost", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.failOn("t3", "sync", syscall.EIO)
		if got := run(t, b, true); got != OutcomeIOError {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "planned" {
			t.Errorf("%q", got)
		}
		delete(b.fail, b.dirs["t3"])
		if got := run(t, b, true); got != "" {
			t.Errorf("after the reprise: %q", got)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "done" {
			t.Errorf("%q", got)
		}
		if !b.sourceThere() {
			t.Error("not back")
		}
	})
	t.Run("restore refused", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call == "rename" || call == "link" {
				return syscall.EINVAL
			}
			return nil
		}
		if got := run(t, b, true); got != OutcomeNameRefused {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
			t.Errorf("%q", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 1 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("restore failed", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.failOn("t3", "rename", syscall.ENOSPC)
		if got := run(t, b, true); got != OutcomeIOError {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != "undone" {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
			t.Errorf("%q", got)
		}
	})
	t.Run("restore from an entry that is gone", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		trashPath := b.text("SELECT CAST(trash_path AS TEXT) FROM trash")
		if err := os.Remove(filepath.Join(b.dirs["t3"], "Atoll", "trash", trashPath)); err != nil {
			t.Fatal(err)
		}
		if got := run(t, b, true); got != OutcomeChanged {
			t.Errorf("%q", got)
		}
		if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
			t.Errorf("%d events", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 1 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("purge lost", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.failOn("t3", "sync", syscall.EIO)
		if got := run(t, b, false); got != OutcomeIOError {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "planned" {
			t.Errorf("%q", got)
		}
		delete(b.fail, b.dirs["t3"])
		if got := run(t, b, false); got != "" {
			t.Errorf("after the reprise: %q", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 0 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("purge failed", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.failOn("t3", "unlink", syscall.EACCES)
		if got := run(t, b, false); got != OutcomeIOError {
			t.Errorf("%q", got)
		}
		if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "undone" {
			t.Errorf("%q", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 1 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("purge of what is already gone", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		trashPath := b.text("SELECT CAST(trash_path AS TEXT) FROM trash")
		if err := os.Remove(filepath.Join(b.dirs["t3"], "Atoll", "trash", trashPath)); err != nil {
			t.Fatal(err)
		}
		if got := run(t, b, false); got != OutcomePurged {
			t.Errorf("%q", got)
		}
	})
}

// The reprise itself: a disk lost while it runs ends that disk's session, says so, and leaves the
// operation for later. EIO must not read as a file that is absent (invariant 4).
func TestALostDiskDuringTheReprise(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(b *bench)
	}{
		{"a stat", func(b *bench) { b.failOn("t3", "stat", syscall.EIO) }},
		{"a sync", func(b *bench) { b.failOn("t3", "sync", syscall.EIO) }},
		{"the read past the cache", func(b *bench) { b.unread[b.dirs["t3"]] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
			b.prune()
			delete(b.fail, b.dirs["t3"])
			tc.break_(b)
			_, summary := b.prune()
			if !summary.DiskLost {
				t.Errorf("%+v", summary)
			}
			if got := b.text("SELECT state FROM ops"); got != "planned" {
				t.Errorf("the operation is %q", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'"); got != 1 {
				t.Errorf("%d rows: the reprise concluded something", got)
			}
			delete(b.fail, b.dirs["t3"])
			delete(b.unread, b.dirs["t3"])
			b.prune()
			if got := b.text("SELECT state FROM ops"); got != "done" {
				t.Errorf("the operation is %q", got)
			}
		})
	}
}

// An error that is not about the disk leaves the operation planned, and the run goes on.
func TestAReprisePuzzled(t *testing.T) {
	b := newBench(t)
	b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
	b.prune()
	b.failOn("t3", "stat", syscall.EACCES)
	_, summary := b.prune()
	if !summary.DiskLost {
		t.Errorf("%+v", summary)
	}
	if got := b.text("SELECT state FROM ops"); got != "planned" {
		t.Errorf("the operation is %q", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "online" {
		t.Errorf("the disk is %q", got)
	}
}

// A disk whose mount changes during a session is not written to, though the handle still answers
// (specs.md §10). Every operation checks: a session that checks once finds out too late.
func TestAMountChangeDuringASession(t *testing.T) {
	// away is what the first outcome of a run does, so that the second finds the mount changed.
	away := func(b *bench) func(string) error {
		seen := 0
		return func(string) error {
			seen++
			if seen == 1 {
				return b.moveAway()
			}
			return nil
		}
	}
	wantSecond := func(t *testing.T, outcomes []string, first string) {
		t.Helper()
		if len(outcomes) != 2 || outcomes[0] != first || outcomes[1] != OutcomeIOError {
			t.Fatalf("%v", outcomes)
		}
	}
	t.Run("prune", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("b")
		s := b.opened()
		var outcomes []string
		next := away(b)
		if _, err := s.Prune(t.Context(), func(i *PruneItem) error {
			outcomes = append(outcomes, i.Outcome)
			return next(i.Outcome)
		}); err != nil {
			t.Fatal(err)
		}
		wantSecond(t, outcomes, OutcomeTrashed)
		dir := b.dirs["t3"]
		if _, err := os.Lstat(filepath.Join(dir, "Atoll")); !os.IsNotExist(err) {
			t.Errorf("it wrote to what stands at the mount path now: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir+".away", "media", "b")); err != nil {
			t.Errorf("the copy moved: %v", err)
		}
		if got := b.text("SELECT state FROM ops WHERE id = 2"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
	})
	t.Run("restore", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("e/a", "e/b")
		b.prune()
		s := b.opened()
		var outcomes []string
		next := away(b)
		if _, err := s.Restore(t.Context(), "e", func(i *EntryItem) error {
			outcomes = append(outcomes, i.Outcome)
			return next(i.Outcome)
		}); err != nil {
			t.Fatal(err)
		}
		wantSecond(t, outcomes, OutcomeRestored)
		dir := b.dirs["t3"]
		if _, err := os.Lstat(filepath.Join(dir, "media", "e")); !os.IsNotExist(err) {
			t.Errorf("it wrote to what stands at the mount path now: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir+".away", "media", "e", "b")); !os.IsNotExist(err) {
			t.Errorf("the entry moved: %v", err)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'restore' AND state = 'planned'"); got != 1 {
			t.Errorf("%d operations left planned", got)
		}
	})
	t.Run("a restore that makes a folder", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("e/a", "e/d/x")
		b.prune()
		if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "e", "d")); err != nil {
			t.Fatal(err)
		}
		s := b.opened()
		var outcomes []string
		next := away(b)
		if _, err := s.Restore(t.Context(), "e", func(i *EntryItem) error {
			outcomes = append(outcomes, i.Outcome)
			return next(i.Outcome)
		}); err != nil {
			t.Fatal(err)
		}
		wantSecond(t, outcomes, OutcomeRestored)
		if _, err := os.Lstat(filepath.Join(b.dirs["t3"]+".away", "media", "e", "d")); !os.IsNotExist(err) {
			t.Errorf("it made a folder after the mount changed: %v", err)
		}
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'mkdir' AND state = 'planned'"); got != 1 {
			t.Errorf("%d folders left planned", got)
		}
	})
	t.Run("purge", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("b")
		b.prune()
		s := b.opened()
		var outcomes []string
		next := away(b)
		var zero time.Duration
		if _, err := s.Purge(t.Context(), &zero, func(i *EntryItem) error {
			outcomes = append(outcomes, i.Outcome)
			return next(i.Outcome)
		}); err != nil {
			t.Fatal(err)
		}
		wantSecond(t, outcomes, OutcomePurged)
		if got := b.count("SELECT count(*) FROM ops WHERE kind = 'unlink' AND state = 'planned'"); got != 1 {
			t.Errorf("%d operations left planned", got)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 1 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("a reprise", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("b")
		b.plan("a", "b")
		once := true
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call == "stat" && once {
				once = false
				if err := b.moveAway(); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
		s := b.opened()
		s.Close()
		if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 1 {
			t.Errorf("%d left planned: the second was looked at through a mount that changed", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
			t.Errorf("the disk is %q", got)
		}
	})
}

// A confirmation that finds the disk read-only now stops writes to it, whatever the check before.
func TestAConfirmationFindsAReadOnlyDisk(t *testing.T) {
	b := newBench(t)
	s, err := Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b.ro[b.dirs["t3"]] = true
	var outcome string
	if _, err := s.Prune(t.Context(), func(i *PruneItem) error { outcome = i.Outcome; return nil }); err != nil {
		t.Fatal(err)
	}
	if outcome != string(solver.PruneDiskUnavailable) {
		t.Errorf("%q", outcome)
	}
	if got := b.count("SELECT count(*) FROM ops"); got != 0 {
		t.Errorf("%d operations", got)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "readonly" {
		t.Errorf("the disk is %q", got)
	}
}

// Nothing is written to a read-only disk: no reprise, no restore, no purge. A copy that stays there
// still counts, as it can be read.
func TestReadOnlyDisks(t *testing.T) {
	t.Run("what stays on one counts", func(t *testing.T) {
		b := newBench(t)
		b.ro[b.dirs["t1"]] = true
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeTrashed {
			t.Errorf("%+v", items)
		}
	})
	t.Run("the reprise", func(t *testing.T) {
		b := newBench(t)
		b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
		b.prune()
		delete(b.fail, b.dirs["t3"])
		b.ro[b.dirs["t3"]] = true
		// What counts is what this session reads past the cache, not the scans before it.
		b.confirm = map[string]int{}
		s, err := Open(t.Context(), b.db, b.config, b, true)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var zero time.Duration
		summary, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !summary.DiskLost {
			t.Errorf("an operation that stays planned must show in the exit code: %+v", summary)
		}
		if got := b.text("SELECT state FROM ops"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
		if n := b.confirm[b.dirs["t3"]]; n != 0 {
			t.Errorf("%d confirmations: a disk it cannot write to has no reason to wake", n)
		}
	})
	for _, restore := range []bool{true, false} {
		t.Run("restore or purge", func(t *testing.T) {
			b := newBench(t)
			b.prune()
			b.ro[b.dirs["t3"]] = true
			s, err := Open(t.Context(), b.db, b.config, b, true)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got := ""
			each := func(i *EntryItem) error { got = i.Outcome; return nil }
			if restore {
				_, err = s.Restore(t.Context(), "a", each)
			} else {
				var zero time.Duration
				_, err = s.Purge(t.Context(), &zero, each)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != string(solver.PruneDiskUnavailable) {
				t.Errorf("%q", got)
			}
			if n := b.count("SELECT count(*) FROM ops WHERE kind <> 'trash'"); n != 0 {
				t.Errorf("%d operations", n)
			}
		})
	}
}

// A disk failed, retired or missing is not opened: nothing wakes it, and nothing looks at what it holds. A disk
// draining is, as its copies are the sources that evacuate it, and it stays in step until it leaves (specs.md §18).
func TestADiskThatCannotBeReadIsLeftAlone(t *testing.T) {
	for _, state := range []solver.DiskState{solver.DiskFailed, solver.DiskRetired, solver.DiskMissing} {
		t.Run(string(state), func(t *testing.T) {
			b := newBench(t)
			// What counts is what this session reads past the cache, not the scan that set the bench up.
			b.confirm = map[string]int{}
			s, err := Open(t.Context(), b.db, b.config, b, true)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.slots["t3"].state = state
			handle, err := s.open(t.Context(), "t3")
			if err != nil || handle != nil {
				t.Errorf("%v, %v", handle, err)
			}
			if n := b.confirm[b.dirs["t3"]]; n != 0 {
				t.Errorf("%d confirmations", n)
			}
		})
	}
	t.Run("readonly is read", func(t *testing.T) {
		b := newBench(t)
		s, err := Open(t.Context(), b.db, b.config, b, false)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.slots["t3"].state = solver.DiskReadOnly
		if handle, err := s.open(t.Context(), "t3"); err != nil || handle == nil {
			t.Errorf("%v, %v", handle, err)
		}
	})
	t.Run("draining is read and written, and stays draining once confirmed", func(t *testing.T) {
		b := newBench(t)
		for i := range b.config.Disks {
			if b.config.Disks[i].ID == "t3" {
				b.config.Disks[i].State = config.DiskDraining
			}
		}
		s, err := Open(t.Context(), b.db, b.config, b, true)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if s.slots["t3"].state != solver.DiskDraining {
			t.Fatalf("opened as %s", s.slots["t3"].state)
		}
		if handle, err := s.open(t.Context(), "t3"); err != nil || handle == nil || !s.Readable("t3") || !s.Writable("t3") {
			t.Errorf("%v, %v, readable %v, writable %v", handle, err, s.Readable("t3"), s.Writable("t3"))
		}
		if s.slots["t3"].state != solver.DiskDraining {
			t.Errorf("confirmed as %s", s.slots["t3"].state)
		}
	})
}

// With no surplus copy, nothing is looked at, and no disk wakes.
func TestNothingToDoWakesNothing(t *testing.T) {
	b := newBench(t)
	b.prune()
	clear(b.confirm)
	stats := 0
	for _, dir := range b.dirs {
		b.fail[dir] = func(call, _ string) error {
			if call == "stat" {
				stats++
			}
			return nil
		}
	}
	if items, _ := b.prune(); len(items) != 0 {
		t.Fatalf("%+v", items)
	}
	if len(b.confirm) != 0 || stats != 0 {
		t.Errorf("%v confirmations and %d stats, for nothing", b.confirm, stats)
	}
}

// The original is worked out again once a copy moved, when the session ends.
func TestAPruneSettles(t *testing.T) {
	b := newBench(t)
	if _, err := b.db.ExecContext(t.Context(), "UPDATE files SET original_disk = 't3'"); err != nil {
		t.Fatal(err)
	}
	b.prune()
	if got := b.text("SELECT original_disk FROM files"); got == "t3" {
		t.Error("the original is still the copy that went")
	}
}

// A file with two names: what happens to one moves the change time of both, which must not read as a
// change for ever after.
func TestHardLinks(t *testing.T) {
	b := newBench(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "c", "y")
	}
	b.write("t1", "d", "y")
	b.write("t2", "d", "y")
	if err := os.Link(filepath.Join(b.dirs["t3"], "media", "c"), filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't3' AND nlink = 2"); got != 2 {
		t.Fatalf("%d rows with two links", got)
	}
	// d stays, so that it can show what happened to its row.
	if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET state = 'ok' WHERE disk_id = 't3' AND file_id = (SELECT id FROM files WHERE lpath = CAST('d' AS BLOB))"); err != nil {
		t.Fatal(err)
	}
	items, _ := b.prune()
	if len(items) != 2 || items[1].Path != "c" || items[1].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	notChanged := func() {
		t.Helper()
		out, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range out.Disks {
			if out.Disks[i].ID == "t3" && len(out.Disks[i].Changed) != 0 {
				t.Errorf("the other name reads as changed: %+v", out.Disks[i])
			}
		}
	}
	// The name in the trash is still a name of that file.
	if got := b.count("SELECT nlink FROM copies WHERE disk_id = 't3'"); got != 2 {
		t.Errorf("the other name records %d links", got)
	}
	notChanged()
	s, err := Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var zero time.Duration
	if _, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT nlink FROM copies WHERE disk_id = 't3'"); got != 1 {
		t.Errorf("after the purge, the other name records %d links", got)
	}
	notChanged()
}

// Purged in a reprise, a name that stays takes what the file is now, as when a purge finishes at once.
func TestHardLinksAfterACrash(t *testing.T) {
	b := newBench(t)
	b.linkedNames()
	if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET state = 'ok' WHERE disk_id = 't3' AND file_id IN (SELECT id FROM files WHERE lpath IN (CAST('d' AS BLOB), CAST('a' AS BLOB)))"); err != nil {
		t.Fatal(err)
	}
	b.prune()
	b.failNth("t3", "sync", 1, syscall.EIO)
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%+v", outcomes)
	}
	delete(b.fail, b.dirs["t3"])
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "done" {
		t.Fatalf("%q", got)
	}
	if got := b.count("SELECT nlink FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't3' AND f.lpath = CAST('d' AS BLOB)"); got != 1 {
		t.Errorf("the other name records %d links", got)
	}
	out, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range out.Disks {
		if out.Disks[i].ID == "t3" && len(out.Disks[i].Changed) != 0 {
			t.Errorf("the other name reads as changed: %+v", out.Disks[i])
		}
	}
}

// A disk lost on the look at the name that stays: only a reprise can finish that purge, at once or in
// a later run.
func TestALostDiskOnTheNameThatStays(t *testing.T) {
	b := newBench(t)
	b.linkedNames()
	if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET state = 'ok' WHERE disk_id = 't3' AND file_id IN (SELECT id FROM files WHERE lpath IN (CAST('d' AS BLOB), CAST('a' AS BLOB)))"); err != nil {
		t.Fatal(err)
	}
	b.prune()
	b.failStatAfter("source-synced", "d", syscall.EIO)
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%+v", outcomes)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "planned" {
		t.Errorf("%q", got)
	}
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "stat" && name == "d" {
			return syscall.EIO
		}
		return nil
	}
	s := b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "planned" {
		t.Errorf("in the reprise: %q", got)
	}
	delete(b.fail, b.dirs["t3"])
	s = b.opened()
	s.Close()
	if got := b.text("SELECT state FROM ops WHERE kind = 'unlink'"); got != "done" {
		t.Errorf("%q", got)
	}
}

// With a name since replaced, what the remaining names record comes from one that still leads there.
func TestLinksComeFromANameThatStillLeadsThere(t *testing.T) {
	b := newBench(t)
	b.linkedAs("c", "d", "e")
	media := filepath.Join(b.dirs["t3"], "media")
	if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET state = 'ok' WHERE disk_id = 't3' AND file_id NOT IN (SELECT id FROM files WHERE lpath = CAST('c' AS BLOB))"); err != nil {
		t.Fatal(err)
	}
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	b.replace(filepath.Join(media, "d"), "other")
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomePurged {
		t.Fatalf("%+v", outcomes)
	}
	if got := b.count("SELECT c.nlink FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't3' AND f.lpath = CAST('e' AS BLOB)"); got != 1 {
		t.Errorf("the name that stays records %d links", got)
	}
}

// Several names of a file in one page of a prune: what happened to the first must not read as a
// change of the others (specs.md §17.1).
func TestHardLinksInOnePage(t *testing.T) {
	b := newBench(t)
	b.linkedNames()
	// The change time must have moved past what the scan saw, for the move to change it.
	time.Sleep(20 * time.Millisecond)
	items, summary := b.prune()
	if len(items) != 3 || summary.Stayed != 0 {
		t.Fatalf("%+v, %+v", items, summary)
	}
	for _, item := range items {
		if item.Outcome != OutcomeTrashed {
			t.Errorf("%+v", item)
		}
	}
}

// A retention past what the catalog holds is not a time long past (specs.md §9, §17.1).
func TestALongRetentionStaysLong(t *testing.T) {
	const year = 365 * 24 * time.Hour
	const largest = int64(1<<63 - 1)
	for _, tc := range []struct {
		retention time.Duration
		want      int64
	}{
		{0, b0Now},
		{time.Hour, b0Now + int64(time.Hour)},
		{100 * year, b0Now + int64(100*year)},
		{280 * year, largest},
		{time.Duration(largest), largest},
	} {
		if got := ops.AddRetention(b0Now, tc.retention); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.retention, got, tc.want)
		}
	}
}

const b0Now = int64(1767225600000000000)

// A restore that finds the root gone never makes it (invariant 3).
func TestARestoreNeverMakesTheRoot(t *testing.T) {
	b := newBench(t)
	b.prune()
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := ""
	if _, err := s.Restore(t.Context(), "a", func(i *EntryItem) error { got = i.Outcome; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != OutcomeIOError {
		t.Errorf("%q", got)
	}
	if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media")); !os.IsNotExist(err) {
		t.Errorf("the root came back: %v", err)
	}
}

// Made with its mode from the start: a crash, or a driver that refuses to change it afterwards, leaves
// it as private as it should be, and never more open.
func TestAPrivateFolderIsNeverOpen(t *testing.T) {
	b := newBench(t)
	b.addFiles("p/x")
	private := filepath.Join(b.dirs["t3"], "media", "p")
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.failOn("t3", "adopt", syscall.EPERM)
	b.prune()
	info, err := os.Stat(filepath.Join(b.dirs["t3"], "Atoll", "trash", day, "2", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("in the trash: %v", info.Mode())
	}
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	if outcomes, _ := b.restored("p/x"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%+v", outcomes)
	}
	if info, err = os.Stat(private); err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("back: %v", info.Mode())
	}
}

// A folder a restore makes takes the mode of the one in the trash, which took that of the source, so
// a private folder stays private on the way there and back.
func TestAPrivateFolderStaysPrivate(t *testing.T) {
	b := newBench(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "p/x", "z")
	}
	private := filepath.Join(b.dirs["t3"], "media", "p")
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.prune()
	info, err := os.Stat(filepath.Join(b.dirs["t3"], "Atoll", "trash", day, "2", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("in the trash: %v", info.Mode())
	}
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := ""
	if _, err := s.Restore(t.Context(), "p/x", func(i *EntryItem) error { got = i.Outcome; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != OutcomeRestored {
		t.Fatalf("%q", got)
	}
	info, err = os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("back: %v", info.Mode())
	}
}

// The handle comes first, so that what a confirmation reads is what the handle leads to: an empty mount
// point in between, whose trash holds nothing, must not pass as the disk (specs.md §10, invariant 4).
func TestConfirmationIsOfTheHandle(t *testing.T) {
	t.Run("a purge", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.beforeRead[b.dirs["t3"]] = func() {
			if err := b.moveAway(); err != nil {
				t.Fatal(err)
			}
		}
		outcomes, _ := b.purgedAll()
		if len(outcomes) != 1 || outcomes[0] != string(solver.PruneDiskUnavailable) {
			t.Errorf("%+v", outcomes)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 1 {
			t.Errorf("%d entries: an empty mount point passed as the disk", got)
		}
		if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
			t.Errorf("the disk is %q", got)
		}
	})
	t.Run("a reprise", func(t *testing.T) {
		b := newBench(t)
		b.failNth("t3", "sync", syncsOfAFirstMove, syscall.EIO)
		b.prune()
		delete(b.fail, b.dirs["t3"])
		b.beforeRead[b.dirs["t3"]] = func() {
			if err := b.moveAway(); err != nil {
				t.Fatal(err)
			}
		}
		s := b.opened()
		s.Close()
		if got := b.text("SELECT state FROM ops"); got != "planned" {
			t.Errorf("the operation is %q", got)
		}
		if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 0 {
			t.Errorf("%d events: something was concluded from an empty mount point", got)
		}
	})
}

// A folder someone else made during a restore is theirs, and one that cannot be made stops it.
func TestFoldersOfARestore(t *testing.T) {
	setUp := func(t *testing.T) *bench {
		t.Helper()
		b := newBench(t)
		for _, id := range []string{"t1", "t2", "t3"} {
			b.write(id, "d/x", "z")
		}
		if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
			t.Fatal(err)
		}
		b.prune()
		if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "d")); err != nil {
			t.Fatal(err)
		}
		return b
	}
	restore := func(t *testing.T, b *bench) string {
		t.Helper()
		s, err := Open(t.Context(), b.db, b.config, b, true)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		got := ""
		if _, err := s.Restore(t.Context(), "d/x", func(i *EntryItem) error { got = i.Outcome; return nil }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
		state   string
		mkdir   string
	}{
		{"made by someone else", syscall.EEXIST, OutcomeRestored, "done", "undone"},
		{"a name refused", syscall.EINVAL, OutcomeNameRefused, "undone", "undone"},
		{"no room", syscall.ENOSPC, OutcomeIOError, "undone", "undone"},
		{"a dead disk", syscall.EIO, OutcomeIOError, "planned", "planned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := setUp(t)
			if errors.Is(tc.err, syscall.EEXIST) {
				// It is there when mkdir runs: the look before saw nothing.
				if err := os.Mkdir(filepath.Join(b.dirs["t3"], "media", "d"), 0o755); err != nil {
					t.Fatal(err)
				}
				look := b.dirs["t3"]
				once := true
				b.fail[look] = func(call, name string) error {
					if call == "stat" && name == "d" && once {
						once = false
						return syscall.ENOENT
					}
					return nil
				}
			} else {
				b.failOn("t3", "mkdir", tc.err)
			}
			if got := restore(t, b); got != tc.outcome {
				t.Errorf("%q, want %q", got, tc.outcome)
			}
			if got := b.text("SELECT state FROM ops WHERE kind = 'restore'"); got != tc.state {
				t.Errorf("the restore is %q, want %q", got, tc.state)
			}
			if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != tc.mkdir {
				t.Errorf("the folder is %q, want %q", got, tc.mkdir)
			}
		})
	}
	t.Run("recorded as Atoll's own", func(t *testing.T) {
		b := setUp(t)
		if got := restore(t, b); got != OutcomeRestored {
			t.Fatalf("%q", got)
		}
		if got := b.text("SELECT created_by FROM entries WHERE disk_id = 't3' AND lpath = CAST('d' AS BLOB)"); got != "atoll" {
			t.Errorf("%q", got)
		}
	})
}

// The trash folders a chain left half made go too, however far down it stopped.
func TestCleaningAfterAHalfMadeChain(t *testing.T) {
	for _, path := range []string{"d/x", "d/e/x", "d/e/f/x"} {
		t.Run(path, func(t *testing.T) {
			b := newBench(t)
			b.addFiles(path)
			b.fail[b.dirs["t3"]] = func(call, name string) error {
				if call == "mkdir" && name == "d" {
					return syscall.ENOSPC
				}
				return nil
			}
			if items, _ := b.prune(); len(items) != 2 {
				t.Fatalf("%+v", items)
			}
			if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "Atoll", "trash", day, "2")); !os.IsNotExist(err) {
				t.Errorf("the folder of the undone operation stays: %v", err)
			}
		})
	}
}

// A replacement between the look and the move: what took the name is not what was asked for.
func TestAReplacementBetweenTheLookAndTheMove(t *testing.T) {
	source := func(b *bench) string { return filepath.Join(b.dirs["t3"], "media", "a") }
	replace := func(t *testing.T, b *bench) {
		t.Helper()
		next := source(b) + ".new"
		if err := os.WriteFile(next, []byte("yy"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, source(b)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("before the name is looked at again", func(t *testing.T) {
		b := newBench(t)
		b.at["journaled"] = func() { replace(t, b) }
		b.fail[b.dirs["t3"]] = func(call, name string) error {
			if call == "mkdir" || call == "rename" || call == "link" {
				t.Errorf("%s %q: nothing moves for a name that no longer holds the copy", call, name)
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeChanged {
			t.Fatalf("%+v", items)
		}
		data, err := os.ReadFile(source(b))
		if err != nil || string(data) != "yy" {
			t.Errorf("the new file: %q, %v", data, err)
		}
		if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "Atoll")); !os.IsNotExist(err) {
			t.Errorf("a folder was made for nothing: %v", err)
		}
		if got := b.text("SELECT state FROM ops"); got != "undone" {
			t.Errorf("the operation is %q", got)
		}
	})
	t.Run("after the name was looked at", func(t *testing.T) {
		b := newBench(t)
		once := true
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call == "mkdir" && once {
				once = false
				replace(t, b)
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeChanged {
			t.Fatalf("%+v", items)
		}
		data, err := os.ReadFile(source(b))
		if err != nil || string(data) != "yy" {
			t.Errorf("the new file: %q, %v", data, err)
		}
	})
	t.Run("just before the rename", func(t *testing.T) {
		b := newBench(t)
		once := true
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			if call == "rename" && once {
				once = false
				replace(t, b)
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeChanged {
			t.Fatalf("%+v", items)
		}
		data, err := os.ReadFile(source(b))
		if err != nil || string(data) != "yy" {
			t.Errorf("the new file did not come back: %q, %v", data, err)
		}
		if got := b.count("SELECT count(*) FROM trash"); got != 0 {
			t.Errorf("%d entries", got)
		}
	})
	t.Run("on the link path, before the unlink", func(t *testing.T) {
		b := newBench(t)
		syncs := 0
		b.fail[b.dirs["t3"]] = func(call, _ string) error {
			switch call {
			case "rename":
				return syscall.EINVAL
			case "sync":
				syncs++
				if syncs == 5 { // the one after the link: the name changes hands right there
					replace(t, b)
				}
			}
			return nil
		}
		items, _ := b.prune()
		if len(items) != 1 || items[0].Outcome != OutcomeChanged {
			t.Fatalf("%+v", items)
		}
		data, err := os.ReadFile(source(b))
		if err != nil || string(data) != "yy" {
			t.Errorf("the new file lost its name: %q, %v", data, err)
		}
		if got := b.text("SELECT state FROM ops"); got != "undone" {
			t.Errorf("the operation is %q", got)
		}
	})
}

// atOrBelow compares whole components: nothing else may match.
func TestAtOrBelow(t *testing.T) {
	for _, tc := range []struct {
		path, under string
		want        bool
	}{
		{"a", "a", true}, {"a/b", "a", true}, {"a/b/c", "a/b", true}, {"ab", "a", false}, {"a", "a/b", false}, {"a b/c", "a", false},
	} {
		if got := atOrBelow(tc.path, tc.under); got != tc.want {
			t.Errorf("atOrBelow(%q, %q) = %v", tc.path, tc.under, got)
		}
	}
}

// A folder made below the root takes the mode and owner of the one it stands for, in two steps: made, then given
// them. A disk that fails in between leaves its operation planned, and the reprise finishes it: with that mode and
// owner, which its event holds. It finished whatever folder it found, the daemon's own: a private folder of
// someone's came back closed to them on that disk (specs.md §17.2, §17.4).
func TestAFolderHalfMadeTakesItsOwnerAtTheReprise(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a folder to another account takes root")
	}
	b := newBench(t)
	b.addFiles("p/x")
	private := filepath.Join(b.dirs["t3"], "media", "p")
	if err := os.Chmod(private, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(private, 1000, 1001); err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.prune()
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	// The disk fails as the folder is given its owner.
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "adopt" && name == "p" {
			return syscall.EIO
		}
		return nil
	}
	if outcomes, _ := b.restored("p/x"); len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%+v", outcomes)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops WHERE kind = 'mkdir'"); got != "mkdir:planned" {
		t.Fatalf("the folder's operation: %s", got)
	}
	info, err := os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid == 1000 { //nolint:forcetypeassert // Linux only
		t.Fatal("the folder took its owner before the disk failed")
	}
	// The next command that writes resolves it.
	delete(b.fail, b.dirs["t3"])
	if _, err := b.db.ExecContext(t.Context(), "UPDATE disks SET state = 'online'"); err != nil {
		t.Fatal(err)
	}
	b.opened().Close()
	if got := b.text("SELECT group_concat(kind || ':' || state) FROM ops WHERE kind = 'mkdir'"); got != "mkdir:done" {
		t.Errorf("the folder's operation: %s", got)
	}
	if info, err = os.Stat(private); err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t) //nolint:forcetypeassert // Linux only
	if info.Mode().Perm() != 0o750 || st.Uid != 1000 || st.Gid != 1001 {
		t.Errorf("after the reprise the folder is %v of %d:%d, want 0750 of 1000:1001", info.Mode().Perm(), st.Uid, st.Gid)
	}
	if got := b.text("SELECT mode FROM entries WHERE CAST(lpath AS TEXT) = 'p' AND disk_id = 't3'"); got != "488" {
		t.Errorf("its entry says mode %s", got)
	}
}
