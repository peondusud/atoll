package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// DiskHolds is what the catalog knows a disk holds, for the disk commands (specs.md §18).
type DiskHolds struct {
	// Copies counts the disk's copies by state.
	Copies map[string]int64
	// Depending counts the files that depend on the disk, and First gives up to n of them by path in byte order: a
	// file with a copy there, ok or unavailable, that has fewer copies ok on disks online, in standby or read-only
	// than its level, or that is in conflict, as the disk may hold one of its versions.
	Depending int64
	First     []string
	// OnlyThere counts the files with a copy there and none ok or surplus anywhere else.
	OnlyThere int64
	// Planned counts the operations left planned on the disk.
	Planned int64
}

// dependsWhere is the condition of a file f that depends on the disk ?1.
const dependsWhere = `
	EXISTS (SELECT 1 FROM copies c WHERE c.file_id = f.id AND c.disk_id = ?1 AND c.state IN ('ok', 'unavailable'))
	AND (f.state = 'conflict' OR (
		SELECT count(*) FROM copies o JOIN disks d ON d.id = o.disk_id
		WHERE o.file_id = f.id AND o.disk_id <> ?1 AND o.state = 'ok' AND d.state IN ('online', 'standby', 'readonly')
	) < f.replicas_target + 1)`

// ReadDiskHolds reads what the catalog knows a disk holds, with the paths of up to n files that depend on it.
func ReadDiskHolds(ctx context.Context, db *sql.DB, disk string, n int) (*DiskHolds, error) {
	h := &DiskHolds{}
	var err error
	if h.Copies, err = copyStates(ctx, db, disk); err != nil {
		return nil, fmt.Errorf("copies of %q: %w", disk, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM files f WHERE"+dependsWhere, disk).Scan(&h.Depending); err != nil {
		return nil, fmt.Errorf("files on %q: %w", disk, err)
	}
	if h.First, err = firstPaths(ctx, db, "SELECT lpath FROM files f WHERE"+dependsWhere+" ORDER BY lpath LIMIT ?2", disk, n); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM files f
		WHERE EXISTS (SELECT 1 FROM copies c WHERE c.file_id = f.id AND c.disk_id = ?1)
			AND NOT EXISTS (SELECT 1 FROM copies o WHERE o.file_id = f.id AND o.disk_id <> ?1 AND o.state IN ('ok', 'surplus'))`,
		disk).Scan(&h.OnlyThere); err != nil {
		return nil, fmt.Errorf("files on %q: %w", disk, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM ops WHERE disk_id = ? AND state = 'planned'", disk).Scan(&h.Planned); err != nil {
		return nil, fmt.Errorf("operations on %q: %w", disk, err)
	}
	return h, nil
}

// copyStates counts a disk's copies by state.
func copyStates(ctx context.Context, db *sql.DB, disk string) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, "SELECT state, count(*) FROM copies WHERE disk_id = ? GROUP BY state", disk)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller says what was read
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err //nolint:wrapcheck // likewise
		}
		out[state] = count
	}
	return out, rows.Err() //nolint:wrapcheck // likewise
}
