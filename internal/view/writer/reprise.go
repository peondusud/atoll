package writer

import (
	"context"
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
		if v.State != catalog.OpPlanned || !w.handles.Has(v.Disk) {
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
