package rules

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxRegexBytes is the longest regex text the grammar of specs.md §29.5
// accepts, in NFC form. It keeps Go's own size and nesting limits out of
// reach.
const MaxRegexBytes = 1000

// maxRepeat bounds each repeat count, and the product of nested counts.
const maxRepeat = 1000

// MaxRegexWeight bounds the weight of a regex (specs.md §29.5). Every element
// that adds to the automaton weighs at least 1 per copy: a character, escape,
// ., anchor, flag group or class item 1, a property 100, a capturing group 1
// more than what it holds, a repeat its count times what it repeats, plus 1
// when it adds a branch. It keeps the automaton Rust builds within the regex
// crate's default size limit, which the length limit alone does not.
const MaxRegexWeight = 1000

// propertyWeight is the weight of \p or \P, in a class or not: Rust compiles
// a property to thousands of automaton states.
const propertyWeight = 100

// RegexError reports a regex outside the grammar of specs.md §29.5.
type RegexError struct {
	Regex  string // the regex text, without re:
	Offset int    // byte offset of the error in Regex
	Msg    string
}

func (e *RegexError) Error() string {
	return fmt.Sprintf("regex %q: %s, at byte %d", e.Regex, e.Msg, e.Offset)
}

// checkRegex reports whether text, in NFC form, follows the regex grammar of
// specs.md §29.5: the common subset of Go regexp and Rust regex in which both
// engines give the same meaning.
func checkRegex(text string) error {
	p := &regexParser{s: text}
	if !utf8.ValidString(text) {
		return p.fail(0, "not valid UTF-8")
	}
	if len(text) > MaxRegexBytes {
		return p.fail(MaxRegexBytes, "longer than %d bytes", MaxRegexBytes)
	}
	sz, err := p.regex()
	if err != nil {
		return err
	}
	if p.i < len(p.s) {
		return p.fail(p.i, ") without (")
	}
	if sz.weight > MaxRegexWeight {
		return p.fail(0, "weighs more than %d: each character, escape, ., anchor, flag group and class item weighs 1 and each property %d; "+
			"a capturing group, *, +, ?, {n,} and {n,m} with n < m add 1; repeats multiply what they repeat",
			MaxRegexWeight, propertyWeight)
	}
	return nil
}

// size is what a parse function measured in the text it parsed (specs.md
// §29.5): prod is the largest product of nested counted repeats, the "chain
// product", and weight the weight, capped at MaxRegexWeight+1.
type size struct{ prod, weight int }

// capWeight caps a weight just above MaxRegexWeight. A sum of two capped
// weights, or a capped weight times a count of at most maxRepeat, never
// overflows.
func capWeight(w int) int { return min(w, MaxRegexWeight+1) }

type regexParser struct {
	s string
	i int
}

func (p *regexParser) fail(at int, format string, args ...any) error {
	return &RegexError{Regex: p.s, Offset: at, Msg: fmt.Sprintf(format, args...)}
}

// peek returns the next character, or -1 at the end.
func (p *regexParser) peek() rune {
	if p.i >= len(p.s) {
		return -1
	}
	r, _ := utf8.DecodeRuneInString(p.s[p.i:])
	return r
}

func (p *regexParser) next() rune {
	r, w := utf8.DecodeRuneInString(p.s[p.i:])
	p.i += w
	return r
}

// The parse functions return the size of what they parsed. Every branch of
// an alternation weighs in, as Rust compiles them all; a non-capturing group
// weighs what it holds.

// regex = branch { "|" branch }
func (p *regexParser) regex() (size, error) {
	sz := size{prod: 1}
	for {
		b, err := p.branch()
		if err != nil {
			return size{}, err
		}
		sz = size{max(sz.prod, b.prod), capWeight(sz.weight + b.weight)}
		if p.peek() != '|' {
			return sz, nil
		}
		p.i++
	}
}

// branch = piece { piece }
func (p *regexParser) branch() (size, error) {
	start, sz := p.i, size{prod: 1}
	for {
		if r := p.peek(); r == -1 || r == '|' || r == ')' {
			break
		}
		pc, err := p.piece()
		if err != nil {
			return size{}, err
		}
		sz = size{max(sz.prod, pc.prod), capWeight(sz.weight + pc.weight)}
	}
	if p.i == start {
		return size{}, p.fail(start, `empty alternative: "", "a|" and "()" are not allowed`)
	}
	return sz, nil
}

