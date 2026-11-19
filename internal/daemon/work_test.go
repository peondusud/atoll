package daemon

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
)

// runWithWork runs the daemon until the test ends, with work for the local API's answers to a guard, and gives its
// passes.
func runWithWork(t *testing.T, opt Options, work chan *CatalogWork) <-chan []string {
	t.Helper()
	passes := make(chan []string, 64)
	opt.Log, opt.Catalog = slog.New(slog.DiscardHandler), work
	opt.OnPass = func(because []string, _ *jobs.Outcome) { passes <- because }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opt) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the daemon stopped: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	select {
	case <-passes:
	case <-time.After(30 * time.Second):
		t.Fatal("no first pass")
	}
	return passes
}

// The local API's answer to a guard runs in the daemon's loop, on its catalog, under the catalog's lock as a pass
// does: it waits for a command that holds the lock, then runs, and what it returns comes back. Work whose request is
// gone before it runs never runs (specs.md §25.1).
func TestTheLoopRunsTheAPIsWorkUnderTheLock(t *testing.T) {
	c := bench(t, "")
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	work := make(chan *CatalogWork)
	runWithWork(t, Options{Config: c, Machine: machine, Apply: true, CheckEvery: 50 * time.Millisecond}, work)
	file, _ := c.Effective()["catalog"].(string)
	release, err := fsx.WaitLockFile(t.Context(), file, nil)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	result := make(chan error, 1)
	mine := errors.New("what the work returned")
	go func() {
		result <- Hand(t.Context(), work, func(ctx context.Context, db *sql.DB) error {
			close(ran)
			var pool string
			if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'pool_id'").Scan(&pool); err != nil || pool != "p" {
				t.Errorf("the work's catalog: %q, %v", pool, err)
			}
			if _, err := fsx.LockFile(file); !errors.Is(err, fsx.ErrBusy) {
				t.Errorf("the work runs without the catalog's lock: %v", err)
			}
			return mine
		})
	}()
	select {
	case <-ran:
		t.Fatal("the work ran while a command held the catalog's lock")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case err := <-result:
		if !errors.Is(err, mine) {
			t.Errorf("the work gave %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the work did not run once the lock was free")
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	err = Hand(gone, work, func(context.Context, *sql.DB) error {
		t.Error("work whose request was gone ran")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("work whose request was gone: %v", err)
	}
	// The loop goes on.
	if err := Hand(t.Context(), work, func(context.Context, *sql.DB) error { return nil }); err != nil {
		t.Errorf("then: %v", err)
	}
}

// A daemon parked as the UPS's battery runs low takes no work: it says so at once, and takes work again once line
// power is back (specs.md §20, §25.1).
func TestAParkedDaemonTakesNoWork(t *testing.T) {
	u := newFakeUPS(t, "OL")
	c, m, _ := upsBench(t, u.config())
	work := make(chan *CatalogWork)
	runWithWork(t, Options{Config: c, Machine: m, Apply: true, CheckEvery: 100 * time.Millisecond, Inotify: true,
		UPSEvery: 50 * time.Millisecond, UPSDead: 300 * time.Millisecond, FlushWait: 5 * time.Second, Load: func() (*config.Config, error) { return c, nil }}, work)
	u.set("OB DISCHRG LB")
	eventually(t, "the disks were not written out", func() bool { return m.times(c.Disks[0].Mount) == 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := Hand(ctx, work, func(context.Context, *sql.DB) error {
		t.Error("a parked daemon ran work")
		return nil
	})
	if !errors.Is(err, ErrParked) {
		t.Errorf("a parked daemon: %v", err)
	}
	u.set("OL CHRG LB")
	eventually(t, "no work once line power is back", func() bool {
		ran := false
		err := Hand(t.Context(), work, func(context.Context, *sql.DB) error { ran = true; return nil })
		if err != nil && !errors.Is(err, ErrParked) {
			t.Fatalf("once line power is back: %v", err)
		}
		return ran
	})
}
