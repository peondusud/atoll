package names

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/vectors"
)

// shownVector is a case of conformance/vectors/shown_name.json: a name and what is written for it, or a range of the
// format characters.
type shownVector struct {
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Shown   *string `json:"shown"`
	From    *int64  `json:"from"`
	To      *int64  `json:"to"`
	Note    string  `json:"note"`
}

// shownName is a name of the vector and what is written for it.
type shownName struct {
	name, shown string
}

// shownCases splits the vector's cases into its names and its ranges of format characters, each in the vector's
// order, or fails t.
func shownCases(t testing.TB) (names []shownName, ranges [][2]rune) {
	t.Helper()
	seen := map[string]bool{}
	for i, v := range vectors.Load[shownVector](t, "shown_name.json") {
		if v.From != nil || v.To != nil {
			switch {
			case v.From == nil || v.To == nil || v.Path != nil || v.PathB64 != nil || v.Shown != nil:
				t.Fatalf("case %d: a range has from and to, and nothing else", i)
			case *v.From < 0 || *v.From > *v.To || *v.To > unicode.MaxRune:
				t.Fatalf("case %d: %d to %d is no range of code points", i, *v.From, *v.To)
			case len(ranges) > 0 && rune(*v.From) <= ranges[len(ranges)-1][1]+1:
				t.Fatalf("case %d: the ranges go in order, and two that touch are one", i)
			}
			ranges = append(ranges, [2]rune{rune(*v.From), rune(*v.To)})
			continue
		}
		name, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if v.Shown == nil {
			t.Fatalf("case %d, %q: no shown", i, name)
		}
		if seen[name] {
			t.Fatalf("case %d, %q: twice", i, name)
		}
		seen[name] = true
		names = append(names, shownName{name, *v.Shown})
	}
	if len(names) == 0 || len(ranges) == 0 {
		t.Fatalf("%d names and %d ranges", len(names), len(ranges))
	}
	return names, ranges
}

func TestShownNameVectors(t *testing.T) {
	names, _ := shownCases(t)
	for _, c := range names {
		if got := Shown(c.name); got != c.shown {
			t.Errorf("Shown(%+q) = %+q, want %+q", c.name, got, c.shown)
		}
		if got := ShownAsIs(c.name); got != (c.shown == c.name) {
			t.Errorf("ShownAsIs(%+q) = %v, want %v", c.name, got, c.shown == c.name)
		}
	}
}

// The format characters are Go's table of Cf, which the vector gives whole: a toolchain whose Unicode moves a character
// in or out of Cf fails here, until the vector, and so the Rust version, says so. The control characters, Cc, are
// U+0000 to U+001F and U+007F to U+009F, which Unicode never changes. Unshowable is the one or the other, and a name
// holding any of them is written in base64.
func TestFormatCharactersAreTheVectors(t *testing.T) {
	_, ranges := shownCases(t)
	inRanges := func(r rune) bool {
		for _, span := range ranges {
			if span[0] <= r && r <= span[1] {
				return true
			}
		}
		return false
	}
	count := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		format, control := inRanges(r), r <= 0x1F || 0x7F <= r && r <= 0x9F
		if format {
			count++
		}
		if got := unicode.Is(unicode.Cf, r); got != format {
			t.Fatalf("U+%04X: Cf is %v in Go's tables of Unicode %s, %v in the vector", r, got, unicode.Version, format)
		}
		if got := unicode.Is(unicode.Cc, r); got != control {
			t.Fatalf("U+%04X: Cc is %v", r, got)
		}
		if got := Unshowable(r); got != (format || control) {
			t.Fatalf("Unshowable(U+%04X) = %v", r, got)
		}
		if 0xD800 <= r && r <= 0xDFFF {
			continue // a surrogate: no UTF-8 holds it
		}
		if name := "Films/a" + string(r) + "b.mkv"; ShownAsIs(name) == (format || control) {
			t.Fatalf("ShownAsIs(%+q) = %v", name, ShownAsIs(name))
		}
	}
	t.Logf("Unicode %s: %d format characters in %d ranges", unicode.Version, count, len(ranges))
}

// FuzzShown: whatever bytes a name holds, a line of human output gets valid UTF-8 with no control and no format
// character, and either the name itself or its base64, from which its bytes come back.
func FuzzShown(f *testing.F) {
	names, _ := shownCases(f)
	for _, c := range names {
		f.Add(c.name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		shown := Shown(name)
		if !utf8.ValidString(shown) || strings.ContainsFunc(shown, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf) }) {
			t.Fatalf("Shown(%+q) = %+q", name, shown)
		}
		asIs := utf8.ValidString(name) && !strings.ContainsFunc(name, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf) })
		if ShownAsIs(name) != asIs {
			t.Fatalf("ShownAsIs(%+q) = %v", name, !asIs)
		}
		if asIs {
			if shown != name {
				t.Fatalf("Shown(%+q) = %+q, not the name", name, shown)
			}
			return
		}
		encoded, ok := strings.CutPrefix(shown, Base64Shown)
		if !ok {
			t.Fatalf("Shown(%+q) = %+q, not in base64", name, shown)
		}
		back, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || string(back) != name {
			t.Fatalf("Shown(%+q) = %+q, which reads %+q, %v", name, shown, back, err)
		}
	})
}
