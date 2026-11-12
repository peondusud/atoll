package watch

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
)

// Options says what a scan does (specs.md §25).
type Options struct {
	Disk string // the one disk to walk; every disk when empty
	Full bool   // also read every copy
}

// Report is what a scan says about one disk. Reason and Message are in the Verdict.
type Report struct {
	ID          string
	Verdict     disks.Verdict
	Walked      bool
	Files       int
	Bytes       int64
	Folders     int
	Symlinks    int
	Ignored     int
	New         int
	Known       int
	Hashed      int
	Changed     []string
	Moved       []MovedCopy
	Absent      []string
	PathTaken   []string
	Unreadable  []string
	OtherMounts []string
	// Seen holds the verdict about each copy that is not as its row says, by file id, and FolderMoves
	// the folders renamed: what a pass decides on (specs.md §15.12).
	Seen        []CopySeen
	FolderMoves []FolderMove
	// UnfoundFolders holds the folders found at neither name whose rows stay for the copies below them. What their
	// rows say of a name is on its way out.
	UnfoundFolders []string
}

// MovedCopy is a copy found at a new name.
type MovedCopy struct {
	Path string
	To   string
}

// Outcome is what a scan says.
type Outcome struct {
	Disks     []Report
	Conflicts []string
}

// Partial says that a disk asked for was skipped or failed.
func (o *Outcome) Partial() bool {
	for i := range o.Disks {
		if !o.Disks[i].Walked || o.Disks[i].Verdict.State == disks.Missing {
			return true
		}
	}
	return false
}

// Scan records what the disks hold, writes nothing to them, moves nothing and starts no job
// (specs.md §15.10).
func Scan(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, opts Options) (*Outcome, error) {
	now := m.Now()
	verdicts, err := CheckDisks(ctx, db, c, m, now)
	if err != nil {
		return nil, err
	}
	out := &Outcome{Disks: []Report{}}
	for i := range c.Disks {
		d := &c.Disks[i]
		// A failed disk is never read again, so it is not walked and has no report (specs.md §18).
		if opts.Disk != "" && d.ID != opts.Disk || d.State == config.DiskFailed {
			continue
		}
		// Confirmed as a pass's look is, and every time, as a scan records what it found (specs.md §10, §15.10).
		report, err := LookAtDisk(ctx, db, c, m, d, verdicts[i], now, &Look{Write: true, Confirm: confirmPastCache(m), ConfirmEach: true})
		if err != nil {
			return nil, err
		}
		// What the look found of each copy is for a pass to decide on, and a scan reports none of it: held through
		// the plan, it is one more entry for each copy of a disk renamed whole (specs.md §26).
		report.Seen = nil
		out.Disks = append(out.Disks, *report)
	}
	if err := Settle(ctx, db, c, now); err != nil {
		return nil, err
	}
	if opts.Full {
		if err := hashCopies(ctx, db, c, m, out, now); err != nil {
			return nil, err
		}
		if err := Settle(ctx, db, c, now); err != nil {
			return nil, err
		}
	}
	for i := range out.Disks {
		sortPaths(&out.Disks[i])
	}
	if out.Conflicts, err = catalog.ConflictPaths(ctx, db); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return out, nil
}

// CheckDisks runs a mount check on every disk of the configuration, and records what it found.
func CheckDisks(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, now int64) ([]disks.Verdict, error) {
	verdicts, err := disks.CheckAll(ctx, m, c.Disks)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	if err := catalog.SyncDisks(ctx, db, diskRows(c, verdicts, m, now)); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return verdicts, nil
}

func sortPaths(r *Report) {
	for _, list := range [][]string{r.Changed, r.Absent, r.PathTaken, r.Unreadable, r.OtherMounts} {
		slices.Sort(list)
	}
	slices.SortFunc(r.Moved, func(a, b MovedCopy) int { return cmp.Compare(a.Path, b.Path) })
}

// diskRows says what to record of each disk. One that passed its check and cannot say how full it is
// counts as failed, in the verdict too, and keeps what the last check measured (specs.md §15.10). A disk the
// configuration marks failed is failed, whatever its check says, and nothing is measured on it; one it marks
// draining is draining, unless its check finds it missing (specs.md §9, §18).
func diskRows(c *config.Config, verdicts []disks.Verdict, m disks.Machine, now int64) []catalog.Disk {
	rows := make([]catalog.Disk, 0, len(c.Disks))
	for i := range c.Disks {
		d := &c.Disks[i]
		row := catalog.Disk{
			ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role,
			Position: i, Domain: d.Domain, Tags: d.Tags, State: string(verdicts[i].State),
		}
		if d.State == config.DiskFailed {
			row.State = config.DiskFailed
			rows = append(rows, row)
			continue
		}
		if verdicts[i].State != disks.Missing {
			capacity, free, err := m.Space(d.Mount)
			if err != nil {
				verdicts[i] = disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: err.Error()}
				row.State = string(disks.Missing)
			} else {
				row.Seen, row.Capacity, row.Free, row.LastSeenNS = true, capacity, free, now
				row.TimePrecisionNS = verdicts[i].TimePrecisionNS
			}
		}
		// Draining while it can be read and written: one mounted read-only is readonly, and its evacuation waits.
		if d.State == config.DiskDraining && row.State == string(disks.Online) {
			row.State = config.DiskDraining
		}
		rows = append(rows, row)
	}
	return rows
}

