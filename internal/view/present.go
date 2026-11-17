package view

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/solver"
)

// Disk is what Atoll's own view knows of a disk (specs.md §21.2).
type Disk struct {
	ID       string
	Position int
	State    solver.DiskState
	// Cache says that the disk is a cache disk the view leaves out, as mergerfs's line does, with view.engine =
	// "mergerfs" (specs.md §19). With Atoll's own view, a cache disk is part of the view, and is not one of these.
	Cache bool
	// Capacity and Free are what the catalog holds, nil when it holds none yet.
	Capacity, Free *int64
	// Tags are the disk's tags, which read.classes names (specs.md §7).
	Tags []string
}

// Copy is a copy of the file at a path, as the catalog holds it: a nil field is one its row holds none of.
type Copy struct {
	Disk          string
	State         solver.CopyState
	Size, MtimeNS *int64
	Mode          *uint32
	UID, GID      *uint32
	Nlink         *int64
	CreationNS    *int64
	FirstSeenNS   int64
	// GoneKind is what the last look found when the copy's clock runs (copies.gone_kind), "" when it does not; OldPath
	// is where the copy sits when it has not followed its file's rename (copies.old_lpath), "" otherwise. Either way,
	// it holds nothing at the path (specs.md §9, §13.4).
	GoneKind string
	OldPath  string
}

// File is the file at a path, as its row holds it.
type File struct {
	State solver.State
	// Original is files.original_disk, "" when it names none; ConflictHeld says that the file, corrupt, holds a
	// conflict (specs.md §4).
	Original     string
	ConflictHeld bool
	Size         int64
	MtimeNS      int64
	Copies       []Copy
}

// Entry is what one disk holds at a path that is no file of the catalog: a folder, a symlink or an ignored file.
type Entry struct {
	Disk          string
	Kind          solver.EntryKind
	Mode          *uint32
	UID, GID      *uint32
	Size, MtimeNS *int64
	Target        string
}

// NodeKind is what a path presents.
type NodeKind string

// What a path presents.
const (
	NodeFile    NodeKind = "file"
	NodeDir     NodeKind = "dir"
	NodeSymlink NodeKind = "symlink"
)

// Node is what a path of the view presents: its attributes, and the disk whose copy or entry they are.
type Node struct {
	Kind NodeKind
	Disk string
	// Ignored says that a file is an ignored one, read from its entry's disk.
	Ignored  bool
	Mode     uint32
	UID, GID uint32
	Size     int64
	MtimeNS  int64
	Nlink    int64
	Target   string
}

// Owner is the owner of the view's own process, which presents what a row lacks.
type Owner struct {
	UID, GID uint32
}

// Defaults of what a row lacks (specs.md §21.2).
const (
	defaultDirMode  = 0o755
	defaultFileMode = 0o644
)

// presentOrder gives each disk that presents its rank: those online, standby, readonly or draining, by position, then
// those failed, by position, as mergerfs reads its branches (specs.md §21.1, §21.2). A disk missing or retired has
// none, and nor has a cache disk the view leaves out, as mergerfs's line does (specs.md §19).
func presentOrder(disks []Disk) map[string]int {
	var first, last []Disk
	for _, d := range disks {
		if d.Cache {
			continue
		}
		switch d.State {
		case solver.DiskOnline, solver.DiskStandby, solver.DiskReadOnly, solver.DiskDraining:
			first = append(first, d)
		case solver.DiskFailed:
			last = append(last, d)
		case solver.DiskMissing, solver.DiskRetired:
			// It presents nothing.
		}
	}
	byPosition := func(a, b Disk) int { return cmp.Compare(a.Position, b.Position) }
	slices.SortFunc(first, byPosition)
	slices.SortFunc(last, byPosition)
	rank := map[string]int{}
	for i, d := range append(first, last...) {
		rank[d.ID] = i
	}
	return rank
}

// holds says whether a copy holds its file at its path on its disk, for the view: one ok or surplus does, its clock not
// running and its file's rename followed, as a source of the plan (specs.md §13.4). A copy its last look did not find
// at its name, or that sits at an old one, has nothing to read there.
func holds(c *Copy) bool {
	return (c.State == solver.CopyOK || c.State == solver.CopySurplus) && c.GoneKind == "" && c.OldPath == ""
}

// Present says what a path presents, from the catalog's rows for it: nil when it is absent (specs.md §21.2). The first
// disk that holds a copy ok or surplus of the file, or an entry, says what the path is, as mergerfs's ff does.
func Present(disks []Disk, file *File, entries []Entry, self Owner) (*Node, error) {
	rank := presentOrder(disks)
	best, copyFirst := -1, false
	var entry *Entry
	if file != nil {
		for i := range file.Copies {
			if r, ok := rank[file.Copies[i].Disk]; ok && holds(&file.Copies[i]) && (best < 0 || r < best) {
				best, copyFirst = r, true
			}
		}
	}
	for i := range entries {
		// A copy at the same rank comes first: one disk holds one thing at a path, and its file's row is the newer.
		if r, ok := rank[entries[i].Disk]; ok && (best < 0 || r < best) {
			best, copyFirst, entry = r, false, &entries[i]
		}
	}
	switch {
	case best < 0:
		return nil, nil
	case copyFirst:
		return presentFile(disks, rank, file, self)
	default:
		return presentEntry(entry, self), nil
	}
}

