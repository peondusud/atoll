package jobs

import (
	"cmp"
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

// MeasureBalance measures what balance.trigger compares, from a plan of the whole pool, writing nothing. A user copy is
// never misplaced there: no setting moves one (specs.md §12.4).
func MeasureBalance(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine) (*BalanceMeasure, error) {
	misplaced, _, err := planBalance(ctx, db, c, m.Now(), false)
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

// MisplacedFiles gives the files with a misplaced copy, from the plan MeasureBalance makes, writing nothing: the bytes
// of the others are those correctly placed (specs.md §13.3, §15.11).
func MisplacedFiles(ctx context.Context, db *sql.DB, c *config.Config, now int64) (map[string]bool, error) {
	misplaced, _, err := planBalance(ctx, db, c, now, false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(misplaced))
	for path := range misplaced {
		out[path] = true
	}
	return out, nil
}

// planBalance makes the plan of atoll balance: the misplaced copies of each file, by disk position, and the copies
// it wants of those files, writing nothing. With originals, a user copy can be misplaced too.
func planBalance(ctx context.Context, db *sql.DB, c *config.Config, now int64, originals bool) (map[string][]string, map[string][]watch.PlannedJob, error) {
	misplaced := map[string][]string{}
	var planned []watch.PlannedJob
	rep := &watch.Replicate{Balance: true, DryRun: true, MoveOriginals: originals,
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
// copy surplus, as the new one stands in for it, and the old one goes to the trash as rebalance. A user copy moves
// only with originals, atoll balance --move-originals: it moves first, and the copy made in its place becomes the
// user copy as it is recorded, the old one an Atoll copy, which then goes the same way. Only the files with a copy to
// move get a job. A copy made by a run stopped before its end is found again by the next, whose plan has no job for
// it, and its old copy goes then. Without apply, it says what it would move, and writes nothing. With until, Unix
// nanoseconds, it starts no file past that time, and says so in Left.
func Balance(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool, until int64, originals bool, each func(*BalanceItem) error) (*BalanceSummary, error) {
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
	misplaced, byPath, err := planBalance(ctx, db, c, now, originals)
	if err != nil {
		return nil, err
	}
	// A user copy moves first: the first copies made stand in for those, in that order (specs.md §13.3).
	users := map[string]int{}
	if originals {
		for path, from := range misplaced {
			if misplaced[path], users[path], err = usersFirst(ctx, db, path, from); err != nil {
				return nil, err
			}
		}
	}
	paths := make([]string, 0, len(misplaced))
	for path := range misplaced {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	b := &balancer{db: db, session: session, sum: sum, each: each, users: users, heirs: map[string]map[string]string{}}
	if originals {
		if b.disks, err = catalog.Disks(ctx, db); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
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
		if err := queueMoves(ctx, db, now, byPath[path], misplaced[path][:users[path]]); err != nil {
			return nil, err
		}
		if err := r.runJobs(ctx, path); err != nil {
			return nil, err
		}
	}
	for _, path := range paths {
		if err := b.handOver(ctx, path, misplaced[path], len(byPath[path])); err != nil {
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

// usersFirst puts a file's misplaced user copies before its Atoll ones, each by disk position, and says how many there
// are.
func usersFirst(ctx context.Context, db *sql.DB, path string, from []string) ([]string, int, error) {
	f, err := catalog.FileAt(ctx, db, path)
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return from, 0, nil
	}
	isUser := func(disk string) bool {
		at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
		return at >= 0 && f.Copies[at].CreatedBy == solver.CreatedByUser
	}
	var users, others []string
	for _, disk := range from {
		if isUser(disk) {
			users = append(users, disk)
		} else {
			others = append(others, disk)
		}
	}
	return append(users, others...), len(users), nil
}

// queueMoves queues the copies of a file's moves, the first ones each standing in for a user copy of replaced, in
// order.
func queueMoves(ctx context.Context, db *sql.DB, now int64, jobs []watch.PlannedJob, replaced []string) error {
	list := make([]catalog.Queue, 0, len(jobs))
	for i := range jobs {
		q := catalog.Queue{FileID: jobs[i].FileID, Src: jobs[i].Src, Dst: jobs[i].Dst, Priority: jobs[i].Priority}
		if i < len(replaced) {
			q.Replaces = replaced[i]
		}
		list = append(list, q)
	}
	if err := catalog.QueueReplicate(ctx, db, now, list); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// BalanceAtoll is Balance as the daemon runs it, with until: it moves Atoll's copies alone, as no setting moves a user
// copy (specs.md §12.4).
func BalanceAtoll(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, until int64, each func(*BalanceItem) error) (*BalanceSummary, error) {
	return Balance(ctx, db, c, m, true, until, false, each)
}

type balancer struct {
	db      *sql.DB
	session *trash.Session
	sum     *BalanceSummary
	each    func(*BalanceItem) error
	// users counts, by path, the misplaced user copies, which come first of its misplaced copies; heirs holds, by path
	// and disk, the copy each that no copy stands in for handed its part to; disks are the catalog's, with
	// --move-originals.
	users map[string]int
	heirs map[string]map[string]string
	disks []catalog.Disk
}

// heir gives the copy a misplaced user copy hands its part to when the plan makes none in its place, as the file's
// copies that break nothing, ok or surplus on a disk that can be read, already reach its level: the first of them, user
// copies first, then by first sight and by disk position. None when they do not reach it, as the user copy is needed.
// A surplus copy counts: the plan of a pass marks one so while it keeps the user copy that breaks the setting.
func (b *balancer) heir(f *catalog.FileRow, from []string) string {
	readable := map[string]int{}
	for i := range b.disks {
		switch solver.DiskState(b.disks[i].State) {
		case solver.DiskOnline, solver.DiskStandby, solver.DiskReadOnly:
			readable[b.disks[i].ID] = b.disks[i].Position
		case solver.DiskMissing, solver.DiskDraining, solver.DiskFailed, solver.DiskRetired:
		}
	}
	var others []catalog.CopyRow
	for _, c := range f.Copies {
		inSync := c.State == string(solver.CopyOK) || c.State == string(solver.CopySurplus)
		if _, ok := readable[c.Disk]; ok && inSync && !slices.Contains(from, c.Disk) {
			others = append(others, c)
		}
	}
	if len(others) < f.ReplicasTarget+1 {
		return ""
	}
	slices.SortFunc(others, func(x, y catalog.CopyRow) int {
		return cmp.Or(boolFirst(x.CreatedBy == solver.CreatedByUser, y.CreatedBy == solver.CreatedByUser), cmp.Compare(x.FirstSeenNS, y.FirstSeenNS),
			cmp.Compare(readable[x.Disk], readable[y.Disk]))
	})
	return others[0].Disk
}

// boolFirst orders true before false.
func boolFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

// handOver gives the part of each misplaced user copy of a file for which the plan makes no copy, past the planned
// ones, to its heir: it then goes as an Atoll copy does (specs.md §13.3).
func (b *balancer) handOver(ctx context.Context, path string, from []string, planned int) error {
	if b.users[path] <= planned {
		return nil
	}
	f, err := catalog.FileAt(ctx, b.db, path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil
	}
	for _, disk := range from[planned:b.users[path]] {
		heir := b.heir(f, from)
		if heir == "" {
			continue
		}
		if err := catalog.HandOver(ctx, b.db, f.ID, disk, heir); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if b.heirs[path] == nil {
			b.heirs[path] = map[string]string{}
		}
		b.heirs[path][disk] = heir
	}
	return nil
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
		heir := ""
		if i >= len(jobs) && i < b.users[path] && f != nil {
			heir = b.heir(f, from)
		}
		switch {
		case i < len(jobs):
			item.To, item.Outcome = jobs[i].Dst, OutcomeToMove
		case replaced(f, disk):
			// Made by a run stopped before its end: only the old copy's move to the trash is left, if it may go.
			if item.Outcome, err = b.retire(ctx, f, disk); err != nil {
				return err
			}
		case heir != "":
			// A user copy no copy need stand in for: its part goes to a copy that is where the setting wants it.
			item.To, item.Outcome = heir, OutcomeToMove
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
			// Made by a run stopped before its end, or a user copy that handed its part over: it goes now.
			item.To, item.Outcome = b.heirs[path][disk], OutcomeReplicated
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

// retire sends an old copy to the trash, as rebalance, when the plan marked it surplus, never a user copy, and only as
// atoll prune would send it (specs.md §17.3): the plan marks a copy surplus for a copy still to make too, which stands
// in for nothing until it is made. In a run that may not write, it says that the copy is to move.
func (b *balancer) retire(ctx context.Context, f *catalog.FileRow, disk string) (string, error) {
	if !replaced(f, disk) {
		return OutcomeCopied, nil
	}
	outcome, err := b.session.Retire(ctx, f, disk, catalog.ReasonRebalance)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	switch outcome {
	case trash.OutcomeTrashed:
		return OutcomeMoved, nil
	case string(solver.PruneToTrash):
		return OutcomeToMove, nil
	case "":
		return OutcomeCopied, nil
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
