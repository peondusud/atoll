package jobs

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

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

// The outcomes of a copy, besides those of the trash it shares (specs.md §14.1).
const (
	OutcomeReplicated  = "replicated"
	OutcomeQueued      = "queued"
	OutcomeToReplicate = "to-replicate"
	OutcomeConflict    = "conflict"
	OutcomeNoRoom      = "no-room"
)

// chunksPerCommit is how many chunk hashes a copy records at a time: 64 MiB (specs.md §14.2).
const chunksPerCommit = 16

// ReplicateOptions is what atoll replicate is asked (specs.md §14.3).
type ReplicateOptions struct {
	Path   string
	Now    bool
	Force  bool
	DryRun bool
}

// ReplicateItem is one job queued or run.
type ReplicateItem struct {
	Path    string
	Disk    string
	Source  string
	Size    int64
	Outcome string
}

// ReplicateSummary is what a run did.
type ReplicateSummary struct {
	Applied bool
	Bytes   int64
	// Failed counts the jobs that ran and ended otherwise than replicated.
	Failed   int
	DiskLost bool
	// Disabled says that replication.enabled is false, so that nothing was queued or copied.
	Disabled bool
}

// Replicate queues the copies the plan wants of the files at or below a path, and with Now or Force runs
// every job of those files (specs.md §14.3).
func Replicate(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, opts ReplicateOptions, each func(*ReplicateItem) error) (*ReplicateSummary, error) {
	apply := !opts.DryRun
	sum := &ReplicateSummary{Applied: apply}
	if enabled, _ := c.Effective()["replication.enabled"].(bool); !enabled {
		sum.Disabled = true
		return sum, nil
	}
	session, err := trash.Open(ctx, db, c, m, apply)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	r := &replicator{db: db, config: c, machine: m, session: session, now: m.Now(), sum: sum, each: each, force: opts.Force}
	if err := r.positions(ctx); err != nil {
		return nil, err
	}
	below := func(path string) bool {
		return opts.Path == "" || path == opts.Path || strings.HasPrefix(path, opts.Path+"/")
	}
	if opts.Force && apply {
		readable := func(id string) bool {
			handle, err := session.Handle(ctx, id)
			return err == nil && handle != nil && session.Readable(id)
		}
		if err := watch.ReadReplicas(ctx, db, c, m, r.now, readable, below); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	var planned []watch.PlannedJob
	rep := &watch.Replicate{Release: below, DryRun: !apply, Queue: func(jobs []watch.PlannedJob) error {
		jobs = slices.DeleteFunc(jobs, func(j watch.PlannedJob) bool { return !below(j.Path) })
		planned = append(planned, jobs...)
		if !apply {
			return nil
		}
		return queue(ctx, db, r.now, jobs)
	}}
	if err := watch.SettleFor(ctx, db, c, r.now, rep); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	switch {
	case !apply:
		err = r.listPlanned(ctx, opts.Path, planned)
	case !opts.Now && !opts.Force:
		err = r.list(plannedItems(planned, OutcomeQueued))
	default:
		err = r.runJobs(ctx, opts.Path)
	}
	if err != nil {
		return nil, err
	}
	if err := session.Finish(ctx); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	sum.DiskLost = session.Lost()
	return sum, nil
}

// queue queues copies the plan wants, each a job and its pending copy (specs.md §14.1).
func queue(ctx context.Context, db *sql.DB, now int64, jobs []watch.PlannedJob) error {
	list := make([]catalog.Queue, 0, len(jobs))
	for i := range jobs {
		list = append(list, catalog.Queue{FileID: jobs[i].FileID, Src: jobs[i].Src, Dst: jobs[i].Dst, Priority: jobs[i].Priority})
	}
	if err := catalog.QueueReplicate(ctx, db, now, list); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

type replicator struct {
	db       *sql.DB
	config   *config.Config
	machine  disks.Machine
	session  *trash.Session
	now      int64
	sum      *ReplicateSummary
	each     func(*ReplicateItem) error
	position map[string]int
	// force runs the copies at the disk's normal I/O priority (specs.md §14).
	force bool
	// on is the session as the copies under way share it, while runAll runs them.
	on *sharedSession
}

func (r *replicator) positions(ctx context.Context) error {
	rows, err := catalog.Disks(ctx, r.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	r.position = make(map[string]int, len(rows))
	for i := range rows {
		r.position[rows[i].ID] = rows[i].Position
	}
	return nil
}

func plannedItems(jobs []watch.PlannedJob, outcome string) []ReplicateItem {
	out := make([]ReplicateItem, 0, len(jobs))
	for i := range jobs {
		out = append(out, ReplicateItem{Path: jobs[i].Path, Disk: jobs[i].Dst, Source: jobs[i].Src, Size: jobs[i].Size, Outcome: outcome})
	}
	return out
}

// listPlanned lists what a dry run would copy: the jobs already queued below the path, and those the plan
// would queue.
func (r *replicator) listPlanned(ctx context.Context, path string, planned []watch.PlannedJob) error {
	queued, err := catalog.ReplicateJobs(ctx, r.db, path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	items := plannedItems(planned, OutcomeToReplicate)
	for i := range queued {
		items = append(items, ReplicateItem{Path: queued[i].Path, Disk: queued[i].Dst, Source: queued[i].Src, Size: queued[i].Size, Outcome: OutcomeToReplicate})
	}
	return r.list(items)
}

// list gives items by path in byte order, then by the target's position (specs.md §14.3).
func (r *replicator) list(items []ReplicateItem) error {
	slices.SortStableFunc(items, func(a, b ReplicateItem) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(r.position[a.Disk], r.position[b.Disk]))
	})
	for i := range items {
		if err := r.give(&items[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *replicator) give(item *ReplicateItem) error {
	switch item.Outcome {
	case OutcomeReplicated, OutcomeQueued, OutcomeToReplicate:
		r.sum.Bytes += item.Size
	default:
		r.sum.Failed++
	}
	return r.each(item)
}

// runJobs runs every job of the files below the path, several at once as their disks allow (specs.md §14), and lists
// each in the order of their lines.
func (r *replicator) runJobs(ctx context.Context, path string) error {
	list, err := catalog.ReplicateJobs(ctx, r.db, path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	next := func(context.Context) ([]catalog.ReplicateJob, error) {
		page := list
		list = nil
		return page, nil
	}
	never := func(*catalog.ReplicateJob) bool { return false }
	return r.runAll(ctx, next, never, func(job *catalog.ReplicateJob, outcome string) error {
		return r.give(&ReplicateItem{Path: job.Path, Disk: job.Dst, Source: job.Src, Size: job.Size, Outcome: outcome})
	})
}

// errDropped says that a copy was dropped, with the outcome it ends with.
type errDropped struct{ outcome string }

func (e *errDropped) Error() string { return e.outcome }

// runOn runs one job as one operation create (specs.md §14.2), on the handles prepare opened. An error is one of the
// catalog, which ends the command; what a disk answers is an outcome. Copies beside it may run at once: what it asks the
// session goes through r.on, and what makes or removes a name on its target holds that disk's lock of names.
func (r *replicator) runOn(ctx context.Context, job *catalog.ReplicateJob, dst, src *ops.Disk) (string, error) {
	file, err := catalog.FileByID(ctx, r.db, job.FileID)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	row, ok := sourceRow(file, job.Src)
	if !ok {
		// The plan chose a copy that no longer qualifies: the next plan chooses again.
		if err := catalog.EndJob(ctx, r.db, job); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return trash.OutcomeChanged, nil
	}
	// What visibly stands in the way ends the job before anything is copied; what comes after the look is
	// found at the rename.
	switch target, _, err := dst.LookAtTarget(job.Path); {
	case errors.Is(err, ops.ErrDiskLost):
		if err := r.on.Lose(ctx, job.Dst, err); err != nil {
			return "", fmt.Errorf("%w", err)
		}
		return trash.OutcomeIOError, nil
	case err != nil:
		return trash.OutcomeIOError, nil //nolint:nilerr // a look that fails is this job's outcome
	case target != ops.TargetFree:
		if err := catalog.EndJob(ctx, r.db, job); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return obstacleOutcome(target), nil
	}
	resume := job.Part != ""
	if !resume {
		if outcome, err := r.lookAtSource(ctx, job, src, row); err != nil || outcome != "" {
			return outcome, err
		}
	}
	// A copy beside it lost one of its disks since it started: it is journaled nothing, as one that starts now would not
	// run.
	if lostSrc, lostDst := r.halted(job); lostSrc || lostDst {
		return OutcomeUnavailable, nil
	}
	op, err := catalog.BeginCreate(ctx, r.db, job, r.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	r.machine.Checkpoint("journaled")
	c := &copier{r: r, job: job, op: op, dst: dst, src: src, row: row, hash: file.Hash}
	if !r.force {
		restore, err := fsx.LowIOPriority()
		if err != nil {
			slog.Warn("a copy runs at the disk's normal I/O priority", "error", err)
		} else {
			defer restore()
		}
	}
	moved, err := c.copy(ctx, resume)
	if err == nil {
		outcome, err := r.commit(ctx, job, op, moved)
		if err == nil && len(c.refused) > 0 {
			// The copy is made all the same: what its filesystem refused is said, never made stale (specs.md §14).
			slog.Warn("a copy keeps no extended attribute its filesystem refuses", "disk", op.Disk, "path", job.Path, "names", c.refused)
			if err := catalog.WriteXattrKept(ctx, r.db, op, c.refused, r.machine.Now()); err != nil {
				return "", fmt.Errorf("catalog: %w", err)
			}
		}
		return outcome, err
	}
	var dropped *errDropped
	switch {
	case errors.As(err, &dropped):
		return dropped.outcome, nil
	case errors.Is(err, errCatalog):
		return "", err
	default:
		return r.failed(ctx, job, op, dst, err)
	}
}

var errCatalog = errors.New("catalog")

// lookAtSource looks at the source of a copy that starts from nothing before anything is journaled: one gone, or no
// longer what its row says, ends the run as changed and waits settle, and one that cannot be read as io-error, with
// nothing journaled or made, as a source a guard holds may stay so for days and must not cost an operation at each
// try (specs.md §14.2). An empty outcome says that the copy may begin.
func (r *replicator) lookAtSource(ctx context.Context, job *catalog.ReplicateJob, src *ops.Disk, row *catalog.KnownCopy) (string, error) {
	f, _, err := src.OpenSource(row.Path, row)
	switch {
	case err == nil:
		_ = f.Close()
		return "", nil
	case errors.Is(err, ops.ErrGone):
		if err := catalog.RetryJob(ctx, r.db, job, sql.NullInt64{Int64: r.machine.Now() + settleOf(r.config, job).Nanoseconds(), Valid: true}, false); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return trash.OutcomeChanged, nil
	}
	if fsx.IsDiskLost(err) || errors.Is(err, ops.ErrDiskLost) {
		if err := r.on.Lose(ctx, job.Src, err); err != nil {
			return "", fmt.Errorf("%w", err)
		}
	}
	slog.Warn("a source cannot be read", "disk", job.Src, "path", row.Path, "error", err)
	if err := catalog.RetryJob(ctx, r.db, job, sql.NullInt64{}, true); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return trash.OutcomeIOError, nil
}

// handles opens the target and the source, or says why a job cannot run in this run.
func (r *replicator) handles(ctx context.Context, job *catalog.ReplicateJob) (dst, src *ops.Disk, outcome string, err error) {
	if !r.on.Writable(job.Dst) || !r.on.Readable(job.Src) {
		return nil, nil, OutcomeUnavailable, nil
	}
	if dst, err = r.on.Handle(ctx, job.Dst); err != nil || dst == nil {
		return nil, nil, OutcomeUnavailable, err
	}
	if src, err = r.on.Handle(ctx, job.Src); err != nil || src == nil {
		return nil, nil, OutcomeUnavailable, err
	}
	return dst, src, "", nil
}

// sourceRow gives what the catalog says of the copy a job reads, while it is one to read from (§13.4): not one whose
// clock runs, as nothing is at its name until its file is decided.
func sourceRow(f *catalog.FileRow, disk string) (*catalog.KnownCopy, bool) {
	if f == nil {
		return nil, false
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Disk != disk {
			continue
		}
		if (c.State != "ok" && c.State != "surplus") || !c.Size.Valid || !c.FileKey.Valid || c.GoneNS.Valid {
			return nil, false
		}
		place := c.Place(f)
		return &catalog.KnownCopy{Path: place, FilePath: f.Path, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}, true
	}
	return nil, false
}

// commit records a copy at its name (specs.md §14.2).
func (r *replicator) commit(ctx context.Context, job *catalog.ReplicateJob, op *catalog.Op, moved *ops.Moved) (string, error) {
	found, err := watch.FileFrom(job.Path, &moved.Stat)
	if err != nil {
		return "", fmt.Errorf("replicate: %w", err)
	}
	// The time it is now, not when the run began: a run can last hours, and the copy is seen and verified as it ends.
	if err := catalog.FinishCreate(ctx, r.db, op, job, ops.NewCopyOf(&found), r.machine.Now()); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	r.on.MarkChanged()
	return OutcomeReplicated, nil
}

// failed says what an error a disk gave leaves: a disk that cannot be relied on keeps the operation
// planned, for the reprise; anything else is resolved at once, as a reprise would (specs.md §14.2, §17.1).
func (r *replicator) failed(ctx context.Context, job *catalog.ReplicateJob, op *catalog.Op, dst *ops.Disk, cause error) (string, error) {
	if errors.Is(cause, ops.ErrDiskLost) {
		if err := r.on.Lose(ctx, op.Disk, cause); err != nil {
			return "", fmt.Errorf("%w", err)
		}
		return trash.OutcomeIOError, nil
	}
	slog.Warn("a copy did not happen", "disk", op.Disk, "op", op.ID, "error", cause)
	names := r.on.namesOf(op.Disk)
	names.Lock()
	finished, err := dst.ResolveNow(ctx, r.db, op, r.now, 0)
	names.Unlock()
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		if err := r.on.Lose(ctx, op.Disk, err); err != nil {
			return "", fmt.Errorf("%w", err)
		}
		return trash.OutcomeIOError, nil
	case err != nil:
		// The operation stays planned, for the next reprise: an outcome of this job, not of the command.
		slog.Warn("an operation stays planned", "disk", op.Disk, "op", op.ID, "error", err)
		return trash.OutcomeIOError, nil //nolint:nilerr // what stays planned is the job's outcome
	case finished:
		r.on.MarkChanged()
		return OutcomeReplicated, nil
	}
	// The reprise put the job back in the queue: this one counts as an attempt.
	if err := catalog.RetryJob(ctx, r.db, job, sql.NullInt64{}, true); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return trash.OutcomeIOError, nil
}

// copier is one operation create, from its part to the copy at its name.
type copier struct {
	r    *replicator
	job  *catalog.ReplicateJob
	op   *catalog.Op
	dst  *ops.Disk
	src  *ops.Disk
	row  *catalog.KnownCopy
	hash []byte
	// refused holds the extended attributes of the source the target's filesystem refused, in byte order.
	refused []string
}

// copy runs the steps of specs.md §14.2 up to the copy at its name. An *errDropped is a copy dropped, the
// operation undone and the job ended or put back, with its outcome; an error wrapping errCatalog ends the
// command; any other is what a disk gave.
func (c *copier) copy(ctx context.Context, resume bool) (*ops.Moved, error) {
	part, start, err := c.openPart(ctx, resume)
	if err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = part.Close()
		}
	}()
	source, read, err := c.src.OpenSource(c.row.Path, c.row)
	switch {
	case errors.Is(err, ops.ErrGone):
		return nil, c.drop(ctx, trash.OutcomeChanged, true)
	case err != nil:
		return nil, c.sourceFailed(ctx, err)
	}
	defer func() { _ = source.Close() }()
	if err := c.fill(ctx, part, source, start, read.Size, resume); err != nil {
		return nil, err
	}
	c.r.machine.Checkpoint("copied")
	// A source a copy beside this one lost says nothing of the file now: it is read again by the next run. A target lost
	// so is written no more.
	switch lostSrc, lostDst := c.r.halted(c.job); {
	case lostDst:
		return nil, fmt.Errorf("%w: %w", ops.ErrDiskLost, errHalted)
	case lostSrc:
		return nil, c.sourceFailed(ctx, fmt.Errorf("%w: %w", ops.ErrDiskLost, errHalted))
	}
	switch still, err := c.src.SourceStill(c.row.Path, &read); {
	case err != nil:
		return nil, c.sourceFailed(ctx, err)
	case !still:
		return nil, c.drop(ctx, trash.OutcomeChanged, true)
	}
	attrs, err := source.Xattrs(fsx.UserNamespace)
	if err != nil {
		return nil, c.sourceFailed(ctx, err)
	}
	if err := c.restore(part, &read, attrs); err != nil {
		return nil, err
	}
	whole, err := c.verify(ctx, part, read.Size)
	if err != nil {
		return nil, err
	}
	c.r.machine.Checkpoint("verified")
	if c.hash != nil && string(c.hash) != string(whole) {
		return nil, c.drop(ctx, trash.OutcomeChanged, true)
	}
	closed = true
	if err := part.Close(); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return c.place(ctx)
}

// openPart makes the part, or opens the one the job resumes and keeps what of it still matches the hashes
// recorded: the copy goes on from the first chunk missing or wrong (specs.md §14). A part Atoll's own view wrote in
// parallel has no hash recorded, and goes on from its start, each chunk compared with the source's (specs.md §12.3).
func (c *copier) openPart(ctx context.Context, resume bool) (*ops.Part, int, error) {
	if resume {
		part, err := c.openOrMake(c.dst.OpenPart)
		switch {
		case err == nil:
			if err := catalog.SetCreateKey(ctx, c.r.db, c.op, strconv.FormatUint(part.Stat.Ino, 10)); err != nil {
				_ = part.Close()
				return nil, 0, fmt.Errorf("%w: %w", errCatalog, err)
			}
			start, err := c.kept(ctx, part)
			if err != nil {
				_ = part.Close()
				return nil, 0, err
			}
			return part, start, nil
		case !errors.Is(err, ops.ErrGone):
			return nil, 0, err
		}
		// Gone since: it is made again, from nothing.
		if err := catalog.DropChunksFrom(ctx, c.r.db, c.job.ID, 0); err != nil {
			return nil, 0, fmt.Errorf("%w: %w", errCatalog, err)
		}
	}
	part, err := c.openOrMake(c.dst.MakePart)
	switch {
	case err != nil && isNoRoom(err):
		// No block or no inode left for the part, or for its folder: no room, as when a write to it finds none.
		return nil, 0, c.drop(ctx, OutcomeNoRoom, true)
	case err != nil:
		return nil, 0, err
	}
	if err := catalog.SetCreateKey(ctx, c.r.db, c.op, strconv.FormatUint(part.Stat.Ino, 10)); err != nil {
		_ = part.Close()
		return nil, 0, fmt.Errorf("%w: %w", errCatalog, err)
	}
	return part, 0, nil
}

// openOrMake opens or makes the part with the target's lock of names held: tmp/ may be made then.
func (c *copier) openOrMake(open func(*ops.Op) (*ops.Part, error)) (*ops.Part, error) {
	names := c.r.on.namesOf(c.op.Disk)
	names.Lock()
	defer names.Unlock()
	return open(c.op)
}

// kept re-reads a part chunk by chunk against the hashes recorded, and gives the first chunk to copy. What the part
// holds past it stays, for fill to compare with the source rather than write again.
func (c *copier) kept(ctx context.Context, part *ops.Part) (int, error) {
	recorded, err := catalog.JobChunks(ctx, c.r.db, c.job.ID)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errCatalog, err)
	}
	buf := make([]byte, integrity.ChunkSize)
	good := 0
	for ; good*integrity.HashSize < len(recorded); good++ {
		n, err := part.ReadAt(buf, int64(good)*integrity.ChunkSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, classifyDisk(err)
		}
		sum := integrity.ChunkSum(buf[:n])
		if n == 0 || string(sum[:]) != string(recorded[good*integrity.HashSize:(good+1)*integrity.HashSize]) {
			break
		}
	}
	if err := catalog.DropChunksFrom(ctx, c.r.db, c.job.ID, good); err != nil {
		return 0, fmt.Errorf("%w: %w", errCatalog, err)
	}
	return good, nil
}

// fill copies the source into the part from a chunk on, and records each chunk's hash as it goes. A part resumed is
// compared with the source chunk by chunk, and written only where it differs, as one the view wrote in parallel holds
// the source's bytes already; what it holds past the source's size goes. Either way, the re-read without cache checks
// every chunk against the source's hashes afterwards (specs.md §14, step 5).
func (c *copier) fill(ctx context.Context, part *ops.Part, source fsx.File, start int, size int64, compare bool) error {
	buf := make([]byte, integrity.ChunkSize)
	var held []byte
	if compare {
		held = make([]byte, integrity.ChunkSize)
	}
	var hashes []byte
	first := start
	record := func() error {
		if err := catalog.RecordChunks(ctx, c.r.db, c.job.ID, first, hashes); err != nil {
			return fmt.Errorf("%w: %w", errCatalog, err)
		}
		first += len(hashes) / integrity.HashSize
		hashes = hashes[:0]
		return nil
	}
	for idx := start; int64(idx)*integrity.ChunkSize < size; idx++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("replicate: %w", err)
		}
		switch lostSrc, lostDst := c.r.halted(c.job); {
		case lostDst:
			return fmt.Errorf("%w: %w", ops.ErrDiskLost, errHalted)
		case lostSrc:
			return c.sourceFailed(ctx, fmt.Errorf("%w: %w", ops.ErrDiskLost, errHalted))
		}
		off := int64(idx) * integrity.ChunkSize
		want := min(integrity.ChunkSize, size-off)
		n, err := source.ReadAt(buf[:want], off)
		switch {
		case int64(n) < want && (err == nil || errors.Is(err, io.EOF)):
			// Shorter than when it was opened: it changed meanwhile.
			return c.drop(ctx, trash.OutcomeChanged, true)
		case err != nil && !errors.Is(err, io.EOF):
			return c.sourceFailed(ctx, err)
		}
		same := false
		if compare {
			m, err := part.ReadAt(held[:n], off)
			if err != nil && !errors.Is(err, io.EOF) {
				return classifyDisk(err)
			}
			same = m == n && bytes.Equal(held[:m], buf[:n])
		}
		if !same {
			if _, err := part.WriteAt(buf[:n], off); err != nil {
				if isNoRoom(err) {
					return c.drop(ctx, OutcomeNoRoom, true)
				}
				return classifyDisk(err)
			}
		}
		sum := integrity.ChunkSum(buf[:n])
		hashes = append(hashes, sum[:]...)
		if len(hashes) == chunksPerCommit*integrity.HashSize {
			if err := record(); err != nil {
				return err
			}
		}
	}
	if compare {
		if err := part.Truncate(size); err != nil {
			return classifyDisk(err)
		}
	}
	return record()
}

