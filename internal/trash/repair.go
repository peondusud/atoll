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
	if err := s.saveChunks(handle, op, w, &st, bad, unreadable, size); err != nil {
		return s.stopRepair(ctx, handle, op, err, false)
	}
	// Right before the first write, as saving can be long: an edit in place is never overwritten.
	if still, err := handle.SourceStill(place, &st); err != nil || !still {
		if err == nil {
			err = ops.ErrGone
		}
		return s.stopRepair(ctx, handle, op, err, false)
	}
	wrote, err := s.writeChunks(source, healthy, handle, w, bad, size, chunks)
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
	after, err := handle.SetTimeBack(w, fsx.TimeOf(c.MtimeNS.Int64))
	if err != nil {
		return s.stopRepair(ctx, handle, op, err, true)
	}
	if err := catalog.FinishRepair(ctx, s.db, op, ops.TimeOf(after.Ctime).Int64, s.now); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	s.changed = true
	return OutcomeRepaired, nil
}

// saveChunks saves every bad chunk of a copy to the trash, then their folder, before any is written. A chunk that
// cannot be read has nothing to save: its place in the trash is an empty file, which says that the repair found it so,
// to whoever looks at the entry and to the reprise.
func (s *Session) saveChunks(handle *ops.Disk, op *ops.Op, w fsx.WritableFile, st *fsx.Stat, bad, unreadable []int64, size int64) error {
	buf := make([]byte, integrity.ChunkSize)
	for _, i := range bad {
		data := buf[:chunkLength(size, i)]
		if slices.Contains(unreadable, i) {
			data = nil
		} else if n, err := w.ReadAt(data, i*integrity.ChunkSize); n < len(data) {
			return fmt.Errorf("read chunk %d: %w", i, readError(err))
		}
		if err := handle.SaveChunk(op, i, data, st); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return handle.SyncChunks(op) //nolint:wrapcheck // the caller says which operation
}

// writeChunks writes each bad chunk in place from the healthy copy, each read there and checked against its hash
// first, then read back past the page cache. It says whether it wrote to the copy at all, done or not.
func (s *Session) writeChunks(source *ops.Disk, healthy *Healthy, handle *ops.Disk, w fsx.WritableFile, bad []int64, size int64, chunks []byte) (bool, error) {
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
		wrote = true
		if err := handle.WriteChunk(w, i, data, want); err != nil {
			return wrote, fmt.Errorf("%w", err)
		}
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
