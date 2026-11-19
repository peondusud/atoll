package solver

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"

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
	// Physical names the physical disk it lives on when another disk of the pool lives there too, and is empty
	// otherwise: the plan counts such disks as one, so that no two copies of a file are on one physical disk (R-06).
	Physical string
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
	// Unverified says that it was not read back since it last changed (copies.verified_ns NULL): no transfer of the
	// SSD cache leans on it (specs.md §19).
	Unverified bool
}

// Residency is what the SSD cache says of a file with a copy on a cache disk (specs.md §19).
type Residency string

// The residencies of a file. A file with none has its copies on cache disks count as any other copy.
const (
	CacheNone Residency = ""
	// CacheResident: it stays on the cache. Its copies there are kept, toward no level, and it gets only the copies
	// cache.mirror and cache.replicate_early ask for.
	CacheResident Residency = "resident"
	// CacheTransfer: it leaves the cache. Its copies there count for nothing and stay sources; once the data disks
	// hold it, each gets a transfer job.
	CacheTransfer Residency = "transfer"
)

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
	// Colocate is none, folder, season or show: the copies of the files of one folder, or of one show, go to the
	// same disks (specs.md §12.4). Empty is none.
	Colocate string
}

// EqualizePercent is the value of balance.equalize that places each copy by the most free share (specs.md §12.4).
const EqualizePercent = "percent"

// The values of colocate, and of placement.new_files (specs.md §12.4).
const (
	ColocateNone   = "none"
	ColocateFolder = "folder"
	ColocateSeason = "season"
	ColocateShow   = "show"
	NewMostFree    = "most-free"
	NewOrdered     = "ordered"
	NewPercent     = "percent"
)

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
	// Cache is what the SSD cache says of it, which needs a copy on a cache disk (specs.md §19).
	Cache Residency
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
	// NewFiles is placement.new_files, most-free when empty, and FillTo placement.fill_to, what ordered fills each disk
	// to (specs.md §12.4).
	NewFiles string
	FillTo   value.Percent
	// Equalize is balance.equalize: with percent, the plan places each copy by the most free share of capacity,
	// whatever NewFiles says, which then decides only where a new file first lands (specs.md §12.4).
	Equalize string
	// Balance makes the plan of atoll balance (specs.md §13.3): an Atoll copy that breaks a strict setting of its
	// file counts for nothing, so that one is made where the setting wants it. It stays a source, and its disk still
	// holds a copy; its result names it.
	Balance bool
	// MoveOriginals makes a user copy count as one too, with Balance: the plan of atoll balance --move-originals, the
	// only way a user copy moves (specs.md §6.1, §13.3).
	MoveOriginals bool
	// Taken answers for the names a disk holds: whether the catalog has, on that disk, a name with the collision key
	// of path and other bytes (specs.md §15.7), be it a file with a copy there, the name a copy still sits at, or an
	// entry, a folder as any other. The plan asks it of an NTFS disk alone, of each folder above a file, then of the
	// file: it holds no name of a disk that the catalog can answer for, as a disk has hundreds of thousands (§26).
	// What is not on a disk yet, it holds itself. Nil answers that nothing is taken.
	Taken func(disk, path string) (bool, error)
	// CacheMirror and CacheEarly are cache.mirror and cache.replicate_early: what a file resident on the SSD cache
	// gets, a copy on a second cache disk and one on a data disk (specs.md §19).
	CacheMirror, CacheEarly bool
}

// JobKind is a kind of job (specs.md §9).
type JobKind string

// The kinds of job a plan gives.
const (
	JobRefresh   JobKind = "refresh"
	JobReplicate JobKind = "replicate"
	// JobTransfer removes a file's copy on the cache disk Src, once the data disks hold it (specs.md §19). It has no
	// Dst.
	JobTransfer JobKind = "transfer"
)

