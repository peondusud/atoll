package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// DiskHolds is what the catalog knows a disk holds, for the disk commands (specs.md §18).
type DiskHolds struct {
	// Copies counts the disk's copies by state.
	Copies map[string]int64
	// Depending counts the files that depend on the disk, and First gives up to n of them by path in byte order: a
	// file with a copy there, ok, surplus or unavailable, that has fewer copies ok or surplus on the disks that hold
	// one than its level, or that is in conflict, or corrupt and holding one, as the disk may hold one of its versions.
	// A surplus copy counts as an ok one, there and elsewhere, as the plan that marked it may have counted a disk gone
	// since (specs.md §25; conformance/models/README.md, flaw 29).
	Depending int64
	First     []string
	// OnlyThere counts the files with a copy there and none ok or surplus anywhere else.
	OnlyThere int64
	// Planned counts the operations left planned on the disk.
	Planned int64
}

// dependsWhere is the condition of a file f that depends on the disk ?1, the disks that hold a copy for it the JSON
// list ?2.
const dependsWhere = `
	EXISTS (SELECT 1 FROM copies c WHERE c.file_id = f.id AND c.disk_id = ?1 AND c.state IN ('ok', 'surplus', 'unavailable'))
	AND (f.state = 'conflict' OR f.conflict_held OR (
		SELECT count(*) FROM copies o
		WHERE o.file_id = f.id AND o.disk_id <> ?1 AND o.state IN ('ok', 'surplus') AND o.disk_id IN (SELECT value FROM json_each(?2))
	) < f.replicas_target + 1)`

// heldSQL gives the disks that hold a copy as the catalog last recorded them: online, in standby or readonly, which a
// draining disk is when mounted read-only; one draining online is recorded draining, and counts for nothing.
const heldSQL = "SELECT id FROM disks WHERE state IN ('online', 'standby', 'readonly') ORDER BY id"

// ReadDiskHolds reads what the catalog knows a disk holds, with the paths of up to n files that depend on it, the disks
// that hold a copy as the catalog last recorded them.
func ReadDiskHolds(ctx context.Context, db *sql.DB, disk string, n int) (*DiskHolds, error) {
	rows, err := db.QueryContext(ctx, heldSQL)
	if err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	held := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("disks: %w", err)
		}
		held = append(held, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	return ReadDiskHoldsAmong(ctx, db, disk, n, held)
}

// ReadDiskHoldsAmong reads what the catalog knows a disk holds, with the paths of up to n files that depend on it, held
// the disks that hold a copy: those its mount checks found so, as atoll disk remove gives them (specs.md §25).
func ReadDiskHoldsAmong(ctx context.Context, db *sql.DB, disk string, n int, held []string) (*DiskHolds, error) {
	list, err := json.Marshal(append([]string{}, held...))
	if err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	h := &DiskHolds{}
	if h.Copies, err = copyStates(ctx, db, disk); err != nil {
		return nil, fmt.Errorf("copies of %q: %w", disk, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM files f WHERE"+dependsWhere, disk, string(list)).Scan(&h.Depending); err != nil {
		return nil, fmt.Errorf("files on %q: %w", disk, err)
	}
	if h.First, err = firstPaths(ctx, db, "SELECT lpath FROM files f WHERE"+dependsWhere+" ORDER BY lpath LIMIT ?3", disk, string(list), n); err != nil {
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
