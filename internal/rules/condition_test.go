package rules

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/names"

	"github.com/peondusud/atoll/internal/glob"
	"github.com/peondusud/atoll/internal/vectors"
)

type matchVector struct {
	Condition  string    `json:"condition"`
	Value      *string   `json:"value"`
	Values     *[]string `json:"values"`
	IgnoreCase *bool     `json:"ignore_case"`
	Path       *string   `json:"path"`
	PathB64    *string   `json:"path_b64"`
	Match      *bool     `json:"match"`
	Error      bool      `json:"error"`
	Note       string    `json:"note"`
}

func parseField(s string) (Field, error) {
	for _, f := range []Field{FieldPath, FieldFilename, FieldExt} {
		if f.String() == s {
			return f, nil
		}
	}
	return 0, fmt.Errorf("unknown condition %q", s)
}

// compileVector compiles the condition of a vector: value for one pattern or
// ext entry, values for an ext list. It returns a label for messages.
func compileVector(v matchVector) (*Condition, string, error, error) {
	field, err := parseField(v.Condition)
	if err != nil {
		return nil, "", nil, err
	}
	if v.IgnoreCase == nil {
		return nil, "", nil, fmt.Errorf("ignore_case is required")
	}
	switch {
	case v.Values != nil && v.Value == nil && field == FieldExt:
		label := fmt.Sprintf("ext %q", *v.Values)
		c, cerr := CompileExt(*v.Values, *v.IgnoreCase)
		return c, label, cerr, nil
	case v.Value != nil && v.Values == nil:
		label := fmt.Sprintf("%s %q", v.Condition, *v.Value)
		c, cerr := Compile(field, *v.Value, *v.IgnoreCase)
		return c, label, cerr, nil
	default:
		return nil, "", nil, fmt.Errorf("want value, or values for ext")
	}
}

func TestMatchVectors(t *testing.T) {
	for i, v := range vectors.Load[matchVector](t, "match.json") {
		c, label, err, verr := compileVector(v)
		if verr != nil {
			t.Errorf("vector %d: %v", i, verr)
			continue
		}
		if v.Error {
			if v.Match != nil || v.Path != nil || v.PathB64 != nil {
				t.Errorf("vector %s: an error case has no path and no match", label)
			}
			if err == nil {
				t.Errorf("%s accepted, want an error", label)
			}
			continue
		}
		if v.Match == nil {
			t.Errorf("vector %s: want match or error", label)
			continue
		}
		path, perr := vectors.Path(v.Path, v.PathB64)
		if perr != nil {
			t.Errorf("vector %s: %v", label, perr)
			continue
		}
		if err != nil {
			t.Errorf("%s (ignore_case %v): %v", label, *v.IgnoreCase, err)
			continue
		}
		if got := c.Match(NewSubject(path)); got != *v.Match {
			t.Errorf("%s (ignore_case %v) on %q = %v, want %v", label, *v.IgnoreCase, path, got, *v.Match)
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

// No vector may hold a run long enough for x/text to insert U+034F, where Go
// and Rust differ (specs.md §29.5).
func TestVectorsAvoidStreamSafeInsertion(t *testing.T) {
	check := func(file, s string) {
		if strings.Count(names.NFC(s), "\u034f") != strings.Count(s, "\u034f") {
			t.Errorf("%s: NFC(%q) inserts U+034F", file, s)
		}
	}
	for _, v := range vectors.Load[matchVector](t, "match.json") {
		if v.Value != nil {
			check("match.json", *v.Value)
		}
		if v.Values != nil {
			for _, e := range *v.Values {
				check("match.json", e)
			}
		}
		if path, err := vectors.Path(v.Path, v.PathB64); err == nil {
			check("match.json", path)
		}
	}
	for _, v := range vectors.Load[regexVector](t, "regex.json") {
		check("regex.json", v.Pattern)
		if path, err := vectors.Path(v.Path, v.PathB64); err == nil {
			check("regex.json", path)
		}
	}
}
