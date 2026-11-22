package watch

import (
	"slices"
	"strings"
)

// MaxNames is how many names a pass of the daemon looks at on a disk at most: past them, it walks the disk whole,
// which costs less than so many looks (specs.md §15.13).
const MaxNames = 10000

// Heard is what the watcher asks a pass of the daemon to look at on a disk: its names, or, with Whole, the whole disk.
type Heard struct {
	Whole bool
	Names []Named
}

// HeardNames gives what ripe groups ask a pass of the daemon to look at, by disk (specs.md §15.13): the name of each
// group someone other than Atoll made, a folder there walked below it and a name an event said was deleted; or the whole
// disk, for a group at its root, as an overflow is and a folder whose path could not be told, or past maxNames names.
// What Atoll did alone is in its journal and its catalog: no look needs to find it. The names of a disk are in byte
// order, each once.
func HeardNames(ripe []Group, maxNames int) map[string]Heard {
	out := map[string]Heard{}
	for i := range ripe {
		g := &ripe[i]
		if !slices.ContainsFunc(g.Origins, func(o string) bool { return o != OriginAtoll }) {
			continue
		}
		h := out[g.Disk]
		if g.Path == "" || slices.Contains(g.Kinds, KindOverflow) {
			h = Heard{Whole: true}
		} else if !h.Whole {
			h.Names = append(h.Names, Named{Path: g.Path, Folder: g.Folder, Deleted: slices.Contains(g.Kinds, KindDeleted)})
		}
		out[g.Disk] = h
	}
	for id, h := range out {
		out[id] = settleHeard(h, maxNames)
	}
	return out
}

// AddHeard adds what more ripe groups ask to what a pass has not taken yet: a disk walked whole stays so, and its names
// add up, each once, up to maxNames.
func AddHeard(into, more map[string]Heard, maxNames int) {
	for id, h := range more {
		was := into[id]
		if was.Whole || h.Whole {
			into[id] = Heard{Whole: true}
			continue
		}
		into[id] = settleHeard(Heard{Names: append(slices.Clip(was.Names), h.Names...)}, maxNames)
	}
}

// settleHeard puts the names of a disk in byte order, each once, one an event said was deleted, or a folder, once any
// of its events said so; past maxNames, the disk is walked whole.
func settleHeard(h Heard, maxNames int) Heard {
	if h.Whole {
		return Heard{Whole: true}
	}
	slices.SortStableFunc(h.Names, func(a, b Named) int { return strings.Compare(a.Path, b.Path) })
	names := h.Names[:0]
	for _, n := range h.Names {
		if last := len(names) - 1; last >= 0 && names[last].Path == n.Path {
			names[last].Folder = names[last].Folder || n.Folder
			names[last].Deleted = names[last].Deleted || n.Deleted
			continue
		}
		names = append(names, n)
	}
	if len(names) > maxNames {
		return Heard{Whole: true}
	}
	return Heard{Names: names}
}
