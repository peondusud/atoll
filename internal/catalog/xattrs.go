package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// OpXattr gives a copy the user. extended attributes another copy of its file holds, as a pass does once Atoll's own
// view changed them on that one (specs.md §21.2).
const OpXattr = "xattr"

// finishXattrSQL gives the copy, found by disk, place and inode, the change time its disk holds now: setting an
// attribute moves it, and a row that kept the one before would read as an edit at the next look.
const finishXattrSQL = `
	UPDATE copies AS c SET ctime_ns = ?4 WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace

// WriteXattrKept says that a copy was made without some extended attributes of its source, which its filesystem
// refused: an event xattr-kept, at level warning, naming them (specs.md §14).
func WriteXattrKept(ctx context.Context, db *sql.DB, op *Op, names []string, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		return writeEventData(ctx, tx, nowNS, "warning", "xattr-kept", op, eventData{Names: names})
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// BeginXattr journals giving a copy the extended attributes of another, with its event, before anything is written.
func BeginXattr(ctx context.Context, db *sql.DB, disk, path, fileKey string, nowNS int64) (*Op, error) {
	return beginMetaOp(ctx, db, OpXattr, disk, path, fileKey, eventData{}, nowNS)
}

// FinishXattr says that a copy holds what it was given, or what its filesystem let it take: the operation is done, and
// the copy takes the change time its disk holds now.
func FinishXattr(ctx context.Context, db *sql.DB, op *Op, ctimeNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, finishXattrSQL, op.Disk, []byte(op.Path), op.FileKey, ctimeNS); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}
