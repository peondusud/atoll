package trash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/ops"
)

// What became of a corrupt copy a repair reached (specs.md §16.1).
const (
	OutcomeRepaired = "repaired"
	OutcomeNoRoom   = "no-room"
)

// errHealthyChanged says that the healthy copy no longer held what its hashes say while a repair read it.
var errHealthyChanged = errors.New("the healthy copy changed while read")

// errHealthyLost says that the healthy copy's disk failed when a repair went to read from it: that disk is lost, and
// the repair stops on a disk that did not fail.
var errHealthyLost = errors.New("the healthy copy's disk failed")

// Healthy is the copy a repair reads from: another copy of the file, read whole and found to hold what its hashes
// say (specs.md §16.1).
type Healthy struct {
	Disk  string
	Known catalog.KnownCopy // where it sits, and what its row says
}

// chunkLength gives the length of chunk i of a file of size bytes.
func chunkLength(size, i int64) int64 {
	return min(integrity.ChunkSize, size-i*integrity.ChunkSize)
}

// RepairChunks repairs a corrupt copy in place from a healthy one, as one operation repair (specs.md §16.1): its bad
// chunks saved to the trash, then each written and read back, then its time set back. chunks holds the file's chunk
// hashes. unreadable holds those of its bad chunks that cannot be read, on a disk that confirms: nothing can be saved
// of them, and what is written over them is what gives the sector another place. It says what became of the copy,
// which stays corrupt unless repaired.
func (s *Session) RepairChunks(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, bad, unreadable []int64, healthy *Healthy, chunks []byte) (string, error) {
	place := c.Place(f)
	// A disk that may only be read is never written to, its corrupt copies included (specs.md §10).
	if !s.writable(c.Disk) {
		return outcomeUnavailable, nil
	}
	handle, err := s.open(ctx, c.Disk)
	if err != nil {
		return "", err
	}
	if handle == nil {
		return outcomeUnavailable, nil
	}
	// A copy left as it is, at a wildcard name on the type ntfs, is written to by nothing, as it goes to the trash by
	// nothing: it stays corrupt, its name refused (specs.md §15.8, §16.1).
	if handle.Leaves(place) {
		return OutcomeNameRefused, nil
	}
	if s.full[c.Disk] {
		return OutcomeNoRoom, nil
	}
	source, err := s.open(ctx, healthy.Disk)
	if err != nil {
		return "", err
	}
	if source == nil {
		return outcomeUnavailable, nil
	}
	known := &catalog.KnownCopy{Path: place, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}
	w, st, err := handle.OpenRepair(known)
	switch {
	case errors.Is(err, ops.ErrGone):
		return OutcomeChanged, nil
	case errors.Is(err, ops.ErrDiskLost):
		return OutcomeIOError, s.lose(ctx, c.Disk, err)
	case err != nil:
		slog.Warn("a corrupt copy cannot be opened to repair it", "disk", c.Disk, "path", place, "error", err)
		return OutcomeIOError, nil
	}
	defer func() { _ = w.Close() }()
	size := c.Size.Int64
	var saved int64
	for _, i := range bad {
		if !slices.Contains(unreadable, i) {
			saved += chunkLength(size, i)
		}
	}
	op, err := catalog.BeginRepair(ctx, s.db, &catalog.RepairStart{
		Disk: c.Disk, Path: place, FileKey: c.FileKey.String, Bad: bad, Size: saved, MtimeNS: c.MtimeNS.Int64,
		NowNS: s.now, ExpiresNS: ops.AddRetention(s.now, s.retention),
	})
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.machine.Checkpoint("journaled")
	found, err := s.saveChunks(handle, op, w, &st, bad, unreadable, size)
	if err != nil {
		return s.stopRepair(ctx, handle, op, err, false)
	}
	// Each look compares the copy with what the repair's own last change left of it, its row's to begin with: anyone
	// else's write since stops the repair, which leaves the copy as it is, the write a change (specs.md §16.1).
	own := st
	wrote, err := s.writeChunks(source, healthy, handle, w, place, &own, bad, found, size, chunks)
	if errors.Is(err, errSomeoneWrote) {
		return s.leaveRepair(ctx, handle, op)
	}
	if err != nil {
		// The healthy copy's disk failing is no failure of the disk repaired (invariant 4): that one is lost, and the
		// repair, which wrote nothing from it, is resolved at once on its own disk, which can still be looked at.
		if errors.As(err, new(sourceError)) {
			if lose := s.lose(ctx, healthy.Disk, err); lose != nil {
				return "", lose
			}
			err = errHealthyLost
		}
		// A chunk that cannot be read back is the disk's fault only when the disk no longer confirms: otherwise the
		// repair is not done, and the copy is left to whoever asked for it, the scrub sending it to the trash.
		if errors.Is(err, ops.ErrReadBack) {
			confirmed, cerr := s.Confirms(ctx, c.Disk, err)
			if cerr != nil {
				return "", cerr
			}
			if !confirmed {
				return OutcomeIOError, nil
			}
		}
		return s.stopRepair(ctx, handle, op, err, wrote)
	}
	// Right before the time is set back, which would give a write since the last one the row's time.
	if moved, err := s.lookAgain(handle, place, &own); err != nil || moved {
		if err != nil {
			return s.stopRepair(ctx, handle, op, err, true)
		}
		return s.leaveRepair(ctx, handle, op)
	}
	s.machine.Checkpoint("times-looked")
	after, err := handle.SetTimeBack(w, fsx.TimeOf(c.MtimeNS.Int64))
	if err != nil {
		return s.stopRepair(ctx, handle, op, err, true)
	}
	// Right before the commit, which records the change time the copy has: one a write since gave it would hide that
	// write behind the row, and the next scrub would call it corrupt.
	if moved, err := s.lookAgain(handle, place, &after); err != nil || moved {
		if err != nil {
			return s.stopRepair(ctx, handle, op, err, true)
		}
		return s.leaveRepair(ctx, handle, op)
	}
	s.machine.Checkpoint("commit-looked")
	if err := catalog.FinishRepair(ctx, s.db, op, ops.TimeOf(after.Ctime).Int64, s.now); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.changed = true
	return OutcomeRepaired, nil
}

