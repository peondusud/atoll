package view

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

// NodeOfStat presents what a look found on a disk, as the view presents what it made or wrote there (specs.md §21.2):
// a folder with one link, a symbolic link with its target, anything else as a file.
func NodeOfStat(disk string, st *fsx.Stat, target string) *Node {
	n := &Node{Kind: NodeFile, Disk: disk, Mode: st.Mode, UID: st.Uid, GID: st.Gid, Size: st.Size,
		MtimeNS: st.Mtime.Sec*1_000_000_000 + st.Mtime.Nsec, Nlink: int64(st.Nlink)}
	switch st.Kind {
	case fsx.KindDir:
		n.Kind, n.Nlink = NodeDir, 1
	case fsx.KindSymlink:
		n.Kind, n.Target, n.Size = NodeSymlink, target, int64(len(target))
	case fsx.KindFile, fsx.KindOther:
	}
	return n
}

// Look is what a pass finds on a disk at a path: nil when nothing is there, and false when it did not look, as on a
// disk it did not walk.
type Look func(ctx context.Context, disk, path string) (*Node, bool, error)

// TakenIn says which of the view's operations no pass has taken in a pass takes in (specs.md §21.2): each done, in the
// order of their ids, once the catalog alone presents at each path it touched what the view presents with all of them,
// and for a folder once every disk the pass could read was walked, so that nothing presented changes then. One that is
// not taken in holds back each later one at, below or above one of its paths. cacheInView says that the view presents
// the cache disks, as with view.engine = "atoll" (specs.md §19): what it made there, the catalog presents too.
func TakenIn(ctx context.Context, db *sql.DB, open []catalog.ViewOp, walkedAll, cacheInView bool, look Look) ([]int64, error) {
	t := &Tree{db: db, cacheInView: cacheInView}
	disks, err := t.disks(ctx)
	if err != nil {
		return nil, err
	}
	ops := make([]Op, 0, len(open))
	for i := range open {
		r := &open[i]
		ops = append(ops, Op{Kind: r.Kind, Disk: r.Disk, Path: r.Path, Path2: r.Path2, Folder: r.Folder})
	}
	var held []string
	var taken []int64
	for i := range open {
		v := &open[i]
		paths := []string{v.Path}
		if v.Path2 != "" {
			paths = append(paths, v.Path2)
		}
		blocked := slices.ContainsFunc(paths, func(p string) bool {
			return slices.ContainsFunc(held, func(h string) bool { return at(p, h) || at(h, p) })
		})
		ok := !blocked && v.State == catalog.OpDone && (!v.Folder || walkedAll)
		for j := 0; ok && j < len(paths); j++ {
			if ok, err = t.agrees(ctx, disks, ops, paths[j], look); err != nil {
				return nil, err
			}
		}
		if ok {
			taken = append(taken, v.ID)
		} else {
			held = append(held, paths...)
		}
	}
	return taken, nil
}

// agrees says whether the catalog alone presents at a path what the view presents with its operations.
func (t *Tree) agrees(ctx context.Context, disks []Disk, ops []Op, path string, look Look) (bool, error) {
	rows, err := catalog.ViewRowsOf(ctx, t.db, []string{path})
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	alone, err := t.present(disks, Source{Kind: FromCatalog, Paths: []string{path}}, path, rows)
	if err != nil {
		return false, err
	}
	s := Resolve(ops, path)
	switch s.Kind {
	case Absent:
		return alone == nil, nil
	case FromCatalog:
		over, err := catalog.ViewRowsOf(ctx, t.db, s.Paths)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		with, err := t.present(disks, s, path, over)
		if err != nil {
			return false, err
		}
		return (with == nil) == (alone == nil) && (with == nil || with.Kind == alone.Kind && with.Disk == alone.Disk), nil
	case FromDisk:
	}
	live, looked, err := look(ctx, s.Disk, path)
	switch {
	case err != nil:
		return false, err
	case !looked:
		return false, nil
	case live == nil || alone == nil:
		return live == nil && alone == nil, nil
	case live.Kind != alone.Kind:
		return false, nil
	}
	switch live.Kind {
	case NodeDir:
		return true, nil
	case NodeSymlink:
		return live.Target == alone.Target, nil
	case NodeFile:
	}
	return alone.Disk == live.Disk && alone.Size == live.Size && alone.MtimeNS == live.MtimeNS, nil
}
