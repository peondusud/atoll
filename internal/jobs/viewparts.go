package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
)

// What a pass makes of the copies Atoll's own view wrote in parallel (specs.md §12.3, §21.2): a part the view handed
// over, done, goes to the replicate job of its file on its disk, which resumes from it and runs the rest of the copy
// pipeline; one no job takes is dropped, its replica left to the plan.

// partPaths gives the paths of the parts the view handed over, which wait for no settle: the view saw the program
// close the file.
func (p *pass) partPaths() map[string]bool {
	out := map[string]bool{}
	for i := range p.viewOps {
		if v := &p.viewOps[i]; v.Kind == catalog.ViewPart && v.State == catalog.OpDone {
			out[v.Path] = true
		}
	}
	return out
}

// adoptParts gives each part the view handed over to the replicate job of its file on its disk, before any job runs.
func (p *pass) adoptParts(ctx context.Context) error {
	if !p.apply {
		return nil
	}
	n, err := catalog.AdoptViewParts(ctx, p.db, p.now)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if n > 0 {
		slog.Info("copies written in parallel go on as replicas", "parts", n)
	}
	return nil
}

// settleParts drops each part the view handed over that no job took: the plan placed no copy of its file on that disk,
// that disk holds one already, or its file left that path, or replication is off. One whose file the catalog does not
// hold yet, while the view's operations at its path are open, waits; so does one on a disk this pass cannot write to.
// What the view still writes is its own.
func (p *pass) settleParts(ctx context.Context) error {
	if !p.apply {
		return nil
	}
	list, err := catalog.OpenViewParts(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	enabled, _ := p.config.Effective()["replication.enabled"].(bool)
	for i := range list {
		r := &list[i]
		if r.State != catalog.OpDone {
			continue
		}
		if !p.looked(r.Disk) || !p.session.Writable(r.Disk) {
			continue
		}
		drop := !enabled
		if !drop {
			f, err := catalog.FileAt(ctx, p.db, r.Path)
			if err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			if f == nil {
				drop = !p.viewOpenAt(r.Path)
			} else {
				drop = true
				for j := range f.Copies {
					if c := &f.Copies[j]; c.Disk == r.Disk && c.State == string(solver.CopyPending) && p.jobWithoutPart(ctx, f.ID, r.Disk) {
						drop = false
					}
				}
			}
		}
		if !drop {
			continue
		}
		if err := p.dropViewPart(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// viewOpenAt says whether an operation of the view other than a part, not taken in, touches a path.
func (p *pass) viewOpenAt(path string) bool {
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if v.Kind != catalog.ViewPart && (v.Path == path || v.Path2 == path) {
			return true
		}
	}
	return false
}

// jobWithoutPart says whether a file has a replicate job queued to a disk with no part yet, which the next pass
// gives the view's part: one queued after this pass adopted the parts.
func (p *pass) jobWithoutPart(ctx context.Context, fileID int64, disk string) bool {
	part, found, err := catalog.ReplicateJobPart(ctx, p.db, fileID, disk)
	return err == nil && found && part == ""
}

// dropViewPart removes a part no job took, then undoes its operation and takes the view's row in.
func (p *pass) dropViewPart(ctx context.Context, r *catalog.ViewPartRow) error {
	handle, err := p.session.Handle(ctx, r.Disk)
	if err != nil || handle == nil {
		return err //nolint:wrapcheck // the session says what failed
	}
	switch err := handle.DropPart(r.Part); {
	case errors.Is(err, ops.ErrDiskLost):
		return p.lose(ctx, r.Disk, err)
	case err != nil:
		slog.Warn("a copy written in parallel stays in tmp/", "disk", r.Disk, "part", r.Part, "error", err)
		return nil
	}
	if err := catalog.DismissViewPart(ctx, p.db, &r.ViewOp, p.now); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	slog.Info("a copy written in parallel no job took is dropped", "disk", r.Disk, "path", r.Path)
	return nil
}
