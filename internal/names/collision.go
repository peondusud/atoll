package names

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Collision is where a path collides with the names on an NTFS target
// (specs.md §15.7).
type Collision struct {
	Component int      // the first component that collides, from 0
	With      []string // the exact names on the target at that component, in byte order
}

// TargetNames holds the names on one NTFS target: what it already holds, and
// what a plan has put there so far.
type TargetNames struct {
	exact map[string]map[string]bool // the exact prefixes that have each key
}

// NewTargetNames returns an empty set of names.
func NewTargetNames() *TargetNames {
	return &TargetNames{exact: map[string]map[string]bool{}}
}

// Add records path and its parent folders. An error means that path is not a
// well-formed logical path.
func (t *TargetNames) Add(path string) error {
	prefixes, err := keyedPrefixes(path)
	if err != nil {
		return err
	}
	for _, p := range prefixes {
		if t.exact[p.key] == nil {
			t.exact[p.key] = map[string]bool{}
		}
		t.exact[p.key][p.prefix] = true
	}
	return nil
}

// Find returns the first component of path that collides with a name in t, or
// nil when none does. A path never collides with itself. For a case-only
// rename, the caller leaves the old name out of t. An error means that path is
// not a well-formed logical path.
func (t *TargetNames) Find(path string) (*Collision, error) {
	prefixes, err := keyedPrefixes(path)
	if err != nil {
		return nil, err
	}
	for i, p := range prefixes {
		var with []string
		for name := range t.exact[p.key] {
			if name != p.prefix {
				with = append(with, name)
			}
		}
		if with != nil {
			slices.Sort(with)
			return &Collision{Component: i, With: with}, nil
		}
	}
	return nil, nil
}

type keyedPrefix struct{ prefix, key string }

// keyedPrefixes returns the prefixes of a logical path, shortest first, up to
// the first one without a key: a longer prefix holds the same invalid bytes, so
// it has no key either, and a name without a key never collides.
func keyedPrefixes(path string) ([]keyedPrefix, error) {
	if err := checkLogicalPath(path); err != nil {
		return nil, err
	}
	components := strings.Split(path, "/")
	prefixes := make([]keyedPrefix, 0, len(components))
	end := -1
	for _, c := range components {
		end += 1 + len(c)
		key, ok := CollisionKey(path[:end])
		if !ok {
			break
		}
		prefixes = append(prefixes, keyedPrefix{path[:end], key})
	}
	return prefixes, nil
}

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