// restore gives the part the source's times and attributes, never its creation time, and syncs it
// (specs.md §14, step 4). What the target's filesystem cannot hold is clamped, and read back at the end. The user.
// extended attributes it is given are the source's, attrs; one the target's filesystem refuses is left out and noted,
// and the copy is made all the same.
func (c *copier) restore(part *ops.Part, read *fsx.Stat, attrs map[string][]byte) error {
	if err := part.SetTimes(fsx.OmitTime, read.Mtime); err != nil {
		return classifyDisk(err)
	}
	// The owner first: a change of owner takes the setuid and setgid bits off a file, so a mode set before it would
	// lose them. And a copy that could not take its source's owner does not keep the bits that would run it as
	// another's.
	mode := read.Mode
	if err := part.SetOwner(read.Uid, read.Gid); err != nil {
		if fsx.IsDiskLost(err) {
			return classifyDisk(err)
		}
		slog.Warn("a copy keeps another owner", "disk", c.op.Disk, "path", c.job.Path, "error", err)
		mode &^= 0o6000
	}
	if err := part.SetMode(mode); err != nil {
		return classifyDisk(err)
	}
	if err := c.giveXattrs(part, attrs); err != nil {
		return err
	}
	if err := part.Sync(); err != nil {
		return fmt.Errorf("%w: %w", ops.ErrDiskLost, err)
	}
	return nil
}

