package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OpRmdir removes a folder below the root that Atoll made, once Atoll left it empty (specs.md §17.5).
const OpRmdir = "rmdir"

// OwnFolder gives the inode a disk's folder row records, and says whether that row is a folder Atoll made.
func OwnFolder(ctx context.Context, db *sql.DB, disk, folder string) (fileKey string, own bool, err error) {
	var key sql.NullString
	err = db.QueryRowContext(ctx, "SELECT file_key FROM entries WHERE disk_id = ? AND lpath = ? AND kind = 'dir' AND created_by = 'atoll'",
		disk, []byte(folder)).Scan(&key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("entries: %w", err)
	}
	// A folder whose identity no row recorded cannot be told from another one made at its name since.
	return key.String, key.Valid && key.String != "", nil
}

// NothingBelow says whether the catalog holds nothing of a disk below a folder: no copy, at its file's path or behind,
// and no entry, ignored files included.
func NothingBelow(ctx context.Context, db *sql.DB, disk, folder string) (bool, error) {
	copies, err := CopiesBelow(ctx, db, disk, folder)
	if err != nil || copies > 0 {
		return false, err
	}
	lo, hi := folderRange(folder)
	var entry bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM entries WHERE lpath >= ? AND lpath < ? AND disk_id = ?)", lo, hi, disk).Scan(&entry); err != nil {
		return false, fmt.Errorf("entries: %w", err)
	}
	return !entry, nil
}

// BeginRmdir journals the removal of a folder Atoll made, with its event, before it runs.
func BeginRmdir(ctx context.Context, db *sql.DB, disk, folder, fileKey string, nowNS int64) (*Op, error) {
	op := &Op{Disk: disk, Kind: OpRmdir, Path: folder, FileKey: fileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES (?, 'rmdir', ?, ?, ?, 'planned')",
			disk, []byte(folder), fileKey, nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		return writeEvent(ctx, tx, nowNS, "info", OpRmdir, op, "")
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", folder, err)
	}
	return op, nil
}

// FinishRmdir says that a folder is gone, and its row with it.
func FinishRmdir(ctx context.Context, db *sql.DB, op *Op) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM entries WHERE disk_id = ? AND lpath = ?", op.Disk, []byte(op.Path)); err != nil {
			return fmt.Errorf("entry %q: %w", op.Path, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}
