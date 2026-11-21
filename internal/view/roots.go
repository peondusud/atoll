package view

import (
	"sync"

	"github.com/peondusud/atoll/internal/fsx"
)

// Roots gives the root folder of a disk, `<mount>/<root>`, as a mount check confirmed it in this session, for the time
// a use of it takes, and ErrUnreadable for a disk that cannot be read now (specs.md §10, §21.2).
type Roots interface {
	Root(disk string, use func(fsx.Dir) error) error
}

// DiskRoots holds the root of each disk a mount check confirmed. A root is replaced or dropped only once no use of it
// is under way, so that a descriptor is never closed, and its number given to another file, while a read opens a copy
// through it. Each disk has a lock of its own: an open that hangs on a disk in trouble holds back that disk's
// replacement, never the reads of the others.
type DiskRoots struct {
	mu    sync.Mutex
	disks map[string]*diskRoot
}

// diskRoot is the root of one disk, nil when it has none.
type diskRoot struct {
	mu   sync.RWMutex
	root fsx.Dir
}

// NewDiskRoots makes an empty set of roots.
func NewDiskRoots() *DiskRoots {
	return &DiskRoots{disks: map[string]*diskRoot{}}
}

// slot gives the root of a disk, made empty when the disk has none yet.
func (r *DiskRoots) slot(disk string) *diskRoot {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.disks[disk]
	if !ok {
		d = &diskRoot{}
		r.disks[disk] = d
	}
	return d
}

// Root runs use with the root of a disk.
func (r *DiskRoots) Root(disk string, use func(fsx.Dir) error) error {
	d := r.slot(disk)
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.root == nil {
		return ErrUnreadable
	}
	return use(d.root)
}

// Set gives a disk the root its last check confirmed, and closes the one it had.
func (r *DiskRoots) Set(disk string, root fsx.Dir) {
	r.slot(disk).replace(root)
}

// Drop takes the root of a disk its check did not confirm, and closes it.
func (r *DiskRoots) Drop(disk string) {
	r.slot(disk).replace(nil)
}

// Close drops every root.
func (r *DiskRoots) Close() {
	r.mu.Lock()
	list := make([]*diskRoot, 0, len(r.disks))
	for _, d := range r.disks {
		list = append(list, d)
	}
	r.mu.Unlock()
	for _, d := range list {
		d.replace(nil)
	}
}

// replace puts a root in place of the one the disk had, once no use of it is under way, and closes that one.
func (d *diskRoot) replace(root fsx.Dir) {
	d.mu.Lock()
	old := d.root
	d.root = root
	d.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}
