package names

import (
	"encoding/base64"
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

// Shown gives a name as a line of human output, and an alert, write it: as it is when ShownAsIs, and otherwise as
// Base64Shown then its bytes in standard base64. The --json outputs and the log do not use it: they hold path, or
// path_b64 for bytes that are not valid UTF-8 (specs.md §29.3).
func Shown(name string) string {
	if ShownAsIs(name) {
		return name
	}
	return Base64Shown + base64.StdEncoding.EncodeToString([]byte(name))
}