// errSomeoneWrote says that a repair's look found someone else's write in the copy: its times are no longer what the
// repair's own last change left, or the chunk it is about to write no longer holds what it found there (specs.md
// §16.1).
var errSomeoneWrote = errors.New("someone else wrote to the copy")

// lookAgain says whether the copy at a place moved from what the repair's own last change left of it: another inode,
// size, modification time or change time. An error says that it could not be looked at.
func (s *Session) lookAgain(handle *ops.Disk, place string, own *fsx.Stat) (bool, error) {
	still, err := handle.SourceStill(place, own)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	return !still, nil
}

// leaveRepair ends a repair whose look found someone else's write in the copy, as its reprise would not: nothing more
// written, the time not set back, the row as it was, so that the write stays a change, which the copy's settling makes
// a conflict (specs.md §16.1). The copy stays corrupt meanwhile, and the outcome is changed.
func (s *Session) leaveRepair(ctx context.Context, handle *ops.Disk, op *ops.Op) (string, error) {
	slog.Warn("a repair stopped: someone else wrote to the copy", "disk", op.Disk, "op", op.ID, "path", op.Path)
	if err := handle.LeaveRepair(ctx, s.db, op, s.now); err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, op.Disk, err)
		}
		// The trash could not be read: the operation stays planned, for the reprise, which reads the copy whole.
		slog.Warn("an operation stays planned", "disk", op.Disk, "op", op.ID, "error", err)
		s.lost = true
		return OutcomeIOError, nil
	}
	s.changed = true
	return OutcomeChanged, nil
}

