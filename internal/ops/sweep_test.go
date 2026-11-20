package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// A sweep runs an operation of a disk once with nothing failing, counting each call it makes through the handle on the
// disk's top, then once for each of those calls, on a new disk each time, with that call failing: with EIO, what a disk
// that cannot be relied on answers, then with EACCES, a refusal that says nothing about the disk nor about the name
// (CLAUDE.md, "Testing rules": fault injection at each step). Whatever fails, and wherever:
//   - the data the operation is about is still on the disk, at one of its names at least: nothing is lost (invariant 1);
//   - no error means that the operation did all it does: a failure is never taken for a success;
//   - EIO comes back as itself, and as the disk lost where the operation classifies what it gives, never as a name gone,
//     taken or refused, nor as a folder not empty: it is "disk unknown", never "file absent" (invariant 4);
//   - EACCES comes back as itself, never as a name gone or taken, nor as the disk lost but from a sync, which
//     classifies any failure as the disk lost (specs.md §17.1).
type sweepCase struct {
	name string
	// driver answers each call as a driver would, before the fault: asNTFS3G refuses a rename that replaces nothing.
	driver func(call, name string) error
	// raw says that the operation gives a disk's error as it is, unclassified: its callers stop at any error.
	raw bool
	// refusal is what the operation answers, by design, for a refusal: nil when it gives the refusal as it is.
	refusal error
	// prepare makes what the operation needs on a new disk at mount, and gives it.
	prepare func(t *testing.T, mount string) swept
}

// swept is an operation a sweep runs, and what tells whether it did its work.
type swept struct {
	op func(d *Disk) error
	// done says whether the operation's whole effect is there: what no error must mean.
	done func() bool
	// kept says whether the data the operation is about is still on the disk; nil for one that moves none.
	kept func() bool
	// best says what only a run with nothing failing must give besides: what a driver may refuse, with a warning, as
	// one that holds no mode does, or what a look the operation does not depend on may miss; nil for nothing more.
	best func() bool
}

// faults counts the calls made through a disk's top while armed, and makes the one at a position fail.
type faults struct {
	mu     sync.Mutex
	armed  bool
	calls  []string
	at     int
	err    error
	driver func(call, name string) error
}

func (f *faults) fail(call, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed {
		f.calls = append(f.calls, call+" "+name)
		if len(f.calls) == f.at {
			return f.err
		}
	}
	if f.driver != nil {
		return f.driver(call, name)
	}
	return nil
}

func (f *faults) arm(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = on
}

// runSwept runs a case on a new disk at mount, with the call at a position failing with err; 0 fails none.
func runSwept(t *testing.T, c *sweepCase, mount string, at int, inject error) (calls []string, s swept, err error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	s = c.prepare(t, mount)
	f := &faults{at: at, err: inject, driver: c.driver}
	d, err := Open(&onDisk{fail: f.fail}, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll", FS: names.Ext4}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f.arm(true)
	err = s.op(d)
	f.arm(false)
	return f.calls, s, err
}

// syncs are the calls whose failure is the disk lost, whatever it says (specs.md §17.1).
var syncs = map[string]bool{"sync": true, "syncentry": true, "filesync": true, "syncfs": true}

func sweep(t *testing.T, c sweepCase) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		top := t.TempDir()
		calls, s, err := runSwept(t, &c, filepath.Join(top, "reference"), 0, nil)
		if err != nil {
			t.Fatalf("with nothing failing: %v", err)
		}
		if !s.done() || (s.best != nil && !s.best()) {
			t.Fatal("with nothing failing, the operation is not done")
		}
		if len(calls) == 0 {
			t.Fatal("no call went through the disk's top")
		}
		for i, call := range calls {
			kind := strings.Fields(call)[0]
			for _, inject := range []error{syscall.EIO, syscall.EACCES} {
				where := fmt.Sprintf("call %d of %d (%s) failing with %v", i+1, len(calls), call, inject)
				_, s, err := runSwept(t, &c, filepath.Join(top, fmt.Sprintf("%d-%d", i, inject)), i+1, inject)
				if s.kept != nil && !s.kept() {
					t.Errorf("%s: the data is gone (%v)", where, err)
				}
				if err == nil {
					if !s.done() {
						t.Errorf("%s: no error, and the operation is not done", where)
					}
					continue
				}
				if !errors.Is(err, inject) {
					t.Errorf("%s: the cause is lost: %v", where, err)
				}
				for _, answer := range []error{ErrGone, ErrPathTaken, ErrNameRefused, ErrNotEmpty, ErrNoLink, ErrLastName} {
					if errors.Is(err, answer) && (inject != syscall.EACCES || answer != c.refusal) {
						t.Errorf("%s: taken for %q: %v", where, answer, err)
					}
				}
				switch {
				case inject == syscall.EIO && !c.raw && !errors.Is(err, ErrDiskLost):
					t.Errorf("%s: not the disk lost: %v", where, err)
				case inject == syscall.EACCES && !syncs[kind] && errors.Is(err, ErrDiskLost):
					t.Errorf("%s: a refusal taken for the disk lost: %v", where, err)
				}
			}
		}
	})
}

