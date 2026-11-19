package jobs

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// parallel is the bench on a machine that tells the kind of each disk, as the delivered binary's does, so that copies
// run at once there (specs.md §14, the maintainer, 2026-11-19). Every call a copy makes on a disk goes through spy,
// which keeps when the copy began and ended its calls on each disk, slows each write of a part so that the copies that
// may run at once do, and can make any call fail. Copies at once each run in a goroutine of their own, so what a span is
// of is a copy, exactly: its calls on a disk, from its goroutine.
type parallel struct {
	*bench
	mu sync.Mutex
	// kinds is the kind of each disk, by mount: "ssd", "hdd" or "unknown", which gives no answer.
	kinds map[string]string
	// faults makes calls fail, by mount; it may be called from several copies at once.
	faults map[string]func(call, name string) error
	slow   time.Duration
	seq    int64
	// main is the goroutine of the test, which runs the run itself: what it does on a disk is no copy's.
	main  uint64
	spans map[spanKey]*span
	// cancel stops the run under way, as SIGTERM would.
	cancel context.CancelFunc
	// stopAt is told of each point a copy reaches, before what the bench does there.
	stopAt func(point string)
	// over says that the run has returned, and late counts the calls made on a disk since: none may come.
	over bool
	late int
}

// spanKey names a span: the calls of one copy on one disk.
type spanKey struct {
	mount string
	copy  uint64
}

// span is what a copy did on a disk, from its first call to its last: whether it wrote a part, read a source, and which.
type span struct {
	first, last int64
	wrote, read bool
	file        string
}

func newParallel(t *testing.T, more string, kinds map[string]string, ids ...string) *parallel {
	t.Helper()
	return parallelOf(newBenchWith(t, more, ids...), kinds)
}

func parallelOf(b *bench, kinds map[string]string) *parallel {
	p := &parallel{bench: b, kinds: map[string]string{}, faults: map[string]func(string, string) error{}, spans: map[spanKey]*span{},
		slow: 20 * time.Millisecond, main: goroutine()}
	for id, dir := range b.dirs {
		p.kinds[dir] = cmp.Or(kinds[id], "hdd")
		b.fail[dir] = p.spy(dir)
	}
	return p
}

