package solver

import (
	"fmt"
	"math"
)

// The limits of a signed 64-bit count of nanoseconds, in seconds and in
// nanoseconds that run forward from the second.
const (
	minTimeSeconds     = -9223372037
	minTimeNanoseconds = 145224192
	maxTimeSeconds     = 9223372036
	maxTimeNanoseconds = 854775807
)

// TimeNS converts a modification or change time, as a filesystem reports it,
// into the catalog's nanoseconds. A time out of range is stored at the nearest
// limit (specs.md §9), so reading it again gives the same value. The range is
// checked here because Go's time.Time.UnixNano wraps outside it without saying
// so: 1601 becomes a date in 2185. An error means that nanoseconds is outside 0
// to 999999999.
func TimeNS(seconds, nanoseconds int64) (int64, error) {
	if err := checkNanoseconds(nanoseconds); err != nil {
		return 0, err
	}
	switch {
	case inTimeRange(seconds, nanoseconds):
		return seconds*1000000000 + nanoseconds, nil
	case seconds < 0:
		return math.MinInt64, nil
	default:
		return math.MaxInt64, nil
	}
}

// CreationTimeNS converts a creation time, as a filesystem reports it, into
// btime_ns. ok is false when the time is absent: exactly 0, or out of range
// (specs.md §7). An error means that nanoseconds is outside 0 to 999999999.
func CreationTimeNS(seconds, nanoseconds int64) (ns int64, ok bool, err error) {
	if err := checkNanoseconds(nanoseconds); err != nil {
		return 0, false, err
	}
	if !inTimeRange(seconds, nanoseconds) || seconds == 0 && nanoseconds == 0 {
		return 0, false, nil
	}
	return seconds*1000000000 + nanoseconds, true, nil
}

// SameTime says whether two times are the same at a precision: each, divided by it and rounded down, toward the
// past, gives the same number, which is what a filesystem that holds only multiples of it keeps of a time
// (specs.md §9). A precision of 0, unknown, or 1 compares exactly.
func SameTime(a, b, precisionNS int64) bool {
	if precisionNS <= 1 {
		return a == b
	}
	return floorDiv(a, precisionNS) == floorDiv(b, precisionNS)
}

// Coarser gives the coarser of two precisions, an unknown one being exact.
func Coarser(a, b int64) int64 {
	return max(a, b, 1)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

func inTimeRange(seconds, nanoseconds int64) bool {
	switch {
	case seconds < minTimeSeconds, seconds > maxTimeSeconds:
		return false
	case seconds == minTimeSeconds:
		return nanoseconds >= minTimeNanoseconds
	case seconds == maxTimeSeconds:
		return nanoseconds <= maxTimeNanoseconds
	default:
		return true
	}
}

func checkNanoseconds(nanoseconds int64) error {
	if nanoseconds < 0 || nanoseconds > 999999999 {
		return fmt.Errorf("time: %d nanoseconds", nanoseconds)
	}
	return nil
}
