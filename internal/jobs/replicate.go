package jobs

import (
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

// runJobs runs every job of the files below the path, one at a time, in the order of their lines.
func (r *replicator) runJobs(ctx context.Context, path string) error {
	list, err := catalog.ReplicateJobs(ctx, r.db, path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i := range list {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("replicate: %w", err)
		}
		job := &list[i]
		outcome, err := r.run(ctx, job)
		if err != nil {
			return err
		}
		if err := r.give(&ReplicateItem{Path: job.Path, Disk: job.Dst, Source: job.Src, Size: job.Size, Outcome: outcome}); err != nil {
			return err
		}
	}
	return nil
}

// errDropped says that a copy was dropped, with the outcome it ends with.
type errDropped struct{ outcome string }

func (e *errDropped) Error() string { return e.outcome }

// run runs one job as one operation create (specs.md §14.2). An error is one of the catalog, which ends
// the command; what a disk answers is an outcome.
func (r *replicator) run(ctx context.Context, job *catalog.ReplicateJob) (string, error) {
	// Still running after the reprise, a job's operation is one the reprise could not conclude: what is at
	// its target may be its own copy, and nothing may be decided of it until that operation is (§17.2).
	if job.State == catalog.JobRunning {
		return trash.OutcomeIOError, nil
	}
	dst, src, outcome, err := r.handles(ctx, job)
	if err != nil || outcome != "" {
		return outcome, err
	}
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
		if err := r.session.Lose(ctx, job.Dst, err); err != nil {
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
		return r.commit(ctx, job, op, moved)
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

// handles opens the target and the source, or says why a job cannot run in this run.
func (r *replicator) handles(ctx context.Context, job *catalog.ReplicateJob) (dst, src *ops.Disk, outcome string, err error) {
	if !r.session.Writable(job.Dst) || !r.session.Readable(job.Src) {
		return nil, nil, OutcomeUnavailable, nil
	}
	if dst, err = r.session.Handle(ctx, job.Dst); err != nil || dst == nil {
		return nil, nil, OutcomeUnavailable, err
	}
	if src, err = r.session.Handle(ctx, job.Src); err != nil || src == nil {
		return nil, nil, OutcomeUnavailable, err
	}
	return dst, src, "", nil
}

// sourceRow gives what the catalog says of the copy a job reads, while it is one to read from (§13.4).
func sourceRow(f *catalog.FileRow, disk string) (*catalog.KnownCopy, bool) {
	if f == nil {
		return nil, false
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Disk != disk {
			continue
		}
		if (c.State != "ok" && c.State != "surplus") || !c.Size.Valid || !c.FileKey.Valid {
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
	if err := catalog.FinishCreate(ctx, r.db, op, job, ops.NewCopyOf(&found), r.now); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	r.session.MarkChanged()
	return OutcomeReplicated, nil
}

// failed says what an error a disk gave leaves: a disk that cannot be relied on keeps the operation
// planned, for the reprise; anything else is resolved at once, as a reprise would (specs.md §14.2, §17.1).
func (r *replicator) failed(ctx context.Context, job *catalog.ReplicateJob, op *catalog.Op, dst *ops.Disk, cause error) (string, error) {
	if errors.Is(cause, ops.ErrDiskLost) {
		if err := r.session.Lose(ctx, op.Disk, cause); err != nil {
			return "", fmt.Errorf("%w", err)
		}
		return trash.OutcomeIOError, nil
	}
	slog.Warn("a copy did not happen", "disk", op.Disk, "op", op.ID, "error", cause)
	finished, err := dst.ResolveNow(ctx, r.db, op, r.now, 0)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		if err := r.session.Lose(ctx, op.Disk, err); err != nil {
			return "", fmt.Errorf("%w", err)
		}
		return trash.OutcomeIOError, nil
	case err != nil:
		// The operation stays planned, for the next reprise: an outcome of this job, not of the command.
		slog.Warn("an operation stays planned", "disk", op.Disk, "op", op.ID, "error", err)
		return trash.OutcomeIOError, nil //nolint:nilerr // what stays planned is the job's outcome
	case finished:
		r.session.MarkChanged()
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
	if err := c.fill(ctx, part, source, start, read.Size); err != nil {
		return nil, err
	}
	c.r.machine.Checkpoint("copied")
	switch still, err := c.src.SourceStill(c.row.Path, &read); {
	case err != nil:
		return nil, c.sourceFailed(ctx, err)
	case !still:
		return nil, c.drop(ctx, trash.OutcomeChanged, true)
	}
	if err := c.restore(part, &read); err != nil {
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
// recorded: the copy goes on from the first chunk missing or wrong (specs.md §14).
func (c *copier) openPart(ctx context.Context, resume bool) (*ops.Part, int, error) {
	if resume {
		part, err := c.dst.OpenPart(c.op)
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
	part, err := c.dst.MakePart(c.op)
	if err != nil {
		return nil, 0, err
	}
	if err := catalog.SetCreateKey(ctx, c.r.db, c.op, strconv.FormatUint(part.Stat.Ino, 10)); err != nil {
		_ = part.Close()
		return nil, 0, fmt.Errorf("%w: %w", errCatalog, err)
	}
	return part, 0, nil
}

// kept re-reads a part chunk by chunk against the hashes recorded, and gives the first chunk to copy.
func (c *copier) kept(ctx context.Context, part *ops.Part) (int, error) {
	recorded, err := catalog.JobChunks(ctx, c.r.db, c.job.ID)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errCatalog, err)
	}
	buf := make([]byte, integrity.ChunkSize)
	good := 0
	// The bytes of the chunks that match: only the source's last chunk is shorter than the others, so a
	// short chunk that matches is the last one, and the part is whole.
	var keptBytes int64
	for ; good*integrity.HashSize < len(recorded); good++ {
		n, err := part.ReadAt(buf, int64(good)*integrity.ChunkSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, classifyDisk(err)
		}
		sum := integrity.ChunkSum(buf[:n])
		if n == 0 || string(sum[:]) != string(recorded[good*integrity.HashSize:(good+1)*integrity.HashSize]) {
			break
		}
		keptBytes += int64(n)
	}
	if err := catalog.DropChunksFrom(ctx, c.r.db, c.job.ID, good); err != nil {
		return 0, fmt.Errorf("%w: %w", errCatalog, err)
	}
	if err := part.Truncate(keptBytes); err != nil {
		return 0, classifyDisk(err)
	}
	return good, nil
}

// fill copies the source into the part from a chunk on, and records each chunk's hash as it goes.
func (c *copier) fill(ctx context.Context, part *ops.Part, source fsx.File, start int, size int64) error {
	buf := make([]byte, integrity.ChunkSize)
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
		if _, err := part.WriteAt(buf[:n], off); err != nil {
			if isNoRoom(err) {
				return c.drop(ctx, OutcomeNoRoom, true)
			}
			return classifyDisk(err)
		}
		sum := integrity.ChunkSum(buf[:n])
		hashes = append(hashes, sum[:]...)
		if len(hashes) == chunksPerCommit*integrity.HashSize {
			if err := record(); err != nil {
				return err
			}
		}
	}
	return record()
}

// restore gives the part the source's times and attributes, never its creation time, and syncs it
// (specs.md §14, step 4). What the target's filesystem cannot hold is clamped, and read back at the end.
func (c *copier) restore(part *ops.Part, read *fsx.Stat) error {
	if err := part.SetTimes(fsx.OmitTime, read.Mtime); err != nil {
		return classifyDisk(err)
	}
	if err := part.SetMode(read.Mode); err != nil {
		return classifyDisk(err)
	}
	if err := part.SetOwner(read.Uid, read.Gid); err != nil {
		if fsx.IsDiskLost(err) {
			return classifyDisk(err)
		}
		slog.Warn("a copy keeps another owner", "disk", c.op.Disk, "path", c.job.Path, "error", err)
	}
	if err := part.Sync(); err != nil {
		return fmt.Errorf("%w: %w", ops.ErrDiskLost, err)
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
	target, missing, err := c.dst.LookAtTarget(c.job.Path)
	if err != nil {
		return nil, err
	}
	if target != ops.TargetFree {
		return nil, c.drop(ctx, obstacleOutcome(target), false)
	}
	for _, folder := range missing {
		like, err := c.src.FolderInfo(folder)
		if err != nil {
			return nil, err
		}
		op, err := catalog.BeginMkdir(ctx, c.r.db, c.op.Disk, folder, c.r.now)
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
	if target == ops.TargetTaken {
		return OutcomeConflict
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
		err = catalog.RetryJob(ctx, c.r.db, c.job, sql.NullInt64{Int64: c.r.now + c.settle().Nanoseconds(), Valid: true}, false)
	default:
		err = catalog.RetryJob(ctx, c.r.db, c.job, sql.NullInt64{}, false)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errCatalog, err)
	}
	return &errDropped{outcome: outcome}
}

// settle is how long a file must stay unchanged, by its rule (specs.md §12.3).
func (c *copier) settle() time.Duration {
	size := c.job.Size
	return c.r.config.Policy(c.job.Path, &size).Settle
}

// sourceFailed says what an error reading the source leaves: a source disk that cannot be relied on is
// lost for this run; the copy keeps its part for the job to resume.
func (c *copier) sourceFailed(ctx context.Context, cause error) error {
	if fsx.IsDiskLost(cause) || errors.Is(cause, ops.ErrDiskLost) {
		if err := c.r.session.Lose(ctx, c.job.Src, cause); err != nil {
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
