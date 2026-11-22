package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// Counted is what the metrics read of the catalog after each pass (specs.md §23.1), each in one read: the files of
// each state and their bytes, and by disk what Counted.Disks says.
type Counted struct {
	Files map[string]CountedFiles
	Disks map[string]*CountedDisk
}

// CountedFiles is a number of files and their bytes.
type CountedFiles struct {
	Files, Bytes int64
}

// CountedDisk is what the catalog holds of a disk: its copies and their bytes, as atoll status counts them; the
// entries of its trash and their bytes, as atoll trash list gives them; and the copies the scrub has to read there.
type CountedDisk struct {
	Copies, Bytes                 int64
	TrashEntries, TrashBytes      int64
	ScrubDueCopies, ScrubDueBytes int64
}

// CountForMetrics counts what the metrics give, a copy verified at or before scrubDueNS being due for the scrub.
func CountForMetrics(ctx context.Context, db *sql.DB, scrubDueNS int64) (*Counted, error) {
	out := &Counted{Files: map[string]CountedFiles{}, Disks: map[string]*CountedDisk{}}
	disk := func(id string) *CountedDisk {
		if out.Disks[id] == nil {
			out.Disks[id] = &CountedDisk{}
		}
		return out.Disks[id]
	}
	err := eachRow(ctx, db, "SELECT state, count(*), COALESCE(sum(size), 0) FROM files GROUP BY state", nil, func(key string, n, bytes int64) {
		out.Files[key] = CountedFiles{Files: n, Bytes: bytes}
	})
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	err = eachRow(ctx, db, "SELECT disk_id, count(*), COALESCE(sum(size), 0) FROM copies GROUP BY disk_id", nil, func(id string, n, bytes int64) {
		disk(id).Copies, disk(id).Bytes = n, bytes
	})
	if err != nil {
		return nil, fmt.Errorf("copies: %w", err)
	}
	err = eachRow(ctx, db, `SELECT t.disk_id, count(*), COALESCE(sum(t.size), 0) FROM trash t JOIN ops o ON o.id = t.op_id
		WHERE o.state = 'done' GROUP BY t.disk_id`, nil, func(id string, n, bytes int64) {
		disk(id).TrashEntries, disk(id).TrashBytes = n, bytes
	})
	if err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	// ?1 is the disk in the scrub's own condition, which names none here.
	err = eachRow(ctx, db, `SELECT c.disk_id, count(*), COALESCE(sum(c.size), 0) FROM copies c JOIN files f ON f.id = c.file_id
		WHERE ?1 IS NULL AND `+scrubDueCopy+` GROUP BY c.disk_id`, []any{nil, scrubDueNS}, func(id string, n, bytes int64) {
		disk(id).ScrubDueCopies, disk(id).ScrubDueBytes = n, bytes
	})
	if err != nil {
		return nil, fmt.Errorf("copies due for the scrub: %w", err)
	}
	return out, nil
}

// eachRow reads rows of a key, a count and a sum.
func eachRow(ctx context.Context, db *sql.DB, query string, args []any, each func(key string, n, sum int64)) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var n, sum int64
		if err := rows.Scan(&key, &n, &sum); err != nil {
			return fmt.Errorf("%w", err)
		}
		each(key, n, sum)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
