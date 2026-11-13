package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ConflictFiles reads the files in conflict, with their copies, by path in byte order, a page at a time (specs.md
// §15.7).
func ConflictFiles(ctx context.Context, r *FileReader) ([]FileRow, error) {
	return r.page(ctx, `SELECT * FROM files WHERE lpath > ? AND state = 'conflict' ORDER BY lpath LIMIT ?`)
}

// BeginResolve writes the conflict-resolve event of a file, before any of its copies moves: atoll conflicts resolve
// keeps the copy on disk keep, and sends the others to the trash (specs.md §15.7).
func BeginResolve(ctx context.Context, db *sql.DB, path, keep string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		data := &eventRow{Disk: keep}
		data.setPath(path)
		return insertEvent(ctx, tx, nowNS, "info", "conflict-resolve", data)
	})
}

// ErrStillAConflict says that a file in conflict still has a copy other than the one kept.
var ErrStillAConflict = errors.New("another copy is left")

// SeenCopy is what a look found of a copy now.
type SeenCopy struct {
	Size, MtimeNS, CtimeNS int64
	BtimeNS                *int64
	FileKey                string
	Nlink                  int64
}

// RefreshCopy records what a copy of a file in conflict holds now, as a walk would for another file: a walk leaves
// the copies of a file in conflict as they were found (specs.md §15.4, §15.7). What it was verified against goes.
func RefreshCopy(ctx context.Context, db *sql.DB, fileID int64, disk string, seen *SeenCopy) error {
	res, err := db.ExecContext(ctx, `UPDATE copies SET size = ?, mtime_ns = ?, ctime_ns = ?, btime_ns = ?, file_key = ?, nlink = ?, verified_ns = NULL,
		gone_ns = NULL, gone_kind = NULL WHERE file_id = ? AND disk_id = ? AND EXISTS(SELECT 1 FROM files WHERE id = ? AND state = 'conflict')`,
		seen.Size, seen.MtimeNS, seen.CtimeNS, seen.BtimeNS, seen.FileKey, seen.Nlink, fileID, disk, fileID)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("copy on %s: not one of a file in conflict", disk)
	}
	return nil
}

// DropCopy removes the row of a copy of a file in conflict that is no longer on its disk, never the last one: what it
// held went with it, and nothing is left to send to the trash (specs.md §15.7).
func DropCopy(ctx context.Context, db *sql.DB, fileID int64, disk string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM copies WHERE file_id = ?1 AND disk_id = ?2
		AND EXISTS(SELECT 1 FROM files WHERE id = ?1 AND state = 'conflict')
		AND EXISTS(SELECT 1 FROM copies o WHERE o.file_id = ?1 AND o.disk_id <> ?2)`, fileID, disk)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("copy on %s: not one of a file in conflict, or its last", disk)
	}
	return nil
}

// ResolveConflict ends the conflict of a file whose copies but the one on keep have left (specs.md §15.7). The file
// takes that copy's size and modification time; its hash and chunk hashes go, as they may be another version's, and
// so does the copy's verification, all to be read again; and it is pending, for the next pass to plan its replicas.
// With another copy left, it changes nothing, and gives ErrStillAConflict; a file no longer in conflict is left as it
// is.
func ResolveConflict(ctx context.Context, db *sql.DB, fileID int64, keep string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		var others int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM copies WHERE file_id = ? AND disk_id <> ?", fileID, keep).Scan(&others); err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		if others > 0 {
			return ErrStillAConflict
		}
		var size, mtime sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT size, mtime_ns FROM copies WHERE file_id = ? AND disk_id = ?", fileID, keep).Scan(&size, &mtime)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no copy on %s", keep)
		}
		if err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		if !size.Valid || !mtime.Valid {
			return fmt.Errorf("the copy on %s has no size or time recorded", keep)
		}
		res, err := tx.ExecContext(ctx, `UPDATE files SET size = ?, mtime_ns = ?, blake3 = NULL, state = 'pending', original_disk = ?, updated_ns = ?
			WHERE id = ? AND state = 'conflict'`, size.Int64, mtime.Int64, keep, nowNS, fileID)
		if err != nil {
			return fmt.Errorf("files: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return nil //nolint:nilerr // no longer in conflict: nothing to end
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM file_chunks WHERE file_id = ?", fileID); err != nil {
			return fmt.Errorf("chunks: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET verified_ns = NULL WHERE file_id = ? AND disk_id = ?", fileID, keep); err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		// What was queued for it was planned from another version.
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE file_id = ? AND state = 'queued'", fileID); err != nil {
			return fmt.Errorf("jobs: %w", err)
		}
		return nil
	})
}