// piece = atom [ repeat ] | anchor | "(?" flags ")"
func (p *regexParser) piece() (size, error) {
	start := p.i
	rest := p.s[p.i:]
	switch {
	case rest[0] == '^' || rest[0] == '$':
		p.i++
		return size{1, 1}, p.noRepeat(start)
	case strings.HasPrefix(rest, `\A`) || strings.HasPrefix(rest, `\z`):
		p.i += 2
		return size{1, 1}, p.noRepeat(start)
	case strings.HasPrefix(rest, "(?") && !strings.HasPrefix(rest, "(?:"):
		p.i += 2
		if err := p.flags(start); err != nil {
			return size{}, err
		}
		switch p.peek() {
		case ')':
			p.i++
			return size{1, 1}, p.noRepeat(start)
		case ':':
			p.i++
			inner, err := p.group(start)
			if err != nil {
				return size{}, err
			}
			return p.repeat(inner)
		default:
			return size{}, p.fail(start, "only (?:…), (?flags) and (?flags:…) groups are allowed")
		}
	}
	inner, err := p.atom()
	if err != nil {
		return size{}, err
	}
	return p.repeat(inner)
}

// group parses a regex and its closing ), the opening part being consumed.
func (p *regexParser) group(start int) (size, error) {
	inner, err := p.regex()
	if err != nil {
		return size{}, err
	}
	if p.peek() != ')' {
		return size{}, p.fail(start, "( without )")
	}
	p.i++
	return inner, nil
}

// noRepeat rejects a repeat after something that is not an atom.
func (p *regexParser) noRepeat(start int) error {
	switch p.peek() {
	case '*', '+', '?', '{':
		return p.fail(start, "nothing to repeat")
	}
	return nil
}

// flags = letters [ "-" letters ] | "-" letters, letters among i, m, s, U,
// each at most once.
func (p *regexParser) flags(start int) error {
	seen := map[rune]bool{}
	letters := func() int {
		n := 0
		for {
			switch r := p.peek(); r {
			case 'i', 'm', 's', 'U':
				if seen[r] {
					return -1
				}
				seen[r] = true
				p.i++
				n++
			default:
				return n
			}
		}
	}
	n := letters()
	if n < 0 {
		return p.fail(start, "a flag appears twice")
	}
	if p.peek() == '-' {
		p.i++
		m := letters()
		if m < 0 {
			return p.fail(start, "a flag appears twice")
		}
		if m == 0 {
			return p.fail(start, "a - in flags must be followed by flags")
		}
		return nil
	}
	if n == 0 {
		return p.fail(start, "unknown group: only the flags i, m, s and U are allowed")
	}
	return nil
}

// atom = char | "." | class | "(" regex ")" | "(?:" regex ")"
func (p *regexParser) atom() (size, error) {
	start := p.i
	switch r := p.peek(); r {
	case '.':
		p.i++
		return size{1, 1}, nil
	case '[':
		w, err := p.class()
		return size{1, w}, err
	case '(':
		if strings.HasPrefix(p.s[p.i:], "(?:") {
			p.i += 3
			return p.group(start)
		}
		// A capturing group adds its two capture states.
		p.i++
		inner, err := p.group(start)
		return size{inner.prod, capWeight(inner.weight + 1)}, err
	case '\\':
		_, prop, err := p.escape(false)
		if prop {
			return size{1, propertyWeight}, err
		}
		return size{1, 1}, err
	case '*', '+', '?':
		return size{}, p.fail(start, "nothing to repeat")
	case '{', '}':
		return size{}, p.fail(start, `a %c outside a repeat must be written \%c`, r, r)
	case ']':
		return size{}, p.fail(start, `a ] outside a class must be written \]`)
	default:
		p.next()
		return size{1, 1}, nil
	}
}

