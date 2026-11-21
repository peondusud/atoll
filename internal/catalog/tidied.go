package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// tidiedKey is the key of meta that holds the last operation whose folders a run looked at (specs.md §17.5).
const tidiedKey = "folders_tidied_op"

// FoldersTidiedThrough gives the last operation whose folders a run looked at for its own empty folders, 0 when none
// did (specs.md §17.5).
func FoldersTidiedThrough(ctx context.Context, db *sql.DB) (int64, error) {
	var text string
	switch err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", tidiedKey).Scan(&text); {
	case isNoRows(err):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("meta: %w", err)
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("meta: %s is %q", tidiedKey, text)
	}
	return id, nil
}

// SetFoldersTidiedThrough records that the folders every operation up to id left were looked at.
func SetFoldersTidiedThrough(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)", tidiedKey, strconv.FormatInt(id, 10)); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	return nil
}

// HighestOp gives the id of the last operation journaled, 0 when there is none.
func HighestOp(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT coalesce(max(id), 0) FROM ops").Scan(&id); err != nil {
		return 0, fmt.Errorf("ops: %w", err)
	}
	return id, nil
}

// leftSinceQuery reads the operations done after an id, up to another, that took something away from a place below a
// root: a move to the trash and a rename, which name that place as their path.
const leftSinceQuery = "SELECT disk_id, path FROM ops WHERE id > ? AND id <= ? AND kind IN ('trash', 'rename') AND state = 'done' ORDER BY id"

// LeftSince calls each with the disk and the place of every operation done after one id and up to another that took
// a copy or a folder away from its place (specs.md §17.5), in the order of the journal.
func LeftSince(ctx context.Context, db *sql.DB, after, upTo int64, each func(disk, place string)) error {
	rows, err := db.QueryContext(ctx, leftSinceQuery, after, upTo)
	if err != nil {
		return fmt.Errorf("ops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var disk string
		var place []byte
		if err := rows.Scan(&disk, &place); err != nil {
			return fmt.Errorf("ops: %w", err)
		}
		each(disk, string(place))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ops: %w", err)
	}
	return nil
}
