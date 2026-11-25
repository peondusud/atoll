package jobs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/watch"
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

// viewFolderMoves gives a disk, as a folder renamed there, a folder's rename through the view that no walk finds
// (specs.md §15.12, §21.2; conformance/models/README.md, observation 5, the maintainer's choice, 2026-11-24): done, no
// pass has taken it in, the last operation of the view at its names, on the disk its journal names, which this pass
// walked and found holding the folder the journal names by its inode at the new name, where that disk's entry already
// holds it, as no walk of that disk found it at the old name in between. A rename back made on that disk alone does
// that: the catalog still holds the files below the old name, from the first rename, and the other disks hold them
// there. The rename then goes on to the disks it missed, as any folder renamed: the files follow, the copies of that
// disk already there, and each other disk gets a job. Only while the catalog holds a file below the old name, or
// another disk a copy or an entry below it with no such job; never while that disk's entries hold a folder at the old
// name too, or a folder this pass found renamed on that disk, or at neither name, is at, below or above either name.
// It reads the rows as the walks left them, before any decision of this pass: one that a decision of this pass makes
// true waits for the next pass, and meanwhile no copy goes against it (viewKeeps). It also gives those operations whose
// disk holds the folder they name at the new name, which viewKeeps reads.
func (p *pass) viewFolderMoves(ctx context.Context) error {
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if v.Kind != catalog.ViewRename || !v.Folder || v.State != catalog.OpDone || v.FileKey == "" || !p.walked[v.Disk] ||
			p.touchedLater(v) {
			continue
		}
		st, there, looked, err := p.statLooked(ctx, v.Disk, v.Path2)
		if err != nil {
			return err
		}
		if !looked || !there || st.Kind != fsx.KindDir || strconv.FormatUint(st.Ino, 10) != v.FileKey {
			continue
		}
		p.kept = append(p.kept, watch.FolderMove{From: v.Path, To: v.Path2})
		p.keptOps = append(p.keptOps, v)
		touches := func(path string) bool { return touchesEither(v, path) }
		if slices.ContainsFunc(p.moves[v.Disk], func(m watch.FolderMove) bool { return touches(m.From) || touches(m.To) }) ||
			slices.ContainsFunc(p.unfound[v.Disk], touches) {
			continue
		}
		known, err := catalog.EntryKeyAt(ctx, p.db, v.Disk, v.Path2)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		again, err := catalog.EntryKeyAt(ctx, p.db, v.Disk, v.Path)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if known != v.FileKey || again != "" {
			continue
		}
		missed, err := p.missedBy(ctx, v)
		if err != nil {
			return err
		}
		if missed {
			p.viewMoves[folderKey{v.Disk, v.Path}] = v.Path2
			p.moves[v.Disk] = append(p.moves[v.Disk], watch.FolderMove{From: v.Path, To: v.Path2})
		}
	}
	return nil
}

// touchesEither says whether a path is at, below or above either name of an operation of the view.
func touchesEither(v *catalog.ViewOp, path string) bool {
	return slices.ContainsFunc([]string{v.Path, v.Path2}, func(name string) bool {
		return name != "" && (isAtOrBelow(path, name) || isAtOrBelow(name, path))
	})
}

// touchedLater says whether an operation of the view no pass has taken in, later than one, is at, below or above either
// of its names: that one is not the last there.
func (p *pass) touchedLater(v *catalog.ViewOp) bool {
	return slices.ContainsFunc(p.viewOps, func(w catalog.ViewOp) bool {
		return w.ID > v.ID && (touchesEither(v, w.Path) || w.Path2 != "" && touchesEither(v, w.Path2))
	})
}

