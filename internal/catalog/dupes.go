package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
)

// DupeGroup is two or more files that hold the same content: one size, one hash, each its own logical path
// (specs.md §16).
type DupeGroup struct {
	Size  int64
	Hash  []byte
	Paths []string // in byte order
}

// dupesQuery gives the files of a size and a hash two files or more hold, of at least ?1 bytes and never empty, by
// size, hash and path.
const dupesQuery = `
	SELECT f.size, f.blake3, f.lpath FROM files f JOIN (
		SELECT size, blake3 FROM files WHERE blake3 IS NOT NULL AND size >= ?1 AND size > 0
		GROUP BY size, blake3 HAVING count(*) > 1
	) g ON g.size = f.size AND g.blake3 = f.blake3
	ORDER BY f.size, f.blake3, f.lpath`

// Dupes gives the groups of files that hold the same content, of at least minSize bytes, never empty: the catalog
// alone says so, and no disk is read (specs.md §16). Only what is in a group is held in memory.
func Dupes(ctx context.Context, db *sql.DB, minSize int64) ([]DupeGroup, error) {
	rows, err := db.QueryContext(ctx, dupesQuery, minSize)
	if err != nil {
		return nil, fmt.Errorf("dupes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var groups []DupeGroup
	for rows.Next() {
		var size int64
		var hash, path []byte
		if err := rows.Scan(&size, &hash, &path); err != nil {
			return nil, fmt.Errorf("dupes: %w", err)
		}
		if n := len(groups); n == 0 || groups[n-1].Size != size || !bytes.Equal(groups[n-1].Hash, hash) {
			groups = append(groups, DupeGroup{Size: size, Hash: hash})
		}
		groups[len(groups)-1].Paths = append(groups[len(groups)-1].Paths, string(path))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dupes: %w", err)
	}
	return groups, nil
}

// Unhashed counts the files at a path and below it, by whole components, that hold something and have no hash yet:
// what a report of duplicates cannot place.
func Unhashed(ctx context.Context, db *sql.DB, under string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE blake3 IS NULL AND size > 0
		AND (?1 = x'' OR lpath = ?1 OR substr(lpath, 1, length(?2)) = ?2)`, []byte(under), []byte(under+"/")).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("unhashed: %w", err)
	}
	return n, nil
}