// Look says what a look at a disk may do besides reading it. Write says that it records what it found.
// Own and Confirm are a pass's: a scan has neither (specs.md §15.12).
type Look struct {
	Write bool
	// Planned holds the paths of operations left planned on this disk, which the reprise could not reach.
	Planned []string
	// Own gives what the disk's own trash and tmp hold of those inodes.
	Own func(d *config.Disk, inodes map[string]bool) (map[string][]solver.FileLook, error)
	// Confirm is asked before anything is concluded from an absence, or with ConfirmEach before anything a walk found is
	// written, as a scan does. It says false for a disk that fails.
	Confirm     func(ctx context.Context, d *config.Disk) (bool, error)
	ConfirmEach bool
}

// Compare walks each disk not failed and compares it with the catalog, as a scan would, and writes nothing, to the
// catalog or to a disk: atoll catalog verify (specs.md §9, §25). Each disk is confirmed past the cache before what its
// walk found is compared, as a scan's is.
func Compare(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine) (*Outcome, error) {
	now := m.Now()
	verdicts, err := disks.CheckAll(ctx, m, c.Disks)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	out := &Outcome{Disks: []Report{}}
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed {
			continue
		}
		report, err := LookAtDisk(ctx, db, c, m, d, verdicts[i], now, &Look{Confirm: confirmPastCache(m), ConfirmEach: true})
		if err != nil {
			return nil, err
		}
		report.Seen = nil
		sortPaths(report)
		out.Disks = append(out.Disks, *report)
	}
	return out, nil
}

// confirmPastCache confirms a disk as a session does before it writes: a mount check, then a read of its head past
// the page cache, whose identifier must be the disk's (specs.md §10).
func confirmPastCache(m disks.Machine) func(context.Context, *config.Disk) (bool, error) {
	return func(_ context.Context, d *config.Disk) (bool, error) {
		return disks.Confirm(m, d).State != disks.Missing, nil
	}
}

// LookAtDisk walks one disk and writes what it found, whole or not at all.
func LookAtDisk(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, d *config.Disk, verdict disks.Verdict, now int64, look *Look) (*Report, error) {
	r := &Report{ID: d.ID, Verdict: verdict, Changed: []string{}, Moved: []MovedCopy{}, Absent: []string{}, PathTaken: []string{}, Unreadable: []string{}, OtherMounts: []string{}}
	if verdict.State == disks.Missing {
		return r, nil
	}
	walked, err := walkRoot(ctx, m, d)
	switch {
	case errors.Is(err, ErrInterrupted):
		r.Verdict = disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: err.Error()}
		if look.Write {
			if err := catalog.SetDiskState(ctx, db, d.ID, string(disks.Missing)); err != nil {
				return nil, fmt.Errorf("catalog: %w", err)
			}
		}
		return r, nil
	case err != nil:
		return nil, err
	}
	found, err := compareWalk(ctx, db, c, d, walked, r, look, now)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if look.Confirm != nil && (look.ConfirmEach || needsConfirming(r)) {
		ok, err := look.Confirm(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		if !ok {
			if look.Write {
				if err := catalog.SetDiskState(ctx, db, d.ID, string(disks.Missing)); err != nil {
					return nil, fmt.Errorf("catalog: %w", err)
				}
			}
			return &Report{ID: d.ID, Verdict: disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: "the disk failed its confirmation"},
				Changed: []string{}, Moved: []MovedCopy{}, Absent: []string{}, PathTaken: []string{}, Unreadable: []string{}, OtherMounts: []string{}}, nil
		}
	}
	if look.Write {
		if err := catalog.WriteWalk(ctx, db, d.ID, now, found); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
	r.Walked = true
	return r, nil
}

func needsConfirming(r *Report) bool {
	return slices.ContainsFunc(r.Seen, func(s CopySeen) bool {
		return s.Verdict == solver.VerdictMoved || s.Verdict == solver.VerdictGone || s.Verdict == solver.VerdictUncertain || s.Verdict == solver.VerdictOwn
	})
}

