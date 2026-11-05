package value

import (
	"fmt"
	"math/big"
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
