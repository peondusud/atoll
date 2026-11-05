package names

import (
	"strings"
	"unicode/utf8"
)

// CollisionKey returns lpath_key, the key that detects name collisions
// (specs.md §15.7, §15.8): the NFD form of the path, then the NTFS upcase
// table of Windows 7 to 11 plus ς → Σ applied to each BMP character, then the
// NFC form. Characters above U+FFFF are unchanged, as NTFS compares UTF-16
// code units and never maps a surrogate. ok is false for a path that is not
// valid UTF-8, which has no key (NULL in the catalog).
//
// The key serves collisions only: rule matching uses Unicode simple case
// folding instead (specs.md §11.3).
func CollisionKey(path string) (key string, ok bool) {
	if !utf8.ValidString(path) {
		return "", false
	}
	d := NFD(path)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		b.WriteRune(collisionUpcase(r))
	}
	return NFC(b.String()), true
}

// collisionUpcase maps one character for CollisionKey.
func collisionUpcase(r rune) rune {
	switch {
	case r == 'ς':
		// Windows XP, Vista and Samba merge ς with σ and Σ; the Windows 7 to
		// 11 table does not.
		return 'Σ'
	case r > 0xFFFF:
		return r
	default:
		return ntfsUpcase(r)
	}
}