// goroutine gives the number of the goroutine that calls it, as its stack's first line says.
func goroutine() uint64 {
	var buf [64]byte
	fields := strings.Fields(string(buf[:runtime.Stack(buf[:], false)]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}

func (p *parallel) spy(mount string) func(call, name string) error {
	return func(call, name string) error {
		part := strings.HasSuffix(name, ".part")
		g := goroutine()
		p.mu.Lock()
		if p.over {
			p.late++
		}
		if g != p.main {
			p.seq++
			key := spanKey{mount: mount, copy: g}
			sp := p.spans[key]
			if sp == nil {
				sp = &span{first: p.seq}
				p.spans[key] = sp
			}
			sp.last = p.seq
			sp.wrote = sp.wrote || part
			if !part && (call == "openfile" || call == "read") {
				sp.read, sp.file = true, name
			}
		}
		fault := p.faults[mount]
		p.mu.Unlock()
		if call == "write" && part {
			time.Sleep(p.slow)
		}
		if fault != nil {
			return fault(call, name)
		}
		return nil
	}
}

func (p *parallel) fault(id string, f func(call, name string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults[p.dirs[id]] = f
}

// Checkpoint is the bench's, for copies at once.
func (p *parallel) Checkpoint(point string) {
	if p.stopAt != nil {
		p.stopAt(point)
	}
	p.mu.Lock()
	p.points = append(p.points, point)
	fire := p.at[point]
	delete(p.at, point)
	p.mu.Unlock()
	if fire != nil {
		fire()
	}
}

// The kinds: each disk is a whole disk of its own, named by its mount.
func (p *parallel) PhysicalDisks(_ names.Filesystem, entry *disks.MountEntry, _ string) ([]string, error) {
	return []string{entry.MountPoint}, nil
}

func (p *parallel) MountPointImmutable(string) (bool, error) { return false, nil }

func (p *parallel) Rotational(name string) (bool, error) {
	switch p.kinds[name] {
	case "ssd":
		return false, nil
	case "unknown":
		return false, errors.New("no answer")
	}
	return true, nil
}

func (p *parallel) BlockStat(string) (disks.BlockStat, error) {
	return disks.BlockStat{}, errors.New("none")
}
func (p *parallel) PowerMode(string) (disks.Power, error) {
	return disks.PowerUnknown, errors.New("none")
}

// atOnce gives the most copies at once among the spans keep takes.
func (p *parallel) atOnce(keep func(k spanKey, s *span) bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	type edge struct {
		at    int64
		delta int
	}
	var edges []edge
	for k, s := range p.spans {
		if keep(k, s) {
			edges = append(edges, edge{s.first, 1}, edge{s.last, -1})
		}
	}
	slices.SortFunc(edges, func(a, b edge) int { return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(b.delta, a.delta)) })
	now, most := 0, 0
	for _, e := range edges {
		now += e.delta
		most = max(most, now)
	}
	return most
}

// forget starts the spans of another run.
func (p *parallel) forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spans = map[spanKey]*span{}
}

// writes, reads and uses give the most copies at once that wrote to a disk, that read from it, and that did either.
func (p *parallel) writes(id string) int {
	return p.atOnce(func(k spanKey, s *span) bool { return k.mount == p.dirs[id] && s.wrote })
}

func (p *parallel) reads(id string) int {
	return p.atOnce(func(k spanKey, s *span) bool { return k.mount == p.dirs[id] && s.read })
}

func (p *parallel) uses(id string) int {
	return p.atOnce(func(k spanKey, _ *span) bool { return k.mount == p.dirs[id] })
}

// copiesOf gives the most copies of a file, by its name, that ran at once: each from its first call on any disk to its
// last.
func (p *parallel) copiesOf(name string) int {
	p.mu.Lock()
	whole := map[uint64]*span{}
	for k, s := range p.spans {
		w := whole[k.copy]
		if w == nil {
			w = &span{first: s.first, last: s.last}
			whole[k.copy] = w
		}
		w.first, w.last = min(w.first, s.first), max(w.last, s.last)
		w.file = cmp.Or(w.file, s.file)
	}
	p.mu.Unlock()
	most := 0
	for g, a := range whole {
		if a.file != name {
			continue
		}
		n := 0
		for h, b := range whole {
			if b.file == name && (g == h || a.first <= b.last && b.first <= a.last) {
				n++
			}
		}
		most = max(most, n)
	}
	return most
}

func (p *parallel) replicateAll() ([]ReplicateItem, error) {
	var items []ReplicateItem
	_, err := Replicate(p.t.Context(), p.db, p.config, p, ReplicateOptions{Now: true}, func(i *ReplicateItem) error {
		items = append(items, *i)
		return nil
	})
	return items, err
}

// fill writes n files below F/ on a disk, each of its own content and size, and scans them.
func (p *parallel) fill(id string, n int) []string {
	p.t.Helper()
	var paths []string
	for i := range n {
		path := fmt.Sprintf("F/f%02d", i)
		p.write(id, path, strings.Repeat(fmt.Sprintf("content %d ", i), 10))
		paths = append(paths, path)
	}
	p.scan(false)
	return paths
}

// copied checks that every file has its copy on a disk, as its source holds it, verified, and that nothing is left
// planned or in tmp/.
func (p *parallel) copied(from, to string, paths []string) {
	p.t.Helper()
	for _, path := range paths {
		want, err := os.ReadFile(filepath.Join(p.dirs[from], "media", path))
		if err != nil {
			p.t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(p.dirs[to], "media", path))
		if err != nil || !bytes.Equal(got, want) {
			p.t.Errorf("%s on %s: %q, %v", path, to, got, err)
		}
	}
	if got := p.count("SELECT count(*) FROM copies WHERE disk_id = ? AND state = 'ok' AND verified_ns IS NOT NULL", to); got != len(paths) {
		p.t.Errorf("%d copies verified on %s, want %d", got, to, len(paths))
	}
	p.clean()
}

// clean checks that nothing is left planned, queued or in tmp/.
func (p *parallel) clean() {
	p.t.Helper()
	if got := p.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		p.t.Errorf("%d operations left planned", got)
	}
	if got := p.count("SELECT count(*) FROM jobs"); got != 0 {
		p.t.Errorf("%d jobs left", got)
	}
	for id := range p.dirs {
		if parts := p.parts(id); len(parts) != 0 {
			p.t.Errorf("%s: tmp/ holds %q", id, parts)
		}
	}
}

func outcomes(items []ReplicateItem) map[string]int {
	out := map[string]int{}
	for _, i := range items {
		out[i.Outcome]++
	}
	return out
}

