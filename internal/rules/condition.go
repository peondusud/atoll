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
	exts  [][]rune // for FieldExt: "." followed by each entry, in NFC form
	fold  bool
}

// Compile compiles a path or filename condition: value is a glob, or a regex
// after re:. For FieldExt, value is a single entry, as CompileExt with one
// entry. ignoreCase is the rule's ignore_case.
func Compile(field Field, value string, ignoreCase bool) (*Condition, error) {
	if field == FieldExt {
		return CompileExt([]string{value}, ignoreCase)
	}
	if field != FieldPath && field != FieldFilename {
		return nil, fmt.Errorf("unknown condition field %d", uint8(field))
	}
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%s %q: not valid UTF-8", field, value)
	}
	value = names.NFC(value)
	c := &Condition{field: field, fold: ignoreCase}
	if text, ok := strings.CutPrefix(value, RegexPrefix); ok {
		re, err := compileRegex(text, ignoreCase)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		c.re = re
		return c, nil
	}
	if field == FieldFilename && strings.Contains(value, "/") {
		return nil, fmt.Errorf("filename %q: a file name never holds /: use path", value)
	}
	g, err := glob.CompileWith(value, glob.Options{IgnoreCase: ignoreCase})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	c.glob = g
	return c, nil
}

// CompileExt compiles an ext condition, which holds when the file name ends
// with "." followed by one of the entries (specs.md §11.3).
func CompileExt(entries []string, ignoreCase bool) (*Condition, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("ext: empty list")
	}
	c := &Condition{field: FieldExt, fold: ignoreCase, exts: make([][]rune, 0, len(entries))}
	for _, e := range entries {
		switch {
		case !utf8.ValidString(e):
			return nil, fmt.Errorf("ext %q: not valid UTF-8", e)
		case e == "":
			return nil, fmt.Errorf("ext %q: empty extension", e)
		case strings.HasPrefix(e, "."):
			return nil, fmt.Errorf("ext %q: write the extension without the dot", e)
		case strings.Contains(e, "/"):
			return nil, fmt.Errorf("ext %q: an extension never holds /", e)
		}
		c.exts = append(c.exts, []rune("."+names.NFC(e)))
	}
	return c, nil
}

// Match reports whether the condition holds for a file.
func (c *Condition) Match(s Subject) bool {
	switch c.field {
	case FieldPath:
		return c.matchPattern(s.path)
	case FieldFilename:
		return c.matchPattern(s.name)
	case FieldExt:
		for _, ext := range c.exts {
			if c.endsWith(s.name, ext) {
				return true
			}
		}
		return false
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

// endsWith reports whether name ends with the characters of suffix, compared
// as the glob "*" followed by the same literal characters would compare them:
// characters are decoded from the start, as the glob matcher does, and a byte
// that is not valid UTF-8 never equals a character of the suffix.
func (c *Condition) endsWith(name string, suffix []rune) bool {
	total := 0
	for i := 0; i < len(name); total++ {
		_, _, w := decode(name[i:])
		i += w
	}
	if total < len(suffix) {
		return false
	}
	i := 0
	for range total - len(suffix) {
		_, _, w := decode(name[i:])
		i += w
	}
	for _, want := range suffix {
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

// compileRegex compiles the text of a re: value, in NFC form, after checking
// that it follows the grammar of specs.md §29.5. ignore_case only adds (?i):
// neither the pattern nor the subject is ever case-folded, so (?-i:…) keeps
// its meaning.
func compileRegex(text string, ignoreCase bool) (*regexp.Regexp, error) {
	if err := checkRegex(text); err != nil {
		return nil, err
	}
	// The grammar keeps to what regexp accepts; compiling the text as written
	// first keeps any error message about the user's own text.
	re, err := regexp.Compile(text)
	if err != nil {
		return nil, fmt.Errorf("regex %q: %w", text, err)
	}
	if !ignoreCase {
		return re, nil
	}
	re, err = regexp.Compile("(?i)" + text)
	if err != nil {
		return nil, fmt.Errorf("regex %q: %w", text, err)
	}
	return re, nil
}
