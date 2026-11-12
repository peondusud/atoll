package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/vectors"
	"github.com/peondusud/atoll/schema"
)

type catalogVector struct {
	SQL   []string           `json:"sql"`
	Error bool               `json:"error"`
	Query *string            `json:"query"`
	Rows  *[]json.RawMessage `json:"rows"`
	Note  string             `json:"note"`
}

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// rowsJSON reads a query's rows as JSON arrays, as the vectors write them.
func rowsJSON(ctx context.Context, tx *sql.Tx, query string) (string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	all := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return "", err
		}
		all = append(all, values)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(all)
	return string(raw), err
}

// Each case runs on the fixture, in a transaction that is rolled back.
func TestCatalogVectors(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, string(vectors.RepoFile(t, "conformance/vectors/catalog_fixture.sql"))); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for i, v := range vectors.Load[catalogVector](t, "catalog.json") {
		if len(v.SQL) == 0 && v.Query == nil || v.Error && v.Query != nil || (v.Query == nil) != (v.Rows == nil) {
			t.Errorf("vector %d (%s): want sql, with error or with query and rows", i, v.Note)
			continue
		}
		runCatalogVector(t, ctx, db, i, v)
	}
}

func runCatalogVector(t testing.TB, ctx context.Context, db *sql.DB, i int, v catalogVector) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}()
	for j, statement := range v.SQL {
		_, err := tx.ExecContext(ctx, statement)
		mustFail := v.Error && j == len(v.SQL)-1
		switch {
		case err != nil && !mustFail:
			t.Errorf("vector %d (%s): statement %d: %v", i, v.Note, j, err)
			return
		case err == nil && mustFail:
			t.Errorf("vector %d (%s): the last statement was accepted, want an error", i, v.Note)
			return
		}
	}
	if v.Query == nil {
		return
	}
	got, err := rowsJSON(ctx, tx, *v.Query)
	if err != nil {
		t.Errorf("vector %d (%s): query: %v", i, v.Note, err)
		return
	}
	raw, err := json.Marshal(*v.Rows)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(raw); got != want {
		t.Errorf("vector %d (%s): rows %s, want %s", i, v.Note, got, want)
	}
}

func TestOpenSetsThePragmas(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	// On more than one connection: each must have them.
	conns := make([]*sql.Conn, 3)
	for i := range conns {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		conns[i] = conn
	}
	for i, conn := range conns {
		var journal string
		var synchronous, foreignKeys int
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if journal != "wal" || synchronous != 2 || foreignKeys != 1 {
			t.Errorf("connection %d: journal_mode %s, synchronous %d, foreign_keys %d", i, journal, synchronous, foreignKeys)
		}
	}
}

func TestOpenAPathWithSpecialCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "un dossier ?#%é", "catalog.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
}

func TestOpenRefusesARelativePath(t *testing.T) {
	// SQLite would refuse it too, for a reason no one could act on.
	db, err := Open(t.Context(), "catalog.db")
	if err == nil {
		_ = db.Close()
		t.Fatal("a relative path was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("got %v", err)
	}
}

func TestMigrationsLeaveAValidCatalog(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	list, err := schema.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if want := list[len(list)-1].Version; version != want {
		t.Errorf("user_version %d, want %d", version, want)
	}
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatal(err)
	}
	if check != "ok" {
		t.Errorf("integrity_check: %s", check)
	}
	rows, err := db.QueryContext(ctx, "SELECT name, strict FROM pragma_table_list WHERE schema = 'main' AND type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	tables := 0
	for rows.Next() {
		var name string
		var strict int
		if err := rows.Scan(&name, &strict); err != nil {
			t.Fatal(err)
		}
		tables++
		if strict != 1 {
			t.Errorf("table %s is not STRICT", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if tables != 12 {
		t.Errorf("%d tables, want 12", tables)
	}
}

func TestOpeningTwiceChangesNothing(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO meta VALUES ('pool_id', 'p')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'pool_id'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "p" {
		t.Errorf("pool_id %q", value)
	}
}

// Several processes may start at once: each waits for the lock, and one migrates.
func TestOpeningConcurrently(t *testing.T) {
	ctx := t.Context()
	// Several rounds, as the race on turning a new file to WAL does not show every time.
	for round := range 15 {
		path := filepath.Join(t.TempDir(), "catalog.db")
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Go(func() {
				db, err := Open(ctx, path)
				if err != nil {
					errs[i] = err
					return
				}
				errs[i] = db.Close()
			})
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d, process %d: %v", round, i, err)
			}
		}
	}
}

// busy_timeout: a writer waits for another, instead of failing at once.
func TestAWriterWaitsForAnother(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "catalog.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	tx, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta VALUES ('a', '1')"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		_, err := second.ExecContext(ctx, "INSERT INTO meta VALUES ('b', '2')")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("the second writer did not wait: %v", err)
	}
}

// _txlock=immediate: a transaction that reads and then writes takes the write lock from the
// start. Otherwise it fails with SQLITE_BUSY on its first write, once another has committed
// since it read, whatever busy_timeout says.
func TestReadThenWriteTransactions(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, "INSERT INTO meta VALUES ('n', '0')"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Go(func() {
			errs[i] = readThenWrite(ctx, db)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("transaction %d: %v", i, err)
		}
	}
	var n string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'n'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != "16" {
		t.Errorf("n = %s, want 16: a write was lost", n)
	}
}

func readThenWrite(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'n'").Scan(&n); err != nil {
		return err
	}
	time.Sleep(time.Millisecond)
	if _, err := tx.ExecContext(ctx, "UPDATE meta SET value = ? WHERE key = 'n'", strconv.Itoa(n+1)); err != nil {
		return err
	}
	return tx.Commit()
}

