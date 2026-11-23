package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
)

// KindXattr gives a copy the user. extended attributes another copy of its file holds (specs.md §21.2).
const KindXattr = catalog.OpXattr

// giveXattrs takes the extended attributes Atoll's own view set or removed on a file's copy to its other copies, one
// operation xattr each, journaled before it runs (specs.md §21.2): each copy ok or surplus at its file's path, on a disk
// this pass can write to, gets the set of user. attributes the copy changed holds now, read on its disk, never the
// file's data. A copy whose filesystem refuses one keeps what it could take, kept, and is not made stale; a copy on a
// disk this pass cannot write to keeps its own set, disk-unavailable. It gives the operations of the view that are not
// done with yet: the copy changed not found, or not where its row says; a copy changed since its row; a disk lost or
// an operation that failed, which the next pass gives again.
func (p *pass) giveXattrs(ctx context.Context) (map[int64]bool, error) {
	waiting := map[int64]bool{}
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if v.Kind != catalog.ViewXattr || v.State != catalog.OpDone || v.Folder {
			continue
		}
		done, err := p.giveXattrsOf(ctx, v)
		if err != nil {
			return nil, err
		}
		if !done {
			waiting[v.ID] = true
		}
	}
	return waiting, nil
}

// giveXattrsOf gives the attributes one operation of the view changed, and says whether it is done with.
func (p *pass) giveXattrsOf(ctx context.Context, v *catalog.ViewOp) (bool, error) {
	if !p.looked(v.Disk) || !p.session.Readable(v.Disk) {
		return false, nil
	}
	known, err := catalog.KnownCopiesByKey(ctx, p.db, v.Disk, []string{v.FileKey})
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if len(known) != 1 {
		// The file of that inode is no longer the catalog's on that disk, deleted or replaced since: nothing is left to
		// give. Two rows of one inode are names of one file there, whose attributes are that file's already.
		return len(known) == 0, nil
	}
	source := &known[0]
	if source.GoneNS.Valid || source.Path != source.FilePath {
		return false, nil
	}
	f, err := catalog.FileByID(ctx, p.db, source.FileID)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if f == nil || f.State == string(solver.StateConflict) || f.State == string(solver.StateCorrupt) {
		// Nothing says which version the others hold: no attribute crosses from one to another.
		return true, nil
	}
	handle, err := p.session.Handle(ctx, v.Disk)
	if err != nil || handle == nil {
		return false, err //nolint:wrapcheck // the session says what failed
	}
	want, st, err := handle.UserXattrs(source.Path)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return false, p.lose(ctx, v.Disk, err)
	case err != nil:
		slog.Warn("the extended attributes of a copy cannot be read", "disk", v.Disk, "path", source.Path, "error", err)
		return false, nil
	case fmt.Sprintf("%d", st.Ino) != v.FileKey:
		return false, nil
	}
	done := true
	for j := range f.Copies {
		c := &f.Copies[j]
		if c.Disk == v.Disk || (c.State != string(solver.CopyOK) && c.State != string(solver.CopySurplus)) || !c.FileKey.Valid {
			continue
		}
		if c.OldPath != nil || c.GoneNS.Valid {
			// It follows a rename, or its clock runs: what it becomes is decided first.
			done = false
			continue
		}
		outcome, err := p.giveXattrsTo(ctx, f, c, v.Disk, want)
		if err != nil {
			return false, err
		}
		switch outcome {
		case "":
			continue
		case OutcomeApplied, OutcomeKept, OutcomeUnavailable:
		default:
			done = false
		}
		p.record(Operation{Disk: c.Disk, Kind: KindXattr, Path: f.Path, Source: v.Disk, Outcome: outcome})
	}
	return done, nil
}

// giveXattrsTo gives one copy a set of attributes, and says how it went, "" when it held that set already.
func (p *pass) giveXattrsTo(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, from string, want map[string][]byte) (string, error) {
	if !p.looked(c.Disk) || !p.session.Writable(c.Disk) {
		slog.Warn("a copy keeps its extended attributes: its disk cannot be written now", "disk", c.Disk, "path", f.Path)
		return OutcomeUnavailable, nil
	}
	// A copy left as it is, at a wildcard name on the type ntfs, keeps what it holds: nothing writes to it (specs.md
	// §15.8).
	if p.leftAsItIs(c.Disk, f.Path) {
		return OutcomeKept, nil
	}
	handle, err := p.session.Handle(ctx, c.Disk)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if handle == nil {
		return OutcomeUnavailable, nil
	}
	known := catalog.KnownCopy{Path: f.Path, FilePath: f.Path, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}
	// What it holds is read first, so that a copy that holds that set already costs no operation.
	have, st, err := handle.UserXattrs(f.Path)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return trash.OutcomeIOError, p.lose(ctx, c.Disk, err)
	case errors.Is(err, ops.ErrGone):
		return trash.OutcomeChanged, nil
	case err != nil:
		slog.Warn("the extended attributes of a copy cannot be read", "disk", c.Disk, "path", f.Path, "error", err)
		return trash.OutcomeIOError, nil
	case fmt.Sprintf("%d", st.Ino) != c.FileKey.String:
		return trash.OutcomeChanged, nil
	case solver.PlanXattrs(want, have).Empty():
		return "", nil
	}
	op, err := catalog.BeginXattr(ctx, p.db, c.Disk, f.Path, c.FileKey.String, p.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	p.machine.Checkpoint("journaled")
	_, after, given := handle.GiveXattrs(&known, want)
	switch err := given; {
	case err == nil, errors.Is(err, ops.ErrXattrRefused):
		// What it could take is on it, the rest refused: the copy holds the same file all the same, and is not made
		// stale (specs.md §21.2). Its row takes the change time setting moved, so that no look reads it as edited.
		ctime, err := solver.TimeNS(after.Ctime.Sec, after.Ctime.Nsec)
		if err != nil {
			return "", fmt.Errorf("change time: %w", err)
		}
		if err := catalog.FinishXattr(ctx, p.db, op, ctime); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		p.session.MarkChanged()
		if given != nil {
			slog.Warn("a copy keeps some of its extended attributes: its filesystem refuses one", "disk", c.Disk, "path", f.Path, "from", from, "error", given)
			return OutcomeKept, nil
		}
		return OutcomeApplied, nil
	case errors.Is(err, ops.ErrGone):
		if err := catalog.Undo(ctx, p.db, op, false, p.now); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return trash.OutcomeChanged, nil
	case errors.Is(err, ops.ErrDiskLost):
		// The operation stays planned, for the reprise (specs.md §17.2).
		return trash.OutcomeIOError, p.lose(ctx, c.Disk, err)
	}
	slog.Warn("extended attributes were not given", "disk", c.Disk, "op", op.ID, "error", given)
	if _, err := handle.ResolveNow(ctx, p.db, op, p.now, 0); err != nil {
		slog.Warn("an operation stays planned", "disk", c.Disk, "op", op.ID, "error", err)
	}
	return trash.OutcomeIOError, nil
}
