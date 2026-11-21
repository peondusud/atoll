package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// PlanSet gives the ids of the files a plan limited to what may have changed takes (specs.md §15.13): the files at or
// below the names a pass looked at, by disk, and the copies left behind there; every file not at its level, or that
// holds a conflict; every file with a copy on a disk of the SSD cache, whose residency runs out with time alone; and
// every file with a copy to make or to transfer, whose room the plan holds for it.
func PlanSet(ctx context.Context, db *sql.DB, names map[string][]string, cacheDisks []string) (map[int64]bool, error) {
	out := map[int64]bool{}
	add := func(query string, args ...any) error {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("%w", err)
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w", err)
		}
		return nil
	}
	for disk, list := range names {
		for _, name := range TopNames(list) {
			if err := atOrBelow("SELECT id FROM files WHERE <cond>", "lpath", 1, name, nil, func(q string, a []any) error { return add(q, a...) }); err != nil {
				return nil, fmt.Errorf("files: %w", err)
			}
		}
		if err := behindAt(ctx, db, disk, list, out); err != nil {
			return nil, fmt.Errorf("copies: %w", err)
		}
	}
	if err := add("SELECT id FROM files WHERE state <> 'ok' OR conflict_held"); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	for _, disk := range cacheDisks {
		if err := add("SELECT file_id FROM copies WHERE disk_id = ?", disk); err != nil {
			return nil, fmt.Errorf("copies: %w", err)
		}
	}
	if err := add("SELECT file_id FROM jobs WHERE state IN ('queued', 'running') AND kind IN ('replicate', 'transfer') AND file_id IS NOT NULL"); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return out, nil
}

// RunningCopies gives, by file, the disks of its pending copies whose replicate job is running: an operation of it is
// left planned, which only the reprise of that disk concludes, so the plan keeps that copy, counts it and never cancels
// it (specs.md §13.4, §14.1). Few, and none most of the time.
func RunningCopies(ctx context.Context, db *sql.DB) (map[int64][]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT file_id, dst_disk FROM jobs WHERE kind = 'replicate' AND state = ? ORDER BY file_id, dst_disk", JobRunning)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var disk string
		if err := rows.Scan(&id, &disk); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		out[id] = append(out[id], disk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return out, nil
}

// RunningFiles gives the files with a job running, which wait for every decision of a pass, as their modifications do
// (specs.md §14.1, §15.4, §15.12).
func RunningFiles(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT file_id FROM jobs WHERE state = ? AND file_id IS NOT NULL", JobRunning)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return out, nil
}

// behindAt adds the files whose copy on a disk is left behind their name at or below one of these names: few, read for
// the disk, and kept when below a name.
func behindAt(ctx context.Context, db *sql.DB, disk string, names []string, out map[int64]bool) error {
	rows, err := db.QueryContext(ctx, "SELECT file_id, old_lpath FROM copies WHERE disk_id = ? AND old_lpath IS NOT NULL", disk)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var p []byte
		if err := rows.Scan(&id, &p); err != nil {
			return fmt.Errorf("%w", err)
		}
		for _, name := range names {
			if string(p) == name || strings.HasPrefix(string(p), name+"/") {
				out[id] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// FileIDsAt gives the ids of the files at these names, those there are.
func FileIDsAt(ctx context.Context, db *sql.DB, names []string) ([]int64, error) {
	var out []int64
	for _, name := range names {
		var id int64
		switch err := db.QueryRowContext(ctx, "SELECT id FROM files WHERE lpath = ?", []byte(name)).Scan(&id); {
		case isNoRows(err):
		case err != nil:
			return nil, fmt.Errorf("files: %w", err)
		default:
			out = append(out, id)
		}
	}
	return out, nil
}

// FileIDsBelow gives the ids of the files at or below a name.
func FileIDsBelow(ctx context.Context, db *sql.DB, name string) ([]int64, error) {
	var out []int64
	err := atOrBelow("SELECT id FROM files WHERE <cond>", "lpath", 1, name, nil, func(query string, args []any) error {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("%w", err)
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	return out, nil
}
