package jobs

import (
	"context"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/view"
)

// What a pass makes of the operations of Atoll's own view (specs.md §21.2): it waits at the paths of those under way,
// as at an operation of its own left planned; a copy the view sent to the trash is gone by the user, which propagates
// whatever deletes.direct says; an empty folder the view removed goes from the disks where it was not empty yet; an
// extended attribute it changed on a copy goes to the file's other copies; and each operation the catalog now presents
// as the view does is taken in.

// readViewOps reads the view's operations no pass has taken in.
func (p *pass) readViewOps(ctx context.Context) error {
	open, err := catalog.OpenViewOps(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	p.viewOps = open
	p.throughView = map[string]bool{}
	for i := range open {
		if v := &open[i]; v.Kind == catalog.ViewUnlink && v.OpID != 0 && v.FileKey != "" {
			p.throughView[v.Disk+"\x00"+v.FileKey] = true
		}
	}
	return nil
}

// viewPlannedOn gives the paths of the view's operations still under way on a disk.
func (p *pass) viewPlannedOn(disk string) []string {
	var paths []string
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if v.State != catalog.OpPlanned || v.Disk != disk || v.Kind == catalog.ViewPart {
			continue
		}
		paths = append(paths, v.Path)
		if v.Path2 != "" {
			paths = append(paths, v.Path2)
		}
	}
	return paths
}

// wentThroughView says whether a copy went through the view to the trash.
func (p *pass) wentThroughView(disk string, fileKey string) bool {
	return fileKey != "" && p.throughView[disk+"\x00"+fileKey]
}

// deletedThroughView says whether the view deleted the file at a path, which no pass has taken in yet: its deletion
// stands whatever deletes.direct says (specs.md §21.2).
func (p *pass) deletedThroughView(path string) bool {
	for i := range p.viewOps {
		if v := &p.viewOps[i]; v.Kind == catalog.ViewUnlink && v.OpID != 0 && v.Path == path {
			return true
		}
	}
	return false
}

// takeInViewOps removes from the disks where it was not empty an empty folder the view removed, then takes in each
// operation of the view the catalog now presents as the view does.
func (p *pass) takeInViewOps(ctx context.Context) error {
	if len(p.viewOps) == 0 {
		return nil
	}
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if v.Kind != catalog.ViewRmdir || v.State != catalog.OpDone {
			continue
		}
		left, err := catalog.FolderDisks(ctx, p.db, v.Path)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		for _, disk := range left {
			if p.looked(disk) {
				if err := p.session.RemoveUserFolder(ctx, disk, v.Path); err != nil {
					return fmt.Errorf("%w", err)
				}
			}
		}
	}
	walkedAll := true
	for i := range p.config.Disks {
		d := &p.config.Disks[i]
		if !p.looked(d.ID) && p.session.Readable(d.ID) {
			walkedAll = false
		}
	}
	settings, err := p.config.Cache()
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	// An extended attribute is taken in once the other copies of its file hold what the copy changed holds; one that
	// waits holds back the later operations at its path, as one under way does.
	waiting, err := p.giveXattrs(ctx)
	if err != nil {
		return err
	}
	// A part written in parallel is taken in by the job that adopts it, or as the pass drops it, never as what the
	// catalog presents: it is no name of the root.
	if err := p.settleParts(ctx); err != nil {
		return err
	}
	open := slices.DeleteFunc(slices.Clone(p.viewOps), func(v catalog.ViewOp) bool { return v.Kind == catalog.ViewPart })
	for i := range open {
		if waiting[open[i].ID] {
			open[i].State = catalog.OpPlanned
		}
	}
	taken, err := view.TakenIn(ctx, p.db, open, walkedAll, settings.InView, p.lookForView)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := catalog.TakeViewOps(ctx, p.db, taken, p.now); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// lookForView looks at a path on a disk this pass walked or spared, for what the view presents of what it made there.
func (p *pass) lookForView(ctx context.Context, disk, path string) (*view.Node, bool, error) {
	if !p.looked(disk) {
		return nil, false, nil
	}
	handle, err := p.session.Handle(ctx, disk)
	if err != nil || handle == nil {
		return nil, false, err //nolint:wrapcheck // the session says what failed
	}
	st, found, err := handle.StatRoot(path)
	if err != nil {
		return nil, false, fmt.Errorf("%w", err)
	}
	if !found {
		return nil, true, nil
	}
	target := ""
	if st.Kind == fsx.KindSymlink {
		target, err = handle.ReadLinkRoot(path)
		if err != nil {
			return nil, false, fmt.Errorf("%w", err)
		}
	}
	return view.NodeOfStat(disk, &st, target), true, nil
}
