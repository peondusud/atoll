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

// String returns the window as hh:mm-hh:mm.
func (w Window) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}
