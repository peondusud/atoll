package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// StartOpsAfter makes the next operation's id come after id, at the least: a rebuilt catalog's operations must not
// meet the folders and parts the old ones left on the disks (specs.md §9, §17).
func StartOpsAfter(ctx context.Context, db *sql.DB, id int64) error {
	return startAfter(ctx, db, "ops", id)
}

// StartFilesAfter makes the next file's id come after id, at the least: a link of the guard an old catalog left on a
// disk must not name a new file (specs.md §9, §17).
func StartFilesAfter(ctx context.Context, db *sql.DB, id int64) error {
	return startAfter(ctx, db, "files", id)
}

// startAfter moves the counter of a table's ids past id, never back.
func startAfter(ctx context.Context, db *sql.DB, table string, id int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		var seq sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&seq)
		switch {
		case isNoRows(err):
			_, err = tx.ExecContext(ctx, "INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)", table, id)
		case err != nil:
		case seq.Int64 < id:
			_, err = tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq = ? WHERE name = ?", id, table)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		return nil
	})
}

// Checkpoint moves what a catalog's write-ahead log holds into the catalog itself, and empties the log, without
// reading the catalog's schema: a catalog this version refuses, of a later one or damaged, is checkpointed too, when
// SQLite can. The log then holds nothing a rebuild could set aside apart from it.
func Checkpoint(ctx context.Context, file string) (err error) {
	dsn := (&url.URL{Scheme: "file", Path: file}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("catalog %s: %w", file, err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var busy, logged, moved int64
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &moved); err != nil {
		return fmt.Errorf("catalog %s: %w", file, err)
	}
	if busy != 0 {
		return fmt.Errorf("catalog %s: the checkpoint was kept from finishing", file)
	}
	return nil
}

// SetAside puts a rebuilt catalog, built at fresh, in place of the one at file, which goes to the name it gives,
// <file>.old-<UTC time>, with its write-ahead log and shared memory beside it. The swap leaves no moment with no
// catalog at file, so that a command that waits for the lock finds one, and locks it (specs.md §9). With no catalog
// at file, fresh is only renamed, and the name given is "".
func SetAside(file, fresh string, nowNS int64) (string, error) {
	there, err := fsx.Exists(file)
	if err != nil {
		return "", fmt.Errorf("catalog %s: %w", file, err)
	}
	if !there {
		if err := fsx.RenameFile(fresh, file); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return "", nil
	}
	old := file + ".old-" + time.Unix(0, nowNS).UTC().Format("20060102T150405Z")
	// The log goes first: left beside the new catalog, SQLite would play the old one's frames into it.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := fsx.RenameFile(file+suffix, old+suffix); err != nil && !fsx.IsAbsent(err) {
			return "", fmt.Errorf("catalog: %w", err)
		}
	}
	if err := fsx.ExchangeFiles(fresh, file); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	if err := fsx.RenameFile(fresh, old); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return old, nil
}