// giveXattrs gives the part the source's user. extended attributes, and takes off any other it holds, as a part
// resumed may hold those of an earlier try (specs.md §14). What its filesystem refuses is left out, and noted.
func (c *copier) giveXattrs(part *ops.Part, attrs map[string][]byte) error {
	have, err := part.Xattrs(fsx.UserNamespace)
	if err != nil {
		return classifyDisk(err)
	}
	plan := solver.PlanXattrs(attrs, have)
	c.refused = nil
	for _, name := range plan.Remove {
		if err := part.RemoveXattr(name); err != nil && !fsx.IsNoXattr(err) && !fsx.IsXattrRefused(err) {
			return classifyDisk(err)
		}
	}
	for _, name := range plan.Set {
		switch err := part.SetXattr(name, attrs[name], 0); {
		case err == nil:
		case fsx.IsDiskLost(err):
			return classifyDisk(err)
		case fsx.IsXattrRefused(err):
			c.refused = append(c.refused, name)
		default:
			return classifyDisk(err)
		}
	}
	return nil
}

// verify re-reads the part without its cache, each chunk against its hash, and gives the whole-file hash,
// which is recorded before anything is renamed (specs.md §14, step 5).
func (c *copier) verify(ctx context.Context, part *ops.Part, size int64) ([]byte, error) {
	if err := part.DropCache(); err != nil {
		return nil, classifyDisk(err)
	}
	recorded, err := catalog.JobChunks(ctx, c.r.db, c.job.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCatalog, err)
	}
	h, err := integrity.NewHasher(integrity.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("replicate: %w", err)
	}
	buf := make([]byte, integrity.ChunkSize)
	var off int64
	for idx := 0; off < size; idx++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("replicate: %w", err)
		}
		if _, lostDst := c.r.halted(c.job); lostDst {
			return nil, fmt.Errorf("%w: %w", ops.ErrDiskLost, errHalted)
		}
		n, err := part.ReadAt(buf[:min(integrity.ChunkSize, size-off)], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, classifyDisk(err)
		}
		sum := integrity.ChunkSum(buf[:n])
		if n == 0 || (idx+1)*integrity.HashSize > len(recorded) || string(sum[:]) != string(recorded[idx*integrity.HashSize:(idx+1)*integrity.HashSize]) {
			slog.Warn("a copy did not read back as written", "disk", c.op.Disk, "path", c.job.Path, "chunk", idx)
			return nil, c.drop(ctx, trash.OutcomeIOError, true)
		}
		_, _ = h.Write(buf[:n])
		off += int64(n)
	}
	if st, err := part.Info(); err != nil || st.Size != size {
		return nil, c.drop(ctx, trash.OutcomeIOError, true)
	}
	whole, _ := h.Sum()
	if err := catalog.RecordVerified(ctx, c.r.db, c.job, whole[:]); err != nil {
		return nil, fmt.Errorf("%w: %w", errCatalog, err)
	}
	return whole[:], nil
}

