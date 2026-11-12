package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

// ErrNotEmpty means that a folder Atoll would remove holds something: it stays (specs.md §17.5).
var ErrNotEmpty = errors.New("the folder is not empty")

// RemoveFolder removes a folder below the root that Atoll made, when its name still holds that folder, by inode, and
// it is empty, then gives its parent its time back and syncs it (specs.md §17.5). ErrGone means that the name no
// longer holds that folder, and ErrNotEmpty that it holds something: either way it stays.
func (d *Disk) RemoveFolder(op *Op) error {
	if err := d.Verify(); err != nil {
		return err
	}
	path := d.rootPlace(op.Path)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	if _, err := holdsFolder(parent.Dir, name, op.FileKey); err != nil {
		return err
	}
	parentTime, err := parent.Info()
	if err != nil {
		return classify(err)
	}
	// Only an empty folder passes: what came into it since the catalog was read keeps it.
	if err := parent.Rmdir(name); err != nil {
		switch {
		case fsx.IsAbsent(err):
			return fmt.Errorf("%w: %w", ErrGone, err)
		case fsx.IsNotEmpty(err), fsx.IsExisting(err):
			return fmt.Errorf("%w: %w", ErrNotEmpty, err)
		default:
			return classify(err)
		}
	}
	d.machine.Checkpoint("rmdir-removed")
	if err := restoreTime(parent.Dir, parentTime.Mtime); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("rmdir-synced")
	return nil
}

// recoverRmdir looks at the folder's name: nothing there, the removal went through; the folder, by inode, nothing
// happened; anything else, someone else's, which stays (specs.md §17.2).
func (d *Disk) recoverRmdir(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	path := d.rootPlace(op.Path)
	st, there, err := d.stat(path)
	if err != nil {
		return err
	}
	source := SeenAbsent
	if there {
		source = SeenOther
		if st.Kind == fsx.KindDir && keyOf(&st) == op.FileKey {
			source = SeenSame
		}
	}
	decision, err := ResolveRmdir(source)
	if err != nil {
		return err
	}
	if !decision.Finish {
		return d.Undo(ctx, db, op, decision.Warn, nowNS)
	}
	// The parent may hold the removal only in its cache.
	if err := d.syncWay(path[:len(path)-1]); err != nil {
		return err
	}
	if err := catalog.FinishRmdir(ctx, db, op); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}
