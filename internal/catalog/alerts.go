package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// The states of a file that raise an alert (specs.md §23), in the order the alerts name them.
var AlertStates = []string{"unavailable", "corrupt", "conflict", "name-incompatible", "name-collision"}

// AlertRow is a row of the table alerts: a condition sent, when it began, when it was last sent and its count then
// (specs.md §23).
type AlertRow struct {
	Key     string
	FirstNS int64
	SentNS  int64
	Count   int64
}

// ReadAlerts gives the rows of the table alerts, by key.
func ReadAlerts(ctx context.Context, db *sql.DB) ([]AlertRow, error) {
	rows, err := db.QueryContext(ctx, "SELECT key, first_ns, sent_ns, count FROM alerts ORDER BY key")
	if err != nil {
		return nil, fmt.Errorf("alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []AlertRow
	for rows.Next() {
		var r AlertRow
		if err := rows.Scan(&r.Key, &r.FirstNS, &r.SentNS, &r.Count); err != nil {
			return nil, fmt.Errorf("alerts: %w", err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: %w", err)
	}
	return list, nil
}

// WriteAlerts puts rows in the table alerts, each replacing the row of its key, and removes the rows of keys
// dropped, in one transaction.
func WriteAlerts(ctx context.Context, db *sql.DB, put []AlertRow, drop []string) error {
	if len(put) == 0 && len(drop) == 0 {
		return nil
	}
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, r := range put {
			if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO alerts(key, first_ns, sent_ns, count) VALUES (?, ?, ?, ?)",
				r.Key, r.FirstNS, r.SentNS, r.Count); err != nil {
				return fmt.Errorf("alert %q: %w", r.Key, err)
			}
		}
		for _, key := range drop {
			if _, err := tx.ExecContext(ctx, "DELETE FROM alerts WHERE key = ?", key); err != nil {
				return fmt.Errorf("alert %q: %w", key, err)
			}
		}
		return nil
	})
}

// FileAlerts counts the files behind the alerts: those in each state of AlertStates, and those of mode manual left
// unprotected since a time.
type FileAlerts struct {
	ByState map[string]int64
	Manual  int64
}

// CountFileAlerts counts, in one read of the files, those in each state of AlertStates, and those of mode manual
// still unprotected that were first seen at or before seenByNS (specs.md §23).
func CountFileAlerts(ctx context.Context, db *sql.DB, seenByNS int64) (*FileAlerts, error) {
	out := &FileAlerts{ByState: map[string]int64{}}
	rows, err := db.QueryContext(ctx, `
		SELECT state, mode = 'manual' AND state = 'unprotected' AND first_seen_ns <= ?1, count(*) FROM files
		WHERE state IN ('unavailable', 'corrupt', 'conflict', 'name-incompatible', 'name-collision')
			OR (mode = 'manual' AND state = 'unprotected' AND first_seen_ns <= ?1)
		GROUP BY 1, 2`, seenByNS)
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var manual bool
		var n int64
		if err := rows.Scan(&state, &manual, &n); err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
		if manual {
			out.Manual += n
		} else {
			out.ByState[state] += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	return out, nil
}

// FirstStatePaths gives the paths of the first n files in a state, in byte order.
func FirstStatePaths(ctx context.Context, db *sql.DB, state string, n int) ([]string, error) {
	return firstPaths(ctx, db, "SELECT lpath FROM files WHERE state = ? ORDER BY lpath LIMIT ?", state, n)
}

// FirstManualPaths gives the paths of the first n files of mode manual still unprotected that were first seen at or
// before seenByNS, in byte order.
func FirstManualPaths(ctx context.Context, db *sql.DB, seenByNS int64, n int) ([]string, error) {
	return firstPaths(ctx, db, `
		SELECT lpath FROM files WHERE mode = 'manual' AND state = 'unprotected' AND first_seen_ns <= ?
		ORDER BY lpath LIMIT ?`, seenByNS, n)
}

func firstPaths(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
		list = append(list, string(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	return list, nil
}
