package watch

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
)

// Level is what the pool can hold with k copies of every file (specs.md §13.1, §13.3).
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

// Capacity works out what the pool can hold with 1, 2 and 3 copies of every file, from the catalog alone, waking no
// disk: what the formula says, and what a plan reaches without moving a copy, as Atoll would place them (specs.md
// §13.3). With [placement] spread_mode strict, the formula is that of failure domains.
func Capacity(ctx context.Context, db *sql.DB, c *config.Config, now int64) ([]Level, error) {
	diskList, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	entries, err := catalog.Entries(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	minFree, err := c.MinFree()
	if err != nil {
		return nil, err
	}
	grace, err := missingGraceOf(c)
	if err != nil {
		return nil, err
	}
	input := solver.Input{MinFree: minFree, Disks: planDisks(diskList, now, grace), Entries: planEntries(entries)}
	if _, err := filesForPlan(ctx, db, c, diskList, &input, nil); err != nil {
		return nil, err
	}
	strict := c.Pool.SpreadMode == solver.ModeStrict
	levels := make([]Level, 0, MaxCopies)
	for k := 1; k <= MaxCopies; k++ {
		level, err := levelOf(&input, minFree, strict, k)
		if err != nil {
			return nil, err
		}
		levels = append(levels, level)
	}
	return levels, nil
}

// levelOf plans with k copies of every file, none waiting for atoll replicate, and measures what it gives.
func levelOf(input *solver.Input, minFree value.MinFree, strict bool, k int) (Level, error) {
	level := Level{Copies: k}
	wanted := map[*solver.Placement]*solver.Placement{}
	files := make([]solver.File, len(input.Files))
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
	results, err := solver.PlanEach(in, func(i int, jobs []solver.Job) error {
		for _, j := range jobs {
			if j.Kind == solver.JobReplicate {
				planned[j.Dst] += files[i].Size
			}
		}
		return nil
	})
	if err != nil {
		return Level{}, fmt.Errorf("plan: %w", err)
	}
	short := map[string]bool{}
	for _, r := range results {
		if slices.ContainsFunc(r.States, func(s solver.State) bool {
			return s == solver.StateDegraded || s == solver.StateUnavailable || s == solver.StateNameIncompatible || s == solver.StateNameCollision
		}) {
			short[r.Path] = true
		}
	}
	for i := range files {
		if short[files[i].Path] {
			level.Short++
		} else {
			level.Stored += files[i].Size
		}
	}
	whole, left := map[string]int64{}, map[string]int64{}
	var domains []string
	domainOf := map[string]string{}
	for _, d := range input.Disks {
		if d.Role != "data" || (d.State != solver.DiskOnline && d.State != solver.DiskStandby) {
			continue
		}
		usable, err := minFree.Usable(d.Capacity)
		if err != nil {
			return Level{}, fmt.Errorf("disk %q: %w", d.ID, err)
		}
		reserved := d.Capacity - usable
		whole[d.ID] = usable
		left[d.ID] = max(d.Free-planned[d.ID]-reserved, 0)
		domain := d.Domain
		if domain == "" {
			domain = "\x00" + d.ID
		}
		domainOf[d.ID] = domain
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
		u, err := solver.Capacity(list, k)
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
	u, err := solver.StrictCapacity(byDomain, k)
	if err != nil {
		return 0, fmt.Errorf("capacity: %w", err)
	}
	return u, nil
}
