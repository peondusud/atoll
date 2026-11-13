package solver

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/value"
)

// The values of a file's placement settings (specs.md §12.4), of a disk's role and of
// a copy's creator (specs.md §9).
const (
	ModePrefer     = "prefer"
	ModeStrict     = "strict"
	OverflowAllow  = "allow"
	OverflowDeny   = "deny"
	SpreadDomain   = "domain"
	SpreadNone     = "none"
	RoleData       = "data"
	RoleCache      = "cache"
	CreatedByUser  = "user"
	CreatedByAtoll = "atoll"
)

// Disk is one disk of the pool, as the plan sees it.
type Disk struct {
	ID        string
	Position  int // its order in the configuration
	State     DiskState
	Role      string
	Capacity  int64
	Free      int64
	FS        names.Filesystem
	Domain    string // empty when the disk forms its own domain
	Tags      []string
	Mount     string
	Root      string
	PastGrace bool // missing for longer than health.missing_grace
}

// Copy is one copy of a file.
type Copy struct {
	Disk        string
	State       CopyState
	CreatedBy   string
	FirstSeenNS int64
	OldPath     string // where it sits, when it has not followed a rename yet (specs.md §15.12)
	// Away says that its last look did not find it at its name: its clock runs (specs.md §15.12). It counts, and is
	// no source until its file is decided, which a tripped guard can put off for days (§13.4).
	Away bool
}

// Placement is what a file's effective policy says about where its copies go.
type Placement struct {
	Replicas   int
	Priority   int
	Tags       []string
	TagsMode   string
	Domains    []string
	Spread     string
	SpreadMode string
	Overflow   string
}

// File is one managed file of the pool. Hold says it waits for atoll replicate, and Leaving that its deletion is
// decided and waits for a trash job: it gets no job and keeps no pending copy, as a copy made now would end its
// deletion (specs.md §13.4). The files of one rule can share a Placement, which the plan does not change.
type File struct {
	Path      string
	Size      int64
	Placement *Placement
	Hold      bool
	Leaving   bool
	Copies    []Copy
}

// EntryKind is what an entry on a disk is, other than a managed file.
type EntryKind string

// The kinds of entry (specs.md §9).
const (
	EntryDir     EntryKind = "dir"
	EntrySymlink EntryKind = "symlink"
	EntryIgnored EntryKind = "ignored"
)

// Entry is a folder, symlink or ignored file on one disk.
type Entry struct {
	Disk string
	Path string
	Kind EntryKind
}

// Input is everything a plan depends on.
type Input struct {
	Disks   []Disk
	Files   []File
	Entries []Entry
	MinFree value.MinFree
	// Balance makes the plan of atoll balance (specs.md §13.3): an Atoll copy that breaks a strict setting of its
	// file counts for nothing, so that one is made where the setting wants it. It stays a source, and its disk still
	// holds a copy; its result names it.
	Balance bool
	// MoveOriginals makes a user copy count as one too, with Balance: the plan of atoll balance --move-originals, the
	// only way a user copy moves (specs.md §6.1, §13.3).
	MoveOriginals bool
}

// JobKind is a kind of job (specs.md §9).
type JobKind string

// The kinds of job a plan gives.
const (
	JobRefresh   JobKind = "refresh"
	JobReplicate JobKind = "replicate"
)

// Job is one copy to make.
type Job struct {
	Kind JobKind
	Path string
	Src  string
	Dst  string
}

// Result is what the plan says about one file. A file with no result has the state ok
// and keeps its marks.
type Result struct {
	Path     string
	States   []State  // in the order of specs.md §4; give them to ShownState
	Surplus  []string // disks whose copy, ok so far, becomes surplus
	Back     []string // disks whose copy, surplus so far, goes back to ok
	Cancel   []string // disks whose pending copy is canceled
	Overflow bool     // a copy went to a disk the rule did not list
	// Misplaced holds, in a plan of atoll balance, the disks whose Atoll copy breaks a strict setting of the file.
	Misplaced []string
}

// Plan is a placement plan (specs.md §13.4).
type Plan struct {
	Jobs    []Job    // in plan order, the refreshes first
	Results []Result // by path in byte order
}

// MakePlan computes the placement plan (specs.md §13.4). It is pure and deterministic:
// the order of the lists in the input does not matter. An error means that the input
// is not valid.
func MakePlan(in Input) (Plan, error) {
	return makePlan(in, nil)
}

