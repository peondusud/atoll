package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
)

// twisted wraps a folder, and each it opens: change alters what Stat says of a name, and info, when it gives an error,
// makes the Info of the folder opened by that name fail, "" for the top. What a disk can say that a test's own cannot.
type twisted struct {
	fsx.Dir
	name   string
	change func(name string, st *fsx.Stat)
	info   func(name string) error
}

func (d *twisted) Unwrap() fsx.Dir { return d.Dir }

func (d *twisted) Info() (fsx.Stat, error) {
	if d.info != nil {
		if err := d.info(d.name); err != nil {
			return fsx.Stat{}, err
		}
	}
	return d.Dir.Info() //nolint:wrapcheck // a test double
}

func (d *twisted) Stat(name string) (fsx.Stat, error) {
	st, err := d.Dir.Stat(name)
	if err == nil && d.change != nil {
		d.change(name, &st)
	}
	return st, err //nolint:wrapcheck // a test double
}

func (d *twisted) OpenDir(name string) (fsx.Dir, error) {
	below, err := d.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &twisted{Dir: below, name: name, change: d.change, info: d.info}, nil
}

func twistStat(change func(name string, st *fsx.Stat)) func(string, fsx.Dir) fsx.Dir {
	return func(_ string, d fsx.Dir) fsx.Dir { return &twisted{Dir: d, change: change} }
}

func twistInfo(info func(name string) error) func(string, fsx.Dir) fsx.Dir {
	return func(_ string, d fsx.Dir) fsx.Dir { return &twisted{Dir: d, info: info} }
}

// newNamedBench is a disk whose catalog knows d/e/f.mkv and d/g, with a symbolic link d/l and a fifo d/p beside them.
func newNamedBench(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1")
	b.write("t1", "d/e/f.mkv", "x")
	b.write("t1", "d/g", "y")
	if err := os.Symlink("g", filepath.Join(b.media("t1"), "d", "l")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(b.media("t1"), "d", "p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := b.scan(Options{}); out.Partial() || out.Disks[0].New != 2 {
		t.Fatalf("the first scan: %+v", out.Disks[0])
	}
	return b
}

// lookAt looks at names on t1, as a pass given them does, and writes what it found.
func (b *bench) lookAt(names ...Named) (*Report, error) {
	b.t.Helper()
	ctx := b.t.Context()
	verdicts, err := disks.CheckAll(ctx, b, b.config.Disks)
	if err != nil {
		b.t.Fatal(err)
	}
	return LookAtDisk(ctx, b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, &Look{Write: true, Names: names})
}

// untouched says that no copy of the disk was concluded gone, moved or replaced, and none forgotten.
func (b *bench) untouched(r *Report) {
	b.t.Helper()
	if len(r.Absent) != 0 || len(r.Moved) != 0 || len(r.Changed) != 0 || len(r.FolderMoves) != 0 {
		b.t.Errorf("concluded from a look that could not read: %+v", r)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'ok' AND gone_ns IS NULL"); got != 2 {
		b.t.Errorf("%d copies left as they were, want 2", got)
	}
}

var everyName = []Named{{Path: "d/e/f.mkv"}, {Path: "d/l"}, {Path: "d/p"}, {Path: "d/g"}}

// A look at the names the watcher gave, whose disk does not answer on the way to one, says nothing of the disk: the
// disk is missing, whatever call failed, and no copy is concluded gone (invariant 4, specs.md §15.13).
func TestALookAtNamesWhoseDiskDoesNotAnswer(t *testing.T) {
	for _, tc := range []struct {
		what string
		wrap func(string, fsx.Dir) fsx.Dir
	}{
		{"a folder on the way", failing("stat", "e", syscall.EIO)},
		{"opening a folder on the way", failing("opendir", "e", syscall.EIO)},
		{"a symbolic link", failing("readlink", "l", syscall.EIO)},
		{"the root's own identity", twistInfo(func(name string) error {
			if name == "" {
				return syscall.EIO
			}
			return nil
		})},
		{"a modification time no disk holds, of a file", twistStat(func(name string, st *fsx.Stat) {
			if name == "g" {
				st.Mtime.Nsec = 1e9
			}
		})},
		{"a change time no disk holds", twistStat(func(name string, st *fsx.Stat) {
			if name == "g" {
				st.Ctime.Nsec = -1
			}
		})},
		{"a modification time no disk holds, of a folder on the way", twistStat(func(name string, st *fsx.Stat) {
			if name == "d" {
				st.Mtime.Nsec = 1e9
			}
		})},
		{"a creation time no disk holds, of a folder on the way", twistStat(func(name string, st *fsx.Stat) {
			if name == "d" {
				st.HasBirth, st.Birth.Sec, st.Birth.Nsec = true, 1, 1e9
			}
		})},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b := newNamedBench(t)
			b.wrap = tc.wrap
			r, err := b.lookAt(everyName...)
			if err != nil {
				t.Fatal(err)
			}
			if r.Walked || r.Verdict.State != disks.Missing || r.Verdict.Reason != disks.IOError {
				t.Errorf("verdict: %+v", r)
			}
			b.untouched(r)
			if got := b.count("SELECT count(*) FROM disks WHERE id = 't1' AND state = 'missing'"); got != 1 {
				t.Error("the disk is not recorded as missing")
			}
		})
	}
}

