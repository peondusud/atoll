package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// OpUtimens journals the modification time a metadata change applies to a copy (specs.md §15.4).
const OpUtimens = "utimens"

// BeginUtimens journals the time a metadata change applies to a copy, with the event that holds it, before anything
// is written: the reprise needs that time to tell the operation that ran from a copy changed since (specs.md §17.2).
func BeginUtimens(ctx context.Context, db *sql.DB, disk, path, fileKey string, mtimeNS, nowNS int64) (*Op, error) {
	op := &Op{Disk: disk, Kind: OpUtimens, Path: path, FileKey: fileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES (?, 'utimens', ?, ?, ?, 'planned')",
			disk, []byte(path), fileKey, nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		return writeEventData(ctx, tx, nowNS, "info", OpUtimens, op, eventData{MtimeNS: &mtimeNS})
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", path, err)
	}
	return op, nil
}

// UtimensTime gives the time an operation utimens applies, from its event, and false when there is none.
func UtimensTime(ctx context.Context, db *sql.DB, opID int64) (int64, bool, error) {
	var mtime sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT json_extract(data, '$.mtime_ns') FROM events WHERE kind = ? AND json_extract(data, '$.op') = ?",
		OpUtimens, opID).Scan(&mtime)
	switch {
	case isNoRows(err):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("event: %w", err)
	}
	return mtime.Int64, mtime.Valid, nil
}

// FinishUtimens says that a time is applied: the operation done, and the copy, found by disk, place and inode,
// takes the times its disk holds now (specs.md §15.4).
func FinishUtimens(ctx context.Context, db *sql.DB, op *Op, mtimeNS, ctimeNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, finishUtimensSQL, op.Disk, []byte(op.Path), op.FileKey, mtimeNS, ctimeNS)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}
