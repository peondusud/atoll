package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/trash"
)

// Copies at once (specs.md §14, the maintainer, 2026-11-19: "on an HDD one at a time, but on SSD/NVMe several"). A run
// starts each copy, in the order of its queue, as soon as the disk it reads and the disk it writes each have room for one
// more, and no other copy of its file runs: an HDD never has two copies at once, read or written, and an SSD at most its
// limit. What a copy writes to the catalog it writes as it would alone, SQLite taking one writer at a time; what it says
// comes out in the order of the queue, whatever order the copies end in.

const (
	// copiesAtOnce is how many copies run at once at most, whatever the disks allow: each holds a chunk of 4 MiB and a
	// hasher while it runs, so that 8 hold some 32 MiB (specs.md §26).
	copiesAtOnce = 8
	// copyWindow is how far past the first copy not over yet a run looks for one that may start: a copy waits for its
	// disks while at most these start after it, and what those say is held until it is over (specs.md §26).
	copyWindow = 4096
)

// copyLimits is how many copies may read or write each disk at once, and how many may run in all.
type copyLimits struct {
	perDisk map[string]int
	total   int
}

// of gives a disk's limit: one for a disk the configuration does not list.
func (l *copyLimits) of(disk string) int {
	if n := l.perDisk[disk]; n > 0 {
		return n
	}
	return 1
}

// limitsOf works out the copies at once each disk takes, by its kind as the kernel tells it, unless
// replication.max_parallel_per_disk says so for every disk (specs.md §14). A machine that cannot tell the kind of a disk,
// as the conformance build, runs one copy at a time: copies at once there would make the ids of the journal, and the
// point a crash stops at, depend on timing (§29.4).
func limitsOf(c *config.Config, m disks.Machine) copyLimits {
	if !disks.ReadsKinds(m) {
		return copyLimits{total: 1}
	}
	set := c.CopyLimits()
	out := copyLimits{perDisk: make(map[string]int, len(c.Disks)), total: copiesAtOnce}
	for i := range c.Disks {
		d := &c.Disks[i]
		out.perDisk[d.ID] = disks.CopyLimit(m, d, set)
	}
	return out
}

// sharedSession is the session of a run as its copies share it: one at a time asks it anything. A disk a copy finds it
// cannot rely on is lost at once for every copy that has not started, and those under way on it stop at their next step;
// the session lets go of it, its handle closed, once none of them uses it any more, as a handle closed under a copy could
// lead its next call to another file that took its number.
type sharedSession struct {
	s     *trash.Session
	mu    sync.Mutex
	inUse map[string]int
	// lost holds the disks lost while a copy used them, with why, until the session is told
	lost map[string]error
	// names holds a lock for each disk, taken around what makes or removes a name there: a part, a folder, the copy at
	// its name. Two copies to one folder would otherwise each find a parent missing and the second fail to make it, or
	// put the folder's time back to what it was before the other's rename.
	names map[string]*sync.Mutex
}

func newSharedSession(s *trash.Session) *sharedSession {
	return &sharedSession{s: s, inUse: map[string]int{}, lost: map[string]error{}, names: map[string]*sync.Mutex{}}
}

// Writable says whether a disk can still be written to in this run.
func (s *sharedSession) Writable(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, lost := s.lost[id]
	return !lost && s.s.Writable(id)
}

// Readable says whether a disk can still be read in this run.
func (s *sharedSession) Readable(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, lost := s.lost[id]
	return !lost && s.s.Readable(id)
}

// Handle gives a handle on a disk, confirmed once in the session, and nil once it is lost.
func (s *sharedSession) Handle(ctx context.Context, id string) (*ops.Disk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, lost := s.lost[id]; lost {
		return nil, nil //nolint:nilnil // a disk lost is one that cannot be reached
	}
	return s.s.Handle(ctx, id) //nolint:wrapcheck // the session says what failed
}

