package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// partOf gives the part the copy of a file writes, once its operation is journaled.
func (p *parallel) partOf(path string) string {
	var part string
	err := p.db.QueryRowContext(context.Background(),
		"SELECT COALESCE((SELECT CAST(path2 AS TEXT) FROM ops WHERE kind = 'create' AND path = CAST(? AS BLOB) ORDER BY id DESC LIMIT 1), '')", path).Scan(&part)
	if err != nil {
		return ""
	}
	return part
}

// A disk one copy finds it cannot rely on stops the copy beside it at its next step, as that disk's own error would
// (invariant 4): its operation stays for the reprise when the target is lost, and is undone, its part kept for the copy
// to resume, when the source is. The handle both use is let go of only once neither does; and the next run makes both.
func TestADiskLostStopsTheCopiesBesideIt(t *testing.T) {
	for _, tc := range []struct{ name, lost, state string }{
		{"the target", "t2", "planned"},
		{"the source", "t1", "undone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
			paths := p.fill("t1", 2)
			p.slow = 0
			// The first file's copy fails on the disk lost; the second's write waits until the first is over and said.
			first := make(chan struct{})
			var once sync.Once
			p.fault("t1", func(call, name string) error {
				if tc.lost == "t1" && call == "read" && name == "f00" {
					return syscall.EIO
				}
				return nil
			})
			p.fault("t2", func(call, name string) error {
				if call != "write" {
					return nil
				}
				if name == p.partOf(paths[0]) {
					if tc.lost == "t2" {
						return syscall.EIO
					}
					return nil
				}
				<-first
				return nil
			})
			var items []ReplicateItem
			_, err := Replicate(t.Context(), p.db, p.config, p, ReplicateOptions{Now: true}, func(i *ReplicateItem) error {
				items = append(items, *i)
				if i.Path == paths[0] {
					once.Do(func() { close(first) })
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 2 || items[0].Outcome != trash.OutcomeIOError || items[1].Outcome != trash.OutcomeIOError {
				t.Fatalf("%+v", items)
			}
			if got := p.count("SELECT count(*) FROM ops WHERE kind = 'create' AND state = ?", tc.state); got != 2 {
				t.Errorf("%d operations %s, want both", got, tc.state)
			}
			if got := p.parts("t2"); len(got) != 2 {
				t.Errorf("tmp/ holds %q, want both parts", got)
			}
			if got := p.text("SELECT state FROM disks WHERE id = ?", tc.lost); got != "missing" {
				t.Errorf("%s is %s", tc.lost, got)
			}
			p.fault("t1", nil)
			p.fault("t2", nil)
			p.now += 3_600_000_000_000
			if items, err := p.replicateAll(); err != nil || outcomes(items)[OutcomeReplicated] != 2 {
				t.Fatalf("%+v, %v", items, err)
			}
			p.copied("t1", "t2", paths)
		})
	}
}

// crashSpec is what a crash test tells the process it starts.
type crashSpec struct {
	IDs   []string
	Kinds map[string]string
	More  string
	Now   int64
	Point string
	Count int64
}

// TestCrashHelper is the process a crash test starts: a pass on the pool the test made, whose copies run at once, stopped
// as kill -9 stops it the Count-th time a copy reaches Point: nothing after it runs, in any copy.
func TestCrashHelper(t *testing.T) {
	top := os.Getenv("ATOLL_JOBS_CRASH")
	if top == "" {
		t.Skip("only as the process a crash test starts")
	}
	var spec crashSpec
	if err := json.Unmarshal([]byte(os.Getenv("ATOLL_JOBS_CRASH_SPEC")), &spec); err != nil {
		t.Fatal(err)
	}
	b := benchIn(t, top, spec.More, false, spec.IDs...)
	b.now = spec.Now
	p := parallelOf(b, spec.Kinds)
	var hits atomic.Int64
	p.stopAt = func(point string) {
		if point == spec.Point && hits.Add(1) == spec.Count {
			os.Exit(137)
		}
	}
	_, err := Run(context.Background(), b.db, b.config, p, true)
	t.Fatalf("it did not stop at %s: %v", spec.Point, err)
}

// crash runs a pass in a process of its own that stops at a point, as kill -9 would, with copies at once under way.
func (p *parallel) crash(spec crashSpec) {
	p.t.Helper()
	spec.Now = p.now
	text, err := json.Marshal(spec)
	if err != nil {
		p.t.Fatal(err)
	}
	cmd := exec.CommandContext(p.t.Context(), os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ATOLL_JOBS_CRASH="+filepath.Dir(p.dirs[spec.IDs[0]]), "ATOLL_JOBS_CRASH_SPEC="+string(text))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 137 {
		p.t.Fatalf("the pass did not stop at %s: %v\n%s", spec.Point, err, out)
	}
}

// fillLarge writes n files of two chunks and a little more below F/, each its own, and scans them.
func (p *parallel) fillLarge(id string, n int) []string {
	p.t.Helper()
	var paths []string
	for i := range n {
		path := fmt.Sprintf("F/f%02d", i)
		data := bytes.Repeat([]byte(fmt.Sprintf("file %d ", i)), (2*4<<20+5000)/7)
		full := filepath.Join(p.dirs[id], "media", path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			p.t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			p.t.Fatal(err)
		}
		paths = append(paths, path)
	}
	p.scan(false)
	return paths
}

// A crash with several copies under way, at each point of the pipeline (specs.md §14.2, §17.2; CLAUDE.md, "Testing
// rules"): the next pass resolves what each left, resumes each part from its first chunk missing or wrong, and makes
// every copy, with nothing left planned or in tmp/.
func TestACrashWithCopiesAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process for each point")
	}
	for _, point := range []string{"journaled", "copied", "verified", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			kinds := map[string]string{"t1": "ssd", "t2": "ssd"}
			p := newParallel(t, "", kinds, "t1", "t2")
			paths := p.fillLarge("t1", 6)
			p.now += 11 * 60_000_000_000
			p.crash(crashSpec{IDs: []string{"t1", "t2"}, Kinds: kinds, Point: point, Count: 3})
			// Several copies were under way when it stopped: each left its operation, and a part for most.
			planned := p.count("SELECT count(*) FROM ops WHERE kind = 'create' AND state = 'planned'")
			if planned < 2 {
				t.Errorf("%d copies under way at the crash", planned)
			}
			t.Logf("at %s: %d operations planned, tmp/ holds %q", point, planned, p.parts("t2"))
			for range 3 {
				p.forget()
				if _, err := Run(t.Context(), p.db, p.config, p, true); err != nil {
					t.Fatal(err)
				}
				if p.writes("t2") > 4 || p.reads("t1") > 4 {
					t.Errorf("%d and %d copies at once", p.writes("t2"), p.reads("t1"))
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

// Copies at once keep every invariant, on pools drawn at random (CLAUDE.md, "Testing rules"): disks of each kind, limits
// of each key, files of several sizes, one or two replicas, and in the first run a fault drawn at random. An HDD is never
// read or written by two copies at once, a copy counting against both its disks, and an SSD by more than its limit;
// every copy recorded is its file byte for byte, no disk holds a copy the catalog does not, the originals never change,
// and once nothing fails every file reaches its level with nothing left planned, queued or in tmp/.
func TestCopiesAtOnceKeepTheInvariants(t *testing.T) {
	seeds := uint64(16)
	if testing.Short() {
		seeds = 4
	}
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, 2026))
			ids := []string{"t1", "t2", "t3", "t4"}[:2+r.IntN(3)]
			kinds := map[string]string{}
			for _, id := range ids {
				kinds[id] = []string{"ssd", "hdd", "unknown", "ssd"}[r.IntN(4)]
			}
			replicas := 1 + r.IntN(2)
			more := fmt.Sprintf("[defaults]\nreplicas = %d\n", replicas)
			perDisk, ssd := 0, 4
			switch r.IntN(3) {
			case 1:
				ssd = 2
				more += "[replication]\nmax_parallel_per_ssd = 2\n"
			case 2:
				perDisk = 2
				more += "[replication]\nmax_parallel_per_disk = 2\n"
			}
			limit := func(id string) int {
				switch {
				case perDisk > 0:
					return perDisk
				case kinds[id] == "ssd":
					return ssd
				}
				return 1
			}
			p := newParallel(t, more, kinds, ids...)
			p.slow = 0
			want := map[string][]byte{}
			from := map[string]string{}
			for i := range 4 + r.IntN(8) {
				path := fmt.Sprintf("D%d/f%02d", i%3, i)
				size := []int{0, 10, 3000, 4<<20 + 100}[r.IntN(4)]
				data := bytes.Repeat([]byte{byte('a' + i)}, size)
				id := ids[r.IntN(len(ids))]
				full := filepath.Join(p.dirs[id], "media", path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, data, 0o644); err != nil {
					t.Fatal(err)
				}
				want[path], from[path] = data, id
			}
			p.scan(false)
			// A fault in the first run, on a disk drawn at random, after a number of its calls drawn at random.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p.cancel = cancel
			victim, after, fault := ids[r.IntN(len(ids))], 1+r.IntN(20), r.IntN(5)
			var calls countOf
			p.fault(victim, func(call, name string) error {
				if (call != "write" && call != "read") || calls.add() != after {
					return nil
				}
				switch fault {
				case 1:
					return syscall.EIO
				case 2:
					return syscall.ENOSPC
				case 3:
					p.cancel()
				case 4:
					return syscall.ENODEV
				}
				return nil
			})
			check := func(run int) {
				for _, id := range ids {
					if n := p.uses(id); n > limit(id) {
						t.Errorf("run %d: %s (%s) used by %d copies at once, limit %d", run, id, kinds[id], n, limit(id))
					}
				}
				if n := p.atOnce(func(spanKey, *span) bool { return true }); n > 2*copiesAtOnce {
					t.Errorf("run %d: %d copies at once", run, n/2)
				}
			}
			if _, err := Replicate(ctx, p.db, p.config, p, ReplicateOptions{Now: true}, func(*ReplicateItem) error { return nil }); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			check(0)
			for id := range p.dirs {
				p.fault(id, nil)
			}
			for run := 1; run <= 4 && p.count("SELECT count(*) FROM jobs") > 0; run++ {
				p.now += 3_600_000_000_000
				p.forget()
				if _, err := Replicate(t.Context(), p.db, p.config, p, ReplicateOptions{Now: true}, func(*ReplicateItem) error { return nil }); err != nil {
					t.Fatal(err)
				}
				check(run)
			}
			p.clean()
			level := min(replicas+1, len(ids))
			for path, data := range want {
				// The original never changes.
				if got, err := os.ReadFile(filepath.Join(p.dirs[from[path]], "media", path)); err != nil || !bytes.Equal(got, data) {
					t.Errorf("the original of %s changed: %v", path, err)
				}
				var on []string
				for _, id := range ids {
					got, err := os.ReadFile(filepath.Join(p.dirs[id], "media", path))
					switch {
					case errors.Is(err, os.ErrNotExist):
						continue
					case err != nil || !bytes.Equal(got, data):
						t.Errorf("%s on %s is not its file: %v", path, id, err)
					}
					on = append(on, id)
				}
				rows := p.copiesOn(path)
				slices.Sort(on)
				if !slices.Equal(on, rows) {
					t.Errorf("%s: on %v, the catalog says %v", path, on, rows)
				}
				if len(rows) != level {
					t.Errorf("%s: %d copies, want %d", path, len(rows), level)
				}
			}
		})
	}
}

// copiesOn gives the disks the catalog holds a copy ok of a file on, in byte order.
func (p *parallel) copiesOn(path string) []string {
	p.t.Helper()
	list, err := p.db.QueryContext(p.t.Context(),
		"SELECT c.disk_id FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST(? AS BLOB) AND c.state = 'ok' ORDER BY c.disk_id", path)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = list.Close() }()
	var out []string
	for list.Next() {
		var id string
		if err := list.Scan(&id); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := list.Err(); err != nil {
		p.t.Fatal(err)
	}
	return out
}
