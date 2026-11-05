package names

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/peondusud/atoll/internal/vectors"
)

type nfcVector struct {
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	NFC     *string `json:"nfc"`
	NFCB64  *string `json:"nfc_b64"`
	Note    string  `json:"note"`
}

func TestNFCVectors(t *testing.T) {
	for _, v := range vectors.Load[nfcVector](t, "nfc.json") {
		in, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Errorf("vector input: %v", err)
			continue
		}
		want, err := vectors.Path(v.NFC, v.NFCB64)
		if err != nil {
			t.Errorf("vector %q: %v", in, err)
			continue
		}
		if got := NFC(in); got != want {
			t.Errorf("NFC(%q) = %q, want %q", in, got, want)
		}
	}
}

// NFD then NFC gives NFC: a check of NFD on the same inputs.
func TestNFDRoundTrip(t *testing.T) {
	for _, v := range vectors.Load[nfcVector](t, "nfc.json") {
		in, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := NFC(NFD(in)), NFC(in); got != want {
			t.Errorf("NFC(NFD(%q)) = %q, want %q", in, got, want)
		}
	}
}

// FoldEqual must agree with regexp (?i) on every pair it is asked about, so
// globs and regexes match the same characters (specs.md §29.5).
func TestFoldEqualMatchesRegexp(t *testing.T) {
	probes := []rune{
		'a', 'A', 'k', 'K', 's', 'S', 'i', 'I', '_', '1',
		0x00B5, 0x00DF, 0x00E9, 0x00C9, 0x00FF, 0x0178, 0x0130, 0x0131, 0x017F,
		0x01C4, 0x01C5, 0x01C6, 0x0345, 0x0399, 0x03B9, 0x1FBE, 0x03A3, 0x03C2,
		0x03C3, 0x03BC, 0x039C, 0x1E9E, 0x212A, 0x2126, 0x03A9, 0x03C9, 0x212B,
		0x00C5, 0x00E5, 0x10D0, 0x1C90, 0x13A0, 0xAB70, 0x10400, 0x10428,
	}
	for _, a := range probes {
		re := regexp.MustCompile(`^(?i)` + regexp.QuoteMeta(string(a)) + `$`)
		for _, b := range probes {
			if got, want := FoldEqual(a, b), re.MatchString(string(b)); got != want {
				t.Errorf("FoldEqual(%U, %U) = %v, regexp (?i) says %v", a, b, got, want)
			}
		}
	}
}

