package view

import (
	"slices"
	"strings"
)

// The kinds of the view's own operations, as view_ops holds them (specs.md §9).
const (
	OpCreate  = "create"
	OpMkdir   = "mkdir"
	OpSymlink = "symlink"
	OpWrite   = "write"
	OpRename  = "rename"
	OpReplace = "replace"
	OpUnlink  = "unlink"
	OpRmdir   = "rmdir"
)

// Op is an operation of the view that no pass has taken in, as Resolve reads it (specs.md §21.2).
type Op struct {
	Kind string
	Disk string
	Path string
	// Path2 is the new path of a rename or a replacement.
	Path2  string
	Folder bool
}

// SourceKind says where what a path presents comes from.
type SourceKind string

// Where what a path presents comes from.
const (
	FromCatalog SourceKind = "catalog"
	FromDisk    SourceKind = "disk"
	Absent      SourceKind = "absent"
)

// Source is what a path resolves to once the view's own operations are laid over the catalog (specs.md §21.2).
type Source struct {
	Kind SourceKind
	// Disk is, from a disk, the disk whose look at the path presents it.
	Disk string
	// Paths are, from the catalog, the paths whose rows present it, the first that presents something: below a folder
	// renamed, the path below its old name, then below each later one, the path itself last, as a pass may have
	// recorded the rename before taking it in.
	Paths []string
}

// at says whether path is p or below it, by whole components.
func at(path, p string) bool {
	return path == p || strings.HasPrefix(path, p+"/")
}

// Resolve says what a path resolves to, the view's own operations given oldest first, the newest deciding: a path
// made, written or renamed to presents what its disk holds there; one deleted, removed, replaced or renamed from, or
// below a folder removed or renamed from, presents nothing; any other, the catalog's rows, below a folder renamed at
// its old name (specs.md §21.2). A folder is renamed on every disk that holds it, so a disk always holds a path at the
// path the view presents it at.
func Resolve(ops []Op, path string) Source {
	q := path
	var later []string
	for i := len(ops) - 1; i >= 0; i-- {
		o := &ops[i]
		switch o.Kind {
		case OpCreate, OpMkdir, OpSymlink, OpWrite:
			if q == o.Path {
				return Source{Kind: FromDisk, Disk: o.Disk}
			}
		case OpUnlink:
			if q == o.Path {
				return Source{Kind: Absent}
			}
		case OpRmdir:
			if at(q, o.Path) {
				return Source{Kind: Absent}
			}
		case OpRename, OpReplace:
			switch {
			case o.Folder && at(q, o.Path2):
				later = append(later, q)
				q = o.Path + q[len(o.Path2):]
			case o.Folder && at(q, o.Path):
				return Source{Kind: Absent}
			case !o.Folder && q == o.Path2:
				return Source{Kind: FromDisk, Disk: o.Disk}
			case !o.Folder && q == o.Path:
				return Source{Kind: Absent}
			}
		}
	}
	paths := []string{q}
	for i := len(later) - 1; i >= 0; i-- {
		paths = append(paths, later[i])
	}
	return Source{Kind: FromCatalog, Paths: paths}
}

// Listing is what a folder's listing may hold once the view's own operations are laid over the catalog: the names the
// catalog lists at each of CatalogDirs, and Names, which the operations made or renamed directly below it, in byte
// order. Each is then resolved on its own, and one that presents nothing left out (specs.md §21.2).
type Listing struct {
	CatalogDirs []string
	Names       []string
}

// ListOf says what a folder's listing may hold, "" for the root.
func ListOf(ops []Op, dir string) Listing {
	var out Listing
	switch s := Resolve(ops, dir); s.Kind {
	case FromCatalog:
		out.CatalogDirs = s.Paths
	case FromDisk:
		out.CatalogDirs = []string{dir}
	case Absent:
		return out
	}
	for k := range ops {
		var target string
		switch o := &ops[k]; o.Kind {
		case OpCreate, OpMkdir, OpSymlink, OpWrite:
			target = o.Path
		case OpRename, OpReplace:
			target = o.Path2
		default:
			continue
		}
		for _, newer := range ops[k+1:] {
			if newer.Kind == OpRename && newer.Folder && at(target, newer.Path) {
				target = newer.Path2 + target[len(newer.Path):]
			}
		}
		parent, name := "", target
		if i := strings.LastIndexByte(target, '/'); i >= 0 {
			parent, name = target[:i], target[i+1:]
		}
		if parent == dir && !slices.Contains(out.Names, name) {
			out.Names = append(out.Names, name)
		}
	}
	slices.Sort(out.Names)
	return out
}
