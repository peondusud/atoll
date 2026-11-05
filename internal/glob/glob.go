package glob

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/names"
)

// MaxExpansions is the largest number of brace expansions a pattern may have.
// It bounds the memory a pattern from the configuration can take.
const MaxExpansions = 1024

// Pattern is a compiled glob (specs.md §29.5). It is immutable and safe for
// concurrent use.
//
// The syntax:
//
//   - * matches any run of characters except /, the empty run included;
//   - ? matches one character except /;
//   - ** as a whole path component matches zero or more components; ** inside
//     a component is an error;
//   - [abc], [a-z] and [!a] match one character from a class, or outside it
//     for [!…]. A class ends at its first ], cannot be empty and never holds /.
//     A - first or last in a class is literal; anywhere else it marks a range.
//     [^ is reserved; negation is written [!…];
//   - {a,b} are alternatives: at least two, none empty, not nested. Classes
//     are read first, so a , or } in a class is literal; braces are then
//     expanded, every other rule applies to each expansion ({*,x}* expands to
//     **, a whole component), and the pattern matches when one of its
//     expansions does. An alternative may contain / and classes;
//   - the backslash is reserved for a future escape syntax and is an error;
//   - every other character is literal, including ! ] and - outside a class
//     and , outside braces;
//   - names starting with a dot match like any other name;
//   - a pattern has no empty component: no leading or trailing /, and no //.
//
// A character is a Unicode scalar value encoded in valid UTF-8. Each byte of a
// path that is not part of a valid UTF-8 sequence counts as one character,
// which only ?, * and negated classes match. Characters are compared exactly,
// unless the pattern ignores case (Options). The caller puts the pattern and
// the path in NFC form (specs.md §11.3).
type Pattern struct {
	text string
	alts [][]component // one per brace expansion
	fold bool          // Options.IgnoreCase
}

// Options changes how a pattern matches.
type Options struct {
	// IgnoreCase compares characters under Unicode simple case folding,
	// exactly as regex (?i) does (specs.md §11.3): a literal matches its case
	// variants, and a class matches a character when one of its case variants
	// is in the class; [!…] matches the other characters. Folding never
	// rewrites the pattern, so range endpoints keep their meaning: [A-z]
	// still holds _.
	IgnoreCase bool
}

// component is one /-separated part of an expanded pattern.
type component struct {
	globstar bool    // the whole component is **
	toks     []token // when not globstar
}

type tokenKind uint8

const (
	tokLiteral tokenKind = iota // one given character
	tokAny                      // ?
	tokStar                     // *
	tokClass                    // [...]
	tokSep                      // /, only before the pattern is split into components
)

type token struct {
	kind   tokenKind
	r      rune        // tokLiteral
	negate bool        // tokClass
	ranges []runeRange // tokClass
}

type runeRange struct{ lo, hi rune }

// SyntaxError reports an invalid pattern.
type SyntaxError struct {
	Pattern string
	Offset  int // byte offset of the error in Pattern, or -1 when it has none
	Msg     string
}

func (e *SyntaxError) Error() string {
	if e.Offset < 0 {
		return fmt.Sprintf("glob %q: %s", e.Pattern, e.Msg)
	}
	return fmt.Sprintf("glob %q: %s, at byte %d", e.Pattern, e.Msg, e.Offset)
}

// Compile parses a glob pattern that compares characters exactly.
func Compile(pattern string) (*Pattern, error) {
	return CompileWith(pattern, Options{})
}

// CompileWith parses a glob pattern with options.
func CompileWith(pattern string, opts Options) (*Pattern, error) {
	fail := func(offset int, format string, args ...any) error {
		return &SyntaxError{Pattern: pattern, Offset: offset, Msg: fmt.Sprintf(format, args...)}
	}
	if pattern == "" {
		return nil, fail(-1, "empty pattern")
	}
	if !utf8.ValidString(pattern) {
		return nil, fail(-1, "not valid UTF-8")
	}
	pieces, err := tokenize(pattern, fail)
	if err != nil {
		return nil, err
	}
	expansions, err := expand(pieces, fail)
	if err != nil {
		return nil, err
	}
	p := &Pattern{text: pattern, alts: make([][]component, 0, len(expansions)), fold: opts.IgnoreCase}
	for _, toks := range expansions {
		comps, err := split(toks, fail)
		if err != nil {
			return nil, err
		}
		p.alts = append(p.alts, comps)
	}
	return p, nil
}

// String returns the source text of the pattern.
func (p *Pattern) String() string {
	return p.text
}