// Job is one copy to make, or one to remove from the SSD cache.
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
	used    int64  // what the disk holds, with what the plan placed: its capacity less its free space, and more
	folders map[string]bool
	others  map[string]bool    // symlinks and ignored files
	old     map[string]bool    // the names copies that have not followed a rename still sit at, made when there is one
	names   *names.TargetNames // on NTFS, the names that are not on the disk yet: ownNames
	asked   map[string]bool    // on NTFS, what the catalog answered of each folder asked
	last    *planFile          // on NTFS, the file last asked of, and what the catalog answered
	taken   bool
}

// sharesWith says whether two disks live on one physical disk (R-06).
func (d *planDisk) sharesWith(other *planDisk) bool {
	return d != other && d.Physical != "" && d.Physical == other.Physical
}

// onPhysicalOf says whether a file holds a copy, or has one planned, on a disk that lives on one physical disk with d.
func (f *planFile) onPhysicalOf(d *planDisk) bool {
	return slices.ContainsFunc(f.holding, d.sharesWith)
}

func (d *planDisk) writable() bool {
	return d.Role == RoleData && (d.State == DiskOnline || d.State == DiskStandby)
}

// planFile is a file and what the plan has done to it so far.
type planFile struct {
	*File
	level    int         // the copies it wants
	want     int         // the copies on data disks it may have now: its level, but on the SSD cache (planner.want)
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
	// rare is nil for most files too: a plan of the whole pool holds a planFile for each file, all at once, and the
	// first pass of a pool of 400,000 copies holds them within its 200 MB (specs.md §26).
	rare *fileRare
}

// fileRare holds what a file needs only in a plan of atoll balance, or on the SSD cache.
type fileRare struct {
	// misplaced holds, in a plan of atoll balance, the disks of its Atoll copies that break a strict setting.
	misplaced []*planDisk
	// For a file on the SSD cache (specs.md §19): cached holds the disks of its copies on cache disks kept, of a file
	// resident; cacheReached counts its copies on cache disks that can be read, and those kept that are pending, on a
	// disk that is not missing; cacheJobs counts the copies placed on a cache disk.
	cached       []*planDisk
	cacheReached int
	cacheJobs    int
}

// more gives what few files need, made at its first need.
func (f *planFile) more() *fileRare {
	if f.rare == nil {
		f.rare = &fileRare{}
	}
	return f.rare
}

// misplacedDisks gives the disks of its misplaced copies, nil for most files.
func (f *planFile) misplacedDisks() []*planDisk {
	if f.rare == nil {
		return nil
	}
	return f.rare.misplaced
}

// cachedDisks gives the disks of its copies kept on cache disks, nil for most files.
func (f *planFile) cachedDisks() []*planDisk {
	if f.rare == nil {
		return nil
	}
	return f.rare.cached
}

// cacheCounts gives its copies on cache disks that can be read, or kept pending, and those placed there.
func (f *planFile) cacheCounts() (reached, jobs int) {
	if f.rare == nil {
		return 0, 0
	}
	return f.rare.cacheReached, f.rare.cacheJobs
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
	// taken is Input.Taken.
	taken func(disk, path string) (bool, error)
	// newFiles and fillTo are Input.NewFiles and Input.FillTo; newFiles is percent with Input.Equalize so.
	newFiles string
	fillTo   value.Percent
	// groups counts, for each group of colocate, the files with a copy on each disk, in any state, or one the plan
	// placed there (specs.md §12.4).
	groups map[string]map[*planDisk]int
	// mirror and early are Input.CacheMirror and Input.CacheEarly.
	mirror, early bool
}

