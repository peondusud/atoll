package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
)

// The ways a corrupt copy is repaired (specs.md §16.1).
const (
	RepairChunk = "chunk"
	RepairFile  = "file"
)

// What became of a corrupt copy, besides the trash's outcomes (specs.md §16.1).
const (
	OutcomeToRepair      = "to-repair"
	OutcomeNoHealthyCopy = "no-healthy-copy"
	// OutcomeVerified is a copy found corrupt that reads right after all, read whole now.
	OutcomeVerified = "verified"
)

// RepairOptions says what atoll repair may do (specs.md §16.1, §25).
type RepairOptions struct {
	// Under keeps the files at that path and below it, by whole components.
	Under string
	// Disk keeps one disk, "" for all of them.
	Disk string
	// Mode is chunk or file, "" for integrity.repair_mode.
	Mode string
	// Apply repairs. Without it, the copies are read, and what a repair would do is said.
	Apply bool
	// Record records what is found without Apply, as Apply does: a copy that reads right after all is verified.
	Record bool
}

// RepairItem is what became of one corrupt copy.
type RepairItem struct {
	Disk    string
	Path    string
	Mode    string
	Chunks  int // the bad chunks found
	Outcome string
}

// RepairSummary is what atoll repair says.
type RepairSummary struct {
	Applied bool
	Items   []RepairItem
	// Partial says that a disk could not be relied on.
	Partial bool
}