// piece is a run of tokens outside braces, or a brace group.
type piece struct {
	toks []token   // outside braces
	alts [][]token // a brace group, when non-nil
}

type failFunc func(offset int, format string, args ...any) error

// tokenize turns the pattern into tokens and brace groups.
func tokenize(pattern string, fail failFunc) ([]piece, error) {
	var (
		pieces     []piece
		fixed      []token   // tokens outside braces, not yet in pieces
		alts       [][]token // alternatives of the open group
		cur        []token   // the alternative being read
		groupStart = -1      // offset of the open {, or -1
	)
	add := func(t token) {
		if groupStart >= 0 {
			cur = append(cur, t)
		} else {
			fixed = append(fixed, t)
		}
	}
	for i := 0; i < len(pattern); {
		r, w := utf8.DecodeRuneInString(pattern[i:])
		switch r {
		case '\\':
			return nil, fail(i, "the backslash is reserved")
		case '[':
			t, n, err := parseClass(pattern, i, fail)
			if err != nil {
				return nil, err
			}
			add(t)
			i += n
			continue
		case '{':
			if groupStart >= 0 {
				return nil, fail(i, "braces do not nest")
			}
			if len(fixed) > 0 {
				pieces = append(pieces, piece{toks: fixed})
				fixed = nil
			}
			groupStart, alts, cur = i, nil, nil
		case ',':
			if groupStart < 0 {
				add(token{kind: tokLiteral, r: r})
				break
			}
			if len(cur) == 0 {
				return nil, fail(i, "empty alternative")
			}
			alts, cur = append(alts, cur), nil
		case '}':
			if groupStart < 0 {
				return nil, fail(i, "} without {")
			}
			if len(cur) == 0 {
				return nil, fail(i, "empty alternative")
			}
			alts = append(alts, cur)
			if len(alts) < 2 {
				return nil, fail(groupStart, "braces need at least two alternatives")
			}
			pieces = append(pieces, piece{alts: alts})
			groupStart, alts, cur = -1, nil, nil
		case '*':
			add(token{kind: tokStar})
		case '?':
			add(token{kind: tokAny})
		case '/':
			add(token{kind: tokSep})
		default:
			add(token{kind: tokLiteral, r: r})
		}
		i += w
	}
	if groupStart >= 0 {
		return nil, fail(groupStart, "{ without }")
	}
	if len(fixed) > 0 {
		pieces = append(pieces, piece{toks: fixed})
	}
	return pieces, nil
}

// parseClass parses the class that starts with the [ at pattern[start]. It
// returns the class and its length in bytes.
func parseClass(pattern string, start int, fail failFunc) (token, int, error) {
	t := token{kind: tokClass}
	i := start + 1
	if strings.HasPrefix(pattern[i:], "^") {
		return token{}, 0, fail(i, "[^ is reserved: write [!…] for a negated class")
	}
	if strings.HasPrefix(pattern[i:], "!") {
		t.negate = true
		i++
	}
	// check rejects the characters no class may hold.
	check := func(r rune, at int) error {
		switch r {
		case '/':
			return fail(at, "a class never holds /")
		case '\\':
			return fail(at, "the backslash is reserved")
		}
		return nil
	}
	for {
		if i >= len(pattern) {
			return token{}, 0, fail(start, "[ without ]")
		}
		lo, w := utf8.DecodeRuneInString(pattern[i:])
		if lo == ']' {
			if len(t.ranges) == 0 {
				return token{}, 0, fail(start, "empty class")
			}
			return t, i + 1 - start, nil
		}
		if err := check(lo, i); err != nil {
			return token{}, 0, err
		}
		next := pattern[i+w:]
		if lo == '-' && len(t.ranges) > 0 && !strings.HasPrefix(next, "]") {
			return token{}, 0, fail(i, "a - inside a class must mark a range, or come first or last")
		}
		if strings.HasPrefix(next, "-") && len(next) > 1 && next[1] != ']' {
			hi, w2 := utf8.DecodeRuneInString(next[1:])
			if err := check(hi, i+w+1); err != nil {
				return token{}, 0, err
			}
			if hi < lo {
				return token{}, 0, fail(i, "reversed range %c-%c", lo, hi)
			}
			t.ranges = append(t.ranges, runeRange{lo, hi})
			i += w + 1 + w2
			continue
		}
		t.ranges = append(t.ranges, runeRange{lo, lo})
		i += w
	}
}