// Lose says that a disk cannot be relied on any more: at once for what starts, and for the session once no copy uses it.
func (s *sharedSession) Lose(ctx context.Context, id string, why error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse[id] > 0 {
		if _, ok := s.lost[id]; !ok {
			s.lost[id] = why
		}
		return nil
	}
	return s.s.Lose(ctx, id, why) //nolint:wrapcheck // the session's own
}

// MarkChanged says that something moved, so that each file is worked out again.
func (s *sharedSession) MarkChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.MarkChanged()
}

// halted says whether a disk a copy under way uses was lost, by it or by a copy beside it.
func (s *sharedSession) halted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, lost := s.lost[id]
	return lost || !s.s.Readable(id)
}

// namesOf gives the lock of a disk's names.
func (s *sharedSession) namesOf(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	mu := s.names[id]
	if mu == nil {
		mu = &sync.Mutex{}
		s.names[id] = mu
	}
	return mu
}

// use says that a copy starts on its disks.
func (s *sharedSession) use(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.inUse[id]++
	}
}

// release says that a copy is over on its disks, and tells the session of a disk lost meanwhile once nothing uses it.
func (s *sharedSession) release(ctx context.Context, ids ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if s.inUse[id]--; s.inUse[id] > 0 {
			continue
		}
		delete(s.inUse, id)
		if why, ok := s.lost[id]; ok {
			delete(s.lost, id)
			errs = append(errs, s.s.Lose(ctx, id, why))
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // the session's own
}

// The states of a copy in a run.
const (
	copyWaits = iota
	copyRuns
	copyOver
	copySkipped
)

// copyEntry is a copy of the queue a run has read, until what it says is given.
type copyEntry struct {
	job     *catalog.ReplicateJob
	state   int
	outcome string
}

// copyDone is a copy over, from the goroutine that ran it.
type copyDone struct {
	entry   *copyEntry
	outcome string
	err     error
}

// runAll runs the copies next gives, in that order, several at once (specs.md §14): each starts as soon as the disks it
// reads and writes have room for one more and no other copy of its file runs, the per-path lock of §14 that every copy of
// a file shares. A copy that waits for a disk lets later copies use the others meanwhile. skip says, as a copy is about
// to start, that it must not, which counts it as waiting; give is told of each copy that ran, in the order next gave
// them, whatever order they ended in. A canceled context, an error of the catalog or one give returns stops the run: no
// copy starts, those under way stop at their next step, and runAll returns once each is over, so that nothing uses a
// disk the session lets go of then.
func (r *replicator) runAll(ctx context.Context, next func(context.Context) ([]catalog.ReplicateJob, error), skip func(*catalog.ReplicateJob) bool, give func(*catalog.ReplicateJob, string) error) error {
	limits := limitsOf(r.config, r.machine)
	r.on = newSharedSession(r.session)
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan copyDone)
	var (
		window   []*copyEntry
		page     []catalog.ReplicateJob
		more     = true
		running  int
		busy     = map[string]int{}
		files    = map[int64]bool{}
		firstErr error
		gave     error
	)
	stop := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
		cancel()
	}
	// over takes a copy over: its disks and its file free again, and a disk lost meanwhile told to the session.
	over := func(d copyDone) {
		running--
		j := d.entry.job
		busy[j.Src]--
		busy[j.Dst]--
		delete(files, j.FileID)
		d.entry.state, d.entry.outcome = copyOver, d.outcome
		if err := r.on.release(ctx, j.Src, j.Dst); err != nil {
			stop(fmt.Errorf("%w", err))
		}
		if d.err != nil {
			// An error of the catalog ends the run: the copy says nothing.
			d.entry.state = copySkipped
			stop(d.err)
		}
	}
	// try starts a copy of the window if it may start now.
	try := func(e *copyEntry) {
		j := e.job
		if firstErr != nil || running >= limits.total || e.state != copyWaits || files[j.FileID] ||
			busy[j.Src] >= limits.of(j.Src) || busy[j.Dst] >= limits.of(j.Dst) {
			return
		}
		if skip(j) {
			e.state = copySkipped
			return
		}
		dst, src, outcome, err := r.prepare(ctx, j)
		switch {
		case err != nil:
			stop(err)
			return
		case outcome != "":
			e.state, e.outcome = copyOver, outcome
			return
		}
		e.state = copyRuns
		running++
		busy[j.Src]++
		busy[j.Dst]++
		files[j.FileID] = true
		r.on.use(j.Src, j.Dst)
		if limits.total == 1 {
			// One at a time, in this goroutine, as before copies ran at once: what a test makes stop, as kill -9 would,
			// stops the run itself.
			outcome, err := r.runOn(work, j, dst, src)
			over(copyDone{entry: e, outcome: outcome, err: err})
			return
		}
		go func() {
			outcome, err := r.runOn(work, j, dst, src)
			results <- copyDone{entry: e, outcome: outcome, err: err}
		}()
	}
	// giveOver gives what is over at the head of the window, in the order of the queue.
	giveOver := func() {
		for firstErr == nil && len(window) > 0 && (window[0].state == copyOver || window[0].state == copySkipped) {
			e := window[0]
			window[0] = nil
			window = window[1:]
			if e.state == copyOver {
				if gave = give(e.job, e.outcome); gave != nil {
					stop(gave)
				}
			}
		}
	}
	for {
		if err := ctx.Err(); err != nil && firstErr == nil {
			stop(fmt.Errorf("replicate: %w", err))
		}
		giveOver()
		if firstErr == nil && running < limits.total && more && len(window) < copyWindow {
			// One more copy of the queue, while one could start: so one at a time, it reads one copy ahead, as before.
			if len(page) == 0 {
				p, err := next(ctx)
				switch {
				case err != nil:
					stop(fmt.Errorf("catalog: %w", err))
				case len(p) == 0:
					more = false
				}
				page = p
				continue
			}
			e := &copyEntry{job: &page[0]}
			page = page[1:]
			window = append(window, e)
			try(e)
			continue
		}
		if running == 0 {
			if firstErr != nil || !more && len(window) == 0 {
				break
			}
			// Nothing runs, and the window is full of copies that could not start: none can be, as nothing holds their
			// disks, but for skip, whose answer may change.
			for _, e := range window {
				try(e)
			}
			continue
		}
		over(<-results)
		// What was over freed disks: the copies that waited for them start, in order.
		for _, e := range window {
			try(e)
		}
	}
	// A run stopped by its context or the catalog still says what the copies over before it did, in their order.
	if gave == nil {
		for _, e := range window {
			if e.state == copyOver {
				if gave = give(e.job, e.outcome); gave != nil {
					break
				}
			}
		}
	}
	return firstErr
}

// prepare says whether a copy can run in this run, and opens its disks: a job whose operation the reprise could not
// conclude does not, nor one whose source or target cannot be reached (specs.md §14.1).
func (r *replicator) prepare(ctx context.Context, job *catalog.ReplicateJob) (dst, src *ops.Disk, outcome string, err error) {
	// Still running after the reprise, a job's operation is one the reprise could not conclude: what is at its target may
	// be its own copy, and nothing may be decided of it until that operation is (§17.2).
	if job.State == catalog.JobRunning {
		return nil, nil, trash.OutcomeIOError, nil
	}
	return r.handles(ctx, job)
}

// errHalted says that a copy beside this one lost a disk this one uses: it stops at its next step, as on that disk's own
// error (invariant 4).
var errHalted = errors.New("a copy beside it lost the disk")

// halted says whether a disk a copy reads or writes was lost since it started: dst for its target, src for its source.
func (r *replicator) halted(job *catalog.ReplicateJob) (src, dst bool) {
	if r.on == nil {
		return false, false
	}
	return r.on.halted(job.Src), r.on.halted(job.Dst)
}