// PlanResults gives the results of the plan MakePlan makes, and no job. A caller that runs no job then holds none: at
// 200,000 files each with a copy to make, the list weighs about 13 MiB, and more while it grows (specs.md §26).
func PlanResults(in Input) ([]Result, error) {
	return PlanEach(in, func(int, []Job) error { return nil })
}

// PlanEach gives the results of the plan MakePlan makes, and hands its jobs to each as they are made, with the
// place in in.Files of the file they are of. It keeps none of them: the jobs of one file, of one kind, come in one
// call, in the order of MakePlan's list, and the slice is reused after the call. An error from each ends the plan
// with that error.
func PlanEach(in Input, each func(file int, jobs []Job) error) ([]Result, error) {
	plan, err := makePlan(in, each)
	if err != nil {
		return nil, err
	}
	return plan.Results, nil
}

// makePlan makes the plan. With each, every file's jobs go to it once made, and are not kept: what a job does to
// the disks, the planner has recorded already.
func makePlan(in Input, each func(file int, jobs []Job) error) (Plan, error) {
	p, err := newPlanner(in)
	if err != nil {
		return Plan{}, err
	}
	order := slices.Clone(p.files)
	slices.SortFunc(order, compareFiles)
	// The pending copies first, in the order of the files: what each keeps is no longer there for the next.
	for _, f := range order {
		p.holdRoom(f)
	}
	var plan Plan
	give := func(f *planFile) error {
		if each == nil || len(plan.Jobs) == 0 {
			return nil
		}
		err := each(int(f.index), plan.Jobs)
		plan.Jobs = plan.Jobs[:0]
		return err
	}
	for _, f := range order {
		if plan.Jobs, err = p.refresh(f, plan.Jobs); err != nil {
			return Plan{}, err
		}
		if err := give(f); err != nil {
			return Plan{}, err
		}
	}
	for _, f := range order {
		if plan.Jobs, err = p.create(f, plan.Jobs); err != nil {
			return Plan{}, err
		}
		if err := give(f); err != nil {
			return Plan{}, err
		}
	}
	// Counted first: grown one by one, a result for each of 200,000 files is for a moment twice its size.
	count := 0
	for _, f := range p.files {
		if r := p.result(f); r.States != nil || f.marks != nil || r.Misplaced != nil {
			count++
		}
	}
	if count > 0 {
		plan.Results = make([]Result, 0, count)
	}
	for _, f := range p.files {
		if r := p.result(f); r.States != nil || f.marks != nil || r.Misplaced != nil {
			plan.Results = append(plan.Results, r)
		}
	}
	slices.SortFunc(plan.Results, func(a, b Result) int { return cmp.Compare(a.Path, b.Path) })
	return plan, nil
}

// planDisk is a disk and what the plan has done to it so far.
type planDisk struct {
	*Disk
	domain  string // its failure domain: a disk with no domain forms its own
	room    int64  // what can still be filled: free space, minus min_free, pending and planned
	folders map[string]bool
	others  map[string]bool    // symlinks and ignored files
	old     map[string]bool    // the names copies that have not followed a rename still sit at, made when there is one
	names   *names.TargetNames // built when a file first looks there, on NTFS only
}

func (d *planDisk) writable() bool {
	return d.Role == RoleData && (d.State == DiskOnline || d.State == DiskStandby)
}

// planFile is a file and what the plan has done to it so far.
type planFile struct {
	*File
	level    int         // the copies it wants
	holding  []*planDisk // the disks with a copy of it, in any state, or one planned
	counted  []*planDisk // the disks whose copy counts and is kept
	covered  []string    // the domains its copies cover
	source   *planDisk   // the disk to read it from, or nil
	jobs     int         // jobs planned
	pending  int         // pending copies kept
	reached  int         // copies that count, on a disk that is not missing
	index    int32       // its place in the input's files
	lost     bool        // it holds a copy that does not count
	overflow bool        // a copy went to a disk the rule did not list
	refused  nameRefusal // what the searches for a disk met
	stale    nameRefusal // what a refresh met
	marks    *fileMarks  // nil for most files
	// misplaced holds, in a plan of atoll balance, the disks of its Atoll copies that break a strict setting.
	misplaced []*planDisk
}

// fileMarks holds the marks a file's copies get.
type fileMarks struct {
	surplus []*planDisk
	back    []*planDisk
	cancel  []*planDisk
}

