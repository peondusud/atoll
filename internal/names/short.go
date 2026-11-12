package names

import "strings"

// ShortForm says whether a name has the form Windows gives the short name of a longer one: at most 8 characters before
// a single dot, the last of them a tilde and digits after at least one other, and at most 3 after the dot, each an
// ASCII letter of either case, a digit or a character a short name may hold. On NTFS such a name may be the short name
// of another entry, which every driver answers for and none lists, and refuses to create: existence there is never
// decided by a look alone (specs.md §15.8, conformance/vectors/short_form.json).
func ShortForm(name string) bool {
	base, ext, dotted := strings.Cut(name, ".")
	if dotted && (ext == "" || len(ext) > 3 || strings.Contains(ext, ".")) {
		return false
	}
	if len(base) > 8 || !shortChars(base) || !shortChars(ext) {
		return false
	}
	tilde := strings.LastIndexByte(base, '~')
	if tilde < 1 || tilde == len(base)-1 {
		return false
	}
	for i := tilde + 1; i < len(base); i++ {
		if base[i] < '0' || base[i] > '9' {
			return false
		}
	}
	return true
}

// shortChars says whether every byte is one a short name may hold.
func shortChars(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'()-@^_`{}~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
