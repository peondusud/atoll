package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// renamesPerRead is how many waiting renames a round reads at a time.
const renamesPerRead = 1000

// errLook marks a look at a disk that failed, as against a failure of the catalog.
var errLook = errors.New("a look at the disk failed")

// work runs what is pending on each disk, by position.
func (p *pass) work(ctx context.Context) error {
	p.listMetaOps()
	for i := range p.config.Disks {
		d := &p.config.Disks[i]
		if !p.apply {
			if err := p.listWork(ctx, d); err != nil {
				return err
			}
			continue
		}
		if !p.session.Writable(d.ID) {
			if err := p.reportUnavailable(ctx, d.ID); err != nil {
				return err
			}
			continue
		}
		handle, err := p.session.Handle(ctx, d.ID)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if handle == nil || !p.session.Writable(d.ID) {
			if err := p.reportUnavailable(ctx, d.ID); err != nil {
				return err
			}
			continue
		}
		if err := p.workOn(ctx, d, handle); err != nil {
			return err
		}
	}
	return nil
}

func (p *pass) workOn(ctx context.Context, d *config.Disk, handle *ops.Disk) error {
	if err := p.runFolderJobs(ctx, d, handle); err != nil {
		return err
	}
	refused, err := p.runRenames(ctx, d, handle)
	if err != nil {
		return err
	}
	if err := p.wantTrash(ctx, d, refused); err != nil {
		return err
	}
	return p.runTrashJobs(ctx, d)
}

// reportUnavailable lists what waits for a disk that cannot be written to. A job the guard holds is held
// whatever its disk says.
func (p *pass) reportUnavailable(ctx context.Context, id string) error {
	folders, err := catalog.FolderJobs(ctx, p.db, id)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	slices.SortFunc(folders, func(a, b catalog.FolderJob) int { return strings.Compare(a.From, b.From) })
	p.reportFoldersUnavailable(id, folders)
	err = eachPending(ctx, p.db, id, func(rows []catalog.RenameRow) error {
		for i := range rows {
			p.record(Operation{Disk: id, Kind: KindRename, Path: rows[i].Place, To: rows[i].Target, Outcome: OutcomeUnavailable})
		}
		return nil
	})
	if err != nil {
		return err
	}
	jobs, err := catalog.TrashJobs(ctx, p.db, id)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return p.reportJobsUnavailable(ctx, id, jobs)
}

func (p *pass) reportJobsUnavailable(ctx context.Context, id string, jobs []catalog.TrashJob) error {
	for i := range jobs {
		if jobs[i].State == catalog.JobCanceled {
			continue
		}
		f, err := catalog.FileByID(ctx, p.db, jobs[i].FileID)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if f == nil || !slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == id }) {
			continue
		}
		outcome := OutcomeUnavailable
		if g := p.guards[jobs[i].Src]; g != nil && g.tripped {
			outcome = OutcomeHeld
		}
		p.record(Operation{Disk: id, Kind: KindTrash, Path: copyOn(f, id).Place(f), Reason: jobs[i].Reason, Outcome: outcome})
	}
	return nil
}

func (p *pass) reportFoldersUnavailable(id string, folders []catalog.FolderJob) {
	for i := range folders {
		p.record(Operation{Disk: id, Kind: KindFolder, Path: folders[i].From, To: folders[i].To, Outcome: OutcomeUnavailable})
	}
}

func eachPending(ctx context.Context, db *sql.DB, disk string, fn func([]catalog.RenameRow) error) error {
	after := ""
	for {
		rows, err := catalog.PendingRenames(ctx, db, disk, after, renamesPerRead)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		last := rows[len(rows)-1].Place
		if err := fn(rows); err != nil {
			return err
		}
		after = last
	}
}

