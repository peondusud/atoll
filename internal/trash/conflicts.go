package trash

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// What atoll conflicts resolve refuses, before it looks at a disk (specs.md §15.7).
var (
	ErrNoFile      = errors.New("the catalog has no file at that path")
	ErrNotConflict = errors.New("the file is not in conflict")
	ErrNoCopy      = errors.New("the file has no copy on that disk")
)

// The outcomes of a resolution, besides those of a move to the trash.
const (
	// OutcomeKept is the copy kept.
	OutcomeKept = "kept"
	// OutcomeToTrash is a copy a run with --yes would send to the trash.
	OutcomeToTrash = "to-trash"
	// OutcomeGone is a copy no longer on its disk: its row goes, and nothing is left to send.
	OutcomeGone = "gone"
	// OutcomeNotMoved is a copy left where it is, as the one to keep could not be reached or is gone.
	OutcomeNotMoved = "not-moved"
)

// ResolveItem is the outcome of one copy of a file in conflict, with the size and time its disk holds.
type ResolveItem struct {
	Path    string
	Disk    string
	Size    int64
	MtimeNS int64
	Outcome string
}

// ResolveSummary says what a resolution did: Resolved, when the file is no longer in conflict.
type ResolveSummary struct {
	Summary
	Resolved bool
}

