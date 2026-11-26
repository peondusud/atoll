package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
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
	// Quarantine repairs a corrupt copy from another copy, read again first and healthy, with integrity.auto_repair,
	// or moves it to the trash (specs.md §16, §16.1).
	Quarantine bool
	// Taken leaves out the corrupt copies left in the root that an earlier slice of the same stretch of
	// integrity.window took again already: the daemon takes each once a stretch (specs.md §15.13, §16). Nil leaves out
	// none, as atoll scrub does.
	Taken map[catalog.CorruptCopy]bool
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
	// Again are the corrupt copies an earlier scrub found and left in the root, taken first by this one, as it takes
	// one it finds corrupt (specs.md §16): each is then in Repaired, Quarantined, Kept or Conflict, never in Corrupt,
	// which says what this scrub found.
	Again []string
	// Repaired are those of them, and of Again, another copy is healthy for, with integrity.auto_repair: repaired, or
	// to be with Quarantine.
	Repaired []string
	// Quarantined are those of them, and of Again, another copy is healthy for: moved to the trash, or to be with
	// Quarantine.
	Quarantined []string
	// Kept are those of them, and of Again, with no healthy copy elsewhere, left where they are, or that could not be
	// moved.
	Kept []string
	// Changed are the copies that changed since their row, or while read: for the next pass to look at.
	Changed []string
	// Conflict are the copies whose content is not their file's, read again past the cache, size and times unchanged,
	// that were never verified, or sit on a disk whose times cannot tell an edit from rot: another version of the file,
	// which becomes a conflict. Nothing is written to them.
	Conflict []string
	// Refused are the copies that could not be opened, for a reason that says nothing of their disk: neither
	// verified nor corrupt, they stay due (specs.md §16).
	Refused []string
	// Left says that copies due were left when the budget ran out.
	Left bool
}

// ScrubOutcome is what a scrub says.
type ScrubOutcome struct {
	Disks []ScrubDisk
	// Partial says that a disk could not be read, or failed while read.
	Partial bool
	// Taken are the corrupt copies of each Again, for the daemon to leave out of the rest of the stretch.
	Taken []catalog.CorruptCopy
}

type scrubber struct {
	db      *sql.DB
	machine disks.Machine
	session *trash.Session
	opts    ScrubOptions
	// repair says integrity.auto_repair, and mode integrity.repair_mode (specs.md §16.1).
	repair bool
	mode   string
	// remake holds the files a copy of which was repaired whole: replication makes it again once the scrub is over.
	remake []string
	// run is what the repair of a copy takes from this scrub (specs.md §16.1).
	run *repairRun
	// changed says that this scrub found a copy corrupt, or in conflict: a state changed.
	changed bool
	// taken are the corrupt copies left in the root that this scrub took again.
	taken    []catalog.CorruptCopy
	now      int64
	due      int64 // a copy verified at or before it is due
	deadline int64
	buf      []byte
}

// errDiskFailed stops the scrub of a disk that failed while read.
var errDiskFailed = errors.New("the disk failed while read")

