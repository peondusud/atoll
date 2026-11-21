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
	open, err := catalog.OpenViewOps(ctx, w.db)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	for i := range open {
		v := &open[i]
		if v.State != catalog.OpPlanned {
			continue
		}
		if v.Kind == catalog.ViewMove {
			if err := w.repriseMove(ctx, v); err != nil {
				w.log.Warn("a file the view moved for room stays planned", "disk", v.Disk, "from", v.Disk2, "path", v.Path, "error", err)
				continue
			}
			w.log.Info("a file the view moved for room resolved after a stop", "disk", v.Disk, "from", v.Disk2, "path", v.Path, "state", v.State)
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
			w.log.Warn("an operation of the view stays planned", "disk", v.Disk, "path", v.Path, "error", err)
			continue
		}
		if err := catalog.FinishViewOp(ctx, w.db, v, state, warn, w.now()); err != nil {
			return fmt.Errorf("%w", err)
		}
		w.log.Info("an operation of the view resolved after a stop", "kind", v.Kind, "disk", v.Disk, "path", v.Path, "state", state)
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
		trashed, err := w.trashed(ctx, d, v.OpID)
		if err != nil || trashed {
			return catalog.OpDone, false, err
		}
		left, err := holdsAt(d, v.Path, v.FileKey, false)
		return catalog.OpUndone, !left, err
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

// resolveReplace finishes a rename onto a name whose copy reached the trash, and undoes one whose copy did not.
func (w *Writer) resolveReplace(ctx context.Context, d *ops.Disk, v *catalog.ViewOp) (string, bool, error) {
	trashed, err := w.trashed(ctx, d, v.OpID)
	if err != nil {
		return "", false, err
	}
	if !trashed {
		return catalog.OpUndone, false, nil
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
// of §17.2 then says what became of it. Its unlink planned is resolved as a purge's reprise resolves one (specs.md
// §17.2). The move is then done.
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
	err = w.handles.Use(v.Disk2, func(d *ops.Disk) error {
		if op.Kind == catalog.OpTrash {
			if err := w.trashLeft(ctx, d, v, &op); err != nil {
				return err
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

// trashLeft goes on with a move's trash left planned, once the file is at its name on the disk it went to, where the
// program wrote from then on: the copy it left, still at its path, goes to the trash, whatever it holds, and stays
// there, unpurged, as nothing says it was not written to since. The table of §17.2 then says what became of it.
func (w *Writer) trashLeft(ctx context.Context, d *ops.Disk, v *catalog.ViewOp, op *catalog.Op) error {
	trashed, err := w.trashed(ctx, d, op.ID)
	if err != nil || trashed {
		return err
	}
	st, found, err := d.StatRoot(v.Path)
	switch {
	case err != nil:
		return err //nolint:wrapcheck // ops says what failed
	case !found || st.Kind != fsx.KindFile || fmt.Sprintf("%d", st.Ino) != op.FileKey:
		return nil
	}
	row, err := knownOf(v.Path, &st)
	if err != nil {
		return err
	}
	if _, err := d.ToTrash(op, row); err != nil && errors.Is(err, ops.ErrDiskLost) {
		return err //nolint:wrapcheck // ops says what failed
	}
	return nil
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
			if _, rerr := d.ResolveNow(ctx, w.db, trashed, w.now(), w.retention); rerr != nil {
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