// repeat = ( "*" | "+" | "?" | "{" n "}" | "{" n ",}" | "{" n "," n "}" ) [ "?" ]
// It returns the size of the repeated atom, whose own size is inner: the
// count of the repeat multiplies the chain product and the weight, and a
// repeat that adds a branch (*, +, ?, {n,} and {n,m} with n < m) weighs 1 more.
func (p *regexParser) repeat(inner size) (size, error) {
	start := p.i
	count, weight, branch := 1, inner.weight, 1
	switch p.peek() {
	case '*', '+', '?':
		p.i++
	case '{':
		p.i++
		lo, ok := p.number()
		if !ok {
			return size{}, p.fail(start, `a { must start a repeat {n}, {n,} or {n,m}; write \{ for a {`)
		}
		hi := lo
		if p.peek() == ',' {
			p.i++
			hi = -1
			if p.peek() != '}' {
				if hi, ok = p.number(); !ok {
					return size{}, p.fail(start, `a { must start a repeat {n}, {n,} or {n,m}; write \{ for a {`)
				}
			}
		}
		if p.peek() != '}' {
			return size{}, p.fail(start, `a { must start a repeat {n}, {n,} or {n,m}; write \{ for a {`)
		}
		p.i++
		switch {
		case lo > maxRepeat || hi > maxRepeat:
			return size{}, p.fail(start, "a repeat count is at most %d", maxRepeat)
		case hi >= 0 && lo > hi:
			return size{}, p.fail(start, "in {n,m}, n must not exceed m")
		case hi == 0:
			// An upper bound of 0 ends every chain through this repeat, and
			// the repeated atom weighs nothing.
			inner.prod, count, weight = 1, 1, 0
		case hi > 0:
			count = hi
		case lo > 0:
			count = lo
		}
		if hi == lo {
			branch = 0 // {n} and {n,n} repeat exactly
		}
	default:
		return inner, nil
	}
	if p.peek() == '?' {
		p.i++ // lazy
	}
	switch p.peek() {
	case '*', '+', '?', '{':
		return size{}, p.fail(p.i, "a repeat applies to an atom: write (?:…) around a repeated part")
	}
	prod := inner.prod * count
	if prod > maxRepeat {
		return size{}, p.fail(start, "nested repeats multiply to more than %d", maxRepeat)
	}
	return size{prod, capWeight(branch + weight*count)}, nil
}

// number = "0" | nonzero { digit }, read at most up to 4 digits.
func (p *regexParser) number() (int, bool) {
	start := p.i
	for p.i < len(p.s) && '0' <= p.s[p.i] && p.s[p.i] <= '9' {
		p.i++
	}
	digits := p.s[start:p.i]
	switch {
	case digits == "" || len(digits) > 4:
		return 0, false
	case len(digits) > 1 && digits[0] == '0':
		return 0, false
	}
	n := 0
	for _, d := range digits {
		n = 10*n + int(d-'0')
	}
	return n, true
}

// escapable are the characters a backslash makes literal.
const escapable = `\.+*?()|[]{}^$-`

