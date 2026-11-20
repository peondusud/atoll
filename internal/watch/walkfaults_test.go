package watch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
)

// orderly lists each folder's names in byte order, so that a test knows which folder a walk comes back to, and makes
// a call fail by the name of the folder it is about and the count of opens of that name so far: "opendir" of a name,
// and "info" of the folder opened by that name, "" for the top.
type orderly struct {
	fsx.Dir
	name  string
	opens map[string]int
	fail  func(call, name string, opens int) error
}

func newOrderly(d fsx.Dir, fail func(call, name string, opens int) error) *orderly {
	return &orderly{Dir: d, opens: map[string]int{}, fail: fail}
}

func (d *orderly) Unwrap() fsx.Dir { return d.Dir }

func (d *orderly) Names() ([]string, error) {
	names, err := d.Dir.Names()
	slices.Sort(names)
	return names, err //nolint:wrapcheck // a test double
}

func (d *orderly) Info() (fsx.Stat, error) {
	if err := d.fail("info", d.name, d.opens[d.name]); err != nil {
		return fsx.Stat{}, err
	}
	return d.Dir.Info() //nolint:wrapcheck // a test double
}

func (d *orderly) OpenDir(name string) (fsx.Dir, error) {
	d.opens[name]++
	if err := d.fail("opendir", name, d.opens[name]); err != nil {
		return nil, err
	}
	below, err := d.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &orderly{Dir: below, name: name, opens: d.opens, fail: d.fail}, nil
}

// deepTree writes a/b/c/f and a/z: with walk.max_open = 3, a walk lets go of a while it is in c, and opens it again from
// the root to read z.
func deepTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"a/b/c/f", "a/z"} {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func walkWith(t *testing.T, root string, limits Limits, fail func(call, name string, opens int) error) (*Walked, error) {
	t.Helper()
	dir, err := fsx.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	return Walk(t.Context(), newOrderly(dir, fail), limits)
}

// A walk that cannot open again a folder it let go of, or cannot tell that it is the folder it left, stops as one whose
// disk does not answer; one gone by then is left aside, with what was found below it, and the walk goes on (invariant
// 4, specs.md §15.8, §15.10).
func TestAFolderOpenedAgain(t *testing.T) {
	limits := Limits{MaxPath: NoBound, MaxOpen: 3}
	if w, err := walkWith(t, deepTree(t), limits, func(string, string, int) error { return nil }); err != nil ||
		len(w.Files) != 2 || len(w.Unreadable) != 0 {
		t.Fatalf("a walk with nothing failing: %+v, %v", w, err)
	}
	for _, tc := range []struct {
		what string
		fail func(call, name string, opens int) error
	}{
		{"the root cannot say what it is", func(call, name string, _ int) error {
			if call == "info" && name == "" {
				return syscall.EIO
			}
			return nil
		}},
		{"its open does not answer", func(call, name string, opens int) error {
			if call == "opendir" && name == "a" && opens == 2 {
				return syscall.EIO
			}
			return nil
		}},
		{"it cannot say what it is", func(call, name string, opens int) error {
			if call == "info" && name == "a" && opens == 2 {
				return syscall.ENOTCONN
			}
			return nil
		}},
	} {
		w, err := walkWith(t, deepTree(t), limits, tc.fail)
		if !errors.Is(err, ErrInterrupted) || w != nil {
			t.Errorf("%s: %+v, %v", tc.what, w, err)
		}
	}
	w, err := walkWith(t, deepTree(t), limits, func(call, name string, opens int) error {
		if call == "opendir" && name == "a" && opens == 2 {
			return syscall.ENOENT
		}
		return nil
	})
	if err != nil || !slices.Equal(w.Unreadable, []string{"a"}) || len(w.Files) != 0 || len(w.Entries) != 0 {
		t.Errorf("a folder gone once it was let go of: %+v, %v", w, err)
	}
}

// A time that no filesystem holds, nanoseconds past a second, is no answer of a disk: the walk stops as one whose disk
// does not answer, and records nothing with a wrong time (invariant 4).
func TestATimeNoDiskHoldsStopsTheWalk(t *testing.T) {
	for what, change := range map[string]func(*fsx.Stat){
		"modification": func(st *fsx.Stat) { st.Mtime.Nsec = 1e9 },
		"change":       func(st *fsx.Stat) { st.Ctime.Nsec = -1 },
		"creation":     func(st *fsx.Stat) { st.HasBirth, st.Birth.Sec, st.Birth.Nsec = true, 5, 2e9 },
	} {
		for _, of := range []string{"z", "b"} {
			root := deepTree(t)
			dir, err := fsx.OpenDir(root)
			if err != nil {
				t.Fatal(err)
			}
			w, err := Walk(t.Context(), &twisted{Dir: dir, change: func(name string, st *fsx.Stat) {
				if name == of {
					change(st)
				}
			}}, Unbounded)
			_ = dir.Close()
			wantErr := what != "change" || of != "b"
			if got := errors.Is(err, ErrInterrupted); got != wantErr || got && w != nil {
				t.Errorf("%s time of %s: %+v, %v", what, of, w, err)
			}
		}
	}
	if _, err := FileFrom("x", &fsx.Stat{Mtime: fsx.Time{Nsec: 1e9}}); err == nil {
		t.Error("a file with a modification time no disk holds")
	}
	if f, err := FileFrom("x", &fsx.Stat{Size: 3, Ino: 7, Nlink: 1, HasBirth: true, Birth: fsx.Time{Sec: 0}}); err != nil || f.FileKey != "7" || f.BtimeNS != nil {
		t.Errorf("a file with no creation time: %+v, %v", f, err)
	}
}

// A walk of the whole disk goes below every name of its own folders too, as of its root (specs.md §15.8).
func TestOwnFoldersOfAWalkWithNoBound(t *testing.T) {
	if got := (Limits{MaxPath: NoBound, MaxOpen: 4}).Own(); got != (Limits{MaxPath: NoBound, MaxOpen: 4}) {
		t.Errorf("%+v", got)
	}
	if got := (Limits{MaxPath: 100, MaxOpen: 4}).Own(); got.MaxPath <= 100 || got.MaxOpen != 4 {
		t.Errorf("%+v", got)
	}
}

// shifting is a bench whose disk changes once a walk has opened its root, as a disk unmounted, or its folders moved,
// while it is walked.
type shifting struct {
	*bench
	after     func()
	shifted   bool
	unmounted bool
}

func (m *shifting) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := m.bench.OpenRoot(mount, root)
	if err == nil && root == "media" && !m.shifted {
		m.shifted = true
		m.after()
	}
	return dir, err
}

