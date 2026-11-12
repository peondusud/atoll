package daemon

import (
	"context"
	"database/sql"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
)

// balanceSlice is how long the daemon moves copies under the catalog's lock at a time, so that a command never waits
// long for it, and passes come in between (specs.md §13.3, §15.13).
const balanceSlice = 5 * time.Minute

// balanceEvery is how often the daemon checks balance.trigger with balance.schedule = "immediate": a check plans the
// whole pool, so at most once an hour (the maintainer, 2026-11-12).
const balanceEvery = time.Hour

// The values of balance.schedule that are no window.
const (
	scheduleNever     = "never"
	scheduleImmediate = "immediate"
)

// balanceDue says when the daemon next balances (specs.md §13.3): nil when it never does, with balance.schedule =
// "never", in shadow mode, or before the first pass. With a window, it checks balance.trigger once a stretch, then
// moves copies while the trigger held, copies are left and balance.budget is not spent; with "immediate", likewise
// once an hour.
func (l *loop) balanceDue(now int64) *int64 {
	if !l.opt.Apply || l.lastEnd == nil || l.set.balanceSchedule == scheduleNever {
		return nil
	}
	working := l.balanceWork && l.balanceSpent < l.set.balanceBudget
	if l.set.balanceSchedule == scheduleImmediate {
		if working {
			return &now
		}
		next := l.balanceNext
		return &next
	}
	nowLocal := time.Unix(0, now).In(l.set.location)
	opened := l.set.balanceWindow.Opened(nowLocal)
	if opened.IsZero() {
		next := l.set.balanceWindow.Next(nowLocal).UnixNano()
		return &next
	}
	if opened.UnixNano() != l.balanceStretch || working {
		return &now
	}
	next := l.set.balanceWindow.Next(opened.AddDate(0, 0, 1)).UnixNano()
	return &next
}

// balanceHolds says whether balance.trigger holds: the share of bytes correctly placed below ratio_below, or more
// than or_more_than bytes to move (specs.md §13.3). With nothing misplaced, it never does.
func (s *settings) balanceHolds(m *jobs.BalanceMeasure) bool {
	if m.Misplaced == 0 {
		return false
	}
	return s.balanceRatio.Below(m.Total-m.Misplaced, m.Total) || m.ToMove > s.balanceMore
}

// balance checks balance.trigger at the start of a stretch of balance.schedule, or of an hour with "immediate", then,
// while it held, moves the misplaced copies a slice of time at a time, until none is left or balance.budget is spent
// (specs.md §13.3).
func (l *loop) balance(ctx context.Context) {
	now := l.opt.Machine.Now()
	fresh := !l.balanceWork
	if l.set.balanceSchedule != scheduleImmediate {
		stretch := l.set.balanceWindow.Opened(time.Unix(0, now).In(l.set.location)).UnixNano()
		fresh = fresh || stretch != l.balanceStretch
		l.balanceStretch = stretch
	}
	if fresh {
		l.balanceNext = now + balanceEvery.Nanoseconds()
		l.balanceWork, l.balanceSpent = false, 0
		var measure *jobs.BalanceMeasure
		_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
			var err error
			measure, err = jobs.MeasureBalance(ctx, db, l.opt.Config, l.opt.Machine)
			return nil, err
		})
		if err != nil {
			if ctx.Err() == nil {
				l.log.Warn("what to balance could not be measured: the next check tries again", "error", err)
			}
			return
		}
		if !l.set.balanceHolds(measure) {
			l.log.Debug("balanced enough", "misplaced_bytes", measure.Misplaced, "total_bytes", measure.Total, "to_move", measure.ToMove)
			return
		}
		l.log.Info("copies break a strict setting: balancing", "misplaced_bytes", measure.Misplaced, "total_bytes", measure.Total, "to_move", measure.ToMove)
		l.balanceWork = true
	}
	start := l.opt.Machine.Now()
	slice := min(balanceSlice, l.set.balanceBudget-l.balanceSpent)
	var sum *jobs.BalanceSummary
	_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		var err error
		sum, err = jobs.Balance(ctx, db, l.opt.Config, l.opt.Machine, true, start+slice.Nanoseconds(), func(item *jobs.BalanceItem) error {
			l.log.Info("a copy was balanced", "path", item.Path, "from", item.From, "to", item.To, "outcome", item.Outcome)
			return nil
		})
		return nil, err
	})
	l.balanceSpent += time.Duration(l.opt.Machine.Now() - start)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("balancing stopped: the next check tries again", "error", err)
		}
		l.balanceWork = false
		return
	}
	l.balanceWork = sum.Left && l.balanceSpent < l.set.balanceBudget
	if sum.Left && !l.balanceWork {
		l.log.Info("balancing reached balance.budget: the next check goes on", "spent", l.balanceSpent.String())
	}
}
