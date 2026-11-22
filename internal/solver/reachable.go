package solver

import (
	"cmp"
	"fmt"
	"slices"
)

// Level is what a pool can hold with k copies of every file (specs.md §13.1, §13.3).
type Level struct {
	// Copies is k: replicas + 1.
	Copies int
	// Theoretical is the formula of §13.1 over the disks the solver can write to, each its capacity less min_free.
	Theoretical int64
	// Stored is the bytes of the files that reach k copies in a plan that wants k copies of every file and moves
	// nothing, and Short counts the files that do not.
	Stored int64
	Short  int64
	// Reachable is Stored, and the formula over the free space that plan leaves, less min_free.
	Reachable int64
}

// MaxCopies is the most copies a file has: replicas + 1, replicas being 2 at most.
const MaxCopies = 3

// Levels works out what a pool can hold with 1, 2 and 3 copies of every file (specs.md §13.3): what the formula of
// §13.1 says, and what a plan reaches without moving a copy, as Atoll would place them. in is the input of the pool's
// plan, each file on the SSD cache leaving it, as each does in time (§19); strict is [placement] spread_mode strict,
// for the formula of failure domains. Each level plans every file at k copies, none waiting for atoll replicate, and
// in stays as it was.
func Levels(in Input, strict bool) ([]Level, error) {
	levels := make([]Level, 0, MaxCopies)
	for k := 1; k <= MaxCopies; k++ {
		level, err := levelOf(&in, strict, k)
		if err != nil {
			return nil, err
		}
		levels = append(levels, level)
	}
	return levels, nil
}

// levelOf plans with k copies of every file, none waiting for atoll replicate, and measures what it gives.
func levelOf(input *Input, strict bool, k int) (Level, error) {
	level := Level{Copies: k}
	wanted := map[*Placement]*Placement{}
	files := make([]File, len(input.Files))
	for i := range input.Files {
		f := input.Files[i]
		p := wanted[f.Placement]
		if p == nil {
			clone := *f.Placement
			clone.Replicas = k - 1
			p = &clone
			wanted[f.Placement] = p
		}
		f.Placement, f.Hold = p, false
		files[i] = f
	}
	in := *input
	in.Files = files
	planned := map[string]int64{}
	results, err := PlanEach(in, func(i int, jobs []Job) error {
		for _, j := range jobs {
			if j.Kind == JobReplicate {
				planned[j.Dst] += files[i].Size
			}
		}
		return nil
	})
	if err != nil {
		return Level{}, fmt.Errorf("plan: %w", err)
	}
	short := map[string]bool{}
	canceled := map[string][]string{}
	for _, r := range results {
		if slices.ContainsFunc(r.States, func(s State) bool {
			return s == StateDegraded || s == StateUnavailable || s == StateNameIncompatible || s == StateNameCollision
		}) {
			short[r.Path] = true
		}
		canceled[r.Path] = r.Cancel
	}
	for i := range files {
		if short[files[i].Path] {
			level.Short++
		} else {
			level.Stored += files[i].Size
		}
		// A pending copy the plan keeps is not on its disk yet: its size comes off that disk's free space, as that of
		// a copy the plan makes does (specs.md §13.4). Left out, a copy waiting in the queue counted for no room.
		for _, c := range files[i].Copies {
			if c.State == CopyPending && !slices.Contains(canceled[files[i].Path], c.Disk) {
				planned[c.Disk] += files[i].Size
			}
		}
	}
	whole, left := map[string]int64{}, map[string]int64{}
	var domains []string
	domainOf := map[string]string{}
	// By position, whatever the order of the list: the plan refused two disks at one position.
	byPosition := slices.Clone(input.Disks)
	slices.SortFunc(byPosition, func(a, b Disk) int { return cmp.Compare(a.Position, b.Position) })
	for _, d := range byPosition {
		if d.Role != RoleData || (d.State != DiskOnline && d.State != DiskStandby) {
			continue
		}
		usable, err := input.MinFree.Usable(d.Capacity)
		if err != nil {
			return Level{}, fmt.Errorf("disk %q: %w", d.ID, err)
		}
		reserved := d.Capacity - usable
		// Disks on one physical disk hold no two copies of a file: the formula sees one disk of all their room, in the
		// failure domain of the first of them by position (specs.md §13.1, R-06).
		unit := d.ID
		if d.Physical != "" {
			unit = d.Physical
		}
		whole[unit] += usable
		left[unit] += max(d.Free-planned[d.ID]-reserved, 0)
		if _, ok := domainOf[unit]; ok {
			continue
		}
		domain := d.Domain
		if domain == "" {
			domain = "\x00" + d.ID
		}
		domainOf[unit] = domain
		if !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}
	if level.Theoretical, err = formula(whole, domainOf, domains, strict, k); err != nil {
		return Level{}, err
	}
	more, err := formula(left, domainOf, domains, strict, k)
	if err != nil {
		return Level{}, err
	}
	level.Reachable = level.Stored + more
	return level, nil
}

// formula is U(k) over the sizes of the disks, or over their failure domains with strict (specs.md §13.1). A pool
// with no disk to write to holds nothing.
func formula(sizes map[string]int64, domainOf map[string]string, domains []string, strict bool, k int) (int64, error) {
	if len(sizes) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(sizes))
	for id := range sizes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if !strict {
		list := make([]int64, 0, len(ids))
		for _, id := range ids {
			list = append(list, sizes[id])
		}
		u, err := Capacity(list, k)
		if err != nil {
			return 0, fmt.Errorf("capacity: %w", err)
		}
		return u, nil
	}
	index := make(map[string]int, len(domains))
	for i, d := range domains {
		index[d] = i
	}
	byDomain := make([][]int64, len(domains))
	for _, id := range ids {
		at := index[domainOf[id]]
		byDomain[at] = append(byDomain[at], sizes[id])
	}
	u, err := StrictCapacity(byDomain, k)
	if err != nil {
		return 0, fmt.Errorf("capacity: %w", err)
	}
	return u, nil
}