// missedBy says whether a folder's rename through the view has something left to take: a file the catalog holds below
// the old name, or a copy or an entry another disk holds below it with no job of that rename.
func (p *pass) missedBy(ctx context.Context, v *catalog.ViewOp) (bool, error) {
	page, err := catalog.FilesBelow(ctx, catalog.NewFileReader(p.db), v.Path)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if len(page) > 0 {
		return true, nil
	}
	others, err := catalog.DisksBelow(ctx, p.db, v.Path, v.Disk)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	for _, other := range others {
		jobs, err := catalog.FolderJobs(ctx, p.db, other)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if !slices.ContainsFunc(jobs, func(j catalog.FolderJob) bool { return j.From == v.Path && j.To == v.Path2 }) {
			return true, nil
		}
	}
	return false, nil
}

// viewKeeps says that a copy's rename would take it from at or below the new name of a folder's rename through the
// view to at or below its old name, while that rename, done and not taken in, is the last operation of the view at its
// names and its disk holds there the folder it names: the rename stands on that disk, and the catalog comes to it,
// the copy waiting where it is meanwhile (specs.md §15.12; conformance/models/README.md, observation 5, the
// maintainer's choice, 2026-11-24).
func (p *pass) viewKeeps(place, target string) bool {
	return slices.ContainsFunc(p.kept, func(m watch.FolderMove) bool {
		return isAtOrBelow(place, m.To) && isAtOrBelow(target, m.From)
	})
}

// missedViewOps gives the folders' renames through the view no pass may take in yet, though the catalog may present
// what the view does (specs.md §21.2; conformance/models/README.md, observation 5, the maintainer's choice,
// 2026-11-24): one a disk that holds the folder, by the catalog's entries at either name, was not looked at by this
// pass, as it may have missed it; and the last operation of the view at its names, done on the disk its journal names,
// which another disk missed: that disk holds at the old name the folder its entry records at either name, and nothing
// at the new; and one whose old name the catalog still holds a file below, no user having undone it, as the take-in
// compares the folder's names alone (conformance/models/README.md, flaw 23, the maintainer's remedy, 2026-11-24): a
// disk that missed it is still to follow, and the rename then goes on to the catalog (viewFolderMoves). A disk
// failed is never read again, and holds nothing back.
func (p *pass) missedViewOps(ctx context.Context, open []catalog.ViewOp) (map[int64]bool, error) {
	held := map[int64]bool{}
	failed := map[string]bool{}
	for i := range p.config.Disks {
		if p.config.Disks[i].State == config.DiskFailed {
			failed[p.config.Disks[i].ID] = true
		}
	}
	for i := range open {
		v := &open[i]
		if v.Kind != catalog.ViewRename || !v.Folder || v.State != catalog.OpDone {
			continue
		}
		holders := []string{v.Disk}
		for _, path := range []string{v.Path, v.Path2} {
			disks, err := catalog.FolderDisks(ctx, p.db, path)
			if err != nil {
				return nil, fmt.Errorf("catalog: %w", err)
			}
			holders = append(holders, disks...)
		}
		if slices.ContainsFunc(holders, func(d string) bool { return d != "" && !failed[d] && p.configured(d) && !p.looked(d) }) {
			held[v.ID] = true
			continue
		}
		missed, err := p.missedOn(ctx, v, holders)
		if err != nil {
			return nil, err
		}
		if missed {
			held[v.ID] = true
			continue
		}
		// Nor while the catalog holds a file below the old name, which the view presents at the new one, but as a user
		// undid it (flaw 23).
		below, err := p.filesBelow(ctx, v)
		if err != nil {
			return nil, err
		}
		if below {
			held[v.ID] = true
		}
	}
	return held, nil
}

// backRenames renames, on a disk, each folder's rename of the view that stands, which that disk missed (backRenamesOn;
// specs.md §15.12, §21.2; conformance/models/README.md, flaw 23, the maintainer's remedy, 2026-11-24). The pass that
// finds it makes it, as a folder's job it runs at once (runFolderJob), after the jobs of that disk, never queued: a job
// could outlive the rename it carries out, which a later rename back undoes. Meanwhile, a folder found renamed and a
// copy found moved against that rename wait (viewKeeps).
func (p *pass) backRenames(ctx context.Context, d *config.Disk, handle *ops.Disk) error {
	jobs, err := p.backRenamesOn(ctx, d)
	if err != nil {
		return err
	}
	for i := range jobs {
		if !p.session.Writable(d.ID) {
			return nil
		}
		if err := p.runFolderJob(ctx, d, handle, &jobs[i]); err != nil {
			return err
		}
	}
	return nil
}

