package watch

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// What an event says (specs.md §15.1).
const (
	KindCreated   = "created"
	KindWritten   = "written"
	KindAttrib    = "attrib"
	KindDeleted   = "deleted"
	KindMovedFrom = "moved-from"
	KindMovedTo   = "moved-to"
	KindOverflow  = "overflow"
)

// Who made an event (specs.md §15.1).
const (
	OriginView    = "view"
	OriginAtoll   = "atoll"
	OriginUser    = "user"
	OriginUnknown = "unknown"
)

var (
	eventKinds   = []string{KindCreated, KindWritten, KindAttrib, KindDeleted, KindMovedFrom, KindMovedTo, KindOverflow}
	eventOrigins = []string{OriginView, OriginAtoll, OriginUser, OriginUnknown}
)

// Event is what the watcher reports of one change on one disk: a hint of where to look (specs.md §15.1).
type Event struct {
	Disk string
	// Path is logical, relative to the disk's root, "" for the root itself.
	Path   string
	Folder bool
	Kind   string
	Origin string
	AtNS   int64
}

// Group gathers the events of one disk at one path (specs.md §15.1).
type Group struct {
	Disk    string
	Path    string
	Folder  bool
	Kinds   []string // a set, in byte order
	Origins []string // a set, in byte order
	FirstNS int64
	LastNS  int64
}

type groupKey struct{ disk, path string }

// Grouping holds the groups not ripe yet. It does no I/O, and is not safe for concurrent use.
type Grouping struct {
	debounce int64
	groups   map[groupKey]*Group
}

// NewGrouping groups over a debounce, in nanoseconds.
func NewGrouping(debounceNS int64) (*Grouping, error) {
	if debounceNS < 0 {
		return nil, fmt.Errorf("grouping: a negative debounce, %d", debounceNS)
	}
	return &Grouping{debounce: debounceNS, groups: map[groupKey]*Group{}}, nil
}

// SetDebounce gives the grouping another debounce, in nanoseconds, as a configuration read again does: the groups
// that wait are ripe by it from then on. A negative one is refused, and the one before stays.
func (g *Grouping) SetDebounce(debounceNS int64) error {
	if debounceNS < 0 {
		return fmt.Errorf("grouping: a negative debounce, %d", debounceNS)
	}
	g.debounce = debounceNS
	return nil
}

// Add puts an event in its group. An error is an event outside the closed lists of specs.md §15.1.
func (g *Grouping) Add(e Event) error {
	switch {
	case !slices.Contains(eventKinds, e.Kind):
		return fmt.Errorf("grouping: unknown kind %q", e.Kind)
	case !slices.Contains(eventOrigins, e.Origin):
		return fmt.Errorf("grouping: unknown origin %q", e.Origin)
	case e.Kind == KindOverflow && e.Path != "":
		return errors.New("grouping: an overflow is at the root")
	}
	k := groupKey{e.Disk, e.Path}
	grp := g.groups[k]
	if grp == nil {
		grp = &Group{Disk: e.Disk, Path: e.Path, FirstNS: e.AtNS, LastNS: e.AtNS}
		g.groups[k] = grp
	}
	grp.Folder = grp.Folder || e.Folder
	grp.Kinds = addSorted(grp.Kinds, e.Kind)
	grp.Origins = addSorted(grp.Origins, e.Origin)
	grp.FirstNS = min(grp.FirstNS, e.AtNS)
	grp.LastNS = max(grp.LastNS, e.AtNS)
	return nil
}

func addSorted(set []string, v string) []string {
	i, found := slices.BinarySearch(set, v)
	if found {
		return set
	}
	return slices.Insert(set, i, v)
}

// Ripe takes out the groups ripe at now, by disk then path in byte order. It says how many still wait, and,
// when one does, the earliest time one of them will be ripe.
func (g *Grouping) Ripe(nowNS int64) (ripe []Group, waiting int, nextNS int64, ok bool) {
	for k, grp := range g.groups {
		due := grp.LastNS + g.debounce
		if due <= nowNS {
			ripe = append(ripe, *grp)
			delete(g.groups, k)
			continue
		}
		waiting++
		if !ok || due < nextNS {
			nextNS, ok = due, true
		}
	}
	slices.SortFunc(ripe, func(a, b Group) int {
		return cmp.Or(cmp.Compare(a.Disk, b.Disk), cmp.Compare(a.Path, b.Path))
	})
	return ripe, waiting, nextNS, ok
}

// Len counts the groups not ripe yet.
func (g *Grouping) Len() int {
	return len(g.groups)
}
