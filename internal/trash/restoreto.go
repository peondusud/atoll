package trash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/watch"
)

// RestoreTo puts back, for each path at or below under, its newest entry on the disk to (specs.md §17.4): moved back
// when to is the entry's own disk, and copied there through the copy pipeline otherwise, the entry staying in its
// trash.
func (s *Session) RestoreTo(ctx context.Context, under, to string, each func(*EntryItem) error) (*Summary, error) {
	return s.restore(ctx, under, func(ctx context.Context, e *catalog.TrashEntry) (string, error) {
		if e.Disk == to {
			return s.restoreEntry(ctx, e)
		}
		return s.restoreTo(ctx, e, to)
	}, each)
}

// readableSource says whether the trash of a disk can be read: online, in standby, read-only or draining.
func (s *Session) readableSource(id string) bool {
	sl := s.slots[id]
	return sl != nil && readable(sl.state)
}

// restoreTo copies one entry to another disk, as a copy is made (specs.md §14.2, §17.4).
func (s *Session) restoreTo(ctx context.Context, e *catalog.TrashEntry, to string) (string, error) {
	sl := s.slots[to]
	if sl == nil || sl.config == nil || !s.writable(to) || !s.readableSource(e.Disk) {
		return outcomeUnavailable, nil
	}
	if problems, err := names.CheckPath(e.Path, sl.config.FS); err != nil || len(problems) > 0 {
		return OutcomeNameRefused, nil //nolint:nilerr // a path the target refuses is this entry's outcome
	}
	dst, err := s.open(ctx, to)
	if err != nil {
		return "", err
	}
	src, err := s.open(ctx, e.Disk)
	if err != nil {
		return "", err
	}
	if dst == nil || src == nil || !s.writable(to) || !s.readableSource(e.Disk) {
		return outcomeUnavailable, nil
	}
	taken, missing, err := dst.LookAt(e.Path)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return outcomeUnavailable, s.lose(ctx, to, err)
	case err != nil:
		slog.Warn("a path cannot be looked at", "disk", to, "error", err)
		return OutcomeIOError, nil
	case taken:
		return OutcomePathTaken, nil
	}
	behind, err := catalog.TakenByRename(ctx, s.db, to, e.Path)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	if behind {
		return OutcomePathTaken, nil
	}
	if collides, err := s.collidesOn(ctx, to, e.Path); err != nil || collides {
		if err != nil {
			return "", err
		}
		return OutcomeNameCollision, nil
	}
	if !s.apply {
		return OutcomeToRestore, nil
	}
	op, err := catalog.BeginRestoreTo(ctx, s.db, to, e, s.now)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	outcome, err := s.copyFromTrash(ctx, dst, src, op, e, missing)
	if err != nil || outcome != OutcomeRestored {
		return outcome, err
	}
	s.changed = true
	return OutcomeRestored, nil
}