func walkRoot(ctx context.Context, m disks.Machine, d *config.Disk) (*Walked, error) {
	root, err := m.OpenRoot(d.Mount, d.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	walked, err := Walk(ctx, root)
	// A folder only read, whose close failing changes nothing that was read from it.
	_ = root.Close()
	if err != nil {
		return walked, err
	}
	if err := withoutOwnLinks(m, d, walked); err != nil {
		return nil, err
	}
	return walked, nil
}

// withoutOwnLinks takes the link guard's links out of the count of names of each file a walk found: they are Atoll's,
// in <workdir>/links/, and no count of a file's names includes them (specs.md §17). Only a file with two names or more
// can have one, so a walk that found none spares the folder.
func withoutOwnLinks(m disks.Machine, d *config.Disk, w *Walked) error {
	if !slices.ContainsFunc(w.Files, func(f FoundFile) bool { return f.Nlink >= 2 }) {
		return nil
	}
	dir, err := m.OpenRoot(d.Mount, path.Join(d.Workdir, layout.LinksFolder))
	switch {
	case fsx.IsAbsent(err):
		return nil
	case err != nil:
		return fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Names()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	own := make(map[string]int64, len(names))
	for _, name := range names {
		st, err := dir.Stat(name)
		switch {
		case fsx.IsAbsent(err):
			continue
		case err != nil:
			return fmt.Errorf("%w: %w", ErrInterrupted, err)
		case st.Kind == fsx.KindFile:
			own[strconv.FormatUint(st.Ino, 10)]++
		}
	}
	for i := range w.Files {
		if f := &w.Files[i]; own[f.FileKey] > 0 {
			f.Nlink -= min(own[f.FileKey], f.Nlink-1)
		}
	}
	return nil
}

// compareWalk says what a walk means against what the catalog knows, and fills the report. Both
// sides are in the byte order of paths, so it is a merge. Only what is not where its row says is looked
// up, and only what no row claims is indexed for that.
func compareWalk(ctx context.Context, db *sql.DB, c *config.Config, d *config.Disk, w *Walked, r *Report, look *Look, now int64) (*catalog.Walk, error) {
	r.Unreadable = w.Unreadable
	r.OtherMounts = w.OtherMounts
	slices.SortFunc(w.Files, func(a, b FoundFile) int { return cmp.Compare(a.Path, b.Path) })
	found := func(f *FoundFile) {
		r.Files++
		r.Bytes += f.Size
	}
	cmpr := compared{notReached: pathSet(w.Unreadable, w.OtherMounts, look.Planned), planned: pathSet(look.Planned), r: r, found: found, quiet: look.Own != nil}
	next := 0
	err := catalog.EachKnownCopy(ctx, db, d.ID, func(k *catalog.KnownCopy) {
		for next < len(w.Files) && w.Files[next].Path < k.Path {
			w.Files[next].rowless = true
			cmpr.rowless = append(cmpr.rowless, &w.Files[next])
			next++
		}
		var at *FoundFile
		if next < len(w.Files) && w.Files[next].Path == k.Path {
			at = &w.Files[next]
			next++
		}
		cmpr.row(k, at)
	})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	for ; next < len(w.Files); next++ {
		w.Files[next].rowless = true
		cmpr.rowless = append(cmpr.rowless, &w.Files[next])
	}
	if err := cmpr.markStraddled(ctx, db, d, w); err != nil {
		return nil, err
	}
	out := &catalog.Walk{}
	// A name the walk could not read may be a folder renamed since it was listed, which only says so by its
	// row: this look may find the new name, and the next look otherwise.
	tops, err := moveFolders(ctx, db, d.ID, w, r, out, pathSet(w.OtherMounts, look.Planned))
	if err != nil {
		return nil, err
	}
	if err := cmpr.resolve(ctx, db, d, w, look); err != nil {
		return nil, err
	}
	out.Clocks, out.Modes = cmpr.clocks, cmpr.modes
	planned := pathSet(look.Planned)
	if err := keepPlanned(ctx, db, d.ID, pathSet(look.Planned, w.Unreadable), out); err != nil {
		return nil, err
	}
	for _, f := range cmpr.rowless {
		switch {
		case f.isTarget:
			found(f)
		case cmpr.behind[f.Path]:
			found(f)
			r.PathTaken = append(r.PathTaken, f.Path)
		case atOrBelow(planned, f.Path), f.straddled:
			found(f)
		default:
			freshFile(c, f, r, out, tops, found)
		}
	}
	for i := range w.Entries {
		e := &w.Entries[i]
		switch e.Kind {
		case solver.EntryDir:
			r.Folders++
		case solver.EntrySymlink:
			r.Symlinks++
		case solver.EntryIgnored:
			r.Ignored++
		}
		if !atOrBelow(tops, e.Path) && !atOrBelow(out.Kept, e.Path) {
			out.Entries = append(out.Entries, catalog.EntryRow{Path: e.Path, Kind: string(e.Kind), Mode: e.Mode, MtimeNS: e.MtimeNS, Target: e.Target, FileKey: e.FileKey, BtimeNS: e.BtimeNS})
		}
	}
	return out, nil
}

// keepPlanned keeps the entries at or below the names of an operation left planned, which only its reprise
// can tell of, and those a walk could not read, which it says nothing of.
func keepPlanned(ctx context.Context, db *sql.DB, disk string, planned map[string]bool, out *catalog.Walk) error {
	if len(planned) == 0 {
		return nil
	}
	if out.Kept == nil {
		out.Kept = map[string]bool{}
	}
	for path := range planned {
		out.Kept[path] = true
		below, err := catalog.EntriesBelow(ctx, db, disk, path)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		for _, p := range below {
			out.Kept[p] = true
		}
	}
	return nil
}

func freshFile(c *config.Config, f *FoundFile, r *Report, out *catalog.Walk, tops map[string]bool, found func(*FoundFile)) {
	size := f.Size
	policy := c.Policy(f.Path, &size)
	if policy.Action == rules.Ignore {
		if !atOrBelow(tops, f.Path) && !atOrBelow(out.Kept, f.Path) {
			out.Entries = append(out.Entries, catalog.EntryRow{Path: f.Path, Kind: string(solver.EntryIgnored), Mode: f.Mode, MtimeNS: f.MtimeNS})
		}
		r.Ignored++
		return
	}
	// Below a folder renamed and not yet decided, nothing gets a row: what says that a copy moved with the
	// folder is that one sits below its new name, and a new file must not pass for that. Nor in a folder made
	// again at its old name, where it would read as a copy left behind, and the folder would wait for good.
	if atOrBelow(tops, f.Path) || isStrictlyBelow(out.Kept, f.Path) {
		found(f)
		return
	}
	out.Copies = append(out.Copies, catalog.NewCopy{
		Path: f.Path, Size: f.Size, MtimeNS: f.MtimeNS, CtimeNS: f.CtimeNS, BtimeNS: f.BtimeNS, FileKey: f.FileKey, Nlink: f.Nlink, Mode: f.Mode,
		UID: f.UID, GID: f.GID,
	})
	r.New++
	found(f)
}

// pathSet makes one set of several lists of paths.
func pathSet(lists ...[]string) map[string]bool {
	set := map[string]bool{}
	for _, list := range lists {
		for _, path := range list {
			set[path] = true
		}
	}
	return set
}

// isStrictlyBelow says whether a path is below a folder in the set.
func isStrictlyBelow(set map[string]bool, path string) bool {
	at := strings.LastIndexByte(path, '/')
	return at >= 0 && atOrBelow(set, path[:at])
}

// atOrBelow says whether a path is in the set, or below a folder that is.
func atOrBelow(set map[string]bool, path string) bool {
	for len(set) > 0 {
		if set[path] {
			return true
		}
		at := strings.LastIndexByte(path, '/')
		if at < 0 {
			return false
		}
		path = path[:at]
	}
	return false
}

// Unchanged says whether a look at a copy found what its row says (specs.md §15.10).
func Unchanged(f *FoundFile, k *catalog.KnownCopy) bool {
	return k.Size.Valid && k.Size.Int64 == f.Size &&
		k.MtimeNS.Valid && k.MtimeNS.Int64 == f.MtimeNS &&
		k.CtimeNS.Valid && k.CtimeNS.Int64 == f.CtimeNS &&
		k.FileKey.Valid && k.FileKey.String == f.FileKey
}

// settled is what a file must remember while the plan runs: at 200,000 files, keeping the rows
// themselves cost more than the plan's input (specs.md §26). update is set when the file already
// changed, whatever the plan says.
type settled struct {
	id        int64
	state     string
	firstSeen int64
	update    *catalog.FileUpdate
}

// Settle gives each file its original, its policy and what the plan says, on the whole catalog.
func Settle(ctx context.Context, db *sql.DB, c *config.Config, now int64) error {
	return SettleFor(ctx, db, c, now, nil)
}

// Replicate is what atoll replicate and the daemon ask of a plan (specs.md §14.1, §14.3).
type Replicate struct {
	// Release says whether a manual file no longer waits for atoll replicate; nil releases none.
	Release func(path string) bool
	// Queue gets every copy the plan wants of a file not in conflict, in plan order, a batch at a time, before
	// anything of the plan is written: at 200,000 files, all of them at once would cost more than the plan
	// (specs.md §26). The copies of one file come together, in one batch. The batch is reused after the call,
	// so a caller copies what it keeps.
	Queue func(jobs []PlannedJob) error
	// DryRun writes nothing of the plan.
	DryRun bool
	// Balance makes the plan of atoll balance (specs.md §13.3): Misplaced gets each file with an Atoll copy that
	// breaks a strict setting, and the disks of those copies, and nothing of the plan is written, as its results are
	// not the pool's.
	Balance   bool
	Misplaced func(path string, disks []string) error
}

// PlannedJob is a job of the plan, with what queueing it needs of its file.
type PlannedJob struct {
	solver.Job
	FileID      int64
	Size        int64
	Priority    int64
	FirstSeenNS int64
}

// SettleFor is Settle, and with a Replicate, also what atoll replicate asks of the plan. A file in
// conflict is never given a job, as nothing says which version to copy (specs.md §14.1).
func SettleFor(ctx context.Context, db *sql.DB, c *config.Config, now int64, rep *Replicate) error {
	diskList, err := catalog.Disks(ctx, db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	entries, err := catalog.Entries(ctx, db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	minFree, err := c.MinFree()
	if err != nil {
		return err
	}
	grace, err := missingGraceOf(c)
	if err != nil {
		return err
	}
	input := solver.Input{MinFree: minFree, Disks: planDisks(diskList, now, grace), Entries: planEntries(entries), Balance: rep != nil && rep.Balance}
	var release func(string) bool
	if rep != nil {
		release = rep.Release
	}
	remembered, err := filesForPlan(ctx, db, c, diskList, &input, release)
	if err != nil {
		return err
	}
	var results []solver.Result
	if rep == nil {
		// No job runs here: the plan's results are all a scan or a pass keeps of it.
		results, err = solver.PlanResults(input)
	} else {
		q := &copyQueue{queue: rep.Queue, files: input.Files, remembered: remembered}
		if results, err = solver.PlanEach(input, q.add); err == nil {
			err = q.flush()
		}
	}
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if input.Balance {
		for i := range results {
			if len(results[i].Misplaced) > 0 && rep.Misplaced != nil {
				if err := rep.Misplaced(results[i].Path, results[i].Misplaced); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if rep != nil && rep.DryRun {
		return nil
	}
	changed, states, marks, err := applyResults(input.Files, remembered, results)
	if err != nil {
		return err
	}
	if err := catalog.ApplyPlan(ctx, db, now, changed, states, marks); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// copyBatch is how many copies Queue gets at a time, at least: a file's copies are never split.
const copyBatch = 1000

// copyQueue hands the replicate jobs of the plan to Queue as the plan makes them, leaving out those of a file in
// conflict.
type copyQueue struct {
	queue      func(jobs []PlannedJob) error
	files      []solver.File
	remembered []settled
	batch      []PlannedJob
}

func (q *copyQueue) add(i int, jobs []solver.Job) error {
	if q.remembered[i].state == string(solver.StateConflict) {
		return nil
	}
	for _, job := range jobs {
		if job.Kind != solver.JobReplicate {
			continue
		}
		q.batch = append(q.batch, PlannedJob{Job: job, FileID: q.remembered[i].id, Size: q.files[i].Size, Priority: int64(q.files[i].Placement.Priority), FirstSeenNS: q.remembered[i].firstSeen})
	}
	if len(q.batch) >= copyBatch {
		return q.flush()
	}
	return nil
}

func (q *copyQueue) flush() error {
	if len(q.batch) == 0 {
		return nil
	}
	err := q.queue(q.batch)
	q.batch = q.batch[:0]
	return err
}

func missingGraceOf(c *config.Config) (time.Duration, error) {
	text, _ := c.Effective()["health.missing_grace"].(string)
	grace, err := value.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("health.missing_grace: %w", err)
	}
	return grace, nil
}

func planDisks(list []catalog.Disk, now int64, grace time.Duration) []solver.Disk {
	out := make([]solver.Disk, 0, len(list))
	for i := range list {
		d := &list[i]
		out = append(out, solver.Disk{
			ID: d.ID, Position: d.Position, State: solver.DiskState(d.State), Role: d.Role,
			Capacity: d.Capacity, Free: d.Free, FS: names.Filesystem(d.FS), Domain: d.Domain, Tags: d.Tags, Mount: d.Mount, Root: d.Root,
			// The plan looks at this only for a disk that is missing. A disk never seen holds nothing.
			PastGrace: d.LastSeenNS > 0 && now-d.LastSeenNS > int64(grace),
		})
	}
	return out
}

func planEntries(list []catalog.EntryKey) []solver.Entry {
	out := make([]solver.Entry, 0, len(list))
	for i := range list {
		out = append(out, solver.Entry{Disk: list[i].Disk, Path: list[i].Path, Kind: solver.EntryKind(list[i].Kind)})
	}
	return out
}

// filesForPlan works out each file's original and policy, gives the plan what it needs of them, and
// says what else must be remembered of each, in the same order.
func filesForPlan(ctx context.Context, db *sql.DB, c *config.Config, diskList []catalog.Disk, input *solver.Input, released func(string) bool) ([]settled, error) {
	position := make(map[string]int, len(diskList))
	state := make(map[string]solver.DiskState, len(diskList))
	for i := range diskList {
		position[diskList[i].ID] = diskList[i].Position
		state[diskList[i].ID] = solver.DiskState(diskList[i].State)
	}
	placements := map[placementKey]*solver.Placement{}
	// Sized once: grown file by file, each list is for a moment both its old size and its new one.
	count, err := catalog.FileCount(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	input.Files = slices.Grow(input.Files, count)
	remembered := make([]settled, 0, count)
	reader := catalog.NewFileReader(db)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return remembered, nil
		}
		leaving, err := catalog.Leaving(ctx, db, page[0].Path, page[len(page)-1].Path)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		for i := range page {
			f := &page[i]
			update, err := settleFile(c, f, position, state)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", f.Path, err)
			}
			// A rule that now ignores a file leaves it what it was, with the pool's placement (specs.md §15.10).
			placement := placementKey{replicas: update.ReplicasTarget, priority: c.Pool.Priority, spread: c.Pool.Spread,
				spreadMode: c.Pool.SpreadMode, tagsMode: c.Pool.TagsMode, overflow: c.Pool.Overflow}
			size := update.Size
			if policy := c.Policy(f.Path, &size); policy.Action != rules.Ignore {
				placement = placementKey{replicas: policy.Replicas, priority: policy.Priority, spread: policy.Spread, spreadMode: policy.SpreadMode,
					tagsMode: policy.TagsMode, overflow: policy.Overflow, tags: joinNames(policy.Tags), domains: joinNames(policy.Domains)}
			}
			copies := make([]solver.Copy, 0, len(f.Copies))
			for j := range f.Copies {
				cp := &f.Copies[j]
				// A copy whose clock runs is no source: nothing is at its name until its file is decided (specs.md §13.4).
				plan := solver.Copy{Disk: cp.Disk, State: solver.CopyState(cp.State), CreatedBy: cp.CreatedBy, FirstSeenNS: cp.FirstSeenNS, Away: cp.GoneNS.Valid}
				if cp.OldPath != nil {
					plan.OldPath = *cp.OldPath
				}
				copies = append(copies, plan)
			}
			hold := update.Mode == "manual" && (released == nil || !released(f.Path))
			input.Files = append(input.Files, solver.File{
				Path: f.Path, Size: update.Size, Placement: placementFor(placements, placement), Hold: hold, Leaving: leaving[f.ID], Copies: copies,
			})
			entry := settled{id: f.ID, state: f.State, firstSeen: f.FirstSeenNS}
			if fileChanged(f, &update) {
				entry.update = &update
			}
			remembered = append(remembered, entry)
		}
	}
}

// settleFile gives what a file's original and policy make of its row. The state is the plan's. With
// no eligible copy it keeps its original, as it keeps that copy's size and date (specs.md §7).
func settleFile(c *config.Config, f *catalog.FileRow, position map[string]int, state map[string]solver.DiskState) (catalog.FileUpdate, error) {
	update := catalog.FileUpdate{ID: f.ID, Size: f.Size, MtimeNS: f.MtimeNS, ReplicasTarget: f.ReplicasTarget, Rule: f.Rule, Mode: f.Mode, State: f.State, Original: f.Original}
	original, err := originalOf(f, position, state)
	if err != nil {
		return update, err
	}
	if original != "" {
		update.Original = sql.NullString{String: original, Valid: true}
		for j := range f.Copies {
			if cp := &f.Copies[j]; cp.Disk == original && cp.Size.Valid && cp.MtimeNS.Valid {
				update.Size, update.MtimeNS = cp.Size.Int64, cp.MtimeNS.Int64
			}
		}
	}
	// A rule that now ignores a file leaves it what it was (specs.md §15.10).
	size := update.Size
	policy := c.Policy(f.Path, &size)
	if policy.Action != rules.Ignore {
		update.ReplicasTarget, update.Mode = policy.Replicas, policy.Mode
		update.Rule = sql.NullString{}
		if policy.Rule != nil {
			update.Rule = sql.NullString{String: policy.Rule.Label, Valid: true}
		}
	}
	return update, nil
}

// placementKey is what a file's effective policy says of where its copies go (specs.md §12.4), as a map key: its
// tags and domains joined by a NUL, which no name holds.
type placementKey struct {
	replicas, priority                     int
	spread, spreadMode, tagsMode, overflow string
	tags, domains                          string
}

func joinNames(list []string) string {
	return strings.Join(list, "\x00")
}

func splitNames(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, "\x00")
}

// placementFor gives the plan the placement of a policy, made once for all the files that share it.
func placementFor(known map[placementKey]*solver.Placement, key placementKey) *solver.Placement {
	if p, ok := known[key]; ok {
		return p
	}
	p := &solver.Placement{
		Replicas: key.replicas, Priority: key.priority, Spread: key.spread, SpreadMode: key.spreadMode, TagsMode: key.tagsMode, Overflow: key.overflow,
		Tags: splitNames(key.tags), Domains: splitNames(key.domains),
	}
	known[key] = p
	return p
}

func originalOf(f *catalog.FileRow, position map[string]int, state map[string]solver.DiskState) (string, error) {
	candidates := make([]solver.Candidate, 0, len(f.Copies))
	for i := range f.Copies {
		cp := &f.Copies[i]
		candidate := solver.Candidate{
			Disk: cp.Disk, Position: position[cp.Disk], State: solver.CopyState(cp.State), DiskState: state[cp.Disk], FirstSeenNS: cp.FirstSeenNS,
		}
		if cp.BtimeNS.Valid {
			candidate.CreationNS = &cp.BtimeNS.Int64
		}
		candidates = append(candidates, candidate)
	}
	previous := ""
	if f.State == string(solver.StateConflict) {
		previous = f.Original.String
	}
	original, err := solver.SelectOriginal(candidates, previous)
	if err != nil {
		return "", fmt.Errorf("original: %w", err)
	}
	return original, nil
}

// applyResults gives each file the state the plan says, with conflict and corrupt kept, as they
// come from elsewhere (specs.md §13.4), and keeps only what changed. Files and results are both by
// path in byte order, so it is a merge.
func applyResults(files []solver.File, remembered []settled, results []solver.Result) ([]*catalog.FileUpdate, []catalog.StateChange, []catalog.CopyMark, error) {
	// The updates are remembered already: a copy of each would hold them twice.
	var changed []*catalog.FileUpdate
	var states []catalog.StateChange
	var marks []catalog.CopyMark
	next := 0
	for i := range files {
		f, r := &files[i], &remembered[i]
		var applying []solver.State
		// Corrupt holds while a copy is: once that copy went to the trash, the file takes the state its copies give
		// it, and a conflict they still hold is found again by the next pass, from their rows (specs.md §16).
		corrupt := r.state == string(solver.StateCorrupt) && slices.ContainsFunc(f.Copies, func(c solver.Copy) bool { return c.State == solver.CopyCorrupt })
		if r.state == string(solver.StateConflict) || corrupt {
			applying = append(applying, solver.State(r.state))
		}
		if next < len(results) && results[next].Path == f.Path {
			result := &results[next]
			next++
			applying = append(applying, result.States...)
			for _, disk := range result.Surplus {
				marks = append(marks, catalog.CopyMark{FileID: r.id, Disk: disk, State: string(solver.CopySurplus)})
			}
			for _, disk := range result.Back {
				marks = append(marks, catalog.CopyMark{FileID: r.id, Disk: disk, State: string(solver.CopyOK)})
			}
			for _, disk := range result.Cancel {
				marks = append(marks, catalog.CopyMark{FileID: r.id, Disk: disk, State: catalog.MarkCanceled})
			}
		}
		shown, err := solver.ShownState(applying)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%q: %w", f.Path, err)
		}
		switch {
		case r.update != nil:
			r.update.State = string(shown)
			changed = append(changed, r.update)
		case r.state != string(shown):
			states = append(states, catalog.StateChange{ID: r.id, State: string(shown)})
		}
	}
	return changed, states, marks, nil
}

func fileChanged(f *catalog.FileRow, u *catalog.FileUpdate) bool {
	return f.Size != u.Size || f.MtimeNS != u.MtimeNS || f.ReplicasTarget != u.ReplicasTarget ||
		f.Rule != u.Rule || f.Mode != u.Mode || f.Original != u.Original
}

// hashCopies reads the copies on the disks that were walked, file after file (specs.md §15.10).
func hashCopies(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, out *Outcome, now int64) error {
	var err error
	readers := make(map[string]*diskReader, len(out.Disks))
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	for i := range out.Disks {
		report := &out.Disks[i]
		if !report.Walked {
			continue
		}
		reader := &diskReader{report: report, skip: pathSet(report.Changed, report.Absent, movedFrom(report)), notReached: pathSet(report.Unreadable, report.OtherMounts)}
		if d := diskOf(c, report.ID); d != nil {
			reader.root, err = m.OpenRoot(d.Mount, d.Root)
			if err != nil {
				reader.fail(err)
			}
		}
		readers[report.ID] = reader
	}
	order := make(map[string]int, len(c.Disks))
	for i := range c.Disks {
		order[c.Disks[i].ID] = i
	}
	files := catalog.NewFileReader(db)
	for {
		page, err := files.Next(ctx)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			if err := hashFile(ctx, db, &page[i], readers, order, now); err != nil {
				return err
			}
		}
	}
	for _, r := range readers {
		if r.failed {
			if err := catalog.SetDiskState(ctx, db, r.report.ID, string(disks.Missing)); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		}
	}
	return nil
}

// HashNew reads the copies of the files that have no hash yet, on the disks that pass their mount check, until
// the time until; it says whether it stopped there with some left (specs.md §15.13). It walks nothing: a copy
// that is no longer what its row says is left as it is, for the next pass to look at, and so is one on a disk
// that fails.
func HashNew(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, until int64) (left bool, err error) {
	now := m.Now()
	verdicts, err := disks.CheckAll(ctx, m, c.Disks)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	readers := make(map[string]*diskReader, len(c.Disks))
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	order := make(map[string]int, len(c.Disks))
	for i := range c.Disks {
		d := &c.Disks[i]
		order[d.ID] = i
		if verdicts[i].State == disks.Missing || d.State == config.DiskFailed {
			continue
		}
		reader := &diskReader{report: &Report{ID: d.ID, Verdict: verdicts[i], Walked: true}, skip: map[string]bool{}, notReached: map[string]bool{}}
		if reader.root, err = m.OpenRoot(d.Mount, d.Root); err != nil {
			reader.fail(err)
		}
		readers[d.ID] = reader
	}
	files := catalog.NewFileReader(db)
	for {
		page, err := files.NextUnhashed(ctx)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return false, nil
		}
		for i := range page {
			if m.Now() >= until {
				return true, nil
			}
			if err := hashFile(ctx, db, &page[i], readers, order, now); err != nil {
				return false, err
			}
		}
	}
}

func movedFrom(r *Report) []string {
	list := make([]string, 0, len(r.Moved))
	for i := range r.Moved {
		list = append(list, r.Moved[i].Path)
	}
	return list
}

func diskOf(c *config.Config, id string) *config.Disk {
	for i := range c.Disks {
		if c.Disks[i].ID == id {
			return &c.Disks[i]
		}
	}
	return nil
}

// diskReader reads the copies of one disk, until it stops answering.
type diskReader struct {
	report *Report
	root   fsx.Dir
	skip   map[string]bool
	// notReached holds what the walk did not get to. A path through another mount would even
	// lead to a file of that mount.
	notReached map[string]bool
	failed     bool
}

func (r *diskReader) fail(err error) {
	r.failed = true
	r.report.Verdict = disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: err.Error()}
}

