package solver

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/vectors"
)

type planVector struct {
	Disks   []diskVector   `json:"disks"`
	Files   []fileVector   `json:"files"`
	Entries []entryVector  `json:"entries"`
	MinFree []string       `json:"min_free"`
	Jobs    []jobVector    `json:"jobs"`
	Results []resultVector `json:"results"`
	Error   bool           `json:"error"`
	Note    string         `json:"note"`
}

type diskVector struct {
	ID        string   `json:"id"`
	Position  int      `json:"position"`
	Capacity  int64    `json:"capacity"`
	Free      int64    `json:"free"`
	State     *string  `json:"state"`
	Role      *string  `json:"role"`
	FS        *string  `json:"fs"`
	Domain    string   `json:"domain"`
	Tags      []string `json:"tags"`
	Mount     *string  `json:"mount"`
	Root      *string  `json:"root"`
	PastGrace bool     `json:"past_grace"`
}

type copyVector struct {
	Disk        string  `json:"disk"`
	State       *string `json:"state"`
	CreatedBy   *string `json:"created_by"`
	FirstSeenNS int64   `json:"first_seen_ns"`
}

type policyVector struct {
	Replicas   *int     `json:"replicas"`
	Priority   int      `json:"priority"`
	Tags       []string `json:"tags"`
	TagsMode   *string  `json:"tags_mode"`
	Domains    []string `json:"domains"`
	Spread     *string  `json:"spread"`
	SpreadMode *string  `json:"spread_mode"`
	Overflow   *string  `json:"overflow"`
}

type fileVector struct {
	Path    *string      `json:"path"`
	PathB64 *string      `json:"path_b64"`
	Size    int64        `json:"size"`
	Copies  []copyVector `json:"copies"`
	Manual  bool         `json:"manual"`
	Policy  policyVector `json:"policy"`
}