// Scrub reads the copies due, one disk at a time, and checks each chunk against its hash (specs.md §16): a copy
// that matches is verified; one that does not, read again past the cache, its size and times unchanged, is
// corrupt, and goes to the trash when another copy is healthy, or its file a conflict when the copy was never verified
// or its disk's times cannot tell an edit from rot. A scrub stopped by its budget resumes at the chunk it
// reached. On each disk, the corrupt copies an earlier scrub left in the root come first, taken again as one found now
// is: mergerfs may present them (invariant 14).
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
	// Without Quarantine it writes to no disk, and so resolves nothing of what a crash left (specs.md §17.2): it only
	// has its disks confirmed, to read through them.
	var session *trash.Session
	if opts.Apply && !opts.Quarantine {
		session, err = trash.OpenToRead(ctx, db, c, m)
	} else {
		session, err = trash.Open(ctx, db, c, m, opts.Apply)
	}
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	now := m.Now()
	s := &scrubber{db: db, machine: m, session: session, opts: opts, now: now, due: now - every.Nanoseconds(),
		deadline: now + opts.Budget.Nanoseconds(), buf: make([]byte, integrity.ChunkSize)}
	s.repair, _ = effective["integrity.auto_repair"].(bool)
	s.mode, _ = effective["integrity.repair_mode"].(string)
	// A repair writes with Quarantine alone, and what a look at a corrupt copy that changed finds is recorded unless
	// the scrub records nothing (specs.md §16.1).
	s.run = &repairRun{db: db, config: c, machine: m, session: session, apply: opts.Apply && opts.Quarantine, record: opts.Apply}
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
		if !sd.Scrubbed && !spent || len(sd.Refused) > 0 {
			out.Partial = true
		}
		out.Disks = append(out.Disks, sd)
	}
	if opts.Apply {
		if err := session.RemoveEmptied(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		out.Partial = out.Partial || session.Lost()
		// A scrub that only verified copies changed no state. The plan of the whole pool is for one that found a copy
		// corrupt or in conflict, repaired or moved one, or lost a disk: the daemon scrubs by slices of 30 s, and each
		// ended by that plan, a night of them for nothing (specs.md §16).
		if s.changed || session.Changed() || session.Lost() {
			if err := session.Settle(ctx); err != nil {
				return nil, fmt.Errorf("%w", err)
			}
		}
	}
	session.Close()
	// A copy repaired whole is made again at once; one that cannot be is made by the next pass (specs.md §16.1).
	for _, path := range s.remake {
		if _, err := Replicate(ctx, db, c, m, ReplicateOptions{Path: path, Now: true}, func(*ReplicateItem) error { return nil }); err != nil {
			slog.Warn("a copy repaired whole is not made again yet", "path", path, "error", err)
		}
	}
	out.Taken = s.taken
	return out, nil
}

