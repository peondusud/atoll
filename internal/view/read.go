package view

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// ReadPolicy is [read] of the configuration (specs.md §7): which in-sync copy of a file the view reads.
type ReadPolicy struct {
	// PreferSpinning is read.policy = "prefer-spinning"; false is "oldest", which reads the original alone.
	PreferSpinning bool
	// Classes is read.classes: tags, a disk that carries an earlier one read first.
	Classes []string
}

// ReadOrder gives the disks whose copy of a file the view opens, best first (specs.md §7, §21.2,
// conformance/vectors/view_read.json). With "oldest", the copy presented alone. With "prefer-spinning", the copies that
// hold the file at its path on a data disk online, standby, readonly or draining, and the copy presented in any case:
// those on a disk not in standby first, then by the first class of read.classes the disk carries, then the copy
// presented, then by position.
func ReadOrder(disks []Disk, copies []Copy, presented string, p ReadPolicy) []string {
	if !p.PreferSpinning {
		return []string{presented}
	}
	byID := make(map[string]*Disk, len(disks))
	for i := range disks {
		byID[disks[i].ID] = &disks[i]
	}
	type reader struct {
		id                string
		asleep, presented bool
		class, position   int
	}
	var list []reader
	add := func(id string) {
		r := reader{id: id, presented: id == presented, class: len(p.Classes)}
		if d := byID[id]; d != nil {
			r.asleep, r.class, r.position = d.State == solver.DiskStandby, classOf(d.Tags, p.Classes), d.Position
		}
		list = append(list, r)
	}
	for i := range copies {
		c := &copies[i]
		d := byID[c.Disk]
		if c.Disk == presented || d == nil || d.Cache || !holds(c) || !readsFrom(d.State) || slices.ContainsFunc(list, func(r reader) bool { return r.id == c.Disk }) {
			continue
		}
		add(c.Disk)
	}
	add(presented)
	slices.SortStableFunc(list, func(a, b reader) int {
		return cmp.Or(boolFirst(!a.asleep, !b.asleep), cmp.Compare(a.class, b.class), boolFirst(a.presented, b.presented), cmp.Compare(a.position, b.position))
	})
	out := make([]string, len(list))
	for i := range list {
		out[i] = list[i].id
	}
	return out
}

// readsFrom says whether the view may read a copy on a disk in that state, as §7 makes it eligible to be the original.
func readsFrom(s solver.DiskState) bool {
	switch s {
	case solver.DiskOnline, solver.DiskStandby, solver.DiskReadOnly, solver.DiskDraining:
		return true
	case solver.DiskMissing, solver.DiskFailed, solver.DiskRetired:
	}
	return false
}

// classOf gives the rank of the first class of read.classes a disk carries, len(classes) for a disk that carries none.
func classOf(tags, classes []string) int {
	for i, class := range classes {
		if slices.Contains(tags, class) {
			return i
		}
	}
	return len(classes)
}

// boolFirst orders true before false.
func boolFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}

// WithRead sets how the view reads (specs.md §7), before it is served, and gives the tree.
func (t *Tree) WithRead(p ReadPolicy) *Tree {
	t.read = p
	return t
}

// WithCache says whether the view presents the cache disks, as Atoll's own view does with view.engine = "atoll", before
// it is served, and gives the tree (specs.md §19).
func (t *Tree) WithCache(inView bool) *Tree {
	t.cacheInView = inView
	return t
}

// Open opens the data of a file node for reading, one name at a time below a disk's root and following no symlink:
// from the disk the node presents with read.policy = "oldest", and from the first copy ReadOrder gives that opens with
// "prefer-spinning", which the file keeps until it is closed (specs.md §7, §21.2). With none, it gives the first one's
// error.
func (t *Tree) Open(ctx context.Context, path string, n *Node) (fsx.File, error) {
	order := []string{n.Disk}
	if t.read.PreferSpinning && n.Kind == NodeFile && !n.Ignored {
		var err error
		if order, err = t.readOrder(ctx, path, n); err != nil {
			return nil, err
		}
	}
	var first error
	for _, disk := range order {
		f, err := t.openOn(disk, path)
		if err == nil {
			return f, nil
		}
		if first == nil {
			first = err
		}
	}
	// The copy presented may have just left its name, as a move of Atoll's own to the trash leaves it until its commit:
	// the next copy in sync, in the order of §7, is opened in its place (specs.md §21.2; conformance/models/README.md,
	// flaw 9, the maintainer's remedy).
	if fsx.IsAbsent(first) && n.Kind == NodeFile && !n.Ignored {
		next, err := t.nextCopies(ctx, path, n, order)
		if err != nil {
			return nil, err
		}
		for _, disk := range next {
			if f, err := t.openOn(disk, path); err == nil {
				return f, nil
			}
		}
	}
	return nil, first
}

// nextCopies gives the disks of the other copies that hold a file at its path, those Open did not try, in the order of
// §7: the original among them, then the original of the rest, and so on. None when the path presents another thing now,
// or what the view's own operations made, which no row holds.
func (t *Tree) nextCopies(ctx context.Context, path string, n *Node, tried []string) ([]string, error) {
	disks, err := t.disks(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := t.ops(ctx)
	if err != nil {
		return nil, err
	}
	s := Resolve(ops, path)
	if s.Kind != FromCatalog {
		return nil, nil
	}
	rows, err := catalog.ViewRowsOf(ctx, t.db, s.Paths)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for _, p := range s.Paths {
		file := fileOf(rows[p].File)
		if file == nil {
			continue
		}
		rest := *file
		rest.Copies = slices.DeleteFunc(slices.Clone(file.Copies), func(c Copy) bool { return slices.Contains(tried, c.Disk) })
		var out []string
		for len(rest.Copies) > 0 {
			got, err := Present(disks, &rest, nil, t.self)
			if err != nil {
				return nil, err
			}
			if got == nil || got.Kind != NodeFile || got.Ignored {
				break
			}
			out = append(out, got.Disk)
			rest.Copies = slices.DeleteFunc(rest.Copies, func(c Copy) bool { return c.Disk == got.Disk })
		}
		return out, nil
	}
	return nil, nil
}

// readOrder reads a file's rows again, and gives ReadOrder of them: the copy a node presents alone when the path
// presents another thing now, or what no row holds, as what the view's own operations made.
func (t *Tree) readOrder(ctx context.Context, path string, n *Node) ([]string, error) {
	disks, err := t.disks(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := t.ops(ctx)
	if err != nil {
		return nil, err
	}
	s := Resolve(ops, path)
	if s.Kind != FromCatalog {
		return []string{n.Disk}, nil
	}
	rows, err := catalog.ViewRowsOf(ctx, t.db, s.Paths)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for _, p := range s.Paths {
		at := rows[p]
		file := fileOf(at.File)
		got, err := Present(disks, file, entriesOf(at.Entries), t.self)
		switch {
		case err != nil:
			return nil, err
		case got == nil:
			continue
		case got.Kind != NodeFile || got.Ignored || file == nil || got.Disk != n.Disk:
			return []string{n.Disk}, nil
		}
		return ReadOrder(disks, file.Copies, n.Disk, t.read), nil
	}
	return []string{n.Disk}, nil
}

// openOn opens the data of a file on one disk.
func (t *Tree) openOn(disk, path string) (fsx.File, error) {
	var f fsx.File
	err := t.roots.Root(disk, func(root fsx.Dir) error {
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
