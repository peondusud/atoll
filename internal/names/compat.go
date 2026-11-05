package names

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Filesystem is the filesystem of a disk (specs.md §9, disks.fs_type).
type Filesystem string

// The filesystems a pool disk may have (specs.md §22).
const (
	Ext4  Filesystem = "ext4"
	XFS   Filesystem = "xfs"
	Btrfs Filesystem = "btrfs"
	NTFS  Filesystem = "ntfs"
)

// Reason is why a filesystem refuses a name. The list is closed and shared by
// both versions (specs.md §15.8, §29.3).
type Reason string

// The reasons, in the order a component reports them.
const (
	TooLong            Reason = "too-long"
	InvalidUTF8        Reason = "invalid-utf8"
	ControlCharacter   Reason = "control-character"
	ForbiddenCharacter Reason = "forbidden-character"
	TrailingDotOrSpace Reason = "trailing-dot-or-space"
	ReservedName       Reason = "reserved-name"
)

// MaxComponentBytes is the longest component, in bytes, that every pool
// filesystem accepts: ext4, XFS and btrfs, and lowntfs-3g on NTFS.
const MaxComponentBytes = 255

// MaxComponentUTF16 is the longest NTFS component, in UTF-16 code units.
const MaxComponentUTF16 = 255

// Problem is a component of a logical path that a filesystem refuses.
type Problem struct {
	Component int      // index of the component, from 0
	Reasons   []Reason // in the order of the Reason constants
}

// CheckPath returns the components of the logical path that fs refuses, in
// path order, or nil when fs accepts every one (specs.md §15.8). Every
// component is checked, folders included. An error means that path is not a
// well-formed logical path (an empty component, ".", "..", or a NUL byte) or
// that fs is unknown.
//
// The check is a pure table, the same in both versions: it never asks the
// operating system, whose answer depends on its version and mount options.
func CheckPath(path string, fs Filesystem) ([]Problem, error) {
	var problems []Problem
	for i, c := range strings.Split(path, "/") {
		reasons, err := ComponentReasons(c, fs)
		if err != nil {
			return nil, fmt.Errorf("logical path %q: %w", path, err)
		}
		if reasons != nil {
			problems = append(problems, Problem{Component: i, Reasons: reasons})
		}
	}
	return problems, nil
}

// ComponentReasons returns the reasons fs refuses the path component name, or
// nil when fs accepts it (specs.md §15.8). Lengths are counted on the exact
// bytes, or on their exact UTF-16 conversion, never on a normalized form.
// Every rule reads bytes except the UTF-16 length, so a name that is not valid
// UTF-8 still gets its other reasons. An error means name is empty, "." or
// "..", or holds "/" or NUL, or that fs is unknown.
func ComponentReasons(name string, fs Filesystem) ([]Reason, error) {
	switch {
	case name == "" || name == "." || name == "..":
		return nil, fmt.Errorf("component %q is not a name", name)
	case strings.ContainsAny(name, "/\x00"):
		return nil, fmt.Errorf("component %q holds / or NUL", name)
	}
	switch fs {
	case Ext4, XFS, Btrfs:
		if len(name) > MaxComponentBytes {
			return []Reason{TooLong}, nil
		}
		return nil, nil
	case NTFS:
		return ntfsReasons(name), nil
	default:
		return nil, fmt.Errorf("unknown filesystem %q", fs)
	}
}

// ntfsReasons applies the Windows rules that every NTFS disk follows.
func ntfsReasons(name string) []Reason {
	var reasons []Reason
	valid := utf8.ValidString(name)
	if len(name) > MaxComponentBytes || valid && utf16Len(name) > MaxComponentUTF16 {
		reasons = append(reasons, TooLong)
	}
	if !valid {
		reasons = append(reasons, InvalidUTF8)
	}
	control, forbidden := false, false
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c < 0x20:
			control = true
		case strings.IndexByte(`"*:<>?\|`, c) >= 0:
			forbidden = true
		}
	}
	if control {
		reasons = append(reasons, ControlCharacter)
	}
	if forbidden {
		reasons = append(reasons, ForbiddenCharacter)
	}
	if last := name[len(name)-1]; last == ' ' || last == '.' {
		reasons = append(reasons, TrailingDotOrSpace)
	}
	if reservedName(name) {
		reasons = append(reasons, ReservedName)
	}
	return reasons
}

// utf16Len returns the number of UTF-16 code units of a valid UTF-8 string.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// reservedName reports whether Windows 10 reads name as a device: the part
// before the first ".", without its trailing spaces, is a device name,
// ignoring the case of ASCII letters only. Windows 11 reserves bare names
// only; Atoll keeps the stricter rule.
func reservedName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	base = strings.TrimRight(base, " ")
	if len(base) < 3 || len(base) > 7 {
		return false
	}
	b := []byte(base)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	switch up := string(b); up {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	default:
		if !strings.HasPrefix(up, "COM") && !strings.HasPrefix(up, "LPT") {
			return false
		}
		switch port := up[3:]; port {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
			return true
		}
		return false
	}
}
