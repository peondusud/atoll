package solver

import (
	"cmp"
	"fmt"
	"slices"
)

// MaxMode is the highest mode a copy holds: the permission bits, setuid, setgid and sticky (specs.md §9).
const MaxMode = 0o7777

// MaxID is the highest user or group id a copy's owner holds (specs.md §9).
const MaxID = 1<<32 - 1

// MetaCopy is one copy of a file whose change is to its metadata only, as what that applies sees it (specs.md
// §15.4). Mtime, Ctime and LookMode are what the look found of a changed copy; Reachable says that the pass can write
// to its disk, and that its look reached it; PrecisionNS is its disk's time_precision_ns, 0 when unknown (specs.md
// §9). A mode is known only with its Has flag: a row recorded before modes were has none, and the owner likewise, its
// user and group ids together. A pending copy is none: its disk holds nothing of it yet.
type MetaCopy struct {
	Disk         string
	Position     int
	Changed      bool
	State        CopyState
	RowMtime     int64
	Mtime        int64
	Ctime        int64
	Reachable    bool
	PrecisionNS  int64
	RowMode      int64
	HasRowMode   bool
	LookMode     int64
	HasLookMode  bool
	RowOwner     Owner
	HasRowOwner  bool
	LookOwner    Owner
	HasLookOwner bool
}

// Owner is a copy's user and group ids.
type Owner struct {
	UID, GID int64
}

// MetaPlan is what a metadata-only change applies: Mtime, when HasMtime, the time of the copy From, to the copies in
// Apply through an operation utimens; Mode, when HasMode, the mode of the copy ModeFrom, to the copies in Chmod
// through an operation chmod; Owner, when HasOwner, the owner of the copy OwnerFrom, to the copies in Chown through an
// operation chown; and through a refresh, whatever they lack, to those in Stale. Each list is by position.
type MetaPlan struct {
	HasMtime  bool
	Mtime     int64
	From      string
	Apply     []string
	HasMode   bool
	Mode      int64
	ModeFrom  string
	Chmod     []string
	HasOwner  bool
	Owner     Owner
	OwnerFrom string
	Chown     []string
	Stale     []string
}

// Source says which copy a stale copy is refreshed from: the one that gives the time, or the mode when no time moved,
// or the owner when neither did.
func (p *MetaPlan) Source() string {
	switch {
	case p.HasMtime:
		return p.From
	case p.HasMode:
		return p.ModeFrom
	default:
		return p.OwnerFrom
	}
}

// MetadataPlan says what a metadata-only change applies to the other copies of a file (specs.md §15.4): the time of
// the changed copy whose time moved last, by change time, the mode of the changed copy whose mode moved last, and the
// owner of the changed copy whose owner moved last, each the first by position on a tie. An error means a state
// outside its closed list, a pending copy, a disk or position that appears twice, a mode outside 0 to 07777, or an id
// outside 0 to 2^32-1.
func MetadataPlan(copies []MetaCopy) (MetaPlan, error) {
	if err := checkMeta(copies); err != nil {
		return MetaPlan{}, err
	}
	sorted := make([]*MetaCopy, 0, len(copies))
	for i := range copies {
		sorted = append(sorted, &copies[i])
	}
	slices.SortFunc(sorted, func(a, b *MetaCopy) int { return cmp.Compare(a.Position, b.Position) })
	var lastTime, lastMode, lastOwner *MetaCopy
	for _, c := range sorted {
		if c.Changed && c.Mtime != c.RowMtime && (lastTime == nil || c.Ctime > lastTime.Ctime) {
			lastTime = c
		}
		// A row with no mode says nothing of what moved. Only a changed copy has a look's mode.
		if c.HasRowMode && c.HasLookMode && c.LookMode != c.RowMode && (lastMode == nil || c.Ctime > lastMode.Ctime) {
			lastMode = c
		}
		if c.HasRowOwner && c.HasLookOwner && c.LookOwner != c.RowOwner && (lastOwner == nil || c.Ctime > lastOwner.Ctime) {
			lastOwner = c
		}
	}
	var plan MetaPlan
	if lastTime != nil {
		plan.HasMtime, plan.Mtime, plan.From = true, lastTime.Mtime, lastTime.Disk
	}
	if lastMode != nil {
		plan.HasMode, plan.Mode, plan.ModeFrom = true, lastMode.LookMode, lastMode.Disk
	}
	if lastOwner != nil {
		plan.HasOwner, plan.Owner, plan.OwnerFrom = true, lastOwner.LookOwner, lastOwner.Disk
	}
	for _, c := range sorted {
		if c.State == CopyStale {
			continue
		}
		// A copy holds the time when it holds it as finely as its disk, or the disk it comes from, can: the copy that
		// gives it does.
		needsTime := lastTime != nil && !SameTime(c.mtimeNow(), plan.Mtime, Coarser(c.PrecisionNS, lastTime.PrecisionNS))
		// An unknown mode is another.
		mode, known := c.modeNow()
		needsMode := lastMode != nil && (!known || mode != plan.Mode)
		owner, knownOwner := c.ownerNow()
		needsOwner := lastOwner != nil && (!knownOwner || owner != plan.Owner)
		switch {
		case !needsTime && !needsMode && !needsOwner:
		case c.Reachable:
			if needsOwner {
				plan.Chown = append(plan.Chown, c.Disk)
			}
			if needsMode {
				plan.Chmod = append(plan.Chmod, c.Disk)
			}
			if needsTime {
				plan.Apply = append(plan.Apply, c.Disk)
			}
		default:
			plan.Stale = append(plan.Stale, c.Disk)
		}
	}
	return plan, nil
}

// mtimeNow is the copy's time: what its look found when it changed, what its row says otherwise.
func (c *MetaCopy) mtimeNow() int64 {
	if c.Changed {
		return c.Mtime
	}
	return c.RowMtime
}

// modeNow is the copy's mode, as mtimeNow, and whether it is known.
func (c *MetaCopy) modeNow() (int64, bool) {
	if c.Changed {
		return c.LookMode, c.HasLookMode
	}
	return c.RowMode, c.HasRowMode
}

// ownerNow is the copy's owner, as modeNow.
func (c *MetaCopy) ownerNow() (Owner, bool) {
	if c.Changed {
		return c.LookOwner, c.HasLookOwner
	}
	return c.RowOwner, c.HasRowOwner
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
		for _, m := range []struct {
			mode int64
			has  bool
		}{{c.RowMode, c.HasRowMode}, {c.LookMode, c.HasLookMode}} {
			if m.has && (m.mode < 0 || m.mode > MaxMode) {
				return fmt.Errorf("metadata: %q has the mode %d, outside 0 to 07777", c.Disk, m.mode)
			}
		}
		for _, o := range []struct {
			owner Owner
			has   bool
		}{{c.RowOwner, c.HasRowOwner}, {c.LookOwner, c.HasLookOwner}} {
			if o.has && (o.owner.UID < 0 || o.owner.UID > MaxID || o.owner.GID < 0 || o.owner.GID > MaxID) {
				return fmt.Errorf("metadata: %q has the owner %d:%d, outside 0 to 2^32-1", c.Disk, o.owner.UID, o.owner.GID)
			}
		}
	}
	return nil
}
