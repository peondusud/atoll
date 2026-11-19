package solver

import (
	"flag"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/value"
)

// The properties of CLAUDE.md ("solver: property tests") with rapid, beside the hand-written ones of plan_test.go:
// rapid draws each pool from choices it can shrink, so that a failure comes down to a few disks and files, and its
// state machine runs the solver over a sequence of what happens to a pool.

// given says which of rapid's settings the run named itself, by its flag or its environment variable (RAPID_SEED,
// RAPID_CHECKS, RAPID_NOFAILFILE): once, before checkRapid sets any.
var given = sync.OnceValue(func() map[string]bool {
	named := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { named[f.Name] = true })
	for name, unset := range map[string]string{"rapid.seed": "0", "rapid.checks": "100", "rapid.nofailfile": "false"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != unset {
			named[name] = true
		}
	}
	return named
})

// checkRapid checks a property with rapid, with its own seed and number of checks unless the run names them: go test
// ./... then draws the same cases on every run, as the hand-written properties do, and a run that names a seed, 0 for
// a new one each time, and more checks explores (.github/workflows/fuzz.yml). Rapid prints the seed of the case that
// failed, which -rapid.seed reproduces, and writes no file to testdata/ unless -rapid.nofailfile=false asks for one.
// Tests that call it never run in parallel.
func checkRapid(t *testing.T, seed uint64, checks int, prop func(*rapid.T)) {
	t.Helper()
	settings := map[string]string{"rapid.seed": strconv.FormatUint(seed, 10), "rapid.checks": strconv.Itoa(checks), "rapid.nofailfile": "true"}
	for name, setting := range settings {
		if given()[name] {
			continue
		}
		if err := flag.Set(name, setting); err != nil {
			t.Fatal(err)
		}
	}
	rapid.Check(t, prop)
}

// oneIn draws true once in n, shrinking to false.
func oneIn(t *rapid.T, n int, label string) bool {
	return rapid.IntRange(0, n-1).Draw(t, label) == n-1
}