// place resolves the target and moves the part there: each missing parent made as an operation mkdir of
// its own, with the mode and owner of the same folder on the source disk (specs.md §14.2).
func (c *copier) place(ctx context.Context) (*ops.Moved, error) {
	names := c.r.on.namesOf(c.op.Disk)
	names.Lock()
	defer names.Unlock()
	if _, lostDst := c.r.halted(c.job); lostDst {
		return nil, fmt.Errorf("%w: %w", ops.ErrDiskLost, errHalted)
	}
	target, missing, err := c.dst.LookAtTarget(c.job.Path)
	if err != nil {
		return nil, err
	}
	if target != ops.TargetFree {
		return nil, c.drop(ctx, obstacleOutcome(target), false)
	}
	for _, folder := range missing {
		// The source's folder: an error there is the source's, as one while it was read, never the target's, which
		// would be lost in its place with every job left for it.
		like, err := c.src.FolderInfo(folder)
		if err != nil {
			return nil, c.sourceFailed(ctx, err)
		}
		op, err := catalog.BeginMkdir(ctx, c.r.db, c.op.Disk, folder, ops.LikeOf(like), c.r.now)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errCatalog, err)
		}
		st, err := c.dst.MakeRootFolder(folder, like)
		switch {
		case err == nil:
			if err := c.dst.FinishMkdir(ctx, c.r.db, op, &st); err != nil {
				return nil, fmt.Errorf("%w: %w", errCatalog, err)
			}
			continue
		case errors.Is(err, ops.ErrDiskLost):
			return nil, err
		}
		if undo := catalog.Undo(ctx, c.r.db, op, false, c.r.now); undo != nil {
			return nil, fmt.Errorf("%w: %w", errCatalog, undo)
		}
		switch {
		case errors.Is(err, ops.ErrPathTaken):
			return nil, c.drop(ctx, trash.OutcomePathTaken, false)
		case errors.Is(err, ops.ErrNameRefused):
			return nil, c.drop(ctx, trash.OutcomeNameRefused, false)
		default:
			return nil, err
		}
	}
	moved, err := c.dst.PlacePart(c.op, c.job.Path)
	switch {
	case err == nil:
		return moved, nil
	case errors.Is(err, ops.ErrPathTaken) && fsx.IsExisting(err):
		return nil, c.drop(ctx, OutcomeConflict, false)
	case errors.Is(err, ops.ErrPathTaken):
		return nil, c.drop(ctx, trash.OutcomePathTaken, false)
	case errors.Is(err, ops.ErrNameRefused):
		return nil, c.drop(ctx, trash.OutcomeNameRefused, false)
	default:
		return nil, err
	}
}

