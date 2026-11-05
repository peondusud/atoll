package rules

import (
	"errors"
	"regexp"
	"strings"
	"testing"

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
