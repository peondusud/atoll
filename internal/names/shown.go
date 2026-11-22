package names

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Base64Shown begins a name that a line of human output, or an alert, writes in base64 (specs.md §23, §25).
const Base64Shown = "(base64) "

// Unshowable says whether a character keeps a name that holds it from being shown as it is, in a line of human output
// or in an alert (specs.md §23, §25): a control character, of the general category Cc, which may write a line of its own
// or clear a terminal; or a format character, Cf, which may reorder what follows it or hide itself, so that the name
// reads as another ("Trojan Source"): U+202E RIGHT-TO-LEFT OVERRIDE, the isolates U+2066 to U+2069, the zero-width
// characters U+200B to U+200D, U+FEFF, U+00AD and the rest. They are the categories of Go's tables, Unicode 17.0.0
// with Go 1.27 (unicode.Version), which conformance/vectors/shown_name.json pins, every range of Cf: a toolchain whose
// tables say otherwise fails its test, rather than show a name another version would hide.
func Unshowable(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf)
}

// ShownAsIs says whether a line of human output, and an alert, write a name as it is: when it is valid UTF-8 and holds
// no character that is Unshowable. A name that needs a format character, Persian with U+200C ZERO WIDTH NON-JOINER or
// an emoji sequence with U+200D ZERO WIDTH JOINER, is written in base64 too, as the maintainer chose (2026-11-22).
func ShownAsIs(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsFunc(name, Unshowable)
}

// Escaped gives a text written for a person, an error, a warning or what a disk's line of a human output says of it, as
// standard error and the human output write it (specs.md §25): each character that is Unshowable as \u{XXXX}, its
// code point in uppercase hexadecimal of four digits or more, so that the text writes no line of its own and reorders
// or hides nothing; each byte that is not UTF-8 as \x{XX}; and a backslash that u{ or x{ follows as \u{005C}, so that
// no backslash of the text reads as one of these. Anything else is written as it is: a text with none of them is
// unchanged, and so is what is escaped already, by Go's %q, by the quotes of a key of the configuration (§24.3), or by
// systemd-escape, as none of them writes braces. conformance/vectors/shown_text.json pins it. The --json outputs and
// the log do not use it: they hold the text as it is.
func Escaped(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x{%02X}`, text[i])
		case Unshowable(r):
			fmt.Fprintf(&b, `\u{%04X}`, r)
		case r == '\\' && (strings.HasPrefix(text[i+1:], "u{") || strings.HasPrefix(text[i+1:], "x{")):
			b.WriteString(`\u{005C}`)
		default:
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Shown gives a name as a line of human output, and an alert, write it: as it is when ShownAsIs, and otherwise as
// Base64Shown then its bytes in standard base64. The --json outputs and the log do not use it: they hold path, or
// path_b64 for bytes that are not valid UTF-8 (specs.md §29.3).
func Shown(name string) string {
	if ShownAsIs(name) {
		return name
	}
	return Base64Shown + base64.StdEncoding.EncodeToString([]byte(name))
}