// scrubDisk reads the copies due on a disk: first the one the last scrub stopped in, then the others, those never
// verified first. Before them it takes again the corrupt copies an earlier scrub left in the root. It says whether
// the budget ran out.
func (s *scrubber) scrubDisk(ctx context.Context, d *config.Disk, sd *ScrubDisk) (bool, error) {
	if !s.session.Readable(d.ID) {
		return false, nil
	}
	left, err := s.leftInRoot(ctx, d.ID)
	if err != nil {
		return false, err
	}
	cursor, resume, err := catalog.ReadScrubCursor(ctx, s.db, d.ID)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if !resume && len(left) == 0 {
		// The catalog first: a disk with nothing due is not opened, which would confirm it, a read of its head past
		// the cache that wakes it (specs.md §10, §16). The daemon scrubs by slices all night, each from the first disk on.
		first, err := catalog.ScrubDue(ctx, s.db, d.ID, s.due, catalog.ScrubKey{VerifiedNS: -2}, 1, s.opts.Under)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if len(first) == 0 {
			sd.Scrubbed = true
			return false, nil
		}
	}
	handle, err := s.session.Handle(ctx, d.ID)
	if err != nil || handle == nil {
		return false, err
	}
	sd.Scrubbed = true
	// The budget counts them as it counts the copies due: one that runs out leaves the others to the next scrub, and
	// the daemon's next slice leaves out those it took (specs.md §15.13).
	for i := range left {
		if s.machine.Now() >= s.deadline {
			sd.Left = true
			return true, nil
		}
		if err := s.takeAgain(ctx, d, &left[i], sd); err != nil {
			return false, err
		}
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
	case err == nil:
	case errors.Is(err, ops.ErrGone):
		sd.Changed = append(sd.Changed, cp.Path)
		return false, nil
	case errors.Is(err, ops.ErrDiskLost):
		return false, s.lost(ctx, d.ID, err)
	case fsx.IsOutOfFiles(err):
		// Nothing else opens either: the scrub stops, and no disk is the worse for it.
		return false, fmt.Errorf("scrub: %q on %s: %w", cp.Path, d.ID, err)
	default:
		// It refuses the account that asks, or its key is not loaded: that says nothing of its disk, whose other
		// copies are read (specs.md §15.10, §16). Taken for the disk's, it lost the disk at each slice, at this copy.
		slog.Warn("a copy cannot be opened to verify it", "disk", d.ID, "path", cp.Path, "error", err)
		sd.Refused = append(sd.Refused, cp.Path)
		return false, nil
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
			// A chunk that cannot be read is one that does not match, when its disk still confirms.
			if err := s.unreadable(ctx, d, cp.Path, i, err); err != nil {
				return false, err
			}
			match = false
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
				if err := s.unreadable(ctx, d, cp.Path, i, err); err != nil {
					return false, err
				}
				match = false
			}
			if !match {
				// A copy never verified, or on a disk whose times cannot tell an edit made in the second of the copy's last
				// change from none, holds another version for all anyone knows: a conflict, nothing written (specs.md §16).
				if integrity.ScrubMismatch(cp.VerifiedNS.Valid, s.session.TimePrecision(d.ID)) == integrity.MismatchConflict {
					return false, s.conflict(ctx, d, cp, sd)
				}
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

// corrupt records a corrupt copy, and when another copy is healthy repairs it from that one, with auto_repair, or
// sends it to the trash (specs.md §16, §16.1): the file is then degraded, and replication makes the copy again. With
// none, it stays, and alerts say so.
// conflict records that a copy does not hold its file's content where nothing tells rot from another version: a copy
// never verified, as nothing says it ever held it, or one on a disk that holds a time to a second or coarser, where an
// edit in place made in the second of the copy's last change leaves the times its row has (specs.md §10, §16). The
// file becomes a conflict, with every version kept (specs.md §15.7, §16): the copy is neither repaired nor moved, as a
// corrupt one would be, which would lose the version it holds.
func (s *scrubber) conflict(ctx context.Context, d *config.Disk, cp *catalog.ScrubCopy, sd *ScrubDisk) error {
	sd.Conflict = append(sd.Conflict, cp.Path)
	slog.Warn("a copy holds another content than its file's, which nothing tells from rot: a conflict", "disk", d.ID, "path", cp.Path,
		"verified", cp.VerifiedNS.Valid, "time_precision_ns", s.session.TimePrecision(d.ID))
	s.changed = true
	if s.opts.Apply {
		if err := catalog.WriteHashes(ctx, s.db, s.now, &catalog.HashWrite{FileID: cp.FileID, Conflict: true}); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	return nil
}

func (s *scrubber) corrupt(ctx context.Context, d *config.Disk, cp *catalog.ScrubCopy, sd *ScrubDisk) error {
	sd.Corrupt = append(sd.Corrupt, cp.Path)
	slog.Warn("a corrupt copy", "disk", d.ID, "path", cp.Path)
	s.changed = true
	if s.opts.Apply {
		if err := catalog.MarkCopyCorrupt(ctx, s.db, cp.FileID, d.ID, cp.Path, s.now); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	f, err := catalog.FileByID(ctx, s.db, cp.FileID)
	if err != nil || f == nil {
		return fmt.Errorf("catalog: %w", err)
	}
	healthy, err := findHealthy(ctx, s.session, f, d.ID, cp.Chunks)
	if err != nil {
		return err
	}
	// With no healthy copy, or on a disk that may only be read, it stays where it is: nothing is written to such a
	// disk, and without --yes nothing is said to be repaired or moved there either (specs.md §10).
	if healthy == nil || !s.session.Writable(d.ID) {
		sd.Kept = append(sd.Kept, cp.Path)
		return nil
	}
	if !s.opts.Apply || !s.opts.Quarantine {
		s.toMend(cp.Path, sd)
		return nil
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == d.ID })
	if at < 0 {
		return nil
	}
	return s.mend(ctx, d, f, &f.Copies[at], healthy, cp.Path, cp.Chunks, sd)
}

// toMend says what a scrub told --yes would do with a corrupt copy another copy is healthy for: repair it, with
// integrity.auto_repair, or move it to the trash.
func (s *scrubber) toMend(path string, sd *ScrubDisk) {
	if s.repair {
		sd.Repaired = append(sd.Repaired, path)
	} else {
		sd.Quarantined = append(sd.Quarantined, path)
	}
}

// mend repairs a corrupt copy from a healthy one, read whole already, with integrity.auto_repair, or moves it to the
// trash, and says which in sd (specs.md §16, §16.1). A copy that neither leaves is kept, and the next scrub of its disk
// takes it again.
func (s *scrubber) mend(ctx context.Context, d *config.Disk, f *catalog.FileRow, c *catalog.CopyRow, healthy *trash.Healthy, path string, chunks []byte,
	sd *ScrubDisk) error {
	if s.repair {
		settled, err := s.repairOrNot(ctx, f, c, healthy, path, chunks, sd)
		if err != nil || settled {
			return err
		}
		if f, c, err = s.leftCorrupt(ctx, f.ID, d.ID, healthy); err != nil {
			return err
		}
		if c == nil {
			slog.Warn("a corrupt copy a repair could not mend stays where it is", "disk", d.ID, "path", path)
			sd.Kept = append(sd.Kept, path)
			return nil
		}
	}
	outcome, err := s.session.Send(ctx, f, c, catalog.ReasonQuarantined)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if outcome == trash.OutcomeTrashed {
		sd.Quarantined = append(sd.Quarantined, path)
	} else {
		slog.Warn("a corrupt copy stays where it is", "disk", d.ID, "path", path, "outcome", outcome)
		sd.Kept = append(sd.Kept, path)
	}
	return nil
}

// leftInRoot gives the corrupt copies of a disk that an earlier scrub found and left in the root, as a disk that
// failed or a machine that stopped then leaves them, or a scrub not told --yes, for this scrub to take again before
// the copies due (specs.md §16): those whose disk can be written to, whose file has another copy ok on a disk that can
// be read, and, in a scrub that writes, with no operation left planned at their name, which only a reprise resolves
// (specs.md §17.2). A corrupt copy with nothing to mend it from stays as it is, marked and alerted, and is not taken
// again; nor is one on a disk that may only be read, which nothing writes to. It reads the catalog alone, so that no
// disk is opened for it.
func (s *scrubber) leftInRoot(ctx context.Context, disk string) ([]catalog.CorruptCopy, error) {
	if !s.session.Writable(disk) {
		return nil, nil
	}
	list, err := catalog.CorruptCopies(ctx, s.db, s.opts.Under, disk)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var left []catalog.CorruptCopy
	for _, cc := range list {
		if s.opts.Taken[cc] {
			continue
		}
		f, err := catalog.FileByID(ctx, s.db, cc.FileID)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if f == nil || !slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
			return c.Disk != disk && c.State == string(solver.CopyOK) && s.session.Readable(c.Disk)
		}) {
			continue
		}
		at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
		if at < 0 {
			continue
		}
		if s.opts.Apply && s.opts.Quarantine {
			planned, err := catalog.PlannedAt(ctx, s.db, disk, f.Copies[at].Place(f))
			if err != nil {
				return nil, fmt.Errorf("catalog: %w", err)
			}
			if planned {
				continue
			}
		}
		left = append(left, cc)
	}
	return left, nil
}

// takeAgain takes a corrupt copy an earlier scrub left in the root as this scrub takes one it finds corrupt, with no
// event of its own, as nothing new was found (specs.md §16): looked at first as a repair looks at it, and settled as
// one when it changed since (specs.md §16.1); then, with another copy healthy, read whole first, repaired from it or
// moved to the trash, or said to be without --yes. A disk lost on the way ends its scrub.
func (s *scrubber) takeAgain(ctx context.Context, d *config.Disk, cc *catalog.CorruptCopy, sd *ScrubDisk) error {
	f, err := catalog.FileByID(ctx, s.db, cc.FileID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == d.ID && c.State == string(solver.CopyCorrupt) })
	if at < 0 {
		return nil
	}
	c := &f.Copies[at]
	path := c.Place(f)
	chunks, err := catalog.FileChunks(ctx, s.db, f.ID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	sd.Again = append(sd.Again, path)
	s.taken = append(s.taken, *cc)
	slog.Info("a corrupt copy left in the root is taken again", "disk", d.ID, "path", path)
	outcome, changed, err := s.run.changedCopy(ctx, f, c, chunks)
	if err != nil {
		return err
	}
	switch {
	case changed && outcome == OutcomeConflict:
		sd.Conflict = append(sd.Conflict, path)
	case changed:
		// It reads right after all, or is still being written, or could not be looked at: it stays where it is.
		slog.Warn("a corrupt copy stays where it is", "disk", d.ID, "path", path, "outcome", outcome)
		sd.Kept = append(sd.Kept, path)
	default:
		healthy, err := findHealthy(ctx, s.session, f, d.ID, chunks)
		switch {
		case err != nil:
			return err
		case healthy == nil:
			sd.Kept = append(sd.Kept, path)
		case !s.opts.Apply || !s.opts.Quarantine:
			s.toMend(path, sd)
		default:
			if err := s.mend(ctx, d, f, c, healthy, path, chunks, sd); err != nil {
				return err
			}
		}
	}
	if !s.session.Readable(d.ID) {
		return fmt.Errorf("%w: %s", errDiskFailed, d.ID)
	}
	return nil
}

// repairOrNot repairs a corrupt copy from a healthy one (specs.md §16.1), and says whether that settled it: repaired,
// or left where it is. A repair that failed leaves it to quarantine (specs.md §16): for want of room, as a move to the
// trash takes no room but its folders', or for anything else that stopped it.
func (s *scrubber) repairOrNot(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, healthy *trash.Healthy, path string, chunks []byte, sd *ScrubDisk) (bool, error) {
	mode := modeFor(c, s.mode)
	// A copy that changed since the scrub read it is read as it is now, as a repair by hand reads it.
	outcome, changed, err := s.run.changedCopy(ctx, f, c, chunks)
	if err == nil && !changed {
		outcome, _, err = s.run.repairCopy(ctx, f, c, healthy, chunks, mode)
	}
	if err != nil {
		return true, err
	}
	switch outcome {
	case trash.OutcomeRepaired:
		sd.Repaired = append(sd.Repaired, path)
		if mode == RepairFile {
			s.remake = append(s.remake, f.Path)
		}
		return true, nil
	case trash.OutcomeNoRoom, trash.OutcomeIOError:
		return false, nil
	case OutcomeConflict:
		// Another version by now, kept with the others: neither repaired nor moved.
		sd.Conflict = append(sd.Conflict, path)
		return true, nil
	default:
		// It reads right after all, or changed, which is for the next pass; or nothing can be moved either, on a disk
		// that cannot be reached, or under a name its trash refuses.
		slog.Warn("a corrupt copy stays where it is", "disk", c.Disk, "path", path, "outcome", outcome)
		sd.Kept = append(sd.Kept, path)
		return true, nil
	}
}

// leftCorrupt gives the copy a repair could not mend, with its file, as the catalog has them now, for the scrub to
// send it to the trash (specs.md §16): what a repair wrote before it stopped gave the copy another change time, which
// its row took (specs.md §16.1), and a move is told of the copy its row says. It gives none for a copy that is to stay
// where it is: no longer corrupt, on a disk that cannot be written to any more, with no healthy copy that can still be
// read, or with an operation on it still planned, which the reprise resolves first (specs.md §17.2).
func (s *scrubber) leftCorrupt(ctx context.Context, fileID int64, disk string, healthy *trash.Healthy) (*catalog.FileRow, *catalog.CopyRow, error) {
	if !s.session.Writable(disk) || !s.session.Readable(healthy.Disk) {
		return nil, nil, nil
	}
	f, err := catalog.FileByID(ctx, s.db, fileID)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: %w", err)
	}
	if f == nil {
		return nil, nil, nil
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk && c.State == string(solver.CopyCorrupt) })
	if at < 0 {
		return nil, nil, nil
	}
	c := &f.Copies[at]
	planned, err := catalog.PlannedAt(ctx, s.db, disk, c.Place(f))
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: %w", err)
	}
	if planned {
		return nil, nil, nil
	}
	return f, c, nil
}

// unreadable says whose fault a read of a chunk that failed is (specs.md §16): the disk's when the disk no longer
// confirms, which loses it and stops its scrub, as an error it gives then; and the chunk's when it does, which is a
// chunk that does not match. A sector that cannot be read is the commonest fault of an old disk: taken for the
// disk's, it sent the disk out of the pool at each slice, to come back at the next mount check, the copy never named.
func (s *scrubber) unreadable(ctx context.Context, d *config.Disk, path string, chunk int64, cause error) error {
	confirmed, err := s.session.Confirms(ctx, d.ID, cause)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if !confirmed {
		return fmt.Errorf("%w: %w", errDiskFailed, cause)
	}
	slog.Warn("a chunk cannot be read, on a disk that confirms", "disk", d.ID, "path", path, "chunk", chunk, "error", cause)
	return nil
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
