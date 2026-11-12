package notify

import (
	"slices"
	"time"
)

// Window is how long a folder waits after a server was told of it before that server is told of it again (specs.md
// §21.5).
const Window = 30 * time.Second

// RetryAfter is how long a server that could not be told waits before it is tried again.
const RetryAfter = 5 * time.Minute

// Queue holds what one server is to be told: the folders of the view that changed, each told at most once a
// Window. It does no I/O, and is not safe for concurrent use.
type Queue struct {
	pending map[string]bool
	held    map[string]bool
	told    map[string]int64
	retryNS int64
}

// NewQueue makes an empty queue.
func NewQueue() *Queue {
	return &Queue{pending: map[string]bool{}, held: map[string]bool{}, told: map[string]int64{}}
}

// Add queues folders, each a path relative to the top of the view, "" for the top itself. A folder set aside by
// Hold stays aside.
func (q *Queue) Add(folders ...string) {
	for _, f := range folders {
		if !q.held[f] {
			q.pending[f] = true
		}
	}
}

// Ready gives the folders that may be told at now, in byte order: queued, and not told in the last Window. None
// while a server that could not be told waits.
func (q *Queue) Ready(now int64) []string {
	if now < q.retryNS {
		return nil
	}
	var out []string
	for f := range q.pending {
		if at, ok := q.told[f]; !ok || now >= at+Window.Nanoseconds() {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// Told records that the server was told of folders at now: they leave the queue, and come back to it with the next
// change.
func (q *Queue) Told(folders []string, now int64) {
	for _, f := range folders {
		delete(q.pending, f)
		q.told[f] = now
	}
	for f, at := range q.told {
		if now >= at+Window.Nanoseconds() {
			delete(q.told, f)
		}
	}
}

// Failed makes the server wait RetryAfter from now before it is tried again: what it was to be told stays queued.
func (q *Queue) Failed(now int64) {
	q.retryNS = now + RetryAfter.Nanoseconds()
}

// Hold sets folders aside until Release: a file below them has a copy on a missing disk, and a scan now would find it
// gone (specs.md §21.4).
func (q *Queue) Hold(folders []string) {
	for _, f := range folders {
		if q.pending[f] {
			delete(q.pending, f)
			q.held[f] = true
		}
	}
}

// Release queues again what Hold set aside, to be looked at again.
func (q *Queue) Release() {
	for f := range q.held {
		q.pending[f] = true
	}
	clear(q.held)
}

// Next gives when the next folder queued may be told, false when none is.
func (q *Queue) Next() (int64, bool) {
	var next int64
	found := false
	for f := range q.pending {
		at := q.retryNS
		if told, ok := q.told[f]; ok {
			at = max(at, told+Window.Nanoseconds())
		}
		if !found || at < next {
			next, found = at, true
		}
	}
	return next, found
}

// Held says how many folders are set aside.
func (q *Queue) Held() int {
	return len(q.held)
}
