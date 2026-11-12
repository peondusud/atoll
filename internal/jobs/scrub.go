package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/value"
)

// scrubPage is how many due copies the scrub reads from the catalog at a time.
const scrubPage = 100

// ScrubOptions says what a scrub may do (specs.md §16).
type ScrubOptions struct {
	// Disk limits the scrub to one disk, "" for all of them, one at a time in configuration order.
	Disk string
	// Under limits it to the files at that path and below it, compared by whole components; "" for all.
	Under string
	// Budget is how long it may read; 0 takes integrity.scrub_budget.
	Budget time.Duration
	// Apply records what it finds. Without it, it reads, and writes nothing.
	Apply bool
	// Quarantine moves a corrupt copy to the trash when another copy, read again first, is healthy.
	Quarantine bool
}

// ScrubDisk says what a scrub did on one disk. Each list holds paths in the order they were read.
type ScrubDisk struct {
	ID string
	// Scrubbed says that the disk was read; false when it could not be, or the budget ran out before it.
	Scrubbed bool
	// Read counts the copies read whole that hold what their hashes say, and Bytes what that read.
	Read  int64
	Bytes int64
	// Corrupt are the copies whose content no longer matches, read again past the cache, size and times unchanged.
	Corrupt []string
	// Quarantined are those of them another copy is healthy for: moved to the trash, or to be with Quarantine.
	Quarantined []string
	// Kept are those of them with no healthy copy elsewhere, left where they are, or that could not be moved.
	Kept []string
	// Changed are the copies that changed since their row, or while read: for the next pass to look at.
	Changed []string
	// Left says that copies due were left when the budget ran out.
	Left bool
}

// ScrubOutcome is what a scrub says.
type ScrubOutcome struct {
	Disks []ScrubDisk
	// Partial says that a disk could not be read, or failed while read.
	Partial bool
}

type scrubber struct {
	db       *sql.DB
	machine  disks.Machine
	session  *trash.Session
	opts     ScrubOptions
	now      int64
	due      int64 // a copy verified at or before it is due
	deadline int64
	buf      []byte
}

// errDiskFailed stops the scrub of a disk that failed while read.
var errDiskFailed = errors.New("the disk failed while read")

