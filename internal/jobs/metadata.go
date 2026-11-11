package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

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
	KindChmod      = catalog.OpChmod
	OutcomeApplied = "applied"
	OutcomeToApply = "to-apply"
)

// metaOp is an operation chmod or utimens, kept until every decision is out: it runs with its decision, and the
// output lists every decision before the first operation.
type metaOp struct {
	disk, kind, place, outcome string
}

// listMetaOps lists the operations of the modifications, first, in the order they ran (specs.md §15.12).
func (p *pass) listMetaOps() {
	for i := range p.metaOps {
		o := &p.metaOps[i]
		if o.outcome == trash.OutcomeIOError {
			p.out.Partial = true
		}
		p.operated(Operation{Disk: o.disk, Kind: o.kind, Path: o.place, Outcome: o.outcome})
	}
	p.metaOps = nil
}

// metadataPlan says what the time and the mode of a metadata change go to (specs.md §15.4). A copy is reached when
// this pass can write to its disk and its look found it there.
func (p *pass) metadataPlan(f *catalog.FileRow, in *solver.ModInput, looks map[string]*watch.FoundFile) (solver.MetaPlan, error) {
	copies := make([]solver.MetaCopy, 0, len(in.Copies))
	for i := range in.Copies {
		mc := &in.Copies[i]
		row := copyOn(f, mc.Disk)
		c := solver.MetaCopy{
			Disk: mc.Disk, Position: mc.Position, Changed: isChanged(mc), State: mc.State, RowMtime: row.MtimeNS.Int64,
			PrecisionNS: p.session.TimePrecision(mc.Disk), RowMode: row.Mode.Int64, HasRowMode: row.Mode.Valid,
		}
		if look := looks[mc.Disk]; c.Changed && look != nil {
			c.Mtime, c.Ctime, c.LookMode, c.HasLookMode = look.MtimeNS, look.CtimeNS, look.Mode, true
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

// applyMetadata gives each copy the plan reaches the mode, then the time, it applies, one operation each, copy by
// copy by position, and says whether every one was applied. A copy an operation reached takes, in looks, what it
// left, which the next operation on it and the record of a changed copy start from. A dry run only lists.
func (p *pass) applyMetadata(ctx context.Context, f *catalog.FileRow, in *solver.ModInput, plan *solver.MetaPlan, looks map[string]*watch.FoundFile) (bool, error) {
	order := slices.Clone(in.Copies)
	slices.SortFunc(order, func(a, b solver.ModCopy) int { return cmp.Compare(a.Position, b.Position) })
	all := true
	for i := range order {
		disk := order[i].Disk
		for _, step := range []struct {
			kind  string
			value int64
			reach bool
		}{
			{KindChmod, plan.Mode, slices.Contains(plan.Chmod, disk)},
			{KindUtimens, plan.Mtime, slices.Contains(plan.Apply, disk)},
		} {
			if !step.reach {
				continue
			}
			c := copyOn(f, disk)
			place := c.Place(f)
			if !p.apply {
				p.metaOps = append(p.metaOps, metaOp{disk: disk, kind: step.kind, place: place, outcome: OutcomeToApply})
				continue
			}
			outcome, after, err := p.metaOperation(ctx, f, c, looks[disk], step.kind, step.value)
			if err != nil {
				return false, err
			}
			p.metaOps = append(p.metaOps, metaOp{disk: disk, kind: step.kind, place: place, outcome: outcome})
			if outcome != OutcomeApplied {
				all = false
				continue
			}
			looks[disk] = after
		}
	}
	return all, nil
}

// metaOperation runs one operation chmod or utimens on a copy, as its row says or as what a look or an earlier
// operation found it (specs.md §15.4), and says what it leaves there.
func (p *pass) metaOperation(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, look *watch.FoundFile, kind string, value int64) (string, *watch.FoundFile, error) {
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
	begin, set, finish := catalog.BeginUtimens, func() (fsx.Stat, error) { return handle.SetMtime(&known, fsx.TimeOf(value)) }, catalog.FinishUtimens
	if kind == KindChmod {
		begin, set, finish = catalog.BeginChmod, func() (fsx.Stat, error) { return handle.SetMode(&known, uint32(value)) }, catalog.FinishChmod
	}
	op, err := begin(ctx, p.db, c.Disk, place, known.FileKey.String, value, p.now)
	if err != nil {
		return "", nil, fmt.Errorf("catalog: %w", err)
	}
	p.machine.Checkpoint("journaled")
	st, err := set()
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
		slog.Warn("a metadata change was not applied", "disk", c.Disk, "op", op.ID, "kind", kind, "error", err)
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
		return "", nil, fmt.Errorf("%s: %w", kind, err)
	}
	held := found.MtimeNS
	if kind == KindChmod {
		held = found.Mode
	}
	if err := finish(ctx, p.db, op, held, found.CtimeNS); err != nil {
		return "", nil, fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	return OutcomeApplied, &found, nil
}
