package jobs

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// TestWhatAnNTFSDiskHoldsRefusesAName: a file whose only other disk is NTFS gets no copy there, and the state
// name-collision, when that disk holds another name with the same key (specs.md §15.7): a file, a folder at any depth,
// an entry that is no managed file, the name a copy still sits at after a rename it has not followed, the name it will
// take, or the folder a copy not yet made will need. The plan asks the catalog of the names a disk holds, and holds
// itself what is not there yet; both must refuse what a table of every name of the disk refused.
func TestWhatAnNTFSDiskHoldsRefusesAName(t *testing.T) {
	onNTFS := func(t *testing.T) *bench {
		t.Helper()
		b := newBench(t, "t1", "t2")
		b.config.Disks[1].FS = names.NTFS
		return b
	}
	for _, tc := range []struct {
		name  string
		setup func(b *bench)
		file  string
		want  string
	}{
		{"a file in another case", func(b *bench) {
			b.write("t2", "Film.mkv", "x")
			b.write("t1", "film.mkv", "y")
		}, "film.mkv", "name-collision"},
		{"a folder in another case", func(b *bench) {
			b.write("t2", "Films/a", "x")
			b.write("t1", "films/b", "y")
		}, "films/b", "name-collision"},
		{"a folder below a folder", func(b *bench) {
			b.write("t2", "A/Films/a", "x")
			b.write("t1", "A/films/b", "y")
		}, "A/films/b", "name-collision"},
		{"an empty folder", func(b *bench) {
			if err := os.Mkdir(filepath.Join(b.dirs["t2"], "media", "FILM"), 0o755); err != nil {
				b.t.Fatal(err)
			}
			b.write("t1", "film", "y")
		}, "film", "name-collision"},
		{"a symbolic link", func(b *bench) {
			if err := os.Symlink("nowhere", filepath.Join(b.dirs["t2"], "media", "Link")); err != nil {
				b.t.Fatal(err)
			}
			b.write("t1", "link", "y")
		}, "link", "name-collision"},
		{"a file above which a folder is wanted", func(b *bench) {
			b.write("t2", "Show", "x")
			b.write("t1", "show/ep1", "y")
		}, "show/ep1", "name-collision"},
		{"the same folder, to the byte", func(b *bench) {
			b.write("t2", "Films/a", "x")
			b.write("t1", "Films/b", "y")
		}, "Films/b", "pending"},
		{"a name on another disk", func(b *bench) {
			b.write("t1", "Film.mkv", "x")
			b.write("t1", "other", "y")
		}, "other", "pending"},
		{"the name a copy still sits at", func(b *bench) {
			b.write("t1", "Old", "x")
			b.write("t2", "Old", "x")
			b.scan(true)
			b.gone[b.dirs["t2"]] = true
			b.rename("t1", "Old", "New")
			b.settle()
			b.gone[b.dirs["t2"]] = false
			b.write("t1", "old", "y")
		}, "old", "name-collision"},
		{"the name a copy is to take", func(b *bench) {
			b.write("t1", "Old", "x")
			b.write("t2", "Old", "x")
			b.scan(true)
			b.gone[b.dirs["t2"]] = true
			b.rename("t1", "Old", "New")
			b.settle()
			b.gone[b.dirs["t2"]] = false
			b.write("t1", "NEW", "y")
		}, "NEW", "name-collision"},
		{"the folder of a copy not made yet", func(b *bench) {
			b.write("t1", "Show/ep1", "x")
			b.scan(false)
			if items, _ := b.replicate("", ReplicateOptions{}); len(items) != 1 || b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'pending'") != 1 {
				b.t.Fatalf("%+v", items)
			}
			b.write("t1", "show/ep2", "y")
		}, "show/ep2", "name-collision"},
		{"a copy not made yet", func(b *bench) {
			b.write("t1", "Show.mkv", "x")
			b.scan(false)
			if items, _ := b.replicate("", ReplicateOptions{}); len(items) != 1 || b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'pending'") != 1 {
				b.t.Fatalf("%+v", items)
			}
			b.write("t1", "show.mkv", "y")
		}, "show.mkv", "name-collision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := onNTFS(t)
			tc.setup(b)
			b.scan(false)
			if got := b.text("SELECT state FROM files WHERE lpath = ?", []byte(tc.file)); got != tc.want {
				t.Errorf("%s is %s, not %s", tc.file, got, tc.want)
			}
		})
	}
}

// TestPlacingAFileHoldsNoNameOfItsDisk: a plan that has one file to place on an NTFS disk asks the catalog whether
// that name is taken there, and holds no name of the disk for it. It gathered every name the disk holds, by its key,
// as soon as one file looked there: 20 to 40 MB a disk of 200,000 names, in a daemon that is to stay under 200 MB
// (specs.md §13.4, §26). What the plan allocates more for that one file must not grow with the disk.
func TestPlacingAFileHoldsNoNameOfItsDisk(t *testing.T) {
	const n = 2000
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	for i := range n {
		path := fmt.Sprintf("set/%03d/%d.mkv", i%100, i)
		b.write("t1", path, "x")
		b.write("t2", path, "x")
	}
	b.scan(false)
	// The least of three plans: what a plan allocates moves a little from one to the next.
	settle := func() uint64 {
		t.Helper()
		least := uint64(math.MaxUint64)
		for range 3 {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err := watch.Settle(t.Context(), b.db, b.config, b.now)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			least = min(least, after.TotalAlloc-before.TotalAlloc)
		}
		return least
	}
	nothing := settle()
	b.write("t1", "set/000/new.mkv", "y")
	b.scan(false)
	if got := b.text("SELECT state FROM files WHERE lpath = ?", []byte("set/000/new.mkv")); got != "pending" {
		t.Fatalf("the new file is %s", got)
	}
	if one := settle(); one > nothing+n*40 {
		t.Errorf("a plan with one file to place allocated %d bytes more than a plan with none, on a disk of %d names: it holds them", one-nothing, n)
	} else {
		t.Logf("%d bytes for a plan with nothing to place, %d more with one file", nothing, int64(one)-int64(nothing))
	}
}