// expand returns every expansion of the brace groups, as token lists.
func expand(pieces []piece, fail failFunc) ([][]token, error) {
	n := 1
	for _, pc := range pieces {
		if pc.alts == nil {
			continue
		}
		n *= len(pc.alts)
		if n > MaxExpansions {
			return nil, fail(-1, "braces expand to more than %d patterns", MaxExpansions)
		}
	}
	out := [][]token{nil}
	for _, pc := range pieces {
		if pc.alts == nil {
			for i := range out {
				out[i] = append(out[i], pc.toks...)
			}
			continue
		}
		next := make([][]token, 0, len(out)*len(pc.alts))
		for _, prefix := range out {
			for _, alt := range pc.alts {
				next = append(next, append(append([]token(nil), prefix...), alt...))
			}
		}
		out = next
	}
	return out, nil
}

// split cuts an expansion into its components.
func split(toks []token, fail failFunc) ([]component, error) {
	var comps []component
	start := 0
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && toks[i].kind != tokSep {
			continue
		}
		part := toks[start:i]
		start = i + 1
		switch {
		case len(part) == 0:
			return nil, fail(-1, "empty path component: no leading or trailing /, and no //")
		case len(part) == 2 && part[0].kind == tokStar && part[1].kind == tokStar:
			comps = append(comps, component{globstar: true})
		default:
			for j := 1; j < len(part); j++ {
				if part[j-1].kind == tokStar && part[j].kind == tokStar {
					return nil, fail(-1, "** must be a whole path component")
				}
			}
			comps = append(comps, component{toks: part})
		}
	}
	return comps, nil
}

// Match reports whether the pattern matches path: a whole logical path without
// a leading /, or a file name. path may hold any bytes.
func (p *Pattern) Match(path string) bool {
	parts := strings.Split(path, "/")
	for _, comps := range p.alts {
		if matchComponents(comps, parts, p.fold) {
			return true
		}
	}
	return false
}

// matchComponents matches path components, where a globstar matches zero or
// more of them. Components match independently of each other, so retrying from
// the last globstar only is enough, as for * in matchComponent.
func matchComponents(comps []component, parts []string, fold bool) bool {
	ci, pi := 0, 0
	starCi, starPi := -1, 0
	for {
		if ci < len(comps) {
			c := comps[ci]
			if c.globstar {
				starCi, starPi = ci, pi
				ci++
				continue
			}
			if pi < len(parts) && matchComponent(c.toks, parts[pi], fold) {
				ci++
				pi++
				continue
			}
		} else if pi == len(parts) {
			return true
		}
		// Mismatch: let the last globstar take one more component.
		if starCi < 0 || starPi >= len(parts) {
			return false
		}
		starPi++
		ci, pi = starCi+1, starPi
	}
}

// matchComponent matches one component of a path against tokens that each
// match one character, except * which matches any run.
func matchComponent(toks []token, s string, fold bool) bool {
	ti, si := 0, 0
	starTi, starSi := -1, 0
	for {
		if ti < len(toks) {
			t := toks[ti]
			if t.kind == tokStar {
				starTi, starSi = ti, si
				ti++
				continue
			}
			if si < len(s) {
				r, valid, w := decode(s[si:])
				if t.matches(r, valid, fold) {
					ti++
					si += w
					continue
				}
			}
		} else if si == len(s) {
			return true
		}
		// Mismatch: let the last * take one more character.
		if starTi < 0 || starSi >= len(s) {
			return false
		}
		_, _, w := decode(s[starSi:])
		starSi += w
		ti, si = starTi+1, starSi
	}
}

// holds reports whether r lies in one of the ranges of a class token.
func (t token) holds(r rune) bool {
	for _, rg := range t.ranges {
		if rg.lo <= r && r <= rg.hi {
			return true
		}
	}
	return false
}

// decode returns the first character of a non-empty s and its length. valid
// is false for a byte that is not part of a valid UTF-8 sequence.
func decode(s string) (r rune, valid bool, size int) {
	r, size = utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return 0, false, 1
	}
	return r, true, size
}

// matches reports whether a single-character token matches a character. With
// fold, characters compare under Unicode simple case folding.
func (t token) matches(r rune, valid, fold bool) bool {
	switch t.kind {
	case tokLiteral:
		return valid && (r == t.r || fold && names.FoldEqual(r, t.r))
	case tokAny:
		return true
	case tokClass:
		if !valid {
			return t.negate
		}
		in := t.holds(r)
		if !in && fold {
			in = names.FoldAny(r, t.holds)
		}
		return in != t.negate
	case tokStar, tokSep:
		return false // never asked: * and / are handled by the callers
	default:
		return false
	}
}
