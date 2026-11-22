package ops

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// poolMachine is onDisk with a mount table and a device that confirm the one disk of a pool, so that a scan walks it.
type poolMachine struct {
	onDisk
	uuid string
}

func (m *poolMachine) Mount(mount string) disks.MountSeen {
	fstype := m.driver
	if fstype == "" {
		fstype = "ext4"
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: fstype, MountOptions: []string{"rw"}, SuperOptions: []string{"rw"},
	}}
}

func (m *poolMachine) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: m.uuid}}
}

func (m *poolMachine) ReadPastCache(fs names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, e)
}

func (m *poolMachine) Root(string, string) disks.RootSeen {
	return disks.RootSeen{Folder: disks.FolderOK}
}

func (m *poolMachine) Now() int64 { return repriseNow }

func (m *poolMachine) Space(string) (capacity, free int64, err error) { return 1 << 30, 1 << 29, nil }

// repriseNow is the time of the bench: 2026-01-01, the day the trash folders are named after.
const repriseNow = 1767225600000000000

// retention is how long an entry stays in the trash.
const retention = 30 * 24 * time.Hour

// pool is one ext4 disk, t1, with a catalog its scan filled: what an operation is journaled in, and what its reprise
// resolves it with.
type pool struct {
	t       *testing.T
	mount   string
	config  *config.Config
	db      *sql.DB
	machine *poolMachine
}

// emptyCatalog is the file of a new catalog of the pool p, made once: each pool starts from a copy of it, as making
// one runs every migration.
var emptyCatalog = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "catalog")
	if err != nil {
		return nil, err //nolint:wrapcheck // a test's
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "catalog.db")
	if _, err := catalog.Create(context.Background(), file, "p"); err != nil {
		return nil, err //nolint:wrapcheck // a test's
	}
	return os.ReadFile(file) //nolint:wrapcheck // a test's
})

// newPool makes a pool whose disk holds what prepare puts below its top, and scans it.
func newPool(t *testing.T, prepare func(mount string)) *pool {
	t.Helper()
	top := t.TempDir()
	mount := filepath.Join(top, "t1")
	if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(mount)
	}
	const uuid = "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c31"
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n[[disk]]\nid = \"t1\"\nuuid = %q\nmount = %q\nfs = \"ext4\"\n",
		filepath.Join(top, "catalog.db"), filepath.Join(top, "pool"), uuid, mount)
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(top, "catalog.db")
	empty, err := emptyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, empty, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := &pool{t: t, mount: mount, config: c, db: db, machine: &poolMachine{uuid: uuid}}
	if _, err := watch.Scan(t.Context(), db, c, p.machine, watch.Options{}); err != nil {
		t.Fatal(err)
	}
	return p
}

// disk opens the pool's disk, its calls through fail.
func (p *pool) disk(fail func(call, name string) error) *Disk {
	p.t.Helper()
	m := &poolMachine{onDisk: onDisk{fail: fail}, uuid: p.machine.uuid}
	d, err := Open(m, &p.config.Disks[0], 0)
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(d.Close)
	return d
}

// copyRow is what the catalog says of the copy at a path.
func (p *pool) copyRow(path string) *catalog.KnownCopy {
	p.t.Helper()
	key := keyAt(p.t, p.mount, "media/"+path)
	row, err := catalog.CopyOf(p.t.Context(), p.db, "t1", path, key)
	if err != nil {
		p.t.Fatal(err)
	}
	return &row
}

// count runs a query that counts.
func (p *pool) count(query string, args ...any) int {
	p.t.Helper()
	var n int
	if err := p.db.QueryRowContext(p.t.Context(), query, args...).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

// opState is what the journal says of an operation.
func (p *pool) opState(id int64) string {
	p.t.Helper()
	var s string
	if err := p.db.QueryRowContext(p.t.Context(), "SELECT state FROM ops WHERE id = ?", id).Scan(&s); err != nil {
		p.t.Fatal(err)
	}
	return s
}

// recover runs the reprise of every operation left planned, on a disk opened anew, nothing failing.
func (p *pool) recover() (*Recovered, error) {
	p.t.Helper()
	planned, err := catalog.PlannedOps(p.t.Context(), p.db)
	if err != nil {
		p.t.Fatal(err)
	}
	return p.disk(nil).Recover(p.t.Context(), p.db, planned, repriseNow+int64(time.Hour), retention)
}

// errKilled is what every call answers once the process is taken as killed.
var errKilled = errors.New("killed")

// writes are the calls that change a disk: a kill between two of them leaves the disk as a kill at the second does.
var writes = map[string]bool{
	"mkdir": true, "symlink": true, "adopt": true, "rename": true, "plainrename": true, "exchange": true, "link": true,
	"unlink": true, "rmdir": true, "create": true, "openwrite": true, "setmtime": true, "setmode": true, "settimes": true,
	"setowner": true, "write": true, "truncate": true, "setxattr": true, "removexattr": true,
}

// killer counts the writes made through a disk's top and fails every call from the one at a position on, as kill -9
// right before it would leave the disk: nothing from it on is done. 0 kills none.
type killer struct {
	at, writes int
	killed     bool
	driver     func(call, name string) error
}

func (k *killer) fail(call, name string) error {
	if writes[call] {
		k.writes++
		k.killed = k.killed || k.at > 0 && k.writes >= k.at
	}
	if k.killed {
		return errKilled
	}
	if k.driver != nil {
		return k.driver(call, name)
	}
	return nil
}

// repriseCase is an operation journaled, then cut short right before each of its writes in turn, or not at all and
// left unrecorded, as kill -9 leaves it, then resolved by the reprise.
type repriseCase struct {
	name   string
	driver func(call, name string) error
	// prepare puts what the operation needs below the top of a new disk, before its scan.
	prepare func(mount string)
	// start journals the operation, and gives it and what runs it.
	start func(p *pool) (*Op, func(d *Disk) error)
	// done says whether what the operation does is all there; undone whether none of it is. kept whether what it is
	// about is still on the disk, at one of its names at least.
	done, undone, kept func(p *pool) bool
}

// reprise runs a case once nothing failing, then once killed before each write, and checks that the reprise leaves nothing
// planned and the operation done or undone, as the disk shows it and the journal says: done whenever the operation
// ran to its end, undone whenever it never started (specs.md §17.2).
func reprise(t *testing.T, c repriseCase) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		k := &killer{driver: c.driver}
		p := newPool(t, c.prepare)
		_, run := c.start(p)
		if err := run(p.disk(k.fail)); err != nil {
			t.Fatalf("with nothing failing: %v", err)
		}
		total := k.writes
		if total == 0 {
			t.Fatal("no write went through the disk's top")
		}
		for at := 1; at <= total+1; at++ {
			where := fmt.Sprintf("killed before write %d of %d", at, total)
			if at > total {
				where = "killed once it ran, before the catalog recorded it"
			}
			p := newPool(t, c.prepare)
			op, run := c.start(p)
			_ = run(p.disk((&killer{at: at, driver: c.driver}).fail))
			if _, err := p.recover(); err != nil {
				t.Errorf("%s: the reprise: %v", where, err)
				continue
			}
			if n := p.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
				t.Errorf("%s: %d operations left planned", where, n)
			}
			if c.kept != nil && !c.kept(p) {
				t.Errorf("%s: the data is gone", where)
			}
			switch state := p.opState(op.ID); {
			case state == "done" && !c.done(p):
				t.Errorf("%s: said done, and not done", where)
			case state == "undone" && !c.undone(p):
				t.Errorf("%s: said undone, and not undone", where)
			case state != "done" && state != "undone":
				t.Errorf("%s: the operation is %s", where, state)
			case at == 1 && state != "undone":
				t.Errorf("%s: nothing ran, and it is %s", where, state)
			case at > total && state != "done":
				t.Errorf("%s: it all ran, and it is %s", where, state)
			case at == 1 && p.count("SELECT count(*) FROM events WHERE kind = 'op-undone' AND json_extract(data, '$.op') = ?", op.ID) != 0:
				// What was found is what Atoll's own operation leaves, which says nothing to warn of.
				t.Errorf("%s: nothing ran, and its undoing warns", where)
			}
		}
	})
}

