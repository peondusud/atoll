package watch

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/solver"
)

// reconfigure parses the bench's configuration again, as newBenchWith writes it, with top keys before every table, keys
// of [view], keys of each disk's table by its id, and more after the disks.
func (b *bench) reconfigure(top, view string, perDisk map[string]string, more string) {
	b.t.Helper()
	dir := filepath.Dir(b.config.Disks[0].Mount)
	var text strings.Builder
	fmt.Fprintf(&text, "pool_id = \"p\"\ncatalog = %q\n%s[view]\nmount = %q\n%s", filepath.Join(dir, "catalog.db"), top, filepath.Join(dir, "pool"), view)
	for i := range b.config.Disks {
		d := &b.config.Disks[i]
		fmt.Fprintf(&text, "[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = \"ext4\"\n%s", d.ID, d.UUID, d.Mount, perDisk[d.ID])
	}
	text.WriteString(more)
	c, err := config.Parse([]byte(text.String()))
	if err != nil {
		b.t.Fatal(err)
	}
	b.config = c
}

// spaced is a bench whose disks have the capacity and the free space it says, by id.
type spaced struct {
	*bench
	spaces map[string][2]int64
}

func (m *spaced) Space(mount string) (capacity, free int64, err error) {
	for id, dir := range m.dirs {
		if dir == mount {
			if s, ok := m.spaces[id]; ok {
				return s[0], s[1], nil
			}
		}
	}
	return m.bench.Space(mount)
}

// scanSpaced records the disks with those spaces.
func (b *bench) scanSpaced(spaces map[string][2]int64) {
	b.t.Helper()
	if _, err := Scan(b.t.Context(), b.db, b.config, &spaced{bench: b, spaces: spaces}, Options{}); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) capacity() []Level {
	b.t.Helper()
	levels, err := Capacity(b.t.Context(), b.db, b.config, b.now)
	if err != nil || len(levels) != MaxCopies {
		b.t.Fatalf("%+v, %v", levels, err)
	}
	return levels
}

