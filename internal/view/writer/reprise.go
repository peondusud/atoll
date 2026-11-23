package writer

import (
	"context"
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
)

// Reprise resolves what the view left planned, on each disk it has a handle on, before it mounts (specs.md §21.2): what
// makes a name is done when something is there, and undone otherwise; a rename is done when its new name holds what
// moved, and undone otherwise, and a hard link when its new name leads to the file it linked; a copy sent to the trash is done when the trash holds it, and undone when its name
// still does; a rename onto a name whose copy reached the trash is finished. An operation on a disk the view cannot
// write to now stays planned, for its next start.
func (w *Writer) Reprise(ctx context.Context) error {
	return w.reprise(ctx, nil)
}

// Recheck resolves, while the view runs, what it left planned, as its start does, under the writer's lock, so that no
// operation of the view is under way meanwhile: what a disk that could not be relied on left planned, once the view's
// check every 30 s finds that disk there again (specs.md §21.2). A row that work under way still answers for, a part
// being written or closed or a move during its copy, is that work's, and stays as it is.
func (w *Writer) Recheck(ctx context.Context, ready Ready) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reprise(ctx, ready)
}

// readyAnswer is what Ready gave of a disk, asked once a recheck.
type readyAnswer struct {
	before int64
	ok     bool
}

// Ready says, of a disk the view left an operation planned on, up to when what the view journaled there may be
// resolved while it runs: the time of the check that first found it on the mount it is on now, once that is a mount of
// its own after the disk was lost, confirmed past the page cache; false otherwise (specs.md §21.2).
type Ready func(disk string) (before int64, ok bool)

// reprise resolves what the view left planned: at its start, ready nil, every row of a disk it has a handle on; while it
// runs, only the rows ready allows, of a disk back on a mount of its own and confirmed, journaled before that mount
// (the maintainer, 2026-11-23).
func (w *Writer) reprise(ctx context.Context, ready Ready) error {
	open, err := catalog.OpenViewOps(ctx, w.db)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	running := ready != nil
	asked := map[string]readyAnswer{}
	allows := func(disk string, at int64) bool {
		if !running || disk == "" {
			return true
		}
		a, done := asked[disk]
		if !done {
			a.before, a.ok = ready(disk)
			asked[disk] = a
		}
		return a.ok && at < a.before
	}
	for i := range open {
		v := &open[i]
		if v.State != catalog.OpPlanned || running && w.isHeld(v.ID) {
			continue
		}
		if !allows(v.Disk, v.AtNS) || !allows(v.Disk2, v.AtNS) {
			continue
		}
		if v.Kind == catalog.ViewMove {
			if err := w.repriseMove(ctx, v); err != nil {
				w.log.Warn("a file the view moved for room stays planned",
					append([]any{"disk", v.Disk, "from", v.Disk2, "path", v.Path, "error", err}, askedBy(v)...)...)
				continue
			}
			w.log.Info("a file the view moved for room resolved after a stop",
				append([]any{"disk", v.Disk, "from", v.Disk2, "path", v.Path, "state", v.State}, askedBy(v)...)...)
			continue
		}
		if !w.handles.Has(v.Disk) {
			continue
		}
		var state string
		var warn bool
		err := w.handles.Use(v.Disk, func(d *ops.Disk) error {
			var err error
			state, warn, err = w.resolve(ctx, d, v)
			return err
		})
		if err != nil {
			w.log.Warn("an operation of the view stays planned", append([]any{"disk", v.Disk, "path", v.Path, "error", err}, askedBy(v)...)...)
			continue
		}
		if err := catalog.FinishViewOp(ctx, w.db, v, state, warn, w.now()); err != nil {
			return fmt.Errorf("%w", err)
		}
		w.log.Info("an operation of the view resolved after a stop",
			append([]any{"kind", v.Kind, "disk", v.Disk, "path", v.Path, "state", state}, askedBy(v)...)...)
	}
	return nil
}