func TestFoldEqualExamples(t *testing.T) {
	for _, c := range []struct {
		a, b rune
		want bool
	}{
		{'k', 0x212A, true},    // KELVIN SIGN
		{'s', 0x017F, true},    // LATIN SMALL LETTER LONG S
		{0x00DF, 0x1E9E, true}, // ß and ẞ
		{0x03C2, 0x03A3, true}, // ς and Σ
		{'i', 0x0130, false},   // İ folds with nothing else
		{'i', 0x0131, false},   // ı folds with nothing else
		{utf8.RuneError, 'a', false},
	} {
		if got := FoldEqual(c.a, c.b); got != c.want {
			t.Errorf("FoldEqual(%U, %U) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := FoldEqual(c.b, c.a); got != c.want {
			t.Errorf("FoldEqual(%U, %U) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

func TestFoldAny(t *testing.T) {
	isS := func(r rune) bool { return r == 's' }
	if !FoldAny(0x017F, isS) {
		t.Error("FoldAny(ſ, is s) = false, want true")
	}
	if FoldAny('k', isS) {
		t.Error("FoldAny(k, is s) = true, want false")
	}
}

// No vector may hold a run long enough for x/text to insert U+034F, where Go
// and Rust differ (specs.md §29.5).
func TestVectorsAvoidStreamSafeInsertion(t *testing.T) {
	check := func(file, s string) {
		if strings.Count(NFC(s), "\u034f") != strings.Count(s, "\u034f") {
			t.Errorf("%s: NFC(%q) inserts U+034F", file, s)
		}
	}
	for _, v := range vectors.Load[nfcVector](t, "nfc.json") {
		in, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		check("nfc.json", in)
	}
	for _, v := range vectors.Load[keyVector](t, "lpath_key.json") {
		in, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		check("lpath_key.json", in)
	}
}

// NonStarterRun counts exactly what Go's normalization counts: a string has a
// run of more than MaxNonStarters exactly when NFC inserts U+034F in it.
func TestNonStarterRun(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 0},
		{"e\u0301", 1},
		{"\u00e9", 1}, // é decomposes to e and U+0301
		{"\u0344", 2}, // U+0308 U+0301
		{"\uac00", 1}, // 가: jamo L, then V
		{"\uac01", 2}, // 각: jamo L, V, then T
		{"\u3133", 1}, // compatibility jamo: NFKD gives a T
		{"\u09be", 1}, // class 0, but composes backwards
		{"a" + strings.Repeat("\u0301", 31), 31},
		{"a" + strings.Repeat("\u0301", 15) + "\xff" + strings.Repeat("\u0301", 15), 15},
	} {
		if got := NonStarterRun(c.s); got != c.want {
			t.Errorf("NonStarterRun(%+q) = %d, want %d", c.s, got, c.want)
		}
	}

	// Random strings of non-starters, characters whose NFKD mixes starters
	// and non-starters, and a few starters.
	var ns, mixed []rune
	for c := rune(0x80); c <= 0x10FFFF; c++ {
		if c >= 0xD800 && c <= 0xDFFF || c == 0x034F {
			continue
		}
		n := NonStarterRun(string(c))
		switch d := utf8.RuneCountInString(norm.NFKD.String(string(c))); n {
		case 0:
		case d:
			ns = append(ns, c)
		default:
			mixed = append(mixed, c)
		}
	}
	starters := []rune("aZ\u00e9\ud55c\uac00\u1100\u30a2")
	// A xorshift generator: the strings are the same on every run.
	x := uint64(1)
	intn := func(n int) int {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		return int(x % uint64(n))
	}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := 1 + intn(80); n > 0; n-- {
			switch k := intn(20); {
			case k == 0:
				b.WriteRune(starters[intn(len(starters))])
			case k < 4:
				b.WriteRune(mixed[intn(len(mixed))])
			default:
				b.WriteRune(ns[intn(len(ns))])
			}
		}
		s := b.String()
		inserts := strings.Contains(norm.NFC.String(s), "\u034f")
		if long := NonStarterRun(s) > MaxNonStarters; long != inserts {
			t.Fatalf("%+q: NonStarterRun %d, but NFC inserts U+034F: %v", s, NonStarterRun(s), inserts)
		}
	}
}

// CollisionKeyVersion names the x/text release the binary is built with.
func TestCollisionKeyVersion(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build information")
	}
	release := ""
	for _, d := range bi.Deps {
		if d.Path == "golang.org/x/text" {
			release = d.Version
			if d.Replace != nil {
				release = d.Replace.Version
			}
		}
	}
	if release != normRelease {
		t.Errorf("built with golang.org/x/text %q, but normRelease is %q", release, normRelease)
	}
	if want := "go golang.org/x/text " + release + " " + norm.Version; CollisionKeyVersion != want {
		t.Errorf("CollisionKeyVersion = %q, want %q", CollisionKeyVersion, want)
	}
}

// UnicodeVersion follows the toolchain: with Go 1.27, both are 17.0.0.
func TestUnicodeVersion(t *testing.T) {
	if UnicodeVersion != unicode.Version {
		t.Logf("normalization uses Unicode %s, case folding %s", UnicodeVersion, unicode.Version)
	}
	if UnicodeVersion == "" {
		t.Error("UnicodeVersion is empty")
	}
}
