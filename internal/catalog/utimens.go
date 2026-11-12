package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// The operations that apply a metadata change to a copy (specs.md §15.4).
const (
	OpUtimens = "utimens"
	OpChmod   = "chmod"
	OpChown   = "chown"
)

// beginMetaOp journals an operation of a metadata change, with the event that holds what it applies, before anything
// is written: the reprise needs that to tell the operation that ran from a copy changed since (specs.md §17.2).
func beginMetaOp(ctx context.Context, db *sql.DB, kind, disk, path, fileKey string, data eventData, nowNS int64) (*Op, error) {
	op := &Op{Disk: disk, Kind: kind, Path: path, FileKey: fileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES (?, ?, ?, ?, ?, 'planned')",
			disk, kind, []byte(path), fileKey, nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		return writeEventData(ctx, tx, nowNS, "info", kind, op, data)
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", path, err)
	}
	return op, nil
}

// opEventValue gives what the event of an operation of a metadata change holds, and false when there is none.
func opEventValue(ctx context.Context, db *sql.DB, kind, field string, opID int64) (int64, bool, error) {
	var value sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT json_extract(data, '$.' || ?) FROM events WHERE kind = ? AND json_extract(data, '$.op') = ?",
		field, kind, opID).Scan(&value)
	switch {
	case isNoRows(err):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("event: %w", err)
	}
	return value.Int64, value.Valid, nil
}

// finishMetaOp says that an operation of a metadata change is done, and gives the copy, found by disk, place and
// inode, what its disk holds now (specs.md §15.4).
func finishMetaOp(ctx context.Context, db *sql.DB, op *Op, query string, value, ctimeNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, query, op.Disk, []byte(op.Path), op.FileKey, value, ctimeNS); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// BeginUtimens journals the modification time a metadata change applies to a copy, in the event's mtime_ns.
func BeginUtimens(ctx context.Context, db *sql.DB, disk, path, fileKey string, mtimeNS, nowNS int64) (*Op, error) {
	return beginMetaOp(ctx, db, OpUtimens, disk, path, fileKey, eventData{MtimeNS: &mtimeNS}, nowNS)
}

// UtimensTime gives the time an operation utimens applies, from its event, and false when there is none.
func UtimensTime(ctx context.Context, db *sql.DB, opID int64) (int64, bool, error) {
	return opEventValue(ctx, db, OpUtimens, "mtime_ns", opID)
}

// FinishUtimens says that a time is applied: the copy takes the modification and change times its disk holds now.
func FinishUtimens(ctx context.Context, db *sql.DB, op *Op, mtimeNS, ctimeNS int64) error {
	return finishMetaOp(ctx, db, op, finishUtimensSQL, mtimeNS, ctimeNS)
}

// BeginChmod journals the mode a metadata change applies to a copy, in the event's mode.
func BeginChmod(ctx context.Context, db *sql.DB, disk, path, fileKey string, mode, nowNS int64) (*Op, error) {
	return beginMetaOp(ctx, db, OpChmod, disk, path, fileKey, eventData{Mode: &mode}, nowNS)
}

// ChmodMode gives the mode an operation chmod applies, from its event, and false when there is none.
func ChmodMode(ctx context.Context, db *sql.DB, opID int64) (int64, bool, error) {
	return opEventValue(ctx, db, OpChmod, "mode", opID)
}

// FinishChmod says that a mode is applied: the copy takes the mode and change time its disk holds now.
func FinishChmod(ctx context.Context, db *sql.DB, op *Op, mode, ctimeNS int64) error {
	return finishMetaOp(ctx, db, op, finishChmodSQL, mode, ctimeNS)
}

// BeginChown journals the owner a metadata change applies to a copy, in the event's uid and gid.
func BeginChown(ctx context.Context, db *sql.DB, disk, path, fileKey string, uid, gid, nowNS int64) (*Op, error) {
	return beginMetaOp(ctx, db, OpChown, disk, path, fileKey, eventData{UID: &uid, GID: &gid}, nowNS)
}

// ChownOwner gives the owner an operation chown applies, from its event, and false when there is none.
func ChownOwner(ctx context.Context, db *sql.DB, opID int64) (uid, gid int64, ok bool, err error) {
	var u, g sql.NullInt64
	err = db.QueryRowContext(ctx, "SELECT json_extract(data, '$.uid'), json_extract(data, '$.gid') FROM events WHERE kind = ? AND json_extract(data, '$.op') = ?",
		OpChown, opID).Scan(&u, &g)
	switch {
	case isNoRows(err):
		return 0, 0, false, nil
	case err != nil:
		return 0, 0, false, fmt.Errorf("event: %w", err)
	}
	return u.Int64, g.Int64, u.Valid && g.Valid, nil
}

// FinishChown says that an owner is applied: the copy takes the owner, mode and change time its disk holds now, as a
// chown may clear setuid and setgid.
func FinishChown(ctx context.Context, db *sql.DB, op *Op, uid, gid, mode, ctimeNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, finishChownSQL, op.Disk, []byte(op.Path), op.FileKey, uid, gid, mode, ctimeNS); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}