// drawInput draws a pool as randomInput makes one, where everything can collide and run out of room.
func drawInput(t *rapid.T) Input {
	in := Input{}
	var err error
	in.MinFree, err = value.ParseMinFree(rapid.SampledFrom([][]string{{"0%"}, {"10%"}, {"5%", "50B"}}).Draw(t, "min_free"))
	if err != nil {
		t.Fatal(err)
	}
	in.CacheMirror, in.CacheEarly = rapid.Bool().Draw(t, "mirror"), rapid.Bool().Draw(t, "early")
	for i := range rapid.IntRange(1, 6).Draw(t, "disks") {
		capacity := rapid.Int64Range(50, 449).Draw(t, "capacity")
		in.Disks = append(in.Disks, Disk{
			ID: fmt.Sprintf("d%d", i), Position: 100 - i, Capacity: capacity, Free: capacity - rapid.Int64Range(0, capacity).Draw(t, "used"),
			State: rapid.SampledFrom([]DiskState{
				DiskOnline, DiskOnline, DiskOnline, DiskStandby, DiskStandby, DiskMissing, DiskReadOnly, DiskDraining, DiskFailed, DiskRetired,
			}).Draw(t, "state"),
			Role:   rapid.SampledFrom([]string{RoleData, RoleData, RoleCache}).Draw(t, "role"),
			FS:     rapid.SampledFrom([]names.Filesystem{names.Ext4, names.Ext4, names.NTFS, names.XFS}).Draw(t, "fs"),
			Domain: rapid.SampledFrom([]string{"", "a", "b", "c"}).Draw(t, "domain"),
			Tags:   rapid.SampledFrom([][]string{nil, {"x"}, {"y"}, {"x", "y"}}).Draw(t, "tags"),
			Mount:  "/mnt/d", Root: "media", PastGrace: rapid.Bool().Draw(t, "past grace"),
			Physical: rapid.SampledFrom([]string{"", "", "", "sda", "sdb"}).Draw(t, "physical"),
		})
	}
	paths := []string{"p", "P", "p/q", "p/r", "P/s", "t", "T", "t/u/v", "a:b", "w", "x", "y", "z/1", "z/2", "z/3"}
	for _, path := range rapid.SliceOfNDistinct(rapid.SampledFrom(paths), 1, len(paths), rapid.ID[string]).Draw(t, "paths") {
		f := File{
			Path: path, Size: rapid.Int64Range(0, 119).Draw(t, "size"), Hold: oneIn(t, 6, "hold"), Leaving: oneIn(t, 8, "leaving"),
			Placement: &Placement{
				Replicas: rapid.IntRange(0, 2).Draw(t, "replicas"), Priority: rapid.SampledFrom([]int{0, 0, 0, 5, -5}).Draw(t, "priority"),
				Tags:       rapid.SampledFrom([][]string{nil, nil, {"x"}, {"z"}}).Draw(t, "rule tags"),
				TagsMode:   rapid.SampledFrom([]string{ModePrefer, ModeStrict}).Draw(t, "tags_mode"),
				Domains:    rapid.SampledFrom([][]string{nil, nil, {"a"}}).Draw(t, "rule domains"),
				Spread:     rapid.SampledFrom([]string{SpreadDomain, SpreadNone}).Draw(t, "spread"),
				SpreadMode: rapid.SampledFrom([]string{ModePrefer, ModeStrict}).Draw(t, "spread_mode"),
				Overflow:   rapid.SampledFrom([]string{OverflowAllow, OverflowDeny}).Draw(t, "overflow"),
			},
		}
		for _, d := range in.Disks {
			if !oneIn(t, 3, "a copy on "+d.ID) {
				continue
			}
			f.Copies = append(f.Copies, Copy{
				Disk: d.ID,
				State: rapid.SampledFrom([]CopyState{
					CopyOK, CopyOK, CopyOK, CopySurplus, CopyPending, CopyStale, CopyCorrupt, CopyUnavailable,
				}).Draw(t, "copy state"),
				CreatedBy:   rapid.SampledFrom([]string{CreatedByUser, CreatedByAtoll}).Draw(t, "created by"),
				FirstSeenNS: rapid.Int64Range(0, 3).Draw(t, "first seen"), Unverified: oneIn(t, 6, "unverified"),
			})
		}
		// A file with a copy on a cache disk may be on the SSD cache, resident or leaving it (specs.md §19).
		if slices.ContainsFunc(f.Copies, func(c Copy) bool { return roleOf(in.Disks, c.Disk) == RoleCache }) {
			f.Cache = rapid.SampledFrom([]Residency{CacheNone, CacheResident, CacheTransfer, CacheTransfer}).Draw(t, "cache")
		}
		in.Files = append(in.Files, f)
	}
	for _, d := range in.Disks {
		if oneIn(t, 3, "an entry on "+d.ID) {
			in.Entries = append(in.Entries, Entry{
				Disk: d.ID, Path: rapid.SampledFrom([]string{"p", "t", "w", "z", "T"}).Draw(t, "entry path"),
				Kind: rapid.SampledFrom([]EntryKind{EntryDir, EntrySymlink, EntryIgnored}).Draw(t, "entry kind"),
			})
		}
	}
	in.Taken = asCatalog(&in)
	return in
}

// TestPlanPropertiesRapid checks every plan as TestPlanProperties does, on pools rapid draws: what no plan may do
// (checkPlan: never two copies on a disk, min_free, the level, user copies never all surplus), the same plan whatever
// the order of the input, and plans that come to rest.
func TestPlanPropertiesRapid(t *testing.T) {
	reach := map[string]int{}
	checkRapid(t, 0x5DEECE66D, 2000, func(t *rapid.T) {
		in := drawInput(t)
		plan, err := MakePlan(in)
		if err != nil {
			t.Fatalf("%v", err)
		}
		checkPlan(t, 0, in, plan)
		for _, j := range plan.Jobs {
			reach[string(j.Kind)]++
		}
		for _, r := range plan.Results {
			reach["surplus"] += len(r.Surplus)
			reach["cancel"] += len(r.Cancel)
		}
		again := in
		again.Disks = rapid.Permutation(in.Disks).Draw(t, "disk order")
		again.Entries = rapid.Permutation(in.Entries).Draw(t, "entry order")
		again.Files = rapid.Permutation(in.Files).Draw(t, "file order")
		for i := range again.Files {
			again.Files[i].Copies = rapid.Permutation(again.Files[i].Copies).Draw(t, "copy order")
		}
		same, err := MakePlan(again)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !reflect.DeepEqual(plan, same) {
			t.Fatalf("the order of the input changed the plan\n%+v\n%+v", plan, same)
		}
		checkStable(t, 0, in, plan)
	})
	checkReach(t, reach, "replicate", "refresh", "transfer", "surplus", "cancel")
}