// resolve says what became of an operation left planned on its disk, and whether what was found is what the view
// leaves.
func (w *Writer) resolve(ctx context.Context, d *ops.Disk, v *catalog.ViewOp) (state string, warn bool, err error) {
	switch v.Kind {
	case catalog.ViewCreate, catalog.ViewMkdir, catalog.ViewSymlink:
		_, found, err := d.StatRoot(v.Path)
		return doneIf(found), false, err //nolint:wrapcheck // ops says what failed
	case catalog.ViewRmdir:
		return catalog.OpDone, false, nil
	case catalog.ViewRename, catalog.ViewLink:
		// A hard link is done when its new name leads to the file it linked, and undone otherwise.
		at, err := holdsAt(d, v.Path2, v.FileKey, v.Folder)
		return doneIf(at), false, err
	case catalog.ViewUnlink:
		if v.OpID == 0 {
			_, found, err := d.StatRoot(v.Path)
			return doneIf(!found), false, err //nolint:wrapcheck // ops says what failed
		}
		// By the table of §17.2, as any move to the trash (specs.md §21.2): both names looked at, the copy's name
		// removed when a link was left beside it, a file that took its name put back.
		return w.resolveTrash(ctx, d, v.OpID)
	case catalog.ViewReplace:
		return w.resolveReplace(ctx, d, v)
	case catalog.ViewPart:
		// What was written in parallel when the view stopped is no copy of a file closed: its part goes, and the
		// ordinary job makes that replica (specs.md §12.3).
		op, found, err := catalog.OpByID(ctx, w.db, v.OpID)
		if err != nil {
			return "", false, fmt.Errorf("%w", err)
		}
		if found && op.Path2 != "" {
			if err := d.DropPart(op.Path2); err != nil {
				return "", false, fmt.Errorf("%w", err)
			}
		}
		return catalog.OpUndone, false, nil
	}
	return catalog.OpDone, false, nil
}

// resolveTrash resolves the move to the trash of a deletion or a replacement of the view by the table of §17.2, the
// copy's row in the catalog its look, or its inode alone for a copy no row records (ops.ResolveViewTrash).
func (w *Writer) resolveTrash(ctx context.Context, d *ops.Disk, opID int64) (string, bool, error) {
	op, found, err := catalog.OpByID(ctx, w.db, opID)
	if err != nil || !found {
		return catalog.OpUndone, false, err //nolint:wrapcheck // the catalog says what failed
	}
	done, warn, err := d.ResolveViewTrash(ctx, w.db, &op, nil)
	if err != nil {
		return "", false, fmt.Errorf("%w", err)
	}
	return doneIf(done), warn, nil
}

// resolveReplace finishes a rename onto a name whose copy reached the trash, by the table of §17.2, and undoes one whose
// copy did not.
func (w *Writer) resolveReplace(ctx context.Context, d *ops.Disk, v *catalog.ViewOp) (string, bool, error) {
	state, warn, err := w.resolveTrash(ctx, d, v.OpID)
	if err != nil || state != catalog.OpDone {
		return state, warn, err
	}
	if moved, err := holdsAt(d, v.Path2, v.FileKey, false); err != nil || moved {
		return catalog.OpDone, false, err
	}
	if left, err := holdsAt(d, v.Path, v.FileKey, false); err != nil || !left {
		// What was to move is gone: what was replaced is in the trash, which is all there is to finish.
		return catalog.OpDone, true, err
	}
	if err := d.RenameUser(v.Path, v.Path2, false, v.FileKey, w.like(ctx)); err != nil {
		return "", false, fmt.Errorf("%w", err)
	}
	return catalog.OpDone, false, nil
}

// errStays says that a disk a move needs cannot be written to now: the move stays planned, for the next start.
var errStays = errors.New("a disk the move needs cannot be written to now")

