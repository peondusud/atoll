package value

import (
	"fmt"
	"time"
)

// Window is a daily time window in wall-clock time, from its start included to
// its end excluded. A window whose end comes before its start wraps past
// midnight. The zero value is an empty window, which contains no time.
type Window struct {
	start, end int // minutes since midnight
}

// ParseWindow parses a time window (specs.md §29.5):
//
//	window = hh ":" mm "-" hh ":" mm        ; "02:00-06:00", "23:00-05:00"
//
// Hours run from 00 to 23 and minutes from 00 to 59. A window whose start
// equals its end is rejected: it could mean an empty window or the whole day.
func ParseWindow(s string) (Window, error) {
	if len(s) != len("hh:mm-hh:mm") || s[5] != '-' {
		return Window{}, fmt.Errorf("time window %q: want hh:mm-hh:mm, such as 02:00-06:00", s)
	}
	start, err := parseClock(s[:5])
	if err != nil {
		return Window{}, fmt.Errorf("time window %q: %w", s, err)
	}
	end, err := parseClock(s[6:])
	if err != nil {
		return Window{}, fmt.Errorf("time window %q: %w", s, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("time window %q: the start equals the end", s)
	}
	return Window{start: start, end: end}, nil
}

// parseClock parses hh:mm into minutes since midnight.
func parseClock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' || countDigits(s[:2]) != 2 || countDigits(s[3:]) != 2 {
		return 0, fmt.Errorf("%q: want hh:mm", s)
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, fmt.Errorf("%q: hours run from 00 to 23 and minutes from 00 to 59", s)
	}
	return h*60 + m, nil
}

// Contains reports whether t falls inside the window. Only the wall-clock time
// of t in its own location counts: the caller converts t to the configured
// time zone first. Daylight-saving changes then need no special case: a
// skipped wall-clock hour shortens the window, a repeated one lengthens it.
func (w Window) Contains(t time.Time) bool {
	h, m, _ := t.Clock()
	minute := h*60 + m
	switch {
	case w.start < w.end:
		return w.start <= minute && minute < w.end
	case w.start > w.end:
		return minute >= w.start || minute < w.end
	default:
		return false
	}
}

// startOn gives the window's start on a day, in a location: that wall-clock time, or, on a night when the clock
// jumps over it, the instant it jumps to, which is the first after it. For a wall-clock time that does not exist,
// time.Date gives one on either side of the jump, as far from it as the time asked for: the hour before it west of
// UTC, where the daemon then slept through that night's window, and after it east of UTC. Whichever it gave is in a
// zone that ends, or began, at the jump, the nearer of its two bounds.
func (w Window) startOn(y int, m time.Month, d int, loc *time.Location) time.Time {
	start := time.Date(y, m, d, w.start/60, w.start%60, 0, 0, loc)
	if h, mi, _ := start.Clock(); h*60+mi == w.start {
		return start
	}
	began, ends := start.ZoneBounds()
	switch {
	case ends.IsZero():
		return began
	case began.IsZero(), ends.Sub(start) < start.Sub(began):
		return ends
	default:
		return began
	}
}

// Next gives the first instant inside the window at or after t, in t's location: t itself when it is inside,
// otherwise the window's start that day, or the next day if that start has gone by. A start that a
// spring-forward night skips is the instant the clock jumps to, when the window still holds it. The zero window
// has none, and gives t.
func (w Window) Next(t time.Time) time.Time {
	if w.start == w.end || w.Contains(t) {
		return t
	}
	y, m, d := t.Date()
	for day := 0; day < 3; day++ {
		start := w.startOn(y, m, d+day, t.Location())
		if !start.Before(t) && w.Contains(start) {
			return start
		}
	}
	return t
}

// Opened gives when the stretch of the window that holds t began, in t's location: its start that day, or the day
// before for a window that wraps past midnight and t after midnight. Two instants of one stretch give the same
// answer, which tells a budget spent in it from the next night's. A start that a spring-forward night skips is
// the instant the clock jumps to, as for Next. Outside the window it gives the zero time.
func (w Window) Opened(t time.Time) time.Time {
	if !w.Contains(t) {
		return time.Time{}
	}
	h, m, _ := t.Clock()
	y, mo, d := t.Date()
	if h*60+m < w.start {
		d--
	}
	return w.startOn(y, mo, d, t.Location())
}

// String returns the window as hh:mm-hh:mm.
func (w Window) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}
