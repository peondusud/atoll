package rules

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/glob"
	"github.com/peondusud/atoll/internal/vectors"
)

type matchVector struct {
	Condition  string  `json:"condition"`
	Value      string  `json:"value"`
	IgnoreCase *bool   `json:"ignore_case"`
	Path       *string `json:"path"`
	PathB64    *string `json:"path_b64"`
	Match      *bool   `json:"match"`
	Error      bool    `json:"error"`
	Note       string  `json:"note"`
}

func parseField(s string) (Field, error) {
	for _, f := range []Field{FieldPath, FieldFilename, FieldExt} {
		if f.String() == s {
			return f, nil
		}
	}
	return 0, fmt.Errorf("unknown condition %q", s)
}

func TestMatchVectors(t *testing.T) {
	for _, v := range vectors.Load[matchVector](t, "match.json") {
		field, err := parseField(v.Condition)
		if err != nil {
			t.Errorf("vector %q: %v", v.Value, err)
			continue
		}
		if v.IgnoreCase == nil {
			t.Errorf("vector %s %q: ignore_case is required", v.Condition, v.Value)
			continue
		}
		c, err := Compile(field, v.Value, *v.IgnoreCase)
		if v.Error {
			if v.Match != nil || v.Path != nil || v.PathB64 != nil {
				t.Errorf("vector %s %q: an error case has no path and no match", v.Condition, v.Value)
			}
			if err == nil {
				t.Errorf("Compile(%s, %q) succeeded, want an error", v.Condition, v.Value)
			}
			continue
		}
		if v.Match == nil {
			t.Errorf("vector %s %q: want match or error", v.Condition, v.Value)
			continue
		}
		path, perr := vectors.Path(v.Path, v.PathB64)
		if perr != nil {
			t.Errorf("vector %s %q: %v", v.Condition, v.Value, perr)
			continue
		}
		if err != nil {
			t.Errorf("Compile(%s, %q, %v): %v", v.Condition, v.Value, *v.IgnoreCase, err)
			continue
		}
		if got := c.Match(NewSubject(path)); got != *v.Match {
			t.Errorf("%s %q (ignore_case %v) on %q = %v, want %v", v.Condition, v.Value, *v.IgnoreCase, path, got, *v.Match)
		}
	}
}

// An ext entry holds exactly when the glob "*." followed by the entry holds on
// the file name (specs.md §11.3), for entries without glob syntax.
func TestExtIsGlob(t *testing.T) {
	entries := []string{"mkv", "MKV", "tar.gz", "srt", "nfo", "ß", "Σ", "é"}
	paths := []string{
		"a.mkv", "a.MKV", "a.mKv", "a.mKv", ".mkv", "mkv", "a.mkv.part",
		"x.tar.gz", "x.TAR.GZ", "a.ſrt", ".nfo", "a.ẞ", "a.ς", "a.é",
		"Films/a.mkv", "a\xff.mkv", "a.\xffmkv", "a.mk\xe2\x82", "",
	}
	for _, e := range entries {
		for _, fold := range []bool{false, true} {
			c, err := Compile(FieldExt, e, fold)
			if err != nil {
				t.Fatal(err)
			}
			g, err := Compile(FieldFilename, "*."+e, fold)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range paths {
				s := NewSubject(p)
				if got, want := c.Match(s), g.Match(s); got != want {
					t.Errorf("ext %q (fold %v) on %q = %v, glob *.%s says %v", e, fold, p, got, e, want)
				}
			}
		}
	}
}

// A glob and the equivalent regex agree under ignore_case on valid UTF-8, by
// construction (specs.md §29.5). The test translates glob literals and
// classes into a regex and compares both on paths made of tricky characters.
func TestGlobAgreesWithRegex(t *testing.T) {
	alphabet := []rune{
		'a', 'A', 'k', 'K', 's', 'S', 'z', 'Z', '_', '×', '÷', 'é', 'É', 'ÿ', 'Ÿ',
		0x017F, 0x212A, 0x00DF, 0x1E9E, 0x03C3, 0x03C2, 0x03A3, 0x00B5, 0x03BC,
		0x0130, 0x0131, 'i', 'I', 0x0345, 0x03B9, 0x01C5, 0x01C4, 0x01C6,
		0x10400, 0x10428,
	}
	tokens := []struct{ glob, re string }{
		{"s", "s"}, {"K", "K"}, {"ß", "ß"}, {"Σ", "Σ"}, {"ǅ", "ǅ"}, {"ι", "ι"},
		{"?", "[^/]"}, {"*", "[^/]*"},
		{"[a-z]", "[a-z]"}, {"[!a-z]", "[^a-z]"}, {"[A-z]", "[A-z]"},
		{"[Z-a]", "[Z-a]"}, {"[!k]", "[^k]"}, {"[!s]", "[^s]"},
		{"[À-Þ]", "[À-Þ]"}, {"[à-ÿ]", "[à-ÿ]"}, {"[!ß]", "[^ß]"},
		{"[σ]", "[σ]"}, {"[İı]", "[İı]"}, {"[𐐀-𐐏]", "[𐐀-𐐏]"},
	}
	n := 0
	// Every pattern of one or two tokens, against every path of one or two
	// characters of the alphabet.
	for _, a := range tokens {
		for _, b := range append([]struct{ glob, re string }{{"", ""}}, tokens...) {
			gp := a.glob + b.glob
			g, err := glob.CompileWith(gp, glob.Options{IgnoreCase: true})
			if err != nil {
				t.Fatalf("glob %q: %v", gp, err)
			}
			re := regexp.MustCompile("^(?i)" + a.re + b.re + "$")
			for _, x := range alphabet {
				for _, y := range append([]rune{-1}, alphabet...) {
					var sb strings.Builder
					sb.WriteRune(x)
					if y >= 0 {
						sb.WriteRune(y)
					}
					s := sb.String()
					n++
					if got, want := g.Match(s), re.MatchString(s); got != want {
						t.Errorf("glob %q on %q = %v, regex %q says %v", gp, s, got, re, want)
					}
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no case checked")
	}
}

func TestSubject(t *testing.T) {
	s := NewSubject("Films/Séries/E01.mkv")
	if s.path != "Films/Séries/E01.mkv" || s.name != "E01.mkv" {
		t.Errorf("NewSubject = %+v", s)
	}
	if s := NewSubject("a.mkv"); s.name != "a.mkv" {
		t.Errorf("NewSubject(a.mkv).name = %q", s.name)
	}
}
