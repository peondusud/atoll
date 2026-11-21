package disks

import "github.com/peondusud/atoll/internal/config"

// Kind is what a disk's filesystem lies on, as the copies at once it takes go by it (specs.md §14, the maintainer,
// 2026-11-19): on a spindle, one copy at a time, as two make its head seek between them; on flash, several.
type Kind string

// The kinds of a disk.
const (
	// KindRotational is a disk with a spindle, behind a USB bridge or not, or one whose kind no one could tell.
	KindRotational Kind = "rotational"
	// KindSolid is a disk with no spindle under it: a SATA SSD, an NVMe drive.
	KindSolid Kind = "solid"
)

// ReadsKinds says whether a machine can tell the kind of a disk: the whole disks below a filesystem, and whether each
// has a spindle. One that cannot, as the conformance build, whose disks are folders, runs one copy at a time, whatever
// its disks (specs.md §14, §29.4).
func ReadsKinds(m Machine) bool {
	_, reads := m.(SpinReader)
	_, tells := m.(Topology)
	return reads && tells
}

// KindOf says what a disk's filesystem lies on, as the kernel tells it, waking no disk: solid when every whole disk below
// it says it has no spindle (/sys/block/<name>/queue/rotational). A whole disk that has one, a filesystem over several
// whole disks one of which has one, a whole disk that gives no answer, a filesystem not mounted or whose whole disks
// cannot be told, and a machine that cannot tell, all count as rotational: the safest, as two copies at once on a spindle
// only slow both, while one at a time on flash costs nothing but time.
func KindOf(m Machine, d *config.Disk) Kind {
	reader, reads := m.(SpinReader)
	topology, tells := m.(Topology)
	if !reads || !tells {
		return KindRotational
	}
	seen := m.Mount(d.Mount)
	if seen.Entry == nil {
		return KindRotational
	}
	names, err := topology.PhysicalDisks(d.FS, seen.Entry, d.UUID)
	if err != nil || len(names) == 0 {
		return KindRotational
	}
	for _, name := range names {
		if rotational, err := reader.Rotational(name); err != nil || rotational {
			return KindRotational
		}
	}
	return KindSolid
}

// CopyLimit is how many copies may read or write a disk at once (specs.md §14): replication.max_parallel_per_disk when it
// is set, on every disk alike, as format version 1 meant it; otherwise max_parallel_per_ssd on a disk with no spindle,
// and max_parallel_per_hdd on one with a spindle, or whose kind the machine cannot tell.
func CopyLimit(m Machine, d *config.Disk, set config.CopyLimits) int {
	switch {
	case set.PerDisk > 0:
		return set.PerDisk
	case KindOf(m, d) == KindSolid:
		return set.SSD
	}
	return set.HDD
}