func (r *diskReader) close() {
	if r.root != nil {
		// A folder only read, whose close failing changes nothing that was read from it.
		_ = r.root.Close()
	}
}

type copyHash struct {
	cp     *catalog.CopyRow
	file   [integrity.HashSize]byte
	chunks []byte
}

// ReadReplicas re-reads the copies Atoll made of the files below a path, as a full scan reads a copy, on
// the disks it may read: what atoll replicate --force does besides --now (specs.md §14.3).
func ReadReplicas(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, now int64, readable func(id string) bool, below func(string) bool) error {
	readers := make(map[string]*diskReader, len(c.Disks))
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	order := make(map[string]int, len(c.Disks))
	for i := range c.Disks {
		d := &c.Disks[i]
		order[d.ID] = i
		if !readable(d.ID) {
			continue
		}
		reader := &diskReader{report: &Report{ID: d.ID}, skip: map[string]bool{}, notReached: map[string]bool{}}
		root, err := m.OpenRoot(d.Mount, d.Root)
		if err != nil {
			reader.fail(err)
		}
		reader.root = root
		readers[d.ID] = reader
	}
	files := catalog.NewFileReader(db)
	for {
		page, err := files.Next(ctx)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			f := page[i]
			if !below(f.Path) {
				continue
			}
			f.Copies = slices.DeleteFunc(f.Copies, func(c catalog.CopyRow) bool { return c.CreatedBy != "atoll" })
			if len(f.Copies) == 0 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("replicate: %w", err)
			}
			if err := hashFile(ctx, db, &f, readers, order, now); err != nil {
				return err
			}
		}
	}
	for _, r := range readers {
		if r.failed {
			if err := catalog.SetDiskState(ctx, db, r.report.ID, string(disks.Missing)); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		}
	}
	return nil
}

