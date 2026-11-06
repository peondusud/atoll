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
// what a plan has put there so far. It is a tree of folders, as a target holds
// hundreds of thousands of names. The key of a path is the keys of its components
// joined by /, since neither NFD nor NFC ever crosses a /. So two paths with the
// same key and different bytes differ first at a component with the same key,
// under one folder.
type TargetNames struct {
	root nameNode
}

// nameNode is one folder.
type nameNode struct {
	key   string               // the key of this folder's own name
	names map[string][]string  // the exact names in this folder, by key
	dirs  map[string]*nameNode // the folders in this folder, by exact name
}

// NewTargetNames returns an empty set of names.
func NewTargetNames() *TargetNames {
	return &TargetNames{}
}

// components calls fn for each component of a path, with its offset. The path must be
// well-formed. fn returns false to stop.
func components(path string, fn func(component string, start int, last bool) bool) {
	for start := 0; ; {
		end := strings.IndexByte(path[start:], '/')
		if end < 0 {
			fn(path[start:], start, true)
			return
		}
		if !fn(path[start:start+end], start, false) {
			return
		}
		start += end + 1
	}
}

// Add records path and its parent folders, up to the first component that has no key:
// a name without a key never collides. An error means that path is not a well-formed
// logical path.
func (t *TargetNames) Add(path string) error {
	if err := checkLogicalPath(path); err != nil {
		return err
	}
	node := &t.root
	components(path, func(c string, _ int, last bool) bool {
		child := node.dirs[c]
		if child == nil {
			key, ok := CollisionKey(c)
			if !ok {
				return false
			}
			if !slices.Contains(node.names[key], c) {
				if node.names == nil {
					node.names = map[string][]string{}
				}
				node.names[key] = append(node.names[key], c)
			}
			if last {
				return false
			}
			child = &nameNode{key: key}
			if node.dirs == nil {
				node.dirs = map[string]*nameNode{}
			}
			node.dirs[c] = child
		}
		node = child
		return true
	})
	return nil
}

// Find returns the first component of path that collides with a name in t, or
// nil when none does. A path never collides with itself. For a case-only
// rename, the caller leaves the old name out of t. An error means that path is
// not a well-formed logical path.
func (t *TargetNames) Find(path string) (*Collision, error) {
	if err := checkLogicalPath(path); err != nil {
		return nil, err
	}
	var found *Collision
	node := &t.root
	index := 0
	components(path, func(c string, start int, _ bool) bool {
		child := node.dirs[c]
		var key string
		if child != nil {
			key = child.key
		} else {
			var ok bool
			if key, ok = CollisionKey(c); !ok {
				return false
			}
		}
		var with []string
		for _, name := range node.names[key] {
			if name != c {
				with = append(with, path[:start]+name)
			}
		}
		if with != nil {
			slices.Sort(with)
			found = &Collision{Component: index, With: with}
			return false
		}
		if child == nil {
			return false
		}
		node = child
		index++
		return true
	})
	return found, nil
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