func (f *planFile) mark() *fileMarks {
	if f.marks == nil {
		f.marks = &fileMarks{}
	}
	return f.marks
}

// nameRefusal says why a disk cannot hold a name.
type nameRefusal struct {
	incompatible bool
	collision    bool
}

func (r nameRefusal) any() bool {
	return r.incompatible || r.collision
}

func (r *nameRefusal) add(other nameRefusal) {
	r.incompatible = r.incompatible || other.incompatible
	r.collision = r.collision || other.collision
}

type planner struct {
	disks   []*planDisk
	byID    map[string]*planDisk
	files   []*planFile
	byPath  map[string]*planFile
	entries []Entry
	balance bool
	// originals says that a user copy can be misplaced too (Input.MoveOriginals).
	originals bool
}

func newPlanner(in Input) (*planner, error) {
	p := &planner{byID: map[string]*planDisk{}, byPath: map[string]*planFile{}, entries: in.Entries, balance: in.Balance,
		originals: in.Balance && in.MoveOriginals}
	if err := p.addDisks(in); err != nil {
		return nil, err
	}
	if err := p.addFiles(in); err != nil {
		return nil, err
	}
	return p, p.addEntries()
}

func (p *planner) addDisks(in Input) error {
	positions := map[int]bool{}
	for i := range in.Disks {
		d := &in.Disks[i]
		if err := checkDisk(d); err != nil {
			return err
		}
		if p.byID[d.ID] != nil || positions[d.Position] {
			return fmt.Errorf("plan: disk %q or position %d appears twice", d.ID, d.Position)
		}
		positions[d.Position] = true
		reserved, err := in.MinFree.Reserved(d.Capacity)
		if err != nil {
			return fmt.Errorf("plan: disk %q: %w", d.ID, err)
		}
		domain := "domain " + d.Domain
		if d.Domain == "" {
			domain = "disk " + d.ID
		}
		pd := &planDisk{Disk: d, domain: domain, room: d.Free - reserved, folders: map[string]bool{}, others: map[string]bool{}}
		p.byID[d.ID] = pd
		p.disks = append(p.disks, pd)
	}
	return nil
}

func checkDisk(d *Disk) error {
	if d.ID == "" {
		return errors.New("plan: a disk with no id")
	}
	if d.Free < 0 || d.Position < 0 {
		return fmt.Errorf("plan: disk %q: negative free space or position", d.ID)
	}
	switch d.State {
	case DiskOnline, DiskStandby, DiskMissing, DiskReadOnly, DiskDraining, DiskFailed, DiskRetired:
	default:
		return fmt.Errorf("plan: disk %q: unknown state %q", d.ID, d.State)
	}
	if d.Role != RoleData && d.Role != RoleCache {
		return fmt.Errorf("plan: disk %q: unknown role %q", d.ID, d.Role)
	}
	if _, err := names.ComponentReasons("x", d.FS); err != nil {
		return fmt.Errorf("plan: disk %q: %w", d.ID, err)
	}
	if _, err := names.FitsBranch(d.Mount, d.Root, "x"); err != nil {
		return fmt.Errorf("plan: disk %q: %w", d.ID, err)
	}
	return nil
}

func (p *planner) addFiles(in Input) error {
	for i := range in.Files {
		f := &in.Files[i]
		if err := checkFile(f); err != nil {
			return err
		}
		if p.byPath[f.Path] != nil {
			return fmt.Errorf("plan: path %q appears twice", f.Path)
		}
		pf := &planFile{File: f, level: f.Placement.Replicas + 1, index: int32(i)} //nolint:gosec // far fewer files than 2^31
		p.byPath[f.Path] = pf
		p.files = append(p.files, pf)
		if err := p.addCopies(pf); err != nil {
			return err
		}
	}
	return nil
}

