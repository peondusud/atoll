package jobs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

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
		if key := sentToTrash(&open[i]); key != "" {
			p.throughView[open[i].Disk+"\x00"+key] = true
		}
	}
	if p.viewSince, err = catalog.NextViewOp(ctx, p.db); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i := range open {
		if open[i].State == catalog.OpPlanned {
			p.viewSince = min(p.viewSince, open[i].ID)
		}
	}
	return nil
}

// sentToTrash gives the inode of the copy an operation of the view sent to the trash: a deletion's, and the one a
// replacement found at its new name, which its operation trash names (specs.md §21.2; conformance/models/README.md,
// flaw 18, the maintainer's remedy, 2026-11-24). Either is the user's deletion of that file, through the view.
func sentToTrash(v *catalog.ViewOp) string {
	switch {
	case v.OpID == 0:
		return ""
	case v.Kind == catalog.ViewUnlink:
		return v.FileKey
	case v.Kind == catalog.ViewReplace && v.OpKind == catalog.OpTrash:
		return v.OpFileKey
	}
	return ""
}

// viewPlannedOn gives the paths of the view's operations still under way on a disk: a move's on the disk it goes to and
// on the disk it leaves (specs.md §21.2).
func (p *pass) viewPlannedOn(disk string) []string {
	var paths []string
	for i := range p.viewOps {
		v := &p.viewOps[i]
		on := v.Disk == disk || v.Kind == catalog.ViewMove && v.Disk2 == disk
		if v.State != catalog.OpPlanned || !on || v.Kind == catalog.ViewPart {
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

// deletedThroughView says whether the view deleted the file at a path, or replaced it, which no pass has taken in yet:
// its deletion stands whatever deletes.direct says (specs.md §21.2).
func (p *pass) deletedThroughView(path string) bool {
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if sentToTrash(v) != "" && v.Path2OrPath() == path {
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
	moot, err := p.mootViewOps(ctx, open)
	if err != nil {
		return err
	}
	taken, err := view.TakenIn(ctx, p.db, open, walkedAll, settings.InView, p.lookForView, moot)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := catalog.TakeViewOps(ctx, p.db, taken, p.now); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// mootViewOps gives the operations of the view done that a user undid directly on a disk this pass looked at, which it
// takes in as they are, the catalog then presenting what the disks hold (specs.md §21.2; conformance/models/README.md,
// flaws 20 and 21, the maintainer's remedy, 2026-11-24): a rename or a replacement of a file whose new name its disk
// no longer holds the inode of; and a folder's rename that every disk whose entry holds the folder at either name no
// longer holds at the new name, the disk the journal names by the inode it names, another by any folder there, or whose
// old name a disk holds a folder at again that no job of that disk will move, on the disk the journal names another
// than the one of its inode, or one the catalog holds nothing of that disk below.
func (p *pass) mootViewOps(ctx context.Context, open []catalog.ViewOp) (map[int64]bool, error) {
	moot := map[int64]bool{}
	for i := range open {
		v := &open[i]
		if v.State != catalog.OpDone {
			continue
		}
		var undone bool
		var err error
		switch {
		case v.Kind == catalog.ViewReplace, v.Kind == catalog.ViewRename && !v.Folder:
			undone, err = p.fileUndone(ctx, v)
		case v.Kind == catalog.ViewRename:
			undone, err = p.folderUndone(ctx, v)
		}
		if err != nil {
			return nil, err
		}
		if undone {
			moot[v.ID] = true
		}
	}
	return moot, nil
}

// fileUndone says that the new name of a file's rename or replacement, on its disk, no longer holds the inode it moved.
func (p *pass) fileUndone(ctx context.Context, v *catalog.ViewOp) (bool, error) {
	if v.FileKey == "" {
		return false, nil
	}
	st, found, looked, err := p.statLooked(ctx, v.Disk, v.Path2)
	if err != nil || !looked {
		return false, err
	}
	return !found || strconv.FormatUint(st.Ino, 10) != v.FileKey, nil
}

// folderUndone says that a folder's rename is undone directly on the disks: see mootViewOps.
func (p *pass) folderUndone(ctx context.Context, v *catalog.ViewOp) (bool, error) {
	holders := map[string]bool{v.Disk: true}
	for _, path := range []string{v.Path, v.Path2} {
		disks, err := catalog.FolderDisks(ctx, p.db, path)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		for _, disk := range disks {
			holders[disk] = true
		}
	}
	gone := true
	for _, disk := range slices.Sorted(maps.Keys(holders)) {
		st, found, looked, err := p.statLooked(ctx, disk, v.Path2)
		if err != nil {
			return false, err
		}
		held := found && st.Kind == fsx.KindDir && (disk != v.Disk || v.FileKey == "" || strconv.FormatUint(st.Ino, 10) == v.FileKey)
		if !looked || held {
			gone = false
			break
		}
	}
	if gone {
		return true, nil
	}
	for i := range p.config.Disks {
		disk := p.config.Disks[i].ID
		st, found, looked, err := p.statLooked(ctx, disk, v.Path)
		if err != nil {
			return false, err
		}
		if !looked || !found || st.Kind != fsx.KindDir {
			continue
		}
		jobs, err := catalog.FolderJobs(ctx, p.db, disk)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if slices.ContainsFunc(jobs, func(j catalog.FolderJob) bool { return j.From == v.Path }) {
			continue
		}
		if disk == v.Disk && v.FileKey != "" && strconv.FormatUint(st.Ino, 10) != v.FileKey {
			return true, nil
		}
		left, err := catalog.MadeBelow(ctx, p.db, disk, v.Path)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if left == 0 {
			return true, nil
		}
	}
	return false, nil
}

// statLooked looks at a path on a disk this pass walked or spared; looked is false on another.
func (p *pass) statLooked(ctx context.Context, disk, path string) (st fsx.Stat, found, looked bool, err error) {
	if !p.looked(disk) {
		return st, false, false, nil
	}
	handle, err := p.session.Handle(ctx, disk)
	if err != nil || handle == nil {
		return st, false, false, err //nolint:wrapcheck // the session says what failed
	}
	if st, found, err = handle.StatRoot(path); err != nil {
		return st, false, false, fmt.Errorf("%w", err)
	}
	return st, found, true, nil
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