// runFolderJobs carries out the folders renamed on other disks, each as one operation when it can be
// (specs.md §15.12). A job that cannot run goes, and the copies follow one by one.
func (p *pass) runFolderJobs(ctx context.Context, d *config.Disk, handle *ops.Disk) error {
	list, err := catalog.FolderJobs(ctx, p.db, d.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	slices.SortFunc(list, func(a, b catalog.FolderJob) int { return strings.Compare(a.From, b.From) })
	ordered, cycle := folderOrder(list)
	for i := range cycle {
		if err := catalog.DropJob(ctx, p.db, cycle[i].ID); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	var kept []catalog.FolderJob
	for i := range ordered {
		job := &ordered[i]
		// What it asks of a job that stays for now, it waits for with it.
		if waitsForAny(kept, job) {
			kept = append(kept, *job)
			continue
		}
		if err := p.runFolderJob(ctx, d, handle, job); err != nil {
			return err
		}
		if !p.session.Writable(d.ID) {
			p.reportFoldersUnavailable(d.ID, ordered[i+1:])
			return nil
		}
		queued, err := catalog.FolderJobs(ctx, p.db, d.ID)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if slices.ContainsFunc(queued, func(q catalog.FolderJob) bool { return q.ID == job.ID }) {
			kept = append(kept, *job)
		}
	}
	return nil
}

// folderOrder puts a job after any other it waits for. No order helps jobs in a cycle, as three renames can leave, or
// a job a further rename left landing below its own old name, which no operation can do: the first of those goes, and
// its copies follow one by one. One only leading into a cycle is not in it, and runs once that is broken.
func folderOrder(list []catalog.FolderJob) (ordered, cycle []catalog.FolderJob) {
	rest := slices.Clone(list)
	for len(rest) > 0 {
		at := slices.IndexFunc(rest, func(j catalog.FolderJob) bool { return !waitsForAny(rest, &j) })
		if at < 0 {
			at = slices.IndexFunc(rest, func(j catalog.FolderJob) bool { return reachesItself(rest, &j) })
			cycle = append(cycle, rest[at])
			rest = slices.Delete(rest, at, at+1)
			continue
		}
		ordered = append(ordered, rest[at])
		rest = slices.Delete(rest, at, at+1)
	}
	return ordered, cycle
}

// reachesItself says that following what a job waits for, and what that waits for, leads back to it. With every job of
// a list waiting for another, the list being finite, at least one does.
func reachesItself(jobs []catalog.FolderJob, j *catalog.FolderJob) bool {
	seen := map[catalog.FolderJob]bool{}
	next := []catalog.FolderJob{*j}
	for len(next) > 0 {
		from := next[0]
		next = next[1:]
		for _, k := range jobs {
			if !waitsFor(&from, &k) {
				continue
			}
			if k == *j {
				return true
			}
			if !seen[k] {
				seen[k] = true
				next = append(next, k)
			}
		}
	}
	return false
}

// waitsForAny says that a job goes after another of those given.
func waitsForAny(jobs []catalog.FolderJob, j *catalog.FolderJob) bool {
	return slices.ContainsFunc(jobs, func(k catalog.FolderJob) bool { return waitsFor(j, &k) })
}

// waitsFor says that a job goes after another. Run first, it would land in that one's old folder and be carried by
// its operation, which its look would then read as a user's rename; or it would make that one's new name, which would
// then find it there and go; or it would carry that one's old folder away, and that one would find nothing. That last
// gives way to the other two, as no order spares them both.
func waitsFor(j, k *catalog.FolderJob) bool {
	if isAtOrBelow(j.To, k.From) || strings.HasPrefix(j.To, k.To+"/") {
		return true
	}
	return strings.HasPrefix(k.From, j.From+"/") && !isAtOrBelow(k.To, j.From) && !strings.HasPrefix(k.To, j.To+"/")
}

func isAtOrBelow(path, folder string) bool {
	return path == folder || strings.HasPrefix(path, folder+"/")
}

// folderReady says whether a folder can be renamed as one operation, and what it is.
func (p *pass) folderReady(ctx context.Context, d *config.Disk, handle *ops.Disk, job *catalog.FolderJob) (bool, fsx.Stat, error) {
	from, there, err := handle.StatRoot(job.From)
	if err != nil {
		return false, from, fmt.Errorf("%w: %w", errLook, err)
	}
	if !there || from.Kind != fsx.KindDir {
		return false, from, nil
	}
	// Before any look at that name: a driver can answer one with an error, which says nothing of the disk.
	refused, err := refusal(ctx, p.db, d, job.To, job.From)
	if err != nil {
		return false, from, err
	}
	if refused {
		return false, from, nil
	}
	_, there, err = handle.StatRoot(job.To)
	if err != nil {
		return false, from, fmt.Errorf("%w: %w", errLook, err)
	}
	if there {
		return false, from, nil
	}
	taken, _, err := handle.LookAt(job.To)
	switch {
	case errors.Is(err, ops.ErrNameRefused):
		// A short name of another entry on the way: the copies follow one by one, and each is refused.
		return false, from, nil //nolint:nilerr // a name refused is this job's answer
	case err != nil:
		return false, from, fmt.Errorf("%w: %w", errLook, err)
	case taken:
		return false, from, nil
	}
	fit, err := p.fitsBelow(ctx, d, job)
	if err != nil || !fit {
		return false, from, err
	}
	held, err := p.landsBadly(ctx, d, job)
	if err != nil {
		return false, from, err
	}
	return !held, from, nil
}

// landsBadly says that the operation would carry a copy at its own file's path past what its branch can reach,
// or onto a name another file holds, where its row would then say nothing true: that copy was told to stay.
func (p *pass) landsBadly(ctx context.Context, d *config.Disk, job *catalog.FolderJob) (bool, error) {
	paths, err := catalog.AtOwnPathBelow(ctx, p.db, d.ID, job.From)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	for _, path := range paths {
		target := job.To + strings.TrimPrefix(path, job.From)
		// Its branch only: a name that disk already holds, it can hold.
		fits, err := names.FitsBranch(d.Mount, d.Root, target)
		if err != nil {
			return false, fmt.Errorf("%w", err)
		}
		if !fits {
			return true, nil
		}
		held, err := catalog.PathHeld(ctx, p.db, target)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

func (p *pass) runFolderJob(ctx context.Context, d *config.Disk, handle *ops.Disk, job *catalog.FolderJob) error {
	drop := func() error {
		if err := catalog.DropJob(ctx, p.db, job.ID); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		return nil
	}
	failed := func(err error) error {
		if errors.Is(err, ops.ErrDiskLost) {
			p.record(Operation{Disk: d.ID, Kind: KindFolder, Path: job.From, To: job.To, Outcome: OutcomeUnavailable})
			return p.lose(ctx, d.ID, err)
		}
		slog.Warn("a folder cannot be renamed", "disk", d.ID, "error", err)
		p.record(Operation{Disk: d.ID, Kind: KindFolder, Path: job.From, To: job.To, Outcome: trash.OutcomeIOError})
		return nil
	}
	ready, from, err := p.folderReady(ctx, d, handle, job)
	switch {
	case errors.Is(err, errLook):
		return failed(err)
	case err != nil:
		return err
	case !ready:
		return drop()
	}
	if err := p.makeParents(ctx, d, handle, job.To, 0); err != nil {
		switch {
		case errors.Is(err, ops.ErrNameRefused):
			return drop()
		case errors.Is(err, errLook):
			return failed(err)
		}
		return err
	}
	op, err := catalog.BeginRename(ctx, p.db, d.ID, job.From, job.To, strconv.FormatUint(from.Ino, 10), p.now)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	p.machine.Checkpoint("journaled")
	if err := handle.RenameFolder(op); err != nil {
		outcome, failed := p.session.AfterFailedMove(ctx, handle, op, err)
		if failed != nil {
			return fmt.Errorf("%w", failed)
		}
		p.record(Operation{Disk: d.ID, Kind: KindFolder, Path: job.From, To: job.To, Outcome: outcome})
		// An error for now keeps the job: each copy below it may read as refused on its own, as a case-only
		// rename does on NTFS, and going one by one would send them to the trash.
		if outcome == trash.OutcomeRenamed || outcome == trash.OutcomeIOError || !p.session.Writable(d.ID) {
			return nil
		}
		return drop()
	}
	if err := catalog.FinishFolderRename(ctx, p.db, op, job.ID); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	p.session.Left(d.ID, job.From)
	p.record(Operation{Disk: d.ID, Kind: KindFolder, Path: job.From, To: job.To, Outcome: trash.OutcomeRenamed})
	return nil
}

func (p *pass) lose(ctx context.Context, id string, why error) error {
	if err := p.session.Lose(ctx, id, why); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

func (p *pass) record(op Operation) {
	p.operated(op)
	switch op.Outcome {
	case OutcomeHeld:
		p.out.Held = true
	case trash.OutcomeIOError, OutcomeUnavailable:
		p.out.Partial = true
	default:
	}
}

// fitsBelow says that each copy behind below a folder would fit its branch at the folder's new name: only the new
// name is asked about, as a name that disk already holds, it can hold. In a dry run a copy a decision leaves behind
// still reads as at its own path, which landsBadly looks at.
func (p *pass) fitsBelow(ctx context.Context, d *config.Disk, job *catalog.FolderJob) (bool, error) {
	after := job.From + "/"
	for {
		rows, err := catalog.PendingRenames(ctx, p.db, d.ID, after, renamesPerRead)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if len(rows) == 0 {
			return true, nil
		}
		for i := range rows {
			if !strings.HasPrefix(rows[i].Place, job.From+"/") {
				return true, nil
			}
			fits, err := names.FitsBranch(d.Mount, d.Root, job.To+strings.TrimPrefix(rows[i].Place, job.From))
			if err != nil {
				return false, fmt.Errorf("%w", err)
			}
			if !fits {
				return false, nil
			}
		}
		after = rows[len(rows)-1].Place
	}
}

// makeParents makes each missing parent of a path, as an operation of its own, like the same folder on
// the first disk whose copy of that file is at the path, or for a folder that any disk has (specs.md §15.12).
// What the disk says is marked errLook, and what the catalog says is not.
func (p *pass) makeParents(ctx context.Context, d *config.Disk, handle *ops.Disk, path string, fileID int64) error {
	_, missing, err := handle.LookAt(path)
	if err != nil {
		return fmt.Errorf("%w: %w", errLook, err)
	}
	if len(missing) == 0 {
		return nil
	}
	holders, err := p.holdersOf(ctx, fileID)
	if err != nil {
		return err
	}
	for _, folder := range missing {
		like, err := p.folderLike(ctx, d.ID, folder, holders)
		if err != nil {
			return err
		}
		op, err := catalog.BeginMkdir(ctx, p.db, d.ID, folder, ops.LikeOf(like), p.now)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		st, err := handle.MakeRootFolder(folder, like)
		switch {
		case err == nil:
			if err := handle.FinishMkdir(ctx, p.db, op, &st); err != nil {
				return fmt.Errorf("%w", err)
			}
		case errors.Is(err, ops.ErrDiskLost):
			return fmt.Errorf("%w: %w", errLook, err)
		case errors.Is(err, ops.ErrPathTaken):
			if err := catalog.Undo(ctx, p.db, op, false, p.now); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		default:
			if undo := catalog.Undo(ctx, p.db, op, false, p.now); undo != nil {
				return fmt.Errorf("catalog: %w", undo)
			}
			if errors.Is(err, ops.ErrNameRefused) {
				return fmt.Errorf("%w", err)
			}
			return fmt.Errorf("%w: %w", errLook, err)
		}
	}
	return nil
}

// holdersOf gives the disks with a copy of a file at its path, and nil with no file: any disk will do.
func (p *pass) holdersOf(ctx context.Context, fileID int64) (map[string]bool, error) {
	if fileID == 0 {
		return nil, nil //nolint:nilnil // no file, so no disk to prefer
	}
	f, err := catalog.FileByID(ctx, p.db, fileID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	holders := map[string]bool{}
	if f != nil {
		for i := range f.Copies {
			if f.Copies[i].OldPath == nil {
				holders[f.Copies[i].Disk] = true
			}
		}
	}
	return holders, nil
}

// folderLike finds a folder to make one like: on the first disk by position among those given that holds
// it. A look that fails ends the search with an error that says nothing of the disk being written to: made
// with no model, a private folder would be open there for good. For the same reason a disk not looked at, which
// may hold it, stops a search that found no other.
func (p *pass) folderLike(ctx context.Context, skip, folder string, holders map[string]bool) (*fsx.Stat, error) {
	away := ""
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		if id == skip || (holders != nil && !holders[id]) {
			continue
		}
		if !p.walked[id] {
			if away == "" {
				away = id
			}
			continue
		}
		// Walked and since lost: it may hold that folder, and only a later pass can say so.
		if !p.session.Readable(id) {
			return nil, fmt.Errorf("%w: disk %s cannot be relied on", errLook, id)
		}
		handle, err := p.session.Handle(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		if handle == nil {
			return nil, fmt.Errorf("%w: disk %s cannot be reached", errLook, id)
		}
		st, err := handle.FolderInfo(folder)
		if errors.Is(err, ops.ErrDiskLost) {
			if err := p.lose(ctx, id, err); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: disk %s cannot be relied on", errLook, id)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: disk %s: %v", errLook, id, err) //nolint:errorlint // says nothing of the disk being written to
		}
		if st != nil {
			return st, nil
		}
	}
	if away != "" {
		return nil, fmt.Errorf("%w: disk %s was not looked at", errLook, away)
	}
	// With none of those holding it, any disk will do: the mode it gets must not depend on which file comes first.
	if holders != nil {
		return p.folderLike(ctx, skip, folder, nil)
	}
	return nil, nil //nolint:nilnil // no folder to be like is an answer
}

// runRenames renames what is behind on a disk, by old path, again and again until a round moves nothing,
// as one may free another's target (specs.md §15.12). It gives what is refused.
func (p *pass) runRenames(ctx context.Context, d *config.Disk, handle *ops.Disk) ([]catalog.RenameRow, error) {
	// A job still here failed for now, and holds what it will carry.
	waiting, err := catalog.FolderJobs(ctx, p.db, d.ID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for {
		moved, refused, last, stuck, err := p.renameRound(ctx, d, handle, waiting)
		if err != nil {
			return nil, err
		}
		if !moved && p.session.Writable(d.ID) {
			// Copies that wait round a cycle, each at another's target, as a swap of two names leaves them: only an
			// exchange of two names gets the first of them there (specs.md §15.12).
			if moved, err = p.swapOne(ctx, d, handle, stuck); err != nil {
				return nil, err
			}
		}
		if !moved || !p.session.Writable(d.ID) {
			for i := range last {
				p.record(last[i])
			}
			return refused, nil
		}
	}
}

// renameRound goes over the copies behind once. What it could not do is only said once the last round
// has run, as an earlier one may have been in its way. It gives the copies whose target was taken.
func (p *pass) renameRound(ctx context.Context, d *config.Disk, handle *ops.Disk, waiting []catalog.FolderJob) (bool, []catalog.RenameRow, []Operation, []catalog.RenameRow, error) {
	moved := false
	var refused, stuck []catalog.RenameRow
	var stayed []Operation
	err := eachPending(ctx, p.db, d.ID, func(rows []catalog.RenameRow) error {
		for i := range rows {
			row := &rows[i]
			// Lost by now, and its handle closed.
			if !p.session.Writable(d.ID) {
				stayed = append(stayed, Operation{Disk: d.ID, Kind: KindRename, Path: row.Place, To: row.Target, Outcome: OutcomeUnavailable})
				continue
			}
			// What a job still here will carry waits for it, and what would land in its old name would be carried too.
			if belowAJob(waiting, row.Place) || landsBelowAJob(waiting, row.Target) {
				continue
			}
			outcome, err := p.renameOne(ctx, d, handle, row)
			if err != nil {
				return err
			}
			switch outcome {
			case trash.OutcomeRenamed, OutcomeAlready:
				moved = true
				p.record(Operation{Disk: d.ID, Kind: KindRename, Path: row.Place, To: row.Target, Outcome: outcome})
			case "":
			case trash.OutcomeNameRefused:
				refused = append(refused, *row)
				stayed = append(stayed, Operation{Disk: d.ID, Kind: KindRename, Path: row.Place, To: row.Target, Outcome: outcome})
			case trash.OutcomePathTaken:
				stuck = append(stuck, *row)
				stayed = append(stayed, Operation{Disk: d.ID, Kind: KindRename, Path: row.Place, To: row.Target, Outcome: outcome})
			default:
				stayed = append(stayed, Operation{Disk: d.ID, Kind: KindRename, Path: row.Place, To: row.Target, Outcome: outcome})
			}
		}
		return nil
	})
	return moved, refused, stayed, stuck, err
}

// swapOne finds a copy whose target holds another copy that waits, from which the targets lead back to the first, as
// a swap of two names leaves them, and exchanges the names of those two (specs.md §15.12). A driver that cannot
// exchange two names, as no NTFS driver can, leaves them waiting, a limit.
func (p *pass) swapOne(ctx context.Context, d *config.Disk, handle *ops.Disk, stuck []catalog.RenameRow) (bool, error) {
	if !canExchange(d.FS) {
		return false, nil
	}
	i, j := findSwap(len(stuck), func(k int) string { return stuck[k].Place }, func(k int) string { return stuck[k].Target })
	if i < 0 {
		return false, nil
	}
	return p.exchange(ctx, d, handle, &stuck[i], &stuck[j])
}

// findSwap finds, among copies that wait for a target another of them holds, by place, the first whose targets lead
// round a cycle back to it, and the one at its target: exchanging their names gets the first there. -1 when none
// does. A real pass and a dry run take the same, in the same order.
func findSwap(n int, place, target func(int) string) (int, int) {
	at := make(map[string]int, n)
	for k := range n {
		at[place(k)] = k
	}
	for i := range n {
		j, ok := at[target(i)]
		if !ok {
			continue
		}
		next := j
		for range n {
			if next == i {
				break
			}
			if next, ok = at[target(next)]; !ok {
				break
			}
		}
		if ok && next == i {
			return i, j
		}
	}
	return -1, -1
}

// canExchange says whether a filesystem's driver exchanges two names: ext4, XFS and btrfs do; ntfs3, ntfs-3g and
// lowntfs-3g refuse the flag.
func canExchange(fs names.Filesystem) bool {
	return fs == names.Ext4 || fs == names.XFS || fs == names.Btrfs
}

// exchange swaps the names of r and of s, which sits at the target of r: r reaches it, and s takes the place of r,
// which is its own target, or the next one round the cycle, from where a later round goes on. Each is journaled as a
// rename, so that a reprise finishes or undoes it as any other (specs.md §15.12, §17.2).
func (p *pass) exchange(ctx context.Context, d *config.Disk, handle *ops.Disk, r, s *catalog.RenameRow) (bool, error) {
	a, err := catalog.BeginRename(ctx, p.db, d.ID, r.Place, r.Target, r.FileKey.String, p.now)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	b, err := catalog.BeginRename(ctx, p.db, d.ID, s.Place, r.Place, s.FileKey.String, p.now)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	p.machine.Checkpoint("journaled")
	knownA, knownB := rowOfRename(r), rowOfRename(s)
	movedA, movedB, seenA, seenB, err := handle.ExchangeFiles(a, b, knownA, knownB)
	if err != nil {
		for _, op := range []*catalog.Op{a, b} {
			// A swap the driver refused moved nothing, as no look is needed to tell: both are undone, with no event.
			if errors.Is(err, ops.ErrPathTaken) {
				if err := handle.Undo(ctx, p.db, op, false, p.now); err != nil {
					return false, fmt.Errorf("%w", err)
				}
				continue
			}
			if _, failed := p.session.AfterFailedMove(ctx, handle, op, err); failed != nil {
				return false, fmt.Errorf("%w", failed)
			}
		}
		return false, nil
	}
	for _, one := range []struct {
		op          *catalog.Op
		known       *catalog.KnownCopy
		moved       *ops.Moved
		seen        *fsx.Stat
		path, place string
	}{{a, knownA, movedA, seenA, r.Place, r.Target}, {b, knownB, movedB, seenB, s.Place, r.Place}} {
		done := &catalog.RenameDone{Nlink: int64(one.moved.Stat.Nlink)}
		if ops.IsUntouched(one.known, one.seen) {
			done.Ctime = ops.TimeOf(one.moved.Stat.Ctime)
		}
		if err := catalog.FinishRename(ctx, p.db, one.op, done, one.moved.Links()); err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		p.record(Operation{Disk: d.ID, Kind: KindRename, Path: one.path, To: one.place, Outcome: trash.OutcomeRenamed})
	}
	p.session.MarkChanged()
	return true, nil
}

func belowAJob(jobs []catalog.FolderJob, place string) bool {
	return slices.ContainsFunc(jobs, func(j catalog.FolderJob) bool { return strings.HasPrefix(place, j.From+"/") })
}

func landsBelowAJob(jobs []catalog.FolderJob, target string) bool {
	return slices.ContainsFunc(jobs, func(j catalog.FolderJob) bool { return isAtOrBelow(target, j.From) })
}

func rowOfRename(r *catalog.RenameRow) *catalog.KnownCopy {
	return &catalog.KnownCopy{Path: r.Place, FilePath: r.Target, FileID: r.FileID, Size: r.Size, MtimeNS: r.MtimeNS, CtimeNS: r.CtimeNS, BtimeNS: r.BtimeNS, FileKey: r.FileKey}
}

// moveUndecided says whether another copy of a file was found at another name and is not due yet: its
// target may be about to change. A copy decided, or on a disk not looked at, says nothing of that.
func (p *pass) moveUndecided(ctx context.Context, r *catalog.RenameRow) (bool, error) {
	f, err := catalog.FileByID(ctx, p.db, r.FileID)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return false, nil
	}
	// Its clock still runs: a row that followed in this pass has none, whatever the walk found of it. One a look
	// could not get to may have gone anywhere since.
	return slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
		if !c.GoneNS.Valid {
			return false
		}
		v := p.verdictOf(f, &c).Verdict
		return (v == solver.VerdictMoved && !p.isDue(&c)) || p.isMovedOutOfReach(&c, v)
	}), nil
}

// lookBefore does what comes before a rename of a copy that is behind (specs.md §15.12), and says what
// stops it, or with nothing to say, "" and whether it can go on.
func (p *pass) lookBefore(ctx context.Context, d *config.Disk, handle *ops.Disk, r *catalog.RenameRow) (outcome string, goOn bool, err error) {
	waiting, err := p.moveUndecided(ctx, r)
	if err != nil || waiting {
		return "", false, err
	}
	refused, err := refusal(ctx, p.db, d, r.Target, r.Place)
	if err != nil {
		return "", false, err
	}
	if refused {
		return trash.OutcomeNameRefused, false, nil
	}
	known := rowOfRename(r)
	source, sourceThere, err := handle.StatRoot(r.Place)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", errLook, err)
	}
	target, targetThere, err := handle.StatRoot(r.Target)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", errLook, err)
	}
	sourceSame := sourceThere && ops.IsSameFile(known, &source)
	targetSame := targetThere && ops.IsSameFile(known, &target)
	switch {
	case !sourceThere && targetSame:
		return OutcomeAlready, false, nil
	case !sourceThere:
		return "", false, nil
	case !sourceSame:
		return trash.OutcomeChanged, false, nil
	}
	// Something at the target, or a parent of it that is no folder, or the short name of another entry on the way.
	taken, _, err := handle.LookAt(r.Target)
	switch {
	case errors.Is(err, ops.ErrNameRefused):
		return trash.OutcomeNameRefused, false, nil
	case err != nil:
		return "", false, fmt.Errorf("%w: %w", errLook, err)
	}
	if taken {
		return trash.OutcomePathTaken, false, nil
	}
	return "", true, nil
}

// renameOne does what §15.12 says of one copy that is behind, and says what became of it. Nothing to
// say gives "".
func (p *pass) renameOne(ctx context.Context, d *config.Disk, handle *ops.Disk, r *catalog.RenameRow) (string, error) {
	outcome, goOn, err := p.lookBefore(ctx, d, handle, r)
	switch {
	case errors.Is(err, errLook):
		return p.lookFailed(ctx, d.ID, err)
	case err != nil:
		return "", err
	case outcome == OutcomeAlready:
		target, there, err := handle.StatRoot(r.Target)
		if err != nil {
			return p.lookFailed(ctx, d.ID, err)
		}
		if !there {
			return "", nil
		}
		return p.alreadyThere(ctx, d, r, &target)
	case !goOn:
		return outcome, nil
	}
	if err := p.makeParents(ctx, d, handle, r.Target, r.FileID); err != nil {
		switch {
		case errors.Is(err, ops.ErrNameRefused):
			return trash.OutcomeNameRefused, nil
		case errors.Is(err, errLook):
			return p.lookFailed(ctx, d.ID, err)
		}
		return "", err
	}
	op, err := catalog.BeginRename(ctx, p.db, d.ID, r.Place, r.Target, r.FileKey.String, p.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	p.machine.Checkpoint("journaled")
	known := rowOfRename(r)
	moved, seen, err := handle.RenameFile(op, known)
	if err != nil {
		outcome, failed := p.session.AfterFailedMove(ctx, handle, op, err)
		if failed != nil {
			return "", fmt.Errorf("%w", failed)
		}
		return outcome, nil
	}
	done := &catalog.RenameDone{Nlink: int64(moved.Stat.Nlink)}
	if ops.IsUntouched(known, seen) {
		done.Ctime = ops.TimeOf(moved.Stat.Ctime)
	}
	if err := catalog.FinishRename(ctx, p.db, op, done, moved.Links()); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	p.session.Left(d.ID, r.Place)
	return trash.OutcomeRenamed, nil
}

func (p *pass) lookFailed(ctx context.Context, id string, err error) (string, error) {
	if errors.Is(err, ops.ErrDiskLost) {
		return trash.OutcomeIOError, p.lose(ctx, id, err)
	}
	slog.Warn("a name cannot be looked at or made", "disk", id, "error", err)
	return trash.OutcomeIOError, nil
}

// alreadyThere says that something else made the rename: only the row follows.
func (p *pass) alreadyThere(ctx context.Context, d *config.Disk, r *catalog.RenameRow, st *fsx.Stat) (string, error) {
	done := &catalog.RenameDone{Nlink: int64(st.Nlink), Ctime: ops.TimeOf(st.Ctime)}
	if err := catalog.CatchUp(ctx, p.db, d.ID, r.FileID, done); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	return OutcomeAlready, nil
}

// wantTrash says what becomes of the copies whose rename a disk refuses: edited, the file is in
// conflict. What the guard counted goes to the trash, if it lets it (specs.md §15.3, §15.12). A copy
// refused for the first time in this pass was not counted, and waits for the next.
func (p *pass) wantTrash(ctx context.Context, d *config.Disk, refused []catalog.RenameRow) error {
	g := p.guards[d.ID]
	for i := range refused {
		r := &refused[i]
		s := watch.SeenOf(p.seen[d.ID], r.FileID)
		if s != nil && (s.Verdict == solver.VerdictEdited || s.Verdict == solver.VerdictReplaced) {
			if err := catalog.SetConflict(ctx, p.db, r.FileID, p.now); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			p.session.MarkChanged()
			continue
		}
		// Its wish comes from before the decisions, and the target may have changed since.
		if g == nil || g.tripped || !g.wishes[r.FileID] || onlyAFolderCan(d, r.Place, r.Target) {
			continue
		}
		if err := catalog.QueueRenamedJob(ctx, p.db, r.FileID, d.ID); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	return nil
}

// goodAtTarget says whether a file has a copy at its path, read and verified, as a walk of this pass
// found it, on a disk this session can still rely on.
func (p *pass) goodAtTarget(f *catalog.FileRow, skip string) bool {
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Disk != skip && c.OldPath == nil && c.VerifiedNS.Valid && p.walked[c.Disk] && p.session.Readable(c.Disk) && p.verdictOf(f, c).Verdict == solver.VerdictThere {
			return true
		}
	}
	return false
}

// runTrashJobs sends what is decided to the trash, by id.
func (p *pass) runTrashJobs(ctx context.Context, d *config.Disk) error {
	list, err := catalog.TrashJobs(ctx, p.db, d.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i := range list {
		if !p.session.Writable(d.ID) {
			return p.reportJobsUnavailable(ctx, d.ID, list[i:])
		}
		// A disk with no room for the last one takes no more in this pass: they wait in the queue (specs.md §17.1).
		if p.session.Full(d.ID) {
			return nil
		}
		if list[i].State == catalog.JobCanceled {
			continue
		}
		if err := p.runTrashJob(ctx, d, &list[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p *pass) runTrashJob(ctx context.Context, d *config.Disk, job *catalog.TrashJob) error {
	f, err := catalog.FileByID(ctx, p.db, job.FileID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == d.ID })
	if at < 0 {
		if err := catalog.DropJob(ctx, p.db, job.ID); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		return nil
	}
	c := &f.Copies[at]
	if g := p.guards[job.Src]; g != nil && g.tripped {
		p.record(Operation{Disk: d.ID, Kind: KindTrash, Path: c.Place(f), Reason: job.Reason, Outcome: OutcomeHeld})
		return nil
	}
	stands, err := p.stillStands(ctx, d, job, f, c)
	if err != nil {
		return err
	}
	if !stands {
		p.record(Operation{Disk: d.ID, Kind: KindTrash, Path: c.Place(f), Reason: job.Reason, Outcome: OutcomeCanceled})
		return nil
	}
	outcome, err := p.session.Send(ctx, f, c, job.Reason)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if outcome == trash.OutcomeChanged {
		if err := catalog.DropJob(ctx, p.db, job.ID); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	p.record(Operation{Disk: d.ID, Kind: KindTrash, Path: c.Place(f), Reason: job.Reason, Outcome: outcome})
	return nil
}

// jobStands looks at what a job was decided on, as it may have waited long (specs.md §15.12). It only reads.
func (p *pass) jobStands(ctx context.Context, d *config.Disk, job *catalog.TrashJob, f *catalog.FileRow, c *catalog.CopyRow) (bool, error) {
	// What a decision of this dry run ends, a real pass has ended by now, as decisions come first.
	bad := (f.State == string(solver.StateConflict) && !p.conflictCleared[f.ID]) || f.State == string(solver.StateCorrupt)
	if job.Reason == reasonRenamed {
		// No longer behind, or asked of another target that only a folder's operation can rename it to: the trash
		// is no answer.
		if c.OldPath == nil || onlyAFolderCan(d, c.Place(f), f.Path) {
			return false, nil
		}
		jobs, err := catalog.FolderJobs(ctx, p.db, d.ID)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		refused, err := lastingRefusal(ctx, p.db, d, f.Path, c.Place(f), withOldNames(p.leavingOn(d.ID), jobs))
		if err != nil {
			return false, err
		}
		return refused && !bad && c.VerifiedNS.Valid && p.goodAtTarget(f, d.ID), nil
	}
	// A copy no longer stale, a file whose versions are all kept, or one with nothing left to refresh it from: the
	// stale copy is then what is left of it, and stays (specs.md §15.4).
	if job.Reason == catalog.ReasonOverwritten {
		source := slices.ContainsFunc(f.Copies, func(o catalog.CopyRow) bool {
			return o.State == string(solver.CopyOK) || o.State == string(solver.CopySurplus)
		})
		return c.State == string(solver.CopyStale) && !bad && source, nil
	}
	covered, err := catalog.JobDisks(ctx, p.db, f.ID)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	// A copy the job does not cover, or another found moved in this pass: a user acted on it. What a decision
	// of this dry run takes off the file is no longer there to say so.
	gained := slices.ContainsFunc(f.Copies, func(o catalog.CopyRow) bool {
		if !p.apply && p.planOf(o.Disk).dropped[f.ID] {
			return false
		}
		v := p.verdictOf(f, &o).Verdict
		return !slices.Contains(covered, o.Disk) || (o.Disk != d.ID && (v == solver.VerdictMoved || p.isMovedOutOfReach(&o, v)))
	})
	return p.direct == "propagate" && !bad && !gained, nil
}

// stillStands says whether a job stands, and removes what does not.
func (p *pass) stillStands(ctx context.Context, d *config.Disk, job *catalog.TrashJob, f *catalog.FileRow, c *catalog.CopyRow) (bool, error) {
	stands, err := p.jobStands(ctx, d, job, f, c)
	if err != nil || stands {
		return stands, err
	}
	if job.Reason == reasonRenamed || job.Reason == catalog.ReasonOverwritten {
		err = catalog.DropJob(ctx, p.db, job.ID)
	} else {
		err = catalog.DropFileJobs(ctx, p.db, f.ID)
	}
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	return false, nil
}

// eachRefused gives each copy of a disk that a refused rename could send to the trash, from what the
// catalog says: as its row says it was last read and verified, in a file with no doubt on it, with no
// job for it yet (specs.md §15.3). Whether a copy at the target can be relied on is for the caller.
func eachRefused(ctx context.Context, db *sql.DB, d *config.Disk, leaving []string, fn func(*catalog.RenameRow, *catalog.FileRow)) error {
	jobs, err := catalog.FolderJobs(ctx, db, d.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	leaving = withOldNames(leaving, jobs)
	return eachPending(ctx, db, d.ID, func(rows []catalog.RenameRow) error {
		for i := range rows {
			r := &rows[i]
			// What a job would carry to its target is refused nothing, whatever the table says of a name that disk
			// holds. If the job goes, the copy is first refused then, and waits for the next pass.
			if !r.Verified.Valid || onlyAFolderCan(d, r.Place, r.Target) || slices.ContainsFunc(jobs, func(j catalog.FolderJob) bool { return carries(&j, r.Place, r.Target) }) {
				continue
			}
			refused, err := lastingRefusal(ctx, db, d, r.Target, r.Place, leaving)
			if err != nil {
				return err
			}
			if !refused {
				continue
			}
			state, err := catalog.TrashJobState(ctx, db, r.FileID, d.ID)
			if err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			if state != "" {
				continue
			}
			f, err := catalog.FileByID(ctx, db, r.FileID)
			if err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			// Its copy there may have gone since the page was read, as a guard command takes no lock.
			if f != nil && f.State != string(solver.StateConflict) && f.State != string(solver.StateCorrupt) && slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == d.ID }) {
				fn(r, f)
			}
		}
		return nil
	})
}

// inOneFolder says that a target differs from where a copy sits first at a folder on the way, and there
// only as a disk with collision keys reads as one name, as a case-only rename does: that disk holds that
// folder, so only a folder's operation can do it, and the copy going to the trash would be no answer.
func inOneFolder(place, target string) bool {
	from, to := strings.Split(place, "/"), strings.Split(target, "/")
	for i := 0; i < min(len(from), len(to))-1; i++ {
		if from[i] == to[i] {
			continue
		}
		a, aOK := names.CollisionKey(strings.Join(from[:i+1], "/"))
		b, bOK := names.CollisionKey(strings.Join(to[:i+1], "/"))
		return aOK && bOK && a == b
	}
	return false
}

// withOldNames adds the old names of folder jobs of a disk to the names on their way out.
func withOldNames(leaving []string, jobs []catalog.FolderJob) []string {
	for i := range jobs {
		leaving = append(leaving, jobs[i].From)
	}
	return leaving
}

// carries says that a folder's operation brings what sits at a place to its target.
func carries(j *catalog.FolderJob, place, target string) bool {
	return strings.HasPrefix(place, j.From+"/") && target == j.To+strings.TrimPrefix(place, j.From)
}

// onlyAFolderCan says that a copy on a disk cannot be renamed on its own, whatever the name check says.
func onlyAFolderCan(d *config.Disk, place, target string) bool {
	return d.FS == names.NTFS && inOneFolder(place, target)
}

// RefusedCopies gives the copies of a disk that a refused rename would send to the trash, as the catalog
// says, for what a guard command does with them: no walk tells it of a disk away, or of a copy gone.
func RefusedCopies(ctx context.Context, db *sql.DB, d *config.Disk) ([]catalog.RefusedCopy, error) {
	rows, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	missing := map[string]bool{}
	for i := range rows {
		missing[rows[i].ID] = rows[i].State == "missing" || rows[i].State == "retired"
	}
	if missing[d.ID] {
		return nil, nil
	}
	var out []catalog.RefusedCopy
	err = eachRefused(ctx, db, d, nil, func(r *catalog.RenameRow, f *catalog.FileRow) {
		if copyOn(f, d.ID).GoneKind.Valid {
			return
		}
		if slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
			return c.Disk != d.ID && c.OldPath == nil && c.VerifiedNS.Valid && !c.GoneKind.Valid && !missing[c.Disk]
		}) {
			out = append(out, catalog.RefusedCopy{FileID: r.FileID, Place: r.Place})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// diskPlan is what a dry run would run on a disk.
type diskPlan struct {
	folders []catalog.FolderJob
	renames []catalog.RenameRow
	trashes []Operation
	// overwrites come after the trash jobs decided before them, as their jobs do (specs.md §15.4)
	overwrites []overwrite
	// decided holds the files whose copy here a decision of this dry run spoke for: what the catalog says
	// of that copy no longer holds. dropped holds those whose queued job has nothing left to do, and emptied
	// counts, by folder renamed, the copies a decision took from below it.
	decided map[int64]bool
	dropped map[int64]bool
	emptied map[string]int64
}

func (p *pass) planOf(disk string) *diskPlan {
	plan := p.plans[disk]
	if plan == nil {
		plan = &diskPlan{decided: map[int64]bool{}, dropped: map[int64]bool{}, emptied: map[string]int64{}}
		p.plans[disk] = plan
	}
	return plan
}

// planDecided says what a decision would run, as nothing a dry run decides is recorded.
func (p *pass) planDecided(ctx context.Context, d *solver.PropDecision, f *catalog.FileRow, change *catalog.FileChange) error {
	if change.DropJobs {
		p.jobsDropped[f.ID] = true
		p.requeued[f.ID] = true
	}
	if change.ClearConflict {
		p.conflictCleared[f.ID] = true
	}
	for i := range d.Copies {
		a := &d.Copies[i]
		if a.Action == solver.ActStay {
			continue
		}
		plan := p.planOf(a.Disk)
		plan.decided[f.ID] = true
		c := copyOn(f, a.Disk)
		switch a.Action {
		case solver.ActPending, solver.ActKeepGone:
			plan.renames = append(plan.renames, catalog.RenameRow{
				FileID: f.ID, Place: c.Place(f), Target: d.Target, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, BtimeNS: c.BtimeNS, FileKey: c.FileKey, Verified: c.VerifiedNS,
			})
		case solver.ActTrash:
			if c.OldPath != nil {
				plan.renames = append(plan.renames, catalog.RenameRow{
					FileID: f.ID, Place: *c.OldPath, Target: f.Path, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, BtimeNS: c.BtimeNS, FileKey: c.FileKey, Verified: c.VerifiedNS,
				})
			}
			// A job queued for that deletion stands, with its source, as a real pass keeps it: the queued jobs list it.
			queued, err := catalog.QueuedTrash(ctx, p.db, f.ID, a.Disk, reasonDeleted)
			if err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			if queued {
				continue
			}
			// A job queued for another reason stands beside this one, as in a real pass: the queued jobs list it too.
			// A copy not as its row says is left there, as a queued job finds it.
			outcome := p.trashOutcome(d.Src)
			if outcome == OutcomeToTrash && !isAsItsRow(p.verdictOf(f, c)) {
				outcome = trash.OutcomeChanged
			}
			plan.trashes = append(plan.trashes, Operation{Disk: a.Disk, Kind: KindTrash, Path: c.Place(f), Reason: reasonDeleted, Outcome: outcome})
		case solver.ActDrop, solver.ActRestore:
			plan.dropped[f.ID] = true
			p.leftBelow(plan, a.Disk, c.Place(f))
		case solver.ActFollow, solver.ActOtherTarget:
			p.leftBelow(plan, a.Disk, c.Place(f))
		case solver.ActClear, solver.ActOwnFile, solver.ActStay:
		}
	}
	return nil
}

// isAsItsRow says that a copy is what its row says, as far as this pass can tell.
func isAsItsRow(s *watch.CopySeen) bool {
	return s.Verdict == solver.VerdictThere || s.AsRow
}

// leftBelow counts a copy a decision takes from below a folder renamed on its disk.
func (p *pass) leftBelow(plan *diskPlan, disk, place string) {
	for _, m := range p.moves[disk] {
		if strings.HasPrefix(place, m.From+"/") {
			plan.emptied[m.From]++
		}
	}
}

// planFolderJobs starts a dry run's folder jobs from those that wait.
func (p *pass) planFolderJobs(ctx context.Context) error {
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		queued, err := catalog.FolderJobs(ctx, p.db, id)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		plan := p.planOf(id)
		plan.folders = append(plan.folders, queued...)
	}
	return nil
}

// moveFolderJobs does to a dry run's folder jobs what a folder renamed does to those in the catalog.
func (p *pass) moveFolderJobs(move watch.FolderMove) {
	for _, plan := range p.plans {
		var kept []catalog.FolderJob
		for _, j := range plan.folders {
			next, moved := catalog.FurtherRename(j.To, move.From, move.To)
			switch {
			case !moved:
				kept = append(kept, j)
			case next != j.From:
				j.To = next
				kept = append(kept, j)
			}
		}
		plan.folders = kept
	}
}

// trashOutcome says what a dry run says of a job with that source.
func (p *pass) trashOutcome(src string) string {
	if g := p.guards[src]; g != nil && g.tripped {
		return OutcomeHeld
	}
	return OutcomeToTrash
}

// listWork says what a dry run would do on a disk, in the order a real pass does it: folders, renames,
// then trash jobs, those that wait first.
func (p *pass) listWork(ctx context.Context, d *config.Disk) error {
	plan := p.planOf(d.ID)
	err := eachPending(ctx, p.db, d.ID, func(rows []catalog.RenameRow) error {
		for i := range rows {
			if !plan.decided[rows[i].FileID] {
				plan.renames = append(plan.renames, rows[i])
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var handle *ops.Disk
	if p.session.Writable(d.ID) {
		if handle, err = p.session.Handle(ctx, d.ID); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	waiting, err := p.queuedTrashes(ctx, d, handle != nil)
	if err != nil {
		return err
	}
	plan.trashes = append(waiting, plan.trashes...)
	// What a real pass drops as it decides.
	plan.folders = slices.DeleteFunc(plan.folders, func(j catalog.FolderJob) bool {
		return slices.ContainsFunc(p.cleared[d.ID], func(place string) bool { return strings.HasPrefix(place, j.From+"/") })
	})
	slices.SortFunc(plan.folders, func(a, b catalog.FolderJob) int { return strings.Compare(a.From, b.From) })
	slices.SortFunc(plan.renames, func(a, b catalog.RenameRow) int { return strings.Compare(a.Place, b.Place) })
	if handle == nil {
		p.listUnavailable(d.ID, plan)
		return nil
	}
	return p.listOn(ctx, d, handle, plan)
}

func (p *pass) queuedTrashes(ctx context.Context, d *config.Disk, writable bool) ([]Operation, error) {
	jobs, err := catalog.TrashJobs(ctx, p.db, d.ID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	// What a real pass queues before it runs anything: the overwrite of a stale copy that lost its job.
	lost, err := p.staleWithoutJob(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	for i := range lost {
		jobs = append(jobs, catalog.TrashJob{FileID: lost[i].FileID, Src: lost[i].Src, Dst: d.ID, Reason: catalog.ReasonOverwritten, State: catalog.JobQueued})
	}
	plan := p.planOf(d.ID)
	var list []Operation
	for i := range jobs {
		job := &jobs[i]
		// A decision of this dry run that drops every job of a file drops the overwrite of its stale copy too, and a
		// real pass queues that one again before it runs anything: it is listed, where the copy will be by then.
		again := job.Reason == catalog.ReasonOverwritten && p.requeued[job.FileID]
		if job.State == catalog.JobCanceled || (p.jobsDropped[job.FileID] && !again) || plan.dropped[job.FileID] {
			continue
		}
		f, err := catalog.FileByID(ctx, p.db, job.FileID)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if f == nil || !slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == d.ID }) {
			continue
		}
		c := copyOn(f, d.ID)
		if again {
			if op, listed := p.overwriteAgain(ctx, d, plan, job, f, c); listed {
				list = append(list, op)
			}
			continue
		}
		outcome := p.trashOutcome(job.Src)
		if outcome != OutcomeHeld {
			stands, err := p.jobStands(ctx, d, job, f, c)
			switch {
			case err != nil:
				return nil, err
			case !stands:
				outcome = OutcomeCanceled
				// A real pass drops every job of the file here, and the later disks have nothing to say of it.
				if writable && job.Reason != reasonRenamed && job.Reason != catalog.ReasonOverwritten {
					p.jobsDropped[job.FileID] = true
				}
			case !isAsItsRow(p.verdictOf(f, c)):
				outcome = trash.OutcomeChanged
			}
		}
		list = append(list, Operation{Disk: d.ID, Kind: KindTrash, Path: c.Place(f), Reason: job.Reason, Outcome: outcome})
	}
	return list, nil
}

// overwriteAgain says what a dry run lists for the overwrite of a stale copy whose job a decision of this dry run
// dropped with every other job of its file: the job a real pass queues again right after its decisions, when it would
// stand, at the name the copy has once this disk's renames are done. A copy the decision itself sends to the trash has
// its own line, and gets none here.
func (p *pass) overwriteAgain(ctx context.Context, d *config.Disk, plan *diskPlan, job *catalog.TrashJob, f *catalog.FileRow, c *catalog.CopyRow) (Operation, bool) {
	place := c.Place(f)
	if slices.ContainsFunc(plan.trashes, func(o Operation) bool { return o.Path == place && o.Reason == reasonDeleted }) {
		return Operation{}, false
	}
	stands, err := p.jobStands(ctx, d, job, f, c)
	if err != nil || !stands {
		return Operation{}, false
	}
	if at := slices.IndexFunc(plan.renames, func(r catalog.RenameRow) bool { return r.FileID == f.ID }); at >= 0 {
		place = plan.renames[at].Target
	}
	return Operation{Disk: d.ID, Kind: KindTrash, Path: place, Reason: job.Reason, Outcome: p.trashOutcome(job.Src)}, true
}

func (p *pass) listUnavailable(id string, plan *diskPlan) {
	for i := range plan.folders {
		p.operated(Operation{Disk: id, Kind: KindFolder, Path: plan.folders[i].From, To: plan.folders[i].To, Outcome: OutcomeUnavailable})
	}
	for i := range plan.renames {
		p.operated(Operation{Disk: id, Kind: KindRename, Path: plan.renames[i].Place, To: plan.renames[i].Target, Outcome: OutcomeUnavailable})
	}
	for i := range plan.trashes {
		op := plan.trashes[i]
		if op.Outcome != OutcomeHeld {
			op.Outcome = OutcomeUnavailable
		}
		p.operated(op)
	}
	for i := range plan.overwrites {
		p.operated(plan.overwrites[i].operation(id, true))
	}
}

// plannedRename is a rename a dry run has looked at.
type plannedRename struct {
	row     *catalog.RenameRow
	outcome string
	goOn    bool
}

func (p *pass) listOn(ctx context.Context, d *config.Disk, handle *ops.Disk, plan *diskPlan) error {
	var whole, waiting []catalog.FolderJob
	ordered, _ := folderOrder(plan.folders)
	for i := range ordered {
		job := &ordered[i]
		if waitsForAny(waiting, job) {
			waiting = append(waiting, *job)
			continue
		}
		ready, _, err := p.folderReady(ctx, d, handle, job)
		switch {
		case errors.Is(err, errLook):
			waiting = append(waiting, *job)
			p.operated(Operation{Disk: d.ID, Kind: KindFolder, Path: job.From, To: job.To, Outcome: trash.OutcomeIOError})
		case err != nil:
			return err
		case ready:
			whole = append(whole, *job)
			p.operated(Operation{Disk: d.ID, Kind: KindFolder, Path: job.From, To: job.To, Outcome: OutcomeToRename})
		}
	}
	var rest []plannedRename
	for i := range plan.renames {
		r := &plan.renames[i]
		// What a folder's operation carries to its target needs no operation of its own.
		if belowAJob(waiting, r.Place) || landsBelowAJob(waiting, r.Target) || slices.ContainsFunc(whole, func(j catalog.FolderJob) bool { return carries(&j, r.Place, r.Target) }) {
			continue
		}
		outcome, goOn, err := p.lookBefore(ctx, d, handle, r)
		switch {
		case errors.Is(err, errLook):
			outcome = trash.OutcomeIOError
		case err != nil:
			return err
		}
		if outcome != "" || goOn {
			rest = append(rest, plannedRename{row: r, outcome: outcome, goOn: goOn})
		}
	}
	// The jobs a refused rename queues come after those of the modifications, which are queued before the work.
	decided := len(plan.trashes)
	for _, left := range p.renameRounds(d, rest, whole) {
		p.operated(Operation{Disk: d.ID, Kind: KindRename, Path: left.row.Place, To: left.row.Target, Outcome: left.outcome})
		if g := p.guards[d.ID]; left.outcome == trash.OutcomeNameRefused && g != nil && !g.tripped && g.wishes[left.row.FileID] && !onlyAFolderCan(d, left.row.Place, left.row.Target) {
			plan.trashes = append(plan.trashes, Operation{Disk: d.ID, Kind: KindTrash, Path: left.row.Place, Reason: reasonRenamed, Outcome: OutcomeToTrash})
		}
	}
	for i := range plan.trashes[:decided] {
		p.operated(plan.trashes[i])
	}
	for i := range plan.overwrites {
		p.operated(plan.overwrites[i].operation(d.ID, false))
	}
	for i := range plan.trashes[decided:] {
		p.operated(plan.trashes[decided+i])
	}
	return nil
}

// isFreed says that a path, or a parent of it, was left by what renames first.
func isFreed(freed map[string]bool, path string) bool {
	for {
		if freed[path] {
			return true
		}
		at := strings.LastIndexByte(path, '/')
		if at < 0 {
			return false
		}
		path = path[:at]
	}
}

// renameRounds takes the rounds a real pass takes: what one rename frees lets another go, and once none can, a swap
// of two names undoes the first of a cycle. It says what would rename as it goes, and gives what stays.
func (p *pass) renameRounds(d *config.Disk, rest []plannedRename, whole []catalog.FolderJob) []plannedRename {
	disk := d.ID
	freed := map[string]bool{}
	for i := range whole {
		freed[whole[i].From] = true
	}
	for {
		for moved := true; moved; {
			moved = false
			var stayed []plannedRename
			for _, r := range rest {
				switch {
				case r.outcome == OutcomeAlready:
					p.operated(Operation{Disk: disk, Kind: KindRename, Path: r.row.Place, To: r.row.Target, Outcome: OutcomeAlready})
					moved = true
				case r.goOn, r.outcome == trash.OutcomePathTaken && isFreed(freed, r.row.Target):
					p.operated(Operation{Disk: disk, Kind: KindRename, Path: r.row.Place, To: r.row.Target, Outcome: OutcomeToRename})
					freed[r.row.Place] = true
					moved = true
				default:
					stayed = append(stayed, r)
				}
			}
			rest = stayed
		}
		if !canExchange(d.FS) {
			return rest
		}
		var stuck []int
		for k := range rest {
			if rest[k].outcome == trash.OutcomePathTaken {
				stuck = append(stuck, k)
			}
		}
		i, j := findSwap(len(stuck), func(k int) string { return rest[stuck[k]].row.Place }, func(k int) string { return rest[stuck[k]].row.Target })
		if i < 0 {
			return rest
		}
		r, s := rest[stuck[i]].row, rest[stuck[j]].row
		p.operated(Operation{Disk: disk, Kind: KindRename, Path: r.Place, To: r.Target, Outcome: OutcomeToRename})
		p.operated(Operation{Disk: disk, Kind: KindRename, Path: s.Place, To: r.Place, Outcome: OutcomeToRename})
		// s takes the place of r, from where it waits for its own target, unless that was it.
		after := *s
		after.Place = r.Place
		rest = slices.DeleteFunc(rest, func(q plannedRename) bool { return q.row == r || q.row == s })
		if after.Place != after.Target {
			rest = append(rest, plannedRename{row: &after, outcome: trash.OutcomePathTaken})
			slices.SortFunc(rest, func(a, b plannedRename) int { return strings.Compare(a.row.Place, b.row.Place) })
		}
	}
}