// Copies run at once as the kind of each disk allows: an HDD is never read or written by two at once, an SSD by its
// limit at most, and every copy is made as it would be alone (specs.md §14, the maintainer, 2026-11-19).
func TestCopiesAtOnceByKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		more     string
		kinds    map[string]string
		files    int
		min, max int // copies at once on the target, and on the source at most
	}{
		{"from one SSD to another, four at once", "", map[string]string{"t1": "ssd", "t2": "ssd"}, 8, 2, 4},
		{"from one HDD to another, one at a time", "", map[string]string{"t1": "hdd", "t2": "hdd"}, 4, 1, 1},
		{"from an SSD to an HDD, one at a time", "", map[string]string{"t1": "ssd", "t2": "hdd"}, 4, 1, 1},
		{"from an HDD to an SSD, one at a time", "", map[string]string{"t1": "hdd", "t2": "ssd"}, 4, 1, 1},
		{"a kind no one can tell is a spindle's", "", map[string]string{"t1": "ssd", "t2": "unknown"}, 4, 1, 1},
		{"max_parallel_per_ssd", "[replication]\nmax_parallel_per_ssd = 2\n", map[string]string{"t1": "ssd", "t2": "ssd"}, 6, 2, 2},
		{"max_parallel_per_disk for every disk, an SSD too", "[replication]\nmax_parallel_per_disk = 1\n", map[string]string{"t1": "ssd", "t2": "ssd"}, 4, 1, 1},
		{"max_parallel_per_disk for every disk, an HDD too", "[replication]\nmax_parallel_per_disk = 3\n", map[string]string{"t1": "hdd", "t2": "hdd"}, 6, 2, 3},
		{"max_parallel_per_hdd", "[replication]\nmax_parallel_per_hdd = 2\n", map[string]string{"t1": "hdd", "t2": "hdd"}, 6, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newParallel(t, tc.more, tc.kinds, "t1", "t2")
			paths := p.fill("t1", tc.files)
			items, err := p.replicateAll()
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomes(items); got[OutcomeReplicated] != tc.files || len(got) != 1 {
				t.Fatalf("%v", got)
			}
			for i := range items {
				if items[i].Path != paths[i] {
					t.Fatalf("the lines are not in the order of the queue: %+v", items)
				}
			}
			if w := p.writes("t2"); w < tc.min || w > tc.max {
				t.Errorf("%d copies at once on the target, want %d to %d", w, tc.min, tc.max)
			}
			if r := p.reads("t1"); r > tc.max {
				t.Errorf("%d copies at once read the source, want %d at most", r, tc.max)
			}
			p.copied("t1", "t2", paths)
		})
	}
}

// A file with two copies to make gets one at a time, the per-path lock every copy of a file shares (specs.md §14), while
// the copies of other files run on other disks at once: an SSD read by two copies, each to an HDD.
func TestCopiesToSeveralDisksAtOnce(t *testing.T) {
	t.Parallel()
	p := newParallel(t, "[defaults]\nreplicas = 2\n", map[string]string{"t1": "ssd", "t2": "hdd", "t3": "hdd"}, "t1", "t2", "t3")
	paths := p.fill("t1", 6)
	items, err := p.replicateAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(items); got[OutcomeReplicated] != 12 {
		t.Fatalf("%v", got)
	}
	if p.writes("t2") != 1 || p.writes("t3") != 1 {
		t.Errorf("an HDD written by %d and %d copies at once", p.writes("t2"), p.writes("t3"))
	}
	both := p.atOnce(func(k spanKey, s *span) bool { return s.wrote && (k.mount == p.dirs["t2"] || k.mount == p.dirs["t3"]) })
	if both != 2 {
		t.Errorf("%d copies at once on the two HDDs, want 2", both)
	}
	// The copies of one file never overlap, on whichever disks.
	for _, path := range paths {
		if n := p.copiesOf(filepath.Base(path)); n != 1 {
			t.Errorf("%s: %d of its copies at once", path, n)
		}
	}
	p.copied("t1", "t2", paths)
	p.copied("t1", "t3", paths)
}

