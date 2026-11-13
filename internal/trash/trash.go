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
	"github.com/peondusud/atoll/internal/layout"
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
	// OutcomeNotACopy says that an entry holds the chunks a repair overwrote, which are never restored (specs.md §17).
	OutcomeNotACopy    = "not-a-copy"
	OutcomeChanged     = string(solver.PruneChanged)
	outcomeUnavailable = string(solver.PruneDiskUnavailable)
	// OutcomeDiskUnavailable says that a disk was not confirmed in this run.
	OutcomeDiskUnavailable = outcomeUnavailable
)

const reasonSurplus = "surplus"

// slot is what a session knows of one disk.
type slot struct {
	config *config.Disk // nil for a disk the configuration no longer lists
	state  solver.DiskState
	handle *ops.Disk
	tried  bool
	// precision is how finely the disk holds a time, as its row says (specs.md §9): 0 when no check read it.
	precision int64
}

// Session is one run of a command that writes: it confirms each disk once, before it first reads or
// writes it, and resolves what a crash left (specs.md §10, §17.2). Without apply it writes nothing
// to a disk, and looks at them through what a mount check found.
type Session struct {
	db      *sql.DB
	config  *config.Config
	machine disks.Machine
	apply   bool
	// writes says that the session may write to the disks: false for one that only confirms them to read through
	// them, which then resolves nothing a crash left and cleans no tmp/ (specs.md §17.2).
	writes    bool
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
	// emptied holds, by disk, the folders a copy left in this run through Atoll's own operation (specs.md §17.5).
	emptied map[string]map[string]bool
	// full holds the disks that had no room for a move to their trash in this run (Full).
	full map[string]bool
}

// Open checks every disk and, when it is going to write, resolves what a crash left.
func Open(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool) (*Session, error) {
	return openSession(ctx, db, c, m, apply, apply)
}

// OpenToRead opens a session that confirms each disk it reads through, as one that writes does, and writes to none: it
// resolves nothing of what a crash left, and cleans no tmp/ (specs.md §17.2). A scrub that was not told --yes is one:
// it reads copies through confirmed handles, and records what it finds.
func OpenToRead(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine) (*Session, error) {
	return openSession(ctx, db, c, m, true, false)
}

