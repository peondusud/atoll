package view

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// Tree answers what Atoll's own view presents, from the catalog alone, so that browsing wakes no disk (specs.md
// §21.2). Only Open reads a disk.
type Tree struct {
	db    *sql.DB
	self  Owner
	roots Roots
	// mountedNS is the time the view was mounted, which its root presents.
	mountedNS int64
}

// NewTree makes the tree of a view, mounted at mountedNS, whose own process is owned by self.
func NewTree(db *sql.DB, roots Roots, self Owner, mountedNS int64) *Tree {
	return &Tree{db: db, self: self, roots: roots, mountedNS: mountedNS}
}

// ErrUnreadable says that the disk a node presents cannot be read now.
var ErrUnreadable = errors.New("the disk of that copy cannot be read now")

// Root is what the root presents: a folder, 0755, owned by the view's own process, of the time it was mounted.
func (t *Tree) Root() *Node {
	return &Node{Kind: NodeDir, Mode: defaultDirMode, UID: t.self.UID, GID: t.self.GID, MtimeNS: t.mountedNS, Nlink: 1}
}

// Lookup says what a path presents, nil when it is absent. The path is the logical path, "" for the root. The view's
// own operations that no pass has taken in are laid over the catalog's rows (specs.md §21.2): what one made or wrote
// is looked at on its disk, and ErrUnreadable says that its disk cannot be read now.
func (t *Tree) Lookup(ctx context.Context, path string) (*Node, error) {
	if path == "" {
		return t.Root(), nil
	}
	disks, err := t.disks(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := t.ops(ctx)
	if err != nil {
		return nil, err
	}
	s := Resolve(ops, path)
	var rows map[string]catalog.ViewAt
	if s.Kind == FromCatalog {
		if rows, err = catalog.ViewRowsOf(ctx, t.db, s.Paths); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
	return t.present(disks, s, path, rows)
}

// present says what a path presents from what it resolves to: from the catalog, the rows at the first of its paths
// that presents something; from a disk, what a look finds there.
func (t *Tree) present(disks []Disk, s Source, path string, rows map[string]catalog.ViewAt) (*Node, error) {
	switch s.Kind {
	case Absent:
		return nil, nil
	case FromDisk:
		return t.look(s.Disk, path)
	case FromCatalog:
	}
	for _, p := range s.Paths {
		at := rows[p]
		n, err := Present(disks, fileOf(at.File), entriesOf(at.Entries), t.self)
		if err != nil || n != nil {
			return n, err
		}
	}
	return nil, nil
}

// ops gives the view's own operations that no pass has taken in, oldest first.
func (t *Tree) ops(ctx context.Context) ([]Op, error) {
	rows, err := catalog.OpenViewOps(ctx, t.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := make([]Op, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, Op{Kind: r.Kind, Disk: r.Disk, Path: r.Path, Path2: r.Path2, Folder: r.Folder})
	}
	return out, nil
}

// look says what a disk holds at a path, as a look there finds it, nil when it holds nothing. A folder presents one
// link, as one of the catalog does (specs.md §21.2).
func (t *Tree) look(disk, path string) (*Node, error) {
	var n *Node
	err := t.roots.Root(disk, func(root fsx.Dir) error {
		return walk(root, path, func(dir fsx.Dir, name string) error {
			st, err := dir.Stat(name)
			if err != nil {
				return fmt.Errorf("%w", err)
			}
			target := ""
			if st.Kind == fsx.KindSymlink {
				if target, err = dir.ReadLink(name); err != nil {
					return fmt.Errorf("%w", err)
				}
			}
			n = NodeOfStat(disk, &st, target)
			return nil
		})
	})
	switch {
	case fsx.IsAbsent(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return n, nil
}

// walk opens each folder of a path below a root, one name at a time and following no symlink, and gives the last
// one, with the path's last name, to use.
func walk(root fsx.Dir, path string, use func(dir fsx.Dir, name string) error) error {
	parts := strings.Split(path, "/")
	dir := root
	for i, name := range parts[:len(parts)-1] {
		next, err := dir.OpenDir(name)
		if i > 0 {
			_ = dir.Close()
		}
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		dir = next
	}
	if len(parts) > 1 {
		defer func() { _ = dir.Close() }()
	}
	return use(dir, parts[len(parts)-1])
}

// Child is a name directly below a folder, and what it presents.
type Child struct {
	Name string
	Node Node
}

// List gives each name directly below a folder that presents something, with what it presents, in byte order
// (specs.md §21.2).
func (t *Tree) List(ctx context.Context, dir string) ([]Child, error) {
	disks, err := t.disks(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := t.ops(ctx)
	if err != nil {
		return nil, err
	}
	l := ListOf(ops, dir)
	set := map[string]bool{}
	for _, name := range l.Names {
		set[name] = true
	}
	for _, d := range l.CatalogDirs {
		names, err := catalog.ViewNames(ctx, t.db, d)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		for _, name := range names {
			set[name] = true
		}
	}
	names := slices.Sorted(maps.Keys(set))
	sources := make([]Source, len(names))
	var paths []string
	for i, name := range names {
		sources[i] = Resolve(ops, join(dir, name))
		paths = append(paths, sources[i].Paths...)
	}
	rows, err := catalog.ViewRowsOf(ctx, t.db, paths)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := make([]Child, 0, len(names))
	for i, name := range names {
		n, err := t.present(disks, sources[i], join(dir, name), rows)
		switch {
		case errors.Is(err, ErrUnreadable):
			// What the view made on a disk that cannot be read now is left out of a listing, as a file none of whose
			// copies can be presented is.
		case err != nil:
			return nil, err
		case n != nil:
			out = append(out, Child{Name: name, Node: *n})
		}
	}
	return out, nil
}

// join gives the logical path of a name in a folder, "" for the root.
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// Statfs sums the disks, from the catalog.
func (t *Tree) Statfs(ctx context.Context) (Statfs, error) {
	disks, err := t.disks(ctx)
	if err != nil {
		return Statfs{}, err
	}
	return StatfsOf(disks), nil
}

// Open opens the data of a file node for reading, from the disk it presents, one name at a time below the root and
// following no symlink.
func (t *Tree) Open(path string, n *Node) (fsx.File, error) {
	var f fsx.File
	err := t.roots.Root(n.Disk, func(root fsx.Dir) error {
		return walk(root, path, func(dir fsx.Dir, name string) error {
			opened, err := dir.OpenFile(name)
			if err != nil {
				return fmt.Errorf("%w", err)
			}
			f = opened
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (t *Tree) disks(ctx context.Context) ([]Disk, error) {
	rows, err := catalog.ViewDisks(ctx, t.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := make([]Disk, 0, len(rows))
	for _, r := range rows {
		d := Disk{ID: r.ID, Position: r.Position, State: solver.DiskState(r.State), Cache: r.Cache}
		if r.Capacity.Valid {
			d.Capacity = &r.Capacity.Int64
		}
		if r.Free.Valid {
			d.Free = &r.Free.Int64
		}
		out = append(out, d)
	}
	return out, nil
}

func fileOf(f *catalog.ViewFile) *File {
	if f == nil {
		return nil
	}
	out := &File{State: solver.State(f.State), Original: f.Original, ConflictHeld: f.ConflictHeld, Size: f.Size, MtimeNS: f.MtimeNS}
	for i := range f.Copies {
		c := &f.Copies[i]
		out.Copies = append(out.Copies, Copy{
			Disk: c.Disk, State: solver.CopyState(c.State), Size: int64Of(c.Size), MtimeNS: int64Of(c.MtimeNS), Nlink: int64Of(c.Nlink),
			Mode: uint32Of(c.Mode), UID: uint32Of(c.UID), GID: uint32Of(c.GID), CreationNS: int64Of(c.BtimeNS), FirstSeenNS: c.FirstSeenNS,
			GoneKind: c.GoneKind, OldPath: c.OldPath,
		})
	}
	return out
}

func entriesOf(rows []catalog.ViewEntry) []Entry {
	out := make([]Entry, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, Entry{
			Disk: r.Disk, Kind: solver.EntryKind(r.Kind), Mode: uint32Of(r.Mode), UID: uint32Of(r.UID), GID: uint32Of(r.GID),
			Size: int64Of(r.Size), MtimeNS: int64Of(r.MtimeNS), Target: string(r.Target),
		})
	}
	return out
}

func int64Of(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func uint32Of(v sql.NullInt64) *uint32 {
	if !v.Valid || v.Int64 < 0 || v.Int64 > 1<<32-1 {
		return nil
	}
	u := uint32(v.Int64)
	return &u
}
