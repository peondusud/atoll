package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"modernc.org/sqlite"
)

// A catalog whose statements fail at a chosen one, as a catalog whose disk fails would: through a driver that wraps
// SQLite's own and counts each statement, each transaction's begin and each commit. A commit that fails is rolled
// back, so that nothing of the transaction is kept, as SQLite keeps nothing of one it could not commit.

// errCatalogFault is what a statement made to fail gives.
var errCatalogFault = errors.New("injected: disk I/O error")

// catalogFaults counts the statements of one catalog while armed, and makes the one at a position fail.
type catalogFaults struct {
	mu    sync.Mutex
	armed bool
	at    int
	seen  []string
}

func (f *catalogFaults) fail(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed {
		return nil
	}
	f.seen = append(f.seen, what)
	if len(f.seen) == f.at {
		return errCatalogFault
	}
	return nil
}

func (f *catalogFaults) arm(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = on
}

var (
	// faultsByName holds the faults of each catalog a test opened through the driver, by its data source name.
	faultsByName   sync.Map
	registerFaults sync.Once
)

type faultDriver struct{}

func (faultDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	f, ok := faultsByName.Load(name)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("no faults for %q", name)
	}
	return &faultConn{Conn: conn, f: f.(*catalogFaults)}, nil
}

type faultConn struct {
	driver.Conn
	f *catalogFaults
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &faultStmt{Stmt: stmt, query: query, f: c.f}, nil
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.f.fail("BEGIN"); err != nil {
		return nil, err
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &faultTx{Tx: tx, f: c.f}, nil
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.f.fail(query); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args) //nolint:wrapcheck // a test double
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.f.fail(query); err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args) //nolint:wrapcheck // a test double
}

func (c *faultConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx) //nolint:wrapcheck // a test double
	}
	return nil
}

type faultTx struct {
	driver.Tx
	f *catalogFaults
}

func (t *faultTx) Commit() error {
	if err := t.f.fail("COMMIT"); err != nil {
		_ = t.Tx.Rollback()
		return err
	}
	return t.Tx.Commit() //nolint:wrapcheck // a test double
}

type faultStmt struct {
	driver.Stmt
	query string
	f     *catalogFaults
}

func (s *faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := s.f.fail(s.query); err != nil {
		return nil, err
	}
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args) //nolint:wrapcheck // a test double
}

func (s *faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := s.f.fail(s.query); err != nil {
		return nil, err
	}
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args) //nolint:wrapcheck // a test double
}

// faultyCatalog opens the bench's catalog once more, as the catalog opens it, through a driver whose statement at a
// position fails while the faults are armed; 0 fails none.
func (b *bench) faultyCatalog(at int) (*sql.DB, *catalogFaults) {
	b.t.Helper()
	registerFaults.Do(func() { sql.Register("sqlite-faults", faultDriver{}) })
	query := url.Values{}
	for _, pragma := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"} {
		query.Add("_pragma", pragma)
	}
	query.Set("_txlock", "immediate")
	file, _ := b.config.Effective()["catalog"].(string)
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(file), RawQuery: query.Encode()}).String()
	f := &catalogFaults{at: at}
	faultsByName.Store(dsn, f)
	db, err := sql.Open("sqlite-faults", dsn)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() {
		_ = db.Close()
		faultsByName.Delete(dsn)
	})
	return db, f
}

// runWithCatalogFault runs a case's pass with the catalog's statement at a position failing (0 fails none), then passes
// with nothing failing until all is done, and gives the statements, the error of the pass that failed, and what the
// pool ends with.
func runWithCatalogFault(t *testing.T, c *passCase, at int) (seen []string, runErr error, end string) {
	t.Helper()
	b := c.pool(t)
	if c.crash != "" {
		b.crashAt(c.crash)
	}
	db, f := b.faultyCatalog(at)
	f.arm(true)
	runErr = c.run(b, db)
	f.arm(false)
	_ = db.Close()
	f.mu.Lock()
	seen = append(seen, f.seen...)
	f.mu.Unlock()
	return seen, runErr, b.settled(false)
}

// settled runs passes with nothing failing until all is done, and says what the pool ends with: settle passed at each,
// so that what was put back waits for nothing, until a pass finds nothing to do; then a full scan, which reads a copy a
// reprise recorded unverified, as its change time may be a write's (specs.md §14.2), and passes again, as what waited
// on that copy goes then.
func (b *bench) settled(trashLeftOut bool) string {
	b.t.Helper()
	settle := func() {
		for range 4 {
			b.now += settleNS + 60_000_000_000
			if out := b.pass(); len(out.Operations)+len(out.Decisions) == 0 && !out.Partial && out.Waiting == 0 {
				return
			}
		}
	}
	settle()
	b.scan(true)
	settle()
	return b.snapshot(trashLeftOut)
}

// sweepCatalog runs a pass once with nothing failing, counting the catalog's statements, then once for each of
// sweepStride of them on a new pool, with that one failing: the statement, the begin of its transaction or its commit.
// Whatever fails:
//   - the pass ends with the catalog's error, or goes on where the statement is one it does without: an error of the
//     catalog is never taken for an answer of a disk;
//   - once the catalog answers again, passes end where the pass with nothing failing ends: no operation ran on a disk
//     that its journal did not hold first, as a reprise would not find it then, and none that it holds is lost
//     (invariant 12, specs.md §17.2).
func sweepCatalog(t *testing.T, c passCase, stride int) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		t.Parallel()
		seen, err, want := runWithCatalogFault(t, &c, 0)
		if err != nil {
			t.Fatalf("with nothing failing: %v", err)
		}
		if len(seen) == 0 {
			t.Fatal("no statement went through the driver")
		}
		t.Logf("%d statements", len(seen))
		if raceBuild {
			stride *= sweepStride
		}
		for i := 0; i < len(seen); i += stride {
			where := fmt.Sprintf("statement %d of %d (%.80s) failing", i+1, len(seen), seen[i])
			_, err, got := runWithCatalogFault(t, &c, i+1)
			if err != nil && !errors.Is(err, errCatalogFault) {
				t.Errorf("%s: the pass ended with another error: %v", where, err)
			}
			if !endsAsWanted(got, want) {
				t.Errorf("%s: it ends otherwise:\n%s\nwhere nothing failing ends:\n%s", where, got, want)
			}
		}
	})
}

// What a pass does ends as it would with nothing failing, whatever statement of the catalog fails, once the catalog
// answers again (specs.md §9, §17.2).
func TestAPassEndsTheSameWhereverItsCatalogFails(t *testing.T) {
	sweepCatalog(t, passCase{name: "a pass", pool: propagated}, 1)
	sweepCatalog(t, passCase{name: "a refresh and a copy", pool: refreshed}, 1)
	sweepCatalog(t, passCase{name: "the reprise after a crash at moved", pool: propagated, crash: "moved"}, 1)
	sweepCatalog(t, passCase{name: "two names exchanged", pool: swapped}, 1)
	sweepCatalog(t, passCase{name: "a copy's reprise after a crash at placed", pool: refreshed, crash: "placed"}, 1)
	sweepCatalog(t, passCase{name: "a transfer", pool: cached}, 1)
	sweepCatalog(t, passCase{name: "an attribute set through the view", pool: attributed}, 1)
	sweepCatalog(t, passCase{name: "atoll replicate", pool: copiesToMake, act: replicateNow("")}, 1)
}
