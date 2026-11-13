package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// modify decides, once the other decisions are made, what a copy edited or replaced means for its file, by path in
// byte order, one file to a transaction (specs.md §15.4).
func (p *pass) modify(ctx context.Context) (err error) {
	ids := p.changedFiles()
	if len(ids) == 0 {
		return nil
	}
	// Every changed file is read here, even one the guard holds, at each pass while it holds.
	finder, err := catalog.NewFileFinder(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	defer func() {
		if closeErr := finder.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("catalog: %w", closeErr))
		}
	}()
	type named struct {
		id   int64
		path string
	}
	list := make([]named, 0, len(ids))
	for _, id := range ids {
		path, ok, err := finder.Path(ctx, id)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if ok {
			list = append(list, named{id: id, path: path})
		}
	}
	slices.SortFunc(list, func(a, b named) int { return strings.Compare(a.path, b.path) })
	positions, err := p.positions(ctx)
	if err != nil {
		return err
	}
	for _, n := range list {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("modify: %w", err)
		}
		if err := p.modifyFile(ctx, finder, n.id, positions); err != nil {
			return err
		}
		if p.sinkErr != nil {
			return nil
		}
	}
	return nil
}

// changedFiles gives the files a look found a copy of edited or replaced, by id. Sorted, rather than a map of
// them: a pass where every file changed holds 200,000 (specs.md §26).
func (p *pass) changedFiles() []int64 {
	var ids []int64
	for _, list := range p.seen {
		for i := range list {
			if s := &list[i]; s.Verdict == solver.VerdictEdited || s.Verdict == solver.VerdictReplaced {
				ids = append(ids, s.FileID)
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func isChanged(c *solver.ModCopy) bool {
	return c.Verdict == solver.VerdictEdited || (c.Verdict == solver.VerdictReplaced && !c.Other)
}

// modInput says what a modification needs of a file, but for the size a changed copy has now, which takes a look.
// What is not due yet counts in waiting.
func (p *pass) modInput(ctx context.Context, finder *catalog.FileFinder, f *catalog.FileRow, positions map[string]int) (*solver.ModInput, map[string]int64, error) {
	running, err := finder.Running(ctx, f.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: %w", err)
	}
	// What an operation left planned at a copy's name, only a reprise can tell (specs.md §17.2).
	for i := range f.Copies {
		place := f.Copies[i].Place(f)
		running = running || slices.ContainsFunc(p.planned[f.Copies[i].Disk], func(name string) bool { return isAtOrBelow(place, name) })
	}
	size := f.Size
	settle := p.config.Policy(f.Path, &size).Settle.Nanoseconds()
	in := &solver.ModInput{State: solver.State(f.State), Size: f.Size, Hash: f.Hash, Running: running}
	ctimes := map[string]int64{}
	for i := range f.Copies {
		c := &f.Copies[i]
		// A pending copy is nothing on its disk yet (specs.md §14.1).
		if c.State == string(solver.CopyPending) {
			continue
		}
		s := p.verdictOf(f, c)
		mc := solver.ModCopy{
			Disk: c.Disk, Position: positions[c.Disk], Verdict: s.Verdict, State: solver.CopyState(c.State), Size: c.Size.Int64,
			Readable: p.walked[c.Disk] && p.session.Readable(c.Disk),
		}
		// A row that says it moved, which this look could not get to, is decided nothing of yet.
		if p.isMovedOutOfReach(c, s.Verdict) {
			mc.Verdict = solver.VerdictUncertain
		}
		// A copy an overwriting move took the name of is gone, not replaced, from the walk on (specs.md §15.12).
		if s.Verdict == solver.VerdictReplaced {
			mc.Other = !s.Regular
		}
		if isChanged(&mc) {
			ctimes[c.Disk] = s.CtimeNS
			due := s.CtimeNS + settle
			mc.Due = !p.apply || due <= p.now
			if !mc.Due {
				p.copiesWaiting++
				p.copiesDue = earliest(p.copiesDue, sql.NullInt64{Int64: due, Valid: true})
			}
		}
		in.Copies = append(in.Copies, mc)
	}
	return in, ctimes, nil
}

func (p *pass) modifyFile(ctx context.Context, finder *catalog.FileFinder, id int64, positions map[string]int) error {
	f, err := finder.ByID(ctx, id)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil
	}
	// Nothing changes for a file in conflict, and what it has changed waits for nothing (specs.md §15.4).
	if f.State == string(solver.StateConflict) || f.State == string(solver.StateCorrupt) {
		return nil
	}
	in, ctimes, err := p.modInput(ctx, finder, f, positions)
	if err != nil {
		return err
	}
	// What waits, or has nothing to decide, needs no look.
	if kind, err := solver.ModifyGate(in); err != nil || kind != solver.ModRead {
		return err
	}
	// Held by the mass-modification guard of its disk: nothing is read until someone answers (specs.md §15.6).
	first := firstChanged(in)
	if first != nil && p.heldBefore(first.Disk, ctimes[first.Disk]) {
		p.out.Held = true
		return nil
	}
	looks := map[string]*watch.FoundFile{}
	for i := range in.Copies {
		mc := &in.Copies[i]
		if !isChanged(mc) {
			continue
		}
		look, ok, err := p.lookAgain(ctx, f, copyOn(f, mc.Disk), ctimes[mc.Disk])
		if err != nil || !ok {
			return err
		}
		mc.Size = look.Size
		looks[mc.Disk] = look
	}
	chunks := map[string][]byte{}
	var d solver.ModDecision
	for {
		if d, err = solver.Modify(in); err != nil {
			return fmt.Errorf("%w", err)
		}
		if d.Kind != solver.ModRead {
			break
		}
		if p.later {
			// This pass reads no file whole: the file is decided by the slices that follow it (specs.md §15.13).
			pending := PendingRead{FileID: f.ID}
			if first != nil {
				pending.AnswerID = p.answerOver(first.Disk, ctimes[first.Disk])
			}
			p.out.ToRead = append(p.out.ToRead, pending)
			return nil
		}
		for _, disk := range d.Read {
			at := slices.IndexFunc(in.Copies, func(c solver.ModCopy) bool { return c.Disk == disk })
			sum, list, ok, err := p.readForModify(ctx, f, copyOn(f, disk), looks[disk])
			if err != nil || !ok {
				return err
			}
			in.Copies[at].Hash, chunks[disk] = sum, list
		}
	}
	if d.Kind == solver.ModNone || d.Kind == solver.ModWait {
		return nil
	}
	answered := false
	if d.Kind == solver.ModModified {
		var goes bool
		if goes, answered, err = p.guardModification(ctx, &d, ctimes[d.Changed[0]]); err != nil || !goes {
			return err
		}
	}
	return p.applyModification(ctx, f, in, &d, looks, chunks, answered)
}

// lookAgain looks at a changed copy before anything is read or decided of it: it must still be the file its walk
// found, with the same change time, or it waits for the next pass (specs.md §15.4).
func (p *pass) lookAgain(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, ctime int64) (*watch.FoundFile, bool, error) {
	handle, err := p.session.Handle(ctx, c.Disk)
	if err != nil || handle == nil {
		return nil, false, err
	}
	place := c.Place(f)
	st, there, err := handle.StatRoot(place)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return nil, false, p.loseRead(ctx, c.Disk, err)
	case err != nil:
		slog.Warn("a copy changed could not be looked at", "disk", c.Disk, "path", place, "error", err)
		return nil, false, nil
	case !there || st.Kind != fsx.KindFile:
		return nil, false, nil
	}
	found, err := watch.FileFrom(place, &st)
	if err != nil || found.CtimeNS != ctime {
		return nil, false, nil //nolint:nilerr // a time no row can hold is a look that says nothing yet
	}
	return &found, true, nil
}

// readForModify reads a copy to its end, as one read gives both hashes (specs.md §15.4, §16): a changed copy as its
// look found it, any other as its row says. It says false when that gives nothing to decide on.
func (p *pass) readForModify(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, look *watch.FoundFile) ([]byte, []byte, bool, error) {
	handle, err := p.session.Handle(ctx, c.Disk)
	if err != nil || handle == nil {
		return nil, nil, false, err
	}
	place := c.Place(f)
	known := catalog.KnownCopy{Path: place, FilePath: f.Path, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}
	if look != nil {
		known.Size = sql.NullInt64{Int64: look.Size, Valid: true}
		known.MtimeNS = sql.NullInt64{Int64: look.MtimeNS, Valid: true}
		known.CtimeNS = sql.NullInt64{Int64: look.CtimeNS, Valid: true}
		known.FileKey = sql.NullString{String: look.FileKey, Valid: true}
	}
	file, opened, err := handle.OpenSource(place, &known)
	switch {
	case errors.Is(err, ops.ErrGone):
		return nil, nil, false, nil
	case errors.Is(err, ops.ErrDiskLost):
		return nil, nil, false, p.loseRead(ctx, c.Disk, err)
	case err != nil:
		slog.Warn("a copy could not be read", "disk", c.Disk, "path", place, "error", err)
		return nil, nil, false, nil
	}
	// A file only read, whose close failing changes nothing that was read from it.
	defer func() { _ = file.Close() }()
	if restore, err := fsx.LowIOPriority(); err != nil {
		slog.Warn("a read runs at the disk's normal I/O priority", "error", err)
	} else {
		defer restore()
	}
	sum, list, err := integrity.HashReader(ctx, file)
	switch {
	case ctx.Err() != nil:
		return nil, nil, false, fmt.Errorf("modify: %w", ctx.Err())
	case err != nil:
		return nil, nil, false, p.loseRead(ctx, c.Disk, err)
	}
	after, err := file.Info()
	if err != nil {
		return nil, nil, false, p.loseRead(ctx, c.Disk, err)
	}
	// Moved during the read: still being written, and the next pass reads it again.
	if after.Size != opened.Size || after.Mtime != opened.Mtime || after.Ctime != opened.Ctime {
		return nil, nil, false, nil
	}
	return sum[:], list, true, nil
}

// loseRead records that a disk failed a read, as a scan does: the pass ends partial (specs.md §15.4, §15.10).
func (p *pass) loseRead(ctx context.Context, disk string, why error) error {
	if err := p.session.Lose(ctx, disk, why); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// applyModification records what was decided, or with a dry run says what it would do.
func (p *pass) applyModification(ctx context.Context, f *catalog.FileRow, in *solver.ModInput, d *solver.ModDecision, looks map[string]*watch.FoundFile, chunks map[string][]byte, answered bool) error {
	stale, src := d.Stale, d.Changed[0]
	if d.Kind == solver.ModMetadata {
		plan, err := p.metadataPlan(f, in, looks)
		if err != nil {
			return err
		}
		stale, src = plan.Stale, plan.Source()
		// The mode and the time go to each copy first: a decision recorded before would read, after a crash, as one
		// that was already there (specs.md §15.4).
		applied, err := p.applyMetadata(ctx, f, in, &plan, looks)
		if err != nil || !applied {
			p.decided(Decision{Path: f.Path, Kind: string(d.Kind)})
			return err
		}
	}
	if !p.apply {
		switch d.Kind {
		case solver.ModModified:
			p.jobsDropped[f.ID] = true
			p.planOverwrites(f, stale, src)
		case solver.ModMetadata:
			p.planOverwrites(f, stale, src)
		case solver.ModConflict, solver.ModNone, solver.ModWait, solver.ModRead:
		}
		p.decided(Decision{Path: f.Path, Kind: string(d.Kind)})
		return nil
	}
	size := f.Size
	m := &catalog.Modification{
		FileID: f.ID, Path: f.Path, Kind: string(d.Kind), Stale: stale, Src: src, Hash: d.Hash, Reference: d.Reference,
		Priority: int64(p.config.Policy(f.Path, &size).Priority), Answered: answered,
	}
	if d.Hash != nil {
		m.Chunks = chunks[d.Changed[0]]
	}
	for _, disk := range d.Changed {
		at := slices.IndexFunc(in.Copies, func(c solver.ModCopy) bool { return c.Disk == disk })
		look := looks[disk]
		changed := catalog.ChangedCopy{
			Disk: disk, FileKey: look.FileKey, Nlink: look.Nlink, Size: look.Size, MtimeNS: look.MtimeNS, CtimeNS: look.CtimeNS,
			Mode: look.Mode, UID: look.UID, GID: look.GID, Replaced: in.Copies[at].Verdict == solver.VerdictReplaced,
		}
		if look.BtimeNS != nil {
			changed.BtimeNS = sql.NullInt64{Int64: *look.BtimeNS, Valid: true}
		}
		m.Changed = append(m.Changed, changed)
	}
	if err := catalog.ApplyModification(ctx, p.db, p.now, m); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	p.machine.Checkpoint("modified")
	p.decided(Decision{Path: f.Path, Kind: string(d.Kind)})
	return nil
}

// planOverwrites says what a modification would send to the trash, as a dry run records nothing: each stale copy that
// holds a file gets a job (specs.md §15.4).
func (p *pass) planOverwrites(f *catalog.FileRow, stale []string, src string) {
	held := p.trashOutcome(src) == OutcomeHeld
	for _, disk := range stale {
		if c := copyOn(f, disk); c.FileKey.Valid {
			plan := p.planOf(disk)
			plan.overwrites = append(plan.overwrites, overwrite{place: c.Place(f), held: held})
		}
	}
}

// overwrite is a trash job a dry run's modification would queue: a whole Operation for each, in a dry run where
// every file changed, came close to the memory of specs.md §26.
type overwrite struct {
	place string
	held  bool
}

func (o *overwrite) operation(disk string, unavailable bool) Operation {
	op := Operation{Disk: disk, Kind: KindTrash, Path: o.place, Reason: catalog.ReasonOverwritten, Outcome: OutcomeToTrash}
	switch {
	case o.held:
		op.Outcome = OutcomeHeld
	case unavailable:
		op.Outcome = OutcomeUnavailable
	}
	return op
}

// ReadChanged decides the files a pass left to read, in their order, until the time until, and gives those it had no
// time for (specs.md §15.4, §15.13). Each is decided as a pass decides it: its copies looked at again, the changed ones
// read, with what they are compared with, the mass-modification guard asked, the decision recorded, and a change of
// metadata applied to the other copies. The copies it makes stale are for the pass that follows: it runs no job.
func ReadChanged(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, sink Sink, pending []PendingRead, until int64) (out *Outcome, left []PendingRead, err error) {
	p := newPass(db, c, m, true, sink, nil)
	if err := p.readSettings(); err != nil {
		return nil, nil, err
	}
	session, err := trash.Open(ctx, db, c, m, true)
	if err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	p.session = session
	planned, err := catalog.PlannedOps(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: %w", err)
	}
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed {
			continue
		}
		if p.rows[d.ID], err = catalog.CopyCount(ctx, db, d.ID); err != nil {
			return nil, nil, fmt.Errorf("catalog: %w", err)
		}
		p.planned[d.ID] = plannedOn(planned, d.ID)
	}
	if err := p.readModifyGuards(ctx); err != nil {
		return nil, nil, err
	}
	finder, err := catalog.NewFileFinder(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: %w", err)
	}
	defer func() {
		if closeErr := finder.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("catalog: %w", closeErr))
		}
	}()
	positions, err := p.positions(ctx)
	if err != nil {
		return nil, nil, err
	}
	left = pending
	for len(left) > 0 && m.Now() < until {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("modify: %w", err)
		}
		next := left[0]
		left = left[1:]
		p.takenUnder = next.AnswerID
		if err := p.lookAtFile(ctx, finder, next.FileID); err != nil {
			return nil, nil, err
		}
		if err := p.modifyFile(ctx, finder, next.FileID, positions); err != nil {
			return nil, nil, err
		}
		if p.sinkErr != nil {
			return nil, nil, p.sinkErr
		}
	}
	if err := p.sortGuards(ctx); err != nil {
		return nil, nil, err
	}
	p.listMetaOps()
	if p.sinkErr != nil {
		return nil, nil, p.sinkErr
	}
	if err := session.Finish(ctx); err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	p.out.Waiting, p.out.NextDue = p.copiesWaiting, p.copiesDue
	p.out.Partial = p.out.Partial || session.Lost()
	return p.out, left, nil
}

