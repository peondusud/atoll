package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// What applying a metadata change says (specs.md §15.4).
const (
	KindUtimens    = catalog.OpUtimens
	OutcomeApplied = "applied"
	OutcomeToApply = "to-apply"
)

// metaOp is an operation utimens, kept until every decision is out: it runs with its decision, and the output lists
// every decision before the first operation.
type metaOp struct {
	disk, place, outcome string
}

// listMetaOps lists the operations utimens of the modifications, first, in the order they ran (specs.md §15.12).
func (p *pass) listMetaOps() {
	for i := range p.metaOps {
		o := &p.metaOps[i]
		if o.outcome == trash.OutcomeIOError {
			p.out.Partial = true
		}
		p.operated(Operation{Disk: o.disk, Kind: KindUtimens, Path: o.place, Outcome: o.outcome})
	}
	p.metaOps = nil
}

// metadataPlan says what the time of a metadata change goes to (specs.md §15.4). A copy is reached when this pass
// can write to its disk and its look found it there.
func (p *pass) metadataPlan(f *catalog.FileRow, in *solver.ModInput, looks map[string]*watch.FoundFile) (solver.MetaPlan, error) {
	copies := make([]solver.MetaCopy, 0, len(in.Copies))
	for i := range in.Copies {
		mc := &in.Copies[i]
		row := copyOn(f, mc.Disk)
		c := solver.MetaCopy{
			Disk: mc.Disk, Position: mc.Position, Changed: isChanged(mc), State: mc.State, RowMtime: row.MtimeNS.Int64,
			PrecisionNS: p.session.TimePrecision(mc.Disk),
		}
		if look := looks[mc.Disk]; c.Changed && look != nil {
			c.Mtime, c.Ctime = look.MtimeNS, look.CtimeNS
		}
		c.Reachable = p.walked[mc.Disk] && p.session.Writable(mc.Disk) && (c.Changed || mc.Verdict == solver.VerdictThere)
		copies = append(copies, c)
	}
	plan, err := solver.MetadataPlan(copies)
	if err != nil {
		return plan, fmt.Errorf("%w", err)
	}
	return plan, nil
}

// applyTime gives each copy the plan reaches the time it applies, one operation each, by position, and says whether
// every one was applied. A changed copy it reached takes, in looks, what its operation left. A dry run only lists.
func (p *pass) applyTime(ctx context.Context, f *catalog.FileRow, plan *solver.MetaPlan, looks map[string]*watch.FoundFile) (bool, error) {
	if !plan.HasMtime {
		return true, nil
	}
	all := true
	for _, disk := range plan.Apply {
		c := copyOn(f, disk)
		place := c.Place(f)
		if !p.apply {
			p.metaOps = append(p.metaOps, metaOp{disk: disk, place: place, outcome: OutcomeToApply})
			continue
		}
		outcome, after, err := p.utimens(ctx, f, c, looks[disk], plan.Mtime)
		if err != nil {
			return false, err
		}
		p.metaOps = append(p.metaOps, metaOp{disk: disk, place: place, outcome: outcome})
		if outcome != OutcomeApplied {
			all = false
			continue
		}
		if looks[disk] != nil {
			looks[disk] = after
		}
	}
	return all, nil
}

// utimens runs one operation utimens on a copy, as its row says or, for a changed copy, as its look found it
// (specs.md §15.4), and says what it leaves there.
func (p *pass) utimens(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, look *watch.FoundFile, mtime int64) (string, *watch.FoundFile, error) {
	place := c.Place(f)
	handle, err := p.session.Handle(ctx, c.Disk)
	if err != nil {
		return "", nil, fmt.Errorf("%w", err)
	}
	// The plan reached it on a disk this pass can write to, and a disk lost since is lost for the plan too.
	if handle == nil {
		return trash.OutcomeIOError, nil, nil
	}
	known := catalog.KnownCopy{Path: place, FilePath: f.Path, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}
	if look != nil {
		known.Size.Int64, known.MtimeNS.Int64, known.CtimeNS.Int64, known.FileKey.String = look.Size, look.MtimeNS, look.CtimeNS, look.FileKey
	}
	op, err := catalog.BeginUtimens(ctx, p.db, c.Disk, place, known.FileKey.String, mtime, p.now)
	if err != nil {
		return "", nil, fmt.Errorf("catalog: %w", err)
	}
	p.machine.Checkpoint("journaled")
	st, err := handle.SetMtime(&known, fsx.TimeOf(mtime))
	switch {
	case errors.Is(err, ops.ErrGone):
		if err := catalog.Undo(ctx, p.db, op, false, p.now); err != nil {
			return "", nil, fmt.Errorf("catalog: %w", err)
		}
		return trash.OutcomeChanged, nil, nil
	case errors.Is(err, ops.ErrDiskLost):
		// The operation stays planned, for the reprise (specs.md §17.2).
		if err := p.session.Lose(ctx, c.Disk, err); err != nil {
			return "", nil, fmt.Errorf("%w", err)
		}
		return trash.OutcomeIOError, nil, nil
	case err != nil:
		slog.Warn("a time was not applied", "disk", c.Disk, "op", op.ID, "error", err)
		finished, err := handle.ResolveNow(ctx, p.db, op, p.now, 0)
		if err != nil {
			slog.Warn("an operation stays planned", "disk", c.Disk, "op", op.ID, "error", err)
		}
		// What the reprise finished is recorded; the next pass decides again, and finds it applied.
		if finished {
			p.session.MarkChanged()
		}
		return trash.OutcomeIOError, nil, nil
	}
	found, err := watch.FileFrom(place, &st)
	if err != nil {
		return "", nil, fmt.Errorf("utimens: %w", err)
	}
	if err := catalog.FinishUtimens(ctx, p.db, op, found.MtimeNS, found.CtimeNS); err != nil {
		return "", nil, fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	return OutcomeApplied, &found, nil
}