// A connection without the pragmas of specs.md §9 is refused, as on a filesystem that cannot
// do WAL. Opened here without them, as nothing else can make them fail.
func TestPrepareChecksThePragmas(t *testing.T) {
	for _, c := range []struct {
		note    string
		pragmas []string
	}{
		{"no WAL", []string{"synchronous(FULL)", "foreign_keys(ON)"}},
		{"synchronous NORMAL", []string{"journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(ON)"}},
		{"no foreign keys", []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(OFF)"}},
	} {
		query := url.Values{}
		for _, p := range c.pragmas {
			query.Add("_pragma", p)
		}
		dsn := (&url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "c.db"), RawQuery: query.Encode()}).String()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepare(t.Context(), db); err == nil {
			t.Errorf("%s: accepted", c.note)
		}
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestANewerCatalogIsRefused(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(ctx, path); err == nil {
		_ = db.Close()
		t.Error("a catalog from a newer version was accepted")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("got %v", err)
	}
}

// A failure leaves the catalog at the version before it, with nothing of the migration in it.
func TestAFailedMigrationLeavesNothing(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	var before int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	next := before + 1
	bad := []schema.Migration{
		{Version: next, Name: "bad", SQL: "CREATE TABLE half_done(a INTEGER) STRICT; CREATE TABLE half_done(a INTEGER) STRICT;"},
	}
	if err := migrate(ctx, db, bad); err == nil {
		t.Fatal("a bad migration was accepted")
	}
	var after, tables int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("user_version %d, want %d", after, before)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name = 'half_done'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Error("half of a failed migration stayed")
	}
	good := []schema.Migration{{Version: next, Name: "good", SQL: "CREATE TABLE whole(a INTEGER) STRICT;"}}
	if err := migrate(ctx, db, good); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != next {
		t.Errorf("user_version %d, want %d", after, next)
	}
	// Applying the same list again does nothing: the table exists, so it would fail.
	if err := migrate(ctx, db, good); err != nil {
		t.Errorf("a migration ran twice: %v", err)
	}
}

// What schema/README.md asks of a migration file.
func TestMigrationFileRules(t *testing.T) {
	list, err := schema.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	forbidden := regexp.MustCompile(`(?im)^\s*(PRAGMA|BEGIN|COMMIT|ROLLBACK|SAVEPOINT|ATTACH)\b`)
	for _, m := range list {
		if !utf8.ValidString(m.SQL) {
			t.Errorf("%s is not valid UTF-8", m.Name)
		}
		if strings.Contains(m.SQL, "\r") {
			t.Errorf("%s holds a CR", m.Name)
		}
		if found := forbidden.FindString(m.SQL); found != "" {
			t.Errorf("%s holds %q: whoever applies the file does that", m.Name, strings.TrimSpace(found))
		}
	}
}
