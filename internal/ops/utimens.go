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
	return d.setMetadata(row, "times-set", func(f fsx.File) error { return f.SetTimes(fsx.OmitTime, mtime) })
}

// SetMode sets the mode of a copy, as SetMtime sets its time (specs.md §15.4).
func (d *Disk) SetMode(row *catalog.KnownCopy, mode uint32) (fsx.Stat, error) {
	return d.setMetadata(row, "mode-set", func(f fsx.File) error { return f.SetMode(mode) })
}

func (d *Disk) setMetadata(row *catalog.KnownCopy, point string, set func(fsx.File) error) (fsx.Stat, error) {
	f, _, err := d.OpenSource(row.Path, row)
	if err != nil {
		return fsx.Stat{}, err
	}
	// A file whose close failing changes nothing: what must be durable was synced.
	defer func() { _ = f.Close() }()
	if err := set(f); err != nil {
		return fsx.Stat{}, classify(err)
	}
	if err := f.Sync(); err != nil {
		return fsx.Stat{}, classify(err)
	}
	d.machine.Checkpoint(point)
	after, err := f.Info()
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return after, nil
}

// recoverUtimens resolves a time left planned (specs.md §17.2): the copy at its place, synced, with the time its
// event holds, at its disk's precision, finishes it; with the time its row says, nothing happened; anything else
// changed since.
func (d *Disk) recoverUtimens(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	wanted, ok, err := catalog.UtimensTime(ctx, db, op.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	row, st, same, err := d.metadataCopyNow(ctx, db, op)
	if err != nil {
		return err
	}
	if !same || !ok {
		return d.Undo(ctx, db, op, true, nowNS)
	}
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return d.Undo(ctx, db, op, true, nowNS)
	}
	// What the disk holds of the time, at its precision: NTFS keeps only multiples of 100 ns (specs.md §9).
	switch {
	case solver.SameTime(mtime, wanted, d.precision):
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

// recoverChmod resolves a mode left planned, as recoverUtimens a time: the mode its event holds finishes it, the mode
// its row says is nothing that happened, or a mode NTFS did not keep, and anything else changed since.
func (d *Disk) recoverChmod(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	wanted, ok, err := catalog.ChmodMode(ctx, db, op.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	row, st, same, err := d.metadataCopyNow(ctx, db, op)
	if err != nil {
		return err
	}
	if !same || !ok {
		return d.Undo(ctx, db, op, true, nowNS)
	}
	mode := int64(st.Mode)
	switch {
	case mode == wanted:
		ctime, err := solver.TimeNS(st.Ctime.Sec, st.Ctime.Nsec)
		if err != nil {
			return fmt.Errorf("change time: %w", err)
		}
		if err := catalog.FinishChmod(ctx, db, op, mode, ctime); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		return nil
	case row.Mode.Valid && mode == row.Mode.Int64:
		return d.Undo(ctx, db, op, false, nowNS)
	default:
		return d.Undo(ctx, db, op, true, nowNS)
	}
}

// metadataCopyNow looks at the copy an operation of a metadata change was applying to, synced first, as what is
// recorded must be what the disk keeps: what was set may not have been synced. It says whether that is the copy its
// row says, with the same inode and size.
func (d *Disk) metadataCopyNow(ctx context.Context, db *sql.DB, op *Op) (catalog.KnownCopy, fsx.Stat, bool, error) {
	row, err := catalog.CopyOf(ctx, db, op.Disk, op.Path)
	if err != nil {
		return row, fsx.Stat{}, false, fmt.Errorf("catalog: %w", err)
	}
	path := d.rootPlace(op.Path)
	seen, st, err := d.seen(path, op)
	if err != nil {
		return row, st, false, err
	}
	if seen == SeenSame {
		if err := d.syncFile(path); err != nil {
			return row, st, false, err
		}
		if seen, st, err = d.seen(path, op); err != nil {
			return row, st, false, err
		}
	}
	same := seen == SeenSame && row.Size.Valid && st.Size == row.Size.Int64 && row.FileKey.String == op.FileKey
	return row, st, same, nil
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
