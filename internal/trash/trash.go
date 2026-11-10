package trash

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/watch"
)

// The outcomes of a copy or an entry, besides those of solver.PruneOutcome (specs.md §17.3, §17.4).
const (
	OutcomeTrashed       = "trashed"
	OutcomeRestored      = "restored"
	OutcomeRenamed       = "renamed"
	OutcomeToRestore     = "to-restore"
	OutcomePurged        = "purged"
	OutcomeToPurge       = "to-purge"
	OutcomePathTaken     = "path-taken"
	OutcomeNameRefused   = "name-refused"
	OutcomeNameCollision = "name-collision"
	OutcomeIOError       = "io-error"
	OutcomeChanged       = string(solver.PruneChanged)
	outcomeUnavailable   = string(solver.PruneDiskUnavailable)
)

const reasonSurplus = "surplus"

// slot is what a session knows of one disk.
type slot struct {
	config *config.Disk // nil for a disk the configuration no longer lists
	state  solver.DiskState
	handle *ops.Disk
	tried  bool
}

// Session is one run of a command that writes: it confirms each disk once, before it first reads or
// writes it, and resolves what a crash left (specs.md §10, §17.2). Without apply it writes nothing
// to a disk, and looks at them through what a mount check found.
type Session struct {
	db        *sql.DB
	config    *config.Config
	machine   disks.Machine
	apply     bool
	now       int64
	retention time.Duration
	slots     map[string]*slot
	verdicts  []disks.Verdict
	changed   bool
	lost      bool
	// reprised holds, by path, the restore this run's reprise finished: an entry from before it is
	// what it was asked for, and one from after it is not.
	reprised map[string]int64
	// repriseTrashed holds the operations whose copy reached the trash in this run's reprise.
	repriseTrashed map[int64]bool
}

