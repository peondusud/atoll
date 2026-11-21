package catalog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// errInjected is the failure a faultyDB gives at the step it was told to fail.
var errInjected = errors.New("injected fault")

// faults counts what a catalog asks of SQLite, and fails one step of it: a transaction begun, a statement prepared,
// run or queried, a row read, a commit. Once it failed, the steps after it work, as after a fault the disk or the
// lock let go: what follows is the code's own handling of that one failure.
type faults struct {
	mu    sync.Mutex
	left  int // steps before the one that fails; -1 for none
	fired bool
}

func (f *faults) step() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.left < 0:
		return nil
	case f.left == 0:
		f.left, f.fired = -1, true
		return errInjected
	}
	f.left--
	return nil
}

// failAt makes the step n, from 0, fail; failAt(-1) fails none.
func (f *faults) failAt(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left, f.fired = n, false
}

func (f *faults) hasFired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

// faultConnector opens SQLite connections, with the pragmas of Open, whose every step goes through faults.
type faultConnector struct {
	dsn string
	drv driver.Driver
	f   *faults
}

func (c *faultConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &faultConn{conn: conn, f: c.f}, nil
}

func (c *faultConnector) Driver() driver.Driver { return c.drv }

type faultConn struct {
	conn driver.Conn
	f    *faults
}

func (c *faultConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *faultConn) Close() error { return c.conn.Close() }

func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.f.step(); err != nil {
		return nil, err
	}
	tx, err := c.conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{tx: tx, f: c.f}, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.f.step(); err != nil {
		return nil, err
	}
	stmt, err := c.conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &faultStmt{stmt: stmt, f: c.f}, nil
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.f.step(); err != nil {
		return nil, err
	}
	return c.conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.f.step(); err != nil {
		return nil, err
	}
	rows, err := c.conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{rows: rows, f: c.f}, nil
}

func (c *faultConn) ResetSession(ctx context.Context) error {
	if r, ok := c.conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *faultConn) IsValid() bool {
	if v, ok := c.conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

type faultTx struct {
	tx driver.Tx
	f  *faults
}

// Commit, when it fails, leaves nothing of the transaction, as a commit SQLite could not write does.
func (t *faultTx) Commit() error {
	if err := t.f.step(); err != nil {
		_ = t.tx.Rollback()
		return err
	}
	return t.tx.Commit()
}

func (t *faultTx) Rollback() error { return t.tx.Rollback() }

type faultStmt struct {
	stmt driver.Stmt
	f    *faults
}

func (s *faultStmt) Close() error  { return s.stmt.Close() }
func (s *faultStmt) NumInput() int { return s.stmt.NumInput() }

func (s *faultStmt) Exec(args []driver.Value) (driver.Result, error) { //nolint:staticcheck // driver.Stmt asks for it
	if err := s.f.step(); err != nil {
		return nil, err
	}
	return s.stmt.Exec(args) //nolint:staticcheck // as above
}

func (s *faultStmt) Query(args []driver.Value) (driver.Rows, error) { //nolint:staticcheck // driver.Stmt asks for it
	if err := s.f.step(); err != nil {
		return nil, err
	}
	rows, err := s.stmt.Query(args) //nolint:staticcheck // as above
	if err != nil {
		return nil, err
	}
	return &faultRows{rows: rows, f: s.f}, nil
}

func (s *faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := s.f.step(); err != nil {
		return nil, err
	}
	return s.stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s *faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := s.f.step(); err != nil {
		return nil, err
	}
	rows, err := s.stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{rows: rows, f: s.f}, nil
}

type faultRows struct {
	rows driver.Rows
	f    *faults
}

func (r *faultRows) Columns() []string { return r.rows.Columns() }
func (r *faultRows) Close() error      { return r.rows.Close() }

// Next fails as a read of a page of the catalog that the disk refused, between two rows or before the first.
func (r *faultRows) Next(dest []driver.Value) error {
	if err := r.f.step(); err != nil {
		return err
	}
	return r.rows.Next(dest)
}

// openFaulty opens a catalog file with the pragmas of Open, through faults.
func openFaulty(t *testing.T, file string, f *faults) *sql.DB {
	t.Helper()
	plain, err := sql.Open("sqlite", "") // opens no connection: only for its driver
	if err != nil {
		t.Fatal(err)
	}
	drv := plain.Driver()
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	query := url.Values{}
	for _, pragma := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"} {
		query.Add("_pragma", pragma)
	}
	query.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: file, RawQuery: query.Encode()}).String()
	return sql.OpenDB(&faultConnector{dsn: dsn, drv: drv, f: f})
}

// snapshot is every row of every table of a catalog, sorted: two snapshots are equal when nothing was written.
func snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var tables string
	if err := db.QueryRowContext(t.Context(), "SELECT group_concat(name, ' ') FROM (SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name)").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, name := range strings.Fields(tables) {
		lines := rowLines(t, db, `SELECT * FROM "`+name+`"`)
		fmt.Fprintf(&out, "%s:\n  %s\n", name, strings.Join(lines, "\n  "))
	}
	return out.String()
}

// rowLines gives each row of a query as one line, sorted.
func rowLines(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%q", values))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	return lines
}