// copyFromTrash runs the copy of an entry to another disk, up to its record, and says how it ended. What ends it
// otherwise leaves no part, and the operation undone; a disk that cannot be relied on keeps the operation planned,
// for the reprise.
func (s *Session) copyFromTrash(ctx context.Context, dst, src *ops.Disk, op *catalog.Op, e *catalog.TrashEntry, missing []string) (string, error) {
	part, err := dst.MakePart(op)
	if err != nil {
		return s.copyFailed(ctx, dst, op, false, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = part.Close()
		}
	}()
	if err := catalog.SetCreateKey(ctx, s.db, op, strconv.FormatUint(part.Stat.Ino, 10)); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	source, read, err := src.OpenTrashSource(e.TrashPath, e.FileKey)
	switch {
	case errors.Is(err, ops.ErrGone):
		return s.copyEnded(ctx, dst, op, OutcomeChanged, true)
	case errors.Is(err, ops.ErrDiskLost):
		return OutcomeIOError, s.dropAndLose(ctx, dst, op, e.Disk, err)
	case err != nil:
		slog.Warn("an entry of the trash cannot be read", "disk", e.Disk, "op", e.OpID, "error", err)
		return s.copyEnded(ctx, dst, op, OutcomeIOError, false)
	}
	defer func() { _ = source.Close() }()
	hashes, err := fillPart(ctx, part, source, read.Size)
	var failedRead sourceError
	switch {
	case errors.Is(err, errSourceShort):
		return s.copyEnded(ctx, dst, op, OutcomeChanged, true)
	case errors.As(err, &failedRead) && fsx.IsDiskLost(failedRead.err):
		// The disk the entry is on, not the one the copy goes to.
		return OutcomeIOError, s.dropAndLose(ctx, dst, op, e.Disk, err)
	case errors.As(err, &failedRead):
		slog.Warn("an entry of the trash cannot be read", "disk", e.Disk, "op", e.OpID, "error", err)
		return s.copyEnded(ctx, dst, op, OutcomeIOError, false)
	case err != nil:
		return s.copyFailed(ctx, dst, op, true, err)
	}
	s.machine.Checkpoint("copied")
	switch still, err := src.TrashSourceStill(e.TrashPath, &read); {
	case err != nil || !still:
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.dropAndLose(ctx, dst, op, e.Disk, err)
		}
		return s.copyEnded(ctx, dst, op, OutcomeChanged, true)
	}
	if err := giveMetadata(part, &read, op.Disk, e.Path); err != nil {
		return s.copyFailed(ctx, dst, op, true, err)
	}
	whole, ok, err := verifyPart(ctx, part, hashes, read.Size)
	switch {
	case err != nil:
		return s.copyFailed(ctx, dst, op, true, err)
	case !ok || (e.Hash != nil && string(whole) != string(e.Hash)):
		slog.Warn("a copy from the trash did not read back as the entry", "disk", op.Disk, "path", e.Path)
		return s.copyEnded(ctx, dst, op, OutcomeIOError, false)
	}
	s.machine.Checkpoint("verified")
	closed = true
	if err := part.Close(); err != nil {
		return s.copyFailed(ctx, dst, op, true, err)
	}
	for _, folder := range missing {
		if err := s.makeFolderOn(ctx, op.Disk, dst, src, e, folder); err != nil {
			if errors.As(err, new(catalogError)) {
				return "", err
			}
			return s.copyFailed(ctx, dst, op, true, err)
		}
	}
	moved, err := dst.PlacePart(op, e.Path)
	switch {
	case errors.Is(err, ops.ErrPathTaken):
		return s.copyEnded(ctx, dst, op, OutcomePathTaken, false)
	case err != nil:
		return s.copyFailed(ctx, dst, op, true, err)
	}
	found, err := watch.FileFrom(e.Path, &moved.Stat)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if err := catalog.FinishRestoreTo(ctx, s.db, op, s.now, ops.NewCopyOf(&found), moved.Links()); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return OutcomeRestored, nil
}

// errSourceShort says that the file of the trash was shorter than when it was opened: it changed meanwhile.
var errSourceShort = errors.New("the file is shorter than when it was opened")

// sourceError is a failure to read the file of the trash, which says something of its disk, not of the target.
type sourceError struct{ err error }

func (e sourceError) Error() string { return "read: " + e.err.Error() }
func (e sourceError) Unwrap() error { return e.err }

// fillPart copies a file into a part, chunk by chunk, and gives each chunk's hash, as a copy does (specs.md §14.2).
func fillPart(ctx context.Context, part *ops.Part, source fsx.File, size int64) ([]byte, error) {
	buf := make([]byte, integrity.ChunkSize)
	var hashes []byte
	for off := int64(0); off < size; off += integrity.ChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
		want := min(integrity.ChunkSize, size-off)
		n, err := source.ReadAt(buf[:want], off)
		switch {
		case int64(n) < want && (err == nil || errors.Is(err, io.EOF)):
			return nil, errSourceShort
		case err != nil && !errors.Is(err, io.EOF):
			return nil, sourceError{err}
		}
		if _, err := part.WriteAt(buf[:n], off); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
		sum := integrity.ChunkSum(buf[:n])
		hashes = append(hashes, sum[:]...)
	}
	return hashes, nil
}

// giveMetadata gives the part the source's modification time, mode and owner, never its creation time, and syncs it
// (specs.md §14). An owner the target refuses is only logged, as for a copy.
func giveMetadata(part *ops.Part, read *fsx.Stat, disk, path string) error {
	if err := part.SetTimes(fsx.OmitTime, read.Mtime); err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := part.SetMode(read.Mode); err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := part.SetOwner(read.Uid, read.Gid); err != nil {
		if fsx.IsDiskLost(err) {
			return fmt.Errorf("%w", err)
		}
		slog.Warn("a copy keeps another owner", "disk", disk, "path", path, "error", err)
	}
	if err := part.Sync(); err != nil {
		return fmt.Errorf("%w: %w", ops.ErrDiskLost, err)
	}
	return nil
}