func formulaOf(t *testing.T, k int, sizes ...int64) int64 {
	t.Helper()
	u, err := solver.Capacity(sizes, k)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// What the pool holds with k copies, from the catalog alone (specs.md §13.1, §13.2, §13.3): the formula over the disks
// the solver can write to, each less min_free; with spread_mode strict, over failure domains, as in the example of
// §13.2, 4 + 6 and 8 + 12; two disks on one physical disk count as one; a disk that is missing counts for nothing, and
// a pool with none to write to holds nothing.
func TestWhatThePoolHolds(t *testing.T) {
	const size = 10
	spaces := map[string][2]int64{"t1": {400, 200}, "t2": {600, 300}, "t3": {800, 400}, "t4": {1200, 600}}
	newPool := func(strict bool) *bench {
		b := newBenchWith(t, "", "t1", "t2", "t3", "t4")
		mode := ""
		if strict {
			mode = "spread_mode = \"strict\"\n"
		}
		b.reconfigure("", "", map[string]string{"t1": "domain = \"a\"\n", "t2": "domain = \"a\"\n", "t3": "domain = \"b\"\n", "t4": "domain = \"b\"\n"},
			"[placement]\nmin_free = [\"0%\"]\n"+mode)
		b.write("t1", "a", strings.Repeat("x", size))
		b.scanSpaced(spaces)
		return b
	}
	theoretical := func(levels []Level) []int64 {
		var out []int64
		for _, l := range levels {
			out = append(out, l.Theoretical)
		}
		return out
	}

	prefer := newPool(false).capacity()
	if got := theoretical(prefer); !slices.Equal(got, []int64{3000, 1500, 900}) {
		t.Errorf("prefer: %v", got)
	}
	for k, level := range prefer {
		if level.Copies != k+1 || level.Short != 0 || level.Stored != size {
			t.Errorf("%d copies: %+v", k+1, level)
		}
	}
	// One copy: the file is where it is, and every byte free is room.
	if prefer[0].Reachable != size+200+300+400+600 {
		t.Errorf("reachable with one copy: %d", prefer[0].Reachable)
	}
	// Two copies, the smallest domain; three, U(3), below it.
	if got := theoretical(newPool(true).capacity()); !slices.Equal(got, []int64{3000, 1000, 900}) {
		t.Errorf("strict: %v", got)
	}

	b := newPool(false)
	if _, err := b.db.ExecContext(t.Context(), "UPDATE disks SET device = 'sda' WHERE id IN ('t3', 't4')"); err != nil {
		t.Fatal(err)
	}
	if got := b.capacity(); got[1].Theoretical != formulaOf(t, 2, 400, 600, 2000) {
		t.Errorf("t3 and t4 on one physical disk: %+v", got[1])
	}
	if _, err := b.db.ExecContext(t.Context(), "UPDATE disks SET device = NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(t.Context(), "UPDATE disks SET state = 'missing' WHERE id = 't4'"); err != nil {
		t.Fatal(err)
	}
	if got := b.capacity(); got[1].Theoretical != formulaOf(t, 2, 400, 600, 800) {
		t.Errorf("t4 missing: %+v", got[1])
	}
	if _, err := b.db.ExecContext(t.Context(), "UPDATE disks SET state = 'missing'"); err != nil {
		t.Fatal(err)
	}
	for _, level := range b.capacity() {
		if level.Theoretical != 0 || level.Reachable != level.Stored {
			t.Errorf("no disk to write to, %d copies: %+v", level.Copies, level)
		}
	}
}

// A copy waiting in the queue is not on its disk yet: its size comes off that disk's free space when the plan keeps it,
// and not when the plan cancels it, as it does with one copy wanted (specs.md §13.4).
func TestAPendingCopyTakesItsRoom(t *testing.T) {
	const size = 10
	b := newBenchWith(t, "[placement]\nmin_free = [\"0%\"]\n", "t1", "t2")
	b.write("t1", "a", strings.Repeat("x", size))
	b.scanSpaced(map[string][2]int64{"t1": {1000, 500}, "t2": {1000, 700}})
	var id int64
	if err := b.db.QueryRowContext(t.Context(), "SELECT id FROM files WHERE lpath = ?", []byte("a")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := catalog.QueueReplicate(t.Context(), b.db, b.now, []catalog.Queue{{FileID: id, Src: "t1", Dst: "t2"}}); err != nil {
		t.Fatal(err)
	}
	levels := b.capacity()
	if levels[0].Reachable != size+500+700 {
		t.Errorf("one copy, the pending one canceled: %+v", levels[0])
	}
	if want := size + formulaOf(t, 2, 500, 700-size); levels[1].Reachable != want {
		t.Errorf("two copies, the pending one kept: %+v, want %d reachable", levels[1], want)
	}
}

// The disks a new file's replicas go to, as Atoll's own view makes it at a path, its size not known yet (specs.md
// §12.4, §21.2): none for a file a rule ignores or wants no replica of; never a disk the view may not write to; and,
// with colocate, the disk its folder's files are on.
func TestWhereANewFilesReplicasGo(t *testing.T) {
	b := newBenchWith(t, "", "t1", "t2", "t3")
	b.write("t1", "show/e1.mkv", "one")
	b.write("t3", "show/e1.mkv", "one")
	b.scan(Options{})
	free := func(string) (int64, bool) { return 0, false }
	all := func(string) bool { return true }
	targets := func(path string, writable func(string) bool) []string {
		t.Helper()
		got, err := ParallelTargets(t.Context(), b.db, b.config, b.now, path, "t1", free, writable)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := targets("show/e2.mkv", all); !slices.Equal(got, []string{"t2"}) {
		t.Errorf("the first disk with the most room: %q", got)
	}
	if got := targets("show/e2.mkv", func(d string) bool { return d != "t2" }); !slices.Equal(got, []string{"t3"}) {
		t.Errorf("t2 not writable: %q", got)
	}
	if got := targets("show/e2.mkv", func(d string) bool { return d == "t1" }); len(got) != 0 {
		t.Errorf("no disk writable: %q", got)
	}
	if got, err := ParallelTargets(t.Context(), b.db, b.config, b.now, "show/e2.mkv", "t1", func(d string) (int64, bool) { return 0, d == "t2" }, all); err != nil || !slices.Equal(got, []string{"t3"}) {
		t.Errorf("t2 full, as its filesystem says now: %q, %v", got, err)
	}

	b.reconfigure("", "", nil, "[placement]\ncolocate = \"folder\"\n[[rule]]\nlabel = \"skip\"\npath = \"tmp/**\"\naction = \"ignore\"\n[[rule]]\nlabel = \"solo\"\npath = \"solo/**\"\nreplicas = 0\n")
	if got := targets("show/e2.mkv", all); !slices.Equal(got, []string{"t3"}) {
		t.Errorf("colocated with its folder's files: %q", got)
	}
	for _, path := range []string{"tmp/x", "solo/x"} {
		if got := targets(path, all); got != nil {
			t.Errorf("%s: %q", path, got)
		}
	}
}

// cacheBench is a pool with Atoll's own view, a cache disk c and two data disks, whose replication.window holds the
// bench's time, now: a residency counts from the change time of a copy, which the disk gives.
func cacheBench(t *testing.T, cache string) *bench {
	t.Helper()
	b := newBenchWith(t, "", "c", "t1", "t2")
	b.now = time.Now().UnixNano()
	hour := time.Unix(0, b.now).UTC().Hour()
	b.reconfigure("timezone = \"UTC\"\n", "engine = \"atoll\"\n", map[string]string{"c": "role = \"cache\"\n"},
		fmt.Sprintf("[defaults]\nsettle = \"0s\"\n[replication]\nwindow = \"%02d:00-%02d:00\"\n%s", hour, (hour+2)%24, cache))
	return b
}

// A file whose residency on the SSD cache is over leaves it once the data disks hold it, as its hashes confirm: its
// copy on the cache is a transfer, planned from the catalog as it stands; one whose copies are not known to be in step,
// or that the data disks do not hold yet, waits; and with no cache disk, no file is resident (specs.md §19).
func TestAFileLeavesTheCacheOnceTheDataDisksHoldIt(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n")
	for _, d := range []string{"c", "t1", "t2"} {
		b.write(d, "show/e1.mkv", "episode")
	}
	b.write("c", "show/e2.mkv", "other")
	b.now = time.Now().UnixNano()
	b.scan(Options{})
	transfers, err := CacheTransfers(t.Context(), b.db, b.config, b.now)
	if err != nil || len(transfers) != 0 {
		t.Errorf("copies not known to be in step: %+v, %v", transfers, err)
	}
	b.scan(Options{Full: true})
	transfers, err = CacheTransfers(t.Context(), b.db, b.config, b.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 1 || transfers[0].Path != "show/e1.mkv" || transfers[0].Src != "c" || transfers[0].FileID == 0 {
		t.Errorf("transfers %+v", transfers)
	}
	residency, err := Residencies(t.Context(), b.db, b.config, mustDisks(t, b), b.now, nil)
	if err != nil || len(residency) != 2 {
		t.Errorf("residencies %v, %v", residency, err)
	}

	// The whole pool is what its data disks hold: every file leaves the cache in time (specs.md §13.3).
	levels := b.capacity()
	if levels[0].Stored != int64(len("episode")+len("other")) {
		t.Errorf("one copy: %+v", levels[0])
	}

	b.reconfigure("timezone = \"UTC\"\n", "engine = \"atoll\"\n", nil, "[cache]\nenabled = true\n")
	if transfers, err := CacheTransfers(t.Context(), b.db, b.config, b.now); err != nil || len(transfers) != 0 {
		t.Errorf("with no cache disk: %+v, %v", transfers, err)
	}
}

func mustDisks(t *testing.T, b *bench) []catalog.Disk {
	t.Helper()
	list, err := catalog.Disks(t.Context(), b.db)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// A file a rule now ignores keeps the pool's residency on the cache, as it keeps its placement (specs.md §15.10, §19).
func TestAnIgnoredFileKeepsThePoolsResidency(t *testing.T) {
	c, err := config.Parse([]byte("pool_id = \"p\"\n[view]\nengine = \"atoll\"\nmount = \"/srv/pool\"\n[placement]\n[cache]\nmin_age = \"3d\"\n" +
		"[[rule]]\nlabel = \"keep\"\npath = \"keep/**\"\naction = \"ignore\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := cacheFile(c, []catalog.CacheCopy{{FileID: 1, Path: "keep/a", Size: 1, Disk: "c1", State: "ok", FirstSeenNS: 5}}, func(string) bool { return true })
	if !f.Cache || f.Held || f.MinAgeNS != (72*time.Hour).Nanoseconds() || !f.Released {
		t.Errorf("%+v", f)
	}
}