func newPlanner(in Input) (*planner, error) {
	if in.Equalize == EqualizePercent {
		in.NewFiles = NewPercent
	}
	p := &planner{byID: map[string]*planDisk{}, byPath: map[string]*planFile{}, entries: in.Entries, balance: in.Balance,
		newFiles: in.NewFiles, fillTo: in.FillTo, groups: map[string]map[*planDisk]int{},
		originals: in.Balance && in.MoveOriginals, taken: in.Taken, mirror: in.CacheMirror, early: in.CacheEarly}
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
		pd := &planDisk{Disk: d, domain: domain, room: d.Free - reserved, used: d.Capacity - d.Free, folders: map[string]bool{}, others: map[string]bool{}}
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
		pf.want = p.want(pf)
		p.byPath[f.Path] = pf
		p.files = append(p.files, pf)
		if err := p.addCopies(pf); err != nil {
			return err
		}
		for _, d := range pf.holding {
			p.holds(pf, d)
		}
	}
	return nil
}

// holds counts a file's copy on a disk in its group of colocate, if it has one.
func (p *planner) holds(f *planFile, d *planDisk) {
	group := groupOf(f.Path, f.Placement.Colocate)
	if group == "" {
		return
	}
	if p.groups[group] == nil {
		p.groups[group] = map[*planDisk]int{}
	}
	p.groups[group][d]++
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
	case f.Cache != CacheNone && f.Cache != CacheResident && f.Cache != CacheTransfer:
		return fmt.Errorf("plan: %q: unknown residency %q", f.Path, f.Cache)
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
		if misplaced := p.misplaced(f); len(misplaced) > 0 {
			f.more().misplaced = misplaced
		}
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
		if f.Cache != CacheNone && d.Role == RoleCache {
			p.cacheCopy(f, c, d)
		} else if slices.Contains(f.misplacedDisks(), d) {
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
	if f.Cache != CacheNone && !slices.ContainsFunc(f.holding, func(d *planDisk) bool { return d.Role == RoleCache }) {
		return fmt.Errorf("plan: %q: on the SSD cache with no copy on a cache disk", f.Path)
	}
	f.source = source
	f.keep(counted)
	return nil
}

// cacheCopy reads a copy on a cache disk of a file on the SSD cache (specs.md §19). A file resident keeps it, toward no
// level and never as surplus, a pending one only with cache.mirror, on a cache disk online or standby; a file that
// leaves counts it for nothing, and cancels it when pending. Either way, one that can be read counts in its states.
func (p *planner) cacheCopy(f *planFile, c *Copy, d *planDisk) {
	writable := d.State == DiskOnline || d.State == DiskStandby
	kept := f.Cache == CacheResident && (counts(c, d) || c.State == CopyPending && !f.Leaving && p.mirror && writable)
	switch {
	case kept:
		f.more().cached = append(f.more().cached, d)
		switch c.State {
		case CopySurplus:
			f.mark().back = append(f.mark().back, d)
		case CopyPending:
			f.pending++
		case CopyOK, CopyStale, CopyCorrupt, CopyUnavailable:
		}
		if d.State != DiskMissing {
			f.more().cacheReached++
		}
	case c.State == CopyPending:
		f.mark().cancel = append(f.mark().cancel, d)
	case counts(c, d) && d.State != DiskMissing:
		f.more().cacheReached++
	}
}

// cacheWant is how many copies on cache disks a file resident on the SSD cache may have: one, and a second with
// cache.mirror, never past its level (specs.md §19).
func (p *planner) cacheWant(f *planFile) int {
	if p.mirror {
		return min(2, f.level)
	}
	return min(1, f.level)
}

// want is how many copies on data disks a file may have now: its level, but for a file resident on the SSD cache, one
// with cache.replicate_early within its level, after what cacheWant leaves, and none otherwise (specs.md §19).
func (p *planner) want(f *planFile) int {
	if f.Cache != CacheResident {
		return f.level
	}
	if !p.early {
		return 0
	}
	return max(0, min(1, f.level-p.cacheWant(f)))
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
	// A cache disk, an SSD, wakes nothing (specs.md §13.4).
	if (a.Role == RoleCache) != (b.Role == RoleCache) {
		return a.Role == RoleCache
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

// addParents records the folders that something at path needs on its disk, the longest first: a folder recorded has
// its own parents recorded, so the first one found ends it. Hashing each of them for each path cost the cube of the
// depth for a chain of nested folders, where this costs its square (specs.md §15.8).
func addParents(d *planDisk, path string) {
	for at := strings.LastIndexByte(path, '/'); at > 0; at = strings.LastIndexByte(path[:at], '/') {
		if d.folders[path[:at]] {
			return
		}
		d.folders[path[:at]] = true
	}
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
			if len(f.counted) < f.level && !slices.Contains(f.covered, c.disk.domain) && !slices.ContainsFunc(f.counted, c.disk.sharesWith) {
				take(c.disk)
			}
		}
	}
	// A copy beside one kept on the same physical disk counts for nothing (R-06).
	besideKept := func(d *planDisk) bool { return slices.ContainsFunc(f.counted, d.sharesWith) }
	for _, c := range byRank {
		if len(f.counted) < f.level && !slices.Contains(f.counted, c.disk) && !besideKept(c.disk) {
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
		d := p.byID[f.Copies[i].Disk]
		// A resident file's second copy on the SSD cache holds its room as any (specs.md §19).
		if f.Copies[i].State == CopyPending && (slices.Contains(f.counted, d) || slices.Contains(f.cachedDisks(), d)) {
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
		f.pending--
		f.mark().cancel = append(f.mark().cancel, d)
		if slices.Contains(f.cachedDisks(), d) {
			f.rare.cached = slices.DeleteFunc(f.rare.cached, func(o *planDisk) bool { return o == d })
			f.rare.cacheReached--
			continue
		}
		canceled = true
		f.reached--
		f.counted = slices.DeleteFunc(f.counted, func(o *planDisk) bool { return o == d })
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
		// Its parents first: a folder recorded has its own (addParents).
		addParents(d, e.Path)
		switch e.Kind {
		case EntryDir:
			d.folders[e.Path] = true
		case EntrySymlink, EntryIgnored:
			d.others[e.Path] = true
		default:
			return fmt.Errorf("plan: %q: unknown kind of entry %q", e.Path, e.Kind)
		}
	}
	return nil
}

// ownNames returns the names an NTFS disk is to hold that the catalog cannot answer for (Input.Taken), gathered the
// first time a file looks there: most files are at their level, and a plan with nothing to place needs none. The
// catalog answers by the key of a name it has on the disk, a folder as any other, so what is left is what is not there
// yet: the folders above a copy that is pending or away, which no look found; the name a copy is to take while it
// still sits at the one it had before a rename, and the folders above it; and whatever the plan places there, as
// placing it looked here. Those are few, where the names of a disk are hundreds of thousands (specs.md §26).
func (p *planner) ownNames(d *planDisk) (*names.TargetNames, error) {
	if d.names != nil {
		return d.names, nil
	}
	t := names.NewTargetNames()
	for _, f := range p.files {
		for i := range f.Copies {
			c := &f.Copies[i]
			if c.Disk != d.ID {
				continue
			}
			name := ""
			switch {
			case c.OldPath != "":
				name = f.Path
			case c.State == CopyPending || c.Away:
				// The catalog answers for the copy's own name, of which it has a row.
				if slash := strings.LastIndexByte(f.Path, '/'); slash > 0 {
					name = f.Path[:slash]
				}
			}
			if name == "" {
				continue
			}
			if err := t.Add(name); err != nil {
				return nil, fmt.Errorf("plan: %w", err)
			}
		}
	}
	d.names = t
	return t, nil
}

// takenOn asks the catalog whether an NTFS disk holds a name that collides with a file's: each folder above it, asked
// once in a plan however many files it holds, then the file, asked once however many times a disk is looked for it.
func (p *planner) takenOn(d *planDisk, f *planFile) (bool, error) {
	if p.taken == nil {
		return false, nil
	}
	if d.last == f {
		return d.taken, nil
	}
	taken := false
	var err error
	parents(f.Path, func(folder string) bool {
		answer, asked := d.asked[folder]
		if !asked {
			if answer, err = p.taken(d.ID, folder); err != nil {
				return false
			}
			if d.asked == nil {
				d.asked = map[string]bool{}
			}
			d.asked[folder] = answer
		}
		taken = answer
		return !taken
	})
	if err == nil && !taken {
		taken, err = p.taken(d.ID, f.Path)
	}
	if err != nil {
		return false, fmt.Errorf("plan: %w", err)
	}
	d.last, d.taken = f, taken
	return taken, nil
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
	return f.want - len(f.counted) - f.jobs
}

func (f *planFile) place(d *planDisk) {
	f.jobs++
	d.room = subtract(d.room, f.Size)
	d.used += f.Size
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
	var refreshed []*planDisk
	for _, d := range stale {
		// A stale copy beside one that counts, or one refreshed, on one physical disk would count for nothing (R-06).
		if slices.ContainsFunc(f.counted, d.sharesWith) || slices.ContainsFunc(refreshed, d.sharesWith) {
			continue
		}
		refusal, err := p.checkName(f, d)
		if err != nil {
			return nil, err
		}
		f.stale.add(refusal)
		if !refusal.any() && f.missing() > 0 && f.Size <= d.room {
			f.place(d)
			refreshed = append(refreshed, d)
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
	var err error
	if f.Cache == CacheResident {
		if jobs, err = p.mirrorCopy(f, jobs); err != nil {
			return nil, err
		}
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
		if err := p.takePlace(f, d); err != nil {
			return nil, err
		}
		jobs = append(jobs, Job{Kind: JobReplicate, Path: f.Path, Src: f.source.ID, Dst: d.ID})
	}
	if f.Cache == CacheTransfer {
		jobs = p.transfers(f, jobs)
	}
	return jobs, nil
}

// takePlace records a copy placed on a disk: the disk holds it, in its group of colocate, with its folders and name.
func (p *planner) takePlace(f *planFile, d *planDisk) error {
	f.holding = append(f.holding, d)
	p.holds(f, d)
	addParents(d, f.Path)
	if d.names != nil {
		if err := d.names.Add(f.Path); err != nil {
			return fmt.Errorf("plan: %w", err)
		}
	}
	return nil
}

// mirrorCopy gives a file resident on the SSD cache its copy on a second cache disk, with cache.mirror (specs.md §19):
// a cache disk online or standby, with no copy of the file, sharing no physical disk with one that has, with room for
// it, a name it takes and its path free; the most room, then by position.
func (p *planner) mirrorCopy(f *planFile, jobs []Job) ([]Job, error) {
	// A second copy, never a first: a file whose copy on the cache does not count is no better for one more there.
	if !p.mirror || len(f.cachedDisks()) == 0 {
		return jobs, nil
	}
	r := f.rare
	// Never past its level, all its copies counted: one already on the data disks may leave no room for it.
	for len(r.cached)+r.cacheJobs < p.cacheWant(f) && len(f.counted)+f.jobs+len(r.cached)+r.cacheJobs < f.level {
		var best *planDisk
		for _, d := range p.disks {
			if d.Role != RoleCache || d.State != DiskOnline && d.State != DiskStandby || slices.Contains(f.holding, d) ||
				f.onPhysicalOf(d) || f.Size > d.room {
				continue
			}
			refusal, err := p.checkName(f, d)
			if err != nil {
				return nil, err
			}
			if refusal.any() || !p.pathFree(f, d) {
				continue
			}
			if best == nil || cmp.Or(cmp.Compare(best.room, d.room), cmp.Compare(d.Position, best.Position)) < 0 {
				best = d
			}
		}
		if best == nil {
			break
		}
		r.cacheJobs++
		best.room = subtract(best.room, f.Size)
		best.used += f.Size
		if err := p.takePlace(f, best); err != nil {
			return nil, err
		}
		jobs = append(jobs, Job{Kind: JobReplicate, Path: f.Path, Src: f.source.ID, Dst: best.ID})
	}
	return jobs, nil
}

// transfers gives each copy of a file leaving the SSD cache its transfer job (specs.md §19), once the copies that count
// reach its level and can be leaned on: each ok, verified, at its path, on a disk online, standby or readonly, with no
// job and no pending copy. A copy on a cache disk goes when it is ok or surplus, at its path, on a disk online or
// standby; by position.
func (p *planner) transfers(f *planFile, jobs []Job) []Job {
	if f.jobs > 0 || f.pending > 0 || len(f.counted) < f.level {
		return jobs
	}
	var from []*planDisk
	for i := range f.Copies {
		c := &f.Copies[i]
		d := p.byID[c.Disk]
		up := d.State == DiskOnline || d.State == DiskStandby
		if slices.Contains(f.counted, d) && (c.State != CopyOK || c.Unverified || c.Away || c.OldPath != "" || !up && d.State != DiskReadOnly) {
			return jobs
		}
		if d.Role == RoleCache && (c.State == CopyOK || c.State == CopySurplus) && !c.Away && c.OldPath == "" && up {
			from = append(from, d)
		}
	}
	slices.SortFunc(from, func(a, b *planDisk) int { return cmp.Compare(a.Position, b.Position) })
	for _, d := range from {
		jobs = append(jobs, Job{Kind: JobTransfer, Path: f.Path, Src: d.ID})
	}
	return jobs
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
		// A copy on the SSD cache of a file there is where the cache wants it: it leaves by its transfer (specs.md §19).
		if !counts(c, d) || c.State == CopyPending || f.Cache != CacheNone && d.Role == RoleCache {
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
	bestTogether := 0
	for _, d := range p.disks {
		if !d.writable() || slices.Contains(f.holding, d) || f.onPhysicalOf(d) {
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
		together := p.together(f, d)
		if best == nil || cmp.Or(
			boolRank(!isListed, !bestListed),
			boolRank(!wantsNew, !bestNew),
			cmp.Compare(bestTogether, together),
			p.compareNew(f, d, best),
			boolRank(d.State != DiskOnline, best.State != DiskOnline),
			cmp.Compare(d.Position, best.Position),
		) < 0 {
			best, bestListed, bestNew, bestTogether = d, isListed, wantsNew, together
		}
	}
	return best, best != nil && !bestListed, nil
}

// GroupFolder gives the folder whose files a file's copies go with (specs.md §12.4): its folder with colocate = folder
// or season, and the folder above it with show; false with none, or for a file too near the root to have it.
func GroupFolder(path, colocate string) (string, bool) {
	group := groupOf(path, colocate)
	if group == "" {
		return "", false
	}
	_, folder, _ := strings.Cut(group, "\x00")
	return folder, true
}

// groupOf gives the group a file's copies go with: GroupFolder's, after colocate, so that two settings never share
// one; "" for none.
func groupOf(path, colocate string) string {
	levels := 0
	switch colocate {
	case ColocateFolder, ColocateSeason:
		levels = 1
	case ColocateShow:
		levels = 2
	}
	group := path
	for range levels {
		i := strings.LastIndexByte(group, '/')
		if i < 0 {
			return ""
		}
		group = group[:i]
	}
	if levels == 0 {
		return ""
	}
	return colocate + "\x00" + group
}

// together says how many other files of a file's group hold a copy on a disk, in any state, or have one the plan
// placed there: with colocate, the disk that holds the most of them is the best (specs.md §12.4).
func (p *planner) together(f *planFile, d *planDisk) int {
	// A candidate holds no copy of the file itself: what the group counts there is the others'.
	group := groupOf(f.Path, f.Placement.Colocate)
	if group == "" {
		return 0
	}
	return p.groups[group][d]
}

// compareNew orders two candidates as placement.new_files does, d before best when below 0.
func (p *planner) compareNew(f *planFile, d, best *planDisk) int {
	return CompareNew(p.newFiles, p.fillTo, f.Size, Fill{Room: d.room, Used: d.used, Capacity: d.Capacity, Position: d.Position},
		Fill{Room: best.room, Used: best.used, Capacity: best.Capacity, Position: best.Position})
}

// Fill is what placement.new_files weighs of a disk: the room it has after min_free, what it holds, its capacity and
// its position.
type Fill struct {
	Room, Used, Capacity int64
	Position             int
}

// CompareNew orders two disks for something of a size as placement.new_files does (specs.md §12.4), a before b when
// below 0: most-free, the most room after min_free; ordered, a disk below fill_to once it holds that size, the lowest
// position first, then the most room; percent, the most room as a share of the disk's capacity, compared exactly.
func CompareNew(newFiles string, fillTo value.Percent, size int64, a, b Fill) int {
	switch newFiles {
	case NewOrdered:
		aBelow, bBelow := !fillTo.Exceeded(a.Used+size, a.Capacity), !fillTo.Exceeded(b.Used+size, b.Capacity)
		if c := boolRank(!aBelow, !bBelow); c != 0 || aBelow {
			return cmp.Or(c, cmp.Compare(a.Position, b.Position))
		}
	case NewPercent:
		return compareShares(b.Room, b.Capacity, a.Room, a.Capacity)
	}
	return cmp.Compare(b.Room, a.Room)
}

// compareShares compares a/b with c/d exactly, a share with no capacity counting as none.
func compareShares(a, b, c, d int64) int {
	share := func(x, y int64) (*big.Int, *big.Int) {
		if y <= 0 || x < 0 {
			return big.NewInt(0), big.NewInt(1)
		}
		return big.NewInt(x), big.NewInt(y)
	}
	an, ad := share(a, b)
	cn, cd := share(c, d)
	return new(big.Int).Mul(an, cd).Cmp(new(big.Int).Mul(cn, ad))
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
	t, err := p.ownNames(d)
	if err != nil {
		return nameRefusal{}, err
	}
	found, err := t.Find(f.Path)
	if err != nil {
		return nameRefusal{}, fmt.Errorf("plan: %w", err)
	}
	if found != nil {
		return nameRefusal{collision: true}, nil
	}
	taken, err := p.takenOn(d, f)
	return nameRefusal{collision: taken}, err
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
	r := Result{Path: f.Path, Overflow: f.overflow, Misplaced: disksByPosition(f.misplacedDisks())}
	if f.marks != nil {
		r.Surplus = disksByPosition(f.marks.surplus)
		r.Back = disksByPosition(f.marks.back)
		r.Cancel = disksByPosition(f.marks.cancel)
	}
	below := f.missing() > 0
	// A file on the SSD cache counts its copies there that can be read: they protect it as any copy (specs.md §19).
	cacheReached, cacheJobs := f.cacheCounts()
	reached := f.reached + f.jobs + cacheJobs + cacheReached
	notCovered := f.Placement.SpreadMode == ModeStrict && p.domainsLeft(f) > 0
	waiting := f.Hold && reached < f.level
	// Resident below its level, it waits for its transfer, which brings its copies.
	resident := f.Cache == CacheResident && reached < f.level
	add := func(applies bool, s State) {
		if applies {
			r.States = append(r.States, s)
		}
	}
	add(f.source == nil, StateUnavailable)
	add(below && f.refused.incompatible || f.stale.incompatible, StateNameIncompatible)
	add(below && f.refused.collision || f.stale.collision, StateNameCollision)
	add(!waiting && !resident && (reached < f.level || notCovered), StateDegraded)
	add(f.jobs > 0 || cacheJobs > 0 || f.pending > 0 || resident, StatePending)
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