// obstacleOutcome says what a copy ends with when its target is not free: something at the path itself is
// a conflict, which the next walk reports, and a parent that is not a folder takes the path.
func obstacleOutcome(target string) string {
	switch target {
	case ops.TargetTaken:
		return OutcomeConflict
	case ops.TargetRefused:
		return trash.OutcomeNameRefused
	}
	return trash.OutcomePathTaken
}

// drop drops a copy: its part goes, its operation is undone, and its job goes back in the queue when
// waiting may help, not before settle when its source changed, or ends with its pending copy when it may
// not (specs.md §14.1, §14.2).
func (c *copier) drop(ctx context.Context, outcome string, retry bool) error {
	if err := c.dst.DropPart(c.op.Path2); err != nil {
		return err
	}
	if err := c.dst.Undo(ctx, c.r.db, c.op, false, c.r.now); err != nil {
		return fmt.Errorf("%w: %w", errCatalog, err)
	}
	var err error
	switch {
	case !retry:
		err = catalog.EndJob(ctx, c.r.db, c.job)
	case outcome == trash.OutcomeChanged:
		err = catalog.RetryJob(ctx, c.r.db, c.job, sql.NullInt64{Int64: c.r.machine.Now() + settleOf(c.r.config, c.job).Nanoseconds(), Valid: true}, false)
	default:
		err = catalog.RetryJob(ctx, c.r.db, c.job, sql.NullInt64{}, false)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errCatalog, err)
	}
	return &errDropped{outcome: outcome}
}

