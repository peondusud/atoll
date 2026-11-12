package daemon

import (
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/value"
)

// When the daemon balances (specs.md §13.3): never in shadow mode, before the first pass or with "never"; with a
// window, a check at the start of each stretch, then slices while copies are left within the budget; with
// "immediate", a check an hour.
func TestBalanceDue(t *testing.T) {
	at := func(clock string) int64 {
		tm, err := time.Parse(time.RFC3339, "2026-11-12T"+clock+":00Z")
		if err != nil {
			t.Fatal(err)
		}
		return tm.UnixNano()
	}
	window, err := value.ParseWindow("02:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	end := int64(1)
	newLoop := func(schedule string, apply bool) *loop {
		return &loop{opt: &Options{Apply: apply}, lastEnd: &end,
			set: &settings{balanceSchedule: schedule, balanceWindow: window, balanceBudget: time.Hour, location: time.UTC}}
	}
	due := func(l *loop, now int64) string {
		d := l.balanceDue(now)
		if d == nil {
			return "never"
		}
		if *d <= now {
			// Wake takes what is due by now as due now.
			return "now"
		}
		return time.Unix(0, *d).UTC().Format("2006-01-02 15:04")
	}
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: %s, want %s", what, got, want)
		}
	}
	check("shadow mode", due(newLoop("02:00-06:00", false), at("03:00")), "never")
	check("never", due(newLoop("never", true), at("03:00")), "never")
	first := newLoop("02:00-06:00", true)
	first.lastEnd = nil
	check("before the first pass", due(first, at("03:00")), "never")

	l := newLoop("02:00-06:00", true)
	check("before the window", due(l, at("01:00")), "2026-11-12 02:00")
	check("a stretch not checked", due(l, at("03:00")), "now")
	l.balanceStretch = at("02:00")
	check("checked, nothing to move", due(l, at("03:00")), "2026-11-13 02:00")
	l.balanceWork = true
	check("copies left within the budget", due(l, at("03:00")), "now")
	l.balanceSpent = time.Hour
	check("the budget spent", due(l, at("03:00")), "2026-11-13 02:00")
	check("the next night, a new check", due(l, at("02:30")+int64(24*time.Hour)), "now")

	i := newLoop("immediate", true)
	check("immediate, never checked", due(i, at("12:00")), "now")
	i.balanceNext = at("13:00")
	check("immediate, checked", due(i, at("12:00")), "2026-11-12 13:00")
	i.balanceWork = true
	check("immediate, copies left", due(i, at("12:00")), "now")
	i.balanceSpent = time.Hour
	check("immediate, the budget spent", due(i, at("12:00")), "2026-11-12 13:00")
}

// balance.trigger holds when the share of bytes correctly placed falls below ratio_below, or when more than
// or_more_than bytes are to move; never with nothing misplaced (specs.md §13.3).
func TestBalanceHolds(t *testing.T) {
	ratio, err := value.ParsePercent("90%")
	if err != nil {
		t.Fatal(err)
	}
	s := &settings{balanceRatio: ratio, balanceMore: 100}
	for _, tc := range []struct {
		m     jobs.BalanceMeasure
		holds bool
	}{
		{jobs.BalanceMeasure{Total: 1000}, false},
		{jobs.BalanceMeasure{Total: 1000, Misplaced: 50, ToMove: 50}, false},
		{jobs.BalanceMeasure{Total: 1000, Misplaced: 100, ToMove: 100}, false},
		{jobs.BalanceMeasure{Total: 1000, Misplaced: 101, ToMove: 101}, true},
		{jobs.BalanceMeasure{Total: 100000, Misplaced: 60, ToMove: 120}, true},
	} {
		if got := s.balanceHolds(&tc.m); got != tc.holds {
			t.Errorf("%+v: %v", tc.m, got)
		}
	}
	// The share alone: far fewer bytes to move than or_more_than.
	far := &settings{balanceRatio: ratio, balanceMore: 1 << 40}
	if !far.balanceHolds(&jobs.BalanceMeasure{Total: 1000, Misplaced: 101, ToMove: 101}) || far.balanceHolds(&jobs.BalanceMeasure{Total: 1000, Misplaced: 100, ToMove: 100}) {
		t.Error("the share of bytes correctly placed does not decide alone")
	}
}

// A check on a pool with nothing misplaced moves nothing, and with "immediate" waits an hour for the next.
func TestBalanceChecksAnHour(t *testing.T) {
	l, _ := tellLoop(t, true)
	end := int64(1)
	l.lastEnd = &end
	ratio, err := value.ParsePercent("90%")
	if err != nil {
		t.Fatal(err)
	}
	l.set.balanceSchedule, l.set.balanceBudget, l.set.balanceRatio, l.set.balanceMore = scheduleImmediate, time.Hour, ratio, 1<<40
	before := l.opt.Machine.Now()
	l.balance(t.Context())
	if l.balanceWork || l.balanceNext < before+balanceEvery.Nanoseconds() {
		t.Errorf("work %v, next %d", l.balanceWork, l.balanceNext)
	}
	if due := l.balanceDue(l.opt.Machine.Now()); due == nil || *due != l.balanceNext {
		t.Errorf("due %v", due)
	}
}
