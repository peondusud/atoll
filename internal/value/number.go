package value

import "math/big"

// number is a decimal number of the grammar, num / 10^scale:
//
//	number = digits [ "." digits ]
type number struct {
	num   *big.Int
	scale int
}

// cutNumber splits s into its leading number and the rest. It reports false
// when s does not start with a number.
func cutNumber(s string) (n number, rest string, ok bool) {
	i := countDigits(s)
	if i == 0 {
		return number{}, s, false
	}
	digits := s[:i]
	scale := 0
	if i < len(s) && s[i] == '.' {
		f := countDigits(s[i+1:])
		if f == 0 {
			return number{}, s, false
		}
		digits += s[i+1 : i+1+f]
		scale = f
		i += 1 + f
	}
	num, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return number{}, s, false
	}
	return number{num: num, scale: scale}, s[i:], true
}

// countDigits returns the length of the run of ASCII digits that starts s.
func countDigits(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// pow10 returns 10^n.
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
