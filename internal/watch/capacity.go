package watch

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/solver"
)

// Level is what the pool can hold with k copies of every file (specs.md §13.1, §13.3), as the solver works it out.
type Level = solver.Level

// MaxCopies is the most copies a file has: replicas + 1, replicas being 2 at most.
const MaxCopies = solver.MaxCopies

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
	taken, done := catalog.KeysTaken(ctx, db)
	defer done()
	newFiles, fillTo, err := NewFilesOf(c)
	if err != nil {
		return nil, err
	}
	input := solver.Input{MinFree: minFree, Disks: planDisks(diskList, now, grace), Entries: planEntries(entries), Taken: taken, NewFiles: newFiles, FillTo: fillTo, Equalize: EqualizeOf(c)}
	leave, err := allLeave(ctx, db, c)
	if err != nil {
		return nil, err
	}
	if _, err := filesForPlan(ctx, db, c, diskList, &input, nil, leave); err != nil {
		return nil, err
	}
	levels, err := solver.Levels(input, c.Pool.SpreadMode == solver.ModeStrict)
	if err != nil {
		// Said as before the solver worked it out: "plan: …", "disk …" or "capacity: …".
		return nil, fmt.Errorf("%w", err)
	}
	return levels, nil
}
