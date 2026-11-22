package watch

import (
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
)

// ViewOp is an operation of Atoll's own view no pass has taken in, as a pass looks at what it did (specs.md §15.13,
// §21.2): its kind, as view_ops holds it, the disk it ran on, its paths, whether it names a folder, and, for a folder,
// the disks whose catalog holds a folder at either path, as the view renamed or removed it on each.
type ViewOp struct {
	Kind, Disk, Path, Path2 string
	Folder                  bool
	Holders                 []string
	// Disk2 is, for a move, the full disk the file leaves (specs.md §21.2).
	Disk2 string
}

// ViewNames gives what a pass looks at for the operations of Atoll's own view no pass has taken in, by disk, as
// HeardNames gives what the watcher heard (specs.md §15.13, §21.2; conformance/vectors/view_names.json): each path an
// operation touched, on its disk, and a folder's on every disk that holds it; a folder walked below it; the name a
// deletion or a removal left, deleted, as the copy the view sent to the trash is gone by the user; a move's on the disk
// it goes to and on the disk it leaves, neither deleted, as what a move sends to the trash is Atoll's own; past maxNames names
// on a disk, the whole disk. The names of a disk are in byte order, each once. A look limited to those names finds what
// a walk of the whole disk would find there, and the catalog's rows below a folder, so that a pass takes in what the
// view did without walking the disks it did it on whole.
func ViewNames(ops []ViewOp, maxNames int) map[string]Heard {
	out := map[string]Heard{}
	for i := range ops {
		v := &ops[i]
		folder := v.Folder || v.Kind == catalog.ViewMkdir || v.Kind == catalog.ViewRmdir
		deleted := v.Kind == catalog.ViewUnlink || v.Kind == catalog.ViewRmdir
		disks := []string{v.Disk}
		if v.Kind == catalog.ViewMove && v.Disk2 != "" && v.Disk2 != v.Disk {
			// The disk it leaves, whose copy went to the trash as Atoll's own: no deletion there.
			disks = append(disks, v.Disk2)
		}
		if folder {
			for _, id := range v.Holders {
				if !slices.Contains(disks, id) {
					disks = append(disks, id)
				}
			}
		}
		for _, id := range disks {
			h := out[id]
			h.Names = append(h.Names, Named{Path: v.Path, Folder: folder, Deleted: deleted})
			if v.Path2 != "" {
				h.Names = append(h.Names, Named{Path: v.Path2, Folder: folder})
			}
			out[id] = h
		}
	}
	for id, h := range out {
		out[id] = settleHeard(h, maxNames)
	}
	return out
}