// checkReach checks that the cases drawn from the test's own seed reach what they are meant to test, as that seed
// draws the same cases each time; a run that names its seed only logs what it reached.
func checkReach(t *testing.T, reach map[string]int, what ...string) {
	t.Helper()
	t.Logf("reached: %v", reach)
	if given()["rapid.seed"] || given()["rapid.checks"] {
		return
	}
	for _, w := range what {
		if reach[w] == 0 {
			t.Errorf("the cases never reached %q: %v", w, reach)
		}
	}
}

// TestCapacityRapid: with k copies of every file, a plan protects no more than the capacity of specs.md §13.1, as
// TestPlanStaysWithinCapacity checks, on pools rapid draws.
func TestCapacityRapid(t *testing.T) {
	minFree, err := value.ParseMinFree([]string{"10%"})
	if err != nil {
		t.Fatal(err)
	}
	checkRapid(t, 0x2545F4914F6CDD1D, 1000, func(t *rapid.T) {
		k := rapid.IntRange(1, 3).Draw(t, "copies")
		in := Input{MinFree: minFree}
		var usable []int64
		room := map[string]int64{}
		for i := range rapid.IntRange(2, 6).Draw(t, "disks") {
			capacity := rapid.Int64Range(100, 999).Draw(t, "capacity")
			d := Disk{ID: fmt.Sprintf("d%d", i), Position: i, State: DiskOnline, Role: RoleData, Capacity: capacity, Free: capacity, FS: names.Ext4, Mount: "/m", Root: "r"}
			in.Disks = append(in.Disks, d)
			u, err := minFree.Usable(capacity)
			if err != nil {
				t.Fatal(err)
			}
			usable = append(usable, u)
			room[d.ID] = u
		}
		// Each file starts with one copy, within what its disk may hold.
		for i := range rapid.IntRange(0, 40).Draw(t, "files") {
			size := rapid.Int64Range(1, 60).Draw(t, "size")
			home := rapid.IntRange(0, len(in.Disks)-1).Draw(t, "home")
			if room[in.Disks[home].ID] < size {
				continue
			}
			room[in.Disks[home].ID] -= size
			in.Disks[home].Free -= size
			in.Files = append(in.Files, File{
				Path: fmt.Sprintf("f%d", i), Size: size,
				Placement: &Placement{Replicas: k - 1, TagsMode: ModePrefer, Spread: SpreadDomain, SpreadMode: ModePrefer, Overflow: OverflowAllow},
				Copies:    []Copy{{Disk: in.Disks[home].ID, State: CopyOK, CreatedBy: CreatedByUser}},
			})
		}
		plan, err := MakePlan(in)
		if err != nil {
			t.Fatal(err)
		}
		checkPlan(t, 0, in, plan)
		jobs := map[string]int{}
		for _, j := range plan.Jobs {
			jobs[j.Path]++
		}
		var protected int64
		for _, f := range in.Files {
			if 1+jobs[f.Path] == k {
				protected += f.Size
			}
		}
		limit, err := Capacity(usable, k)
		if err != nil {
			t.Fatal(err)
		}
		if protected > limit {
			t.Fatalf("%d bytes at %d copies on %v, past the capacity %d", protected, k, usable, limit)
		}
	})
}

// pool is the state of TestSolverStateMachine: the data disks of a pool and its files, as the catalog holds them
// after each step, each job of a plan run to its end at once.
type pool struct {
	in    Input
	step  int64
	reach map[string]int // what the steps reached, for the test to check
}

