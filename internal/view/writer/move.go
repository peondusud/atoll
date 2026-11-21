package writer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/view"
)

// A new file whose write runs out of space moves to another disk, with view.move_on_enospc, as mergerfs's moveonenospc
// moves it (specs.md §21.2; the maintainer, 2026-11-21). Its copy is made on the other disk by the copy pipeline's steps,
// journaled first, read back without the cache, and at its name before the file's handle goes there; then the copy left
// on the full disk goes to that disk's trash, and is purged at once. The program's writes wait meanwhile, and the write
// that ran out of space is made on the other disk.

// Why a file stays where it is.
var (
	errNotAlone     = errors.New("it was opened again to be written")
	errHeld         = errors.New("the catalog holds a file at its path")
	errNotPresented = errors.New("its path no longer presents it from its disk")
	errChanged      = errors.New("it changed meanwhile")
	errLinked       = errors.New("it has another name")
	errMismatch     = errors.New("its copy does not read back as it was written")
)

// move moves a file whose write ran out of space to another disk (specs.md §21.2), under f.mu held alone, which holds
// back whatever else reaches its handle. write is the size of that write. It says why it did not move the file, the
// file then still open where it was.
func (w *Writer) move(f *File, write int) error {
	ctx := f.ctx
	st, err := f.WritableFile.Info()
	if err != nil {
		return fmt.Errorf("look: %w", err)
	}
	m, err := w.startMove(ctx, f, &st, st.Size+int64(write))
	if err != nil {
		return err
	}
	w.machine.Checkpoint("move-journaled")
	if err := m.copy(ctx, f.WritableFile, &st); err != nil {
		w.dropMove(ctx, f, m, err)
		return err
	}
	w.machine.Checkpoint("move-copied")
	// What the trash compares the copy left with, right before it moves it: the file as it was copied.
	row, err := knownOf(f.Path, &st)
	if err != nil {
		w.dropMove(ctx, f, m, err)
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.mayMove(ctx, f, &st); err != nil {
		w.dropMove(ctx, f, m, err)
		return err
	}
	if err := w.placeMove(ctx, f, m); err != nil {
		return err
	}
	w.machine.Checkpoint("move-placed")
	now := w.now()
	trashed, err := catalog.ViewMoveTrash(ctx, w.db, m.v, &catalog.TrashStart{
		Disk: m.v.Disk2, Path: f.Path, FileKey: m.v.FileKey, Size: row.Size, Reason: catalog.ReasonSurplus, NowNS: now, ExpiresNS: expires(now, w.retention),
	})
	if err != nil {
		// Its copy is at its name on the other disk, the same bytes as here: the reprise finishes it (specs.md §21.2).
		f.movable = false
		_ = m.part.Close()
		w.log.Warn("a file moved for room stays planned: the journal cannot be written", "disk", m.v.Disk, "path", f.Path, "error", err)
		return fmt.Errorf("%w", err)
	}
	w.machine.Checkpoint("move-trash-journaled")
	// From here on the file is on the other disk: its handle is the part's, and the view presents it from there.
	left := f.WritableFile
	f.WritableFile, f.Disk = m.part.WritableFile, m.v.Disk
	f.moves++
	if err := left.Close(); err != nil {
		w.log.Warn("the handle a moved file left did not close cleanly", "disk", m.v.Disk2, "path", f.Path, "error", err)
	}
	w.machine.Checkpoint("move-swapped")
	w.log.Info("a file whose write ran out of space moved to another disk", "from", m.v.Disk2, "to", m.v.Disk, "path", f.Path, "size", st.Size)
	w.leave(ctx, m.v, trashed, row)
	return nil
}

// moving is a move under way: its row, its create, its part, open, and the writer's log.
type moving struct {
	v    *catalog.ViewOp
	op   *catalog.Op
	part *ops.Part
	log  *slog.Logger
}

// startMove checks that a file may move, picks the disk it goes to, journals the move and makes its part, under the
// writer's lock (specs.md §21.2, step 1). need is the room the file needs there: its size now and the write.
func (w *Writer) startMove(ctx context.Context, f *File, st *fsx.Stat, need int64) (*moving, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.mayMove(ctx, f, st); err != nil {
		return nil, err
	}
	target, err := w.placeFor(ctx, f.Path, f.Disk, need)
	if err != nil {
		return nil, err
	}
	if err := w.mayCreate(ctx, asking(f.who), target, f.Path); err != nil {
		return nil, err
	}
	m := &moving{v: &catalog.ViewOp{Disk: target, Disk2: f.Disk, Path: f.Path, FileKey: fmt.Sprintf("%d", st.Ino), AtNS: w.now()}, log: w.log}
	if m.op, err = catalog.BeginViewMove(ctx, w.db, m.v); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	err = w.handles.Use(target, func(d *ops.Disk) error {
		var err error
		if m.part, err = d.MakePart(m.op); err != nil {
			return err //nolint:wrapcheck // ops says what failed
		}
		return catalog.SetCreateKey(ctx, w.db, m.op, fmt.Sprintf("%d", m.part.Stat.Ino)) //nolint:wrapcheck // the catalog says what failed
	})
	if err != nil {
		w.dropMove(ctx, f, m, err)
		return nil, err
	}
	return m, nil
}

// mayMove says whether a file may still move, under the writer's lock: the file the view made, open through that one
// handle alone, at a path the catalog holds no file at, which still presents it from its disk, and as it was when the
// move began, with no other name.
func (w *Writer) mayMove(ctx context.Context, f *File, st *fsx.Stat) error {
	if f.again || w.writing[f.Path] != f {
		return errNotAlone
	}
	held, err := catalog.FileAt(ctx, w.db, f.Path)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case held != nil:
		return errHeld
	}
	n, err := w.tree.Lookup(ctx, f.Path)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case n == nil, n.Kind != view.NodeFile, n.Disk != f.Disk:
		return errNotPresented
	}
	return w.handles.Use(f.Disk, func(d *ops.Disk) error {
		there, found, err := d.StatRoot(f.Path)
		switch {
		case err != nil:
			return err //nolint:wrapcheck // ops says what failed
		case !found || there.Kind != fsx.KindFile || there.Ino != st.Ino:
			return errNotPresented
		case !sameAs(&there, st):
			return errChanged
		case there.Nlink != 1:
			// A name a hard link gave it would keep what the move leaves, and lose what is written from then on.
			return errLinked
		}
		return nil
	})
}