// Scrub reads the copies due, one disk at a time, and checks each chunk against its hash (specs.md §16): a copy
// that matches is verified; one that does not, read again past the cache, its size and times unchanged, is
// corrupt, and goes to the trash when another copy is healthy. A scrub stopped by its budget resumes at the chunk it
// reached.
func Scrub(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, opts ScrubOptions) (*ScrubOutcome, error) {
	effective := c.Effective()
	everyText, _ := effective["integrity.scrub_every"].(string)
	every, err := value.ParseDuration(everyText)
	if err != nil {
		return nil, fmt.Errorf("integrity.scrub_every: %w", err)
	}
	if opts.Budget <= 0 {
		budgetText, _ := effective["integrity.scrub_budget"].(string)
		if opts.Budget, err = value.ParseDuration(budgetText); err != nil {
			return nil, fmt.Errorf("integrity.scrub_budget: %w", err)
		}
	}
	session, err := trash.Open(ctx, db, c, m, opts.Apply)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	now := m.Now()
	s := &scrubber{db: db, machine: m, session: session, opts: opts, now: now, due: now - every.Nanoseconds(),
		deadline: now + opts.Budget.Nanoseconds(), buf: make([]byte, integrity.ChunkSize)}
	out := &ScrubOutcome{Disks: []ScrubDisk{}}
	spent := false
	for i := range c.Disks {
		d := &c.Disks[i]
		if opts.Disk != "" && d.ID != opts.Disk || d.State == config.DiskFailed {
			continue
		}
		sd := ScrubDisk{ID: d.ID}
		if !spent {
			if spent, err = s.scrubDisk(ctx, d, &sd); err != nil {
				if !errors.Is(err, errDiskFailed) {
					return nil, err
				}
				out.Partial = true
			}
		}
		if !sd.Scrubbed && !spent {
			out.Partial = true
		}
		out.Disks = append(out.Disks, sd)
	}
	if opts.Apply {
		if err := session.RemoveEmptied(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		out.Partial = out.Partial || session.Lost()
		if err := session.Settle(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	return out, nil
}

// scrubDisk reads the copies due on a disk: first the one the last scrub stopped in, then the others, those never
// verified first. It says whether the budget ran out.
func (s *scrubber) scrubDisk(ctx context.Context, d *config.Disk, sd *ScrubDisk) (bool, error) {
	if !s.session.Readable(d.ID) {
		return false, nil
	}
	handle, err := s.session.Handle(ctx, d.ID)
	if err != nil || handle == nil {
		return false, err
	}
	sd.Scrubbed = true
	cursor, resume, err := catalog.ReadScrubCursor(ctx, s.db, d.ID)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if resume {
		cp, found, err := catalog.ScrubCopyOf(ctx, s.db, d.ID, cursor.FileID)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		start := int64(0)
		if found && cp.Size == cursor.Size && cp.MtimeNS == cursor.MtimeNS && cp.CtimeNS == cursor.CtimeNS {
			start = cursor.Chunk
		}
		if found && underPath(cp.Path, s.opts.Under) {
			if spent, err := s.scrubCopy(ctx, d, handle, &cp, start, sd); spent || err != nil {
				sd.Left = spent
				return spent, err
			}
		}
		if s.opts.Apply && (!found || underPath(cp.Path, s.opts.Under)) {
			if err := catalog.ClearScrubCursor(ctx, s.db, d.ID); err != nil {
				return false, fmt.Errorf("catalog: %w", err)
			}
		}
	}
	after := catalog.ScrubKey{VerifiedNS: -2}
	for {
		page, err := catalog.ScrubDue(ctx, s.db, d.ID, s.due, after, scrubPage, s.opts.Under)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return false, nil
		}
		for i := range page {
			cp := &page[i]
			after = catalog.ScrubKey{VerifiedNS: -1, FileID: cp.FileID}
			if cp.VerifiedNS.Valid {
				after.VerifiedNS = cp.VerifiedNS.Int64
			}
			if resume && cp.FileID == cursor.FileID {
				continue
			}
			if spent, err := s.scrubCopy(ctx, d, handle, cp, 0, sd); spent || err != nil {
				sd.Left = spent
				return spent, err
			}
		}
	}
}

// scrubCopy reads a copy from a chunk on, and says whether the budget ran out before its end.
func (s *scrubber) scrubCopy(ctx context.Context, d *config.Disk, handle *ops.Disk, cp *catalog.ScrubCopy, start int64, sd *ScrubDisk) (bool, error) {
	n := chunks(cp.Size)
	if int64(len(cp.Chunks)) != n*integrity.HashSize {
		// No chunk hashes yet: reading the file's hash is not the scrub's.
		return false, nil
	}
	known := &catalog.KnownCopy{Path: cp.Path, FileID: cp.FileID, Size: sql.NullInt64{Int64: cp.Size, Valid: true},
		MtimeNS: sql.NullInt64{Int64: cp.MtimeNS, Valid: true}, CtimeNS: sql.NullInt64{Int64: cp.CtimeNS, Valid: true}, FileKey: cp.FileKey}
	f, st, err := handle.OpenSource(cp.Path, known)
	switch {
	case errors.Is(err, ops.ErrGone):
		sd.Changed = append(sd.Changed, cp.Path)
		return false, nil
	case err != nil:
		return false, s.lost(ctx, d.ID, err)
	}
	defer func() { _ = f.Close() }()
	for i := start; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("scrub: %w", err)
		}
		if s.machine.Now() >= s.deadline {
			if s.opts.Apply {
				if err := catalog.WriteScrubCursor(ctx, s.db, &catalog.ScrubCursor{Disk: d.ID, FileID: cp.FileID, Chunk: i,
					Size: cp.Size, MtimeNS: cp.MtimeNS, CtimeNS: cp.CtimeNS}); err != nil {
					return false, fmt.Errorf("catalog: %w", err)
				}
			}
			return true, nil
		}
		want := cp.Chunks[i*integrity.HashSize : (i+1)*integrity.HashSize]
		match, err := s.readChunk(f, cp.Size, i, want)
		if err != nil {
			return false, s.lost(ctx, d.ID, err)
		}
		if !match {
			// A chunk that does not match is read again past the cache, the copy's name and times looked at first: a
			// copy changed is a modification, for the next pass (specs.md §16).
			still, err := handle.SourceStill(cp.Path, &st)
			if err != nil {
				return false, s.lost(ctx, d.ID, err)
			}
			if !still {
				sd.Changed = append(sd.Changed, cp.Path)
				return false, nil
			}
			if err := f.DropCache(); err != nil {
				return false, s.lost(ctx, d.ID, err)
			}
			if match, err = s.readChunk(f, cp.Size, i, want); err != nil {
				return false, s.lost(ctx, d.ID, err)
			}
			if !match {
				return false, s.corrupt(ctx, d, cp, sd)
			}
			slog.Warn("a chunk read wrong once, then right past the cache", "disk", d.ID, "path", cp.Path, "chunk", i)
		}
		sd.Bytes += min(integrity.ChunkSize, cp.Size-i*integrity.ChunkSize)
	}
	still, err := handle.SourceStill(cp.Path, &st)
	if err != nil {
		return false, s.lost(ctx, d.ID, err)
	}
	if !still {
		sd.Changed = append(sd.Changed, cp.Path)
		return false, nil
	}
	sd.Read++
	if s.opts.Apply {
		if err := catalog.SetCopyVerified(ctx, s.db, cp.FileID, d.ID, s.machine.Now()); err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
	}
	return false, nil
}

