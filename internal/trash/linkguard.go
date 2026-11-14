package trash

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
)

// linkBatch is how many operations on links one transaction journals, and one sync of the folder makes durable: the
// first pass with the guard on makes one for each copy of the disk.
const linkBatch = 1000

// LinkItem is a link of the guard sent to the trash: its data lost every name Atoll knows in the root.
type LinkItem struct {
	Disk    string
	Path    string
	Reason  string
	Outcome string
}

// Guard keeps the link guard of a disk in step with its copies, once a pass has looked at the disk (specs.md §17).
// With the guard on, each copy on the disk gets a link at <workdir>/links/<file id>; a link whose data lost every
// name Atoll knows in the root goes to the trash, at its file's path; a link whose data has another name Atoll knows,
// a copy or an entry of the trash, goes. With the guard off, every link goes the same way, a link to its file's
// copy included, and the folder with them. Nothing is done without apply, or on a disk that cannot be written to.
// report is told of each link sent to the trash.
func (s *Session) Guard(ctx context.Context, id string, on bool, report func(*LinkItem) error) error {
	if !s.apply || !s.writable(id) {
		return nil
	}
	if !on {
		// Off on a disk the guard never wrote to, there is nothing to remove, and no reason to open the disk.
		used, err := catalog.GuardUsed(ctx, s.db, id)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if !used {
			return nil
		}
	}
	handle, err := s.open(ctx, id)
	if err != nil || handle == nil || !s.writable(id) {
		return err
	}
	entries, err := handle.Links()
	if err != nil {
		return s.guardFailed(ctx, id, err)
	}
	if !on && len(entries) == 0 {
		handle.RemoveLinksFolder()
		return s.countUnnamed(ctx, id, 0)
	}
	in, err := s.linksInput(ctx, id, on, entries)
	if err != nil {
		return err
	}
	plan := solver.PlanLinks(in)
	var unlinks, makes []catalog.LinkStart
	unnamed := 0
	for i := range plan {
		step := &plan[i]
		switch step.Do {
		case solver.LinkKeep:
		case solver.LinkUnlink:
			unlinks = append(unlinks, catalog.LinkStart{FileID: step.FileID, Path: step.Path, FileKey: step.Inode})
		case solver.LinkTrash:
			drop, err := s.sendLink(ctx, handle, step, sizeOf(entries, step.Name), report)
			if err != nil {
				return err
			}
			if drop != nil {
				unlinks = append(unlinks, *drop)
			} else if s.lostDisk(id) {
				return nil
			}
		case solver.LinkUnnamed:
			slog.Warn("a link of the guard holds data that nothing names: it stays, for the user to look at", "disk", id, "link", step.Name)
			unnamed++
		case solver.LinkStray:
			slog.Warn("the links folder holds something Atoll did not make: it stays", "disk", id, "name", step.Name)
		case solver.LinkMake:
			makes = append(makes, catalog.LinkStart{FileID: step.FileID, Path: step.Path, FileKey: step.Inode})
		}
	}
	if err := s.countUnnamed(ctx, id, unnamed); err != nil {
		return err
	}
	if _, err := s.linkOps(ctx, handle, catalog.OpUnlink, unlinks); err != nil || s.lostDisk(id) {
		return err
	}
	if _, err := s.linkOps(ctx, handle, catalog.OpLink, makes); err != nil || s.lostDisk(id) {
		return err
	}
	if !on {
		// Empty once every link went, the folder goes too; one that still holds something stays.
		handle.RemoveLinksFolder()
	}
	return nil
}

