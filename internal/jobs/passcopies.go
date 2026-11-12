package jobs

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/watch"
)

// copies queues the copies the plan wants of every file whose settle has passed since it was first seen, then
// runs every copy that is due, one at a time (specs.md §14.1). A dry run says what it would copy.
func (p *pass) copies(ctx context.Context) error {
	if enabled, _ := p.config.Effective()["replication.enabled"].(bool); !enabled {
		return nil
	}
	// What the walks found has served, and the plan reads every file.
	p.seen, p.moves = nil, nil
	var lines []copyLine
	rep := &watch.Replicate{DryRun: !p.apply, Queue: func(jobs []watch.PlannedJob) error {
		jobs = p.settledCopies(jobs)
		if !p.apply {
			for i := range jobs {
				lines = append(lines, copyLine{path: jobs[i].Path, dst: jobs[i].Dst, src: jobs[i].Src, priority: jobs[i].Priority})
			}
			return nil
		}
		return queue(ctx, p.db, p.now, jobs)
	}}
	if err := watch.SettleFor(ctx, p.db, p.config, p.now, rep); err != nil {
		return fmt.Errorf("%w", err)
	}
	if !p.apply {
		// The queue is read once the plan has gone, so that both are never held at once.
		lines, err := p.dueLines(ctx, lines)
		if err != nil {
			return err
		}
		return p.listCopies(lines)
	}
	p.settled = true
	r := &replicator{db: p.db, config: p.config, machine: p.machine, session: p.session, now: p.now, sum: &ReplicateSummary{Applied: true}}
	schedule, err := scheduleOf(p.config)
	if err != nil {
		return err
	}
	reader := catalog.NewDueReader(p.db, p.now)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("replicate: %w", err)
			}
			job := &page[i]
			if p.waitsForWindow(job, schedule) {
				continue
			}
			// What a copy does, the files must be worked out again for.
			p.settled = false
			outcome, err := r.run(ctx, job)
			if err != nil {
				return err
			}
			p.record(Operation{Disk: job.Dst, Kind: KindReplicate, Path: job.Path, Source: job.Src, Outcome: outcome})
			if p.sinkErr != nil {
				return p.sinkErr
			}
		}
	}
	count, next, err := catalog.CopiesLater(ctx, p.db, p.now)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	p.copiesWaiting += count
	p.copiesDue = earliest(p.copiesDue, next)
	return nil
}

// settledCopies keeps the copies of the files whose settle has passed since they were first seen, and counts
// the files that still wait for it (specs.md §14.1): with no watcher, that is all that tells a file still
// being written. A file in immediate mode the watcher saw closed, and quiet since, waits for nothing. The copies of
// one file come together.
func (p *pass) settledCopies(jobs []watch.PlannedJob) []watch.PlannedJob {
	last := int64(-1)
	return slices.DeleteFunc(jobs, func(j watch.PlannedJob) bool {
		size := j.Size
		policy := p.config.Policy(j.Path, &size)
		if policy.Mode == "immediate" && p.closed[j.Path] {
			return false
		}
		due := j.FirstSeenNS + policy.Settle.Nanoseconds()
		if p.now >= due {
			return false
		}
		if j.FileID != last {
			last = j.FileID
			p.copiesWaiting++
		}
		p.copiesDue = earliest(p.copiesDue, sql.NullInt64{Int64: due, Valid: true})
		return true
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func earliest(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid || (b.Valid && b.Int64 < a.Int64) {
		return b
	}
	return a
}

// copyLine is what a dry run keeps of a copy until it has them all, to sort them: at 200,000 copies, a whole
// Operation for each, besides the plan, came close to the memory of specs.md §26.
type copyLine struct {
	path, dst, src string
	priority       int64
	queued         bool // queued already, so before what the plan would queue at its priority
}

// dueLines adds the jobs due already, in the order a real pass runs them.
func (p *pass) dueLines(ctx context.Context, lines []copyLine) ([]copyLine, error) {
	schedule, err := scheduleOf(p.config)
	if err != nil {
		return nil, err
	}
	reader := catalog.NewDueReader(p.db, p.now)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return lines, nil
		}
		for i := range page {
			j := &page[i]
			if p.waitsForWindow(j, schedule) {
				continue
			}
			lines = append(lines, copyLine{path: j.Path, dst: j.Dst, src: j.Src, priority: j.Priority, queued: true})
		}
	}
}

// listCopies lists what a dry run would copy, in the order a real pass would: by priority, highest first, the
// jobs due already before those the plan would queue, each in its own order.
func (p *pass) listCopies(lines []copyLine) error {
	slices.SortStableFunc(lines, func(a, b copyLine) int {
		return cmp.Or(cmp.Compare(b.priority, a.priority), cmp.Compare(boolInt(b.queued), boolInt(a.queued)))
	})
	for i := range lines {
		l := &lines[i]
		p.operated(Operation{Disk: l.dst, Kind: KindReplicate, Path: l.path, Source: l.src, Outcome: OutcomeToReplicate})
		if p.sinkErr != nil {
			return p.sinkErr
		}
	}
	return nil
}

// schedule is replication.window, in the time zone of timezone: where the copies of a file in scheduled mode run
// (specs.md §12.3).
type schedule struct {
	window   value.Window
	location *time.Location
}

// scheduleOf reads replication.window, maintenance.window when left out, and timezone, local when left out.
func scheduleOf(c *config.Config) (schedule, error) {
	effective := c.Effective()
	text, _ := effective["replication.window"].(string)
	w, err := value.ParseWindow(text)
	if err != nil {
		return schedule{}, fmt.Errorf("replication.window: %w", err)
	}
	s := schedule{window: w, location: time.Local}
	if zone, _ := effective["timezone"].(string); zone != "" && zone != "local" {
		if s.location, err = time.LoadLocation(zone); err != nil {
			return schedule{}, fmt.Errorf("timezone: %w", err)
		}
	}
	return s, nil
}

// waitsForWindow says whether a copy of a file in scheduled mode waits for replication.window: outside it, it stays
// queued, it counts as waiting, and the pass says when the window opens, so that the daemon wakes then (specs.md
// §12.3, §14.1).
func (p *pass) waitsForWindow(job *catalog.ReplicateJob, s schedule) bool {
	size := job.Size
	if p.config.Policy(job.Path, &size).Mode != "scheduled" {
		return false
	}
	now := time.Unix(0, p.now).In(s.location)
	if s.window.Contains(now) {
		return false
	}
	p.copiesWaiting++
	p.copiesDue = earliest(p.copiesDue, sql.NullInt64{Int64: s.window.Next(now).UnixNano(), Valid: true})
	return true
}