// Repair repairs the corrupt copies of the files at a path, each from a healthy copy, by chunk or whole, and says
// what became of each (specs.md §16.1).
func Repair(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, opts RepairOptions) (*RepairSummary, error) {
	mode := opts.Mode
	if mode == "" {
		mode, _ = c.Effective()["integrity.repair_mode"].(string)
	}
	session, err := trash.Open(ctx, db, c, m, opts.Apply)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	list, err := catalog.CorruptCopies(ctx, db, opts.Under, opts.Disk)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	sum := &RepairSummary{Applied: opts.Apply, Items: []RepairItem{}}
	var remake []string
	for i := range list {
		item, err := repairOne(ctx, db, session, &list[i], mode, &opts, m.Now())
		if err != nil {
			return nil, err
		}
		if item == nil {
			continue
		}
		if item.Outcome == trash.OutcomeRepaired && item.Mode == RepairFile {
			remake = append(remake, item.Path)
		}
		sum.Items = append(sum.Items, *item)
	}
	if opts.Apply {
		if err := session.RemoveEmptied(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		if err := session.Settle(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	sum.Partial = session.Lost()
	session.Close()
	// A copy repaired whole is made again at once, as atoll replicate --now makes it; one that cannot be is made by
	// the next pass (specs.md §16.1).
	for _, path := range remake {
		if _, err := Replicate(ctx, db, c, m, ReplicateOptions{Path: path, Now: true}, func(*ReplicateItem) error { return nil }); err != nil {
			slog.Warn("a copy repaired whole is not made again yet", "path", path, "error", err)
		}
	}
	return sum, nil
}

// repairOne repairs one corrupt copy, or says what a repair would do. It gives nil for a copy no longer corrupt.
func repairOne(ctx context.Context, db *sql.DB, session *trash.Session, cc *catalog.CorruptCopy, mode string, opts *RepairOptions, nowNS int64) (*RepairItem, error) {
	f, err := catalog.FileByID(ctx, db, cc.FileID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil, nil
	}
	at := -1
	for i := range f.Copies {
		if f.Copies[i].Disk == cc.Disk && f.Copies[i].State == string(solver.CopyCorrupt) {
			at = i
		}
	}
	if at < 0 {
		return nil, nil
	}
	c := &f.Copies[at]
	item := &RepairItem{Disk: c.Disk, Path: c.Place(f), Mode: modeFor(c, mode)}
	if !session.Readable(c.Disk) {
		item.Outcome = trash.OutcomeDiskUnavailable
		return item, nil
	}
	chunks, err := catalog.FileChunks(ctx, db, f.ID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	healthy, err := findHealthy(ctx, session, f, c.Disk, chunks)
	if err != nil {
		return nil, err
	}
	if healthy == nil {
		item.Outcome = OutcomeNoHealthyCopy
		return item, nil
	}
	outcome, bad, err := repairCopy(ctx, db, session, f, c, healthy, chunks, item.Mode, opts.Apply, opts.Apply || opts.Record, nowNS)
	item.Chunks, item.Outcome = bad, outcome
	return item, err
}

// modeFor gives the way a copy is repaired: a copy with more than one link always by chunk, so that every name of
// it stays the same file (specs.md §16.1).
func modeFor(c *catalog.CopyRow, mode string) string {
	if c.Nlink.Valid && c.Nlink.Int64 > 1 {
		return RepairChunk
	}
	return mode
}

// repairCopy reads a corrupt copy whole, and repairs it from a healthy one when apply says so: by chunk, or whole,
// sent to the trash for replication to make again. It gives the outcome and the bad chunks found.
func repairCopy(ctx context.Context, db *sql.DB, session *trash.Session, f *catalog.FileRow, c *catalog.CopyRow, healthy *trash.Healthy, chunks []byte, mode string, apply, record bool, nowNS int64) (string, int, error) {
	handle, err := session.Handle(ctx, c.Disk)
	if err != nil {
		return "", 0, fmt.Errorf("%w", err)
	}
	if handle == nil {
		return trash.OutcomeDiskUnavailable, 0, nil
	}
	place := c.Place(f)
	// A chunk that cannot be read is a bad chunk, when the disk still confirms, which is asked once (specs.md §16.1).
	var unreadable []int64
	confirmed := false
	bad, err := badChunks(handle, &catalog.KnownCopy{Path: place, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}, chunks,
		func(chunk int64, cause error) (bool, error) {
			if !confirmed {
				ok, err := session.Confirms(ctx, c.Disk, cause)
				if err != nil {
					return false, fmt.Errorf("%w", err)
				}
				if !ok {
					return false, fmt.Errorf("%w: %w", ops.ErrDiskLost, cause)
				}
				confirmed = true
			}
			slog.Warn("a chunk of a corrupt copy cannot be read, on a disk that confirms", "disk", c.Disk, "path", place, "chunk", chunk, "error", cause)
			unreadable = append(unreadable, chunk)
			return true, nil
		})
	switch {
	case errors.Is(err, ops.ErrGone):
		return trash.OutcomeChanged, 0, nil
	case errors.Is(err, ops.ErrDiskLost):
		return trash.OutcomeIOError, 0, session.Lose(ctx, c.Disk, err) //nolint:wrapcheck // the session's own
	case err != nil:
		slog.Warn("a corrupt copy cannot be read", "disk", c.Disk, "path", place, "error", err)
		return trash.OutcomeIOError, 0, nil
	}
	if len(bad) == 0 {
		if record {
			if err := catalog.ClearCorrupt(ctx, db, f.ID, c.Disk, nowNS); err != nil {
				return "", 0, fmt.Errorf("catalog: %w", err)
			}
			session.MarkChanged()
		}
		return OutcomeVerified, 0, nil
	}
	if !apply {
		return OutcomeToRepair, len(bad), nil
	}
	if mode == RepairFile {
		outcome, err := session.Send(ctx, f, c, catalog.ReasonRepaired)
		if outcome == trash.OutcomeTrashed {
			outcome = trash.OutcomeRepaired
		}
		return outcome, len(bad), err //nolint:wrapcheck // the session's own
	}
	outcome, err := session.RepairChunks(ctx, f, c, bad, unreadable, healthy, chunks)
	return outcome, len(bad), err //nolint:wrapcheck // the session's own
}

// badChunks reads a copy whole, through its disk's confirmed handle, and gives the chunks whose hash is not the
// file's. ops.ErrGone means that its name no longer holds what its row says, before or after the read. A chunk that
// cannot be read is an error, unless unreadable, when given, says that it is a bad chunk like another: it is asked for
// each, with the error of the read.
func badChunks(handle *ops.Disk, known *catalog.KnownCopy, chunks []byte, unreadable func(chunk int64, cause error) (bool, error)) ([]int64, error) {
	f, st, err := handle.OpenSource(known.Path, known)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { _ = f.Close() }()
	size := known.Size.Int64
	n := (size + integrity.ChunkSize - 1) / integrity.ChunkSize
	if int64(len(chunks)) != n*integrity.HashSize {
		return nil, fmt.Errorf("%q: %d chunk hashes for %d bytes: %w", known.Path, len(chunks)/integrity.HashSize, size, ops.ErrGone)
	}
	buf := make([]byte, integrity.ChunkSize)
	var bad []int64
	for i := range n {
		length := min(integrity.ChunkSize, size-i*integrity.ChunkSize)
		if read, err := f.ReadAt(buf[:length], i*integrity.ChunkSize); int64(read) < length {
			if err == nil || errors.Is(err, io.EOF) {
				return nil, ops.ErrGone
			}
			if unreadable == nil {
				return nil, fmt.Errorf("%w", err)
			}
			isBad, askErr := unreadable(i, err)
			if askErr != nil || !isBad {
				return nil, errors.Join(askErr, fmt.Errorf("chunk %d: %w", i, err))
			}
			bad = append(bad, i)
			continue
		}
		sum := integrity.ChunkSum(buf[:length])
		if !bytes.Equal(sum[:], chunks[i*integrity.HashSize:(i+1)*integrity.HashSize]) {
			bad = append(bad, i)
		}
	}
	still, err := handle.SourceStill(known.Path, &st)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	if !still {
		return nil, ops.ErrGone
	}
	return bad, nil
}

// findHealthy gives another copy of the file, ok on a disk that can be read, that holds what its hashes say, read
// whole now: the first by disk position, and nil when there is none (specs.md §16).
func findHealthy(ctx context.Context, session *trash.Session, f *catalog.FileRow, notOn string, chunks []byte) (*trash.Healthy, error) {
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Disk == notOn || c.State != string(solver.CopyOK) || !session.Readable(c.Disk) {
			continue
		}
		handle, err := session.Handle(ctx, c.Disk)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		if handle == nil {
			continue
		}
		place := c.Place(f)
		known := catalog.KnownCopy{Path: place, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}
		// A copy that cannot be read whole is no healthy copy, whatever its disk says.
		bad, err := badChunks(handle, &known, chunks, nil)
		if err != nil {
			slog.Warn("a copy cannot be read to check it", "disk", c.Disk, "path", place, "error", err)
			continue
		}
		if len(bad) == 0 {
			return &trash.Healthy{Disk: c.Disk, Known: known}, nil
		}
	}
	return nil, nil
}
