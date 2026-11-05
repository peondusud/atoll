package glob

import (
	"errors"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type vector struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Match   *bool   `json:"match"`
	Error   bool    `json:"error"`
	Note    string  `json:"note"`
}

func TestVectors(t *testing.T) {
	for _, v := range vectors.Load[vector](t, "glob.json") {
		p, err := Compile(v.Pattern)
		if v.Error {
			if v.Match != nil || v.Path != nil || v.PathB64 != nil {
				t.Errorf("vector %q: an error case has no path and no match", v.Pattern)
			}
			if err == nil {
				t.Errorf("Compile(%q) succeeded, want an error", v.Pattern)
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
			t.Errorf("Compile(%q): %v", v.Pattern, err)
			continue
		}
		if got := p.Match(path); got != *v.Match {
			t.Errorf("Compile(%q).Match(%q) = %v, want %v", v.Pattern, path, got, *v.Match)
		}
		if got := naiveMatch(p, path); got != *v.Match {
			t.Errorf("naive match of %q against %q = %v, want %v", v.Pattern, path, got, *v.Match)
		}
	}
}

func TestSyntaxError(t *testing.T) {
	_, err := Compile("Films/[a")
	var se *SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("Compile: %v, want a *SyntaxError", err)
	}
	if se.Offset != len("Films/") {
		t.Errorf("Offset = %d, want %d", se.Offset, len("Films/"))
	}
	if !strings.Contains(err.Error(), "at byte 6") {
		t.Errorf("Error() = %q, want the offset", err.Error())
	}
}

// Patterns come from the configuration, which is valid UTF-8; Compile checks
// it anyway rather than read invalid bytes as U+FFFD.
func TestCompileRejectsInvalidUTF8(t *testing.T) {
	if _, err := Compile("a\xffb"); err == nil {
		t.Error("Compile accepted a pattern that is not valid UTF-8")
	}
}

func TestString(t *testing.T) {
	const s = "{Films,Séries}/**"
	p, err := Compile(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != s {
		t.Errorf("String() = %q, want %q", p.String(), s)
	}
}

// The matcher retries from the last * or globstar only, which keeps it linear
// per expansion. A long path against many stars must stay fast.
func TestNoExponentialBacktracking(t *testing.T) {
	p, err := Compile(strings.Repeat("*a", 30) + "b/" + strings.Repeat("**/", 30) + "c")
	if err != nil {
		t.Fatal(err)
	}
	path := strings.Repeat("a", 5000) + "/" + strings.Repeat("x/", 2000) + "d"
	if p.Match(path) {
		t.Error("unexpected match")
	}
}

// FuzzMatch checks the matcher against a naive recursive one, which tries every
// split and is easy to trust. Memoization keeps it polynomial.
func FuzzMatch(f *testing.F) {
	for _, s := range []struct {
		pattern, path string
		fold          bool
	}{
		{"Films/**", "Films/a/b.mkv", false},
		{"*a*b*", "xaybz", false},
		{"a/**/b/*c", "a/x/b/yc", false},
		{"{poster,fanart}.{jpg,png}", "fanart.png", false},
		{"[!a-c]?*", "d\xffe", false},
		{"**/*[[]1080p]*", "x/Movie [1080p].mkv", false},
		{"[!s]*ß", "\u017fxẞ", true},
		{"*[a-z]K", "_\u017f\u212a", true},
	} {
		f.Add(s.pattern, s.path, s.fold)
	}
	f.Fuzz(func(t *testing.T, pattern, path string, fold bool) {
		// Long inputs find no more logic bugs, and matching costs up to
		// expansions × pattern length × path length.
		if len(pattern) > 128 || len(path) > 256 {
			return
		}
		p, err := CompileWith(pattern, Options{IgnoreCase: fold})
		if err != nil {
			return
		}
		if got, want := p.Match(path), naiveMatch(p, path); got != want {
			t.Errorf("Compile(%q).Match(%q) = %v, naive match = %v", pattern, path, got, want)
		}
	})
}

// memoKey identifies a subproblem by the lengths of the remaining suffixes.
type memoKey struct{ a, b int }

func naiveMatch(p *Pattern, path string) bool {
	parts := strings.Split(path, "/")
	for _, comps := range p.alts {
		if naiveComponents(comps, parts, p.fold, map[memoKey]bool{}) {
			return true
		}
	}
	return false
}

func naiveComponents(comps []component, parts []string, fold bool, memo map[memoKey]bool) bool {
	key := memoKey{len(comps), len(parts)}
	if v, ok := memo[key]; ok {
		return v
	}
	var v bool
	switch {
	case len(comps) == 0:
		v = len(parts) == 0
	case comps[0].globstar:
		for k := 0; k <= len(parts) && !v; k++ {
			v = naiveComponents(comps[1:], parts[k:], fold, memo)
		}
	default:
		v = len(parts) > 0 &&
			naiveComponent(comps[0].toks, parts[0], fold, map[memoKey]bool{}) &&
			naiveComponents(comps[1:], parts[1:], fold, memo)
	}
	memo[key] = v
	return v
}

func naiveComponent(toks []token, s string, fold bool, memo map[memoKey]bool) bool {
	key := memoKey{len(toks), len(s)}
	if v, ok := memo[key]; ok {
		return v
	}
	var v bool
	switch {
	case len(toks) == 0:
		v = s == ""
	case toks[0].kind == tokStar:
		for i := 0; ; {
			if v = naiveComponent(toks[1:], s[i:], fold, memo); v || i == len(s) {
				break
			}
			_, _, w := decode(s[i:])
			i += w
		}
	case s == "":
		v = false
	default:
		r, valid, w := decode(s)
		v = toks[0].matches(r, valid, fold) && naiveComponent(toks[1:], s[w:], fold, memo)
	}
	memo[key] = v
	return v
}
