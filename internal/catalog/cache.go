package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// CacheCopy is a copy on a cache disk, with what the SSD cache asks of its file (specs.md §19).
type CacheCopy struct {
	FileID int64
	Path   string
	Size   int64 // the file's
	Disk   string
	State  string
	// CtimeNS is the copy's change time as last recorded, which its residency counts from; FirstSeenNS stands in for it
	// when it has none.
	CtimeNS     sql.NullInt64
	FirstSeenNS int64
}

// CacheCopies gives every copy on a cache disk, by path in byte order, then by disk position. A cache holds few files,
// next to the pool: all of them at once is no weight (specs.md §26).
func CacheCopies(ctx context.Context, db *sql.DB) ([]CacheCopy, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.id, f.lpath, f.size, c.disk_id, c.state, c.ctime_ns, c.first_seen_ns
		FROM copies c JOIN files f ON f.id = c.file_id JOIN disks d ON d.id = c.disk_id
		WHERE d.role = 'cache'
		ORDER BY f.lpath, d.position`)
	if err != nil {
		return nil, fmt.Errorf("cache copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CacheCopy
	for rows.Next() {
		var c CacheCopy
		var path []byte
		if err := rows.Scan(&c.FileID, &path, &c.Size, &c.Disk, &c.State, &c.CtimeNS, &c.FirstSeenNS); err != nil {
			return nil, fmt.Errorf("cache copies: %w", err)
		}
		c.Path = string(path)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache copies: %w", err)
	}
	return out, nil
}