// lookAtFile looks at each copy of a file where it sits, as a walk would, for a slice of reads: what a pass found of
// it may be hours old by the time its slice comes. A copy that is no longer at its name is gone, as far as this look
// can tell, which leaves its file to the next pass; so does a copy this slice cannot get to.
func (p *pass) lookAtFile(ctx context.Context, finder *catalog.FileFinder, id int64) error {
	clear(p.seen)
	clear(p.walked)
	f, err := finder.ByID(ctx, id)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.State == string(solver.CopyPending) {
			continue
		}
		handle, err := p.session.Handle(ctx, c.Disk)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if handle == nil {
			continue
		}
		place := c.Place(f)
		seen := watch.CopySeen{FileID: f.ID, Verdict: solver.VerdictUnknown}
		st, there, err := handle.StatRoot(place)
		switch {
		case errors.Is(err, ops.ErrDiskLost):
			if err := p.loseRead(ctx, c.Disk, err); err != nil {
				return err
			}
			continue
		case err != nil:
			slog.Warn("a copy could not be looked at", "disk", c.Disk, "path", place, "error", err)
		case !there:
			seen.Verdict = solver.VerdictGone
		case st.Kind != fsx.KindFile:
			seen.Verdict = solver.VerdictReplaced
		default:
			found, err := watch.FileFrom(place, &st)
			if err != nil {
				break
			}
			known := catalog.KnownCopy{Path: place, FilePath: f.Path, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, BtimeNS: c.BtimeNS, FileKey: c.FileKey}
			look := watch.FoundLook(&found)
			seen.Verdict, _ = solver.VerdictOf(&solver.VerdictInput{Row: watch.RowLook(&known), AtName: &look})
			seen.CtimeNS, seen.Nlink, seen.Regular = found.CtimeNS, found.Nlink, true
		}
		p.walked[c.Disk] = true
		if seen.Verdict != solver.VerdictThere {
			p.seen[c.Disk] = []watch.CopySeen{seen}
		}
	}
	return nil
}
