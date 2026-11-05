package names

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// UnicodeVersion is the Unicode version of NFC and NFD, and so of CollisionKey.
// The catalog records it with the keys (meta.unicode_version, specs.md §15.8):
// keys computed under another version are recomputed, never compared.
const UnicodeVersion = norm.Version

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