// readChunk reads chunk i of a file of size bytes, and says whether its hash is want.
func (s *scrubber) readChunk(f fsx.File, size, i int64, want []byte) (bool, error) {
	length := min(integrity.ChunkSize, size-i*integrity.ChunkSize)
	n, err := f.ReadAt(s.buf[:length], i*integrity.ChunkSize)
	if int64(n) < length {
		if err == nil || errors.Is(err, io.EOF) {
			// Shorter than its row: a change the look at its name tells.
			return false, nil
		}
		return false, err //nolint:wrapcheck // the caller says which disk
	}
	sum := integrity.ChunkSum(s.buf[:length])
	return bytes.Equal(sum[:], want), nil
}

// corrupt records a corrupt copy, and sends it to the trash when another copy is healthy (specs.md §16): the file is
// then degraded, and replication makes the copy again. With none, it stays, and alerts say so.
func (s *scrubber) corrupt(ctx context.Context, d *config.Disk, cp *catalog.ScrubCopy, sd *ScrubDisk) error {
	sd.Corrupt = append(sd.Corrupt, cp.Path)
	slog.Warn("a corrupt copy", "disk", d.ID, "path", cp.Path)
	if s.opts.Apply {
		if err := catalog.MarkCopyCorrupt(ctx, s.db, cp.FileID, d.ID, cp.Path, s.now); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	f, err := catalog.FileByID(ctx, s.db, cp.FileID)
	if err != nil || f == nil {
		return fmt.Errorf("catalog: %w", err)
	}
	healthy, err := s.healthyCopy(ctx, f, d.ID, cp.Chunks)
	if err != nil {
		return err
	}
	if !healthy {
		sd.Kept = append(sd.Kept, cp.Path)
		return nil
	}
	if !s.opts.Apply || !s.opts.Quarantine {
		sd.Quarantined = append(sd.Quarantined, cp.Path)
		return nil
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Disk != d.ID {
			continue
		}
		outcome, err := s.session.Send(ctx, f, c, catalog.ReasonQuarantined)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if outcome == trash.OutcomeTrashed {
			sd.Quarantined = append(sd.Quarantined, cp.Path)
		} else {
			slog.Warn("a corrupt copy stays where it is", "disk", d.ID, "path", cp.Path, "outcome", outcome)
			sd.Kept = append(sd.Kept, cp.Path)
		}
	}
	return nil
}

// healthyCopy says whether another copy of the file, ok on a disk that can be read, holds what its hashes say,
// read whole now: a copy is quarantined only when one does (specs.md §16).
func (s *scrubber) healthyCopy(ctx context.Context, f *catalog.FileRow, notOn string, want []byte) (bool, error) {
	n := chunks(f.Size)
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Disk == notOn || c.State != string(solver.CopyOK) || !s.session.Readable(c.Disk) {
			continue
		}
		handle, err := s.session.Handle(ctx, c.Disk)
		if err != nil {
			return false, fmt.Errorf("%w", err)
		}
		if handle == nil {
			continue
		}
		place := c.Place(f)
		file, st, err := handle.OpenSource(place, &catalog.KnownCopy{Path: place, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey})
		if err != nil {
			slog.Warn("a copy cannot be read to check it", "disk", c.Disk, "path", place, "error", err)
			continue
		}
		good := true
		for i := int64(0); i < n && good; i++ {
			match, err := s.readChunk(file, f.Size, i, want[i*integrity.HashSize:(i+1)*integrity.HashSize])
			good = err == nil && match
		}
		_ = file.Close()
		if !good {
			continue
		}
		if still, err := handle.SourceStill(place, &st); err == nil && still {
			return true, nil
		}
	}
	return false, nil
}

// lost says that a disk failed while read: the scrub of it stops there.
func (s *scrubber) lost(ctx context.Context, id string, cause error) error {
	if err := s.session.Lose(ctx, id, cause); err != nil {
		return fmt.Errorf("%w", err)
	}
	return fmt.Errorf("%w: %w", errDiskFailed, cause)
}

// underPath says whether a path is under, or below it by whole components; any is under "".
func underPath(p, under string) bool {
	return under == "" || p == under || strings.HasPrefix(p, under+"/")
}

// chunks gives how many chunks a file of size bytes has: none when it is empty (specs.md §16).
func chunks(size int64) int64 {
	return (size + integrity.ChunkSize - 1) / integrity.ChunkSize
}