// repriseMove resolves a move of a file whose write ran out of space, by the operation it names (specs.md §21.2). Its
// create planned: a part at the file's path on the disk it goes to means that the rename went through; its name in
// tmp/ goes, where an interrupted link left one, and the copy on the full disk goes to its trash, when it holds the size
// and the modification time the moved one took from it, but stays there, unpurged, as nothing says that it was not
// written to since; with another, both stay, and the full disk's is presented. No part at the path means that nothing
// moved: the part goes, and the move is undone. Its trash planned: the copy still at its path on the full disk goes
// to the trash all the same, as the program wrote to the other disk from then on, and stays there, unpurged; the table
// of §17.2 then says what became of it. A trash with no room for it leaves the copy at its path, never removed there by
// a reprise, and the move planned, for the next start. Its unlink planned is resolved as a purge's reprise resolves
// one (specs.md §17.2), at the path for one that removes the copy there. The move is then done.
func (w *Writer) repriseMove(ctx context.Context, v *catalog.ViewOp) error {
	op, found, err := catalog.OpByID(ctx, w.db, v.OpID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	state, _, err := catalog.OpStateOf(ctx, w.db, v.OpID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	switch {
	case !found:
		return fmt.Errorf("the move names operation %d, which the journal does not hold", v.OpID)
	case state != catalog.OpPlanned:
		// Resolved before a stop that came before the move's end.
		end := catalog.OpDone
		if op.Kind == catalog.OpCreate && state == catalog.OpUndone {
			end = catalog.OpUndone
		}
		return catalog.EndViewMove(ctx, w.db, v, "", end, false, w.now()) //nolint:wrapcheck // the catalog says what failed
	case op.Kind == catalog.OpCreate:
		return w.repriseMoveCreate(ctx, v, &op)
	}
	if !w.handles.Has(v.Disk2) {
		return errStays
	}
	if op.Kind == catalog.OpUnlink && op.Path2 == "" {
		return w.repriseRemoveHere(ctx, v, &op)
	}
	err = w.handles.Use(v.Disk2, func(d *ops.Disk) error {
		if op.Kind == catalog.OpTrash {
			row, err := w.trashLeft(ctx, d, v, &op)
			if err != nil {
				return err
			}
			if row != nil {
				// Against what the look right before the move found, as no row of the catalog records the copy.
				_, err := d.ResolveTrashNow(ctx, w.db, &op, row, w.now(), w.retention)
				return err //nolint:wrapcheck // ops says what failed
			}
		}
		_, err := d.ResolveNow(ctx, w.db, &op, w.now(), w.retention)
		return err //nolint:wrapcheck // ops says what failed
	})
	if err != nil {
		return err
	}
	return catalog.EndViewMove(ctx, w.db, v, "", catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
}

// repriseRemoveHere resolves the removal at its path of the copy a move left on a full disk, left planned, as a
// purge's reprise resolves one (specs.md §17.2), the path in place of the trash: nothing there, once its folder is
// synced, it finished; the copy still there, nothing happened, and it stays, beside the moved one, as nothing says it is
// still what the move compared it with; another file there, nothing happened either, with an op-undone event. The move
// is then done.
func (w *Writer) repriseRemoveHere(ctx context.Context, v *catalog.ViewOp, op *catalog.Op) error {
	var gone, other bool
	err := w.handles.Use(v.Disk2, func(d *ops.Disk) error {
		var err error
		gone, other, err = d.LeftAt(op)
		return err //nolint:wrapcheck // ops says what failed
	})
	if err != nil {
		return err
	}
	if !gone {
		w.log.Warn("the copy a moved file left stays at its path, beside the moved one", "disk", v.Disk2, "path", v.Path)
	}
	return catalog.EndViewMove(ctx, w.db, v, doneIf(gone), catalog.OpDone, other, w.now()) //nolint:wrapcheck // the catalog says what failed
}

// errNoRoomStays says that the full disk's trash has no room for the copy a move left: in a reprise, where only its size
// and modification time say that it is the moved one's, it is never removed at its path, and the move stays planned,
// for the next start to try again.
var errNoRoomStays = errors.New("the trash of the full disk has no room for the copy left: the move stays planned")

// noRoomStays says whether a trash that failed for want of room moved nothing: then it stays planned, the copy at its
// path (errNoRoomStays), and the folders it made before it ran out of room go.
func (w *Writer) noRoomStays(ctx context.Context, d *ops.Disk, v *catalog.ViewOp, op *catalog.Op, why error) error {
	if !noRoom(why) {
		return nil
	}
	left, err := holdsAt(d, v.Path, op.FileKey, false)
	if err != nil {
		return err
	}
	inTrash, err := w.trashed(ctx, d, op.ID)
	if err != nil || !left || inTrash {
		return err
	}
	d.CleanTrash(op.Path2)
	return fmt.Errorf("%w: %w", errNoRoomStays, why)
}

// trashLeft goes on with a move's trash left planned, once the file is at its name on the disk it went to, where the
// program wrote from then on: the copy it left, still at its path, goes to the trash, whatever it holds, and stays
// there, unpurged, as nothing says it was not written to since. The table of §17.2 then says what became of it, against
// the look right before the move, which it gives when it made one: a file that took the copy's name with its inode
// number in that moment is not the copy (§17.1, step 4).
func (w *Writer) trashLeft(ctx context.Context, d *ops.Disk, v *catalog.ViewOp, op *catalog.Op) (*catalog.KnownCopy, error) {
	trashed, err := w.trashed(ctx, d, op.ID)
	if err != nil || trashed {
		return nil, err
	}
	st, found, err := d.StatRoot(v.Path)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // ops says what failed
	case !found || st.Kind != fsx.KindFile || fmt.Sprintf("%d", st.Ino) != op.FileKey:
		return nil, nil //nolint:nilnil // no look before a move is an answer
	}
	row, err := knownOf(v.Path, &st)
	if err != nil {
		return nil, err
	}
	if _, err := d.ToTrash(op, row); err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return nil, err //nolint:wrapcheck // ops says what failed
		}
		return row, w.noRoomStays(ctx, d, v, op, err)
	}
	return row, nil
}

