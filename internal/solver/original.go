package solver

import (
	"cmp"
	"errors"
	"fmt"
)

// CopyState is the state of one copy of a file (specs.md §4).
type CopyState string

// The states of a copy.
const (
	CopyOK          CopyState = "ok"
	CopyPending     CopyState = "pending"
	CopyStale       CopyState = "stale"
	CopyCorrupt     CopyState = "corrupt"
	CopyUnavailable CopyState = "unavailable"
	CopySurplus     CopyState = "surplus"
)

// DiskState is the state of a disk (specs.md §4).
type DiskState string

// The states of a disk.
const (
	DiskOnline   DiskState = "online"
	DiskStandby  DiskState = "standby"
	DiskMissing  DiskState = "missing"
	DiskReadOnly DiskState = "readonly"
	DiskDraining DiskState = "draining"
	DiskFailed   DiskState = "failed"
	DiskRetired  DiskState = "retired"
)

// Candidate is one copy of a file, as the choice of the original sees it.
type Candidate struct {
	Disk        string
	Position    int // the disk's order in the configuration
	State       CopyState
	DiskState   DiskState
	CreationNS  *int64 // nil when the creation time is absent (see CreationTimeNS)
	FirstSeenNS int64
}

// SelectOriginal returns the disk of the original of a file (specs.md §7), or ""
// when no copy is eligible. previous is the previous original of a file in
// conflict, which stays while its copy is eligible; it is "" for any other file.
// An error means that a state is unknown, that a disk has no name, or that a disk
// or a position appears twice.
func SelectOriginal(copies []Candidate, previous string) (string, error) {
	disks := map[string]bool{}
	positions := map[int]bool{}
	var best *Candidate
	kept := false
	for i := range copies {
		c := &copies[i]
		if c.Disk == "" {
			return "", errors.New("original: a copy with no disk")
		}
		if disks[c.Disk] || positions[c.Position] {
			return "", fmt.Errorf("original: disk %q or position %d appears twice", c.Disk, c.Position)
		}
		disks[c.Disk], positions[c.Position] = true, true
		eligible, err := isEligible(c)
		if err != nil {
			return "", err
		}
		if !eligible {
			continue
		}
		if c.Disk == previous {
			kept = true
		}
		if best == nil || older(c, best) {
			best = c
		}
	}
	switch {
	case kept:
		return previous, nil
	case best == nil:
		return "", nil
	default:
		return best.Disk, nil
	}
}

func isEligible(c *Candidate) (bool, error) {
	copyOK := false
	switch c.State {
	case CopyOK, CopySurplus:
		copyOK = true
	case CopyPending, CopyStale, CopyCorrupt, CopyUnavailable:
	default:
		return false, fmt.Errorf("original: unknown copy state %q", c.State)
	}
	switch c.DiskState {
	case DiskOnline, DiskStandby, DiskReadOnly, DiskDraining:
		return copyOK, nil
	case DiskMissing, DiskFailed, DiskRetired:
		return false, nil
	default:
		return false, fmt.Errorf("original: unknown disk state %q", c.DiskState)
	}
}

// older reports whether a comes before b: by its creation time or, where that is
// absent, its first_seen, in exact nanoseconds; then its first_seen; then its
// position. cmp.Compare never subtracts, so the limits of int64 cannot wrap.
func older(a, b *Candidate) bool {
	return cmp.Or(
		cmp.Compare(effectiveTime(a), effectiveTime(b)),
		cmp.Compare(a.FirstSeenNS, b.FirstSeenNS),
		cmp.Compare(a.Position, b.Position),
	) < 0
}

func effectiveTime(c *Candidate) int64 {
	if c.CreationNS != nil {
		return *c.CreationNS
	}
	return c.FirstSeenNS
}