func checkFile(f *File) error {
	if _, err := names.CheckPath(f.Path, names.Ext4); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	place := f.Placement
	if place == nil {
		return fmt.Errorf("plan: %q: no placement", f.Path)
	}
	switch {
	case f.Size < 0:
		return fmt.Errorf("plan: %q: a negative size", f.Path)
	case place.Replicas < 0 || place.Replicas > 2:
		return fmt.Errorf("plan: %q: %d replicas, want 0 to 2", f.Path, place.Replicas)
	case place.TagsMode != ModePrefer && place.TagsMode != ModeStrict:
		return fmt.Errorf("plan: %q: unknown tags_mode %q", f.Path, place.TagsMode)
	case place.SpreadMode != ModePrefer && place.SpreadMode != ModeStrict:
		return fmt.Errorf("plan: %q: unknown spread_mode %q", f.Path, place.SpreadMode)
	case place.Spread != SpreadDomain && place.Spread != SpreadNone:
		return fmt.Errorf("plan: %q: unknown spread %q", f.Path, place.Spread)
	case place.Overflow != OverflowAllow && place.Overflow != OverflowDeny:
		return fmt.Errorf("plan: %q: unknown overflow %q", f.Path, place.Overflow)
	}
	return nil
}

// addCopies reads a file's copies: which count, which it has lost, and its source. It
// then keeps the level's number of them.
func (p *planner) addCopies(f *planFile) error {
	var source *planDisk
	var counted []countedCopy
	seen := make([]*planDisk, 0, len(f.Copies))
	for i := range f.Copies {
		c := &f.Copies[i]
		d := p.byID[c.Disk]
		if d == nil {
			return fmt.Errorf("plan: %q: a copy on unknown disk %q", f.Path, c.Disk)
		}
		if slices.Contains(seen, d) {
			return fmt.Errorf("plan: %q: two copies on disk %q", f.Path, c.Disk)
		}
		seen = append(seen, d)
		if err := checkCopy(c); err != nil {
			return fmt.Errorf("plan: %q: %w", f.Path, err)
		}
	}
	if p.balance && !f.Leaving {
		f.misplaced = p.misplaced(f)
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		d := p.byID[c.Disk]
		f.holding = append(f.holding, d)
		addParents(d, f.Path)
		if c.OldPath != "" {
			// It sits at that name on its disk, whatever its file is called (specs.md §15.12): the name is taken
			// there for any other file. Taken for free, another file's copy was sent there, ended in conflict, and
			// was sent there again at each plan, with room on another disk.
			if d.old == nil {
				d.old = map[string]bool{}
			}
			d.old[c.OldPath] = true
		}
		if slices.Contains(f.misplaced, d) {
			// It counts for nothing, so that one is made where the setting wants it; it is no loss.
		} else if counts(c, d) && (!f.Leaving || c.State != CopyPending) {
			// A file leaving keeps no pending copy: the copy made would end its deletion (specs.md §13.4).
			counted = append(counted, countedCopy{c, d})
		} else {
			f.lost = true
			if c.State == CopyPending {
				f.mark().cancel = append(f.mark().cancel, d)
			}
		}
		if isSource(c, d) && (source == nil || betterSource(d, source)) {
			source = d
		}
	}
	f.source = source
	f.keep(counted)
	return nil
}

func checkCopy(c *Copy) error {
	switch c.State {
	case CopyOK, CopyPending, CopyStale, CopyCorrupt, CopyUnavailable, CopySurplus:
	default:
		return fmt.Errorf("unknown copy state %q", c.State)
	}
	if c.CreatedBy != CreatedByUser && c.CreatedBy != CreatedByAtoll {
		return fmt.Errorf("unknown created_by %q", c.CreatedBy)
	}
	return nil
}

// counts reports whether a copy counts toward the level of its file (specs.md §13.4).
func counts(c *Copy, d *planDisk) bool {
	readable := c.State == CopyOK || c.State == CopySurplus
	switch d.State {
	case DiskOnline, DiskStandby:
		return readable || c.State == CopyPending && d.writable()
	case DiskReadOnly:
		return readable
	case DiskMissing:
		return !d.PastGrace && (readable || c.State == CopyUnavailable)
	case DiskDraining, DiskFailed, DiskRetired:
		return false
	default:
		return false
	}
}

func isSource(c *Copy, d *planDisk) bool {
	if (c.State != CopyOK && c.State != CopySurplus) || c.Away {
		return false
	}
	switch d.State {
	case DiskOnline, DiskStandby, DiskReadOnly, DiskDraining:
		return true
	case DiskMissing, DiskFailed, DiskRetired:
		return false
	default:
		return false
	}
}

func betterSource(a, b *planDisk) bool {
	if (a.State == DiskOnline) != (b.State == DiskOnline) {
		return a.State == DiskOnline
	}
	return a.Position < b.Position
}

