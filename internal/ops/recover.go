package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// Op is one operation of the journal.
type Op = catalog.Op

// Recovered says what a reprise did on a disk.
type Recovered struct {
	// Finished says that a move finished, so that what depends on the copies must be worked out again.
	Finished bool
	// Restored holds the restores it finished: a retry of that command must not take the next older
	// entry of a path for what it asks.
	Restored []RestoredPath
	// Trashed holds the operations it finished that sent a copy to the trash: those entries could
	// not be seen until now, so the command that finished them must not purge them too.
	Trashed []int64
}

// RestoredPath is a restore a reprise finished.
type RestoredPath struct {
	Path string
	Op   int64
}

// Recover resolves the operations of this disk left planned, in the order given (specs.md §17.2).
// An error wrapping ErrDiskLost means that the disk cannot go on, and any other that one operation
// stays planned, which the next run looks at again.
func (d *Disk) Recover(ctx context.Context, db *sql.DB, list []Op, nowNS int64, retention time.Duration) (*Recovered, error) {
	out := &Recovered{}
	var stayed error
	for i := range list {
		if list[i].Disk != d.ID {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("reprise: %w", err)
		}
		if err := d.recoverOne(ctx, db, &list[i], nowNS, retention, out); err != nil {
			if errors.Is(err, ErrDiskLost) {
				return out, err
			}
			slog.Warn("an operation stays planned", "disk", d.ID, "op", list[i].ID, "error", err)
			stayed = errors.Join(stayed, err)
		}
	}
	return out, stayed
}

// ResolveNow resolves one operation at once, as a reprise would, after a move that failed, and says
// whether the look found that the move went through, and finished it.
func (d *Disk) ResolveNow(ctx context.Context, db *sql.DB, op *Op, nowNS int64, retention time.Duration) (bool, error) {
	out := &Recovered{}
	err := d.recoverOne(ctx, db, op, nowNS, retention, out)
	return out.Finished, err
}

func (d *Disk) recoverOne(ctx context.Context, db *sql.DB, op *Op, nowNS int64, retention time.Duration, out *Recovered) error {
	if err := d.Verify(); err != nil {
		return err
	}
	switch op.Kind {
	case catalog.OpTrash:
		return d.recoverTrash(ctx, db, op, nowNS, retention, out)
	case catalog.OpRestore:
		return d.recoverRestore(ctx, db, op, nowNS, out)
	case catalog.OpUnlink:
		return d.recoverUnlink(ctx, db, op, nowNS)
	case catalog.OpMkdir:
		return d.recoverMkdir(ctx, db, op, nowNS)
	case catalog.OpRmdir:
		return d.recoverRmdir(ctx, db, op, nowNS)
	case catalog.OpRename:
		return d.recoverRename(ctx, db, op, nowNS, out)
	case catalog.OpCreate:
		return d.recoverCreate(ctx, db, op, nowNS, out)
	case catalog.OpUtimens:
		return d.recoverUtimens(ctx, db, op, nowNS)
	case catalog.OpChmod:
		return d.recoverChmod(ctx, db, op, nowNS)
	case catalog.OpChown:
		return d.recoverChown(ctx, db, op, nowNS)
	case catalog.OpRepair:
		return d.recoverRepair(ctx, db, op, nowNS)
	default:
		return fmt.Errorf("reprise: operation %d: no reprise for %q", op.ID, op.Kind)
	}
}

// seen says what a name holds, compared with the file the operation moved.
func (d *Disk) seen(path []string, op *Op) (Seen, fsx.Stat, error) {
	st, there, err := d.stat(path)
	switch {
	case err != nil:
		return "", st, err
	case !there:
		return SeenAbsent, st, nil
	case st.Kind == fsx.KindFile && strconv.FormatUint(st.Ino, 10) == op.FileKey:
		return SeenSame, st, nil
	default:
		return SeenOther, st, nil
	}
}