// trashed sends the copy at a path to the trash, as a pass does, nothing failing, and gives its entry.
func (p *pool) trashed(path string) catalog.TrashEntry {
	p.t.Helper()
	row := p.copyRow(path)
	op := p.beginTrash(path, row)
	moved, err := p.disk(nil).ToTrash(op, row)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := catalog.FinishTrash(p.t.Context(), p.db, op, nil, moved.Links()); err != nil {
		p.t.Fatal(err)
	}
	entry, ok, err := catalog.TrashEntryOfOp(p.t.Context(), p.db, path, op.ID)
	if err != nil || !ok {
		p.t.Fatalf("no entry: %v", err)
	}
	return entry
}

func (p *pool) beginTrash(path string, row *catalog.KnownCopy) *Op {
	p.t.Helper()
	op, err := catalog.BeginTrash(p.t.Context(), p.db, &catalog.TrashStart{Disk: "t1", Path: path, FileKey: row.FileKey.String,
		Size: row.Size, Reason: "deleted", NowNS: repriseNow, ExpiresNS: AddRetention(repriseNow, retention)})
	if err != nil {
		p.t.Fatal(err)
	}
	return op
}

// copies counts the copies the catalog records at a path.
func (p *pool) copies(path string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST(? AS BLOB)", path)
}

// sitsAt says whether the catalog records the copy of the file at path sitting at another name, place, with the change
// time its name holds, as a rename of the file finished leaves it (specs.md §15.12, §17.2).
func (p *pool) sitsAt(path, place string) bool {
	p.t.Helper()
	var old sql.NullString
	var ctime sql.NullInt64
	if err := p.db.QueryRowContext(p.t.Context(), "SELECT CAST(c.old_lpath AS TEXT), c.ctime_ns FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST(? AS BLOB)",
		path).Scan(&old, &ctime); err != nil {
		p.t.Fatal(err)
	}
	return old.String == place && ctime == TimeOf(statAt(p.t, p.mount, "media/"+place).Ctime)
}

// recorded says what the catalog records of the copy at a path, by a column of copies.
func (p *pool) recorded(path, column string) int64 {
	p.t.Helper()
	var n sql.NullInt64
	query := "SELECT c." + column + " FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST(? AS BLOB)"
	if err := p.db.QueryRowContext(p.t.Context(), query, path).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n.Int64
}