// parents calls fn for each parent folder of a path, the shortest first, until it
// returns false.
func parents(path string, fn func(parent string) bool) {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && !fn(path[:i]) {
			return
		}
	}
}

// addParents records the folders that something at path needs on its disk.
func addParents(d *planDisk, path string) {
	parents(path, func(parent string) bool {
		d.folders[parent] = true
		return true
	})
}

type countedCopy struct {
	copy *Copy
	disk *planDisk
}

// keep chooses the copies that stay when more count than the level (specs.md §13.4), and sets their marks. The room
// of a pending copy it keeps is held once every file is read: holdRoom.
func (f *planFile) keep(counted []countedCopy) {
	byRank := slices.Clone(counted)
	slices.SortFunc(byRank, func(a, b countedCopy) int { return compareKeep(a, b, f.Placement) })
	take := func(d *planDisk) {
		f.counted = append(f.counted, d)
		f.covered = appendNew(f.covered, d.domain)
	}
	if f.Placement.Spread == SpreadDomain {
		for _, c := range byRank {
			if len(f.counted) < f.level && !slices.Contains(f.covered, c.disk.domain) {
				take(c.disk)
			}
		}
	}
	for _, c := range byRank {
		if len(f.counted) < f.level && !slices.Contains(f.counted, c.disk) {
			take(c.disk)
		}
	}
	for _, c := range counted {
		kept := slices.Contains(f.counted, c.disk)
		switch {
		case kept && c.copy.State == CopySurplus:
			f.mark().back = append(f.mark().back, c.disk)
		case !kept && c.copy.State == CopyOK:
			f.mark().surplus = append(f.mark().surplus, c.disk)
		case !kept && c.copy.State == CopyPending:
			f.mark().cancel = append(f.mark().cancel, c.disk)
		case kept && c.copy.State == CopyPending:
			f.pending++
		}
		// A pending copy the plan has just canceled is no copy the file reaches: counted, a file left with one copy
		// that can be read, beside one on a disk away, was not degraded until the next plan.
		if c.disk.State != DiskMissing && (kept || c.copy.State != CopyPending) {
			f.reached++
		}
	}
}

// holdRoom takes the size of each pending copy a file keeps off its disk, by position, when the file fits in what that
// disk has left after min_free. One that does not fit is canceled, and counts no more: its file is then below its
// level, and is placed again as any other, on a disk with room, or waits there (specs.md §12.4, §13.4). Kept whatever
// room was left, a copy queued while there was room was written later into the reserve of min_free.
func (p *planner) holdRoom(f *planFile) {
	if f.pending == 0 {
		return
	}
	var kept []*planDisk
	for i := range f.Copies {
		if d := p.byID[f.Copies[i].Disk]; f.Copies[i].State == CopyPending && slices.Contains(f.counted, d) {
			kept = append(kept, d)
		}
	}
	slices.SortFunc(kept, func(a, b *planDisk) int { return cmp.Compare(a.Position, b.Position) })
	canceled := false
	for _, d := range kept {
		if f.Size <= d.room {
			d.room = subtract(d.room, f.Size)
			continue
		}
		canceled = true
		f.pending--
		f.reached--
		f.counted = slices.DeleteFunc(f.counted, func(o *planDisk) bool { return o == d })
		f.mark().cancel = append(f.mark().cancel, d)
	}
	if canceled {
		f.covered = f.covered[:0]
		for _, d := range f.counted {
			f.covered = appendNew(f.covered, d.domain)
		}
	}
}

// compareKeep orders the copies that count, the first kept first (specs.md §13.4). Where only listed disks are
// candidates, a copy on one outranks a copy of the same maker on a disk the lists no longer allow.
func compareKeep(a, b countedCopy, place *Placement) int {
	strict := onlyListed(place)
	return cmp.Or(
		boolRank(a.copy.State == CopyPending, b.copy.State == CopyPending),
		boolRank(a.disk.State == DiskMissing, b.disk.State == DiskMissing),
		boolRank(a.copy.CreatedBy != CreatedByUser, b.copy.CreatedBy != CreatedByUser),
		boolRank(strict && !listed(a.disk, place), strict && !listed(b.disk, place)),
		cmp.Compare(a.copy.FirstSeenNS, b.copy.FirstSeenNS),
		cmp.Compare(a.disk.Position, b.disk.Position),
	)
}

// boolRank orders false before true.
func boolRank(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	default:
		return 1
	}
}

