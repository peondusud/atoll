package telemetry

import (
	"context"
	"errors"
	"sync"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// What the queue of the log records holds and sends: at most queueSize records, sent by batches of batchSize at
// least every sendEvery.
const (
	queueSize = 2048
	batchSize = 512
	sendEvery = time.Second
)

// queue is the processor of the log records (specs.md §23.2): OnEmit puts a record in a ring of fixed size and never
// waits, one goroutine sends them by batches, each export given timeout in all. A full ring drops its oldest record,
// and a batch that fails is dropped too; the reporter counts both.
type queue struct {
	exporter sdklog.Exporter
	report   *reporter
	timeout  time.Duration
	every    time.Duration

	mu      sync.Mutex
	ring    []sdklog.Record
	head, n int
	dropped int64
	closed  bool

	kick  chan struct{}
	flush chan chan struct{}
	stop  chan time.Time
	done  chan struct{}
}

// newQueue starts the queue's goroutine, whose exports take their values from ctx; Shutdown alone ends it.
func newQueue(ctx context.Context, exporter sdklog.Exporter, report *reporter, timeout, every time.Duration, size int) *queue {
	q := &queue{exporter: exporter, report: report, timeout: timeout, every: every, ring: make([]sdklog.Record, size),
		kick: make(chan struct{}, 1), flush: make(chan chan struct{}), stop: make(chan time.Time, 1), done: make(chan struct{})}
	go q.run(context.WithoutCancel(ctx))
	return q
}

// Enabled takes every record the handler lets through.
func (q *queue) Enabled(context.Context, sdklog.EnabledParameters) bool {
	return true
}

// OnEmit queues a copy of the record, the oldest dropped when the ring is full. It never waits on the exporter.
func (q *queue) OnEmit(_ context.Context, r *sdklog.Record) error {
	record := r.Clone()
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	if q.n == len(q.ring) {
		q.ring[q.head] = sdklog.Record{}
		q.head = (q.head + 1) % len(q.ring)
		q.n--
		q.dropped++
	}
	q.ring[(q.head+q.n)%len(q.ring)] = record
	q.n++
	full := q.n >= batchSize
	q.mu.Unlock()
	if full {
		select {
		case q.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// take takes the next batch out of the ring, and what was dropped since the last take.
func (q *queue) take() (batch []sdklog.Record, dropped int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := min(q.n, batchSize)
	batch = make([]sdklog.Record, k)
	for i := range k {
		at := (q.head + i) % len(q.ring)
		batch[i], q.ring[at] = q.ring[at], sdklog.Record{}
	}
	q.head, q.n = (q.head+k)%len(q.ring), q.n-k
	dropped, q.dropped = q.dropped, 0
	return batch, dropped
}

// empty drops what the ring holds, and gives how many it dropped, those dropped before it included.
func (q *queue) empty() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	left := int64(q.n) + q.dropped
	clear(q.ring)
	q.head, q.n, q.dropped = 0, 0, 0
	return left
}

// send sends one batch within ctx and the queue's timeout, and says whether one went: a failure leaves the rest for the
// next turn, so that a dead collector is not asked again at once.
func (q *queue) send(ctx context.Context) bool {
	batch, dropped := q.take()
	if dropped > 0 {
		q.report.dropped(signalLogs, dropped)
	}
	if len(batch) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	err := q.exporter.Export(ctx, batch)
	q.report.sent(signalLogs, err, int64(len(batch)))
	return err == nil
}

func (q *queue) run(ctx context.Context) {
	defer close(q.done)
	ticker := time.NewTicker(q.every)
	defer ticker.Stop()
	for {
		select {
		case deadline := <-q.stop:
			// What is left goes within the stop's time.
			stopping, cancel := context.WithDeadline(ctx, deadline)
			for stopping.Err() == nil && q.send(stopping) {
			}
			cancel()
			// What is left once the time to stop is over is dropped, and said so.
			if left := q.empty(); left > 0 {
				q.report.dropped(signalLogs, left)
			}
			return
		case reply := <-q.flush:
			for q.send(ctx) {
			}
			close(reply)
		case <-ticker.C:
			for q.send(ctx) {
			}
		case <-q.kick:
			for q.send(ctx) {
			}
		}
	}
}

// ForceFlush sends what the ring holds, within ctx.
func (q *queue) ForceFlush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case q.flush <- reply:
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown sends what the ring holds by ctx's deadline, or within stopWithin with none, drops the rest, and shuts the
// exporter down. Records that come after are dropped.
func (q *queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	q.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(stopWithin)
	}
	q.stop <- deadline
	select {
	case <-q.done:
	case <-ctx.Done():
		return errors.Join(ctx.Err(), q.exporter.Shutdown(ctx))
	}
	return q.exporter.Shutdown(ctx)
}