// What a pass says of its copies is in the order of the queue, whatever order they end in (specs.md §15.12, §29.3): the
// first copy here ends last.
func TestCopiesAtOnceListInTheOrderOfTheQueue(t *testing.T) {
	t.Parallel()
	p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
	paths := p.fill("t1", 6)
	p.fault("t1", func(call, name string) error {
		if call == "read" && name == "f00" {
			time.Sleep(300 * time.Millisecond)
		}
		return nil
	})
	p.now += 11 * 60_000_000_000
	out, err := Run(t.Context(), p.db, p.config, p, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, op := range out.Operations {
		if op.Kind == KindReplicate {
			if op.Outcome != OutcomeReplicated {
				t.Errorf("%+v", op)
			}
			got = append(got, op.Path)
		}
	}
	if !slices.Equal(got, paths) {
		t.Errorf("copies listed %q, want %q", got, paths)
	}
	if p.writes("t2") < 2 {
		t.Error("the copies did not run at once")
	}
	p.copied("t1", "t2", paths)
}

// Faults with several copies under way (CLAUDE.md, "Testing rules"): a disk that fails stops every copy on it at its
// next step, its operations left for the reprise, while the copies of other disks go on; the next run, with nothing
// failing, makes every copy, and nothing is lost or left.
func TestFaultsWithCopiesAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		arm  func(p *parallel, writes *countOf)
		// what the run that fails says, at least
		lost    string // the disk lost, if any
		outcome string // an outcome some copy has
		failed  bool   // the run itself ends in an error
	}{
		{"an I/O error on the target", func(p *parallel, n *countOf) {
			p.fault("t2", func(call, name string) error {
				if call == "write" && n.add() == 3 {
					return syscall.EIO
				}
				return nil
			})
		}, "t2", trash.OutcomeIOError, false},
		{"no room for one copy", func(p *parallel, n *countOf) {
			p.fault("t2", func(call, name string) error {
				if call == "write" && n.add() == 3 {
					return syscall.ENOSPC
				}
				return nil
			})
		}, "", OutcomeNoRoom, false},
		{"a target that goes away", func(p *parallel, n *countOf) {
			p.fault("t2", func(call, name string) error {
				if n.add() >= 12 {
					return syscall.ENODEV
				}
				return nil
			})
		}, "t2", trash.OutcomeIOError, false},
		{"an I/O error on the source several copies read", func(p *parallel, n *countOf) {
			p.fault("t1", func(call, name string) error {
				if call == "read" && n.add() == 3 {
					return syscall.EIO
				}
				return nil
			})
		}, "t1", trash.OutcomeIOError, false},
		{"stopped, as SIGTERM or a low battery stops it", func(p *parallel, n *countOf) {
			p.fault("t2", func(call, name string) error {
				if call == "write" && n.add() == 3 {
					p.cancel()
				}
				return nil
			})
		}, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
			paths := p.fill("t1", 6)
			ctx, cancel := context.WithCancel(t.Context())
			p.cancel = cancel
			var n countOf
			tc.arm(p, &n)
			var items []ReplicateItem
			_, err := Replicate(ctx, p.db, p.config, p, ReplicateOptions{Now: true}, func(i *ReplicateItem) error {
				items = append(items, *i)
				return nil
			})
			switch {
			case tc.failed && !errors.Is(err, context.Canceled):
				t.Fatalf("the run gave %v", err)
			case !tc.failed && err != nil:
				t.Fatal(err)
			}
			// Once the run has returned, no copy of it touches a disk: the session let go of them.
			p.mu.Lock()
			p.over = true
			p.mu.Unlock()
			time.Sleep(200 * time.Millisecond)
			p.mu.Lock()
			if p.late > 0 {
				t.Errorf("%d calls on a disk after the run returned", p.late)
			}
			p.over, p.late = false, 0
			p.mu.Unlock()
			if tc.outcome != "" && outcomes(items)[tc.outcome] == 0 {
				t.Errorf("no copy %s: %v", tc.outcome, outcomes(items))
			}
			for i := 1; i < len(items); i++ {
				if items[i].Path <= items[i-1].Path {
					t.Errorf("the lines are not in the order of the queue: %+v", items)
				}
			}
			if tc.lost != "" {
				if got := p.text("SELECT state FROM disks WHERE id = ?", tc.lost); got != "missing" {
					t.Errorf("%s is %s", tc.lost, got)
				}
				for _, other := range []string{"t1", "t2"} {
					if other != tc.lost && p.text("SELECT state FROM disks WHERE id = ?", other) == "missing" {
						t.Errorf("%s was lost for an error of %s", other, tc.lost)
					}
				}
			}
			// A copy recorded is one at its name, as its source holds it.
			rows := p.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok'")
			made := 0
			for _, path := range paths {
				if p.exists("t2", "media/"+path) {
					made++
				}
			}
			if rows > made {
				t.Errorf("%d copies recorded, %d made", rows, made)
			}
			// The next run, with nothing failing, makes what is left: the reprise first.
			p.fault("t1", nil)
			p.fault("t2", nil)
			p.now += 3_600_000_000_000
			for range 3 {
				if _, err := p.replicateAll(); err != nil {
					t.Fatal(err)
				}
				if p.count("SELECT count(*) FROM jobs") == 0 {
					break
				}
				p.now += 3_600_000_000_000
			}
			p.copied("t1", "t2", paths)
		})
	}
}

// countOf counts calls from copies at once.
type countOf struct {
	mu sync.Mutex
	n  int
}

func (c *countOf) add() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}
