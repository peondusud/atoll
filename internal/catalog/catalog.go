package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"modernc.org/sqlite" // the driver: no cgo (CLAUDE.md)

	"github.com/peondusud/atoll/schema"
)

// Open opens the catalog at an absolute path, creating it if needed, and brings its schema up
// to date. Every connection has the pragmas of specs.md §9, which Open checks: a filesystem
// that cannot do WAL must fail here, not lose a journal entry later. A catalog from a newer
// version is refused.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	db, err := connect(path)
	if err != nil {
		return nil, err
	}
	if err := prepare(ctx, db); err != nil {
		return nil, errors.Join(fmt.Errorf("catalog %q: %w", path, err), db.Close())
	}
	return db, nil
}

func connect(path string) (*sql.DB, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("catalog %q: want an absolute path", path)
	}
	query := url.Values{}
	for _, pragma := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"} {
		query.Add("_pragma", pragma)
	}
	// A transaction takes the write lock when it begins, so two writers wait for each other
	// rather than fail halfway when one tries to upgrade its lock.
	query.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog %q: %w", path, err)
	}
	return db, nil
}

// prepare checks the pragmas and applies the migrations. busy_timeout does not cover turning a
// new file to WAL, measured with several processes opening one at once, so a busy catalog is
// tried again here for up to about 5 s.
func prepare(ctx context.Context, db *sql.DB) error {
	list, err := schema.Migrations()
	if err != nil {
		return err
	}
	const sqliteBusy = 5
	for attempt := 0; ; attempt++ {
		err := checkPragmas(ctx, db)
		if err == nil {
			err = migrate(ctx, db, list)
		}
		var failure *sqlite.Error
		if err == nil || !errors.As(err, &failure) || failure.Code()&0xff != sqliteBusy || attempt == 100 {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for a busy catalog: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func checkPragmas(ctx context.Context, db *sql.DB) error {
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("journal_mode is %q, want wal", journal)
	}
	var synchronous, foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		return fmt.Errorf("synchronous: %w", err)
	}
	if synchronous != 2 {
		return fmt.Errorf("synchronous is %d, want 2 (FULL)", synchronous)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("foreign_keys is %d, want 1", foreignKeys)
	}
	return nil
}

// migrate applies the migrations a catalog does not have yet, each in its own transaction with
// its user_version, so a failure leaves the catalog at the version before it.
func migrate(ctx context.Context, db *sql.DB, list []schema.Migration) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("user_version: %w", err)
	}
	latest := 0
	if len(list) > 0 {
		latest = list[len(list)-1].Version
	}
	if current > latest {
		return fmt.Errorf("schema version %d, newer than this version of Atoll (%d)", current, latest)
	}
	for _, m := range list {
		if m.Version > current {
			if err := apply(ctx, db, m); err != nil {
				return fmt.Errorf("migration %s: %w", m.Name, err)
			}
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, m schema.Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // after a commit there is nothing to roll back

	// Another process may have applied it while this one waited for the lock.
	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("user_version: %w", err)
	}
	if current >= m.Version {
		return nil
	}
	// Foreign keys are checked at the commit, so that a table made again keeps what refers to it (specs.md §9). The
	// commit turns it off again.
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("defer_foreign_keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	// A pragma takes no parameter. The version is an integer from a file name checked by
	// schema.Migrations.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
		return fmt.Errorf("user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
