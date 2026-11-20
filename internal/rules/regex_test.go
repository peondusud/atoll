package rules

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/vectors"
)

type regexVector struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Match   *bool   `json:"match"`
	Error   bool    `json:"error"`
	Note    string  `json:"note"`
}

func TestRegexVectors(t *testing.T) {
	for _, v := range vectors.Load[regexVector](t, "regex.json") {
		c, err := Compile(FieldPath, RegexPrefix+v.Pattern, false)
		if v.Error {
			if v.Match != nil || v.Path != nil || v.PathB64 != nil {
				t.Errorf("vector %q: an error case has no path and no match", v.Pattern)
			}
			if err == nil {
				t.Errorf("regex %q accepted, want an error", v.Pattern)
			}
			continue
		}
		if v.Match == nil {
			t.Errorf("vector %q: want match or error", v.Pattern)
			continue
		}
		path, perr := vectors.Path(v.Path, v.PathB64)
		if perr != nil {
			t.Errorf("vector %q: %v", v.Pattern, perr)
			continue
		}
		if err != nil {
			t.Errorf("regex %q: %v", v.Pattern, err)
			continue
		}
		if got := c.Match(NewSubject(path)); got != *v.Match {
			t.Errorf("regex %q on %q = %v, want %v", v.Pattern, path, got, *v.Match)
		}
	}
}

func TestRegexError(t *testing.T) {
	_, err := Compile(FieldFilename, `re:^a\d`, true)
	var re *RegexError
	if !errors.As(err, &re) {
		t.Fatalf("Compile: %v, want a *RegexError", err)
	}
	if re.Offset != 2 {
		t.Errorf("Offset = %d, want 2", re.Offset)
	}
	// The message quotes the user's text, never the (?i) that ignore_case adds.
	if strings.Contains(err.Error(), "(?i)") {
		t.Errorf("Error() = %q mentions (?i)", err.Error())
	}

	// The grammar applies to the NFC form: n and U+0301 compose into ń. The
	// message quotes the text as typed, then the NFC form it checked.
	typed := "re:\\n\u0301"
	_, err = Compile(FieldPath, typed, false)
	if !errors.As(err, &re) {
		t.Fatalf("Compile(%q): %v, want a *RegexError", typed, err)
	}
	if want := strconv.QuoteToASCII(typed); !strings.Contains(err.Error(), want) {
		t.Errorf("Error() = %q, want it to quote the typed text %s", err.Error(), want)
	}
	if re.Regex != "\\\u0144" || re.Offset != 0 {
		t.Errorf("RegexError = %q at %d, want %q at 0", re.Regex, re.Offset, "\\\u0144")
	}
}

// Every regex the vectors reject, the grammar rejects itself, before the engine compiles it (specs.md §29.5): Go's
// regexp would refuse many of them anyway, Rust's regex accepts more, and both versions check the same grammar.
func TestRegexVectorsTheGrammarRejects(t *testing.T) {
	checked := 0
	for _, v := range vectors.Load[regexVector](t, "regex.json") {
		if !v.Error || !utf8.ValidString(v.Pattern) || names.NonStarterRun(v.Pattern) > names.MaxNonStarters {
			continue // refused before the grammar is checked
		}
		checked++
		text := names.NFC(v.Pattern)
		var re *RegexError
		if err := checkRegex(text); !errors.As(err, &re) {
			t.Errorf("regex %q: the grammar says %v, want a *RegexError", v.Pattern, err)
		}
	}
	if checked == 0 {
		t.Fatal("no error case checked")
	}
}

// A regex outside the grammar is refused with where and why (specs.md §29.5): the byte offset of what is wrong, and
// a message that names it.
func TestRegexErrorsSayWhereAndWhy(t *testing.T) {
	for _, c := range []struct {
		pattern string
		offset  int
		says    string
	}{
		{`(?ii)`, 0, "a flag appears twice"},
		{`a(?i-i)`, 1, "a flag appears twice"},
		{`(?i-)`, 0, "a - in flags must be followed by flags"},
		{`(?)`, 0, "unknown group"},
		{`(?i`, 0, "only (?:…), (?flags) and (?flags:…) groups"},
		{`\0`, 0, "back-references and octal escapes"},
		{`a\9`, 1, "back-references and octal escapes"},
		{`\e`, 0, `\e is not an allowed escape`},
		{`(?:a{10}){101}`, 9, "nested repeats multiply to more than 1000"},
		{`a{500}b{501}`, 0, "weighs more than 1000"},
		{`a{1001}`, 1, "a repeat count is at most 1000"},
		{`a{1001,}`, 1, "a repeat count is at most 1000"},
		{`a{0,1001}`, 1, "a repeat count is at most 1000"},
		{`a{3,2}`, 1, "n must not exceed m"},
		{`a{2}{3}`, 4, "a repeat applies to an atom"},
		{`[!-&&]`, 3, "&& in a class is a set operation in Rust"},
		{`[--a]`, 1, "-- in a class is a set operation in Rust"},
		{`[a~~]`, 2, "~~ in a class is a set operation in Rust"},
		{`[b-a]`, 2, "reversed range"},
		{`[a-\pL]`, 2, "a range ends with a character, not a property"},
		{`\x{}`, 0, `\x{…} holds 1 to 6 hexadecimal digits`},
		{`\x{1234567}`, 0, `\x{…} holds 1 to 6 hexadecimal digits`},
		{`\x{12`, 0, `\x{ without }`},
		{`a\x1`, 1, `\x is followed by two hexadecimal digits`},
		{`\xZ1`, 0, `\x takes hexadecimal digits`},
		{`\x{D800}`, 0, "not a Unicode scalar value"},
		{`\p{greek}`, 0, "neither a general category nor a script"},
		{`\pX`, 0, `\p and \P take one of C L M N P S Z`},
		{`(a`, 0, "( without )"},
		{`a)`, 1, ") without ("},
		{`[]`, 0, "empty class"},
		{`[a`, 0, "[ without ]"},
		{`[a-`, 3, "[ without ]"},
	} {
		err := checkRegex(c.pattern)
		var re *RegexError
		if !errors.As(err, &re) {
			t.Errorf("regex %q: %v, want a *RegexError", c.pattern, err)
			continue
		}
		if re.Offset != c.offset || !strings.Contains(re.Msg, c.says) {
			t.Errorf("regex %q: %q at byte %d, want %q at byte %d", c.pattern, re.Msg, re.Offset, c.says, c.offset)
		}
	}
}

// FuzzRegexGrammar checks that every regex the grammar accepts compiles in Go,
// with and without (?i): the grammar never accepts what regexp rejects.
func FuzzRegexGrammar(f *testing.F) {
	for _, v := range []string{
		`^Films/`, `(a|b)c`, `(?i:a)b`, `a{2,3}?`, `[\p{Greek}a-z\-]`, `\x{1F600}`,
		`(a{10}){100}`, `\{tmdb-[0-9]+\}`, `(?im-s:a.b)`, `[^\]]+`, `\A\pL*\z`,
	} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if checkRegex(text) != nil {
			return
		}
		if _, err := regexp.Compile(text); err != nil {
			t.Errorf("grammar accepts %q, regexp rejects it: %v", text, err)
		}
		if _, err := regexp.Compile("(?i)" + text); err != nil {
			t.Errorf("grammar accepts %q, regexp rejects (?i) with it: %v", text, err)
		}
	})
}
