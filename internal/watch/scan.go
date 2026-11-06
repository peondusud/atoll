package watch

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
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
	Absent      []string
	Unreadable  []string
	OtherMounts []string
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
		if opts.Disk != "" && d.ID != opts.Disk {
			continue
		}
		report, err := walkDisk(ctx, db, c, m, d, verdicts[i], now)
		if err != nil {
			return nil, err
		}
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
	for _, list := range [][]string{r.Changed, r.Absent, r.Unreadable, r.OtherMounts} {
		slices.Sort(list)
	}
}

// diskRows says what to record of each disk. One that passed its check and cannot say how full it is
// counts as failed, in the verdict too, and keeps what the last check measured (specs.md §15.10).
func diskRows(c *config.Config, verdicts []disks.Verdict, m disks.Machine, now int64) []catalog.Disk {
	rows := make([]catalog.Disk, 0, len(c.Disks))
	for i := range c.Disks {
		d := &c.Disks[i]
		row := catalog.Disk{
			ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role,
			Position: i, Domain: d.Domain, Tags: d.Tags, State: string(verdicts[i].State),
		}
		if verdicts[i].State != disks.Missing {
			capacity, free, err := m.Space(d.Mount)
			if err != nil {
				verdicts[i] = disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: err.Error()}
				row.State = string(disks.Missing)
			} else {
				row.Seen, row.Capacity, row.Free, row.LastSeenNS = true, capacity, free, now
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// walkDisk walks one disk and writes what it found, whole or not at all.
func walkDisk(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, d *config.Disk, verdict disks.Verdict, now int64) (*Report, error) {
	r := &Report{ID: d.ID, Verdict: verdict, Changed: []string{}, Absent: []string{}, Unreadable: []string{}, OtherMounts: []string{}}
	if verdict.State == disks.Missing {
		return r, nil
	}
	walked, err := walkRoot(ctx, m, d)
	switch {
	case errors.Is(err, ErrInterrupted):
		r.Verdict = disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: err.Error()}
		if err := catalog.SetDiskState(ctx, db, d.ID, string(disks.Missing)); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		return r, nil
	case err != nil:
		return nil, err
	}
	newCopies, entries, err := compareWalk(ctx, db, c, d.ID, walked, r)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if err := catalog.WriteWalk(ctx, db, d.ID, now, newCopies, entries); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	r.Walked = true
	return r, nil
}

func walkRoot(ctx context.Context, m disks.Machine, d *config.Disk) (*Walked, error) {
	root, err := m.OpenRoot(d.Mount, d.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	walked, err := Walk(ctx, root)
	// A folder only read, whose close failing changes nothing that was read from it.
	_ = root.Close()
	return walked, err
}

// compareWalk says what a walk means against what the catalog knows, and fills the report. Both
// sides are in the byte order of paths, so it is a merge, and nothing needs to be looked up.
func compareWalk(ctx context.Context, db *sql.DB, c *config.Config, disk string, w *Walked, r *Report) ([]catalog.NewCopy, []catalog.EntryRow, error) {
	r.Unreadable = w.Unreadable
	r.OtherMounts = w.OtherMounts
	slices.SortFunc(w.Files, func(a, b FoundFile) int { return cmp.Compare(a.Path, b.Path) })
	var newCopies []catalog.NewCopy
	entries := make([]catalog.EntryRow, 0, len(w.Entries))
	found := func(f *FoundFile) {
		r.Files++
		r.Bytes += f.Size
	}
	fresh := func(f *FoundFile) {
		size := f.Size
		policy := c.Policy(f.Path, &size)
		if policy.Action == rules.Ignore {
			entries = append(entries, catalog.EntryRow{Path: f.Path, Kind: string(solver.EntryIgnored), Mode: f.Mode, MtimeNS: f.MtimeNS})
			r.Ignored++
			return
		}
		newCopies = append(newCopies, catalog.NewCopy{
			Path: f.Path, Size: f.Size, MtimeNS: f.MtimeNS, CtimeNS: f.CtimeNS, BtimeNS: f.BtimeNS, FileKey: f.FileKey, Nlink: f.Nlink,
		})
		r.New++
		found(f)
	}
	notReached := pathSet(w.Unreadable, w.OtherMounts)
	next := 0
	err := catalog.EachKnownCopy(ctx, db, disk, func(k *catalog.KnownCopy) {
		for next < len(w.Files) && w.Files[next].Path < k.Path {
			fresh(&w.Files[next])
			next++
		}
		if next == len(w.Files) || w.Files[next].Path != k.Path {
			if !atOrBelow(notReached, k.Path) {
				r.Absent = append(r.Absent, k.Path)
			}
			return
		}
		f := &w.Files[next]
		next++
		found(f)
		if Unchanged(f, k) {
			r.Known++
		} else {
			r.Changed = append(r.Changed, f.Path)
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	for ; next < len(w.Files); next++ {
		fresh(&w.Files[next])
	}
	for i := range w.Entries {
		e := &w.Entries[i]
		entries = append(entries, catalog.EntryRow{Path: e.Path, Kind: string(e.Kind), Mode: e.Mode, MtimeNS: e.MtimeNS, Target: e.Target})
		switch e.Kind {
		case solver.EntryDir:
			r.Folders++
		case solver.EntrySymlink:
			r.Symlinks++
		case solver.EntryIgnored:
			r.Ignored++
		}
	}
	return newCopies, entries, nil
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
	id     int64
	state  string
	update *catalog.FileUpdate
}

// Settle gives each file its original, its policy and what the plan says, on the whole catalog.
func Settle(ctx context.Context, db *sql.DB, c *config.Config, now int64) error {
	diskList, err := catalog.Disks(ctx, db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	entries, err := catalog.Entries(ctx, db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	minFree, err := minFreeOf(c)
	if err != nil {
		return err
	}
	grace, err := missingGraceOf(c)
	if err != nil {
		return err
	}
	input := solver.Input{MinFree: minFree, Disks: planDisks(diskList, now, grace), Entries: planEntries(entries)}
	remembered, err := filesForPlan(ctx, db, c, diskList, &input)
	if err != nil {
		return err
	}
	plan, err := solver.MakePlan(input)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	changed, states, marks, err := applyResults(input.Files, remembered, plan.Results)
	if err != nil {
		return err
	}
	if err := catalog.ApplyPlan(ctx, db, now, changed, states, marks); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

func minFreeOf(c *config.Config) (value.MinFree, error) {
	raw, _ := c.Effective()["placement.min_free"].([]any)
	list := make([]string, 0, len(raw))
	for _, v := range raw {
		text, _ := v.(string)
		list = append(list, text)
	}
	minFree, err := value.ParseMinFree(list)
	if err != nil {
		return value.MinFree{}, fmt.Errorf("placement.min_free: %w", err)
	}
	return minFree, nil
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
func filesForPlan(ctx context.Context, db *sql.DB, c *config.Config, diskList []catalog.Disk, input *solver.Input) ([]settled, error) {
	position := make(map[string]int, len(diskList))
	state := make(map[string]solver.DiskState, len(diskList))
	for i := range diskList {
		position[diskList[i].ID] = diskList[i].Position
		state[diskList[i].ID] = solver.DiskState(diskList[i].State)
	}
	placements := map[placementKey]*solver.Placement{}
	var remembered []settled
	reader := catalog.NewFileReader(db)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return remembered, nil
		}
		for i := range page {
			f := &page[i]
			update, err := settleFile(c, f, position, state)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", f.Path, err)
			}
			placement := placementKey{replicas: update.ReplicasTarget, priority: c.Pool.Priority, spread: c.Pool.Spread}
			size := update.Size
			if policy := c.Policy(f.Path, &size); policy.Action != rules.Ignore {
				placement = placementKey{replicas: policy.Replicas, priority: policy.Priority, spread: policy.Spread}
			}
			copies := make([]solver.Copy, 0, len(f.Copies))
			for j := range f.Copies {
				cp := &f.Copies[j]
				copies = append(copies, solver.Copy{Disk: cp.Disk, State: solver.CopyState(cp.State), CreatedBy: cp.CreatedBy, FirstSeenNS: cp.FirstSeenNS})
			}
			input.Files = append(input.Files, solver.File{
				Path: f.Path, Size: update.Size, Placement: placementFor(placements, placement), Hold: update.Mode == "manual", Copies: copies,
			})
			entry := settled{id: f.ID, state: f.State}
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

type placementKey struct {
	replicas, priority int
	spread             string
}

// placementFor gives the plan what it may act on. Until phase 1b, what specs.md §24.4 says has no
// effect is not given to it: no tags or domains, and no strict mode or denied overflow. Delete a
// line here when its phase lands.
func placementFor(known map[placementKey]*solver.Placement, key placementKey) *solver.Placement {
	if p, ok := known[key]; ok {
		return p
	}
	p := &solver.Placement{
		Replicas: key.replicas, Priority: key.priority, Spread: key.spread,
		TagsMode: "prefer", SpreadMode: "prefer", Overflow: "allow",
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
func applyResults(files []solver.File, remembered []settled, results []solver.Result) ([]catalog.FileUpdate, []catalog.StateChange, []catalog.CopyMark, error) {
	var changed []catalog.FileUpdate
	var states []catalog.StateChange
	var marks []catalog.CopyMark
	next := 0
	for i := range files {
		f, r := &files[i], &remembered[i]
		var applying []solver.State
		if r.state == string(solver.StateConflict) || r.state == string(solver.StateCorrupt) {
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
		}
		shown, err := solver.ShownState(applying)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%q: %w", f.Path, err)
		}
		switch {
		case r.update != nil:
			r.update.State = string(shown)
			changed = append(changed, *r.update)
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
		reader := &diskReader{report: report, skip: pathSet(report.Changed, report.Absent), notReached: pathSet(report.Unreadable, report.OtherMounts)}
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

func hashFile(ctx context.Context, db *sql.DB, f *catalog.FileRow, readers map[string]*diskReader, order map[string]int, now int64) error {
	copies := make([]*catalog.CopyRow, 0, len(f.Copies))
	for i := range f.Copies {
		copies = append(copies, &f.Copies[i])
	}
	slices.SortFunc(copies, func(a, b *catalog.CopyRow) int { return cmp.Compare(order[a.Disk], order[b.Disk]) })
	var read []copyHash
	for _, cp := range copies {
		reader := readers[cp.Disk]
		if reader == nil || reader.failed || reader.skip[f.Path] || atOrBelow(reader.notReached, f.Path) {
			continue
		}
		hash, ok, err := readCopy(ctx, reader, f.Path, cp)
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
			readers[read[i].cp.Disk].report.Changed = append(readers[read[i].cp.Disk].report.Changed, f.Path)
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
