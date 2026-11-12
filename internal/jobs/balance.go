package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// The outcomes of a move, besides those of a copy and of a move to the trash (specs.md §13.3).
const (
	// OutcomeMoved is a copy made where the setting wants it, and the old one in the trash.
	OutcomeMoved = "moved"
	// OutcomeToMove is a move a run with --yes would make.
	OutcomeToMove = "to-move"
	// OutcomeNoTarget is a copy with no disk the setting allows that has room: it stays.
	OutcomeNoTarget = "no-target"
	// OutcomeCopied is a copy made, while the old one stays, as the plan still keeps it.
	OutcomeCopied = "copied"
)

// BalanceItem is one move: a copy made on To, then the copy on From to the trash. To is empty with no target.
type BalanceItem struct {
	Path    string
	From    string
	To      string
	Size    int64
	Outcome string
}

// BalanceSummary is what a run of atoll balance did.
type BalanceSummary struct {
	Applied bool
	// Bytes counts the files moved, or that would be.
	Bytes int64
	// ToDo counts what a run with --yes would move, and Stayed what stays where it is.
	ToDo, Stayed int
	DiskLost     bool
	// Disabled says that replication.enabled is false, so that nothing was copied.
	Disabled bool
	// Left says that the run stopped at its time with files still to move.
	Left bool
}

// BalanceMeasure is what balance.trigger compares (specs.md §13.3): the bytes of every file, those of the files
// with a misplaced copy, and those their misplaced copies hold, which a move copies.
type BalanceMeasure struct {
	Total, Misplaced, ToMove int64
}

// MeasureBalance measures what balance.trigger compares, from a plan of the whole pool, writing nothing.
func MeasureBalance(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine) (*BalanceMeasure, error) {
	misplaced, _, err := planBalance(ctx, db, c, m.Now())
	if err != nil {
		return nil, err
	}
	out := &BalanceMeasure{}
	if out.Total, err = catalog.TotalBytes(ctx, db); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for path, disks := range misplaced {
		f, err := catalog.FileAt(ctx, db, path)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if f != nil {
			out.Misplaced += f.Size
			out.ToMove += f.Size * int64(len(disks))
		}
	}
	return out, nil
}

// planBalance makes the plan of atoll balance: the misplaced copies of each file, by disk position, and the copies
// it wants of those files, writing nothing.
func planBalance(ctx context.Context, db *sql.DB, c *config.Config, now int64) (map[string][]string, map[string][]watch.PlannedJob, error) {
	misplaced := map[string][]string{}
	var planned []watch.PlannedJob
	rep := &watch.Replicate{Balance: true, DryRun: true,
		Queue: func(jobs []watch.PlannedJob) error {
			planned = append(planned, jobs...)
			return nil
		},
		Misplaced: func(path string, disks []string) error {
			misplaced[path] = disks
			return nil
		}}
	if err := watch.SettleFor(ctx, db, c, now, rep); err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	byPath := map[string][]watch.PlannedJob{}
	for _, j := range planned {
		if misplaced[j.Path] != nil {
			byPath[j.Path] = append(byPath[j.Path], j)
		}
	}
	return misplaced, byPath, nil
}

// Balance moves the Atoll copies that break a strict setting of their file (specs.md §13.3). For each, a copy is
// made where the setting wants it, through the copy pipeline; the plan of the pool, made again, then marks the old
// copy surplus, as the new one stands in for it, and the old one goes to the trash as rebalance. A user copy never
// moves. Only the files with a copy to move get a job. A copy made by a run stopped before its end is found again by
// the next, whose plan has no job for it, and its old copy goes then. Without apply, it says what it would move, and
// writes nothing. With until, Unix nanoseconds, it starts no file past that time, and says so in Left.
func Balance(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool, until int64, each func(*BalanceItem) error) (*BalanceSummary, error) {
	sum := &BalanceSummary{Applied: apply}
	if enabled, _ := c.Effective()["replication.enabled"].(bool); !enabled {
		sum.Disabled = true
		return sum, nil
	}
	session, err := trash.Open(ctx, db, c, m, apply)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	now := m.Now()
	// The copies the plan wants of the files with a copy to move, and of no other.
	misplaced, byPath, err := planBalance(ctx, db, c, now)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(misplaced))
	for path := range misplaced {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	b := &balancer{db: db, session: session, sum: sum, each: each}
	if !apply {
		for _, path := range paths {
			if err := b.preview(ctx, path, misplaced[path], byPath[path]); err != nil {
				return nil, err
			}
		}
		return sum, nil
	}
	made := map[string][]ReplicateItem{}
	r := &replicator{db: db, config: c, machine: m, session: session, now: now, sum: &ReplicateSummary{Applied: true}, each: func(item *ReplicateItem) error {
		made[item.Path] = append(made[item.Path], *item)
		return nil
	}}
	if err := r.positions(ctx); err != nil {
		return nil, err
	}
	for i, path := range paths {
		if until > 0 && m.Now() >= until {
			sum.Left, paths = true, paths[:i]
			break
		}
		if err := queue(ctx, db, now, byPath[path]); err != nil {
			return nil, err
		}
		if err := r.runJobs(ctx, path); err != nil {
			return nil, err
		}
	}
	// The plan of the pool, with the copies made: an old copy a new one stands in for is surplus now.
	if err := watch.Settle(ctx, db, c, now); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	for _, path := range paths {
		if err := b.finish(ctx, path, misplaced[path], made[path]); err != nil {
			return nil, err
		}
	}
	if err := session.Finish(ctx); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	sum.DiskLost = session.Lost()
	return sum, nil
}