// saveChunks saves every bad chunk of a copy to the trash, then their folder, before any is written, and gives the
// hash of each it read, as it found it. A chunk that cannot be read has nothing to save: its place in the trash is an
// empty file, which says that the repair found it so, to whoever looks at the entry and to the reprise.
func (s *Session) saveChunks(handle *ops.Disk, op *ops.Op, w fsx.WritableFile, st *fsx.Stat, bad, unreadable []int64, size int64) (map[int64]*[integrity.HashSize]byte, error) {
	buf := make([]byte, integrity.ChunkSize)
	found := make(map[int64]*[integrity.HashSize]byte, len(bad))
	for _, i := range bad {
		data := buf[:chunkLength(size, i)]
		if slices.Contains(unreadable, i) {
			data = nil
		} else if n, err := w.ReadAt(data, i*integrity.ChunkSize); n < len(data) {
			return nil, fmt.Errorf("read chunk %d: %w", i, readError(err))
		} else {
			sum := integrity.ChunkSum(data)
			found[i] = &sum
		}
		if err := handle.SaveChunk(op, i, data, st); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	return found, handle.SyncChunks(op) //nolint:wrapcheck // the caller says which operation
}

// writeChunks writes each bad chunk in place from the healthy copy, each read there and checked against its hash
// first, then read back past the page cache. Right before each write, as reading the healthy copy can be long, from a
// disk that may have to spin up first, it looks at the copy again: its times still what own says, the repair's own last
// change, which each write moves on, and the chunk still what the repair found there, found by index, as a write in the
// second of the last change moves no time on a disk that holds a time to the second (specs.md §16.1); errSomeoneWrote
// says that either moved. It says whether it wrote to the copy at all, done or not.
func (s *Session) writeChunks(source *ops.Disk, healthy *Healthy, handle *ops.Disk, w fsx.WritableFile, place string, own *fsx.Stat, bad []int64,
	found map[int64]*[integrity.HashSize]byte, size int64, chunks []byte) (bool, error) {
	r, _, err := source.OpenSource(healthy.Known.Path, &healthy.Known)
	switch {
	case errors.Is(err, ops.ErrGone):
		return false, errHealthyChanged
	case errors.Is(err, ops.ErrDiskLost):
		// An error of the disk read from, which is not the one written to.
		return false, sourceError{err}
	case err != nil:
		// Nothing can be read from it, and nothing says its disk is gone: no healthy copy, as one that changed.
		slog.Warn("the healthy copy cannot be opened", "disk", healthy.Disk, "path", healthy.Known.Path, "error", err)
		return false, errHealthyChanged
	}
	defer func() { _ = r.Close() }()
	buf := make([]byte, integrity.ChunkSize)
	again := make([]byte, integrity.ChunkSize)
	wrote := false
	for _, i := range bad {
		data := buf[:chunkLength(size, i)]
		if n, err := r.ReadAt(data, i*integrity.ChunkSize); n < len(data) {
			if err != nil && !errors.Is(err, io.EOF) {
				slog.Warn("the healthy copy cannot be read", "disk", healthy.Disk, "path", healthy.Known.Path, "error", err)
			}
			return wrote, errHealthyChanged
		}
		want := chunks[i*integrity.HashSize : (i+1)*integrity.HashSize]
		if sum := integrity.ChunkSum(data); !bytes.Equal(sum[:], want) {
			return wrote, errHealthyChanged
		}
		if moved, err := s.lookAgain(handle, place, own); err != nil || moved {
			if err == nil {
				err = errSomeoneWrote
			}
			return wrote, err
		}
		if still, err := handle.ChunkStill(w, i, again[:len(data)], found[i]); err != nil || !still {
			if err == nil {
				err = errSomeoneWrote
			}
			return wrote, fmt.Errorf("%w", err)
		}
		s.machine.Checkpoint("chunk-looked")
		wrote = true
		after, err := handle.WriteChunk(w, i, data, want)
		if err != nil {
			return wrote, fmt.Errorf("%w", err)
		}
		*own = after
	}
	return wrote, nil
}

// readError gives what a short read says: an error, or the file ending sooner than its row.
func readError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// stopRepair resolves a repair that could not go on, as its reprise would, and says what to report: a disk that
// fails keeps its operation planned for the reprise (specs.md §16.1). wrote says that the repair wrote to the copy
// before it stopped, which a reprise could only work out from what the copy holds.
func (s *Session) stopRepair(ctx context.Context, handle *ops.Disk, op *ops.Op, cause error, wrote bool) (string, error) {
	slog.Warn("a repair stopped", "disk", op.Disk, "op", op.ID, "error", cause)
	if errors.Is(cause, ops.ErrDiskLost) {
		return OutcomeIOError, s.lose(ctx, op.Disk, cause)
	}
	outcome := OutcomeIOError
	switch {
	case fsx.IsNoRoom(cause):
		s.full[op.Disk] = true
		outcome = OutcomeNoRoom
	case errors.Is(cause, ops.ErrGone), errors.Is(cause, errHealthyChanged):
		outcome = OutcomeChanged
	}
	finished, err := handle.ResolveRepair(ctx, s.db, op, s.now, outcome, wrote)
	if err != nil {
		if errors.Is(err, ops.ErrDiskLost) {
			return OutcomeIOError, s.lose(ctx, op.Disk, err)
		}
		// A chunk that cannot be read is the copy's fault on a disk that confirms right after, and its disk's on one that
		// does not, which Confirms records (invariant 4, specs.md §16.1).
		if errors.Is(err, ops.ErrChunkUnreadable) {
			if confirmed, cerr := s.Confirms(ctx, op.Disk, err); cerr != nil || !confirmed {
				return OutcomeIOError, cerr
			}
		}
		slog.Warn("an operation stays planned", "disk", op.Disk, "op", op.ID, "error", err)
		s.lost = true
		return OutcomeIOError, nil
	}
	s.changed = true
	if finished {
		return OutcomeRepaired, nil
	}
	return outcome, nil
}
