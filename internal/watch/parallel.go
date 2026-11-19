package watch

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/solver"
)

// groupLimit bounds the files of a new file's group a plan of it weighs for colocate: past it, the plan of the file
// alone is made, as the view makes it while a program waits (specs.md §12.4).
const groupLimit = 2000

// ParallelTargets gives the disks the solver places the replicas of a new file on, as Atoll's own view makes it at a
// path on original, its size not known yet (specs.md §11.3, §12.3, §21.2): the plan of that file, with the files of its
// group for colocate, its policy read without a size, its copy on original the one that counts, as any replica's
// targets are chosen (invariant 18). free gives the free space a disk's filesystem says it has now, which the plan reads
// in place of the catalog's; a disk writable says the view may not write to is no candidate. None for a file a rule
// ignores, or with no replica wanted. The targets come in the plan's order.
func ParallelTargets(ctx context.Context, db *sql.DB, c *config.Config, now int64, path, original string, free func(string) (int64, bool), writable func(string) bool) ([]string, error) {
	policy := c.Policy(path, nil)
	if policy.Action == rules.Ignore || policy.Replicas == 0 {
		return nil, nil
	}
	diskList, err := catalog.Disks(ctx, db)
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
	newFiles, fillTo, err := NewFilesOf(c)
	if err != nil {
		return nil, err
	}
	disks := planDisks(diskList, now, grace)
	for i := range disks {
		d := &disks[i]
		if d.ID == original {
			continue
		}
		if !writable(d.ID) && (d.State == solver.DiskOnline || d.State == solver.DiskStandby) {
			d.State = solver.DiskReadOnly
		}
		if f, ok := free(d.ID); ok {
			d.Free = f
		}
	}
	taken, done := catalog.KeysTaken(ctx, db)
	defer done()
	in := solver.Input{MinFree: minFree, Disks: disks, Taken: taken, NewFiles: newFiles, FillTo: fillTo, Equalize: EqualizeOf(c)}
	placement := &solver.Placement{
		Replicas: policy.Replicas, Priority: policy.Priority, Tags: policy.Tags, TagsMode: policy.TagsMode, Domains: policy.Domains,
		Spread: policy.Spread, SpreadMode: policy.SpreadMode, Overflow: policy.Overflow, Colocate: policy.Colocate,
	}
	in.Files = append(in.Files, solver.File{Path: path, Placement: placement, Copies: []solver.Copy{
		{Disk: original, State: solver.CopyOK, CreatedBy: solver.CreatedByUser, FirstSeenNS: now},
	}})
	if folder, ok := solver.GroupFolder(path, policy.Colocate); ok {
		group, err := groupFiles(ctx, db, c, diskList, folder)
		if err != nil {
			return nil, err
		}
		in.Files = append(in.Files, group...)
	}
	var targets []string
	_, err = solver.PlanEach(in, func(i int, jobs []solver.Job) error {
		if i != 0 {
			return nil
		}
		for _, j := range jobs {
			if j.Kind == solver.JobReplicate {
				targets = append(targets, j.Dst)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return targets, nil
}

// groupFiles gives the plan the files below a folder, with their copies, which colocate weighs, none past groupLimit.
func groupFiles(ctx context.Context, db *sql.DB, c *config.Config, diskList []catalog.Disk, folder string) ([]solver.File, error) {
	k, err := newPlanKit(ctx, db, c, diskList, nil)
	if err != nil {
		return nil, err
	}
	reader := catalog.NewFileReader(db)
	var out []solver.File
	for {
		page, err := catalog.FilesBelow(ctx, reader, folder)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return out, nil
		}
		for i := range page {
			file, _, err := k.planFile(&page[i], false, solver.CacheNone)
			if err != nil {
				return nil, err
			}
			out = append(out, file)
		}
		if len(out) > groupLimit {
			return nil, nil
		}
	}
}