// verifyPart reads the part again without its cache, each chunk against its hash, and gives the whole-file hash, and
// whether every chunk read back as written (specs.md §14, step 5).
func verifyPart(ctx context.Context, part *ops.Part, hashes []byte, size int64) ([]byte, bool, error) {
	if err := part.DropCache(); err != nil {
		return nil, false, fmt.Errorf("%w", err)
	}
	h, err := integrity.NewHasher(integrity.ChunkSize)
	if err != nil {
		return nil, false, fmt.Errorf("restore: %w", err)
	}
	buf := make([]byte, integrity.ChunkSize)
	var off int64
	for idx := 0; off < size; idx++ {
		if err := ctx.Err(); err != nil {
			return nil, false, fmt.Errorf("restore: %w", err)
		}
		n, err := part.ReadAt(buf[:min(integrity.ChunkSize, size-off)], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, fmt.Errorf("%w", err)
		}
		sum := integrity.ChunkSum(buf[:n])
		if n == 0 || (idx+1)*integrity.HashSize > len(hashes) || string(sum[:]) != string(hashes[idx*integrity.HashSize:(idx+1)*integrity.HashSize]) {
			return nil, false, nil
		}
		_, _ = h.Write(buf[:n])
		off += int64(n)
	}
	if st, err := part.Info(); err != nil || st.Size != size {
		return nil, false, nil //nolint:nilerr // a part of another size did not read back as written
	}
	whole, _ := h.Sum()
	return whole[:], true, nil
}

// copyEnded ends a copy that will not be: the part goes, and the operation is undone, with an event when warn says
// that what it read was not what it expected.
func (s *Session) copyEnded(ctx context.Context, dst *ops.Disk, op *catalog.Op, outcome string, warn bool) (string, error) {
	if err := dst.DropPart(op.Path2); err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, op.Disk, err)
		}
		// The operation stays planned: its reprise removes the part.
		slog.Warn("a part stays, for the reprise", "disk", op.Disk, "op", op.ID, "error", err)
		return OutcomeIOError, nil
	}
	if err := catalog.Undo(ctx, s.db, op, warn, s.now); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return outcome, nil
}

// copyFailed ends a copy on an error a disk gave: a disk that cannot be relied on keeps the operation planned, for
// the reprise, and anything else ends it as io-error. made says whether the part was made.
func (s *Session) copyFailed(ctx context.Context, dst *ops.Disk, op *catalog.Op, made bool, cause error) (string, error) {
	if errors.Is(cause, ops.ErrDiskLost) || fsx.IsDiskLost(cause) {
		return OutcomeIOError, s.lose(ctx, op.Disk, cause)
	}
	slog.Warn("a copy from the trash did not happen", "disk", op.Disk, "op", op.ID, "error", cause)
	if !made {
		if err := catalog.Undo(ctx, s.db, op, false, s.now); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return OutcomeIOError, nil
	}
	return s.copyEnded(ctx, dst, op, OutcomeIOError, false)
}

// dropAndLose ends a copy whose source's disk cannot be relied on: the part goes, and that disk is lost.
func (s *Session) dropAndLose(ctx context.Context, dst *ops.Disk, op *catalog.Op, source string, cause error) error {
	if _, err := s.copyEnded(ctx, dst, op, OutcomeIOError, false); err != nil {
		return err
	}
	return s.lose(ctx, source, cause)
}

// collidesOn says whether an NTFS disk holds a name that collides with a path or a folder on the way to it (specs.md
// §15.7).
func (s *Session) collidesOn(ctx context.Context, disk, path string) (bool, error) {
	if sl := s.slots[disk]; sl == nil || sl.config == nil || sl.config.FS != names.NTFS {
		return false, nil
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		taken, err := catalog.KeyTaken(ctx, s.db, disk, strings.Join(parts[:i+1], "/"))
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if taken {
			return true, nil
		}
	}
	return false, nil
}

// makeFolderOn makes one missing folder on the disk a copy goes to, as an operation of its own, with the mode and owner
// of the same folder in the trash it comes from (specs.md §17.4).
func (s *Session) makeFolderOn(ctx context.Context, to string, dst, src *ops.Disk, e *catalog.TrashEntry, folder string) error {
	op, err := catalog.BeginMkdir(ctx, s.db, to, folder, s.now)
	if err != nil {
		return catalogError{fmt.Errorf("catalog: %w", err)}
	}
	trashFolder := strings.TrimSuffix(e.TrashPath, e.Path) + folder
	like, err := src.TrashFolderInfo(trashFolder)
	if err != nil {
		if undo := catalog.Undo(ctx, s.db, op, false, s.now); undo != nil {
			return catalogError{fmt.Errorf("catalog: %w", undo)}
		}
		return err
	}
	st, err := dst.MakeRootFolder(folder, like)
	switch {
	case err == nil:
		if err := dst.FinishMkdir(ctx, s.db, op, &st); err != nil {
			return catalogError{err}
		}
		return nil
	case errors.Is(err, ops.ErrDiskLost):
		return err
	case errors.Is(err, ops.ErrPathTaken):
		if err := catalog.Undo(ctx, s.db, op, false, s.now); err != nil {
			return catalogError{fmt.Errorf("catalog: %w", err)}
		}
		return nil
	default:
		if undo := catalog.Undo(ctx, s.db, op, false, s.now); undo != nil {
			return catalogError{fmt.Errorf("catalog: %w", undo)}
		}
		return err
	}
}