// faultBench is a pool's catalog in a known state, the template each step of a sweep starts from.
type faultBench struct {
	t        *testing.T
	template string
	file     string
	f        *faults
	db       *sql.DB
	before   string
	dirty    bool
}

// newFaultBench makes a pool's catalog, fills it with setup through a plain handle, and keeps it as the template.
func newFaultBench(t *testing.T, setup func(b *testDB)) *faultBench {
	t.Helper()
	dir := t.TempDir()
	template := filepath.Join(dir, "template.db")
	if _, err := Create(t.Context(), template, "p"); err != nil {
		t.Fatal(err)
	}
	plain, err := OpenPool(t.Context(), template, "p")
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(&testDB{t: t, db: plain})
	}
	// Closing the last connection checkpoints the log into the file, which the steps copy.
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	fb := &faultBench{t: t, template: template, file: filepath.Join(dir, "catalog.db"), f: &faults{left: -1}, dirty: true}
	t.Cleanup(fb.close)
	return fb
}

func (fb *faultBench) close() {
	if fb.db != nil {
		_ = fb.db.Close()
		fb.db = nil
	}
}

// reset puts the template back in place, when the last step wrote anything.
func (fb *faultBench) reset() {
	t := fb.t
	t.Helper()
	if !fb.dirty {
		return
	}
	fb.close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(fb.file + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(fb.template)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fb.file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fb.db = openFaulty(t, fb.file, fb.f)
	if fb.before == "" {
		fb.before = snapshot(t, fb.db)
	}
	fb.dirty = false
}

// sweepOptions say what a sweep may see besides an error that leaves the catalog as it was.
type sweepOptions struct {
	// partial: the function writes in more than one transaction, or statement by statement, and what a failure
	// leaves is checked by the case itself, if at all.
	partial bool
	// keeps: a value the function gives with its error, rather than none.
	keeps bool
}

// sweep runs fn once for each step it takes on the template, that step failing, then once with no failure. Each
// failure must come back as an error that wraps it, with no result, and leave the catalog as it was: a reader that
// fails gives nothing that could be taken for an answer (an empty list, no copy, no job), and a writer that fails
// leaves no half of what it writes, so that no operation is journaled without its event, nor a row changed without
// the rest (specs.md §9, CLAUDE.md invariant 12). It gives the number of steps, and the last result.
func (fb *faultBench) sweep(name string, opt sweepOptions, fn func(ctx context.Context, db *sql.DB) (any, error)) (int, any) {
	t := fb.t
	t.Helper()
	ctx := t.Context()
	for n := 0; n < 10000; n++ {
		fb.reset()
		fb.f.failAt(n)
		got, err := fn(ctx, fb.db)
		fired := fb.f.hasFired()
		fb.f.failAt(-1)
		if !fired {
			if err != nil {
				t.Errorf("%s: with no fault: %v", name, err)
			}
			fb.dirty = true
			return n, got
		}
		switch {
		case err == nil:
			t.Errorf("%s: step %d failed, and the function said nothing", name, n)
		case !errors.Is(err, errInjected):
			t.Errorf("%s: step %d: %v, which does not wrap the failure", name, n, err)
		}
		if err != nil && !opt.keeps && got != nil && !reflect.ValueOf(got).IsZero() {
			t.Errorf("%s: step %d failed, and it gave %#v with its error", name, n, got)
		}
		if after := snapshot(t, fb.db); after != fb.before {
			fb.dirty = true
			if !opt.partial {
				t.Errorf("%s: step %d failed, and the catalog changed:\n%s", name, n, diffLines(fb.before, after))
			}
		}
	}
	t.Fatalf("%s: no end to its steps", name)
	return 0, nil
}

// diffLines gives the lines of two snapshots that only one of them has.
func diffLines(before, after string) string {
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	var out []string
	for _, line := range b {
		if !slices.Contains(a, line) {
			out = append(out, "- "+line)
		}
	}
	for _, line := range a {
		if !slices.Contains(b, line) {
			out = append(out, "+ "+line)
		}
	}
	return strings.Join(out, "\n")
}

// The bench itself: each kind of step fails once, and a sweep sees every one of them.
func TestFaultsReachEveryStep(t *testing.T) {
	fb := newFaultBench(t, func(b *testDB) {
		b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES ('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online')`)
	})
	steps, got := fb.sweep("a transaction", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
		var ids []string
		err := inTransaction(ctx, db, func(tx *sql.Tx) error {
			stmt, err := tx.PrepareContext(ctx, "UPDATE disks SET free_bytes = 1 WHERE id = ?")
			if err != nil {
				return err
			}
			defer func() { _ = stmt.Close() }()
			if _, err := stmt.ExecContext(ctx, "u1"); err != nil {
				return err
			}
			ids, err = queryAll(ctx, tx, scanBytes, "SELECT CAST(id AS BLOB) FROM disks")
			return err
		})
		if err != nil {
			return nil, err
		}
		return ids, nil
	})
	// Begin, prepare, exec, query, the row, the end of the rows, commit.
	if steps != 7 || !reflect.DeepEqual(got, []string{"u1"}) {
		t.Errorf("%d steps, %v", steps, got)
	}
}
