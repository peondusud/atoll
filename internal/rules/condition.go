package rules

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/glob"
	"github.com/peondusud/atoll/internal/names"
)

// RegexPrefix marks a path or filename value as a regex (specs.md §29.5).
const RegexPrefix = "re:"

// Field is what a condition looks at (specs.md §11.1).
type Field uint8

const (
	FieldPath     Field = iota // the whole logical path
	FieldFilename              // the file name, the last component of the path
	FieldExt                   // one entry of ext, matched against the file name
)

func (f Field) String() string {
	switch f {
	case FieldPath:
		return "path"
	case FieldFilename:
		return "filename"
	case FieldExt:
		return "ext"
	default:
		return fmt.Sprintf("Field(%d)", uint8(f))
	}
}

// Subject is a file as conditions see it: its logical path and its file name,
// in NFC form run by run (specs.md §11.3). Build it once per file and match it
// against every condition.
type Subject struct {
	path string
	name string
}

// NewSubject returns the subject for a logical path, given without a leading
// /. The path may hold any bytes.
func NewSubject(logicalPath string) Subject {
	// NFC never composes across /, so the file name of the NFC path is the
	// NFC form of the file name.
	p := names.NFC(logicalPath)
	return Subject{path: p, name: p[strings.LastIndexByte(p, '/')+1:]}
}

// Condition is a compiled path, filename or ext condition. It is immutable and
// safe for concurrent use.
type Condition struct {
	field Field
	glob  *glob.Pattern
	re    *regexp.Regexp
	ext   []rune // for FieldExt: "." followed by the entry, in NFC form
	fold  bool
}

// Compile compiles one condition. value is a glob, or a regex after re:, for
// FieldPath and FieldFilename, and one entry of the ext list for FieldExt.
// ignoreCase is the rule's ignore_case.
func Compile(field Field, value string, ignoreCase bool) (*Condition, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%s %q: not valid UTF-8", field, value)
	}
	value = names.NFC(value)
	c := &Condition{field: field, fold: ignoreCase}
	switch field {
	case FieldPath, FieldFilename:
		if text, ok := strings.CutPrefix(value, RegexPrefix); ok {
			re, err := compileRegex(text, ignoreCase)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", field, value, err)
			}
			c.re = re
			return c, nil
		}
		g, err := glob.CompileWith(value, glob.Options{IgnoreCase: ignoreCase})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		c.glob = g
		return c, nil
	case FieldExt:
		switch {
		case value == "":
			return nil, fmt.Errorf("ext %q: empty extension", value)
		case strings.HasPrefix(value, "."):
			return nil, fmt.Errorf("ext %q: write the extension without the dot", value)
		case strings.Contains(value, "/"):
			return nil, fmt.Errorf("ext %q: an extension never holds /", value)
		}
		c.ext = []rune("." + value)
		return c, nil
	default:
		return nil, fmt.Errorf("unknown condition field %d", uint8(field))
	}
}

// Match reports whether the condition holds for a file.
func (c *Condition) Match(s Subject) bool {
	switch c.field {
	case FieldPath:
		return c.matchPattern(s.path)
	case FieldFilename:
		return c.matchPattern(s.name)
	case FieldExt:
		return c.matchExt(s.name)
	default:
		return false
	}
}

func (c *Condition) matchPattern(s string) bool {
	if c.re != nil {
		// regexp reads each byte that is not valid UTF-8 as one U+FFFD,
		// which is the rule of specs.md §11.3.
		return c.re.MatchString(s)
	}
	return c.glob.Match(s)
}

// matchExt reports whether name ends with "." and the entry, exactly as the
// glob "*." followed by the entry would: characters are decoded from the
// start, as the glob matcher does, and a byte that is not valid UTF-8 never
// equals a character of the entry.
func (c *Condition) matchExt(name string) bool {
	total := 0
	for i := 0; i < len(name); total++ {
		_, _, w := decode(name[i:])
		i += w
	}
	if total < len(c.ext) {
		return false
	}
	i := 0
	for range total - len(c.ext) {
		_, _, w := decode(name[i:])
		i += w
	}
	for _, want := range c.ext {
		r, valid, w := decode(name[i:])
		same := r == want || c.fold && names.FoldEqual(r, want)
		if !valid || !same {
			return false
		}
		i += w
	}
	return true
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

// compileRegex compiles the text of a re: value, after checking that it stays
// within the common subset of Go regexp and Rust regex (specs.md §29.5).
// ignore_case only adds (?i): neither the pattern nor the subject is ever
// case-folded, so (?-i:…) keeps its meaning.
func compileRegex(text string, ignoreCase bool) (*regexp.Regexp, error) {
	if err := checkRegexSubset(text); err != nil {
		return nil, err
	}
	if ignoreCase {
		text = "(?i)" + text
	}
	re, err := regexp.Compile(text)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	return re, nil
}

// checkRegexSubset rejects the escapes whose meaning differs between Go and
// Rust: \d, \w, \s and \b are ASCII-only in Go and Unicode in Rust, and so
// are their negations; \Q…\E exists in Go only.
func checkRegexSubset(text string) error {
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' || i+1 == len(text) {
			continue
		}
		i++
		switch c := text[i]; c {
		case 'd', 'D', 'w', 'W', 's', 'S', 'b', 'B':
			return fmt.Errorf(`\%c is not allowed: it means different things in Go and Rust (write [0-9] or \p{L} instead)`, c)
		case 'Q', 'E':
			return fmt.Errorf(`\%c is not allowed: \Q…\E exists in Go only`, c)
		}
	}
	return nil
}
