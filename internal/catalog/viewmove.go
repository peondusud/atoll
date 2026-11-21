package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/layout"
)

// A new file Atoll's own view moves to another disk once a write ran out of space (specs.md §9, §21.2): its row of
// view_ops, kind move, names the one operation of ops it runs now, each journaled with its event in the transaction that
// points op_id at it, the one before it done there: the create of its part on the disk it goes to, then the trash of the
// copy it leaves on the full disk, surplus, then the unlink that purges that copy. So the one planned operation of a move
// is always the view's, which no command's or daemon's reprise resolves.

// ReasonSurplus is the reason of the trash a copy of the SSD cache's transfer, or the copy a move of the view leaves on a
// full disk, goes to the trash with (specs.md §17, §19, §21.2).
const ReasonSurplus = "surplus"

// BeginViewMove journals a move of the view before anything is made: the create of its part on v.Disk, planned, whose
// part is <op id>.part, journaled with its replicate event naming v.Disk2, the full disk, as its source, and the view's
// row, planned, that names it, in one transaction.
func BeginViewMove(ctx context.Context, db *sql.DB, v *ViewOp) (*Op, error) {
	op := &Op{Disk: v.Disk, Kind: OpCreate, Path: v.Path}
	v.Kind, v.State, v.Folder, v.Path2 = ViewMove, OpPlanned, false, ""
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES (?, 'create', ?, ?, 'planned')", v.Disk, []byte(v.Path), v.AtNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		op.Path2 = layout.PartName(op.ID)
		if _, err := tx.ExecContext(ctx, "UPDATE ops SET path2 = ? WHERE id = ?", []byte(op.Path2), op.ID); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if err := writeEventData(ctx, tx, v.AtNS, "info", "replicate", op, eventData{Source: v.Disk2}); err != nil {
			return err
		}
		v.OpID = op.ID
		return insertViewOp(ctx, tx, v)
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", v.Path, err)
	}
	v.OpKind = OpCreate
	return op, nil
}

// ViewMoveTrash journals the trash of the copy a move leaves on the full disk, once its part is at its name on the disk
// it goes to: the create done, the operation trash on v.Disk2, its trash row and its event, and op_id pointing at it, in
// one transaction (specs.md §17.1, §21.2).
func ViewMoveTrash(ctx context.Context, db *sql.DB, v *ViewOp, s *TrashStart) (*Op, error) {
	var op *Op
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, v.OpID, OpDone); err != nil {
			return err
		}
		var err error
		if op, err = beginTrashTx(ctx, tx, s); err != nil {
			return err
		}
		return pointViewMove(ctx, tx, v, op.ID)
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", v.Path, err)
	}
	v.OpID, v.OpKind = op.ID, OpTrash
	return op, nil
}

// ViewMovePurge journals the purge of the copy a move sent to the full disk's trash: the trash done, a row the catalog
// may hold of that copy gone as §17.1 says, the operation unlink, its purge event, and op_id pointing at it, in one
// transaction (specs.md §17.4, §21.2).
func ViewMovePurge(ctx context.Context, db *sql.DB, v *ViewOp, trashed *Op, nowNS int64) (*Op, error) {
	op := &Op{Disk: trashed.Disk, Kind: OpUnlink, Path: trashed.Path, Path2: trashed.Path2, FileKey: trashed.FileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, trashed.ID, OpDone); err != nil {
			return err
		}
		if err := finishCopy(ctx, tx, trashed); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, path2, file_key, started_ns, state) VALUES (?, 'unlink', ?, ?, ?, ?, 'planned')",
			op.Disk, []byte(op.Path), []byte(op.Path2), op.FileKey, nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if err := writeEvent(ctx, tx, nowNS, "info", "purge", op, ""); err != nil {
			return err
		}
		return pointViewMove(ctx, tx, v, op.ID)
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", v.Path, err)
	}
	v.OpID, v.OpKind = op.ID, OpUnlink
	return op, nil
}

func pointViewMove(ctx context.Context, tx *sql.Tx, v *ViewOp, opID int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE view_ops SET op_id = ? WHERE id = ? AND kind = 'move'", opID, v.ID); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	return nil
}

// ErrStillPlanned says that the operation a move names is still planned, which leaves the move planned too.
var ErrStillPlanned = errors.New("the operation of the move is still planned")

// EndViewMove ends a move by what became of the operation it names, in one transaction: that operation takes opState,
// and the move viewState. A trash done drops a row the catalog may hold of the copy it moved, as §17.1 says, and one
// undone its trash row; an unlink done, its entry of the trash. warn writes an op-undone event, as §17.2 says, for an
// operation undone that found what it moves no longer what it expected (specs.md §21.2). An opState of "" leaves the
// operation as a reprise resolved it, and ErrStillPlanned when that left it planned.
func EndViewMove(ctx context.Context, db *sql.DB, v *ViewOp, opState, viewState string, warn bool, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		op, err := opInTx(ctx, tx, v.OpID)
		if err != nil {
			return err
		}
		if opState == "" {
			if op.state == OpPlanned {
				return ErrStillPlanned
			}
			_, err := tx.ExecContext(ctx, "UPDATE view_ops SET state = ? WHERE id = ? AND kind = 'move'", viewState, v.ID)
			if err != nil {
				return fmt.Errorf("view journal: %w", err)
			}
			return nil
		}
		if err := markOp(ctx, tx, op.ID, opState); err != nil {
			return err
		}
		switch {
		case op.Kind == OpTrash && opState == OpDone:
			if err := finishCopy(ctx, tx, op.Op); err != nil {
				return err
			}
		case op.Kind == OpTrash && opState == OpUndone:
			if _, err := tx.ExecContext(ctx, undoTrashSQL, []byte(op.Path), op.ID); err != nil {
				return fmt.Errorf("trash: %w", err)
			}
		case op.Kind == OpUnlink && opState == OpDone:
			if err := dropTrashRow(ctx, tx, op.Op); err != nil {
				return err
			}
		}
		if warn && opState == OpUndone {
			if err := writeEvent(ctx, tx, nowNS, "warning", "op-undone", op.Op, ""); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE view_ops SET state = ? WHERE id = ? AND kind = 'move'", viewState, v.ID); err != nil {
			return fmt.Errorf("view journal: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", v.Path, err)
	}
	v.State = viewState
	return nil
}

// stateOp is an operation of the journal, with its state.
type stateOp struct {
	*Op
	state string
}

func opInTx(ctx context.Context, tx *sql.Tx, id int64) (stateOp, error) {
	op := stateOp{Op: &Op{ID: id}}
	var path, path2 []byte
	err := tx.QueryRowContext(ctx, "SELECT disk_id, kind, path, COALESCE(path2, x''), COALESCE(file_key, ''), state FROM ops WHERE id = ?", id).
		Scan(&op.Disk, &op.Kind, &path, &path2, &op.FileKey, &op.state)
	if err != nil {
		return stateOp{}, fmt.Errorf("journal: %w", err)
	}
	op.Path, op.Path2 = string(path), string(path2)
	return op, nil
}

// OpStateOf gives the state of an operation of the journal, and false when there is none of that id.
func OpStateOf(ctx context.Context, db *sql.DB, id int64) (string, bool, error) {
	var state string
	err := db.QueryRowContext(ctx, "SELECT state FROM ops WHERE id = ?", id).Scan(&state)
	switch {
	case isNoRows(err):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("journal: %w", err)
	}
	return state, true, nil
}