// linksInput reads what deciding a disk's links needs. Everything here is held until the last link is decided, for
// a disk of 200,000 copies (specs.md §26): each copy is read once, straight into the plan's list, and a link that
// leads to its file's copy, as nearly every link does, shares the text of that copy's inode.
func (s *Session) linksInput(ctx context.Context, id string, on bool, entries []ops.LinkEntry) (*solver.LinksInput, error) {
	in := &solver.LinksInput{On: on, Links: make([]solver.LinkEntry, 0, len(entries))}
	err := catalog.LinkCopies(ctx, s.db, id,
		func(n int) { in.Copies = make([]solver.LinkCopy, 0, n) },
		func(c *catalog.LinkCopy) {
			in.Copies = append(in.Copies, solver.LinkCopy{FileID: c.FileID, Inode: c.FileKey, Place: c.Place, There: c.There})
		})
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	// Only a link that does not lead to its file's copy can need a path, and at a pass nearly every link does.
	var ids []int64
	var digits [20]byte
	for i := range entries {
		e := &entries[i]
		inode := strconv.AppendUint(digits[:0], e.Ino, 10)
		fileID, named := layout.LinkID(e.Name)
		key := ""
		if named {
			key = copyInode(in.Copies, fileID)
		}
		if !named || key != string(inode) {
			key = string(inode)
			if named {
				ids = append(ids, fileID)
			}
		}
		in.Links = append(in.Links, solver.LinkEntry{Name: e.Name, File: e.File, Inode: key})
	}
	if in.Trash, err = catalog.TrashKeys(ctx, s.db, id); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if in.Files, err = catalog.FilePaths(ctx, s.db, ids); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var unknown []int64
	for _, fileID := range ids {
		if _, ok := in.Files[fileID]; !ok {
			unknown = append(unknown, fileID)
		}
	}
	if in.Gone, err = catalog.GonePaths(ctx, s.db, id, unknown); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return in, nil
}

// copyInode gives the inode of a file's copy, empty when the disk has none of it: the copies come by file id, so it is
// found by search, with no map of them.
func copyInode(copies []solver.LinkCopy, fileID int64) string {
	i, found := slices.BinarySearchFunc(copies, fileID, func(c solver.LinkCopy, id int64) int { return cmp.Compare(c.FileID, id) })
	if !found {
		return ""
	}
	return copies[i].Inode
}

// sizeOf gives the size a look found of a link: the links come by name, so it is found by search, with no map of them.
func sizeOf(entries []ops.LinkEntry, name string) int64 {
	i, found := slices.BinarySearchFunc(entries, name, func(e ops.LinkEntry, name string) int { return cmp.Compare(e.Name, name) })
	if !found {
		return 0
	}
	return entries[i].Size
}

