package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/jobs"
)

// CatalogWork is what the local API asks the loop to do on the catalog: an answer to a guard (specs.md §25.1). The
// loop runs it between two steps, under the catalog's lock as it runs a pass, so that it waits for a command that holds
// the lock as a command waits for a pass. In the loop, nothing else of the daemon's process takes the lock meanwhile:
// taking it opens and closes a descriptor of the catalog's file, and closing one drops every lock SQLite holds on that
// file in this process, those of a pass that writes included.
type CatalogWork struct {
	// gone and why are the request's Done and Err: work whose request is gone does not run, and the wait for the lock
	// and the work end with it.
	gone <-chan struct{}
	why  func() error
	do   func(ctx context.Context, db *sql.DB) error
	done chan error
}

// ErrParked says that the daemon is parked, as the UPS's battery runs low (specs.md §20): it takes no work until line
// power is back.
var ErrParked = errors.New("the UPS's battery runs low: the daemon does nothing until line power is back")

// Hand gives do to the loop through work, and gives what do returned (specs.md §25.1). It gives up with ctx's error
// when the loop does not take the work before ctx is done. Once the loop took it, it waits for the loop, which ends do
// with ctx: what it gives then says whether do ran to its end.
func Hand(ctx context.Context, work chan<- *CatalogWork, do func(ctx context.Context, db *sql.DB) error) error {
	w := &CatalogWork{gone: ctx.Done(), why: ctx.Err, do: do, done: make(chan error, 1)}
	select {
	case work <- w:
	case <-ctx.Done():
		return fmt.Errorf("waiting for the daemon: %w", ctx.Err())
	}
	return <-w.done
}

// runWork runs work of the local API on the catalog, under its lock, and tells its request what came of it.
func (l *loop) runWork(ctx context.Context, w *CatalogWork) {
	select {
	case <-w.gone:
		w.done <- fmt.Errorf("the request is gone: %w", w.why())
		return
	default:
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-w.gone:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) { return nil, w.do(ctx, db) })
	if gone := w.why(); err != nil && gone != nil {
		err = fmt.Errorf("%w: %w", err, gone)
	}
	w.done <- err
}

// takeWork runs the work the local API handed, if there is any, and says whether there was.
func (l *loop) takeWork(ctx context.Context) bool {
	select {
	case w := <-l.opt.Catalog:
		l.runWork(ctx, w)
		return true
	default:
		return false
	}
}