// put writes a file below a mount, with the folders on the way.
func put(t *testing.T, mount, rel, text string) {
	t.Helper()
	full := filepath.Join(mount, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// holding says whether a file below a mount holds a text.
func holding(mount, rel, text string) bool {
	data, err := os.ReadFile(filepath.Join(mount, filepath.FromSlash(rel)))
	return err == nil && string(data) == text
}

// absent says whether nothing is at a name below a mount.
func absent(mount, rel string) bool {
	_, err := os.Lstat(filepath.Join(mount, filepath.FromSlash(rel)))
	return errors.Is(err, os.ErrNotExist)
}

// statAt looks at a name below a mount, without following a link.
func statAt(t *testing.T, mount, rel string) fsx.Stat {
	t.Helper()
	full := filepath.Join(mount, filepath.FromSlash(rel))
	dir, err := fsx.OpenDir(filepath.Dir(full))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Stat(filepath.Base(full))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// keyAt gives the inode of what is at a name below a mount, as an operation records it.
func keyAt(t *testing.T, mount, rel string) string {
	t.Helper()
	st := statAt(t, mount, rel)
	return keyOf(&st)
}

// rowAt is what the catalog would say of the copy at a logical path, after a look at it.
func rowAt(t *testing.T, mount, logical string) *catalog.KnownCopy {
	t.Helper()
	st := statAt(t, mount, "media/"+logical)
	f, err := watch.FileFrom(logical, &st)
	if err != nil {
		t.Fatal(err)
	}
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	return &catalog.KnownCopy{
		Path: logical, FilePath: logical, FileID: 1, Size: valid(f.Size), MtimeNS: valid(f.MtimeNS), CtimeNS: valid(f.CtimeNS),
		FileKey: sql.NullString{String: f.FileKey, Valid: true}, Mode: valid(f.Mode), UID: valid(int64(f.UID)), GID: valid(int64(f.GID)),
	}
}

// sameFile says whether two names below a mount lead to one file.
func sameFile(t *testing.T, mount, a, b string) bool {
	t.Helper()
	sa, sb := statAt(t, mount, a), statAt(t, mount, b)
	return sa.Ino == sb.Ino
}

func TestMovesLoseNothingWhereverTheyFail(t *testing.T) {
	const trashed = "Atoll/trash/2026-01-01/7/Films/a"
	toTrash := func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		row := rowAt(t, mount, "Films/a")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/a", Path2: "2026-01-01/7/Films/a", FileKey: row.FileKey.String}
		return swept{
			op:   func(d *Disk) error { _, err := d.ToTrash(op, row); return err },
			done: func() bool { return holding(mount, trashed, "film") && absent(mount, "media/Films/a") },
			kept: func() bool { return holding(mount, trashed, "film") || holding(mount, "media/Films/a", "film") },
		}
	}
	fromTrash := func(t *testing.T, mount string) swept {
		put(t, mount, trashed, "film")
		put(t, mount, "media/Films/other", "x")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRestore, Path: "Films/a", Path2: "2026-01-01/7/Films/a", FileKey: keyAt(t, mount, trashed)}
		return swept{
			op:   func(d *Disk) error { _, err := d.FromTrash(op); return err },
			done: func() bool { return holding(mount, "media/Films/a", "film") && absent(mount, trashed) },
			kept: func() bool { return holding(mount, trashed, "film") || holding(mount, "media/Films/a", "film") },
		}
	}
	renameFile := func(t *testing.T, mount string) swept {
		put(t, mount, "media/a", "film")
		put(t, mount, "media/Films/other", "x")
		row := rowAt(t, mount, "a")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRename, Path: "a", Path2: "Films/b", FileKey: row.FileKey.String}
		return swept{
			op:   func(d *Disk) error { _, _, err := d.RenameFile(op, row); return err },
			done: func() bool { return holding(mount, "media/Films/b", "film") && absent(mount, "media/a") },
			kept: func() bool { return holding(mount, "media/Films/b", "film") || holding(mount, "media/a", "film") },
		}
	}
	renameFolder := func(t *testing.T, mount string) swept {
		put(t, mount, "media/d/f", "film")
		put(t, mount, "media/Films/other", "x")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRename, Path: "d", Path2: "Films/e", FileKey: keyAt(t, mount, "media/d")}
		return swept{
			op:   func(d *Disk) error { return d.RenameFolder(op) },
			done: func() bool { return holding(mount, "media/Films/e/f", "film") && absent(mount, "media/d") },
			kept: func() bool { return holding(mount, "media/Films/e/f", "film") || holding(mount, "media/d/f", "film") },
		}
	}
	placePart := func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/tmp/5.part", "film")
		put(t, mount, "media/Films/other", "x")
		op := &Op{ID: 5, Disk: "t1", Kind: catalog.OpCreate, Path: "Films/a", Path2: "5.part", FileKey: keyAt(t, mount, "Atoll/tmp/5.part")}
		recorded := false
		return swept{
			op: func(d *Disk) error {
				_, err := d.PlacePart(op, "Films/a", func(placed *fsx.Stat) error {
					recorded = keyOf(placed) == op.FileKey
					return nil
				})
				return err
			},
			done: func() bool {
				return recorded && holding(mount, "media/Films/a", "film") && absent(mount, "Atoll/tmp/5.part")
			},
			kept: func() bool { return holding(mount, "media/Films/a", "film") || holding(mount, "Atoll/tmp/5.part", "film") },
		}
	}
	linkToTrash := func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/links/42", "film")
		put(t, mount, "media/Films/other", "x")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/Gone/a", Path2: "2026-01-01/7/Films/Gone/a", Link: 42,
			FileKey: keyAt(t, mount, "Atoll/links/42")}
		const place = "Atoll/trash/2026-01-01/7/Films/Gone/a"
		return swept{
			op:   func(d *Disk) error { _, err := d.LinkToTrash(op); return err },
			done: func() bool { return holding(mount, place, "film") && absent(mount, "Atoll/links/42") },
			kept: func() bool { return holding(mount, place, "film") || holding(mount, "Atoll/links/42", "film") },
		}
	}
	exchange := func(t *testing.T, mount string) swept {
		put(t, mount, "media/a", "A")
		put(t, mount, "media/Films/b", "B")
		rowA, rowB := rowAt(t, mount, "a"), rowAt(t, mount, "Films/b")
		a := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRename, Path: "a", Path2: "Films/b", FileKey: rowA.FileKey.String}
		b := &Op{ID: 8, Disk: "t1", Kind: catalog.OpRename, Path: "Films/b", Path2: "a", FileKey: rowB.FileKey.String}
		return swept{
			op:   func(d *Disk) error { _, _, _, _, err := d.ExchangeFiles(a, b, rowA, rowB); return err },
			done: func() bool { return holding(mount, "media/a", "B") && holding(mount, "media/Films/b", "A") },
			kept: func() bool {
				return (holding(mount, "media/a", "A") || holding(mount, "media/Films/b", "A")) &&
					(holding(mount, "media/a", "B") || holding(mount, "media/Films/b", "B"))
			},
		}
	}
	for _, driver := range []struct {
		name string
		fail func(call, name string) error
	}{{"ext4", nil}, {"ntfs-3g", asNTFS3G}} {
		t.Run(driver.name, func(t *testing.T) {
			sweep(t, sweepCase{name: "to the trash", driver: driver.fail, prepare: toTrash})
			sweep(t, sweepCase{name: "from the trash", driver: driver.fail, prepare: fromTrash})
			sweep(t, sweepCase{name: "a file renamed", driver: driver.fail, prepare: renameFile})
			sweep(t, sweepCase{name: "a folder renamed", driver: driver.fail, prepare: renameFolder})
			sweep(t, sweepCase{name: "a part placed", driver: driver.fail, prepare: placePart})
			sweep(t, sweepCase{name: "a link to the trash", driver: driver.fail, prepare: linkToTrash})
		})
	}
	// No NTFS driver exchanges two names: only ext4, XFS and btrfs are asked to (specs.md §15.12).
	sweep(t, sweepCase{name: "two files exchanged", prepare: exchange})
}
