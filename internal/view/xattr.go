package view

import (
	"fmt"
	"maps"
	"slices"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// Xattrs gives the extended attributes a path presents, those of the user. namespace, from the copy or the entry it
// presents, on its disk alone, which a read wakes as a read of the file's data does: never from another copy, whose
// set may lag behind the one the view changes (specs.md §21.2). The root and what has no disk present none.
// ErrUnreadable says that its disk cannot be read now.
func (t *Tree) Xattrs(path string, n *Node) (map[string][]byte, error) {
	if path == "" || n.Disk == "" {
		return map[string][]byte{}, nil
	}
	var out map[string][]byte
	err := t.roots.Root(n.Disk, func(root fsx.Dir) error {
		return walk(root, path, func(dir fsx.Dir, name string) error {
			attrs, err := dir.Xattrs(name, fsx.UserNamespace)
			if err != nil {
				return fmt.Errorf("%w", err)
			}
			out = attrs
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	maps.DeleteFunc(out, func(name string, _ []byte) bool { return !solver.KeptXattr(name) })
	return out, nil
}

// XattrNames gives the names of what Xattrs gives, in byte order.
func XattrNames(attrs map[string][]byte) []string {
	return slices.Sorted(maps.Keys(attrs))
}
