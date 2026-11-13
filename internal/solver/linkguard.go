package solver

import (
	"cmp"
	"slices"
	"strconv"
)

// LinkEntry is an entry of a disk's links folder, as a look found it (specs.md §17).
type LinkEntry struct {
	Name string
	// File says that it is a regular file; Inode is then what it leads to.
	File  bool
	Inode string
}

// LinkCopy is a copy of the disk, as the catalog has it.
type LinkCopy struct {
	FileID int64
	// Inode is empty for a copy not on the disk yet.
	Inode string
	// Place is where it sits, and There says that the last look found it there.
	Place string
	There bool
}

// LinksInput is what deciding a disk's links needs (specs.md §17): whether the guard is on, the links, the copies, the
// inodes of what its trash holds, the path of each file the catalog still has, and for a file it no longer has, the
// path its last copy-gone event on that disk gives it.
type LinksInput struct {
	On     bool
	Links  []LinkEntry
	Copies []LinkCopy
	Trash  map[string]bool
	Files  map[int64]string
	Gone   map[int64]string
}

// LinkAct is what becomes of a link, or of a copy with none.
type LinkAct string

// What becomes of a link, or of a copy with none.
const (
	// LinkKeep: the link leads to its file's copy.
	LinkKeep LinkAct = "keep"
	// LinkUnlink: its data has another name Atoll knows, a copy or an entry of the trash, or the guard is off and it
	// leads to its file's copy. Removing it loses nothing.
	LinkUnlink LinkAct = "unlink"
	// LinkTrash: its data lost every name Atoll knows; it goes to the trash, at Path, for Reason.
	LinkTrash LinkAct = "trash"
	// LinkUnnamed: as LinkTrash, but nothing says the path it had, and a trash entry is a path: it stays.
	LinkUnnamed LinkAct = "unnamed"
	// LinkStray: not a name Atoll gives, or not a regular file. It stays, as nothing of Atoll's.
	LinkStray LinkAct = "stray"
	// LinkMake: a copy on the disk without its link, made from Place.
	LinkMake LinkAct = "make"
)

// LinkStep is one thing to do. Name is the link's, but for LinkMake, which names the copy by FileID.
type LinkStep struct {
	Do     LinkAct
	Name   string
	FileID int64
	Inode  string
	Path   string
	Reason string
}

// The reasons of what a link sends to the trash (specs.md §17).
const (
	LinkDeleted     = "deleted"
	LinkOverwritten = "overwritten"
)

// PlanLinks decides what becomes of each link of a disk, then which copies get one, the links first, by name in byte
// order, then the copies, by file id (specs.md §17). The guard keeps one link per copy, named by its file's id:
//
//   - a link that leads to its file's copy is kept, or removed when the guard is off, as the copy holds its data; but
//     a copy the last look did not find there, as a guard holding its deletion leaves it, keeps its link even then;
//   - one whose data is another copy found there, or an entry of the trash, is removed: Atoll moved that data itself;
//   - any other's data lost every name Atoll knows: a deletion or a replacement that went around Atoll, through the
//     merged view. It goes to the trash, overwritten when its file still has a copy on the disk, of another version or
//     waiting for one, deleted otherwise; at its file's path, or at the path the copy-gone event gave it once its
//     file's row went. With neither, it stays, unnamed;
//   - with the guard on, each copy on the disk, there and with an inode, whose link is not kept, gets one.
func PlanLinks(in *LinksInput) []LinkStep {
	own := make(map[int64]*LinkCopy, len(in.Copies))
	copyInodes := make(map[string]bool, len(in.Copies))
	for i := range in.Copies {
		c := &in.Copies[i]
		own[c.FileID] = c
		if c.Inode != "" && c.There {
			copyInodes[c.Inode] = true
		}
	}
	links := slices.Clone(in.Links)
	slices.SortFunc(links, func(a, b LinkEntry) int { return cmp.Compare(a.Name, b.Name) })
	out := make([]LinkStep, 0, len(links))
	kept := map[int64]bool{}
	taken := map[int64]bool{}
	for i := range links {
		l := &links[i]
		id, ok := linkID(l.Name)
		if !ok || !l.File {
			out = append(out, LinkStep{Do: LinkStray, Name: l.Name, FileID: id})
			if ok {
				taken[id] = true
			}
			continue
		}
		c := own[id]
		step := LinkStep{Name: l.Name, FileID: id, Inode: l.Inode}
		switch {
		case c != nil && c.Inode != "" && c.Inode == l.Inode && (in.On || !c.There):
			// Off, a copy a look did not find holds its data in the link alone, as a guard holds its deletion: it waits.
			step.Do = LinkKeep
			kept[id] = true
		case c != nil && c.Inode != "" && c.Inode == l.Inode, copyInodes[l.Inode], in.Trash[l.Inode]:
			step.Do = LinkUnlink
			if c != nil {
				step.Path = c.Place
			}
		default:
			step.Do, step.Reason = LinkTrash, LinkDeleted
			if c != nil {
				step.Reason = LinkOverwritten
			}
			if path, ok := in.Files[id]; ok {
				step.Path = path
			} else if path, ok := in.Gone[id]; ok {
				step.Path = path
			} else {
				step.Do, step.Reason = LinkUnnamed, ""
			}
		}
		out = append(out, step)
	}
	if !in.On {
		return out
	}
	copies := slices.Clone(in.Copies)
	slices.SortFunc(copies, func(a, b LinkCopy) int { return cmp.Compare(a.FileID, b.FileID) })
	for i := range copies {
		c := &copies[i]
		if c.There && c.Inode != "" && !kept[c.FileID] && !taken[c.FileID] {
			out = append(out, LinkStep{Do: LinkMake, FileID: c.FileID, Inode: c.Inode, Path: c.Place})
		}
	}
	return out
}

// linkID reads a link's name as layout.LinkName writes it: a file id in decimal, positive, with no leading zero.
func linkID(name string) (int64, bool) {
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != name {
		return 0, false
	}
	return id, true
}
