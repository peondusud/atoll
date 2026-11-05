package value

import (
	"fmt"
	"math/big"
)

// ParseSize parses a size into bytes (specs.md §29.5):
//
//	size = number ( "B" | "KB" | "MB" | "GB" | "TB"      ; powers of 1000
//	              | "KiB" | "MiB" | "GiB" | "TiB" )      ; powers of 1024
//
// The value is computed exactly, then rounded down to whole bytes. The result
// must fit in an int64.
func ParseSize(s string) (int64, error) {
	n, unit, ok := cutNumber(s)
	if !ok {
		return 0, fmt.Errorf("size %q: want a number followed by a unit, such as 100GiB", s)
	}
	mult, ok := sizeUnit(unit)
	if !ok {
		return 0, fmt.Errorf("size %q: unknown unit %q: want B, KB, MB, GB, TB, KiB, MiB, GiB or TiB", s, unit)
	}
	b := new(big.Int).Mul(n.num, big.NewInt(mult))
	b.Quo(b, pow10(n.scale)) // both are non-negative: rounds down
	if !b.IsInt64() {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	return b.Int64(), nil
}

func sizeUnit(unit string) (int64, bool) {
	switch unit {
	case "B":
		return 1, true
	case "KB":
		return 1e3, true
	case "MB":
		return 1e6, true
	case "GB":
		return 1e9, true
	case "TB":
		return 1e12, true
	case "KiB":
		return 1 << 10, true
	case "MiB":
		return 1 << 20, true
	case "GiB":
		return 1 << 30, true
	case "TiB":
		return 1 << 40, true
	default:
		return 0, false
	}
}
