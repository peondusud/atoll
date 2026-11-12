package disks

import "github.com/peondusud/atoll/internal/names"

// Topology is what atoll disk add asks of a machine beyond the mount check (specs.md §10, R-06): which whole disks
// hold a filesystem, so that two pool filesystems never share one, and whether the folder a mount covers is
// immutable, so that nothing can write there while the disk is unmounted. A test double need not have it: then
// neither is checked, and disk add says so.
type Topology interface {
	// PhysicalDisks gives the whole disks below a mounted filesystem, by name, in byte order.
	PhysicalDisks(fs names.Filesystem, entry *MountEntry, uuid string) ([]string, error)
	// MountPointImmutable says whether the folder under a mount point has the immutable flag.
	MountPointImmutable(mount string) (bool, error)
}