type entryVector struct {
	Disk string `json:"disk"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type jobVector struct {
	Kind    string  `json:"kind"`
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Src     string  `json:"src"`
	Dst     string  `json:"dst"`
}

type resultVector struct {
	Path     *string  `json:"path"`
	PathB64  *string  `json:"path_b64"`
	States   []string `json:"states"`
	Surplus  []string `json:"surplus"`
	Back     []string `json:"back"`
	Cancel   []string `json:"cancel"`
	Overflow bool     `json:"overflow"`
}

func or[T any](v *T, fallback T) T {
	if v != nil {
		return *v
	}
	return fallback
}

// input turns a vector into the input of MakePlan, with the defaults of
// conformance/vectors/README.md.
func (v *planVector) input() (Input, error) {
	var in Input
	var err error
	minFree := v.MinFree
	if minFree == nil {
		minFree = []string{"0%"}
	}
	if in.MinFree, err = value.ParseMinFree(minFree); err != nil {
		return in, fmt.Errorf("min_free: %w", err)
	}
	for _, d := range v.Disks {
		in.Disks = append(in.Disks, Disk{
			ID: d.ID, Position: d.Position, Capacity: d.Capacity, Free: d.Free,
			State: DiskState(or(d.State, "online")), Role: or(d.Role, "data"),
			FS: names.Filesystem(or(d.FS, "ext4")), Domain: d.Domain, Tags: d.Tags,
			Mount: or(d.Mount, "/mnt/"+d.ID), Root: or(d.Root, "media"), PastGrace: d.PastGrace,
		})
	}
	for _, f := range v.Files {
		path, err := vectors.Path(f.Path, f.PathB64)
		if err != nil {
			return in, err
		}
		file := File{Path: path, Size: f.Size, Hold: f.Manual, Placement: &Placement{
			Replicas: or(f.Policy.Replicas, 1), Priority: f.Policy.Priority,
			Tags: f.Policy.Tags, TagsMode: or(f.Policy.TagsMode, "prefer"), Domains: f.Policy.Domains,
			Spread: or(f.Policy.Spread, "domain"), SpreadMode: or(f.Policy.SpreadMode, "prefer"),
			Overflow: or(f.Policy.Overflow, "allow"),
		}}
		for _, c := range f.Copies {
			file.Copies = append(file.Copies, Copy{
				Disk: c.Disk, State: CopyState(or(c.State, "ok")),
				CreatedBy: or(c.CreatedBy, "user"), FirstSeenNS: c.FirstSeenNS,
			})
		}
		in.Files = append(in.Files, file)
	}
	for _, e := range v.Entries {
		in.Entries = append(in.Entries, Entry{Disk: e.Disk, Path: e.Path, Kind: EntryKind(e.Kind)})
	}
	return in, nil
}

func wantJobs(t *testing.T, v []jobVector) []Job {
	t.Helper()
	jobs := make([]Job, 0, len(v))
	for _, j := range v {
		path, err := vectors.Path(j.Path, j.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, Job{Kind: JobKind(j.Kind), Path: path, Src: j.Src, Dst: j.Dst})
	}
	return jobs
}

func wantResults(t *testing.T, v []resultVector) []Result {
	t.Helper()
	results := make([]Result, 0, len(v))
	for _, r := range v {
		path, err := vectors.Path(r.Path, r.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		var states []State
		for _, s := range r.States {
			states = append(states, State(s))
		}
		results = append(results, Result{Path: path, States: states, Surplus: r.Surplus, Back: r.Back, Cancel: r.Cancel, Overflow: r.Overflow})
	}
	return results
}

func TestPlanVectors(t *testing.T) {
	for i, v := range vectors.Load[planVector](t, "plan.json") {
		if v.Error == (v.Jobs != nil || v.Results != nil) {
			t.Errorf("vector %d (%s): want jobs and results, or error", i, v.Note)
			continue
		}
		in, err := v.input()
		var plan Plan
		if err == nil {
			plan, err = MakePlan(in)
		}
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): got a plan, want an error", i, v.Note)
			}
			continue
		}
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		if want := wantJobs(t, v.Jobs); !slices.Equal(plan.Jobs, want) && (len(plan.Jobs) > 0 || len(want) > 0) {
			t.Errorf("vector %d (%s): jobs\n%+v, want\n%+v", i, v.Note, plan.Jobs, want)
		}
		if want := wantResults(t, v.Results); !reflect.DeepEqual(plan.Results, want) && (len(plan.Results) > 0 || len(want) > 0) {
			t.Errorf("vector %d (%s): results\n%s, want\n%s", i, v.Note, show(plan.Results), show(want))
		}
		for _, r := range plan.Results {
			if _, err := ShownState(r.States); err != nil {
				t.Errorf("vector %d (%s): %v", i, v.Note, err)
			}
		}
	}
}

func show(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}

// xorshift is a small deterministic generator: no math/rand in the pure core.
type xorshift uint64

func (x *xorshift) next() uint64 {
	v := uint64(*x)
	v ^= v << 13
	v ^= v >> 7
	v ^= v << 17
	*x = xorshift(v)
	return v
}

func (x *xorshift) intn(n int) int {
	return int(x.next() % uint64(n))
}

func oneOf[T any](x *xorshift, list ...T) T {
	return list[x.intn(len(list))]
}

// randomInput makes a small pool where everything can collide and run out of room.
func randomInput(x *xorshift) Input {
	in := Input{}
	var err error
	in.MinFree, err = value.ParseMinFree(oneOf(x, []string{"0%"}, []string{"10%"}, []string{"5%", "50B"}))
	if err != nil {
		panic(err) //nolint:forbidigo // a test helper with constant input
	}
	nDisks := 1 + x.intn(6)
	for i := range nDisks {
		capacity := int64(50 + x.intn(400))
		in.Disks = append(in.Disks, Disk{
			ID: fmt.Sprintf("d%d", i), Position: 100 - i, Capacity: capacity, Free: int64(x.intn(int(capacity) + 1)),
			State:  oneOf(x, DiskOnline, DiskOnline, DiskOnline, DiskStandby, DiskStandby, DiskMissing, DiskReadOnly, DiskDraining, DiskFailed, DiskRetired),
			Role:   oneOf(x, RoleData, RoleData, RoleData, RoleCache),
			FS:     oneOf(x, names.Ext4, names.Ext4, names.NTFS, names.XFS),
			Domain: oneOf(x, "", "a", "b", "c"),
			Tags:   oneOf(x, nil, []string{"x"}, []string{"y"}, []string{"x", "y"}),
			Mount:  "/mnt/d", Root: "media", PastGrace: x.intn(2) == 0,
		})
	}
	paths := []string{"p", "P", "p/q", "p/r", "P/s", "t", "T", "t/u/v", "a:b", "w", "x", "y", "z/1", "z/2", "z/3"}
	x.shuffle(paths)
	for _, path := range paths[:x.intn(len(paths)+1)] {
		f := File{
			Path: path, Size: int64(x.intn(120)), Hold: x.intn(6) == 0,
			Placement: &Placement{
				Replicas: x.intn(3), Priority: oneOf(x, 0, 0, 0, 5, -5),
				Tags: oneOf(x, nil, nil, []string{"x"}, []string{"z"}), TagsMode: oneOf(x, ModePrefer, ModeStrict),
				Domains: oneOf(x, nil, nil, []string{"a"}), Spread: oneOf(x, SpreadDomain, SpreadNone),
				SpreadMode: oneOf(x, ModePrefer, ModeStrict), Overflow: oneOf(x, OverflowAllow, OverflowDeny),
			},
		}
		for _, d := range in.Disks {
			if x.intn(3) == 0 {
				f.Copies = append(f.Copies, Copy{
					Disk: d.ID, State: oneOf(x, CopyOK, CopyOK, CopyOK, CopySurplus, CopyPending, CopyStale, CopyCorrupt, CopyUnavailable),
					CreatedBy: oneOf(x, CreatedByUser, CreatedByAtoll), FirstSeenNS: int64(x.intn(4)),
				})
			}
		}
		in.Files = append(in.Files, f)
	}
	for _, d := range in.Disks {
		if x.intn(3) == 0 {
			in.Entries = append(in.Entries, Entry{Disk: d.ID, Path: oneOf(x, "p", "t", "w", "z", "T"), Kind: oneOf(x, EntryDir, EntrySymlink, EntryIgnored)})
		}
	}
	return in
}

func (x *xorshift) shuffle(list []string) {
	for i := len(list) - 1; i > 0; i-- {
		j := x.intn(i + 1)
		list[i], list[j] = list[j], list[i]
	}
}

func shuffled[T any](x *xorshift, list []T) []T {
	out := slices.Clone(list)
	for i := len(out) - 1; i > 0; i-- {
		j := x.intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func writable(d *Disk) bool {
	return d.Role == RoleData && (d.State == DiskOnline || d.State == DiskStandby)
}

func TestPlanProperties(t *testing.T) {
	x := xorshift(0x9E3779B97F4A7C15)
	withJobs := 0
	reach := map[string]int{}
	for round := range 20000 {
		in := randomInput(&x)
		plan, err := MakePlan(in)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(plan.Jobs) > 0 {
			withJobs++
		}
		checkPlan(t, round, in, plan)

		again := in
		again.Disks = shuffled(&x, in.Disks)
		again.Entries = shuffled(&x, in.Entries)
		again.Files = shuffled(&x, in.Files)
		for i := range again.Files {
			again.Files[i].Copies = shuffled(&x, again.Files[i].Copies)
		}
		same, err := MakePlan(again)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if !reflect.DeepEqual(plan, same) {
			t.Errorf("round %d: the order of the input changed the plan\n%+v\n%+v", round, plan, same)
		}
		checkStable(t, round, in, plan)
		for _, j := range plan.Jobs {
			reach[string(j.Kind)]++
		}
		for _, r := range plan.Results {
			reach["surplus"] += len(r.Surplus)
			reach["back"] += len(r.Back)
			reach["cancel"] += len(r.Cancel)
			if r.Overflow {
				reach["overflow"]++
			}
			for _, s := range r.States {
				reach[string(s)]++
			}
		}
	}
	// The generator must reach what it is meant to test.
	if withJobs < 2000 {
		t.Errorf("only %d of 20000 plans had a job", withJobs)
	}
	for _, what := range []string{
		"refresh", "replicate", "surplus", "back", "cancel", "overflow",
		"unavailable", "name-incompatible", "name-collision", "degraded", "pending", "unprotected",
	} {
		if reach[what] < 100 {
			t.Errorf("the generator reached %q only %d times", what, reach[what])
		}
	}
}

// checkPlan checks what no plan may ever do (CLAUDE.md, "solver: property tests").
func checkPlan(t *testing.T, round int, in Input, plan Plan) {
	t.Helper()
	disks := map[string]*Disk{}
	for i := range in.Disks {
		disks[in.Disks[i].ID] = &in.Disks[i]
	}
	files := map[string]*File{}
	for i := range in.Files {
		files[in.Files[i].Path] = &in.Files[i]
	}
	room := map[string]int64{}
	for _, d := range in.Disks {
		reserved, err := in.MinFree.Reserved(d.Capacity)
		if err != nil {
			t.Fatal(err)
		}
		room[d.ID] = d.Free - reserved
	}
	results := map[string]*Result{}
	for i := range plan.Results {
		results[plan.Results[i].Path] = &plan.Results[i]
	}
	for _, f := range in.Files {
		for _, c := range f.Copies {
			if c.State == CopyPending && !slices.Contains(results[f.Path].Cancel, c.Disk) {
				room[c.Disk] -= f.Size
			}
		}
	}
	placed := map[string]int{}
	seenReplicate := false
	jobsOn := map[string]bool{}
	for _, j := range plan.Jobs {
		f, d := files[j.Path], disks[j.Dst]
		if f == nil || d == nil {
			t.Fatalf("round %d: job %+v names what does not exist", round, j)
		}
		if !writable(d) {
			t.Errorf("round %d: job %+v on a disk that is not writable: %+v", round, j, *d)
		}
		mark := j.Path + "\x00" + j.Dst
		if jobsOn[mark] {
			t.Errorf("round %d: two jobs put %q on %q", round, j.Path, j.Dst)
		}
		jobsOn[mark] = true
		i := slices.IndexFunc(f.Copies, func(c Copy) bool { return c.Disk == j.Dst })
		switch j.Kind {
		case JobRefresh:
			if seenReplicate {
				t.Errorf("round %d: a refresh after a replica: %+v", round, plan.Jobs)
			}
			if i < 0 || f.Copies[i].State != CopyStale {
				t.Errorf("round %d: refresh %+v of a copy that is not stale", round, j)
			}
		case JobReplicate:
			seenReplicate = true
			if i >= 0 {
				t.Errorf("round %d: two copies of %q on %q", round, j.Path, j.Dst)
			}
			if f.Hold {
				t.Errorf("round %d: a replica of %q, which waits for atoll replicate", round, j.Path)
			}
		default:
			t.Errorf("round %d: job %+v of an unknown kind", round, j)
		}
		checkNames(t, round, j.Path, d)
		src := slices.IndexFunc(f.Copies, func(c Copy) bool { return c.Disk == j.Src })
		if src < 0 || !isSource(&f.Copies[src], &planDisk{Disk: disks[j.Src]}) {
			t.Errorf("round %d: job %+v reads from a copy that cannot be read", round, j)
		}
		room[j.Dst] -= f.Size
		if room[j.Dst] < 0 {
			t.Errorf("round %d: job %+v goes past min_free on %q", round, j, j.Dst)
		}
		placed[j.Path]++
	}
	checkCollisions(t, round, in, plan)
	for _, f := range in.Files {
		r := results[f.Path]
		if r == nil {
			r = &Result{Path: f.Path}
		}
		checkMarks(t, round, f, r, disks, placed[f.Path])
	}
	if !slices.IsSortedFunc(plan.Results, func(a, b Result) int {
		switch {
		case a.Path < b.Path:
			return -1
		case a.Path > b.Path:
			return 1
		default:
			return 0
		}
	}) || len(results) != len(plan.Results) {
		t.Errorf("round %d: results are not at most one per file, by path", round)
	}
	for _, r := range plan.Results {
		if r.States == nil && r.Surplus == nil && r.Back == nil && r.Cancel == nil {
			t.Errorf("round %d: a result with nothing to say: %+v", round, r)
		}
	}
}

func checkNames(t *testing.T, round int, path string, d *Disk) {
	t.Helper()
	problems, err := names.CheckPath(path, d.FS)
	if err != nil || len(problems) > 0 {
		t.Errorf("round %d: %q on %s: %v, %v", round, path, d.FS, problems, err)
	}
	fits, err := names.FitsBranch(d.Mount, d.Root, path)
	if err != nil || !fits {
		t.Errorf("round %d: %q past the limit of the branch: %v", round, path, err)
	}
}

// checkCollisions replays the jobs on each NTFS disk: none may collide with a name there.
func checkCollisions(t *testing.T, round int, in Input, plan Plan) {
	t.Helper()
	for i := range in.Disks {
		d := &in.Disks[i]
		if d.FS != names.NTFS {
			continue
		}
		target := names.NewTargetNames()
		for _, e := range in.Entries {
			if e.Disk == d.ID {
				if err := target.Add(e.Path); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, f := range in.Files {
			if slices.ContainsFunc(f.Copies, func(c Copy) bool { return c.Disk == d.ID }) {
				if err := target.Add(f.Path); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, j := range plan.Jobs {
			if j.Dst != d.ID {
				continue
			}
			found, err := target.Find(j.Path)
			if err != nil {
				t.Fatal(err)
			}
			if found != nil {
				t.Errorf("round %d: %q collides with %v on NTFS", round, j.Path, found.With)
			}
			if err := target.Add(j.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// checkMarks checks the marks of one file: never beyond its level, and never away from
// its last readable copy or its last readable user copy.
func checkMarks(t *testing.T, round int, f File, r *Result, disks map[string]*Disk, placed int) {
	t.Helper()
	counted, readable, readableUser := 0, 0, 0
	surplusReadable, surplusUser := 0, 0
	for i := range f.Copies {
		c := &f.Copies[i]
		d := &planDisk{Disk: disks[c.Disk]}
		if c.State == CopyPending && slices.Contains(r.Cancel, c.Disk) {
			continue
		}
		if !counts(c, d) {
			if slices.Contains(r.Surplus, c.Disk) || slices.Contains(r.Back, c.Disk) {
				t.Errorf("round %d: a copy that does not count has a mark: %+v, %+v", round, *c, *r)
			}
			continue
		}
		counted++
		inHand := isSource(c, d)
		user := c.CreatedBy == CreatedByUser
		if inHand {
			readable++
			if user {
				readableUser++
			}
		}
		if slices.Contains(r.Surplus, c.Disk) {
			if c.State != CopyOK {
				t.Errorf("round %d: %q on %s is %s, and marked surplus", round, f.Path, c.Disk, c.State)
			}
			counted--
			if inHand {
				surplusReadable++
				if user {
					surplusUser++
				}
			}
		}
		if slices.Contains(r.Back, c.Disk) && c.State != CopySurplus {
			t.Errorf("round %d: %q on %s is %s, and goes back to ok", round, f.Path, c.Disk, c.State)
		}
	}
	level := f.Placement.Replicas + 1
	if placed > 0 && counted+placed > level {
		t.Errorf("round %d: %q wants %d copies: %d count and %d are planned", round, f.Path, level, counted, placed)
	}
	if readable > 0 && surplusReadable == readable {
		t.Errorf("round %d: every readable copy of %q is marked surplus: %+v, %+v", round, f.Path, f.Copies, *r)
	}
	if readableUser > 0 && surplusUser == readableUser {
		t.Errorf("round %d: every readable user copy of %q is marked surplus: %+v, %+v", round, f.Path, f.Copies, *r)
	}
}

// checkStable feeds a plan back: with its jobs as pending copies, there is nothing to
// add. A plan that kept finding more would move data for nothing.
func checkStable(t *testing.T, round int, in Input, plan Plan) {
	t.Helper()
	next := in
	next.Files = slices.Clone(in.Files)
	for i := range next.Files {
		f := &next.Files[i]
		f.Copies = slices.Clone(f.Copies)
		for _, j := range plan.Jobs {
			if j.Path != f.Path {
				continue
			}
			if k := slices.IndexFunc(f.Copies, func(c Copy) bool { return c.Disk == j.Dst }); k >= 0 {
				f.Copies[k].State = CopyPending
			} else {
				f.Copies = append(f.Copies, Copy{Disk: j.Dst, State: CopyPending, CreatedBy: CreatedByAtoll, FirstSeenNS: 9})
			}
		}
	}
	second, err := MakePlan(next)
	if err != nil {
		t.Fatalf("round %d: %v", round, err)
	}
	if len(second.Jobs) > 0 {
		t.Errorf("round %d: a second plan adds %+v to %+v", round, second.Jobs, plan.Jobs)
	}
}

// With k copies of every file, a pool cannot protect more than the capacity of
// specs.md §13.1, whatever the order of the files.
func TestSubtractStops(t *testing.T) {
	for _, c := range []struct{ room, size, want int64 }{
		{10, 3, 7},
		{0, 0, 0},
		{-5, 3, -8},
		{math.MinInt64 + 3, 3, math.MinInt64},
		{math.MinInt64 + 2, 3, math.MinInt64},
		{math.MinInt64, math.MaxInt64, math.MinInt64},
	} {
		if got := subtract(c.room, c.size); got != c.want {
			t.Errorf("subtract(%d, %d) = %d, want %d", c.room, c.size, got, c.want)
		}
	}
}

func TestPlanStaysWithinCapacity(t *testing.T) {
	x := xorshift(0x2545F4914F6CDD1D)
	reached := 0
	for round := range 5000 {
		k := 1 + x.intn(3)
		minFree, err := value.ParseMinFree([]string{"10%"})
		if err != nil {
			t.Fatal(err)
		}
		in := Input{MinFree: minFree}
		var usable []int64
		room := map[string]int64{}
		for i := range 2 + x.intn(5) {
			capacity := int64(100 + x.intn(900))
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
		for i := range 40 {
			size := int64(1 + x.intn(60))
			home := in.Disks[x.intn(len(in.Disks))].ID
			if room[home] < size {
				continue
			}
			room[home] -= size
			for j := range in.Disks {
				if in.Disks[j].ID == home {
					in.Disks[j].Free -= size
				}
			}
			in.Files = append(in.Files, File{
				Path: fmt.Sprintf("f%d", i), Size: size,
				Placement: &Placement{Replicas: k - 1, TagsMode: ModePrefer, Spread: SpreadDomain, SpreadMode: ModePrefer, Overflow: OverflowAllow},
				Copies:    []Copy{{Disk: home, State: CopyOK, CreatedBy: CreatedByUser}},
			})
		}
		plan, err := MakePlan(in)
		if err != nil {
			t.Fatal(err)
		}
		jobs := map[string]int{}
		for _, j := range plan.Jobs {
			jobs[j.Path]++
		}
		var protected int64
		for _, f := range in.Files {
			if 1+jobs[f.Path] == k {
				protected += f.Size
				reached++
			}
		}
		limit, err := Capacity(usable, k)
		if err != nil {
			t.Fatal(err)
		}
		if protected > limit {
			t.Errorf("round %d: %d bytes at %d copies on %v, past the capacity %d", round, protected, k, usable, limit)
		}
	}
	if reached == 0 {
		t.Error("no file ever reached its level")
	}
}

// The cost of a plan for the 200,000 files of specs.md §26, with an NTFS disk to look at.
func BenchmarkPlan200k(b *testing.B) {
	minFree, err := value.ParseMinFree([]string{"5%", "100GiB"})
	if err != nil {
		b.Fatal(err)
	}
	const tb = int64(1e12)
	in := Input{MinFree: minFree, Disks: []Disk{
		{ID: "t1", Position: 0, State: DiskOnline, Role: RoleData, Capacity: 8 * tb, Free: 4 * tb, FS: names.Ext4, Domain: "a", Mount: "/mnt/t1", Root: "media"},
		{ID: "t2", Position: 1, State: DiskOnline, Role: RoleData, Capacity: 8 * tb, Free: 4 * tb, FS: names.Ext4, Domain: "a", Mount: "/mnt/t2", Root: "media"},
		{ID: "u1", Position: 2, State: DiskStandby, Role: RoleData, Capacity: 12 * tb, Free: 6 * tb, FS: names.NTFS, Domain: "b", Mount: "/mnt/u1", Root: "media"},
		{ID: "u2", Position: 3, State: DiskStandby, Role: RoleData, Capacity: 12 * tb, Free: 6 * tb, FS: names.NTFS, Domain: "b", Mount: "/mnt/u2", Root: "media"},
	}}
	place := &Placement{Replicas: 1, TagsMode: ModePrefer, Spread: SpreadDomain, SpreadMode: ModePrefer, Overflow: OverflowAllow}
	for i := range 200000 {
		home := in.Disks[i%4].ID
		path := fmt.Sprintf("Library/Show %04d/Season %02d/Show %04d - S%02dE%04d - A Title.mkv", i/500, i/100%10, i/500, i/100%10, i)
		f := File{Path: path, Size: int64(1+i%5000) * 1000000, Placement: place, Copies: []Copy{{Disk: home, State: CopyOK, CreatedBy: CreatedByUser}}}
		// Half the library already has its replica, as on a pool in service.
		if i%2 == 0 {
			f.Copies = append(f.Copies, Copy{Disk: in.Disks[(i+2)%4].ID, State: CopyOK, CreatedBy: CreatedByAtoll})
		}
		in.Files = append(in.Files, f)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := MakePlan(in); err != nil {
			b.Fatal(err)
		}
	}
}