// The names a user writes in TestSolverStateMachine: each one every filesystem of the pool takes, and none that
// collides with another on NTFS, so that a plan can always place a file where there is room.
var poolNames = []string{
	"Movies/A (2001)/A.mkv", "Movies/B (2002)/B.mkv", "Movies/C (2003)/C.mkv",
	"Shows/D/Season 01/D - S01E01.mkv", "Shows/D/Season 01/D - S01E02.mkv", "Shows/E/Season 02/E - S02E01.mkv",
	"Music/F/01 - G.flac", "Music/F/02 - H.flac", "Photos/I.jpg", "Photos/J.jpg",
}

func (p *pool) disk(id string) *Disk {
	for i := range p.in.Disks {
		if p.in.Disks[i].ID == id {
			return &p.in.Disks[i]
		}
	}
	return nil
}

// counted gives the copies of a file that count toward its level, a physical disk once, and whether the file has a
// source a job can read, as specs.md §13.4 says of the copies and disks of this pool: written here again, so that a
// change to the solver's own counts cannot change what the model expects.
func (p *pool) counted(f *File) (count int, source bool) {
	physical := map[string]bool{}
	for i := range f.Copies {
		c := &f.Copies[i]
		d := p.disk(c.Disk)
		readable := c.State == CopyOK || c.State == CopySurplus
		source = source || readable && d.State == DiskOnline
		if !readable || d.State != DiskOnline && (d.State != DiskMissing || d.PastGrace) {
			continue
		}
		if d.Physical != "" {
			if physical[d.Physical] {
				continue
			}
			physical[d.Physical] = true
		}
		count++
	}
	return count, source
}

// placeable says whether a plan could still place a copy of f on d: a data disk that can be written to, holding no
// copy of it, on no physical disk that holds one.
func (p *pool) placeable(f *File, d *Disk) bool {
	if !writable(d) {
		return false
	}
	for _, c := range f.Copies {
		other := p.disk(c.Disk)
		if other.ID == d.ID || d.Physical != "" && other.Physical == d.Physical {
			return false
		}
	}
	return true
}

func (p *pool) plan(t *rapid.T) Plan {
	p.in.Taken = asCatalog(&p.in)
	plan, err := MakePlan(p.in)
	if err != nil {
		t.Fatalf("step %d: %v", p.step, err)
	}
	checkPlan(t, int(p.step), p.in, plan)
	return plan
}

// add: a user writes a new file on a disk that can take it.
func (p *pool) add(t *rapid.T) {
	free := slices.DeleteFunc(slices.Clone(poolNames), func(name string) bool {
		return slices.ContainsFunc(p.in.Files, func(f File) bool { return f.Path == name })
	})
	if len(free) == 0 {
		t.Skip("every name is taken")
	}
	name := rapid.SampledFrom(free).Draw(t, "name")
	size := rapid.Int64Range(1, 1000).Draw(t, "size")
	var homes []string
	for _, d := range p.in.Disks {
		if writable(&d) && d.Free >= size {
			homes = append(homes, d.ID)
		}
	}
	if len(homes) == 0 {
		t.Skip("no disk can take it")
	}
	home := p.disk(rapid.SampledFrom(homes).Draw(t, "home"))
	home.Free -= size
	p.step++
	p.in.Files = append(p.in.Files, File{
		Path: name, Size: size,
		Placement: &Placement{
			Replicas: rapid.IntRange(0, 2).Draw(t, "replicas"), TagsMode: ModePrefer, Spread: SpreadDomain,
			SpreadMode: ModePrefer, Overflow: OverflowAllow,
		},
		Copies: []Copy{{Disk: home.ID, State: CopyOK, CreatedBy: CreatedByUser, FirstSeenNS: p.step}},
	})
}

// remove: a user deletes a file, every copy of it, as a deletion through the view propagates (specs.md §15.5).
func (p *pool) remove(t *rapid.T) {
	if len(p.in.Files) == 0 {
		t.Skip("no file")
	}
	i := rapid.IntRange(0, len(p.in.Files)-1).Draw(t, "file")
	for _, c := range p.in.Files[i].Copies {
		p.disk(c.Disk).Free += p.in.Files[i].Size
	}
	p.in.Files = slices.Delete(p.in.Files, i, i+1)
}

