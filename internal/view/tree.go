package view

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// Roots gives the root folder of a disk, `<mount>/<root>`, as the mount check confirmed it in this session, and false
// for a disk that cannot be read now (specs.md §10).
type Roots interface {
	Root(disk string) (fsx.Dir, bool)
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

// Lookup says what a path presents, nil when it is absent. The path is the logical path, "" for the root.
func (t *Tree) Lookup(ctx context.Context, path string) (*Node, error) {
	if path == "" {
		return t.Root(), nil
	}
	disks, err := t.disks(ctx)
	if err != nil {
		return nil, err
	}
	return t.lookup(ctx, disks, path)
}

func (t *Tree) lookup(ctx context.Context, disks []Disk, path string) (*Node, error) {
	f, rows, err := catalog.ViewRows(ctx, t.db, path)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return Present(disks, fileOf(f), entriesOf(rows), t.self)
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
	names, err := catalog.ViewNames(ctx, t.db, dir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = name
		if dir != "" {
			paths[i] = dir + "/" + name
		}
	}
	rows, err := catalog.ViewRowsOf(ctx, t.db, paths)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := make([]Child, 0, len(names))
	for i, name := range names {
		at := rows[paths[i]]
		n, err := Present(disks, fileOf(at.File), entriesOf(at.Entries), t.self)
		if err != nil {
			return nil, err
		}
		if n != nil {
			out = append(out, Child{Name: name, Node: *n})
		}
	}
	return out, nil
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
	root, ok := t.roots.Root(n.Disk)
	if !ok {
		return nil, ErrUnreadable
	}
	parts := strings.Split(path, "/")
	dir := root
	for i, name := range parts[:len(parts)-1] {
		next, err := dir.OpenDir(name)
		if i > 0 {
			_ = dir.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		dir = next
	}
	f, err := dir.OpenFile(parts[len(parts)-1])
	if len(parts) > 1 {
		_ = dir.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("%w", err)
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
