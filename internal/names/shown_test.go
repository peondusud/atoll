package names

import (
	"encoding/base64"
	"strconv"
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

// textVector is a case of conformance/vectors/shown_text.json: a text and how a line for a person writes it.
type textVector struct {
	Text    *string `json:"text"`
	TextB64 *string `json:"text_b64"`
	Shown   *string `json:"shown"`
	Note    string  `json:"note"`
}

// textCases gives the vector's texts and what is written for each, in its order, or fails t.
func textCases(t testing.TB) []shownName {
	t.Helper()
	var out []shownName
	seen := map[string]bool{}
	for i, v := range vectors.Load[textVector](t, "shown_text.json") {
		text, err := vectors.Bytes("text", v.Text, v.TextB64)
		switch {
		case err != nil:
			t.Fatalf("case %d: %v", i, err)
		case v.Shown == nil:
			t.Fatalf("case %d, %+q: no shown", i, text)
		case seen[text]:
			t.Fatalf("case %d, %+q: twice", i, text)
		}
		seen[text] = true
		out = append(out, shownName{text, *v.Shown})
	}
	if len(out) == 0 {
		t.Fatal("no case")
	}
	return out
}

func TestShownTextVectors(t *testing.T) {
	for _, c := range textCases(t) {
		if got := Escaped(c.name); got != c.shown {
			t.Errorf("Escaped(%+q) = %+q, want %+q", c.name, got, c.shown)
		}
	}
}

// unescaped reads back what Escaped wrote: \u{} with four to six uppercase hexadecimal digits, no 0 leading past the
// fourth, a character; \x{} with two, a byte; any other backslash itself. It says whether every escape was written so.
func unescaped(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' || !strings.HasPrefix(s[i+1:], "u{") && !strings.HasPrefix(s[i+1:], "x{") {
			b.WriteByte(s[i])
			i++
			continue
		}
		digits, _, closed := strings.Cut(s[i+3:], "}")
		upper := closed && digits != "" && !strings.ContainsFunc(digits, func(r rune) bool { return (r < '0' || '9' < r) && (r < 'A' || 'F' < r) })
		if !upper {
			return "", false
		}
		v, err := strconv.ParseUint(digits, 16, 32)
		switch {
		case err != nil:
			return "", false
		case s[i+1] == 'x' && len(digits) == 2:
			b.WriteByte(byte(v))
		case s[i+1] == 'u' && len(digits) >= 4 && (len(digits) == 4 || digits[0] != '0') && v <= unicode.MaxRune && utf8.ValidRune(rune(v)):
			b.WriteRune(rune(v))
		default:
			return "", false
		}
		i += 3 + len(digits) + 1
	}
	return b.String(), true
}

// FuzzEscaped: whatever a text holds, the line gets valid UTF-8 with no control and no format character, from which
// the text's bytes come back, each escape in its one spelling; and a text with nothing to escape is written as it is.
func FuzzEscaped(f *testing.F) {
	for _, c := range textCases(f) {
		f.Add(c.name)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got := Escaped(text)
		if !utf8.ValidString(got) || strings.ContainsFunc(got, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf) }) {
			t.Fatalf("Escaped(%+q) = %+q", text, got)
		}
		back, ok := unescaped(got)
		if !ok || back != text {
			t.Fatalf("Escaped(%+q) = %+q, which reads %+q, %v", text, got, back, ok)
		}
		plain := utf8.ValidString(text) && !strings.ContainsFunc(text, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf) }) &&
			!strings.Contains(text, `\u{`) && !strings.Contains(text, `\x{`)
		if plain != (got == text) {
			t.Fatalf("Escaped(%+q) = %+q", text, got)
		}
	})
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