// lose: a disk goes missing, within health.missing_grace or past it; its copies stay in the catalog.
func (p *pool) lose(t *rapid.T) {
	var up []string
	for _, d := range p.in.Disks {
		if d.State == DiskOnline {
			up = append(up, d.ID)
		}
	}
	if len(up) == 0 {
		t.Skip("no disk to lose")
	}
	d := p.disk(rapid.SampledFrom(up).Draw(t, "disk"))
	d.State, d.PastGrace = DiskMissing, rapid.Bool().Draw(t, "past grace")
}

// back: a missing disk comes back, with what it held.
func (p *pool) back(t *rapid.T) {
	var gone []string
	for _, d := range p.in.Disks {
		if d.State == DiskMissing {
			gone = append(gone, d.ID)
		}
	}
	if len(gone) == 0 {
		t.Skip("no disk is missing")
	}
	d := p.disk(rapid.SampledFrom(gone).Draw(t, "disk"))
	d.State, d.PastGrace = DiskOnline, false
}

// pass: a plan, whose copies are each made at once and whose marks are recorded, as a pass does (specs.md §15.12).
// Then every file with a source is at its level, or has a copy on every disk a copy of it could go to; and a plan
// made again finds nothing more to do.
func (p *pool) pass(t *rapid.T) {
	plan := p.plan(t)
	p.step++
	for _, j := range plan.Jobs {
		f := &p.in.Files[slices.IndexFunc(p.in.Files, func(f File) bool { return f.Path == j.Path })]
		switch j.Kind {
		case JobReplicate:
			p.reach["copy"]++
			if slices.ContainsFunc(f.Copies, func(c Copy) bool { d := p.disk(c.Disk); return d.State == DiskMissing && d.PastGrace }) {
				p.reach["copy for a disk past its grace"]++
			}
			p.disk(j.Dst).Free -= f.Size
			f.Copies = append(f.Copies, Copy{Disk: j.Dst, State: CopyOK, CreatedBy: CreatedByAtoll, FirstSeenNS: p.step})
		case JobRefresh, JobTransfer:
			t.Fatalf("step %d: a pool with neither stale copies nor a cache gets %+v", p.step, j)
		default:
			t.Fatalf("step %d: a job of no kind: %+v", p.step, j)
		}
	}
	for _, r := range plan.Results {
		f := &p.in.Files[slices.IndexFunc(p.in.Files, func(f File) bool { return f.Path == r.Path })]
		for i := range f.Copies {
			c := &f.Copies[i]
			switch {
			case slices.Contains(r.Surplus, c.Disk):
				p.reach["surplus"]++
				c.State = CopySurplus
			case slices.Contains(r.Back, c.Disk):
				c.State = CopyOK
			}
		}
	}
	for i := range p.in.Files {
		f := &p.in.Files[i]
		count, source := p.counted(f)
		if !source || count >= f.Placement.Replicas+1 {
			continue
		}
		for j := range p.in.Disks {
			if d := &p.in.Disks[j]; p.placeable(f, d) && d.Free-f.Size >= p.reserved(t, d) {
				t.Fatalf("step %d: %q has %d copies of %d, and none was placed on %q: %+v", p.step, f.Path, count, f.Placement.Replicas+1, d.ID, f.Copies)
			}
		}
	}
	if again := p.plan(t); len(again.Jobs) > 0 {
		t.Fatalf("step %d: a plan after the pass adds %+v", p.step, again.Jobs)
	}
}

func (p *pool) reserved(t *rapid.T, d *Disk) int64 {
	reserved, err := p.in.MinFree.Reserved(d.Capacity)
	if err != nil {
		t.Fatal(err)
	}
	return reserved
}

