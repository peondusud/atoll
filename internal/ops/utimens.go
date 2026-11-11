package ops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// SetMtime sets the modification time of a copy, after a look that its name still holds what row says, leaving
// its access time as it is, and syncs the file (specs.md §15.4). It says what the file holds after. ErrGone means
// the name no longer holds that file, and that nothing was written.
func (d *Disk) SetMtime(row *catalog.KnownCopy, mtime fsx.Time) (fsx.Stat, error) {
	f, _, err := d.OpenSource(row.Path, row)
	if err != nil {
		return fsx.Stat{}, err
	}
	// A file whose close failing changes nothing: what must be durable was synced.
	defer func() { _ = f.Close() }()
	if err := f.SetTimes(fsx.OmitTime, mtime); err != nil {
		return fsx.Stat{}, classify(err)
	}
	if err := f.Sync(); err != nil {
		return fsx.Stat{}, classify(err)
	}
	d.machine.Checkpoint("times-set")
	after, err := f.Info()
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return after, nil
}

// recoverUtimens resolves a time left planned (specs.md §17.2): the copy at its place, synced, with the time its
// event holds finishes it; with the time its row says, nothing happened; anything else changed since.
func (d *Disk) recoverUtimens(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	wanted, ok, err := catalog.UtimensTime(ctx, db, op.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	row, err := catalog.CopyOf(ctx, db, op.Disk, op.Path)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	path := d.rootPlace(op.Path)
	seen, st, err := d.seen(path, op)
	if err != nil {
		return err
	}
	if seen == SeenSame {
		// What is recorded must be what the disk keeps: the time may have been set and not yet synced.
		if err := d.syncFile(path); err != nil {
			return err
		}
		if seen, st, err = d.seen(path, op); err != nil {
			return err
		}
	}
	if seen != SeenSame || !ok || !row.Size.Valid || st.Size != row.Size.Int64 || row.FileKey.String != op.FileKey {
		return d.Undo(ctx, db, op, true, nowNS)
	}
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return d.Undo(ctx, db, op, true, nowNS)
	}
	switch {
	case mtime == wanted:
		ctime, err := solver.TimeNS(st.Ctime.Sec, st.Ctime.Nsec)
		if err != nil {
			return fmt.Errorf("change time: %w", err)
		}
		if err := catalog.FinishUtimens(ctx, db, op, mtime, ctime); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		return nil
	case row.MtimeNS.Valid && mtime == row.MtimeNS.Int64:
		return d.Undo(ctx, db, op, false, nowNS)
	default:
		return d.Undo(ctx, db, op, true, nowNS)
	}
}

// syncFile makes durable what a file holds of itself, its times included, through a handle on it.
func (d *Disk) syncFile(path []string) error {
	dir, err := d.open(path[:len(path)-1])
	if err != nil {
		return classify(err)
	}
	defer dir.close()
	f, err := dir.OpenFile(path[len(path)-1])
	if err != nil {
		return classify(err)
	}
	// A file whose close failing changes nothing: the sync said what it had to.
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return classify(err)
	}
	return nil
}