// A look at names whose root cannot be opened says nothing of the disk either.
func TestALookAtNamesWithoutItsRoot(t *testing.T) {
	b := newNamedBench(t)
	if err := os.Rename(b.media("t1"), filepath.Join(b.dirs["t1"], "elsewhere")); err != nil {
		t.Fatal(err)
	}
	r, err := b.lookAt(everyName...)
	if err != nil {
		t.Fatal(err)
	}
	if r.Walked || r.Verdict.State != disks.Missing {
		t.Errorf("verdict: %+v", r)
	}
	b.untouched(r)
}

// A name the look cannot reach is reported as a walk reports it, and is no absence: a folder on the way that refuses
// the account, or a link that does; a name gone in the meantime says nothing of the disk (specs.md §15.10, §15.13).
func TestALookAtNamesItCannotReach(t *testing.T) {
	for _, tc := range []struct {
		call, name string
		err        error
		want       []string
	}{
		{"stat", "e", syscall.EACCES, []string{"d/e"}},
		{"opendir", "e", syscall.EACCES, []string{"d/e"}},
		{"readlink", "l", syscall.EACCES, []string{"d/l"}},
		{"readlink", "l", syscall.ENOENT, nil},
	} {
		t.Run(tc.call+"/"+tc.err.Error(), func(t *testing.T) {
			b := newNamedBench(t)
			b.wrap = failing(tc.call, tc.name, tc.err)
			r, err := b.lookAt(everyName...)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Walked || r.Verdict.State == disks.Missing {
				t.Errorf("verdict: %+v", r)
			}
			if !slices.Equal(r.Unreadable, tc.want) {
				t.Errorf("unreadable %q, want %q", r.Unreadable, tc.want)
			}
			var refused []string
			if errors.Is(tc.err, syscall.EACCES) {
				refused = tc.want
			}
			if !slices.Equal(r.Refused, refused) {
				t.Errorf("refused %q, want %q", r.Refused, refused)
			}
			b.untouched(r)
		})
	}
}

// What a look at names finds there is what a walk would: a symbolic link with its target, a fifo, which no rule makes
// a file of, and the folders on the way; a name given empty is the root, which only a walk of the whole disk looks at
// (specs.md §15.10, §15.13).
func TestWhatALookAtNamesFinds(t *testing.T) {
	b := newNamedBench(t)
	r, err := b.lookAt(append([]Named{{Path: ""}}, everyName...)...)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Walked || r.Symlinks != 1 || r.Ignored != 1 || r.Files != 2 || !slices.Equal(r.Looked, []string{"d/e/f.mkv", "d/g", "d/l", "d/p"}) {
		t.Errorf("%+v", r)
	}
	b.untouched(r)
	var target string
	if err := b.db.QueryRowContext(t.Context(), "SELECT CAST(target AS TEXT) FROM entries WHERE lpath = ? AND kind = 'symlink'", []byte("d/l")).Scan(&target); err != nil || target != "g" {
		t.Errorf("the link's entry: %q, %v", target, err)
	}
	if got := b.count("SELECT count(*) FROM entries WHERE lpath = ? AND kind = 'ignored'", []byte("d/p")); got != 1 {
		t.Error("the fifo has no entry")
	}
}

// A folder on another mount than the root's, on the way to a name or at it, is reported and not gone below: what is
// below it says nothing of this disk, and its copies are no absence (specs.md §15.10).
func TestALookAtNamesStopsAtAnotherMount(t *testing.T) {
	for _, names := range [][]Named{{{Path: "d/e/f.mkv"}}, {{Path: "d/e", Folder: true}}} {
		b := newNamedBench(t)
		b.wrap = func(_ string, d fsx.Dir) fsx.Dir { return &otherMount{Dir: d, t: t, name: "e"} }
		r, err := b.lookAt(names...)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Walked || !slices.Equal(r.OtherMounts, []string{"d/e"}) || r.Files != 0 {
			t.Errorf("%q: %+v", names[0].Path, r)
		}
		b.untouched(r)
	}
}

// A look that is canceled concludes nothing, and writes nothing.
func TestACanceledLookAtNames(t *testing.T) {
	b := newNamedBench(t)
	b.write("t1", "d/new", "n")
	verdicts, err := disks.CheckAll(t.Context(), b, b.config.Disks)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = LookAtDisk(ctx, b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, &Look{Write: true, Names: []Named{{Path: "d/new"}}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
	if got := b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("d/new")); got != 0 {
		t.Error("a canceled look wrote what it found")
	}
}

// A folder renamed on the disk, whose old name no event gave, is read as a rename: the look takes in the old name of
// the folder it found where no row is, from the folder's inode (specs.md §15.13).
func TestALookTakesInTheOldNameOfAFolder(t *testing.T) {
	b := newNamedBench(t)
	if err := os.Rename(filepath.Join(b.media("t1"), "d", "e"), filepath.Join(b.media("t1"), "d", "e2")); err != nil {
		t.Fatal(err)
	}
	r, err := b.lookAt(Named{Path: "d/e2", Folder: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Looked, "d/e") || !slices.Contains(r.Looked, "d/e2") {
		t.Errorf("looked at %q", r.Looked)
	}
	if !slices.Contains(r.FolderMoves, FolderMove{From: "d/e", To: "d/e2"}) {
		t.Errorf("folder moves %+v, seen %+v", r.FolderMoves, r.Seen)
	}
}