// subtract takes size off room, stopping at the lowest value rather than wrapping.
func subtract(room, size int64) int64 {
	if room < math.MinInt64+size {
		return math.MinInt64
	}
	return room - size
}

func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

func (p *planner) addEntries() error {
	for i := range p.entries {
		e := &p.entries[i]
		d := p.byID[e.Disk]
		if d == nil {
			return fmt.Errorf("plan: an entry on unknown disk %q", e.Disk)
		}
		if _, err := names.CheckPath(e.Path, names.Ext4); err != nil {
			return fmt.Errorf("plan: %w", err)
		}
		switch e.Kind {
		case EntryDir:
			d.folders[e.Path] = true
		case EntrySymlink, EntryIgnored:
			d.others[e.Path] = true
		default:
			return fmt.Errorf("plan: %q: unknown kind of entry %q", e.Path, e.Kind)
		}
		addParents(d, e.Path)
	}
	return nil
}

// namesOf returns the names on an NTFS disk, gathered the first time a file looks
// there: most files are at their level, and a plan with nothing to place needs none.
// Whatever the plan has placed on this disk is in them, as placing it looked here.
func (p *planner) namesOf(d *planDisk) (*names.TargetNames, error) {
	if d.names != nil {
		return d.names, nil
	}
	t := names.NewTargetNames()
	for i := range p.entries {
		if p.entries[i].Disk == d.ID {
			if err := t.Add(p.entries[i].Path); err != nil {
				return nil, fmt.Errorf("plan: %w", err)
			}
		}
	}
	for _, f := range p.files {
		if slices.Contains(f.holding, d) {
			if err := t.Add(f.Path); err != nil {
				return nil, fmt.Errorf("plan: %w", err)
			}
		}
		// A copy that has not followed a rename is at that name on its disk, whatever the file is called.
		for i := range f.Copies {
			if c := &f.Copies[i]; c.Disk == d.ID && c.OldPath != "" {
				if err := t.Add(c.OldPath); err != nil {
					return nil, fmt.Errorf("plan: %w", err)
				}
			}
		}
	}
	d.names = t
	return t, nil
}

// compareFiles is the order of specs.md §13.4.
func compareFiles(a, b *planFile) int {
	return cmp.Or(
		cmp.Compare(b.Placement.Priority, a.Placement.Priority),
		boolRank(!a.lost, !b.lost),
		cmp.Compare(a.Size, b.Size),
		cmp.Compare(a.Path, b.Path),
	)
}

// missing is how many copies the file still needs.
func (f *planFile) missing() int {
	return f.level - len(f.counted) - f.jobs
}

func (f *planFile) place(d *planDisk) {
	f.jobs++
	d.room = subtract(d.room, f.Size)
	f.covered = appendNew(f.covered, d.domain)
}

// refresh gives the stale copies of a file their jobs, by position.
func (p *planner) refresh(f *planFile, jobs []Job) ([]Job, error) {
	if f.source == nil || f.Leaving {
		return jobs, nil
	}
	var stale []*planDisk
	for i := range f.Copies {
		if d := p.byID[f.Copies[i].Disk]; f.Copies[i].State == CopyStale && d.writable() {
			stale = append(stale, d)
		}
	}
	slices.SortFunc(stale, func(a, b *planDisk) int { return cmp.Compare(a.Position, b.Position) })
	for _, d := range stale {
		refusal, err := p.checkName(f, d)
		if err != nil {
			return nil, err
		}
		f.stale.add(refusal)
		if !refusal.any() && f.missing() > 0 && f.Size <= d.room {
			f.place(d)
			jobs = append(jobs, Job{Kind: JobRefresh, Path: f.Path, Src: f.source.ID, Dst: d.ID})
		}
	}
	return jobs, nil
}

// create gives a file the replicas it still needs, one disk at a time.
func (p *planner) create(f *planFile, jobs []Job) ([]Job, error) {
	if f.Hold || f.Leaving || f.source == nil {
		return jobs, nil
	}
	for f.missing() > 0 {
		d, overflow, err := p.pick(f)
		if err != nil {
			return nil, err
		}
		if d == nil {
			break
		}
		f.overflow = f.overflow || overflow
		f.place(d)
		f.holding = append(f.holding, d)
		addParents(d, f.Path)
		if d.names != nil {
			if err := d.names.Add(f.Path); err != nil {
				return nil, fmt.Errorf("plan: %w", err)
			}
		}
		jobs = append(jobs, Job{Kind: JobReplicate, Path: f.Path, Src: f.source.ID, Dst: d.ID})
	}
	return jobs, nil
}