func (d *Disk) recoverTrash(ctx context.Context, db *sql.DB, op *Op, nowNS int64, retention time.Duration, out *Recovered) error {
	src, dst := d.rootPlace(op.Path), d.trashPlace(op.Path2)
	// First, as these can be long, and nothing long may come between the looks and what they decide.
	if err := d.syncWay(dst); err != nil {
		return err
	}
	source, sourceStat, err := d.seen(src, op)
	if err != nil {
		return err
	}
	target, _, err := d.seen(dst, op)
	if err != nil {
		return err
	}
	row, err := catalog.CopyOf(ctx, db, op.Disk, op.Path, op.FileKey)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	changed := false
	if source == SeenSame && target == SeenSame {
		if changed, err = editedInPlace(&row, &sourceStat); err != nil {
			return err
		}
	}
	decision, err := ResolveTrash(target, source, changed)
	if err != nil {
		return err
	}
	switch {
	case decision.RemoveTarget:
		if err := d.dropName(dst, op.FileKey); err != nil {
			return err
		}
	case decision.MoveBack:
		if err := d.moveBack(dst, src); err != nil {
			return err
		}
	}
	if !decision.Finish {
		return d.undoMove(ctx, db, op, src, dst, decision.Warn, nowNS)
	}
	if err := d.finishNames(src, dst, decision.RemoveSource, op.FileKey); err != nil {
		return err
	}
	// Looked at again, as the source's name going moved its change time and link count.
	target, moved, err := d.seen(dst, op)
	if err != nil {
		return err
	}
	if target != SeenSame {
		return d.undoMove(ctx, db, op, src, dst, true, nowNS)
	}
	restart := catalog.Restart{DeletedNS: nowNS, ExpiresNS: AddRetention(nowNS, retention)}
	if err := catalog.FinishTrash(ctx, db, op, &restart, linksOf(&moved)); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	out.Finished = true
	out.Trashed = append(out.Trashed, op.ID)
	return nil
}

// editedInPlace says whether a file both names lead to is no longer what its row says. Link moved its
// change time, so only its size and modification time can tell.
func editedInPlace(row *catalog.KnownCopy, st *fsx.Stat) (bool, error) {
	now, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return false, fmt.Errorf("reprise: %w", err)
	}
	return !row.Size.Valid || !row.MtimeNS.Valid || row.Size.Int64 != st.Size || row.MtimeNS.Int64 != now, nil
}

func (d *Disk) recoverRestore(ctx context.Context, db *sql.DB, op *Op, nowNS int64, out *Recovered) error {
	src, dst := d.trashPlace(op.Path2), d.rootPlace(op.Path)
	// First, as for a copy going to the trash.
	if err := d.syncWay(dst); err != nil {
		return err
	}
	source, _, err := d.seen(src, op)
	if err != nil {
		return err
	}
	target, st, err := d.seen(dst, op)
	if err != nil {
		return err
	}
	decision, err := ResolveRestore(target, source)
	if err != nil {
		return err
	}
	if !decision.Finish {
		return d.undoMove(ctx, db, op, src, dst, decision.Warn, nowNS)
	}
	if err := d.finishNames(src, dst, decision.RemoveSource, op.FileKey); err != nil {
		return err
	}
	// Looked at again, as for a copy going to the trash: what is there now is what gets recorded.
	target, st, err = d.seen(dst, op)
	if err != nil {
		return err
	}
	if target != SeenSame {
		return d.undoMove(ctx, db, op, src, dst, true, nowNS)
	}
	found, err := watch.FileFrom(op.Path, &st)
	if err != nil {
		return fmt.Errorf("reprise: %w", err)
	}
	if err := catalog.FinishRestore(ctx, db, op, nowNS, NewCopyOf(&found), linksOf(&st)); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	d.CleanTrash(op.Path2)
	out.Finished = true
	out.Restored = append(out.Restored, RestoredPath{Path: op.Path, Op: op.ID})
	return nil
}