// Resolve keeps the copy of a file in conflict on disk keep, and sends every other copy to the trash, as overwritten
// (specs.md §15.7). It looks at each copy as it is now, since a walk leaves the copies of a file in conflict as it
// found them: the copy kept is what its disk holds, and so is each copy that goes, its row first brought to what its
// disk holds, and a copy no longer there has its row removed. Once no other copy is left, the file takes the kept
// copy's size and time, its hash is read again, and the next pass makes its replicas from it. A copy on a disk that
// cannot be reached stays, and so does the conflict: running it again finishes it. Copies are given by disk position,
// the kept one first.
func (s *Session) Resolve(ctx context.Context, path, keep string, each func(*ResolveItem) error) (*ResolveSummary, error) {
	f, err := catalog.FileAt(ctx, s.db, path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("catalog: %w", err)
	case f == nil:
		return nil, ErrNoFile
	case f.State != string(solver.StateConflict):
		return nil, ErrNotConflict
	case !slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == keep }):
		return nil, ErrNoCopy
	}
	position, err := s.positions(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(f.Copies, func(a, b catalog.CopyRow) int {
		switch {
		case a.Disk == keep:
			return -1
		case b.Disk == keep:
			return 1
		default:
			return cmp.Compare(position[a.Disk], position[b.Disk])
		}
	})
	summary := &ResolveSummary{Summary: Summary{Applied: s.apply}}
	report := func(c *catalog.CopyRow, outcome string) error {
		if err := each(&ResolveItem{Path: f.Path, Disk: c.Disk, Size: c.Size.Int64, MtimeNS: c.MtimeNS.Int64, Outcome: outcome}); err != nil {
			return fmt.Errorf("%w", err)
		}
		return nil
	}
	seen := make([]*catalog.SeenCopy, len(f.Copies))
	reached := make([]bool, len(f.Copies))
	for i := range f.Copies {
		if seen[i], reached[i], err = s.lookNow(ctx, f, &f.Copies[i]); err != nil {
			return nil, err
		}
		if seen[i] != nil {
			show(&f.Copies[i], seen[i])
		}
	}
	if !reached[0] || seen[0] == nil {
		outcome := OutcomeDiskUnavailable
		if reached[0] {
			outcome = OutcomeGone
		}
		summary.Stayed = len(f.Copies)
		if err := report(&f.Copies[0], outcome); err != nil {
			return nil, err
		}
		for i := 1; i < len(f.Copies); i++ {
			if err := report(&f.Copies[i], OutcomeNotMoved); err != nil {
				return nil, err
			}
		}
		summary.DiskLost = s.lost
		return summary, nil
	}
	if s.apply {
		if err := catalog.BeginResolve(ctx, s.db, f.Path, keep, s.now); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if err := catalog.RefreshCopy(ctx, s.db, f.ID, keep, seen[0]); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
	if err := report(&f.Copies[0], OutcomeKept); err != nil {
		return nil, err
	}
	for i := 1; i < len(f.Copies); i++ {
		c := &f.Copies[i]
		outcome, err := s.resolveOther(ctx, f, c, seen[i], reached[i])
		if err != nil {
			return nil, err
		}
		switch outcome {
		case OutcomeToTrash:
			summary.ToDo++
			summary.Bytes += c.Size.Int64
		case OutcomeTrashed:
			summary.Bytes += c.Size.Int64
		case OutcomeGone:
			if !s.apply {
				summary.ToDo++
			}
		default:
			summary.Stayed++
		}
		if err := report(c, outcome); err != nil {
			return nil, err
		}
	}
	if s.apply && summary.Stayed == 0 {
		switch err := catalog.ResolveConflict(ctx, s.db, f.ID, keep, s.now); {
		case errors.Is(err, catalog.ErrStillAConflict):
		case err != nil:
			return nil, fmt.Errorf("catalog: %w", err)
		default:
			summary.Resolved = true
		}
	}
	if err := s.RemoveEmptied(ctx); err != nil {
		return nil, err
	}
	summary.DiskLost = s.lost
	return summary, nil
}

// resolveOther sends a copy other than the one kept to the trash, as its disk holds it now, or removes its row when
// it is no longer there.
func (s *Session) resolveOther(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, seen *catalog.SeenCopy, reached bool) (string, error) {
	switch {
	case !reached:
		return OutcomeDiskUnavailable, nil
	case seen == nil && !s.apply:
		return OutcomeGone, nil
	case seen == nil:
		if err := catalog.DropCopy(ctx, s.db, f.ID, c.Disk); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return OutcomeGone, nil
	case !s.apply:
		return OutcomeToTrash, nil
	}
	if err := catalog.RefreshCopy(ctx, s.db, f.ID, c.Disk, seen); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return s.Send(ctx, f, c, catalog.ReasonOverwritten)
}

// lookNow says what a copy's disk holds at its place now: nil when nothing is there, or not a file, and reached
// false for a disk that cannot be reached.
func (s *Session) lookNow(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow) (*catalog.SeenCopy, bool, error) {
	handle, err := s.open(ctx, c.Disk)
	if err != nil || handle == nil {
		return nil, false, err
	}
	place := c.Place(f)
	st, there, err := handle.StatRoot(place)
	if err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return nil, false, s.lose(ctx, c.Disk, err)
		}
		slog.Warn("a copy cannot be looked at", "disk", c.Disk, "error", err)
		return nil, false, nil
	}
	if !there || st.Kind != fsx.KindFile {
		return nil, true, nil
	}
	found, err := watch.FileFrom(place, &st)
	if err != nil {
		slog.Warn("a copy cannot be looked at", "disk", c.Disk, "error", err)
		return nil, false, nil
	}
	return &catalog.SeenCopy{Size: found.Size, MtimeNS: found.MtimeNS, CtimeNS: found.CtimeNS, BtimeNS: found.BtimeNS, FileKey: found.FileKey,
		Nlink: found.Nlink}, true, nil
}

// show gives a copy's row what its disk holds now, for what is reported and what a move compares.
func show(c *catalog.CopyRow, seen *catalog.SeenCopy) {
	c.Size = sql.NullInt64{Int64: seen.Size, Valid: true}
	c.MtimeNS = sql.NullInt64{Int64: seen.MtimeNS, Valid: true}
	c.CtimeNS = sql.NullInt64{Int64: seen.CtimeNS, Valid: true}
	c.BtimeNS = sql.NullInt64{}
	if seen.BtimeNS != nil {
		c.BtimeNS = sql.NullInt64{Int64: *seen.BtimeNS, Valid: true}
	}
	c.FileKey = sql.NullString{String: seen.FileKey, Valid: true}
	c.Nlink = sql.NullInt64{Int64: seen.Nlink, Valid: true}
}