// sendLink sends a link to the trash, as a copy goes there (specs.md §17.1). Its data may still have its name in the
// root, as a file a rule now ignores leaves it: then the link only goes, and drop says so.
func (s *Session) sendLink(ctx context.Context, handle *ops.Disk, step *solver.LinkStep, size int64, report func(*LinkItem) error) (*catalog.LinkStart, error) {
	switch seen, there, err := handle.StatRoot(step.Path); {
	case err != nil:
		return nil, s.guardFailed(ctx, handle.ID, err)
	case there && seen.Kind == fsx.KindFile && strconv.FormatUint(seen.Ino, 10) == step.Inode:
		return &catalog.LinkStart{FileID: step.FileID, Path: step.Path, FileKey: step.Inode}, nil
	}
	if s.full[handle.ID] {
		// No room for another folder in that trash: it waits for the next pass, as any move to it does.
		return nil, nil
	}
	op, err := catalog.BeginTrash(ctx, s.db, &catalog.TrashStart{
		Disk: handle.ID, Path: step.Path, FileKey: step.Inode, Size: sql.NullInt64{Int64: size, Valid: true}, Reason: step.Reason,
		NowNS: s.now, ExpiresNS: ops.AddRetention(s.now, s.retention), Link: step.FileID,
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	outcome := OutcomeTrashed
	moved, moveErr := handle.LinkToTrash(op)
	if moveErr == nil {
		if err := catalog.FinishTrash(ctx, s.db, op, nil, moved.Links()); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	} else {
		if fsx.IsNoRoom(moveErr) {
			s.full[handle.ID] = true
		}
		if outcome, err = s.afterFailedMove(ctx, handle, op, moveErr); err != nil {
			return nil, err
		}
	}
	if report != nil {
		if err := report(&LinkItem{Disk: handle.ID, Path: step.Path, Reason: step.Reason, Outcome: outcome}); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	return nil, nil
}

// linkOps journals and runs operations of the guard on links, a batch at a time, and says how many went through. A
// link that is no longer what the plan saw stays, undone, for the next pass to look at again.
func (s *Session) linkOps(ctx context.Context, handle *ops.Disk, kind string, list []catalog.LinkStart) (int, error) {
	if len(list) == 0 {
		return 0, nil
	}
	links, err := handle.OpenLinks(kind == catalog.OpLink)
	if err != nil {
		return 0, s.guardFailed(ctx, handle.ID, err)
	}
	if links == nil {
		return 0, nil
	}
	defer links.Close()
	through := 0
	for start := 0; start < len(list); start += linkBatch {
		batch := list[start:min(start+linkBatch, len(list))]
		begun, err := catalog.BeginLinks(ctx, s.db, handle.ID, kind, batch, s.now)
		if err != nil {
			return through, fmt.Errorf("catalog: %w", err)
		}
		s.machine.Checkpoint("journaled")
		var done []catalog.LinkEnd
		var undone []int64
		for i := range begun {
			op := &begun[i]
			var left *catalog.Links
			if kind == catalog.OpLink {
				left, err = handle.MakeLink(op, links)
			} else {
				left, err = s.dropLink(ctx, handle, op, links)
			}
			switch {
			case err == nil:
				done = append(done, catalog.LinkEnd{ID: op.ID, Links: left})
			case errors.Is(err, ops.ErrDiskLost):
				// What this batch did stays planned, for the reprise to tell.
				return through, s.lose(ctx, handle.ID, err)
			default:
				slog.Warn("a link of the guard stays as it is", "disk", handle.ID, "kind", kind, "file", op.Link, "error", err)
				undone = append(undone, op.ID)
			}
		}
		if err := links.Sync(); err != nil {
			return through, s.lose(ctx, handle.ID, err)
		}
		if err := catalog.EndLinks(ctx, s.db, handle.ID, done, undone); err != nil {
			return through, fmt.Errorf("catalog: %w", err)
		}
		through += len(done)
	}
	return through, nil
}

// dropLink removes a link, and says what the names its data keeps must record: the removal moved its change time.
func (s *Session) dropLink(ctx context.Context, handle *ops.Disk, op *catalog.Op, links *ops.LinksFolder) (*catalog.Links, error) {
	prior, err := handle.DropLink(op, links)
	if err != nil {
		return nil, err
	}
	left, err := handle.LinksLeft(ctx, s.db, op.FileKey, prior)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return left, nil
}

// dropLinksOf removes the links of the guard that lead to an entry about to be purged: Atoll sent that data to the
// trash itself, and a link left behind would keep it once purged, and send it back to the trash at the next pass.
func (s *Session) dropLinksOf(ctx context.Context, handle *ops.Disk, e *catalog.TrashEntry) error {
	ids, err := handle.OwnLinksOf(e.TrashPath, e.FileKey)
	if err != nil {
		return s.guardFailed(ctx, e.Disk, err)
	}
	list := make([]catalog.LinkStart, len(ids))
	for i, fileID := range ids {
		list[i] = catalog.LinkStart{FileID: fileID, Path: e.Path, FileKey: e.FileKey}
	}
	_, err = s.linkOps(ctx, handle, catalog.OpUnlink, list)
	return err
}

// countUnnamed records on a disk's row the links its run found whose data nothing names, for the alert link-unnamed
// and atoll status (specs.md §17, §23).
func (s *Session) countUnnamed(ctx context.Context, id string, links int) error {
	if err := catalog.SetLinksUnnamed(ctx, s.db, id, links); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// guardFailed loses a disk that failed, and lets anything else wait for the next pass: a link is a second name.
func (s *Session) guardFailed(ctx context.Context, id string, err error) error {
	if errors.Is(err, ops.ErrDiskLost) {
		return s.lose(ctx, id, err)
	}
	slog.Warn("the links of the guard wait for the next pass", "disk", id, "error", err)
	return nil
}

// lostDisk says that a disk can no longer be written to in this session.
func (s *Session) lostDisk(id string) bool {
	return !s.writable(id)
}