// undoMove says that a move did not happen, once what it left is durable: a name that went back, or
// one that went, and so the file at the source, whose own record holds its names (specs.md §17.2). The
// way to the target was synced first.
func (d *Disk) undoMove(ctx context.Context, db *sql.DB, op *Op, src, dst []string, warn bool, nowNS int64) error {
	if err := d.syncEntryAt(src); err != nil {
		return err
	}
	if err := d.syncFolder(src[:len(src)-1]); err != nil {
		return err
	}
	if err := d.syncFolder(dst[:len(dst)-1]); err != nil {
		return err
	}
	return d.Undo(ctx, db, op, warn, nowNS)
}

// Undo says that an operation did not happen, and clears the folders of the trash left empty: what it
// made, or those of an entry whose file is gone (specs.md §17.1).
func (d *Disk) Undo(ctx context.Context, db *sql.DB, op *Op, warn bool, nowNS int64) error {
	if err := catalog.Undo(ctx, db, op, warn, nowNS); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if op.Path2 != "" && op.Kind != catalog.OpRename && op.Kind != catalog.OpCreate {
		d.CleanTrash(op.Path2)
	}
	return nil
}

// recoverCreate resolves a copy left planned, by the table of specs.md §14.2: the target is the file's
// path now, and the source the part, both compared by inode with the operation's.
func (d *Disk) recoverCreate(ctx context.Context, db *sql.DB, op *Op, nowNS int64, out *Recovered) error {
	job, err := catalog.JobOfPart(ctx, db, op.Disk, op.Path2)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	target := op.Path
	if job != nil {
		target = job.Path
	}
	dst, part := d.rootPlace(target), d.tmpPlace(op.Path2)
	// First, as these can be long, and nothing long may come between the looks and what they decide.
	if err := d.syncWay(dst); err != nil {
		return err
	}
	if err := d.syncFolder(part[:len(part)-1]); err != nil {
		return err
	}
	atTarget, _, err := d.seen(dst, op)
	if err != nil {
		return err
	}
	inTmp, _, err := d.seen(part, op)
	if err != nil {
		return err
	}
	if atTarget != SeenSame {
		if err := d.undoMove(ctx, db, op, part, dst, false, nowNS); err != nil {
			return err
		}
		switch {
		case job != nil:
			if err := catalog.Requeue(ctx, db, job, inTmp == SeenSame); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		case inTmp == SeenSame:
			return d.DropPart(op.Path2)
		case op.FileKey == "" && inTmp != SeenAbsent:
			// A part made before its inode could be recorded: its name, of this operation's id, is this operation's
			// alone, as no id comes back (specs.md §17).
			return d.DropPart(op.Path2)
		}
		return nil
	}
	if err := d.finishNames(part, dst, inTmp == SeenSame, op.FileKey); err != nil {
		return err
	}
	// Looked at again: what is there now is what gets recorded.
	atTarget, st, err := d.seen(dst, op)
	if err != nil {
		return err
	}
	if atTarget != SeenSame {
		return d.undoMove(ctx, db, op, part, dst, true, nowNS)
	}
	found, err := watch.FileFrom(target, &st)
	if err != nil {
		return fmt.Errorf("reprise: %w", err)
	}
	// A copy of a trash entry to another disk is recorded as a restored copy, the user's, never as a replica of what
	// the path holds now (specs.md §17.4).
	restore := false
	if job == nil {
		if restore, err = catalog.IsRestoreTo(ctx, db, op.ID); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	if restore {
		err = catalog.FinishRestoreTo(ctx, db, op, nowNS, NewCopyOf(&found), linksOf(&st))
	} else {
		err = catalog.FinishCreate(ctx, db, op, job, NewCopyOf(&found), nowNS)
	}
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	out.Finished = true
	return nil
}

// recoverRename resolves a rename left planned, as §17.2 does a restore: the target is a logical path,
// so what is found there stays. Finishing keeps old_lpath when the file has moved on meanwhile, so that
// a rename the user made since still reaches this copy (specs.md §17.2).
func (d *Disk) recoverRename(ctx context.Context, db *sql.DB, op *Op, nowNS int64, out *Recovered) error {
	src, dst := d.rootPlace(op.Path), d.rootPlace(op.Path2)
	if err := d.syncWay(dst); err != nil {
		return err
	}
	row, err := catalog.CopyOf(ctx, db, op.Disk, op.Path, op.FileKey)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	source, _, err := d.seenRenamed(src, op, &row)
	if err != nil {
		return err
	}
	target, _, err := d.seenRenamed(dst, op, &row)
	if err != nil {
		return err
	}
	decision, err := ResolveRename(target, source)
	if err != nil {
		return err
	}
	if !decision.Finish {
		return d.undoMove(ctx, db, op, src, dst, decision.Warn, nowNS)
	}
	if err := d.finishNames(src, dst, decision.RemoveSource, op.FileKey); err != nil {
		return err
	}
	target, st, err := d.seenRenamed(dst, op, &row)
	if err != nil {
		return err
	}
	if target != SeenSame {
		return d.undoMove(ctx, db, op, src, dst, true, nowNS)
	}
	if st.Kind == fsx.KindDir {
		if err := catalog.FinishFolderRename(ctx, db, op, 0); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	} else if err := catalog.FinishRename(ctx, db, op, renameDone(&row, &st), linksOf(&st)); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	out.Finished = true
	return nil
}

// seenRenamed says what a name holds for a rename: the inode, and the identity of §15.1 where a row says
// it, so that a file that took the inode again is another file. A folder has only the inode to go by.
func (d *Disk) seenRenamed(path []string, op *Op, row *catalog.KnownCopy) (Seen, fsx.Stat, error) {
	st, there, err := d.stat(path)
	switch {
	case err != nil:
		return "", st, err
	case !there:
		return SeenAbsent, st, nil
	case keyOf(&st) != op.FileKey:
		return SeenOther, st, nil
	case st.Kind == fsx.KindDir:
		return SeenSame, st, nil
	case st.Kind != fsx.KindFile:
		return SeenOther, st, nil
	}
	if row.FileKey.Valid && !isSameAsRow(row, &st) {
		return SeenOther, st, nil
	}
	return SeenSame, st, nil
}

// renameDone says what a row records once a reprise finds a file at its target: what a look found, with
// the change time only when its size and date are what the row says, so that an edit still reads as one.
// The move hid whether an edit came before it, so the copy is no longer known to hold what it did: read
// again, a mismatch is a conflict (specs.md §15.12).
func renameDone(row *catalog.KnownCopy, st *fsx.Stat) *catalog.RenameDone {
	done := &catalog.RenameDone{Nlink: int64(st.Nlink), Unverify: true}
	if changed, err := editedInPlace(row, st); err == nil && !changed {
		done.Ctime = timeOf(st.Ctime)
	}
	return done
}

// moveBack puts a file back where it was, as it is not what the operation looked at.
func (d *Disk) moveBack(from, to []string) error {
	source, err := d.open(from[:len(from)-1])
	if err != nil {
		return classify(err)
	}
	defer source.close()
	target, err := d.open(to[:len(to)-1])
	if err != nil {
		return classify(err)
	}
	defer target.close()
	name := to[len(to)-1]
	if err := source.Rename(from[len(from)-1], target.Dir, name); err != nil {
		if !fsx.IsFlagRefused(err) {
			return classify(err)
		}
		if err := source.Link(from[len(from)-1], target.Dir, name); err != nil {
			return classify(err)
		}
		if err := syncEntry(target.Dir, name); err != nil {
			return err
		}
		if err := target.Sync(); err != nil {
			return lost(err)
		}
		if err := source.Unlink(from[len(from)-1]); err != nil {
			return classify(err)
		}
	}
	if err := syncEntry(target.Dir, name); err != nil {
		return err
	}
	if err := target.Sync(); err != nil {
		return lost(err)
	}
	if err := source.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// dropName removes one of two names of a file, after a look right before it, as the syncs before it
// can be long: a name that no longer holds the file is someone else's, and stays (specs.md §17.1, step
// 4). Its folder keeps its time, as it would have after the move. The caller syncs that folder.
func (d *Disk) dropName(path []string, fileKey string) error {
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil
	case err != nil:
		return classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	switch _, err := holds(parent.Dir, name, byKey(fileKey)); {
	case errors.Is(err, ErrGone):
		return nil
	case err != nil:
		return err
	}
	before, err := parent.Info()
	if err != nil {
		return classify(err)
	}
	switch err := parent.Unlink(name); {
	case err == nil:
	case fsx.IsAbsent(err):
		// Gone since the look, or a name its record no longer holds, which ntfs3 leaves when a device
		// fails between the syncs of a move's two folders, and answers with ENOENT, removing nothing
		// (measured). Only a look can tell, and then nothing can be concluded.
		if _, err := holds(parent.Dir, name, byKey(fileKey)); err == nil {
			return fmt.Errorf("%w: %q", ErrNamesDisagree, name)
		}
	default:
		return classify(err)
	}
	return restoreTime(parent.Dir, before.Mtime)
}

// syncWay syncs what a name leads to, if anything, then every folder from the one that holds it to the
// top of the disk: the operation does not say which of them were made, nor whether the move reached
// the sync of what it moved.
func (d *Disk) syncWay(path []string) error {
	if err := d.syncEntryAt(path); err != nil {
		return err
	}
	for end := len(path) - 1; end >= 0; end-- {
		if err := d.syncFolder(path[:end]); err != nil {
			return err
		}
	}
	return nil
}

// finishNames takes the source's name away, the way to the target being durable, and makes that durable
// before anything is committed on the strength of it: the file at the target, whose own record lost a
// name, then the source's folder.
func (d *Disk) finishNames(src, dst []string, removeSource bool, fileKey string) error {
	if removeSource {
		if err := d.dropName(src, fileKey); err != nil {
			return err
		}
		if err := d.syncEntryAt(dst); err != nil {
			return err
		}
	}
	return d.syncFolder(src[:len(src)-1])
}

// syncEntryAt syncs what a path leads to, when there is something there.
func (d *Disk) syncEntryAt(path []string) error {
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil
	case err != nil:
		return classify(err)
	}
	defer parent.close()
	return syncEntry(parent.Dir, path[len(path)-1])
}

func (d *Disk) syncFolder(path []string) error {
	f, err := d.open(path)
	switch {
	case fsx.IsAbsent(err):
		return nil
	case err != nil:
		return classify(err)
	}
	defer f.close()
	if err := f.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

func (d *Disk) recoverUnlink(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	if layout.IsChunks(op.Path2) {
		// Atoll's own chunks, which nobody else writes: the purge goes on, and ends.
		if err := d.PurgeChunks(op); err != nil {
			return err
		}
		if err := catalog.FinishPurge(ctx, db, op, nil); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		d.CleanTrash(op.Path2)
		return nil
	}
	path := d.trashPlace(op.Path2)
	source, _, err := d.seen(path, op)
	if err != nil {
		return err
	}
	decision, err := ResolveUnlink(source)
	if err != nil {
		return err
	}
	if !decision.Finish {
		return d.Undo(ctx, db, op, decision.Warn, nowNS)
	}
	if err := d.syncFolder(path[:len(path)-1]); err != nil {
		return err
	}
	// It cannot tell what change time the file had before, so every name follows what is there now.
	links, err := d.LinksLeft(ctx, db, op.FileKey, sql.NullInt64{})
	if err != nil {
		return err
	}
	if err := catalog.FinishPurge(ctx, db, op, links); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	d.CleanTrash(op.Path2)
	return nil
}

func (d *Disk) recoverMkdir(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	path := d.rootPlace(op.Path)
	st, there, err := d.stat(path)
	if err != nil {
		return err
	}
	target := SeenAbsent
	if there {
		target = SeenOther
		if st.Kind == fsx.KindDir {
			target = SeenSame
		}
	}
	decision, err := ResolveMkdir(target)
	if err != nil {
		return err
	}
	if !decision.Finish {
		return d.Undo(ctx, db, op, decision.Warn, nowNS)
	}
	if err := d.syncWay(path); err != nil {
		return err
	}
	return d.FinishMkdir(ctx, db, op, &st)
}

// FinishMkdir records a folder made below the root as Atoll's own (specs.md §6.1).
func (d *Disk) FinishMkdir(ctx context.Context, db *sql.DB, op *Op, st *fsx.Stat) error {
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return fmt.Errorf("folder: %w", err)
	}
	// Its identity from the start: a rename of it before the next look is a folder renamed like any other.
	key := strconv.FormatUint(st.Ino, 10)
	row := catalog.EntryRow{Path: op.Path, Mode: int64(st.Mode), MtimeNS: mtime, FileKey: &key}
	if st.HasBirth {
		birth, ok, err := solver.CreationTimeNS(st.Birth.Sec, st.Birth.Nsec)
		if err != nil {
			return fmt.Errorf("folder: %w", err)
		}
		if ok {
			row.BtimeNS = &birth
		}
	}
	if err := catalog.FinishMkdir(ctx, db, op, &row); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// NewCopyOf says what the catalog records of a file a look at the disk found.
func NewCopyOf(f *watch.FoundFile) *catalog.NewCopy {
	return &catalog.NewCopy{
		Path: f.Path, Size: f.Size, MtimeNS: f.MtimeNS, CtimeNS: f.CtimeNS, BtimeNS: f.BtimeNS, FileKey: f.FileKey, Nlink: f.Nlink, Mode: f.Mode,
		UID: f.UID, GID: f.GID,
	}
}

// linksOf says what the other names of a file must record, when it has any: what happens to one of
// its names moves its change time and link count for all (specs.md §17.1).
func linksOf(st *fsx.Stat) *catalog.Links {
	if st.Nlink <= 1 {
		return nil
	}
	return linksFrom(st)
}

// linksFrom says what every name of a file must record, from a look at any of them. After a purge the
// last one has one link, and its row still records two.
func linksFrom(st *fsx.Stat) *catalog.Links {
	ctime, err := solver.TimeNS(st.Ctime.Sec, st.Ctime.Nsec)
	if err != nil {
		return nil
	}
	return &catalog.Links{FileKey: strconv.FormatUint(st.Ino, 10), CtimeNS: ctime, Nlink: int64(st.Nlink)}
}

// LinksLeft says what the names of a file that remain must record, from a look at the first that still
// leads to it, once one of its names is gone for good. A name that cannot be looked at says nothing,
// and the next scan reports it as changed. A disk lost here is the caller's to act on. prior is the
// change time the file had before, when that is known.
func (d *Disk) LinksLeft(ctx context.Context, db *sql.DB, fileKey string, prior sql.NullInt64) (*catalog.Links, error) {
	paths, err := catalog.LinkedPaths(ctx, db, d.ID, fileKey)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for _, path := range paths {
		st, there, err := d.StatRoot(path)
		switch {
		case errors.Is(err, ErrDiskLost):
			return nil, err
		case err != nil, !there, st.Kind != fsx.KindFile, keyOf(&st) != fileKey:
			continue
		}
		links := linksFrom(&st)
		if links != nil {
			links.PriorNS = prior
		}
		return links, nil
	}
	return nil, nil //nolint:nilnil // no other name is an answer
}

// AddRetention gives when an entry made now expires, at the largest time the catalog holds if that
// is past it (specs.md §9, §17.1): a wrapped time would make every entry due at once.
func AddRetention(nowNS int64, retention time.Duration) int64 {
	const largest = int64(^uint64(0) >> 1)
	if retention > 0 && nowNS > largest-retention.Nanoseconds() {
		return largest
	}
	return nowNS + retention.Nanoseconds()
}