func hashFile(ctx context.Context, db *sql.DB, f *catalog.FileRow, readers map[string]*diskReader, order map[string]int, now int64) error {
	copies := make([]*catalog.CopyRow, 0, len(f.Copies))
	for i := range f.Copies {
		// A pending copy has nothing on its disk to read yet (specs.md §14.1).
		if f.Copies[i].State != string(solver.CopyPending) {
			copies = append(copies, &f.Copies[i])
		}
	}
	slices.SortFunc(copies, func(a, b *catalog.CopyRow) int { return cmp.Compare(order[a.Disk], order[b.Disk]) })
	var read []copyHash
	for _, cp := range copies {
		reader := readers[cp.Disk]
		place := cp.Place(f)
		if reader == nil || reader.failed || reader.skip[place] || atOrBelow(reader.notReached, place) || carriedOutOfReach(reader.report.FolderMoves, reader.notReached, place) {
			continue
		}
		hash, ok, err := readCopy(ctx, reader, place, cp)
		if err != nil {
			return err
		}
		if ok {
			read = append(read, hash)
		}
	}
	if len(read) == 0 {
		return nil
	}
	write := catalog.HashWrite{FileID: f.ID}
	reference := f.Hash
	if reference == nil {
		pick := &read[0]
		for i := range read {
			if read[i].cp.Disk == f.Original.String {
				pick = &read[i]
			}
		}
		reference = pick.file[:]
		write.Hash, write.Chunks = reference, pick.chunks
	}
	for i := range read {
		switch {
		case bytes.Equal(read[i].file[:], reference):
			write.Verified = append(write.Verified, read[i].cp.Disk)
		case read[i].cp.VerifiedNS.Valid:
			// Whether this is a modification or corruption is for the scrub (specs.md §16). Until
			// then nothing may rely on it: prune counts verified copies as what stays.
			readers[read[i].cp.Disk].report.Changed = append(readers[read[i].cp.Disk].report.Changed, read[i].cp.Place(f))
			write.Unverified = append(write.Unverified, read[i].cp.Disk)
		default:
			write.Conflict = true
		}
	}
	if err := catalog.WriteHashes(ctx, db, now, &write); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// readCopy reads one copy to its end. It says false when that gives no hash to use: the name can
// no longer be reached, or the copy moved during the read, or the disk stopped answering. Only a
// canceled context is an error.
func readCopy(ctx context.Context, reader *diskReader, path string, cp *catalog.CopyRow) (copyHash, bool, error) {
	file, err := fsx.OpenFileAt(reader.root, path)
	switch {
	case fsx.IsUnreadableName(err):
		reader.report.Unreadable = append(reader.report.Unreadable, path)
		return copyHash{}, false, nil
	case err != nil:
		reader.fail(err)
		return copyHash{}, false, nil
	}
	// A file only read, whose close failing changes nothing that was read from it.
	defer func() { _ = file.Close() }()
	before, err := file.Info()
	if err != nil {
		reader.fail(err)
		return copyHash{}, false, nil
	}
	sum, chunks, err := integrity.HashReader(ctx, file)
	switch {
	case ctx.Err() != nil:
		return copyHash{}, false, fmt.Errorf("scan: %w", ctx.Err())
	case err != nil:
		reader.fail(err)
		return copyHash{}, false, nil
	}
	after, err := file.Info()
	if err != nil {
		reader.fail(err)
		return copyHash{}, false, nil
	}
	reader.report.Hashed++
	if !sameVersion(&before, &after) || !matchesRow(&before, cp) {
		reader.report.Changed = append(reader.report.Changed, path)
		return copyHash{}, false, nil
	}
	return copyHash{cp: cp, file: sum, chunks: chunks}, true, nil
}

// sameVersion compares two looks at one open file.
func sameVersion(a, b *fsx.Stat) bool {
	return a.Size == b.Size && a.Mtime == b.Mtime && a.Ctime == b.Ctime
}

// matchesRow says whether a copy is still what the walk of this scan recorded.
func matchesRow(st *fsx.Stat, cp *catalog.CopyRow) bool {
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return false
	}
	ctime, err := solver.TimeNS(st.Ctime.Sec, st.Ctime.Nsec)
	if err != nil {
		return false
	}
	return cp.Size.Valid && cp.Size.Int64 == st.Size &&
		cp.MtimeNS.Valid && cp.MtimeNS.Int64 == mtime &&
		cp.CtimeNS.Valid && cp.CtimeNS.Int64 == ctime
}