// backRenamesOn gives the folders' renames of the view that stand which a disk this pass walked missed: it holds at the
// old name the folder its entry records at either name, and nothing at the new.
func (p *pass) backRenamesOn(ctx context.Context, d *config.Disk) ([]catalog.FolderJob, error) {
	var jobs []catalog.FolderJob
	for _, v := range p.keptOps {
		if v.Disk == d.ID {
			continue
		}
		old, found, looked, err := p.statLooked(ctx, d.ID, v.Path)
		if err != nil {
			return nil, err
		}
		if !looked || !found || old.Kind != fsx.KindDir {
			continue
		}
		_, taken, _, err := p.statLooked(ctx, d.ID, v.Path2)
		if err != nil {
			return nil, err
		}
		if taken {
			continue
		}
		key := strconv.FormatUint(old.Ino, 10)
		records := false
		for _, path := range []string{v.Path, v.Path2} {
			known, err := catalog.EntryKeyAt(ctx, p.db, d.ID, path)
			if err != nil {
				return nil, fmt.Errorf("catalog: %w", err)
			}
			records = records || known == key
		}
		if records {
			jobs = append(jobs, catalog.FolderJob{From: v.Path, To: v.Path2})
		}
	}
	return jobs, nil
}

// filesBelow says that the catalog holds a file below the old name of a folder's rename of the view no user undid.
func (p *pass) filesBelow(ctx context.Context, v *catalog.ViewOp) (bool, error) {
	page, err := catalog.FilesBelow(ctx, catalog.NewFileReader(p.db), v.Path)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if len(page) == 0 {
		return false, nil
	}
	undone, err := p.folderUndone(ctx, v)
	if err != nil {
		return false, err
	}
	return !undone, nil
}

// configured says whether a disk is one of the pool's.
func (p *pass) configured(disk string) bool {
	return slices.ContainsFunc(p.config.Disks, func(d config.Disk) bool { return d.ID == disk })
}

// missedOn says that a folder's rename through the view, the last operation of the view at its names and done on the
// disk its journal names, was missed by another disk that holds it: see missedViewOps.
func (p *pass) missedOn(ctx context.Context, v *catalog.ViewOp, holders []string) (bool, error) {
	if v.FileKey == "" || p.touchedLater(v) {
		return false, nil
	}
	st, there, looked, err := p.statLooked(ctx, v.Disk, v.Path2)
	if err != nil || !looked || !there || st.Kind != fsx.KindDir || strconv.FormatUint(st.Ino, 10) != v.FileKey {
		return false, err
	}
	for _, disk := range slices.Compact(slices.Sorted(slices.Values(holders))) {
		if disk == v.Disk {
			continue
		}
		old, found, looked, err := p.statLooked(ctx, disk, v.Path)
		if err != nil {
			return false, err
		}
		if !looked || !found || old.Kind != fsx.KindDir {
			continue
		}
		_, taken, _, err := p.statLooked(ctx, disk, v.Path2)
		if err != nil {
			return false, err
		}
		if taken {
			continue
		}
		key := strconv.FormatUint(old.Ino, 10)
		for _, path := range []string{v.Path, v.Path2} {
			known, err := catalog.EntryKeyAt(ctx, p.db, disk, path)
			if err != nil {
				return false, fmt.Errorf("catalog: %w", err)
			}
			if known == key {
				return true, nil
			}
		}
	}
	return false, nil
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
	// A folder's rename a disk may have missed waits, as one under way does, whatever the catalog presents.
	missed, err := p.missedViewOps(ctx, open)
	if err != nil {
		return err
	}
	for i := range open {
		if waiting[open[i].ID] || missed[open[i].ID] {
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
