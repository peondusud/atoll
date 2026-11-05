package names

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// FindingKind is what a finding of the name report is about. The list is closed
// and shared by both versions (specs.md §15.9).
type FindingKind string

// The kinds, in the order one name reports them.
const (
	FindingInvalidUTF8        FindingKind = "invalid-utf8"
	FindingNotNFC             FindingKind = "not-nfc"
	FindingIncompatible       FindingKind = "incompatible"
	FindingCollision          FindingKind = "collision"
	FindingBranchPathTooLong  FindingKind = "branch-path-too-long"
	FindingWindowsPathTooLong FindingKind = "windows-path-too-long"
)

// Finding is one problem of one name: the shortest path that has it.
type Finding struct {
	Kind    FindingKind
	Path    string
	FS      Filesystem // FindingIncompatible
	Reasons []Reason   // FindingIncompatible
	With    []string   // FindingCollision: in byte order
	Disk    string     // FindingBranchPathTooLong
}

// Report is the outcome of Check.
type Report struct {
	Checked  int       // the names in scope
	Findings []Finding // sorted as specs.md §15.9 states; never nil
}

// CheckedDisk is what Check needs to know about a disk of the pool.
type CheckedDisk struct {
	ID, Mount, Root string
}

// Check returns the name report of a pool (specs.md §15.9): paths are its logical
// paths, in any order, and each stands for its parent folders too. A non-empty
// under keeps the names equal to it, its parents and everything below it. An
// error means that a path or under is not a well-formed logical path, or that a
// disk has an empty field.
//
// The filesystems are not an input: ext4 and ntfs are checked whatever the pool
// holds, as XFS and btrfs follow the rule of ext4.
func Check(paths []string, disks []CheckedDisk, under string) (Report, error) {
	if under != "" {
		if err := checkLogicalPath(under); err != nil {
			return Report{}, err
		}
	}
	sorted := slices.Clone(disks)
	longestRoot := ""
	for _, d := range sorted {
		if d.ID == "" || d.Mount == "" || d.Root == "" {
			return Report{}, fmt.Errorf("disk %q: empty id, mount or root", d.ID)
		}
		if utf16Len(d.Root) > utf16Len(longestRoot) {
			longestRoot = d.Root
		}
	}
	slices.SortFunc(sorted, func(a, b CheckedDisk) int { return strings.Compare(a.ID, b.ID) })

	names, err := allNames(paths)
	if err != nil {
		return Report{}, err
	}
	siblings := collidingSiblings(names)

	report := Report{Findings: []Finding{}}
	for _, name := range names {
		if under != "" && !related(name, under) {
			continue
		}
		report.Checked++
		found, err := nameFindings(name, sorted, longestRoot, siblings[name])
		if err != nil {
			return Report{}, err
		}
		report.Findings = append(report.Findings, found...)
	}
	return report, nil
}

// allNames returns every path and parent folder once, in byte order.
func allNames(paths []string) ([]string, error) {
	set := map[string]bool{}
	for _, p := range paths {
		if err := checkLogicalPath(p); err != nil {
			return nil, err
		}
		for end := 0; end < len(p); end++ {
			if p[end] == '/' {
				set[p[:end]] = true
			}
		}
		set[p] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

// collidingSiblings returns, for each name that collides, the others with the
// same key and the same parent, in byte order. Names whose parents differ are
// left to the finding on those parents. names is in byte order.
func collidingSiblings(names []string) map[string][]string {
	type group struct{ parent, key string }
	groups := map[group][]string{}
	for _, n := range names {
		if key, ok := CollisionKey(n); ok {
			g := group{parentOf(n), key}
			groups[g] = append(groups[g], n)
		}
	}
	siblings := map[string][]string{}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		for _, n := range members {
			siblings[n] = slices.DeleteFunc(slices.Clone(members), func(o string) bool { return o == n })
		}
	}
	return siblings
}

// nameFindings returns the findings of one name, in the order of the kinds.
func nameFindings(name string, disks []CheckedDisk, longestRoot string, with []string) ([]Finding, error) {
	var found []Finding
	last := name[strings.LastIndexByte(name, '/')+1:]
	if !utf8.ValidString(last) {
		found = append(found, Finding{Kind: FindingInvalidUTF8, Path: name})
	}
	if NFC(last) != last {
		found = append(found, Finding{Kind: FindingNotNFC, Path: name})
	}
	for _, fs := range []Filesystem{Ext4, NTFS} {
		reasons, err := ComponentReasons(last, fs)
		if err != nil {
			return nil, err
		}
		if reasons != nil {
			found = append(found, Finding{Kind: FindingIncompatible, Path: name, FS: fs, Reasons: reasons})
		}
	}
	if with != nil {
		found = append(found, Finding{Kind: FindingCollision, Path: name, With: with})
	}
	parent := parentOf(name)
	for _, d := range disks {
		past, err := firstPast(name, parent, func(p string) (bool, error) { return FitsBranch(d.Mount, d.Root, p) })
		if err != nil {
			return nil, err
		}
		if past {
			found = append(found, Finding{Kind: FindingBranchPathTooLong, Path: name, Disk: d.ID})
		}
	}
	if longestRoot != "" {
		past, err := firstPast(name, parent, func(p string) (bool, error) { return FitsWindows(longestRoot, p) })
		if err != nil {
			return nil, err
		}
		if past {
			found = append(found, Finding{Kind: FindingWindowsPathTooLong, Path: name})
		}
	}
	return found, nil
}

// firstPast reports whether name does not fit and its parent, if any, does: only
// the shortest path past a limit gets a finding.
func firstPast(name, parent string, fits func(string) (bool, error)) (bool, error) {
	ok, err := fits(name)
	if err != nil || ok {
		return false, err
	}
	if parent == "" {
		return true, nil
	}
	return fits(parent)
}

func parentOf(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return ""
}

// related reports whether name is under, below it or one of its parents,
// comparing whole components.
func related(name, under string) bool {
	return name == under || strings.HasPrefix(name, under+"/") || strings.HasPrefix(under, name+"/")
}
