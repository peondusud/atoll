package names

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// UnicodeVersion is the Unicode version of NFC and NFD, and so of CollisionKey.
const UnicodeVersion = norm.Version

// normRelease is the release of golang.org/x/text that go.mod requires. A
// test checks it against the build.
const normRelease = "v0.43.0"

// CollisionKeyVersion names what computes CollisionKey: the language, the
// normalization library, its release and its Unicode version. The catalog
// records it with the keys (meta.lpath_key_version, specs.md §15.8): keys
// computed by any other version are recomputed, never compared. It names the
// implementation, as Go's normalization inserts U+034F after 30 non-starters
// and Rust's does not, and the release, as two releases with one Unicode
// version may still normalize differently.
const CollisionKeyVersion = "go golang.org/x/text " + normRelease + " " + UnicodeVersion

// MaxNonStarters is the longest run of non-starters a pattern may hold
// (specs.md §29.5). After it, Go's normalization inserts U+034F, following the
// UAX #15 stream-safe format, and Rust's does not.
const MaxNonStarters = 30

// NonStarterRun returns the length of the longest run of non-starters in s,
// counted as Go's normalization counts them before it inserts U+034F: in the
// compatibility decomposition (NFKD) of each character, a non-starter has a
// nonzero canonical combining class, or NFC may compose it with the character
// before it (NFC_Quick_Check Maybe: the conjoining jamo V and T, U+09BE…). A
// byte that is not part of a valid UTF-8 sequence ends a run, as it ends a
// normalization run (NFC).
func NonStarterRun(s string) int {
	run, longest := 0, 0
	for i := 0; i < len(s); {
		c, w := utf8.DecodeRuneInString(s[i:])
		i += w
		if c == utf8.RuneError && w == 1 {
			run = 0
			continue
		}
		// One character never decomposes into more than 30 non-starters, so
		// norm inserts no U+034F here.
		for _, r := range norm.NFKD.String(string(c)) {
			p := norm.NFC.PropertiesString(string(r))
			if p.CCC() == 0 && p.BoundaryBefore() {
				run = 0
				continue
			}
			run++
			longest = max(longest, run)
		}
	}
	return longest
}

// NFC returns the NFC form of s, run by run (specs.md §11.3): each maximal run
// of valid UTF-8 is normalized, and every byte that is not part of a valid
// UTF-8 sequence is kept as it is and never composes with its neighbors.
//
// Atoll calls golang.org/x/text/unicode/norm through this function only.
// Splitting the runs here, rather than passing raw bytes to norm, keeps the
// rule exact: norm.NFC leaves the last character unnormalized when the input
// ends with a 4-byte lead byte followed by one 2-byte character, such as
// "\xf3" followed by U+0344.
func NFC(s string) string {
	return byRun(s, norm.NFC)
}

// NFD returns the NFD form of s, run by run, as NFC does.
func NFD(s string) string {
	return byRun(s, norm.NFD)
}

func byRun(s string, f norm.Form) string {
	if utf8.ValidString(s) {
		return f.String(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	start := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && w == 1 {
			b.WriteString(f.String(s[start:i]))
			b.WriteByte(s[i])
			i++
			start = i
			continue
		}
		i += w
	}
	b.WriteString(f.String(s[start:]))
	return b.String()
}

// FoldEqual reports whether a and b are equal under Unicode simple case
// folding (CaseFolding.txt, statuses C and S), exactly as regexp compares
// characters under (?i): both use unicode.SimpleFold (specs.md §11.3, §29.5).
func FoldEqual(a, b rune) bool {
	if a == b {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

// FoldAny reports whether r, or one of its case variants under Unicode simple
// case folding, satisfies in. A class under (?i) matches a character exactly
// when this holds for the class's membership test.
func FoldAny(r rune, in func(rune) bool) bool {
	if in(r) {
		return true
	}
	for v := unicode.SimpleFold(r); v != r; v = unicode.SimpleFold(v) {
		if in(v) {
			return true
		}
	}
	return false
}