// listed reports whether a disk is one the rule's lists name (specs.md §13.4).
func listed(d *planDisk, place *Placement) bool {
	if len(place.Tags) > 0 && !slices.ContainsFunc(place.Tags, func(t string) bool { return slices.Contains(d.Tags, t) }) {
		return false
	}
	return len(place.Domains) == 0 || slices.Contains(place.Domains, d.Domain)
}

func onlyListed(place *Placement) bool {
	return place.TagsMode == ModeStrict || place.Overflow == OverflowDeny
}

// domainsOf gives the failure domains a file with strict domains is to cover: those of the data disks that are not
// failed, retired or draining, and listed when only listed disks are candidates (specs.md §13.4).
func (p *planner) domainsOf(f *planFile) []string {
	var domains []string
	for _, d := range p.disks {
		if d.Role != RoleData || d.State == DiskFailed || d.State == DiskRetired || d.State == DiskDraining {
			continue
		}
		if onlyListed(f.Placement) && !listed(d, f.Placement) {
			continue
		}
		domains = appendNew(domains, d.domain)
	}
	return domains
}

// misplaced gives the disks of a file's Atoll copies that break a strict setting of its placement, for atoll balance
// (specs.md §13.3). Where only listed disks are candidates, a copy on a disk the lists do not allow. Then, with
// spread_mode strict, while the copies of a file at its level cover fewer domains than it must, copies that double up
// a domain, the youngest first, as many as domains are missing. A user copy is never one, but with --move-originals,
// nor is a copy that does not count or is pending.
func (p *planner) misplaced(f *planFile) []*planDisk {
	var out []*planDisk
	type held struct {
		c *Copy
		d *planDisk
	}
	var kept []held
	for i := range f.Copies {
		c := &f.Copies[i]
		d := p.byID[c.Disk]
		if !counts(c, d) || c.State == CopyPending {
			continue
		}
		if p.movable(c) && onlyListed(f.Placement) && !listed(d, f.Placement) {
			out = append(out, d)
			continue
		}
		kept = append(kept, held{c, d})
	}
	// Below its level, a file's next copies go to the domains it lacks, as strict spreading has them: nothing to move.
	if f.Placement.SpreadMode != ModeStrict || len(kept)+len(out) < f.level {
		return out
	}
	domains := p.domainsOf(f)
	var covered []string
	for _, h := range kept {
		if slices.Contains(domains, h.d.domain) {
			covered = appendNew(covered, h.d.domain)
		}
	}
	short := min(f.level, len(domains)) - len(covered)
	if short <= 0 {
		return out
	}
	// In each domain, the copy the plan would keep first stays; the Atoll copies after it are what can go, and with
	// --move-originals the user copies too.
	slices.SortFunc(kept, func(a, b held) int { return compareKeep(countedCopy{a.c, a.d}, countedCopy{b.c, b.d}, f.Placement) })
	var first []string
	var extra []held
	for _, h := range kept {
		if !slices.Contains(first, h.d.domain) {
			first = append(first, h.d.domain)
		} else if p.movable(h.c) {
			extra = append(extra, h)
		}
	}
	slices.SortFunc(extra, func(a, b held) int {
		return cmp.Or(cmp.Compare(b.c.FirstSeenNS, a.c.FirstSeenNS), cmp.Compare(b.d.Position, a.d.Position))
	})
	for _, h := range extra[:min(short, len(extra))] {
		out = append(out, h.d)
	}
	return out
}

// movable says whether atoll balance may move a copy: an Atoll copy, or with --move-originals any.
func (p *planner) movable(c *Copy) bool {
	return c.CreatedBy == CreatedByAtoll || p.originals
}

// domainsLeft is how many failure domains a file with strict domains has yet to cover.
func (p *planner) domainsLeft(f *planFile) int {
	domains := p.domainsOf(f)
	covered := 0
	for _, domain := range domains {
		if slices.Contains(f.covered, domain) {
			covered++
		}
	}
	return min(f.level, len(domains)) - covered
}