// escape parses an escape: a literal escape, a control escape, a hex escape
// or a property. It returns the character, or prop true for a property.
func (p *regexParser) escape(inClass bool) (r rune, prop bool, err error) {
	start := p.i
	p.i++ // the backslash
	if p.i >= len(p.s) {
		return 0, false, p.fail(start, "a trailing backslash")
	}
	c := p.s[p.i]
	switch c {
	case 'a', 'f', 't', 'n', 'r', 'v':
		p.i++
		return controlEscape(c), false, nil
	case 'x':
		p.i++
		r, err := p.hex(start)
		return r, false, err
	case 'p', 'P':
		p.i++
		return 0, true, p.property(start)
	case 'd', 'D':
		return 0, false, p.fail(start, `\%c means different things in Go and Rust: write [0-9] or [^0-9]`, c)
	case 'w', 'W':
		return 0, false, p.fail(start, `\%c means different things in Go and Rust: write [0-9A-Za-z_] or [\p{L}\p{N}_]`, c)
	case 's', 'S':
		return 0, false, p.fail(start, `\%c means different things in Go and Rust: write a class such as [ \t]`, c)
	case 'b', 'B':
		return 0, false, p.fail(start, `\%c means different things in Go and Rust, and the common subset has no word boundary`, c)
	case 'Q', 'E':
		return 0, false, p.fail(start, `\Q…\E exists in Go only: escape each character with \`)
	case '<', '>':
		return 0, false, p.fail(start, `\%c is a word boundary in Rust and a literal %c in Go: write %c`, c, c, c)
	case 'A', 'z':
		if inClass {
			return 0, false, p.fail(start, `\%c is not allowed in a class`, c)
		}
	}
	if '0' <= c && c <= '9' {
		return 0, false, p.fail(start, `back-references and octal escapes are not supported: write \x{…}`)
	}
	if strings.IndexByte(escapable, c) >= 0 {
		p.i++
		return rune(c), false, nil
	}
	r, _ = utf8.DecodeRuneInString(p.s[p.i:])
	return 0, false, p.fail(start, `\%c is not an allowed escape`, r)
}

// controlEscape returns the character of \a, \f, \t, \n, \r or \v.
func controlEscape(c byte) rune {
	switch c {
	case 'a':
		return '\a'
	case 'f':
		return '\f'
	case 't':
		return '\t'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	default:
		return '\v'
	}
}

// hex parses the digits of \xHH or \x{H…}, the \x being consumed.
func (p *regexParser) hex(start int) (rune, error) {
	isHex := func(c byte) bool {
		return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
	}
	digits := ""
	if p.peek() == '{' {
		end := strings.IndexByte(p.s[p.i:], '}')
		if end < 0 {
			return 0, p.fail(start, `\x{ without }`)
		}
		digits = p.s[p.i+1 : p.i+end]
		p.i += end + 1
		if digits == "" || len(digits) > 6 {
			return 0, p.fail(start, `\x{…} holds 1 to 6 hexadecimal digits`)
		}
	} else {
		if p.i+2 > len(p.s) {
			return 0, p.fail(start, `\x is followed by two hexadecimal digits`)
		}
		digits = p.s[p.i : p.i+2]
		p.i += 2
	}
	v := rune(0)
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if !isHex(c) {
			return 0, p.fail(start, `\x takes hexadecimal digits`)
		}
		switch {
		case c <= '9':
			v = 16*v + rune(c-'0')
		case c >= 'a':
			v = 16*v + rune(c-'a'+10)
		default:
			v = 16*v + rune(c-'A'+10)
		}
	}
	if !utf8.ValidRune(v) {
		return 0, p.fail(start, `\x{%X} is not a Unicode scalar value`, v)
	}
	return v, nil
}

// property parses the name of \p or \P, the letter being consumed.
func (p *regexParser) property(start int) error {
	if p.i >= len(p.s) {
		return p.fail(start, `\p and \P need a name`)
	}
	if p.s[p.i] != '{' {
		switch p.s[p.i] {
		case 'C', 'L', 'M', 'N', 'P', 'S', 'Z':
			p.i++
			return nil
		}
		return p.fail(start, `\p and \P take one of C L M N P S Z, or a name in braces`)
	}
	end := strings.IndexByte(p.s[p.i:], '}')
	if end < 0 {
		return p.fail(start, `\p{ without }`)
	}
	name := p.s[p.i+1 : p.i+end]
	p.i += end + 1
	if !isCategory(name) && !isScript(name) {
		return p.fail(start, "%q is neither a general category nor a script of Unicode 16.0, spelled exactly", name)
	}
	return nil
}

// class = "[" [ "^" ] item { item } "]"
// It returns the weight of the class: 1 per character or range, and
// propertyWeight per property.
func (p *regexParser) class() (int, error) {
	start := p.i
	p.i++
	if p.peek() == '^' {
		p.i++
	}
	items, weight := 0, 0
	for {
		if p.i >= len(p.s) {
			return 0, p.fail(start, "[ without ]")
		}
		rest := p.s[p.i:]
		switch {
		case rest[0] == ']':
			if items == 0 {
				return 0, p.fail(start, `empty class: write \] for a ]`)
			}
			p.i++
			return capWeight(weight), nil
		case strings.HasPrefix(rest, "--") || strings.HasPrefix(rest, "&&") || strings.HasPrefix(rest, "~~"):
			return 0, p.fail(p.i, "%s in a class is a set operation in Rust", rest[:2])
		case rest[0] == '-':
			if items > 0 && !strings.HasPrefix(rest, "-]") {
				return 0, p.fail(p.i, "a - in a class must mark a range between two characters, or come first or last")
			}
			p.i++
			items, weight = items+1, weight+1
			continue
		}
		lo, prop, err := p.classChar()
		if err != nil {
			return 0, err
		}
		items++
		if prop {
			weight += propertyWeight
			continue
		}
		weight++
		rest = p.s[p.i:]
		if strings.HasPrefix(rest, "--") {
			return 0, p.fail(p.i, "-- in a class is a set operation in Rust")
		}
		if !strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, "-]") {
			continue
		}
		at := p.i
		p.i++
		hi, prop, err := p.classChar()
		if err != nil {
			return 0, err
		}
		if prop {
			return 0, p.fail(at, "a range ends with a character, not a property")
		}
		if hi < lo {
			return 0, p.fail(at, "reversed range")
		}
	}
}

// classChar parses one character or property of a class.
func (p *regexParser) classChar() (r rune, prop bool, err error) {
	switch c := p.peek(); c {
	case -1:
		return 0, false, p.fail(p.i, "[ without ]")
	case '[':
		return 0, false, p.fail(p.i, `a [ in a class must be written \[: nested and POSIX classes are not supported`)
	case '\\':
		return p.escape(true)
	case '-', '&', '~':
		// Two of them in a row are rejected wherever they stand, even after a
		// range end ([!-&&]), so the rule needs no parsing to apply.
		if at := p.i; strings.HasPrefix(p.s[at+1:], string(c)) {
			return 0, false, p.fail(at, "%c%c in a class is a set operation in Rust", c, c)
		}
		return p.next(), false, nil
	default:
		return p.next(), false, nil
	}
}
