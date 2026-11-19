package telemetry

import (
	"context"
	"errors"
	"sync"

	"github.com/peondusud/atoll/internal/config"
)

// Manager runs what [telemetry] asks of a process that runs for good, and changes it when a configuration read again
// asks otherwise (specs.md §15.13, §23.2): the format of the log at once, and the export started again. What it stops
// sends what is left in the background, within stopWithin, so that a reload never waits on the collector.
type Manager struct {
	opt Options

	mu       sync.Mutex
	current  *Export
	stopping sync.WaitGroup
}

// NewManager gives a manager of the export for a process: opt.Logs must be its handler.
func NewManager(opt Options) *Manager {
	return &Manager{opt: opt}
}

// Apply sets the format of the log, and starts the export settings ask unless it runs as they ask already. An export
// that cannot start leaves the one before running, and says why.
func (m *Manager) Apply(ctx context.Context, settings config.Telemetry) error {
	m.opt.Logs.SetFormat(settings.LogFormat)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && sameExport(m.current.settings, settings) {
		return nil
	}
	next, err := Start(context.WithoutCancel(ctx), settings, m.opt)
	if err != nil {
		return err
	}
	if next.sink == nil {
		m.opt.Logs.SetSink(nil)
	}
	old := m.current
	m.current = next
	if old != nil {
		m.stopping.Go(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopWithin)
			defer cancel()
			_ = old.Shutdown(ctx)
		})
	}
	return nil
}

// Shutdown stops the export, sending what is left within ctx.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	current := m.current
	m.current = nil
	m.mu.Unlock()
	m.opt.Logs.SetSink(nil)
	err := current.Shutdown(ctx)
	stopped := make(chan struct{})
	go func() {
		m.stopping.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		err = errors.Join(err, ctx.Err())
	}
	return err
}

// sameExport says whether two settings ask the same export: the format of the log aside.
func sameExport(a, b config.Telemetry) bool {
	a.LogFormat, b.LogFormat = "", ""
	return a == b
}