// presentFile presents a file's original (specs.md §7), or, when no copy is eligible, its first copy on a failed disk.
func presentFile(disks []Disk, rank map[string]int, file *File, self Owner) (*Node, error) {
	position := map[string]int{}
	state := map[string]solver.DiskState{}
	for _, d := range disks {
		position[d.ID], state[d.ID] = d.Position, d.State
	}
	candidates := make([]solver.Candidate, 0, len(file.Copies))
	for _, c := range file.Copies {
		dstate, ok := state[c.Disk]
		if _, ranked := rank[c.Disk]; !ok || !ranked || !holds(&c) {
			continue
		}
		candidates = append(candidates, solver.Candidate{Disk: c.Disk, Position: position[c.Disk], State: c.State, DiskState: dstate,
			CreationNS: c.CreationNS, FirstSeenNS: c.FirstSeenNS})
	}
	previous := ""
	if file.State == solver.StateConflict || file.ConflictHeld {
		previous = file.Original
	}
	original, err := solver.SelectOriginal(candidates, previous)
	if err != nil {
		return nil, fmt.Errorf("view: %w", err)
	}
	var chosen *Copy
	for i := range file.Copies {
		c := &file.Copies[i]
		r, ok := rank[c.Disk]
		switch {
		case original != "":
			if c.Disk == original && holds(c) {
				chosen = c
			}
		case ok && holds(c) && (chosen == nil || r < rank[chosen.Disk]):
			chosen = c
		}
	}
	if chosen == nil {
		return nil, nil
	}
	n := &Node{Kind: NodeFile, Disk: chosen.Disk, Mode: defaultFileMode, UID: self.UID, GID: self.GID, Size: file.Size, MtimeNS: file.MtimeNS, Nlink: 1}
	if chosen.Mode != nil {
		n.Mode = *chosen.Mode
	}
	if chosen.UID != nil && chosen.GID != nil {
		n.UID, n.GID = *chosen.UID, *chosen.GID
	}
	if chosen.Size != nil {
		n.Size = *chosen.Size
	}
	if chosen.MtimeNS != nil {
		n.MtimeNS = *chosen.MtimeNS
	}
	if chosen.Nlink != nil {
		n.Nlink = *chosen.Nlink
	}
	return n, nil
}

// presentEntry presents a folder, a symlink or an ignored file from its first disk's row.
func presentEntry(e *Entry, self Owner) *Node {
	n := &Node{Kind: NodeFile, Disk: e.Disk, Ignored: true, Mode: defaultFileMode, UID: self.UID, GID: self.GID, Nlink: 1}
	switch e.Kind {
	case solver.EntryDir:
		n.Kind, n.Ignored, n.Mode = NodeDir, false, defaultDirMode
	case solver.EntrySymlink:
		n.Kind, n.Ignored, n.Mode, n.Target, n.Size = NodeSymlink, false, defaultDirMode, e.Target, int64(len(e.Target))
	case solver.EntryIgnored:
	}
	if e.Mode != nil {
		n.Mode = *e.Mode
	}
	if e.UID != nil && e.GID != nil {
		n.UID, n.GID = *e.UID, *e.GID
	}
	if e.Size != nil {
		n.Size = *e.Size
	}
	if e.MtimeNS != nil {
		n.MtimeNS = *e.MtimeNS
	}
	return n
}

// Statfs is what statfs gives of the view.
type Statfs struct {
	Bsize, Blocks, Bfree, Bavail, Namemax uint64
}

// blockSize is the block statfs counts in.
const blockSize = 4096

// StatfsOf sums the data disks that present, from what the catalog holds (specs.md §21.2): the size and the free
// space, each disk rounded down to whole blocks; the free space is also the space available.
func StatfsOf(disks []Disk) Statfs {
	out := Statfs{Bsize: blockSize, Namemax: 255}
	rank := presentOrder(disks)
	for _, d := range disks {
		if _, ok := rank[d.ID]; !ok || d.Cache {
			continue
		}
		if d.Capacity != nil && *d.Capacity > 0 {
			out.Blocks += uint64(*d.Capacity) / blockSize
		}
		if d.Free != nil && *d.Free > 0 {
			out.Bfree += uint64(*d.Free) / blockSize
		}
	}
	out.Bavail = out.Bfree
	return out
}