// sameAs says whether a file is as a look found it: its size, modification time, change time and inode.
func sameAs(a, b *fsx.Stat) bool {
	return a.Ino == b.Ino && a.Size == b.Size && a.Mtime == b.Mtime && a.Ctime == b.Ctime
}

// copy copies what a file holds into the move's part, each chunk hashed as it is read, then gives the part the file's
// modification time, owner, mode and user. extended attributes, syncs it and reads it again without its cache, each
// chunk against its hash, as the copy pipeline does (specs.md §14, §21.2, steps 2 to 4).
func (m *moving) copy(ctx context.Context, src fsx.WritableFile, st *fsx.Stat) error {
	buf := make([]byte, integrity.ChunkSize)
	var sums [][integrity.HashSize]byte
	for off := int64(0); off < st.Size; off += integrity.ChunkSize {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("move: %w", err)
		}
		want := min(integrity.ChunkSize, st.Size-off)
		n, err := src.ReadAt(buf[:want], off)
		switch {
		case int64(n) < want && (err == nil || errors.Is(err, io.EOF)):
			return errChanged
		case err != nil && !errors.Is(err, io.EOF):
			return fmt.Errorf("read: %w", err)
		}
		if _, err := m.part.WriteAt(buf[:n], off); err != nil {
			return partErr("write", err)
		}
		sums = append(sums, integrity.ChunkSum(buf[:n]))
	}
	after, err := src.Info()
	switch {
	case err != nil:
		return fmt.Errorf("look: %w", err)
	case !sameAs(&after, st):
		return errChanged
	}
	if err := m.restore(src, st); err != nil {
		return err
	}
	if err := m.part.Sync(); err != nil {
		return fmt.Errorf("%w: sync: %w", ops.ErrDiskLost, err)
	}
	if err := m.part.DropCache(); err != nil {
		return partErr("drop the cache", err)
	}
	for idx, off := 0, int64(0); off < st.Size; idx, off = idx+1, off+integrity.ChunkSize {
		want := min(integrity.ChunkSize, st.Size-off)
		n, err := m.part.ReadAt(buf[:want], off)
		switch {
		case err != nil && !errors.Is(err, io.EOF):
			return partErr("read back", err)
		case int64(n) < want, integrity.ChunkSum(buf[:n]) != sums[idx]:
			return errMismatch
		}
	}
	held, err := m.part.Info()
	switch {
	case err != nil:
		return partErr("look", err)
	case held.Size != st.Size:
		return errMismatch
	}
	return nil
}

