package value

import (
	"fmt"
	"math/big"
	"strconv"
	"time"
)

// ParseDuration parses a duration (specs.md §29.5):
//
//	duration = digits ( "s" | "m" | "h" | "d" | "w" )
//
// A day is 24 hours and a week 7 days. The result must fit in a time.Duration.
func ParseDuration(s string) (time.Duration, error) {
	i := countDigits(s)
	if i == 0 || len(s) != i+1 {
		return 0, fmt.Errorf("duration %q: want digits followed by one unit: s, m, h, d or w", s)
	}
	var unit time.Duration
	switch s[i] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("duration %q: unknown unit %q: want s, m, h, d or w", s, s[i:])
	}
	n, ok := new(big.Int).SetString(s[:i], 10)
	if !ok {
		return 0, fmt.Errorf("duration %q: want digits followed by one unit: s, m, h, d or w", s)
	}
	n.Mul(n, big.NewInt(int64(unit)))
	if !n.IsInt64() {
		return 0, fmt.Errorf("duration %q: too long", s)
	}
	return time.Duration(n.Int64()), nil
}

// FormatDuration writes a duration as the configuration writes it (specs.md §29.5): in the largest of d, h, m and s
// that divides it, "0s" for none, and never in weeks, as people write "7d" and "14d". ParseDuration reads it back. A duration of no whole seconds is written in
// nanoseconds, with "ns", which the configuration does not take: no duration it gives is one.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if d%u.unit == 0 {
			return strconv.FormatInt(int64(d/u.unit), 10) + u.name
		}
	}
	return strconv.FormatInt(int64(d), 10) + "ns"
}
