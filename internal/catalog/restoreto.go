package catalog

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/peondusud/atoll/internal/layout"
)

// BeginRestoreTo journals a copy of a trash entry to another disk (specs.md §17.4): an operation create on that disk,
// whose part the operation names, with a restore event whose source is the entry's disk. The entry stays where it is.
func BeginRestoreTo(ctx context.Context, db *sql.DB, disk string, e *TrashEntry, nowNS int64) (*Op, error) {
	op := &Op{Disk: disk, Kind: OpCreate, Path: e.Path}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES (?, 'create', ?, ?, 'planned')",
			disk, []byte(e.Path), nowNS)
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
		return writeEventData(ctx, tx, nowNS, "info", OpRestore, op, eventData{Source: e.Disk})
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", e.Path, err)
	}
	return op, nil
}

// IsRestoreTo says whether an operation create copies a trash entry to another disk, as its event says.
func IsRestoreTo(ctx context.Context, db *sql.DB, opID int64) (bool, error) {
	var is bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE kind = 'restore' AND json_extract(data, '$.op') = ?)", opID).Scan(&is)
	if err != nil {
		return false, fmt.Errorf("event: %w", err)
	}
	return is, nil
}

// FinishRestoreTo says that a copy of a trash entry is at its path on another disk. It is recorded as a restored copy
// is (FinishRestore), and the entry stays in its trash.
func FinishRestoreTo(ctx context.Context, db *sql.DB, op *Op, nowNS int64, c *NewCopy, links *Links) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if err := dropCopyRow(ctx, tx, op.Disk, op.Path); err != nil {
			return err
		}
		var behind bool
		err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM files f JOIN copies k ON k.file_id = f.id WHERE f.lpath = ? AND k.disk_id = ?)", []byte(op.Path), op.Disk).Scan(&behind)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if !behind {
			if err := writeCopies(ctx, tx, op.Disk, nowNS, []NewCopy{*c}); err != nil {
				return err
			}
		}
		return writeLinks(ctx, tx, op.Disk, links)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}