// Whatever operation of Atoll's a kill cuts short, wherever, the reprise resolves it: it finishes what went through and
// undoes what did not, as the disk shows it, and loses nothing (specs.md §17.2; CLAUDE.md, "Testing rules": kill -9 at
// each step).
func TestTheRepriseResolvesWhatAKillLeft(t *testing.T) {
	const trashed = "Atoll/trash/2026-01-01/1/Films/a"
	film := func(mount string) {
		put(t, mount, "media/Films/a", "film")
		put(t, mount, "media/Films/other", "x")
	}
	inTrash := func(p *pool) bool {
		return holding(p.mount, trashed, "film") && absent(p.mount, "media/Films/a") &&
			p.count("SELECT count(*) FROM trash WHERE op_id = 1") == 1 && p.copies("Films/a") == 0
	}
	// Undone, the move leaves nothing of its own in the trash: the folders it made there go (specs.md §17.1).
	atPath := func(p *pool) bool {
		return holding(p.mount, "media/Films/a", "film") && absent(p.mount, "Atoll/trash/2026-01-01/1") && p.copies("Films/a") == 1
	}
	either := func(p *pool) bool {
		return holding(p.mount, trashed, "film") || holding(p.mount, "media/Films/a", "film")
	}
	toTrash := func(p *pool) (*Op, func(d *Disk) error) {
		row := p.copyRow("Films/a")
		op := p.beginTrash("Films/a", row)
		return op, func(d *Disk) error { _, err := d.ToTrash(op, row); return err }
	}
	fromTrash := func(p *pool) (*Op, func(d *Disk) error) {
		entry := p.trashed("Films/a")
		op, err := catalog.BeginFromTrash(p.t.Context(), p.db, catalog.OpRestore, &entry, repriseNow)
		if err != nil {
			p.t.Fatal(err)
		}
		return op, func(d *Disk) error { _, err := d.FromTrash(op); return err }
	}
	restored := func(p *pool) bool {
		return holding(p.mount, "media/Films/a", "film") && absent(p.mount, trashed) && p.copies("Films/a") == 1 &&
			p.count("SELECT count(*) FROM trash") == 0
	}
	stillTrashed := func(p *pool) bool {
		return holding(p.mount, trashed, "film") && absent(p.mount, "media/Films/a") && p.copies("Films/a") == 0 &&
			p.count("SELECT count(*) FROM trash") == 1
	}
	// A folder renamed leaves the rows of its copies as they were, for the next look to find them below its new name.
	renamed := func(from, to string, file bool) (func(p *pool) bool, func(p *pool) bool, func(p *pool) bool) {
		return func(p *pool) bool {
				return holding(p.mount, "media/"+to, "film") && absent(p.mount, "media/"+from) && (!file || p.sitsAt(from, to))
			},
			func(p *pool) bool { return holding(p.mount, "media/"+from, "film") && absent(p.mount, "media/"+to) },
			func(p *pool) bool {
				return holding(p.mount, "media/"+from, "film") || holding(p.mount, "media/"+to, "film")
			}
	}
	renameFile := func(p *pool) (*Op, func(d *Disk) error) {
		row := p.copyRow("Films/a")
		op, err := catalog.BeginRename(p.t.Context(), p.db, "t1", "Films/a", "Films/b", row.FileKey.String, repriseNow)
		if err != nil {
			p.t.Fatal(err)
		}
		return op, func(d *Disk) error { _, _, err := d.RenameFile(op, row); return err }
	}
	renameFolder := func(p *pool) (*Op, func(d *Disk) error) {
		op, err := catalog.BeginRename(p.t.Context(), p.db, "t1", "Films", "Movies", keyAt(p.t, p.mount, "media/Films"), repriseNow)
		if err != nil {
			p.t.Fatal(err)
		}
		return op, func(d *Disk) error { return d.RenameFolder(op) }
	}
	fileDone, fileUndone, fileKept := renamed("Films/a", "Films/b", true)
	folderDone, folderUndone, folderKept := renamed("Films/a", "Movies/a", false)
	for _, driver := range []struct {
		name string
		fail func(call, name string) error
	}{{"ext4", nil}, {"ntfs-3g", asNTFS3G}} {
		t.Run(driver.name, func(t *testing.T) {
			reprise(t, repriseCase{name: "to the trash", driver: driver.fail, prepare: film, start: toTrash, done: inTrash, undone: atPath, kept: either})
			reprise(t, repriseCase{name: "from the trash", driver: driver.fail, prepare: film, start: fromTrash, done: restored, undone: stillTrashed,
				kept: either})
			reprise(t, repriseCase{name: "a file renamed", driver: driver.fail, prepare: film, start: renameFile, done: fileDone, undone: fileUndone,
				kept: fileKept})
			reprise(t, repriseCase{name: "a folder renamed", driver: driver.fail, prepare: film, start: renameFolder, done: folderDone,
				undone: folderUndone, kept: folderKept})
		})
	}
	reprise(t, repriseCase{name: "an entry purged", prepare: film,
		start: func(p *pool) (*Op, func(d *Disk) error) {
			entry := p.trashed("Films/a")
			op, err := catalog.BeginFromTrash(p.t.Context(), p.db, catalog.OpUnlink, &entry, repriseNow)
			if err != nil {
				p.t.Fatal(err)
			}
			return op, func(d *Disk) error { _, err := d.PurgeEntry(op); return err }
		},
		done:   func(p *pool) bool { return absent(p.mount, trashed) && p.count("SELECT count(*) FROM trash") == 0 },
		undone: stillTrashed,
	})
	// Made under the units' umask, 0027, a folder a kill cut short before it was opened up is opened up by the reprise.
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	reprise(t, repriseCase{name: "a folder made", prepare: film,
		start: func(p *pool) (*Op, func(d *Disk) error) {
			op, err := catalog.BeginMkdir(p.t.Context(), p.db, "t1", "Films/New", LikeOf(nil), repriseNow)
			if err != nil {
				p.t.Fatal(err)
			}
			return op, func(d *Disk) error {
				_, err := d.MakeRootFolder("Films/New", nil, func(key string) error { return catalog.SetCreateKey(p.t.Context(), p.db, op, key) })
				return err
			}
		},
		// Its row records the folder as it is then: its mode and its inode.
		done: func(p *pool) bool {
			st := statAt(p.t, p.mount, "media/Films/New")
			return st.Mode&0o777 == FolderMode && p.count("SELECT count(*) FROM entries WHERE lpath = CAST('Films/New' AS BLOB) AND kind = 'dir' AND mode = ? AND file_key = ?",
				int64(st.Mode), keyOf(&st)) == 1
		},
		undone: func(p *pool) bool { return absent(p.mount, "media/Films/New") },
	})
	reprise(t, repriseCase{name: "a folder removed",
		prepare: func(mount string) {
			film(mount)
			if err := os.Mkdir(filepath.Join(mount, "media", "Films", "Old"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		start: func(p *pool) (*Op, func(d *Disk) error) {
			op, err := catalog.BeginRmdir(p.t.Context(), p.db, "t1", "Films/Old", keyAt(p.t, p.mount, "media/Films/Old"), repriseNow)
			if err != nil {
				p.t.Fatal(err)
			}
			return op, func(d *Disk) error { return d.RemoveFolder(op) }
		},
		done: func(p *pool) bool {
			return absent(p.mount, "media/Films/Old") && p.count("SELECT count(*) FROM entries WHERE lpath = CAST('Films/Old' AS BLOB)") == 0
		},
		undone: func(p *pool) bool { return !absent(p.mount, "media/Films/Old") },
		kept:   func(p *pool) bool { return holding(p.mount, "media/Films/a", "film") },
	})
	metadata := func(name string, begin func(p *pool, key string) (*Op, error), set func(d *Disk, row *catalog.KnownCopy) error,
		is func(st *fsx.Stat) bool, column string, value func(st *fsx.Stat) int64,
	) {
		var before fsx.Stat
		reprise(t, repriseCase{name: name, prepare: film,
			start: func(p *pool) (*Op, func(d *Disk) error) {
				row := p.copyRow("Films/a")
				before = statAt(p.t, p.mount, "media/Films/a")
				op, err := begin(p, row.FileKey.String)
				if err != nil {
					p.t.Fatal(err)
				}
				return op, func(d *Disk) error { return set(d, row) }
			},
			done: func(p *pool) bool {
				st := statAt(p.t, p.mount, "media/Films/a")
				return is(&st) && p.recorded("Films/a", column) == value(&st)
			},
			undone: func(p *pool) bool {
				st := statAt(p.t, p.mount, "media/Films/a")
				return !is(&st) && p.recorded("Films/a", column) == value(&before)
			},
			kept: func(p *pool) bool { return holding(p.mount, "media/Films/a", "film") },
		})
	}
	metadata("a time set",
		func(p *pool, key string) (*Op, error) {
			return catalog.BeginUtimens(p.t.Context(), p.db, "t1", "Films/a", key, 1000*int64(time.Second)+5, repriseNow) //nolint:wrapcheck // a test's
		},
		func(d *Disk, row *catalog.KnownCopy) error {
			_, err := d.SetMtime(row, fsx.Time{Sec: 1000, Nsec: 5})
			return err
		},
		func(st *fsx.Stat) bool { return st.Mtime == fsx.Time{Sec: 1000, Nsec: 5} }, "mtime_ns",
		func(st *fsx.Stat) int64 { return st.Mtime.Sec*int64(time.Second) + st.Mtime.Nsec })
	metadata("a mode set",
		func(p *pool, key string) (*Op, error) {
			return catalog.BeginChmod(p.t.Context(), p.db, "t1", "Films/a", key, 0o600, repriseNow) //nolint:wrapcheck // a test's
		},
		func(d *Disk, row *catalog.KnownCopy) error { _, err := d.SetMode(row, 0o600); return err },
		func(st *fsx.Stat) bool { return st.Mode == 0o600 }, "mode", func(st *fsx.Stat) int64 { return int64(st.Mode) })
	if os.Getuid() == 0 {
		metadata("an owner set",
			func(p *pool, key string) (*Op, error) {
				return catalog.BeginChown(p.t.Context(), p.db, "t1", "Films/a", key, 1000, 1001, repriseNow) //nolint:wrapcheck // a test's
			},
			func(d *Disk, row *catalog.KnownCopy) error { _, err := d.SetOwner(row, 1000, 1001); return err },
			func(st *fsx.Stat) bool { return st.Uid == 1000 && st.Gid == 1001 }, "uid", func(st *fsx.Stat) int64 { return int64(st.Uid) })
	}
}

// A copy edited in place once a kill cut its move to the trash short, through whichever of its names holds it then,
// never leaves the library for the trash: the reprise puts it back at its path, or leaves it there, and the move is
// undone, so that the next look reads the edit (specs.md §17.1, step 4, §17.2). A file a user put at the copy's path
// meanwhile stays where it is, and so does the copy: nothing is replaced.
func TestTheRepriseKeepsWhatAUserDidAfterAKill(t *testing.T) {
	const trashed = "Atoll/trash/2026-01-01/1/Films/a"
	film := func(mount string) {
		put(t, mount, "media/Films/a", "film")
		put(t, mount, "media/Films/other", "x")
	}
	// The copy at its path alone, at both names, at the trash's alone: what a kill leaves, before the move, between the
	// link and the source's name going where a driver refuses the rename, and once it ran.
	for _, driver := range []struct {
		name  string
		fail  func(call, name string) error
		kills map[string]func(driver func(call, name string) error) func(call, name string) error
	}{
		{"ext4", nil, map[string]func(func(string, string) error) func(string, string) error{
			"before the move": func(d func(string, string) error) func(string, string) error {
				return (&killedBefore{after: "stat", call: "rename", driver: d}).fail
			},
			"once it ran": func(d func(string, string) error) func(string, string) error { return (&killer{driver: d}).fail },
		}},
		{"ntfs-3g", asNTFS3G, map[string]func(func(string, string) error) func(string, string) error{
			"before the move": func(d func(string, string) error) func(string, string) error {
				return (&killedBefore{after: "rename", call: "link", driver: d}).fail
			},
			"between the two names": func(d func(string, string) error) func(string, string) error {
				return (&killedBefore{after: "link", call: "unlink", driver: d}).fail
			},
			"once it ran": func(d func(string, string) error) func(string, string) error { return (&killer{driver: d}).fail },
		}},
	} {
		t.Run(driver.name, func(t *testing.T) {
			for _, when := range slices.Sorted(maps.Keys(driver.kills)) {
				for _, user := range []string{"edits the copy", "edits it keeping its size", "edits it keeping its date", "puts another file at its path"} {
					where := fmt.Sprintf("killed %s, then a user %s", when, user)
					p := newPool(t, film)
					row := p.copyRow("Films/a")
					op := p.beginTrash("Films/a", row)
					_, _ = p.disk(driver.kills[when](driver.fail)).ToTrash(op, row)
					other := false
					name := filepath.Join(p.mount, "media", "Films", "a")
					if absent(p.mount, "media/Films/a") {
						name = filepath.Join(p.mount, filepath.FromSlash(trashed))
					}
					edited := "film, edited"
					switch {
					case user == "edits the copy":
						writeInPlace(t, name, edited)
					case user == "edits it keeping its size":
						edited = "flim"
						at := mtimeAt(t, name).Add(time.Second)
						writeInPlace(t, name, edited)
						setMtimeAt(t, name, at)
					case user == "edits it keeping its date":
						at := mtimeAt(t, name)
						writeInPlace(t, name, edited)
						setMtimeAt(t, name, at)
					case absent(p.mount, "media/Films/a"):
						put(t, p.mount, "media/Films/a", "another film")
						other = true
					default:
						// The copy is still at its path: no one can put a file there.
						continue
					}
					if _, err := p.recover(); err != nil {
						t.Errorf("%s: the reprise: %v", where, err)
						continue
					}
					state := p.opState(op.ID)
					switch {
					case !other && (state != "undone" || !holding(p.mount, "media/Films/a", edited) || !absent(p.mount, trashed)):
						t.Errorf("%s: the edit is not at its path alone, and the move is %s", where, state)
					case other && (!holding(p.mount, "media/Films/a", "another film") || !holding(p.mount, trashed, "film")):
						t.Errorf("%s: the user's file or the copy is gone, and the move is %s", where, state)
					}
				}
			}
		})
	}
}

// When an entry made now expires: its retention after now, and the largest time the catalog holds when that is past it
// (specs.md §9, §17.1); with none, now.
func TestAddRetention(t *testing.T) {
	const largest = int64(^uint64(0) >> 1)
	for _, tc := range []struct {
		now       int64
		retention time.Duration
		want      int64
	}{
		{1000, 0, 1000},
		{1000, time.Second, 1000 + int64(time.Second)},
		{largest - 5, time.Second, largest},
		{largest - int64(time.Second), time.Second, largest},
	} {
		if got := AddRetention(tc.now, tc.retention); got != tc.want {
			t.Errorf("%d and %v: %d", tc.now, tc.retention, got)
		}
	}
}

// A reprise that a disk fails says so, and leaves the operation planned, for the next to look at again (specs.md
// §17.2, invariant 4); one that cannot be relied on is the disk lost.
func TestARepriseOnADiskThatFailsSaysSo(t *testing.T) {
	p := newPool(t, func(mount string) { put(t, mount, "media/Films/a", "film") })
	row := p.copyRow("Films/a")
	op := p.beginTrash("Films/a", row)
	planned, err := catalog.PlannedOps(t.Context(), p.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.disk(func(call, _ string) error {
		if call == "stat" {
			return syscall.EIO
		}
		return nil
	}).Recover(t.Context(), p.db, planned, repriseNow, retention); !errors.Is(err, ErrDiskLost) {
		t.Errorf("%v", err)
	}
	if p.opState(op.ID) != "planned" {
		t.Errorf("the operation is %s", p.opState(op.ID))
	}
}

// The reprise syncs every folder on the way to a move's target before it looks, the top of the disk included, and each
// sync that fails is the disk lost: a way synced only in part can leave a name that leads nowhere after a power cut
// (specs.md §17.2).
func TestARepriseSyncsTheWholeWay(t *testing.T) {
	for _, folder := range []string{"", "Atoll", "trash", "2026-01-01", "1", "Films"} {
		p := newPool(t, func(mount string) { put(t, mount, "media/Films/a", "film") })
		row := p.copyRow("Films/a")
		op := p.beginTrash("Films/a", row)
		_, _ = p.disk((&killer{at: 1000}).fail).ToTrash(op, row)
		planned, err := catalog.PlannedOps(t.Context(), p.db)
		if err != nil {
			t.Fatal(err)
		}
		sync := &failingOn{call: "sync", name: folder}
		if _, err := p.disk(sync.fail).Recover(t.Context(), p.db, planned, repriseNow, retention); !errors.Is(err, ErrDiskLost) || sync.calls == 0 {
			t.Errorf("the sync of %q failing, %d syncs: %v", folder, sync.calls, err)
		}
		if p.opState(op.ID) != "planned" {
			t.Errorf("the sync of %q failing, the operation is %s", folder, p.opState(op.ID))
		}
	}
}

// killedBefore fails every call from the first of a kind on, once a call of another kind went through: kill -9 right
// before it.
type killedBefore struct {
	after, call  string
	seen, killed bool
	driver       func(call, name string) error
}

func (k *killedBefore) fail(call, name string) error {
	switch {
	case call == k.after:
		k.seen = true
	case k.seen && call == k.call:
		k.killed = true
	}
	if k.killed {
		return errKilled
	}
	if k.driver != nil {
		return k.driver(call, name)
	}
	return nil
}

// Wherever the reprise of a move a kill left fails, it says so, and leaves the operation planned; the next reprise
// resolves it, nothing lost: a failure is never taken for a step that went through (specs.md §17.2). Only what tidies
// the trash once the operation is resolved may fail unsaid: a folder of the trash that stays harms nothing.
func TestARepriseLosesNothingWhereverItFails(t *testing.T) {
	film := func(mount string) {
		put(t, mount, "media/Films/a", "film")
		put(t, mount, "media/Films/other", "x")
	}
	const trashed = "Atoll/trash/2026-01-01/1/Films/a"
	for _, tc := range []struct {
		name   string
		driver func(call, name string) error
		kill   func(driver func(call, name string) error) func(call, name string) error
		// user is what a user does once the kill came, before the reprise; text what the copy holds then.
		user  func(p *pool)
		text  string
		state string
		// at is where the reprise with nothing failing leaves the copy.
		at string
		// from is the call from which each fails in turn, the first of its kind: the steps before it are those of the
		// case before, swept there.
		from string
	}{
		// Both names there, the link made and the source's name not gone: the source's name dropped, then the way
		// synced and the move finished.
		{"ntfs-3g, both names", asNTFS3G, func(d func(string, string) error) func(string, string) error {
			return (&killedBefore{after: "link", call: "unlink", driver: d}).fail
		}, nil, "film", "done", trashed, ""},
		// The move made, then the copy edited in the trash: it goes back to its path, a link and an unlink where the
		// driver refuses the rename, and the move is undone. The looks and syncs before that are the case before's.
		{"ntfs-3g, moved, then edited", asNTFS3G, func(d func(string, string) error) func(string, string) error {
			return (&killer{driver: d}).fail
		}, func(p *pool) { writeInPlace(p.t, filepath.Join(p.mount, filepath.FromSlash(trashed)), "film, edited") }, "film, edited", "undone", "media/Films/a", "rename"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := func(p *pool) bool {
				return holding(p.mount, trashed, tc.text) || holding(p.mount, "media/Films/a", tc.text)
			}
			start := func() (*pool, *Op) {
				p := newPool(t, film)
				row := p.copyRow("Films/a")
				op := p.beginTrash("Films/a", row)
				_, _ = p.disk(tc.kill(tc.driver)).ToTrash(op, row)
				if tc.user != nil {
					tc.user(p)
				}
				return p, op
			}
			reprise := func(p *pool, f *faults) error {
				planned, err := catalog.PlannedOps(t.Context(), p.db)
				if err != nil {
					t.Fatal(err)
				}
				f.arm(true)
				_, err = p.disk(f.fail).Recover(t.Context(), p.db, planned, repriseNow, retention)
				return err
			}
			p, op := start()
			if p.opState(op.ID) != "planned" || !kept(p) {
				t.Fatal("the kill left nothing to resolve")
			}
			ref := &faults{driver: tc.driver}
			if err := reprise(p, ref); err != nil || p.opState(op.ID) != tc.state || !holding(p.mount, tc.at, tc.text) {
				t.Fatalf("with nothing failing: %v, the operation %s", err, p.opState(op.ID))
			}
			first := 0
			for first < len(ref.calls) && tc.from != "" && !strings.HasPrefix(ref.calls[first], tc.from+" ") {
				first++
			}
			for i, call := range ref.calls {
				// A folder is reached through each one above it: the first of the way failing is what its caller
				// answers, as for any of them.
				if i < first || i > 0 && strings.HasPrefix(call, "opendir ") && strings.HasPrefix(ref.calls[i-1], "opendir ") {
					continue
				}
				where := fmt.Sprintf("call %d of %d (%s) failing", i+1, len(ref.calls), call)
				p, op := start()
				err := reprise(p, &faults{at: i + 1, err: syscall.EIO, driver: tc.driver})
				if !kept(p) {
					t.Errorf("%s: the data is gone", where)
				}
				state := p.opState(op.ID)
				// Once resolved, the folders of the trash left empty are looked for, and removed: those calls alone.
				tidying := state != "planned" && (strings.HasPrefix(call, "rmdir ") || strings.HasPrefix(call, "opendir "))
				switch {
				case err == nil && !tidying:
					t.Errorf("%s: no error, and the operation is %s", where, state)
				case err != nil && !errors.Is(err, syscall.EIO):
					t.Errorf("%s: the cause is lost: %v", where, err)
				case err != nil && state != "planned":
					t.Errorf("%s: %v, and the operation is %s", where, err, state)
				}
				if _, err := p.recover(); err != nil || p.opState(op.ID) != tc.state || !holding(p.mount, tc.at, tc.text) {
					t.Errorf("%s: the next reprise: %v, the operation %s", where, err, p.opState(op.ID))
				}
			}
		})
	}
}

// A file renamed, then edited at its new name before the rename was recorded: the reprise finishes the rename, and its
// row keeps the change time it had, so that the next look reads the edit (specs.md §17.2).
func TestARenameFinishedByTheRepriseKeepsAnEditToRead(t *testing.T) {
	p := newPool(t, func(mount string) { put(t, mount, "media/Films/a", "film") })
	row := p.copyRow("Films/a")
	op, err := catalog.BeginRename(t.Context(), p.db, "t1", "Films/a", "Films/b", row.FileKey.String, repriseNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.disk(nil).RenameFile(op, row); err != nil {
		t.Fatal(err)
	}
	writeInPlace(t, filepath.Join(p.mount, "media", "Films", "b"), "film, edited")
	if _, err := p.recover(); err != nil || p.opState(op.ID) != "done" {
		t.Fatalf("%v, the operation %s", err, p.opState(op.ID))
	}
	if got := p.recorded("Films/a", "ctime_ns"); got == TimeOf(statAt(t, p.mount, "media/Films/b").Ctime).Int64 || got != row.CtimeNS.Int64 {
		t.Errorf("the row records the change time %d, its name holds %d, the row had %d", got, TimeOf(statAt(t, p.mount, "media/Films/b").Ctime).Int64,
			row.CtimeNS.Int64)
	}
}

// A link of the guard sent to the trash, a kill cut short at any write, is resolved by the reprise as a copy's move is:
// both names there, its data unchanged, it finishes, as a link has no row its data could be edited from (specs.md §17,
// §17.2).
func TestTheRepriseOfALinkSentToTheTrash(t *testing.T) {
	const place = "Atoll/trash/2026-01-01/1/Films/Gone/a"
	prepare := func(mount string) {
		put(t, mount, "media/Films/other", "x")
		put(t, mount, "media/Films/Gone/a", "film")
		link(t, mount, "media/Films/Gone/a", "Atoll/links/42")
		if err := os.Remove(filepath.Join(mount, "media", "Films", "Gone", "a")); err != nil {
			t.Fatal(err)
		}
	}
	start := func(p *pool) (*Op, func(d *Disk) error) {
		op, err := catalog.BeginTrash(p.t.Context(), p.db, &catalog.TrashStart{Disk: "t1", Path: "Films/Gone/a", FileKey: keyAt(p.t, p.mount, "Atoll/links/42"),
			Reason: "deleted", NowNS: repriseNow, ExpiresNS: AddRetention(repriseNow, retention), Link: 42})
		if err != nil {
			p.t.Fatal(err)
		}
		return op, func(d *Disk) error { _, err := d.LinkToTrash(op); return err }
	}
	done := func(p *pool) bool { return holding(p.mount, place, "film") && absent(p.mount, "Atoll/links/42") }
	undone := func(p *pool) bool { return holding(p.mount, "Atoll/links/42", "film") && absent(p.mount, place) }
	kept := func(p *pool) bool {
		return holding(p.mount, place, "film") || holding(p.mount, "Atoll/links/42", "film")
	}
	for _, driver := range []struct {
		name string
		fail func(call, name string) error
	}{{"ext4", nil}, {"ntfs-3g", asNTFS3G}} {
		t.Run(driver.name, func(t *testing.T) {
			reprise(t, repriseCase{name: "a link to the trash", driver: driver.fail, prepare: prepare, start: start, done: done, undone: undone, kept: kept})
		})
	}
	p := newPool(t, prepare)
	op, _ := start(p)
	if _, err := p.disk((&killedBefore{after: "link", call: "unlink", driver: asNTFS3G}).fail).LinkToTrash(op); !errors.Is(err, errKilled) {
		t.Fatalf("not killed before the link's name went: %v", err)
	}
	if _, err := p.recover(); err != nil || p.opState(op.ID) != "done" || !done(p) {
		t.Errorf("both names there: %v, the operation %s", err, p.opState(op.ID))
	}
}

// A file with two names, one of them sent to the trash, a kill once it moved: the reprise records what the move did to
// the name that stays, its change time and its count of names, as the move moved them for both (specs.md §17.1).
func TestTheRepriseOfAFileWithTwoNames(t *testing.T) {
	p := newPool(t, func(mount string) {
		put(t, mount, "media/Films/a", "film")
		link(t, mount, "media/Films/a", "media/Films/b")
	})
	row := p.copyRow("Films/a")
	op := p.beginTrash("Films/a", row)
	if _, err := p.disk((&killer{}).fail).ToTrash(op, row); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recover(); err != nil || p.opState(op.ID) != "done" {
		t.Fatalf("%v, the operation %s", err, p.opState(op.ID))
	}
	st := statAt(t, p.mount, "media/Films/b")
	if got := p.recorded("Films/b", "ctime_ns"); got != TimeOf(st.Ctime).Int64 {
		t.Errorf("b records the change time %d, its name holds %d", got, TimeOf(st.Ctime).Int64)
	}
	if got := p.recorded("Films/b", "nlink"); got != int64(st.Nlink) {
		t.Errorf("b records %d names, it has %d", got, st.Nlink)
	}
}

// A repair a kill cut short at any write, a copy of one chunk that rotted, is resolved by the reprise from what the copy
// holds and what its folder in the trash holds: not begun, nothing happened; its chunk saved, it ends done, the copy as
// it was or repaired (specs.md §16.1, §17.2). And the links of the guard, made or removed, end with what the links
// folder holds.
func TestTheRepriseOfARepairAndOfTheGuard(t *testing.T) {
	const chunks = "Atoll/trash/2026-01-01/1.chunks/Films/a"
	film := func(mount string) {
		put(t, mount, "media/Films/a", "film")
		put(t, mount, "media/Films/other", "x")
	}
	// A copy of one chunk, then one of two, the last shorter, every chunk rotted: each saved from its offset, then each
	// written; what the copy holds at each chunk is then its good bytes or its bad ones.
	for _, size := range []int{4, integrity.ChunkSize + 3} {
		good := bytes.Repeat([]byte("f"), size)
		bad := bytes.Repeat([]byte("b"), size)
		chunked := func(data []byte) [][]byte {
			var out [][]byte
			for at := 0; at < len(data); at += integrity.ChunkSize {
				out = append(out, data[at:min(at+integrity.ChunkSize, len(data))])
			}
			return out
		}
		// holds says whether each chunk of the copy is its good bytes or its bad ones.
		holds := func(p *pool) bool {
			data, err := os.ReadFile(filepath.Join(p.mount, "media", "Films", "a"))
			if err != nil || len(data) != size {
				return false
			}
			for i, c := range chunked(data) {
				if !bytes.Equal(c, chunked(good)[i]) && !bytes.Equal(c, chunked(bad)[i]) {
					return false
				}
			}
			return true
		}
		// saved says whether a chunk reached the trash, and each one there holds what the copy held there.
		saved := func(p *pool) bool {
			some := false
			for i, c := range chunked(bad) {
				place := fmt.Sprintf("%s/%08d", chunks, i)
				if absent(p.mount, place) {
					continue
				}
				if !holding(p.mount, place, string(c)) {
					return false
				}
				some = true
			}
			return some
		}
		reprise(t, repriseCase{name: fmt.Sprintf("a repair of %d bytes", size),
			prepare: func(mount string) {
				put(t, mount, "media/Films/a", string(good))
				put(t, mount, "media/Films/other", "x")
			},
			start: func(p *pool) (*Op, func(d *Disk) error) {
				// Read whole, for the hashes of its chunks.
				if _, err := watch.Scan(p.t.Context(), p.db, p.config, p.machine, watch.Options{Full: true}); err != nil {
					p.t.Fatal(err)
				}
				full := filepath.Join(p.mount, "media", "Films", "a")
				at := mtimeAt(p.t, full)
				writeInPlace(p.t, full, string(bad))
				setMtimeAt(p.t, full, at)
				if _, err := p.db.ExecContext(p.t.Context(), "UPDATE copies SET ctime_ns = ?", TimeOf(statAt(p.t, p.mount, "media/Films/a").Ctime).Int64); err != nil {
					p.t.Fatal(err)
				}
				if p.count("SELECT count(*) FROM file_chunks") != 2 {
					p.t.Fatal("the files are not hashed")
				}
				row := p.copyRow("Films/a")
				var badChunks []int64
				for i := range chunked(bad) {
					badChunks = append(badChunks, int64(i))
				}
				op, err := catalog.BeginRepair(p.t.Context(), p.db, &catalog.RepairStart{Disk: "t1", Path: "Films/a", FileKey: row.FileKey.String, Bad: badChunks,
					Size: int64(size), MtimeNS: row.MtimeNS.Int64, NowNS: repriseNow, ExpiresNS: AddRetention(repriseNow, retention)})
				if err != nil {
					p.t.Fatal(err)
				}
				return op, func(d *Disk) error {
					f, st, err := d.OpenRepair(row)
					if err != nil {
						return err
					}
					defer func() { _ = f.Close() }()
					for i, c := range chunked(bad) {
						if err := d.SaveChunk(op, int64(i), c, &st); err != nil {
							return err
						}
					}
					if err := d.SyncChunks(op); err != nil {
						return err
					}
					for i, c := range chunked(good) {
						sum := integrity.ChunkSum(c)
						if err := d.WriteChunk(f, int64(i), c, sum[:]); err != nil {
							return err
						}
					}
					_, err = d.SetTimeBack(f, fsx.TimeOf(row.MtimeNS.Int64))
					return err
				}
			},
			done: func(p *pool) bool { return saved(p) && holds(p) },
			// Nothing saved, nothing written: the reprise says it stopped it.
			undone: func(p *pool) bool {
				return holding(p.mount, "media/Films/a", string(bad)) && absent(p.mount, chunks+"/00000000") &&
					p.count("SELECT count(*) FROM events WHERE kind = ? AND json_extract(data, '$.reason') = 'reprise'", catalog.EventRepairStopped) == 1
			},
			kept: holds,
		})
	}
	guard := func(kind string, drop bool) repriseCase {
		return repriseCase{name: "a link " + kind,
			prepare: func(mount string) {
				film(mount)
				if drop {
					link(t, mount, "media/Films/a", "Atoll/links/1")
				}
			},
			start: func(p *pool) (*Op, func(d *Disk) error) {
				id := p.count("SELECT id FROM files WHERE lpath = CAST('Films/a' AS BLOB)")
				if id != 1 {
					p.t.Fatalf("the file is %d", id)
				}
				list, err := catalog.BeginLinks(p.t.Context(), p.db, "t1", kind, []catalog.LinkStart{{FileID: 1, Path: "Films/a", FileKey: keyAt(p.t, p.mount, "media/Films/a")}}, repriseNow)
				if err != nil {
					p.t.Fatal(err)
				}
				op := &list[0]
				return op, func(d *Disk) error {
					folder, err := d.OpenLinks(!drop)
					if err != nil {
						return err
					}
					defer folder.Close()
					if drop {
						_, err = d.DropLink(op, folder)
					} else {
						_, err = d.MakeLink(op, folder)
					}
					if err != nil {
						return err
					}
					return folder.Sync()
				}
			},
			done:   func(p *pool) bool { return drop == absent(p.mount, "Atoll/links/1") },
			undone: func(p *pool) bool { return drop != absent(p.mount, "Atoll/links/1") },
			kept:   func(p *pool) bool { return holding(p.mount, "media/Films/a", "film") },
		}
	}
	reprise(t, guard(catalog.OpLink, false))
	reprise(t, guard(catalog.OpUnlink, true))
}

// What Atoll's own folders hold of the inodes a walk no longer found: a copy its move sent to the trash, found by the
// journal's name for it, a part in tmp/, whatever else tmp/ holds, and, once the trash holds what the journal does not
// name, what a walk of the trash finds; nothing of an inode that is none of these (specs.md §15.5, invariant 11).
func TestOwnFilesFindWhatAtollsFoldersHold(t *testing.T) {
	p := newPool(t, func(mount string) {
		put(t, mount, "media/Films/a", "film")
		put(t, mount, "media/Films/b", "other")
	})
	trashedKey := p.copyRow("Films/a").FileKey.String
	p.trashed("Films/a")
	put(t, p.mount, "Atoll/tmp/1.part", "another part")
	put(t, p.mount, "Atoll/tmp/5.part", "part")
	partKey := keyAt(t, p.mount, "Atoll/tmp/5.part")
	elsewhere := keyAt(t, p.mount, "media/Films/b")
	d := p.disk(nil)
	out, err := d.OwnFiles(t.Context(), p.db, map[string]bool{trashedKey: true, partKey: true, elsewhere: true}, watch.Unbounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(out[trashedKey]) != 1 || out[trashedKey][0].Size != 4 || len(out[partKey]) != 1 || out[partKey][0].Size != 4 || len(out[elsewhere]) != 0 {
		t.Errorf("%+v", out)
	}
	// A file the journal's own folder of an operation holds beside its entry: the journal answers for the trash, and
	// nothing is walked there.
	put(t, p.mount, "Atoll/trash/2026-01-01/1/Films/beside", "beside")
	beside := keyAt(t, p.mount, "Atoll/trash/2026-01-01/1/Films/beside")
	if out, err := d.OwnFiles(t.Context(), p.db, map[string]bool{beside: true}, watch.Unbounded); err != nil || len(out[beside]) != 0 {
		t.Errorf("beside an entry the journal names: %+v, %v", out, err)
	}
	put(t, p.mount, "Atoll/trash/2026-01-01/99/Films/x", "stranger")
	stranger := keyAt(t, p.mount, "Atoll/trash/2026-01-01/99/Films/x")
	out, err = d.OwnFiles(t.Context(), p.db, map[string]bool{stranger: true, elsewhere: true}, watch.Unbounded)
	if err != nil || len(out[stranger]) != 1 || out[stranger][0].Size != 8 || len(out[elsewhere]) != 0 {
		t.Errorf("what the journal does not name: %+v, %v", out, err)
	}
	unread := &failingOn{call: "names", name: "trash"}
	if _, err := p.disk(unread.fail).OwnFiles(t.Context(), p.db, map[string]bool{stranger: true}, watch.Unbounded); !errors.Is(err, ErrDiskLost) {
		t.Errorf("a trash that cannot be listed: %v", err)
	}
}

// What the names of a file that stay record once one goes: what a look at the first still there finds, with the change
// time the file had before when that is known; nothing when no other name is left (specs.md §17.1).
func TestTheNamesLeftOfAFile(t *testing.T) {
	p := newPool(t, func(mount string) {
		put(t, mount, "media/Films/a", "film")
		link(t, mount, "media/Films/a", "media/Films/b")
		put(t, mount, "media/Films/c", "alone")
	})
	d := p.disk(nil)
	key := keyAt(t, p.mount, "media/Films/a")
	prior := sql.NullInt64{Int64: 5, Valid: true}
	links, err := d.LinksLeft(t.Context(), p.db, key, prior)
	st := statAt(t, p.mount, "media/Films/b")
	if err != nil || links == nil || links.FileKey != key || links.Nlink != int64(st.Nlink) || links.CtimeNS != TimeOf(st.Ctime).Int64 || links.PriorNS != prior {
		t.Errorf("%+v, %v", links, err)
	}
	if links, err := d.LinksLeft(t.Context(), p.db, "1", prior); err != nil || links != nil {
		t.Errorf("no name of it: %+v, %v", links, err)
	}
}

// A folder a kill kept from going, its name holding another folder by the time the reprise looks, is someone else's:
// the removal is undone, and a warning says that what was there is not what Atoll's operation left (specs.md §17.2).
func TestTheRepriseOfAFolderRemovedFindsAnother(t *testing.T) {
	p := newPool(t, func(mount string) {
		put(t, mount, "media/Films/a", "film")
		if err := os.Mkdir(filepath.Join(mount, "media", "Films", "Old"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	op, err := catalog.BeginRmdir(t.Context(), p.db, "t1", "Films/Old", keyAt(t, p.mount, "media/Films/Old"), repriseNow)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(p.mount, "media", "Films", "Old")
	if err := os.Rename(old, old+".was"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recover(); err != nil || p.opState(op.ID) != "undone" || absent(p.mount, "media/Films/Old") {
		t.Fatalf("%v, the operation %s", err, p.opState(op.ID))
	}
	if got := p.count("SELECT count(*) FROM events WHERE kind = 'op-undone' AND json_extract(data, '$.op') = ?", op.ID); got != 1 {
		t.Errorf("%d warnings", got)
	}
}

// A change of a copy's time, mode or owner a kill left unrecorded, once it ran, then the copy changed by someone else:
// edited to another size, or replaced by another file of its size, or given another group, the change it shows is no
// longer Atoll's alone, and is undone with a warning, whatever it holds of what was set; a copy that cannot be synced
// first says so (specs.md §15.4, §17.2).
func TestTheRepriseOfAMetadataChangeTheUserChangedAgain(t *testing.T) {
	wanted := fsx.Time{Sec: 1000, Nsec: 5}
	setTime := func(p *pool) (*Op, func(d *Disk) error) {
		row := p.copyRow("Films/a")
		op, err := catalog.BeginUtimens(p.t.Context(), p.db, "t1", "Films/a", row.FileKey.String, 1000*int64(time.Second)+5, repriseNow)
		if err != nil {
			p.t.Fatal(err)
		}
		return op, func(d *Disk) error { _, err := d.SetMtime(row, wanted); return err }
	}
	setOwner := func(p *pool) (*Op, func(d *Disk) error) {
		row := p.copyRow("Films/a")
		op, err := catalog.BeginChown(p.t.Context(), p.db, "t1", "Films/a", row.FileKey.String, 1000, 1001, repriseNow)
		if err != nil {
			p.t.Fatal(err)
		}
		return op, func(d *Disk) error { _, err := d.SetOwner(row, 1000, 1001); return err }
	}
	full := func(p *pool) string { return filepath.Join(p.mount, "media", "Films", "a") }
	for _, tc := range []struct {
		name  string
		start func(p *pool) (*Op, func(d *Disk) error)
		user  func(p *pool)
		root  bool
	}{
		{"its time set, then edited to another size", setTime, func(p *pool) {
			writeInPlace(p.t, full(p), "a longer film")
			setMtimeAt(p.t, full(p), time.Unix(1000, 5))
		}, false},
		{"its time set, then replaced by a file of its size", setTime, func(p *pool) {
			put(p.t, p.mount, "media/Films/new", "mlif")
			setMtimeAt(p.t, filepath.Join(p.mount, "media", "Films", "new"), time.Unix(1000, 5))
			if err := os.Rename(filepath.Join(p.mount, "media", "Films", "new"), full(p)); err != nil {
				p.t.Fatal(err)
			}
		}, false},
		{"its owner set, then given another group", setOwner, func(p *pool) {
			if err := os.Chown(full(p), 1000, 2000); err != nil {
				p.t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.root && os.Getuid() != 0 {
				t.Skip("giving a file to another account needs root")
			}
			p := newPool(t, func(mount string) { put(t, mount, "media/Films/a", "film") })
			op, run := tc.start(p)
			if err := run(p.disk(nil)); err != nil {
				t.Fatal(err)
			}
			tc.user(p)
			if _, err := p.recover(); err != nil || p.opState(op.ID) != "undone" {
				t.Fatalf("%v, the operation %s", err, p.opState(op.ID))
			}
			if got := p.count("SELECT count(*) FROM events WHERE kind = 'op-undone' AND json_extract(data, '$.op') = ?", op.ID); got != 1 {
				t.Errorf("%d warnings", got)
			}
		})
	}
	t.Run("a copy that cannot be synced", func(t *testing.T) {
		p := newPool(t, func(mount string) { put(t, mount, "media/Films/a", "film") })
		op, run := setTime(p)
		if err := run(p.disk(nil)); err != nil {
			t.Fatal(err)
		}
		planned, err := catalog.PlannedOps(t.Context(), p.db)
		if err != nil {
			t.Fatal(err)
		}
		sync := &failingOn{call: "filesync", name: "a"}
		if _, err := p.disk(sync.fail).Recover(t.Context(), p.db, planned, repriseNow, retention); !errors.Is(err, ErrDiskLost) || sync.calls != 1 {
			t.Errorf("%d syncs: %v", sync.calls, err)
		}
		if p.opState(op.ID) != "planned" {
			t.Errorf("the operation is %s", p.opState(op.ID))
		}
	})
}
