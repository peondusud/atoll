package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// BackupKeep is how many backups the catalog's backups folder keeps (specs.md §9).
const BackupKeep = 7

// BackupFolder is the folder of the backups, beside the catalog.
const BackupFolder = "backups"

// backupPrefix and backupSuffix name a backup: catalog-<UTC time>.db, which sorts by time.
const (
	backupPrefix = "catalog-"
	backupSuffix = ".db"
	// backupPart ends the name a backup is written under until it is checked.
	backupPart = ".part"
)

// BackupName gives the name of a backup made at a time.
func BackupName(nowNS int64) string {
	return backupPrefix + time.Unix(0, nowNS).UTC().Format("20060102T150405Z") + backupSuffix
}

// Backup writes a consistent copy of the catalog in the folder backups beside it, under the name of that time:
// through <name>.part, which SQLite's VACUUM INTO fills from one snapshot, checked whole, then renamed. It then
// removes the oldest backups past BackupKeep. It gives the path written and the names removed (specs.md §9). A part
// goes with the backup that fails, and one an earlier backup left, as a crash leaves it, goes first: the system disk
// that had no room for one must not keep what was written of it.
func Backup(ctx context.Context, db *sql.DB, catalogFile, poolID string, nowNS int64) (written string, removed []string, err error) {
	dir := path.Join(path.Dir(catalogFile), BackupFolder)
	if err := fsx.MkdirAll(dir); err != nil {
		return "", nil, fmt.Errorf("backup: %w", err)
	}
	if err := removeParts(dir); err != nil {
		return "", nil, err
	}
	written = path.Join(dir, BackupName(nowNS))
	part := written + backupPart
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", part); err != nil {
		_ = fsx.RemoveFile(part)
		return "", nil, fmt.Errorf("backup %s: %w", part, err)
	}
	if err := CheckBackup(ctx, part, poolID); err != nil {
		_ = fsx.RemoveFile(part)
		return "", nil, err
	}
	if err := fsx.RenameFile(part, written); err != nil {
		_ = fsx.RemoveFile(part)
		return "", nil, fmt.Errorf("backup: %w", err)
	}
	names, err := fsx.ListNames(dir)
	if err != nil {
		return written, nil, fmt.Errorf("backup: %w", err)
	}
	var backups []string
	for _, n := range names {
		if strings.HasPrefix(n, backupPrefix) && strings.HasSuffix(n, backupSuffix) {
			backups = append(backups, n)
		}
	}
	slices.Sort(backups)
	for len(backups) > BackupKeep {
		if err := fsx.RemoveFile(path.Join(dir, backups[0])); err != nil && !fsx.IsAbsent(err) {
			return written, removed, fmt.Errorf("backup: %w", err)
		}
		removed = append(removed, backups[0])
		backups = backups[1:]
	}
	return written, removed, nil
}

// removeParts removes what backups that did not end left in their folder: catalog-<time>.db.part, and nothing else,
// as the folder is not Atoll's alone.
func removeParts(dir string) error {
	names, err := fsx.ListNames(dir)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, backupPrefix) || !strings.HasSuffix(n, backupSuffix+backupPart) {
			continue
		}
		if err := fsx.RemoveFile(path.Join(dir, n)); err != nil && !fsx.IsAbsent(err) {
			return fmt.Errorf("backup: %w", err)
		}
	}
	return nil
}

// CheckBackup opens a catalog file read-only, and checks it whole: SQLite's integrity check, its foreign keys, and
// that it is this pool's.
func CheckBackup(ctx context.Context, file, poolID string) (err error) {
	dsn := (&url.URL{Scheme: "file", Path: file, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("backup %s: %w", file, err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	problems, err := Integrity(ctx, db)
	if err != nil {
		return fmt.Errorf("backup %s: %w", file, err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("backup %s: %s", file, strings.Join(problems, "; "))
	}
	var pool string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'pool_id'").Scan(&pool); err != nil {
		return fmt.Errorf("backup %s: %w", file, err)
	}
	if pool != poolID {
		return fmt.Errorf("backup %s: of the pool %q", file, pool)
	}
	return nil
}

// Integrity runs SQLite's integrity check and foreign key check, and gives what they say is wrong: nothing for a
// catalog in order.
func Integrity(ctx context.Context, db *sql.DB) ([]string, error) {
	var problems []string
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("integrity check: %w", err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	keys, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("foreign key check: %w", err)
	}
	defer func() { _ = keys.Close() }()
	for keys.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fk int64
		if err := keys.Scan(&table, &rowid, &parent, &fk); err != nil {
			return nil, fmt.Errorf("foreign key check: %w", err)
		}
		problems = append(problems, fmt.Sprintf("%s row %d: no row in %s", table, rowid.Int64, parent))
	}
	if err := keys.Err(); err != nil {
		return nil, fmt.Errorf("foreign key check: %w", err)
	}
	return problems, nil
}
