package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// AllPaths gives every logical path the catalog knows, of a file or of an entry, each once, in byte order: the
// names atoll names check reads (specs.md §15.9).
func AllPaths(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT lpath FROM files UNION SELECT lpath FROM entries WHERE length(lpath) > 0 ORDER BY 1")
	if err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("paths: %w", err)
		}
		list = append(list, string(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	return list, nil
}