// pick returns the best candidate for one more copy, or nil. overflow says that it is
// not a disk the rule listed.
func (p *planner) pick(f *planFile) (best *planDisk, overflow bool, err error) {
	place := f.Placement
	mustCover := false
	if place.SpreadMode == ModeStrict {
		left := p.domainsLeft(f)
		mustCover = left > 0 && left >= f.missing()
	}
	var bestListed, bestNew bool
	for _, d := range p.disks {
		if !d.writable() || slices.Contains(f.holding, d) {
			continue
		}
		isListed := listed(d, place)
		if onlyListed(place) && !isListed {
			continue
		}
		refusal, err := p.checkName(f, d)
		if err != nil {
			return nil, false, err
		}
		f.refused.add(refusal)
		if refusal.any() || !p.pathFree(f, d) {
			continue
		}
		isNew := !slices.Contains(f.covered, d.domain)
		if mustCover && !isNew {
			continue
		}
		if f.Size > d.room {
			continue
		}
		wantsNew := place.Spread == SpreadDomain && isNew
		if best == nil || cmp.Or(
			boolRank(!isListed, !bestListed),
			boolRank(!wantsNew, !bestNew),
			cmp.Compare(best.room, d.room),
			boolRank(d.State != DiskOnline, best.State != DiskOnline),
			cmp.Compare(d.Position, best.Position),
		) < 0 {
			best, bestListed, bestNew = d, isListed, wantsNew
		}
	}
	return best, best != nil && !bestListed, nil
}

// checkName says whether a disk can hold a file's name.
func (p *planner) checkName(f *planFile, d *planDisk) (nameRefusal, error) {
	problems, err := names.CheckPath(f.Path, d.FS)
	if err != nil {
		return nameRefusal{}, fmt.Errorf("plan: %w", err)
	}
	fits, err := names.FitsBranch(d.Mount, d.Root, f.Path)
	if err != nil {
		return nameRefusal{}, fmt.Errorf("plan: %w", err)
	}
	if len(problems) > 0 || !fits {
		return nameRefusal{incompatible: true}, nil
	}
	if d.FS != names.NTFS {
		return nameRefusal{}, nil
	}
	t, err := p.namesOf(d)
	if err != nil {
		return nameRefusal{}, err
	}
	found, err := t.Find(f.Path)
	if err != nil {
		return nameRefusal{}, fmt.Errorf("plan: %w", err)
	}
	return nameRefusal{collision: found != nil}, nil
}

// pathFree reports whether nothing on the disk stands in the way of a path.
func (p *planner) pathFree(f *planFile, d *planDisk) bool {
	if d.folders[f.Path] || d.others[f.Path] || d.old[f.Path] {
		return false
	}
	free := true
	parents(f.Path, func(parent string) bool {
		other := p.byPath[parent]
		free = !d.others[parent] && !d.old[parent] && (other == nil || !slices.Contains(other.holding, d))
		return free
	})
	return free
}

// result gives the states and marks of a file once the plan is made.
func (p *planner) result(f *planFile) Result {
	r := Result{Path: f.Path, Overflow: f.overflow, Misplaced: disksByPosition(f.misplaced)}
	if f.marks != nil {
		r.Surplus = disksByPosition(f.marks.surplus)
		r.Back = disksByPosition(f.marks.back)
		r.Cancel = disksByPosition(f.marks.cancel)
	}
	below := f.missing() > 0
	reached := f.reached + f.jobs
	notCovered := f.Placement.SpreadMode == ModeStrict && p.domainsLeft(f) > 0
	waiting := f.Hold && reached < f.level
	add := func(applies bool, s State) {
		if applies {
			r.States = append(r.States, s)
		}
	}
	add(f.source == nil, StateUnavailable)
	add(below && f.refused.incompatible || f.stale.incompatible, StateNameIncompatible)
	add(below && f.refused.collision || f.stale.collision, StateNameCollision)
	add(!waiting && (reached < f.level || notCovered), StateDegraded)
	add(f.jobs > 0 || f.pending > 0, StatePending)
	add(f.Placement.Replicas == 0 || waiting, StateUnprotected)
	return r
}

func disksByPosition(list []*planDisk) []string {
	if len(list) == 0 {
		return nil
	}
	sorted := slices.Clone(list)
	slices.SortFunc(sorted, func(a, b *planDisk) int { return cmp.Compare(a.Position, b.Position) })
	out := make([]string, 0, len(sorted))
	for _, d := range sorted {
		out = append(out, d.ID)
	}
	return out
}