// settleOf is how long a job's file must stay unchanged, by its rule (specs.md §12.3).
func settleOf(c *config.Config, job *catalog.ReplicateJob) time.Duration {
	size := job.Size
	return c.Policy(job.Path, &size).Settle
}

// sourceFailed says what an error reading the source leaves: a source disk that cannot be relied on is
// lost for this run; the copy keeps its part for the job to resume.
func (c *copier) sourceFailed(ctx context.Context, cause error) error {
	if fsx.IsDiskLost(cause) || errors.Is(cause, ops.ErrDiskLost) {
		if err := c.r.on.Lose(ctx, c.job.Src, cause); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	slog.Warn("a source cannot be read", "disk", c.job.Src, "path", c.row.Path, "error", cause)
	if err := c.dst.Undo(ctx, c.r.db, c.op, false, c.r.now); err != nil {
		return fmt.Errorf("%w: %w", errCatalog, err)
	}
	if err := catalog.RetryJob(ctx, c.r.db, c.job, sql.NullInt64{}, true); err != nil {
		return fmt.Errorf("%w: %w", errCatalog, err)
	}
	return &errDropped{outcome: trash.OutcomeIOError}
}

// classifyDisk says what an error on the target means: one that says the disk is gone loses it, so that
// the operation stays planned (invariant 4); any other is resolved at once.
func classifyDisk(err error) error {
	if fsx.IsDiskLost(err) {
		return fmt.Errorf("%w: %w", ops.ErrDiskLost, err)
	}
	return fmt.Errorf("%w", err)
}

func isNoRoom(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