type balancer struct {
	db      *sql.DB
	session *trash.Session
	sum     *BalanceSummary
	each    func(*BalanceItem) error
}

// preview says what a run with --yes would move of one file: each copy that breaks a setting, with the disk the plan
// gives its replacement, in order, or none.
func (b *balancer) preview(ctx context.Context, path string, from []string, jobs []watch.PlannedJob) error {
	f, err := catalog.FileAt(ctx, b.db, path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	var size int64
	if f != nil {
		size = f.Size
	}
	for i, disk := range from {
		item := BalanceItem{Path: path, From: disk, Size: size, Outcome: OutcomeNoTarget}
		switch {
		case i < len(jobs):
			item.To, item.Outcome = jobs[i].Dst, OutcomeToMove
		case replaced(f, disk):
			// Made by a run stopped before its end: only the old copy's move to the trash is left.
			item.Outcome = OutcomeToMove
		}
		if item.Outcome == OutcomeToMove {
			b.sum.ToDo++
			b.sum.Bytes += size
		} else {
			b.sum.Stayed++
		}
		if err := b.each(&item); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}

// finish sends to the trash each copy of a file that a copy made stands in for, which the plan marked surplus.
func (b *balancer) finish(ctx context.Context, path string, from []string, made []ReplicateItem) error {
	f, err := catalog.FileAt(ctx, b.db, path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i, disk := range from {
		item := BalanceItem{Path: path, From: disk, Outcome: OutcomeNoTarget}
		if f != nil {
			item.Size = f.Size
		}
		switch {
		case i < len(made):
			item.To, item.Size, item.Outcome = made[i].Disk, made[i].Size, made[i].Outcome
		case replaced(f, disk):
			// Made by a run stopped before its end: its old copy goes now.
			item.Outcome = OutcomeReplicated
		}
		if item.Outcome == OutcomeReplicated {
			if item.Outcome, err = b.retire(ctx, f, disk); err != nil {
				return err
			}
		}
		if item.Outcome == OutcomeMoved {
			b.sum.Bytes += item.Size
		} else {
			b.sum.Stayed++
		}
		if err := b.each(&item); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}

// retire sends an old copy to the trash, as rebalance, when the plan marked it surplus: never a user copy.
func (b *balancer) retire(ctx context.Context, f *catalog.FileRow, disk string) (string, error) {
	if f == nil {
		return OutcomeCopied, nil
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
	if at < 0 || f.Copies[at].State != string(solver.CopySurplus) || f.Copies[at].CreatedBy != solver.CreatedByAtoll {
		return OutcomeCopied, nil
	}
	outcome, err := b.session.Send(ctx, f, &f.Copies[at], catalog.ReasonRebalance)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if outcome == trash.OutcomeTrashed {
		return OutcomeMoved, nil
	}
	return outcome, nil
}

// replaced says that a file's Atoll copy on a disk is already surplus: a copy elsewhere stands in for it.
func replaced(f *catalog.FileRow, disk string) bool {
	if f == nil {
		return false
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
	return at >= 0 && f.Copies[at].State == string(solver.CopySurplus) && f.Copies[at].CreatedBy == solver.CreatedByAtoll
}
