package value

import (
	"fmt"
	"math/big"
)

// Percent is a percentage from 0 to 100, kept exact. The zero value is 0%.
type Percent struct {
	n    number
	text string
}

// ParsePercent parses a percentage (specs.md §29.5):
//
//	percent = number "%"        ; 0 to 100
func ParsePercent(s string) (Percent, error) {
	n, rest, ok := cutNumber(s)
	if !ok || rest != "%" {
		return Percent{}, fmt.Errorf("percentage %q: want a number from 0 to 100 followed by %%, such as 5%%", s)
	}
	limit := new(big.Int).Mul(big.NewInt(100), pow10(n.scale))
	if n.num.Cmp(limit) > 0 {
		return Percent{}, fmt.Errorf("percentage %q: above 100%%", s)
	}
	return Percent{n: n, text: s}, nil
}

// Of returns the share p of total, computed exactly and rounded down.
func (p Percent) Of(total int64) int64 {
	if p.n.num == nil {
		return 0
	}
	r := new(big.Int).Mul(big.NewInt(total), p.n.num)
	// Div is Euclidean: with a positive divisor it rounds down. The result lies
	// between 0 and total, so it fits in an int64.
	r.Div(r, new(big.Int).Mul(big.NewInt(100), pow10(p.n.scale)))
	return r.Int64()
}

// String returns the percentage as written in the configuration.
func (p Percent) String() string {
	if p.text == "" {
		return "0%"
	}
	return p.text
}