// restore gives the part the file's modification time, then its owner, which takes setuid and setgid off, then its
// mode, without those two bits when the owner was refused, then its user. extended attributes; one the disk refuses
// is left out, with a line of the log, as a replica goes without it (specs.md §14).
func (m *moving) restore(src fsx.WritableFile, st *fsx.Stat) error {
	if err := m.part.SetTimes(fsx.OmitTime, st.Mtime); err != nil {
		return partErr("times", err)
	}
	mode := st.Mode
	if err := m.part.SetOwner(st.Uid, st.Gid); err != nil {
		if fsx.IsDiskLost(err) {
			return partErr("owner", err)
		}
		m.log.Warn("a moved file keeps another owner", "disk", m.v.Disk, "path", m.v.Path, "error", err)
		mode &^= 0o6000
	}
	if err := m.part.SetMode(mode); err != nil {
		return partErr("mode", err)
	}
	attrs, err := src.Xattrs(fsx.UserNamespace)
	if err != nil {
		return fmt.Errorf("read the attributes: %w", err)
	}
	have, err := m.part.Xattrs(fsx.UserNamespace)
	if err != nil {
		return partErr("attributes", err)
	}
	plan := solver.PlanXattrs(attrs, have)
	for _, name := range plan.Set {
		switch err := m.part.SetXattr(name, attrs[name], 0); {
		case err == nil:
		case fsx.IsXattrRefused(err):
			m.log.Warn("a moved file goes without an extended attribute its disk refuses", "disk", m.v.Disk, "path", m.v.Path, "name", name, "error", err)
		default:
			return partErr("attributes", err)
		}
	}
	return nil
}