// prune: atoll prune sends the surplus copies DecidePrune lets go to the trash (specs.md §17.3): a file keeps a user
// copy when it had one, and as many copies that count as its level wants, or all it had.
func (p *pool) prune(t *rapid.T) {
	for i := range p.in.Files {
		f := &p.in.Files[i]
		in := PruneInput{State: StateOK, Replicas: f.Placement.Replicas, HasHash: true}
		users := 0
		for _, c := range f.Copies {
			d := p.disk(c.Disk)
			in.Copies = append(in.Copies, PruneCopy{
				Disk: c.Disk, Position: d.Position, State: c.State, CreatedBy: c.CreatedBy, DiskState: d.State,
				Verified: true, Unchanged: d.State != DiskMissing,
			})
			if c.CreatedBy == CreatedByUser {
				users++
			}
		}
		before, _ := p.counted(f)
		results, err := DecidePrune(in)
		if err != nil {
			t.Fatalf("step %d: %v", p.step, err)
		}
		for _, r := range results {
			p.reach["prune "+string(r.Outcome)]++
			if r.Outcome == PruneToTrash {
				p.disk(r.Disk).Free += f.Size
				f.Copies = slices.DeleteFunc(f.Copies, func(c Copy) bool { return c.Disk == r.Disk })
			}
		}
		after, _ := p.counted(f)
		kept := slices.ContainsFunc(f.Copies, func(c Copy) bool { return c.CreatedBy == CreatedByUser })
		if users > 0 && !kept {
			t.Fatalf("step %d: prune took the last user copy of %q", p.step, f.Path)
		}
		if after < min(before, f.Placement.Replicas+1) {
			t.Fatalf("step %d: prune left %q %d copies that count, of %d before and a level of %d", p.step, f.Path, after, before, f.Placement.Replicas+1)
		}
	}
}

// check holds after every step: never two copies of a file on a disk (invariant 13 by disk), and the room of each
// disk is what its copies leave.
func (p *pool) check(t *rapid.T) {
	used := map[string]int64{}
	for _, f := range p.in.Files {
		seen := map[string]bool{}
		for _, c := range f.Copies {
			if seen[c.Disk] {
				t.Fatalf("step %d: two copies of %q on %q", p.step, f.Path, c.Disk)
			}
			seen[c.Disk] = true
			used[c.Disk] += f.Size
		}
	}
	for _, d := range p.in.Disks {
		if d.Free != d.Capacity-used[d.ID] {
			t.Fatalf("step %d: %q has %d bytes free, and its copies leave %d", p.step, d.ID, d.Free, d.Capacity-used[d.ID])
		}
	}
}

// TestSolverStateMachine runs the solver over what happens to a pool: files written and deleted, disks lost, past
// their grace or not, and back, passes, and atoll prune, in any order, with the properties of each plan checked at
// each pass (checkPlan).
func TestSolverStateMachine(t *testing.T) {
	reach := map[string]int{}
	checkRapid(t, 0x9E3779B97F4A7C15, 1000, func(t *rapid.T) {
		p := &pool{reach: reach}
		var err error
		p.in.MinFree, err = value.ParseMinFree(rapid.SampledFrom([][]string{{"0%"}, {"10%"}, {"5%", "500B"}}).Draw(t, "min_free"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range rapid.IntRange(2, 5).Draw(t, "disks") {
			capacity := rapid.Int64Range(2000, 20000).Draw(t, "capacity")
			p.in.Disks = append(p.in.Disks, Disk{
				ID: fmt.Sprintf("d%d", i), Position: i, State: DiskOnline, Role: RoleData, Capacity: capacity, Free: capacity,
				FS:    rapid.SampledFrom([]names.Filesystem{names.Ext4, names.XFS, names.NTFS}).Draw(t, "fs"),
				Mount: "/mnt/d", Root: "media",
				Physical: rapid.SampledFrom([]string{"", "", "", "sda"}).Draw(t, "physical"),
			})
		}
		t.Repeat(map[string]func(*rapid.T){
			"add":    p.add,
			"remove": p.remove,
			"lose":   p.lose,
			"back":   p.back,
			"pass":   p.pass,
			"prune":  p.prune,
			"":       p.check,
		})
	})
	checkReach(t, reach, "copy", "copy for a disk past its grace", "surplus", "prune to-trash", "prune below-level", "prune last-user-copy")
}
