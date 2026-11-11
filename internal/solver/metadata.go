package solver

import (
	"cmp"
	"fmt"
	"slices"
)

// MetaCopy is one copy of a file whose change is to its metadata only, as what that applies sees it (specs.md
// §15.4). Mtime and Ctime are what the look found of a changed copy; Reachable says that the pass can write to its
// disk, and that its look reached it. A pending copy is none: its disk holds nothing of it yet.
type MetaCopy struct {
	Disk      string
	Position  int
	Changed   bool
	State     CopyState
	RowMtime  int64
	Mtime     int64
	Ctime     int64
	Reachable bool
}

// MetaPlan is what a metadata-only change applies: Mtime, when HasMtime, the time of the copy From, to the copies in
// Apply through an operation, and through a refresh to those in Stale, each by position.
type MetaPlan struct {
	HasMtime bool
	Mtime    int64
	From     string
	Apply    []string
	Stale    []string
}

// MetadataPlan says what a metadata-only change applies to the other copies of a file (specs.md §15.4): the time of
// the changed copy whose time moved last, by change time, the first by position on a tie. An error means a state
// outside its closed list, a pending copy, or a disk or position that appears twice.
func MetadataPlan(copies []MetaCopy) (MetaPlan, error) {
	if err := checkMeta(copies); err != nil {
		return MetaPlan{}, err
	}
	sorted := make([]*MetaCopy, 0, len(copies))
	for i := range copies {
		sorted = append(sorted, &copies[i])
	}
	slices.SortFunc(sorted, func(a, b *MetaCopy) int { return cmp.Compare(a.Position, b.Position) })
	var last *MetaCopy
	for _, c := range sorted {
		if c.Changed && c.Mtime != c.RowMtime && (last == nil || c.Ctime > last.Ctime) {
			last = c
		}
	}
	var plan MetaPlan
	if last == nil {
		return plan, nil
	}
	plan.HasMtime, plan.Mtime, plan.From = true, last.Mtime, last.Disk
	for _, c := range sorted {
		now := c.RowMtime
		if c.Changed {
			now = c.Mtime
		}
		switch {
		case c == last || now == plan.Mtime || c.State == CopyStale:
		case c.Reachable:
			plan.Apply = append(plan.Apply, c.Disk)
		default:
			plan.Stale = append(plan.Stale, c.Disk)
		}
	}
	return plan, nil
}

func checkMeta(copies []MetaCopy) error {
	disks := make(map[string]bool, len(copies))
	positions := make(map[int]bool, len(copies))
	for i := range copies {
		c := &copies[i]
		if disks[c.Disk] || positions[c.Position] {
			return fmt.Errorf("metadata: disk %q or position %d appears twice", c.Disk, c.Position)
		}
		disks[c.Disk], positions[c.Position] = true, true
		switch c.State {
		case CopyOK, CopySurplus, CopyStale, CopyCorrupt, CopyUnavailable:
		case CopyPending:
			return fmt.Errorf("metadata: %q is pending, and has nothing on its disk", c.Disk)
		default:
			return fmt.Errorf("metadata: unknown copy state %q", c.State)
		}
	}
	return nil
}
