package solver

import (
	"cmp"
	"fmt"
	"slices"
)

// PruneOutcome says what atoll prune does with a surplus copy (specs.md §17.3).
type PruneOutcome string

// The outcomes the decision gives. The others come from trying the move.
const (
	PruneToTrash         PruneOutcome = "to-trash"
	PruneConflict        PruneOutcome = "conflict"
	PruneCorrupt         PruneOutcome = "corrupt"
	PruneDiskUnavailable PruneOutcome = "disk-unavailable"
	PruneChanged         PruneOutcome = "changed"
	PruneUnverified      PruneOutcome = "unverified"
	PruneBelowLevel      PruneOutcome = "below-level"
	PruneLastUserCopy    PruneOutcome = "last-user-copy"
)

// PruneCopy is one copy of a file, as the decision sees it. Unchanged says that a look at the disk
// just now found what its row says, so a copy that could not be looked at is not.
type PruneCopy struct {
	Disk      string
	Position  int
	State     CopyState
	CreatedBy string
	DiskState DiskState
	Verified  bool
	Unchanged bool
}

// PruneInput is one file.
type PruneInput struct {
	State    State
	Replicas int
	HasHash  bool
	Copies   []PruneCopy
}

// PruneResult is what happens to one surplus copy.
type PruneResult struct {
	Disk    string
	Outcome PruneOutcome
}

// DecidePrune says what atoll prune does with each surplus copy of a file, by position (specs.md
// §17.3). It moves nothing, and removes nothing a copy that stays does not cover. An error means
// that a value is outside its closed list, or that a disk or a position appears twice.
func DecidePrune(in PruneInput) ([]PruneResult, error) {
	if err := checkPrune(&in); err != nil {
		return nil, err
	}
	copies := slices.Clone(in.Copies)
	slices.SortFunc(copies, func(a, b PruneCopy) int { return cmp.Compare(a.Position, b.Position) })
	stay := staying(copies)
	level := in.Replicas + 1
	results := []PruneResult{}
	for i := range copies {
		c := &copies[i]
		if c.State == CopySurplus {
			results = append(results, PruneResult{Disk: c.Disk, Outcome: pruneOutcome(&in, c, &stay, level)})
		}
	}
	return results, nil
}

type stayingCopies struct {
	count, verified, user int
}

// staying counts the copies that remain once the surplus ones have gone: a copy that can be read
// now, and is what its row says.
func staying(copies []PruneCopy) stayingCopies {
	var stay stayingCopies
	for i := range copies {
		c := &copies[i]
		if c.State != CopyOK || !c.Unchanged || !readableNow(c.DiskState) {
			continue
		}
		stay.count++
		if c.Verified {
			stay.verified++
		}
		if c.CreatedBy == CreatedByUser {
			stay.user++
		}
	}
	return stay
}

func readableNow(d DiskState) bool {
	return d == DiskOnline || d == DiskStandby || d == DiskReadOnly
}

func pruneOutcome(in *PruneInput, c *PruneCopy, stay *stayingCopies, level int) PruneOutcome {
	switch {
	case in.State == StateConflict:
		return PruneConflict
	case in.State == StateCorrupt:
		return PruneCorrupt
	case c.DiskState != DiskOnline && c.DiskState != DiskStandby:
		return PruneDiskUnavailable
	case !c.Unchanged:
		return PruneChanged
	case !c.Verified || !in.HasHash:
		return PruneUnverified
	case stay.count < level:
		return PruneBelowLevel
	case stay.verified < level:
		return PruneUnverified
	case c.CreatedBy == CreatedByUser && stay.user == 0:
		return PruneLastUserCopy
	default:
		return PruneToTrash
	}
}

func checkPrune(in *PruneInput) error {
	if _, err := stateRank(in.State); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if in.Replicas < 0 || in.Replicas > 2 {
		return fmt.Errorf("prune: replicas %d", in.Replicas)
	}
	disks := make(map[string]bool, len(in.Copies))
	positions := make(map[int]bool, len(in.Copies))
	for i := range in.Copies {
		c := &in.Copies[i]
		if disks[c.Disk] || positions[c.Position] {
			return fmt.Errorf("prune: disk %q or position %d appears twice", c.Disk, c.Position)
		}
		disks[c.Disk], positions[c.Position] = true, true
		copyOf := Copy{State: c.State, CreatedBy: c.CreatedBy}
		if err := checkCopy(&copyOf); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
		switch c.DiskState {
		case DiskOnline, DiskStandby, DiskMissing, DiskReadOnly, DiskDraining, DiskFailed, DiskRetired:
		default:
			return fmt.Errorf("prune: unknown disk state %q", c.DiskState)
		}
	}
	return nil
}