// partErr says what failed on the part: no room, or the disk lost, or anything else.
func partErr(what string, err error) error {
	if fsx.IsDiskLost(err) {
		return fmt.Errorf("%w: %s: %w", ops.ErrDiskLost, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// placeMove renames the part to the file's path on the disk it goes to, after the folders that disk lacks, each made as
// the view presents it (specs.md §21.2, step 5). A rename that may not have happened is looked at: what did not happen
// drops the move; what cannot be told, on a disk that cannot be relied on, leaves it planned for the view's reprise.
func (w *Writer) placeMove(ctx context.Context, f *File, m *moving) error {
	err := w.handles.Use(m.v.Disk, func(d *ops.Disk) error {
		_, err := d.PlaceUserPart(m.op, f.Path, w.like(ctx))
		if err == nil || errors.Is(err, ops.ErrDiskLost) {
			return err //nolint:wrapcheck // ops says what failed
		}
		placed, look := holdsAt(d, f.Path, m.op.FileKey, false)
		switch {
		case look != nil:
			return fmt.Errorf("%w: %w", ops.ErrDiskLost, look)
		case placed:
			// The name holds the part all the same: the move goes on.
			return nil
		}
		return err //nolint:wrapcheck // ops says what failed
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ops.ErrDiskLost), errors.Is(err, ErrNoHandle):
		f.movable = false
		_ = m.part.Close()
		w.log.Warn("a file moved for room stays planned: the disk it goes to cannot be relied on", "disk", m.v.Disk, "path", f.Path, "error", err)
		return err
	}
	w.dropMove(ctx, f, m, err)
	return err
}

// dropMove ends a move that stopped before its part was at its name: the part removed, and its create and the move
// undone. On a disk that cannot be relied on, both stay planned, for the view's reprise, and the file never moves again.
func (w *Writer) dropMove(ctx context.Context, f *File, m *moving, why error) {
	if m.part != nil {
		_ = m.part.Close()
	}
	err := w.handles.Use(m.v.Disk, func(d *ops.Disk) error { return d.DropPart(m.op.Path2) }) //nolint:wrapcheck // ops says what failed
	if err != nil {
		f.movable = false
		w.log.Warn("a file moved for room stays planned: its part cannot be removed", "disk", m.v.Disk, "path", f.Path, "error", err, "reason", why)
		return
	}
	if err := catalog.EndViewMove(ctx, w.db, m.v, catalog.OpUndone, catalog.OpUndone, false, w.now()); err != nil {
		f.movable = false
		w.log.Warn("a file moved for room stays planned: the journal cannot be written", "disk", m.v.Disk, "path", f.Path, "error", err)
	}
}

// leave sends the copy a move left on the full disk to its trash, then purges it from there at once, each journaled,
// and says the move done (specs.md §21.2, steps 8 and 9). A trash with no room for it, its folders or its rename
// answered ENOSPC or EDQUOT, nothing moved, has it removed at its path instead (the maintainer, 2026-11-21). What stops
// on a disk that cannot be relied on stays planned, for the view's reprise; a copy its trash refuses stays at its path,
// beside the moved one.
func (w *Writer) leave(ctx context.Context, v *catalog.ViewOp, trashed *catalog.Op, row *catalog.KnownCopy) {
	err := w.handles.Use(v.Disk2, func(d *ops.Disk) error {
		if _, err := d.ToTrash(trashed, row); err != nil {
			if errors.Is(err, ops.ErrDiskLost) {
				return err //nolint:wrapcheck // ops says what failed
			}
			if noRoom(err) {
				done, rerr := w.removeHere(ctx, d, v, trashed, row, err)
				if done || rerr != nil {
					return rerr
				}
			}
			// Looked at as a reprise looks at it, by the table of §17.2.
			finished, rerr := d.ResolveNow(ctx, w.db, trashed, w.now(), w.retention)
			if rerr != nil {
				return fmt.Errorf("%w: %w", err, rerr)
			}
			if !finished {
				w.log.Warn("the copy a moved file left stays at its path", "disk", v.Disk2, "path", v.Path, "error", err)
				return catalog.EndViewMove(ctx, w.db, v, "", catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
			}
		}
		w.machine.Checkpoint("move-trashed")
		return w.purgeLeft(ctx, d, v, trashed)
	})
	if err != nil {
		w.log.Warn("a file moved for room stays planned, its copy left on the full disk to the reprise", "disk", v.Disk2, "path", v.Path, "error", err)
	}
}

// removeHere removes at its path the copy a move left on a full disk whose trash had no room for it, why the error that
// said so (specs.md §21.2): only once a look finds that nothing moved, the copy at its path and none in the trash; then
// the trash undone and the unlink journaled with its purge event, in one transaction, and the copy removed after the
// checks the trash makes, against what the move copied. It says whether it ended the move: false leaves it to the table
// of §17.2, as something moved after all.
func (w *Writer) removeHere(ctx context.Context, d *ops.Disk, v *catalog.ViewOp, trashed *catalog.Op, row *catalog.KnownCopy, why error) (bool, error) {
	left, err := holdsAt(d, v.Path, trashed.FileKey, false)
	if err != nil {
		return false, err
	}
	inTrash, err := w.trashed(ctx, d, trashed.ID)
	if err != nil || !left || inTrash {
		return false, err
	}
	// The folders the trash made before it ran out of room hold nothing.
	d.CleanTrash(trashed.Path2)
	here, err := catalog.ViewMoveRemoveHere(ctx, w.db, v, trashed, w.now())
	if err != nil {
		return true, fmt.Errorf("%w", err)
	}
	w.machine.Checkpoint("move-unlink-journaled")
	switch err := d.RemoveLeft(here, row); {
	case err == nil:
		w.log.Info("the copy a moved file left is removed at its path: its trash had no room for it", "disk", v.Disk2, "path", v.Path, "reason", why)
		return true, catalog.EndViewMove(ctx, w.db, v, catalog.OpDone, catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
	case errors.Is(err, ops.ErrDiskLost):
		return true, err //nolint:wrapcheck // ops says what failed
	default:
		// Changed since it was copied, or gone: what is there stays, beside the moved copy.
		w.log.Warn("the copy a moved file left stays at its path", "disk", v.Disk2, "path", v.Path, "error", err)
		return true, catalog.EndViewMove(ctx, w.db, v, catalog.OpUndone, catalog.OpDone, errors.Is(err, ops.ErrGone), w.now()) //nolint:wrapcheck // the catalog says what failed
	}
}

// purgeLeft purges the copy a move sent to the full disk's trash, journaled, and says the move done: a purge that
// cannot be made leaves the copy in the trash, which its retention or trash.max_size empties.
func (w *Writer) purgeLeft(ctx context.Context, d *ops.Disk, v *catalog.ViewOp, trashed *catalog.Op) error {
	purge, err := catalog.ViewMovePurge(ctx, w.db, v, trashed, w.now())
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	w.machine.Checkpoint("move-purge-journaled")
	if _, err := d.PurgeEntry(purge); err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return err //nolint:wrapcheck // ops says what failed
		}
		w.log.Warn("the copy a moved file left stays in the trash", "disk", v.Disk2, "path", v.Path, "error", err)
		if errors.Is(err, ops.ErrGone) {
			return catalog.EndViewMove(ctx, w.db, v, catalog.OpUndone, catalog.OpDone, true, w.now()) //nolint:wrapcheck // the catalog says what failed
		}
		if _, rerr := d.ResolveNow(ctx, w.db, purge, w.now(), w.retention); rerr != nil {
			return fmt.Errorf("%w: %w", err, rerr)
		}
		return catalog.EndViewMove(ctx, w.db, v, "", catalog.OpDone, false, w.now()) //nolint:wrapcheck // the catalog says what failed
	}
	w.machine.Checkpoint("move-purged")
	if err := catalog.EndViewMove(ctx, w.db, v, catalog.OpDone, catalog.OpDone, false, w.now()); err != nil {
		return fmt.Errorf("%w", err)
	}
	d.CleanTrash(purge.Path2)
	return nil
}
