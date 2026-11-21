package watch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/solver"
)

// The trees of these tests go past PATH_MAX, which the kernel takes from no path given whole: they are made one name
// at a time, through handles, as Atoll reads them.

// deepFolder opens the folder at rel below base, making each folder on the way.
func deepFolder(t *testing.T, base, rel string) int {
	t.Helper()
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name := range strings.SplitSeq(rel, "/") {
		if name == "" {
			continue
		}
		if err := unix.Mkdirat(fd, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	return fd
}

// deepWrite writes a file at rel below base, and the folders above it.
func deepWrite(t *testing.T, base, rel, text string) {
	t.Helper()
	at := strings.LastIndexByte(rel, '/')
	dir := deepFolder(t, base, rel[:max(at, 0)])
	defer func() { _ = unix.Close(dir) }()
	fd, err := unix.Openat(dir, rel[at+1:], unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if _, err := unix.Write(fd, []byte(text)); err != nil {
		t.Fatal(err)
	}
}

// deepRename renames a name below base to another, making the folders above the new one.
func deepRename(t *testing.T, base, from, to string) {
	t.Helper()
	parent := func(rel string) (string, string) {
		at := strings.LastIndexByte(rel, '/')
		return rel[:max(at, 0)], rel[at+1:]
	}
	fromDir, fromName := parent(from)
	toDir, toName := parent(to)
	a, b := deepFolder(t, base, fromDir), deepFolder(t, base, toDir)
	defer func() { _ = unix.Close(a); _ = unix.Close(b) }()
	if err := unix.Renameat(a, fromName, b, toName); err != nil {
		t.Fatal(err)
	}
}

// chain gives a relative path of exactly n bytes, n > 0, made of folders of at most 200 bytes named with c.
func chain(c byte, n int) string {
	var parts []string
	for n > 0 {
		k := min(n, 200)
		if n-k == 1 {
			// A slash alone cannot end it.
			k--
		}
		parts = append(parts, strings.Repeat(string(c), k))
		n -= k
		if n > 0 {
			n--
		}
	}
	return strings.Join(parts, "/")
}

func (b *bench) media(disk string) string {
	return filepath.Join(b.dirs[disk], "media")
}

func (b *bench) limit(disk string) int {
	for i := range b.config.Disks {
		if d := &b.config.Disks[i]; d.ID == disk {
			return LimitsOf(b.config, d).MaxPath
		}
	}
	b.t.Fatalf("no disk %s", disk)
	return 0
}

// TestThePathLimitOfAMergerfsBranch: a walk records a name, and goes below it, while <mount>/<root>/<logical path> is
// at most 4095 bytes, what a mergerfs branch presents; past it, the name is reported and nothing below it is read
// (specs.md §15.8). walk.long_paths = "walk" walks them all.
func TestThePathLimitOfAMergerfsBranch(t *testing.T) {
	for _, all := range []bool{false, true} {
		extra := ""
		if all {
			extra = "[walk]\nlong_paths = \"walk\"\n"
		}
		b := newBenchWith(t, extra, "t1")
		d := &b.config.Disks[0]
		limit := names.MaxBranchPathBytes - len(d.Mount) - 1 - len(d.Root) - 1
		if fits, err := names.FitsBranch(d.Mount, d.Root, chain('x', limit)); err != nil || !fits {
			t.Fatalf("a path of %d bytes does not fit its branch: %v", limit, err)
		}
		if fits, _ := names.FitsBranch(d.Mount, d.Root, chain('x', limit+1)); fits {
			t.Fatalf("a path of %d bytes fits its branch", limit+1)
		}
		media := b.media("t1")
		folder := chain('d', limit-20)
		under, at, over := folder+"/"+strings.Repeat("f", 18), folder+"/"+strings.Repeat("g", 19), folder+"/"+strings.Repeat("h", 20)
		cross := folder + "/" + strings.Repeat("e", 17)
		for _, rel := range []string{under, at, over, cross + "/sub/deeper/file", "near/a.mkv"} {
			deepWrite(t, media, rel, "x")
		}
		if len(under) != limit-1 || len(at) != limit || len(over) != limit+1 || len(cross) != limit-2 {
			t.Fatalf("lengths %d, %d, %d, %d for a limit of %d", len(under), len(at), len(over), len(cross), limit)
		}
		r := b.scan(Options{}).Disks[0]
		if !r.Walked || r.Verdict.State != disks.Online {
			t.Fatalf("%+v", r)
		}
		want, files := []string{cross + "/sub", over}, 3
		if all {
			want, files = nil, 5
		}
		if !slices.Equal(r.TooLong, want) {
			t.Errorf("walk %v: past the limit: %q, want %q", all, r.TooLong, want)
		}
		if r.New != files || b.count("SELECT count(*) FROM files") != files {
			t.Errorf("walk %v: %d new, %d files, want %d", all, r.New, b.count("SELECT count(*) FROM files"), files)
		}
		for _, rel := range []string{under, at} {
			if b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte(rel)) != 1 {
				t.Errorf("walk %v: %d bytes not recorded", all, len(rel))
			}
		}
		below := b.count("SELECT count(*) FROM entries WHERE lpath >= ? AND lpath < ?", []byte(cross+"/sub"), []byte(cross+"/sub\xff"))
		if (below != 0) != all {
			t.Errorf("walk %v: %d entries at or below the name past the limit", all, below)
		}
		if b.count("SELECT count(*) FROM entries WHERE lpath = ?", []byte(cross)) != 1 {
			t.Errorf("walk %v: the folder whose child is past the limit is not recorded", all)
		}
	}
}

// look walks the disk t1 as a pass does, writing what it found, and gives what it says of each copy.
func (b *bench) look() *Report {
	b.t.Helper()
	verdicts, err := disks.CheckAll(b.t.Context(), b, b.config.Disks)
	if err != nil {
		b.t.Fatal(err)
	}
	r, err := LookAtDisk(b.t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, &Look{Write: true})
	if err != nil {
		b.t.Fatal(err)
	}
	return r
}

// TestPastTheLimitIsNoDeletion: a copy that went past the limit of its branch, by a folder above it renamed, by its
// folder moved there, or by itself, is not read as deleted: the walk did not look there (invariant 4, specs.md §15.8).
// While a name is past the limit, no copy of the disk is: any could have gone there.
func TestPastTheLimitIsNoDeletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(b *bench, limit int)
		// absent says that the copy reads as gone: nothing is past the limit.
		absent bool
	}{
		{"a folder above it renamed", func(b *bench, limit int) { deepRename(b.t, b.media("t1"), "a", chain('d', limit-3)+"/a") }, false},
		{"its folder moved past the limit", func(b *bench, limit int) { deepRename(b.t, b.media("t1"), "a", chain('d', limit-1)+"/a") }, false},
		{"itself moved past the limit", func(b *bench, limit int) { deepRename(b.t, b.media("t1"), "a/x", chain('d', limit-1)+"/x") }, false},
		{"deleted, with a name past the limit elsewhere", func(b *bench, limit int) {
			deepWrite(b.t, b.media("t1"), chain('d', limit+1), "y")
			if err := os.Remove(filepath.Join(b.media("t1"), "a", "x")); err != nil {
				b.t.Fatal(err)
			}
		}, false},
		{"deleted, with nothing past the limit", func(b *bench, _ int) {
			if err := os.Remove(filepath.Join(b.media("t1"), "a", "x")); err != nil {
				b.t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1")
			b.write("t1", "a/x", "x")
			b.write("t1", "b", "y")
			if first := b.scan(Options{}); first.Disks[0].New != 2 {
				t.Fatalf("%+v", first.Disks[0])
			}
			tc.move(b, b.limit("t1"))
			r := b.look()
			if !r.Walked {
				t.Fatalf("%+v", r)
			}
			if (len(r.TooLong) == 0) != tc.absent {
				t.Errorf("past the limit: %q", r.TooLong)
			}
			var verdict solver.CopyVerdict
			for _, s := range r.Seen {
				verdict = s.Verdict
			}
			// Below a folder renamed to where it is past the limit, it is unknown, as below a name in unreadable; a
			// copy found nowhere is uncertain, which keeps its file whatever its other copies say.
			switch {
			case tc.absent && (verdict != solver.VerdictGone || !slices.Equal(r.Absent, []string{"a/x"})):
				t.Errorf("a deletion with nothing past the limit: %s, absent %q", verdict, r.Absent)
			case !tc.absent && (verdict != solver.VerdictUnknown && verdict != solver.VerdictUncertain || len(r.Seen) != 1):
				t.Errorf("read as deleted: %+v, absent %q", r.Seen, r.Absent)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE gone_kind = 'gone'"); got != 0 && !tc.absent {
				t.Errorf("%d copies gone", got)
			}
			if got := b.count("SELECT count(*) FROM files"); got != 2 {
				t.Errorf("%d files", got)
			}
		})
	}
}

// TestALookAtNamesStopsAtTheLimit: a look limited to the names the watcher gave reads no further than a walk, and
// reports a name past the limit that is there (specs.md §15.8, §15.13).
func TestALookAtNamesStopsAtTheLimit(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	limit := b.limit("t1")
	folder := chain('d', limit-1)
	deepWrite(t, b.media("t1"), folder+"/x/y", "z")
	verdicts, err := disks.CheckAll(t.Context(), b, b.config.Disks)
	if err != nil {
		t.Fatal(err)
	}
	r, err := LookAtDisk(t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now,
		&Look{Names: []Named{{Path: folder + "/x/y"}, {Path: folder + "/gone"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Walked || !slices.Equal(r.TooLong, []string{folder + "/x"}) {
		t.Errorf("past the limit: %q", r.TooLong)
	}
}

// openCount counts the folders a walk holds open, through a wrapper of its root.
type openCount struct {
	mu         sync.Mutex
	open, most int
}

func (c *openCount) add(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open += n
	c.most = max(c.most, c.open)
}

type counted struct {
	fsx.Dir
	c *openCount
}

func (d *counted) OpenDir(name string) (fsx.Dir, error) {
	below, err := d.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	d.c.add(1)
	return &counted{Dir: below, c: d.c}, nil
}

func (d *counted) Close() error {
	d.c.add(-1)
	return d.Dir.Close() //nolint:wrapcheck // a test double
}

// comb writes a chain of folders below base, each of width bytes, with a file in each and a folder beside the next
// that holds a file: climbing back, a walk comes back to each of them.
func comb(t *testing.T, base string, depth, width int) (files int) {
	t.Helper()
	path := ""
	for i := range depth {
		path += strings.Repeat(string(rune('a'+i%26)), width) + "/"
		deepWrite(t, base, path+"f", "x")
		deepWrite(t, base, path+"s/g", "y")
		files += 2
	}
	return files
}

// TestAWalkHoldsAtMostMaxOpenFolders: with walk.long_paths = "walk", a walk goes below every name, and holds no more
// than walk.max_open folders open at once, its root included, however deep the tree (specs.md §15.8). By default, it
// holds a folder for each level it is below, as many as fit below the limit of a branch.
func TestAWalkHoldsAtMostMaxOpenFolders(t *testing.T) {
	for _, tc := range []struct {
		extra string
		most  int
	}{
		{"[walk]\nlong_paths = \"walk\"\nmax_open = 4\n", 4},
		{"[walk]\nlong_paths = \"walk\"\nmax_open = 3\n", 3},
		{"", 0},
	} {
		b := newBenchWith(t, tc.extra, "t1")
		files := comb(t, b.media("t1"), 40, 120)
		count := &openCount{}
		b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
			count.add(1)
			return &counted{Dir: d, c: count}
		}
		r := b.scan(Options{}).Disks[0]
		if !r.Walked {
			t.Fatalf("%q: %+v", tc.extra, r)
		}
		if count.open != 0 {
			t.Errorf("%q: %d folders left open", tc.extra, count.open)
		}
		if tc.most == 0 {
			// 40 levels of 121 bytes pass the limit after some 33: the walk holds each level it is below, then stops.
			if len(r.TooLong) == 0 || r.New >= files || count.most < 20 {
				t.Errorf("by default: %d past the limit, %d new of %d, %d open at most", len(r.TooLong), r.New, files, count.most)
			}
			continue
		}
		if count.most > tc.most {
			t.Errorf("%q: %d folders open at once", tc.extra, count.most)
		}
		if r.New != files || len(r.TooLong) != 0 || len(r.Unreadable) != 0 {
			t.Errorf("%q: %d new of %d, past the limit %q, unreadable %q", tc.extra, r.New, files, r.TooLong, r.Unreadable)
		}
	}
}

// TestAFolderChangedWhileAWalkIsBelowIt: a walk that let go of a folder above it opens it again from one it holds when
// it comes back to it, and it must be the same folder: renamed away, or replaced by another, it is left aside as a name
// the walk could not reach, with what was found below it, and nothing below it reads as deleted (specs.md §15.8,
// §15.10).
func TestAFolderChangedWhileAWalkIsBelowIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(media string) error
	}{
		{"renamed away", func(media string) error {
			return os.Rename(filepath.Join(media, "a", "A"), filepath.Join(media, "a", "A2"))
		}},
		{"replaced", func(media string) error {
			if err := os.Rename(filepath.Join(media, "a", "A"), filepath.Join(media, "a", "A2")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(media, "a", "A"), 0o755)
		}},
		{"removed", func(media string) error { return os.RemoveAll(filepath.Join(media, "a", "A")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBenchWith(t, "[walk]\nlong_paths = \"walk\"\nmax_open = 3\n", "t1")
			media := b.media("t1")
			for _, rel := range []string{"top.mkv", "a/z.mkv", "a/A/X/deep1/deep2/deep3/deep4/f", "a/A/X/x", "a/A/Y/deep1/deep2/deep3/deep4/g", "a/A/Y/y"} {
				deepWrite(t, media, rel, "v")
			}
			if first := b.scan(Options{}).Disks[0]; first.New != 6 {
				t.Fatalf("the first scan: %+v", first)
			}
			b.write("t1", "new.mkv", "w")
			var changed atomic.Bool
			b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
				return fsx.Fault(d, func(call, name string) error {
					if call == "names" && name == "deep4" && changed.CompareAndSwap(false, true) {
						if err := tc.change(media); err != nil {
							t.Error(err)
						}
					}
					return nil
				})
			}
			r := b.look()
			if !changed.Load() {
				t.Fatal("the walk never went that deep")
			}
			if !r.Walked || r.Verdict.State != disks.Online {
				t.Fatalf("the disk is taken for one that fails: %+v", r)
			}
			if !slices.Equal(r.Unreadable, []string{"a/A"}) {
				t.Errorf("unreadable: %q", r.Unreadable)
			}
			if len(r.Absent) != 0 || slices.ContainsFunc(r.Seen, func(s CopySeen) bool { return s.Verdict != solver.VerdictUnknown }) {
				t.Errorf("concluded of what is below: %+v, absent %q", r.Seen, r.Absent)
			}
			if got := b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("new.mkv")); got != 1 {
				t.Error("the rest of the disk was not read")
			}
			if got := b.count("SELECT count(*) FROM files"); got != 7 {
				t.Errorf("%d files", got)
			}
			if got := b.count("SELECT count(*) FROM entries WHERE lpath >= ? AND lpath < ?", []byte("a/A"), []byte("a/A\xff")); got != 11 {
				t.Errorf("%d entries at or below the folder left aside, want the 11 of before", got)
			}
		})
	}
}