func (m *shifting) Mount(mount string) disks.MountSeen {
	if m.unmounted {
		// Nothing is mounted there: the mount point is an empty folder.
		return disks.MountSeen{Path: disks.PathOK}
	}
	return m.bench.Mount(mount)
}

// A walk reads its root through a handle opened some time after the disk's check. A disk unmounted, or a mount point
// or a root moved, once the walk opened it: the walk read a folder that is not the disk's, and concludes nothing, every
// file of the disk being no absence (invariant 4, specs.md §10).
func TestADiskThatShiftsDuringItsWalk(t *testing.T) {
	for _, tc := range []struct {
		what  string
		after func(b *bench, m *shifting)
	}{
		{"unmounted", func(_ *bench, m *shifting) { m.unmounted = true }},
		{"its mount point moved", func(b *bench, _ *shifting) {
			if err := os.Rename(b.dirs["t1"], b.dirs["t1"]+".moved"); err != nil {
				t.Fatal(err)
			}
		}},
		{"its root moved", func(b *bench, _ *shifting) {
			if err := os.Rename(b.media("t1"), b.media("t1")+".moved"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b := newBench(t, "t1")
			b.write("t1", "a", "x")
			b.write("t1", "s/b", "y")
			b.scan(Options{})
			if err := os.Remove(filepath.Join(b.media("t1"), "a")); err != nil {
				t.Fatal(err)
			}
			m := &shifting{bench: b}
			m.after = func() { tc.after(b, m) }
			verdicts, err := disks.CheckAll(t.Context(), b, b.config.Disks)
			if err != nil {
				t.Fatal(err)
			}
			r, err := LookAtDisk(t.Context(), b.db, b.config, m, &b.config.Disks[0], verdicts[0], b.now, &Look{Write: true})
			if err != nil {
				t.Fatal(err)
			}
			if r.Walked || r.Verdict.State != disks.Missing || !strings.Contains(r.Verdict.Message, ErrInterrupted.Error()) {
				t.Errorf("verdict: %+v", r)
			}
			if len(r.Absent) != 0 || b.count("SELECT count(*) FROM copies WHERE gone_ns IS NULL") != 2 {
				t.Errorf("concluded from a walk of another folder: %+v", r)
			}
			if got := b.count("SELECT count(*) FROM disks WHERE state = 'missing'"); got != 1 {
				t.Error("the disk is not recorded as missing")
			}
		})
	}
}

// A walk whose root cannot say what it is says nothing of the disk.
func TestAWalkWhoseRootCannotSayWhatItIs(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	b.wrap = twistInfo(func(name string) error {
		if name == "" {
			return syscall.EIO
		}
		return nil
	})
	r := b.scan(Options{}).Disks[0]
	if r.Walked || r.Verdict.State != disks.Missing || b.count("SELECT count(*) FROM copies") != 1 {
		t.Errorf("%+v", r)
	}
}