// repriseMoveCreate resolves a move whose create was left planned.
func (w *Writer) repriseMoveCreate(ctx context.Context, v *catalog.ViewOp, op *catalog.Op) error {
	if !w.handles.Has(v.Disk) {
		return errStays
	}
	var moved fsx.Stat
	placed := false
	err := w.handles.Use(v.Disk, func(d *ops.Disk) error {
		st, found, err := d.StatRoot(v.Path)
		if err != nil {
			return err //nolint:wrapcheck // ops says what failed
		}
		placed = found && st.Kind == fsx.KindFile && op.FileKey != "" && fmt.Sprintf("%d", st.Ino) == op.FileKey
		moved = st
		// Not at its name, the part is no copy of anything: it goes. At its name, a second name of it in tmp/ goes.
		return d.DropPart(op.Path2) //nolint:wrapcheck // ops says what failed
	})
	if err != nil {
		return err
	}
	if !placed {
		return catalog.EndViewMove(ctx, w.db, v, catalog.OpUndone, catalog.OpUndone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
	}
	if !w.handles.Has(v.Disk2) {
		return errStays
	}
	return w.handles.Use(v.Disk2, func(d *ops.Disk) error {
		st, found, err := d.StatRoot(v.Path)
		switch {
		case err != nil:
			return err //nolint:wrapcheck // ops says what failed
		case !found || st.Kind != fsx.KindFile || fmt.Sprintf("%d", st.Ino) != v.FileKey:
			// Nothing of it is left on the full disk: the moved copy is the file.
			return catalog.EndViewMove(ctx, w.db, v, catalog.OpDone, catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
		case st.Size != moved.Size || st.Mtime != moved.Mtime:
			// Written to since the copy: both stay, and the full disk's is presented, for a pass to find them both.
			w.log.Warn("a file moved for room differs from the copy it left: both stay", "disk", v.Disk, "from", v.Disk2, "path", v.Path)
			return catalog.EndViewMove(ctx, w.db, v, catalog.OpDone, catalog.OpUndone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
		}
		row, err := knownOf(v.Path, &st)
		if err != nil {
			return err
		}
		now := w.now()
		trashed, err := catalog.ViewMoveTrash(ctx, w.db, v, &catalog.TrashStart{
			Disk: v.Disk2, Path: v.Path, FileKey: v.FileKey, Size: row.Size, Reason: catalog.ReasonSurplus, NowNS: now, ExpiresNS: expires(now, w.retention),
		})
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if _, err := d.ToTrash(trashed, row); err != nil {
			if errors.Is(err, ops.ErrDiskLost) {
				return err //nolint:wrapcheck // ops says what failed
			}
			if err := w.noRoomStays(ctx, d, v, trashed, err); err != nil {
				return err
			}
			if _, rerr := d.ResolveTrashNow(ctx, w.db, trashed, row, w.now(), w.retention); rerr != nil {
				return fmt.Errorf("%w: %w", err, rerr)
			}
			return catalog.EndViewMove(ctx, w.db, v, "", catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
		}
		// In the trash it stays, unpurged: when in doubt, keep.
		return catalog.EndViewMove(ctx, w.db, v, catalog.OpDone, catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
	})
}

// trashed says whether the trash holds what an operation of ops sent there.
func (w *Writer) trashed(ctx context.Context, d *ops.Disk, opID int64) (bool, error) {
	op, found, err := catalog.OpByID(ctx, w.db, opID)
	if err != nil || !found {
		return false, err //nolint:wrapcheck // the catalog says what failed
	}
	st, found, err := d.StatTrash(op.Path2)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	return found && fmt.Sprintf("%d", st.Ino) == op.FileKey, nil
}

// holdsAt says whether a path holds what an operation moved: the file of that inode, or with no inode recorded, a
// folder.
func holdsAt(d *ops.Disk, path, key string, isFolder bool) (bool, error) {
	st, found, err := d.StatRoot(path)
	switch {
	case err != nil:
		return false, fmt.Errorf("%w", err)
	case !found:
		return false, nil
	case isFolder && key == "":
		return st.Kind == fsx.KindDir, nil
	}
	return fmt.Sprintf("%d", st.Ino) == key, nil
}

func doneIf(ok bool) string {
	if ok {
		return catalog.OpDone
	}
	return catalog.OpUndone
}
