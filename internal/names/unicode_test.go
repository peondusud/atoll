package names

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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

// UnicodeVersion follows the toolchain: with Go 1.27, both are 17.0.0.
func TestUnicodeVersion(t *testing.T) {
	if UnicodeVersion != unicode.Version {
		t.Logf("normalization uses Unicode %s, case folding %s", UnicodeVersion, unicode.Version)
	}
	if UnicodeVersion == "" {
		t.Error("UnicodeVersion is empty")
	}
}