// openSession is Open, with whether the session may write to the disks: only one that may runs the reprise.
func openSession(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply, writes bool) (*Session, error) {
	s := &Session{
		db: db, config: c, machine: m, apply: apply, writes: writes, now: m.Now(), slots: map[string]*slot{},
		reprised: map[string]int64{}, repriseTrashed: map[int64]bool{}, emptied: map[string]map[string]bool{}, full: map[string]bool{},
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
		s.slots[rows[i].ID] = &slot{state: solver.DiskState(rows[i].State), precision: rows[i].TimePrecisionNS}
	}
	for i := range c.Disks {
		if sl := s.slots[c.Disks[i].ID]; sl != nil {
			sl.config = &c.Disks[i]
		}
	}
	if writes {
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

// TimePrecision says how finely a disk holds a time (specs.md §9), 0 when nothing read it.
func (s *Session) TimePrecision(id string) int64 {
	if sl := s.slots[id]; sl != nil {
		return sl.precision
	}
	return 0
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
			continue
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("%w", err)
		case err != nil:
			s.lost = true
		}
		// What the reprise resolved keeps its parts no more: tmp/ is cleaned after it, as it was when the disk was
		// opened for what earlier runs had resolved (specs.md §14.2).
		if s.writable(id) {
			if err := s.cleanTmp(ctx, id, handle); err != nil {
				return err
			}
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
	handle, err := ops.Open(s.machine, sl.config, sl.precision)
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
		if state := heldState(sl.config, solver.DiskState(verdict.State)); state != sl.state {
			sl.state = state
			if err := catalog.SetDiskState(ctx, s.db, id, string(verdict.State)); err != nil {
				handle.Close()
				return nil, fmt.Errorf("catalog: %w", err)
			}
		}
		if s.writes && s.writable(id) {
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
	// What tmp/ holds first, nearly always nothing: the journal is only read for that.
	names, err := handle.TmpNames()
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return s.lose(ctx, id, err)
	case err != nil:
		slog.Warn("parts stay in tmp/", "disk", id, "error", err)
		return nil
	case len(names) == 0:
		return nil
	}
	named, err := catalog.PartStates(ctx, s.db, id, names)
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

// heldState is the state of a disk whose mount check gave state: a disk the configuration marks draining stays
// draining while it can be read and written (specs.md §9, §18). One mounted read-only is readonly, and its
// evacuation waits.
func heldState(d *config.Disk, state solver.DiskState) solver.DiskState {
	if d.State == config.DiskDraining && (state == solver.DiskOnline || state == solver.DiskStandby) {
		return solver.DiskDraining
	}
	return state
}

// readable and writable take a draining disk in: its copies are the sources that evacuate it, and it stays in step
// with the others until it leaves (specs.md §18). The plan never makes a copy on it.
func readable(state solver.DiskState) bool {
	return state == solver.DiskOnline || state == solver.DiskStandby || state == solver.DiskReadOnly || state == solver.DiskDraining
}

func (s *Session) writable(id string) bool {
	sl := s.slots[id]
	return sl != nil && (sl.state == solver.DiskOnline || sl.state == solver.DiskStandby || sl.state == solver.DiskDraining)
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
	if err := s.RemoveEmptied(ctx); err != nil {
		return err
	}
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
	// Full lists, for a purge, the disks whose trash still holds more than trash.max_size of their capacity, by id
	// in byte order.
	Full []string
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
			if err := s.RemoveEmptied(ctx); err != nil {
				return nil, err
			}
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

// decidePrune says what becomes of each surplus copy of a file, by position, from a look at each of its copies now
// (specs.md §17.3). It puts the file's copies in the order of their disks.
func (s *Session) decidePrune(ctx context.Context, f *catalog.FileRow, position map[string]int) ([]solver.PruneResult, error) {
	slices.SortFunc(f.Copies, func(a, b catalog.CopyRow) int { return cmp.Compare(position[a.Disk], position[b.Disk]) })
	if err := s.refreshCtimes(ctx, f); err != nil {
		return nil, err
	}
	input := solver.PruneInput{State: solver.State(f.State), Replicas: s.wantedReplicas(f), HasHash: f.Hash != nil}
	for i := range f.Copies {
		c := &f.Copies[i]
		unchanged, err := s.unchanged(ctx, f.Path, c)
		if err != nil {
			return nil, err
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
		return nil, fmt.Errorf("%q: %w", f.Path, err)
	}
	return results, nil
}

// Retire sends the surplus copy a file has on one disk to the trash, for a reason of the caller's, when atoll prune
// would send it and only then (specs.md §13.3, §17.3): the copies that stay, ok, unchanged and on a disk that can be
// read, reach the file's level without it, each verified, and the file is neither in conflict nor corrupt. A copy still
// to make is none of those, whatever the plan keeps it for. It says what became of the copy: trashed; to-trash in a
// run that may not write; why it stays; and nothing when the file has no surplus copy there.
func (s *Session) Retire(ctx context.Context, f *catalog.FileRow, disk, reason string) (string, error) {
	position, err := s.positions(ctx)
	if err != nil {
		return "", err
	}
	results, err := s.decidePrune(ctx, f, position)
	if err != nil {
		return "", err
	}
	for _, r := range results {
		if r.Disk != disk {
			continue
		}
		if r.Outcome == solver.PruneToTrash && s.apply {
			return s.Send(ctx, f, copyOn(f, disk), reason)
		}
		return string(r.Outcome), nil
	}
	return "", nil
}

func (s *Session) pruneFile(ctx context.Context, f *catalog.FileRow, position map[string]int, summary *Summary, each func(*PruneItem) error) error {
	if !hasSurplus(f) {
		return nil
	}
	results, err := s.decidePrune(ctx, f, position)
	if err != nil {
		return err
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
	// A disk that may only be read is never written to, whoever asks (specs.md §10): its trash is on it.
	if !s.writable(c.Disk) {
		return outcomeUnavailable, nil
	}
	handle, err := s.open(ctx, c.Disk)
	if err != nil {
		return "", err
	}
	if handle == nil {
		return outcomeUnavailable, nil
	}
	// A stale copy holds a version that came before the file's hash, and a corrupt one holds none.
	hash := f.Hash
	if reason == catalog.ReasonOverwritten || reason == catalog.ReasonQuarantined || reason == catalog.ReasonRepaired {
		hash = nil
	}
	op, err := catalog.BeginTrash(ctx, s.db, &catalog.TrashStart{
		Disk: c.Disk, Path: place, FileKey: c.FileKey.String, Size: c.Size, Hash: hash, Reason: reason,
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
		s.Left(c.Disk, place)
		return OutcomeTrashed, nil
	}
	if fsx.IsNoRoom(moveErr) {
		s.full[c.Disk] = true
	}
	return s.afterFailedMove(ctx, handle, op, moveErr)
}

// Full says that a disk had no room for a move to its trash in this run, as each takes a folder there: what else would
// go to its trash would be journaled and undone for nothing, at every pass until room comes back (specs.md §17.1).
func (s *Session) Full(id string) bool {
	return s.full[id]
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
	// Why says why a purge reaches it: expired, older-than or max-size.
	Why string
}

// Restore puts back, for each path at or below under, its newest entry (specs.md §17.4). An entry from
// before a restore of its path that a reprise finished in this run is left alone: that restore was what
// this asks for.
func (s *Session) Restore(ctx context.Context, under string, each func(*EntryItem) error) (*Summary, error) {
	return s.restore(ctx, under, s.restoreEntry, each)
}

// restore gives one function each newest entry at or below under, as Restore says.
func (s *Session) restore(ctx context.Context, under string, one func(context.Context, *catalog.TrashEntry) (string, error), each func(*EntryItem) error) (*Summary, error) {
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
		outcome, err := one(ctx, e)
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
	// What a repair overwrote is the bytes a corrupt copy had: to look at, never to put back (specs.md §17).
	if layout.IsChunks(e.TrashPath) {
		return OutcomeNotACopy, nil
	}
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
	trashFolder := strings.TrimSuffix(e.TrashPath, e.Path) + folder
	like, err := handle.TrashFolderInfo(trashFolder)
	if err != nil {
		// Nothing can be said of the mode it should have, and a private folder must not come back open.
		// Nothing is journaled, and nothing made, whatever the error says of the disk.
		return err
	}
	// The look first: the operation holds what the folder is to take, for its reprise.
	op, err := catalog.BeginMkdir(ctx, s.db, e.Disk, folder, ops.LikeOf(like), s.now)
	if err != nil {
		return catalogError{fmt.Errorf("catalog: %w", err)}
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

// Purge deletes what has reached its time, then what goes for room on each disk whose trash holds more than
// trash.max_size of its capacity; with olderThan, only what was trashed at least that long ago (specs.md §17,
// §17.4). The summary's Full lists the disks whose trash still holds more once what was purged, or would be, is
// gone.
func (s *Session) Purge(ctx context.Context, olderThan *time.Duration, each func(*EntryItem) error) (*Summary, error) {
	entries, err := catalog.TrashEntries(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	in, err := PurgeInput(ctx, s.db, s.config, s.now, entries)
	if err != nil {
		return nil, err
	}
	if olderThan != nil {
		age := olderThan.Nanoseconds()
		in.OlderThanNS = &age
	}
	for i := range in.Entries {
		in.Entries[i].Held = s.repriseTrashed[in.Entries[i].OpID]
	}
	choice, err := solver.ChoosePurge(in)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	why := make(map[int64]solver.PurgeWhy, len(choice.Purge))
	for _, c := range choice.Purge {
		why[c.OpID] = c.Why
	}
	used := solver.TrashUsed(in.Entries)
	summary := &Summary{Applied: s.apply}
	for i := range entries {
		e := &entries[i]
		if why[e.OpID] == "" {
			continue
		}
		outcome, err := s.purgeEntry(ctx, e)
		if err != nil {
			return nil, err
		}
		switch outcome {
		case OutcomeToPurge:
			summary.ToDo++
		case OutcomePurged:
		default:
			summary.Stayed++
		}
		if outcome == OutcomeToPurge || outcome == OutcomePurged {
			summary.Bytes += e.Size.Int64
			used[e.Disk] -= max(e.Size.Int64, 0)
		}
		if err := each(&EntryItem{TrashEntry: *e, Outcome: outcome, Why: string(why[e.OpID])}); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	summary.Full = solver.FullTrashes(in.MaxSize, in.Disks, used)
	summary.DiskLost = s.lost
	return summary, nil
}

// PurgeInput reads what the choice of what a purge reaches needs, besides the age and the entries a reprise held:
// trash.max_size, each disk's capacity as the catalog last recorded it, and the entries.
func PurgeInput(ctx context.Context, db *sql.DB, c *config.Config, nowNS int64, entries []catalog.TrashEntry) (*solver.PurgeInput, error) {
	weights := make([]catalog.TrashWeight, len(entries))
	for i := range entries {
		e := &entries[i]
		weights[i] = catalog.TrashWeight{Disk: e.Disk, OpID: e.OpID, Reason: e.Reason, Size: e.Size, DeletedNS: e.DeletedNS, ExpiresNS: e.ExpiresNS}
	}
	return purgeInput(ctx, db, c, nowNS, weights)
}

// purgeInput is PurgeInput from what the choice needs of each entry.
func purgeInput(ctx context.Context, db *sql.DB, c *config.Config, nowNS int64, entries []catalog.TrashWeight) (*solver.PurgeInput, error) {
	text, _ := c.Effective()["trash.max_size"].(string)
	maxSize, err := value.ParsePercent(text)
	if err != nil {
		return nil, fmt.Errorf("trash.max_size: %w", err)
	}
	rows, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	in := &solver.PurgeInput{NowNS: nowNS, MaxSize: maxSize, Disks: make([]solver.TrashDisk, 0, len(rows)), Entries: make([]solver.TrashEntry, 0, len(entries))}
	listed := make(map[string]bool, len(rows))
	for i := range rows {
		d := &rows[i]
		in.Disks = append(in.Disks, solver.TrashDisk{ID: d.ID, Capacity: d.Capacity, HasCapacity: d.HasCapacity})
		listed[d.ID] = true
	}
	for i := range entries {
		e := &entries[i]
		// The catalog keeps every disk it knew, retired ones too; one it lost says nothing of its capacity.
		if !listed[e.Disk] {
			in.Disks = append(in.Disks, solver.TrashDisk{ID: e.Disk})
			listed[e.Disk] = true
		}
		in.Entries = append(in.Entries, solver.TrashEntry{
			Disk: e.Disk, OpID: e.OpID, Reason: e.Reason, Size: e.Size.Int64, HasSize: e.Size.Valid, DeletedNS: e.DeletedNS, ExpiresNS: e.ExpiresNS,
		})
	}
	return in, nil
}

// PurgeDue says whether a purge would reach anything now, reading the catalog only: the daemon looks before it checks
// a disk (specs.md §15.13). It reads no path: it is asked after each pass, whatever the trash holds.
func PurgeDue(ctx context.Context, db *sql.DB, c *config.Config, nowNS int64) (bool, error) {
	entries, err := catalog.TrashWeights(ctx, db)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if len(entries) == 0 {
		return false, nil
	}
	in, err := purgeInput(ctx, db, c, nowNS, entries)
	if err != nil {
		return false, err
	}
	choice, err := solver.ChoosePurge(in)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	return len(choice.Purge) > 0, nil
}

// purgeChunks ends a purge of the chunks a repair left in the trash: Atoll's own files, removed with their folder
// (specs.md §17).
func (s *Session) purgeChunks(ctx context.Context, handle *ops.Disk, op *ops.Op) (string, error) {
	if err := handle.PurgeChunks(op); err != nil {
		slog.Warn("an entry stays in the trash", "disk", op.Disk, "op", op.ID, "error", err)
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, op.Disk, err)
		}
		if err := catalog.Undo(ctx, s.db, op, false, s.now); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return OutcomeIOError, nil
	}
	if err := catalog.FinishPurge(ctx, s.db, op, nil); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	handle.CleanTrash(op.Path2)
	return OutcomePurged, nil
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
	if !layout.IsChunks(e.TrashPath) {
		if err := s.dropLinksOf(ctx, handle, e); err != nil {
			return "", err
		}
		if !s.writable(e.Disk) {
			return OutcomeIOError, nil
		}
	}
	op, err := catalog.BeginFromTrash(ctx, s.db, catalog.OpUnlink, e, s.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	if layout.IsChunks(e.TrashPath) {
		return s.purgeChunks(ctx, handle, op)
	}
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