// Open checks every disk and, when it is going to write, resolves what a crash left.
func Open(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool) (*Session, error) {
	s := &Session{
		db: db, config: c, machine: m, apply: apply, now: m.Now(), slots: map[string]*slot{},
		reprised: map[string]int64{}, repriseTrashed: map[int64]bool{},
	}
	text, _ := c.Effective()["trash.retention"].(string)
	retention, err := value.ParseDuration(text)
	if err != nil {
		return nil, fmt.Errorf("trash.retention: %w", err)
	}
	s.retention = retention
	if s.verdicts, err = watch.CheckDisks(ctx, db, c, m, s.now); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	rows, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for i := range rows {
		s.slots[rows[i].ID] = &slot{state: solver.DiskState(rows[i].State)}
	}
	for i := range c.Disks {
		if sl := s.slots[c.Disks[i].ID]; sl != nil {
			sl.config = &c.Disks[i]
		}
	}
	if apply {
		if err := s.recoverAll(ctx); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// VerdictOf says what the mount check found of a disk of the configuration, by its order there.
func (s *Session) VerdictOf(i int) disks.Verdict {
	return s.verdicts[i]
}

// Readable says whether a disk can still be looked at in this session.
func (s *Session) Readable(id string) bool {
	sl := s.slots[id]
	return sl != nil && readable(sl.state)
}

// Writable says whether a disk can still be written to in this session.
func (s *Session) Writable(id string) bool {
	return s.writable(id)
}

// Handle gives a handle on a disk, confirmed if this session writes, and nil when it cannot be reached.
func (s *Session) Handle(ctx context.Context, id string) (*ops.Disk, error) {
	return s.open(ctx, id)
}

// Lose records that a disk cannot be relied on any more.
func (s *Session) Lose(ctx context.Context, id string, why error) error {
	return s.lose(ctx, id, why)
}

// MarkMissing says that a look found a disk missing.
func (s *Session) MarkMissing(ctx context.Context, id string) error {
	sl := s.slots[id]
	if sl == nil {
		return nil
	}
	sl.state = solver.DiskMissing
	sl.tried = true
	if sl.handle != nil {
		sl.handle.Close()
		sl.handle = nil
	}
	if !s.apply {
		return nil
	}
	if err := catalog.SetDiskState(ctx, s.db, id, string(disks.Missing)); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// MarkChanged says that something moved, so that each file is worked out again.
func (s *Session) MarkChanged() {
	s.changed = true
}

// Lost says that a disk could not be relied on, or an operation stayed planned, in this run.
func (s *Session) Lost() bool {
	return s.lost
}

// Settle works out the original, policy and state of each file, whatever happened.
func (s *Session) Settle(ctx context.Context) error {
	if err := watch.Settle(ctx, s.db, s.config, s.now); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// Close lets go of the disks.
func (s *Session) Close() {
	for _, sl := range s.slots {
		if sl.handle != nil {
			sl.handle.Close()
			sl.handle = nil
		}
	}
}

func (s *Session) recoverAll(ctx context.Context) error {
	planned, err := catalog.PlannedOps(ctx, s.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	seen := map[string]bool{}
	for i := range planned {
		id := planned[i].Disk
		if seen[id] {
			continue
		}
		seen[id] = true
		if !s.writable(id) {
			// It waits for that disk, and stays planned, which the exit code says (specs.md §17.2).
			s.lost = true
			continue
		}
		handle, err := s.open(ctx, id)
		if err != nil {
			return err
		}
		if handle == nil || !s.writable(id) {
			s.lost = true
			continue
		}
		recovered, err := handle.Recover(ctx, s.db, planned, s.now, s.retention)
		s.changed = s.changed || recovered.Finished
		for _, r := range recovered.Restored {
			s.reprised[r.Path] = max(s.reprised[r.Path], r.Op)
		}
		for _, op := range recovered.Trashed {
			s.repriseTrashed[op] = true
		}
		switch {
		case errors.Is(err, ops.ErrDiskLost):
			if err := s.lose(ctx, id, err); err != nil {
				return err
			}
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("%w", err)
		case err != nil:
			s.lost = true
		}
	}
	return nil
}

// open gives a handle on a disk, and nil when it cannot be reached. Going to write, it confirms the
// disk, once: the handle first, so that what is confirmed is what the handle leads to, and an
// unmount in between cannot pass an empty mount point as the disk (specs.md §10).
func (s *Session) open(ctx context.Context, id string) (*ops.Disk, error) {
	sl := s.slots[id]
	if sl == nil {
		return nil, nil //nolint:nilnil // a disk nothing knows is one that cannot be reached
	}
	if sl.tried {
		return sl.handle, nil
	}
	sl.tried = true
	if sl.config == nil || !readable(sl.state) {
		return nil, nil //nolint:nilnil // likewise
	}
	handle, err := ops.Open(s.machine, sl.config)
	if err != nil {
		return nil, s.lose(ctx, id, err)
	}
	if s.apply {
		verdict := disks.Confirm(s.machine, sl.config)
		if verdict.State == disks.Missing {
			handle.Close()
			return nil, s.lose(ctx, id, fmt.Errorf("%s: %s", verdict.Reason, verdict.Message))
		}
		if err := handle.Verify(); err != nil {
			handle.Close()
			return nil, s.lose(ctx, id, err)
		}
		if state := solver.DiskState(verdict.State); state != sl.state {
			sl.state = state
			if err := catalog.SetDiskState(ctx, s.db, id, string(verdict.State)); err != nil {
				handle.Close()
				return nil, fmt.Errorf("catalog: %w", err)
			}
		}
		if s.writable(id) {
			if err := s.cleanTmp(ctx, id, handle); err != nil {
				handle.Close()
				return nil, err
			}
		}
	}
	sl.handle = handle
	return handle, nil
}

// cleanTmp removes what copies done or undone left in the disk's tmp/, now that a command that writes has
// it open, so that no disk is woken for this alone (specs.md §14.2). A disk that fails there is lost; any
// other failure leaves the parts for the next time.
func (s *Session) cleanTmp(ctx context.Context, id string, handle *ops.Disk) error {
	named, err := catalog.PartOps(ctx, s.db, id)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if len(named) == 0 {
		return nil
	}
	keep, err := catalog.PartsInUse(ctx, s.db, id)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	switch err := handle.CleanTmp(named, keep); {
	case errors.Is(err, ops.ErrDiskLost):
		return s.lose(ctx, id, err)
	case err != nil:
		slog.Warn("parts stay in tmp/", "disk", id, "error", err)
	}
	return nil
}

func readable(state solver.DiskState) bool {
	return state == solver.DiskOnline || state == solver.DiskStandby || state == solver.DiskReadOnly
}

func (s *Session) writable(id string) bool {
	sl := s.slots[id]
	return sl != nil && (sl.state == solver.DiskOnline || sl.state == solver.DiskStandby)
}

// lose records that a disk cannot be relied on any more, and what ran on it is over.
func (s *Session) lose(ctx context.Context, id string, why error) error {
	slog.Warn("a disk cannot be relied on", "disk", id, "error", why)
	sl := s.slots[id]
	sl.state = solver.DiskMissing
	sl.tried = true
	s.lost = true
	if sl.handle != nil {
		sl.handle.Close()
		sl.handle = nil
	}
	if !s.apply {
		return nil
	}
	if err := catalog.SetDiskState(ctx, s.db, id, string(disks.Missing)); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// Finish works out the original, policy and state of each file again, if anything moved.
func (s *Session) Finish(ctx context.Context) error {
	if !s.changed {
		return nil
	}
	if err := watch.Settle(ctx, s.db, s.config, s.now); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// Summary says how a command went.
type Summary struct {
	Applied bool
	Bytes   int64
	// ToDo counts what it would have done, had it been allowed to write.
	ToDo int
	// Stayed counts what stayed where it is.
	Stayed int
	// DiskLost says that a disk could not be relied on, or an operation stayed planned, in this run,
	// whatever else is listed.
	DiskLost bool
}

// PruneItem is the outcome of one surplus copy.
type PruneItem struct {
	Path    string
	Disk    string
	Size    int64
	Outcome string
}

// Prune moves the surplus copies to the trash (specs.md §17.3), and gives each outcome to each, by
// path in byte order, then by position.
func (s *Session) Prune(ctx context.Context, each func(*PruneItem) error) (*Summary, error) {
	summary := &Summary{Applied: s.apply}
	position, err := s.positions(ctx)
	if err != nil {
		return nil, err
	}
	reader := catalog.NewFileReader(s.db)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			summary.DiskLost = s.lost
			return summary, nil
		}
		for i := range page {
			if err := s.pruneFile(ctx, &page[i], position, summary, each); err != nil {
				return nil, err
			}
		}
	}
}

func (s *Session) positions(ctx context.Context) (map[string]int, error) {
	rows, err := catalog.Disks(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := make(map[string]int, len(rows))
	for i := range rows {
		out[rows[i].ID] = rows[i].Position
	}
	return out, nil
}

func hasSurplus(f *catalog.FileRow) bool {
	return slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return c.State == string(solver.CopySurplus) })
}

// wantedReplicas is the larger of what the last scan recorded and what the configuration says now,
// so that a copy now wanted does not go on the strength of an old mark (specs.md §17.3).
func (s *Session) wantedReplicas(f *catalog.FileRow) int {
	size := f.Size
	policy := s.config.Policy(f.Path, &size)
	if policy.Action != rules.Ignore && policy.Replicas > f.ReplicasTarget {
		return policy.Replicas
	}
	return f.ReplicasTarget
}

func (s *Session) pruneFile(ctx context.Context, f *catalog.FileRow, position map[string]int, summary *Summary, each func(*PruneItem) error) error {
	if !hasSurplus(f) {
		return nil
	}
	slices.SortFunc(f.Copies, func(a, b catalog.CopyRow) int { return cmp.Compare(position[a.Disk], position[b.Disk]) })
	if err := s.refreshCtimes(ctx, f); err != nil {
		return err
	}
	input := solver.PruneInput{State: solver.State(f.State), Replicas: s.wantedReplicas(f), HasHash: f.Hash != nil}
	for i := range f.Copies {
		c := &f.Copies[i]
		unchanged, err := s.unchanged(ctx, f.Path, c)
		if err != nil {
			return err
		}
		state := solver.DiskMissing
		if sl := s.slots[c.Disk]; sl != nil {
			state = sl.state
		}
		input.Copies = append(input.Copies, solver.PruneCopy{
			Disk: c.Disk, Position: position[c.Disk], State: solver.CopyState(c.State), CreatedBy: c.CreatedBy,
			DiskState: state, Verified: c.VerifiedNS.Valid, Unchanged: unchanged,
		})
	}
	results, err := solver.DecidePrune(input)
	if err != nil {
		return fmt.Errorf("%q: %w", f.Path, err)
	}
	for _, r := range results {
		c := copyOn(f, r.Disk)
		outcome := string(r.Outcome)
		if r.Outcome == solver.PruneToTrash && s.apply {
			if outcome, err = s.Send(ctx, f, c, reasonSurplus); err != nil {
				return err
			}
		}
		switch outcome {
		case string(solver.PruneToTrash):
			summary.ToDo++
			summary.Bytes += c.Size.Int64
		case OutcomeTrashed:
			summary.Bytes += c.Size.Int64
		default:
			summary.Stayed++
		}
		if err := each(&PruneItem{Path: f.Path, Disk: c.Disk, Size: c.Size.Int64, Outcome: outcome}); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}

// refreshCtimes reads again what the copies of a file record, once this run has moved anything: this
// page was read before that, and a move changes the change time of every name of a file (specs.md
// §17.1), which must not read as a change of the others.
func (s *Session) refreshCtimes(ctx context.Context, f *catalog.FileRow) error {
	if !s.changed {
		return nil
	}
	ctimes, err := catalog.CopyCtimes(ctx, s.db, f.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i := range f.Copies {
		if ctime, ok := ctimes[f.Copies[i].Disk]; ok {
			f.Copies[i].CtimeNS = ctime
		}
	}
	return nil
}

func copyOn(f *catalog.FileRow, disk string) *catalog.CopyRow {
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
	return &f.Copies[at]
}

// unchanged looks at a copy, and says whether it is what its row says. A copy that cannot be looked
// at is not, as nothing can be said of it.
func (s *Session) unchanged(ctx context.Context, path string, c *catalog.CopyRow) (bool, error) {
	if c.OldPath != nil {
		// It has not followed a rename yet: it waits for that (specs.md §17.3).
		return false, nil
	}
	handle, err := s.open(ctx, c.Disk)
	if err != nil || handle == nil {
		return false, err
	}
	st, there, err := handle.StatRoot(path)
	if err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return false, s.lose(ctx, c.Disk, err)
		}
		slog.Warn("a copy cannot be looked at", "disk", c.Disk, "error", err)
		return false, nil
	}
	if !there || st.Kind != fsx.KindFile {
		return false, nil
	}
	found, err := watch.FileFrom(path, &st)
	if err != nil {
		slog.Warn("a copy cannot be looked at", "disk", c.Disk, "error", err)
		return false, nil
	}
	return watch.Unchanged(&found, &catalog.KnownCopy{Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}), nil
}

// Send sends one copy to the trash, and says what became of it (specs.md §17.1).
func (s *Session) Send(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, reason string) (string, error) {
	place := c.Place(f)
	handle, err := s.open(ctx, c.Disk)
	if err != nil {
		return "", err
	}
	if handle == nil {
		return outcomeUnavailable, nil
	}
	op, err := catalog.BeginTrash(ctx, s.db, &catalog.TrashStart{
		Disk: c.Disk, Path: place, FileKey: c.FileKey.String, Size: c.Size, Hash: f.Hash, Reason: reason,
		NowNS: s.now, ExpiresNS: ops.AddRetention(s.now, s.retention),
	})
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	moved, moveErr := handle.ToTrash(op, &catalog.KnownCopy{Path: place, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey})
	if moveErr == nil {
		if err := catalog.FinishTrash(ctx, s.db, op, nil, moved.Links()); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		s.changed = true
		return OutcomeTrashed, nil
	}
	return s.afterFailedMove(ctx, handle, op, moveErr)
}

// AfterFailedMove is afterFailedMove for what else moves things.
func (s *Session) AfterFailedMove(ctx context.Context, handle *ops.Disk, op *ops.Op, moveErr error) (string, error) {
	return s.afterFailedMove(ctx, handle, op, moveErr)
}

// afterFailedMove resolves an operation whose move failed by looking at what is there, as a reprise
// does, and says what to report (specs.md §17.1).
func (s *Session) afterFailedMove(ctx context.Context, handle *ops.Disk, op *ops.Op, moveErr error) (string, error) {
	slog.Warn("a move did not happen", "disk", op.Disk, "op", op.ID, "error", moveErr)
	switch {
	case errors.Is(moveErr, ops.ErrDiskLost):
		return OutcomeIOError, s.lose(ctx, op.Disk, moveErr)
	case errors.Is(moveErr, ops.ErrOtherMount):
		// Refused before anything moved, and no look can go through it.
		if err := handle.Undo(ctx, s.db, op, false, s.now); err != nil {
			return "", fmt.Errorf("%w", err)
		}
		return failedOutcome(op, moveErr), nil
	}
	finished, err := handle.ResolveNow(ctx, s.db, op, s.now, s.retention)
	if err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, op.Disk, err)
		}
		slog.Warn("an operation stays planned", "disk", op.Disk, "op", op.ID, "error", err)
		s.lost = true
		return OutcomeIOError, nil
	}
	if finished {
		s.changed = true
		switch op.Kind {
		case catalog.OpRestore:
			return OutcomeRestored, nil
		case catalog.OpRename:
			return OutcomeRenamed, nil
		}
		return OutcomeTrashed, nil
	}
	return failedOutcome(op, moveErr), nil
}

// failedOutcome says what became of an operation that did not happen. A path taken is a restore's
// outcome: the place of a copy in the trash holds nothing of the user's that they could act on.
func failedOutcome(op *ops.Op, moveErr error) string {
	switch {
	case errors.Is(moveErr, ops.ErrNameRefused):
		return OutcomeNameRefused
	case errors.Is(moveErr, ops.ErrPathTaken) && (op.Kind == catalog.OpRestore || op.Kind == catalog.OpRename):
		return OutcomePathTaken
	case errors.Is(moveErr, ops.ErrGone):
		return OutcomeChanged
	default:
		return OutcomeIOError
	}
}

// EntryItem is the outcome of one entry of the trash.
type EntryItem struct {
	catalog.TrashEntry
	Outcome string
}

// Restore puts back, for each path at or below under, its newest entry (specs.md §17.4). An entry from
// before a restore of its path that a reprise finished in this run is left alone: that restore was what
// this asks for.
func (s *Session) Restore(ctx context.Context, under string, each func(*EntryItem) error) (*Summary, error) {
	entries, err := catalog.TrashEntries(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	summary := &Summary{Applied: s.apply}
	for i := range entries {
		e := &entries[i]
		// By path, then by operation, so the last of a path is its newest.
		if !atOrBelow(e.Path, under) || (i+1 < len(entries) && entries[i+1].Path == e.Path) || e.OpID < s.reprised[e.Path] {
			continue
		}
		outcome, err := s.restoreEntry(ctx, e)
		if err != nil {
			return nil, err
		}
		switch outcome {
		case OutcomeToRestore:
			summary.ToDo++
		case OutcomeRestored:
		default:
			summary.Stayed++
		}
		if err := each(&EntryItem{TrashEntry: *e, Outcome: outcome}); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	summary.DiskLost = s.lost
	return summary, nil
}

func atOrBelow(path, under string) bool {
	return path == under || strings.HasPrefix(path, under+"/")
}

func (s *Session) restoreEntry(ctx context.Context, e *catalog.TrashEntry) (string, error) {
	if !s.writable(e.Disk) {
		return outcomeUnavailable, nil
	}
	handle, err := s.open(ctx, e.Disk)
	if err != nil {
		return "", err
	}
	if handle == nil || !s.writable(e.Disk) {
		return outcomeUnavailable, nil
	}
	taken, missing, err := handle.LookAt(e.Path)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return outcomeUnavailable, s.lose(ctx, e.Disk, err)
	case err != nil:
		slog.Warn("a path cannot be looked at", "disk", e.Disk, "error", err)
		return OutcomeIOError, nil
	case taken:
		return OutcomePathTaken, nil
	}
	behind, err := catalog.TakenByRename(ctx, s.db, e.Disk, e.Path)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	if behind {
		return OutcomePathTaken, nil
	}
	collides, err := s.collides(ctx, e)
	if err != nil {
		return "", err
	}
	if collides {
		return OutcomeNameCollision, nil
	}
	if !s.apply {
		return OutcomeToRestore, nil
	}
	op, err := catalog.BeginFromTrash(ctx, s.db, catalog.OpRestore, e, s.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	for _, folder := range missing {
		if err := s.makeFolder(ctx, handle, e, folder); err != nil {
			if errors.As(err, new(catalogError)) {
				return "", err
			}
			return s.afterFailedMove(ctx, handle, op, err)
		}
	}
	moved, moveErr := handle.FromTrash(op)
	if moveErr != nil {
		return s.afterFailedMove(ctx, handle, op, moveErr)
	}
	found, err := watch.FileFrom(op.Path, &moved.Stat)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if err := catalog.FinishRestore(ctx, s.db, op, s.now, ops.NewCopyOf(&found), moved.Links()); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	handle.CleanTrash(op.Path2)
	s.changed = true
	return OutcomeRestored, nil
}

// catalogError marks a failure of the catalog, which stops the whole command, among the failures of
// a disk, which stop only what ran on it.
type catalogError struct{ error }

// collides says whether an NTFS disk holds a name that collides with the path or a folder on the way
// to it: both names would be stored, and Windows would see one (specs.md §15.7).
func (s *Session) collides(ctx context.Context, e *catalog.TrashEntry) (bool, error) {
	if sl := s.slots[e.Disk]; sl == nil || sl.config == nil || sl.config.FS != names.NTFS {
		return false, nil
	}
	parts := strings.Split(e.Path, "/")
	for i := range parts {
		taken, err := catalog.KeyTaken(ctx, s.db, e.Disk, strings.Join(parts[:i+1], "/"))
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if taken {
			return true, nil
		}
	}
	return false, nil
}

// makeFolder makes one missing folder for a restore, as an operation of its own, with the mode and
// owner of the same folder in the trash. One made by someone else meanwhile is theirs, and fine.
func (s *Session) makeFolder(ctx context.Context, handle *ops.Disk, e *catalog.TrashEntry, folder string) error {
	op, err := catalog.BeginMkdir(ctx, s.db, e.Disk, folder, s.now)
	if err != nil {
		return catalogError{fmt.Errorf("catalog: %w", err)}
	}
	trashFolder := strings.TrimSuffix(e.TrashPath, e.Path) + folder
	like, err := handle.TrashFolderInfo(trashFolder)
	if err != nil {
		// Nothing can be said of the mode it should have, and a private folder must not come back open.
		// Nothing was made, whatever the error says of the disk.
		if undo := catalog.Undo(ctx, s.db, op, false, s.now); undo != nil {
			return catalogError{fmt.Errorf("catalog: %w", undo)}
		}
		return err
	}
	st, err := handle.MakeRootFolder(folder, like)
	switch {
	case err == nil:
		if err := handle.FinishMkdir(ctx, s.db, op, &st); err != nil {
			return catalogError{err}
		}
		return nil
	case errors.Is(err, ops.ErrDiskLost):
		return err
	case errors.Is(err, ops.ErrPathTaken):
		if err := catalog.Undo(ctx, s.db, op, false, s.now); err != nil {
			return catalogError{fmt.Errorf("catalog: %w", err)}
		}
		return nil
	default:
		if undo := catalog.Undo(ctx, s.db, op, false, s.now); undo != nil {
			return catalogError{fmt.Errorf("catalog: %w", undo)}
		}
		return err
	}
}

// Purge deletes what has reached its time, or with olderThan what was trashed at least that long
// ago (specs.md §17.4).
func (s *Session) Purge(ctx context.Context, olderThan *time.Duration, each func(*EntryItem) error) (*Summary, error) {
	entries, err := catalog.TrashEntries(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	summary := &Summary{Applied: s.apply}
	for i := range entries {
		e := &entries[i]
		if s.repriseTrashed[e.OpID] {
			continue
		}
		due := e.ExpiresNS <= s.now
		if olderThan != nil {
			due = e.DeletedNS <= s.now-olderThan.Nanoseconds()
		}
		if !due {
			continue
		}
		outcome, err := s.purgeEntry(ctx, e)
		if err != nil {
			return nil, err
		}
		switch outcome {
		case OutcomeToPurge:
			summary.ToDo++
			summary.Bytes += e.Size.Int64
		case OutcomePurged:
			summary.Bytes += e.Size.Int64
		default:
			summary.Stayed++
		}
		if err := each(&EntryItem{TrashEntry: *e, Outcome: outcome}); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	summary.DiskLost = s.lost
	return summary, nil
}

func (s *Session) purgeEntry(ctx context.Context, e *catalog.TrashEntry) (string, error) {
	if !s.writable(e.Disk) {
		return outcomeUnavailable, nil
	}
	if !s.apply {
		return OutcomeToPurge, nil
	}
	handle, err := s.open(ctx, e.Disk)
	if err != nil {
		return "", err
	}
	if handle == nil || !s.writable(e.Disk) {
		return outcomeUnavailable, nil
	}
	op, err := catalog.BeginFromTrash(ctx, s.db, catalog.OpUnlink, e, s.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	purged, err := handle.PurgeEntry(op)
	if err != nil {
		slog.Warn("an entry stays in the trash", "disk", e.Disk, "op", op.ID, "error", err)
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, e.Disk, err)
		}
		gone := errors.Is(err, ops.ErrGone)
		if err := catalog.Undo(ctx, s.db, op, gone, s.now); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		if gone {
			return OutcomeChanged, nil
		}
		return OutcomeIOError, nil
	}
	links, err := s.remainingLinks(ctx, handle, e, purged)
	if err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, e.Disk, err)
		}
		return "", err
	}
	if err := catalog.FinishPurge(ctx, s.db, op, links); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	handle.CleanTrash(op.Path2)
	return OutcomePurged, nil
}

// remainingLinks finds what the other names of a purged file must record, and spares the look when it
// had no other.
func (s *Session) remainingLinks(ctx context.Context, handle *ops.Disk, e *catalog.TrashEntry, purged ops.Purged) (*catalog.Links, error) {
	if purged.Nlink <= 1 {
		return nil, nil //nolint:nilnil // no other name is an answer
	}
	links, err := handle.LinksLeft(ctx, s.db, e.FileKey, purged.PriorNS)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return links, nil
}
