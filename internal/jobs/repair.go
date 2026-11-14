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
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// The ways a corrupt copy is repaired (specs.md §16.1).
const (
	RepairChunk = "chunk"
	RepairFile  = "file"
)

// What became of a corrupt copy, besides the trash's outcomes (specs.md §16.1). A copy that changed since it was found
// corrupt and does not hold its file's content ends OutcomeConflict.
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
	// Record records what is found without Apply, as Apply does: a copy that reads right after all is verified, and one
	// that changed and holds another content makes its file a conflict.
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

// repairRun is what the repair of a copy takes from its run, atoll repair's or a scrub's (specs.md §16.1).
type repairRun struct {
	db      *sql.DB
	config  *config.Config
	machine disks.Machine
	session *trash.Session
	// apply repairs, and record records what is found: what apply does, and what a run that may not write still may.
	apply, record bool
	// conflicts holds the files a copy of which holds another content, and that are corrupt still, by another copy:
	// they are a conflict once the run has worked the states out, when none is corrupt any more.
	conflicts []int64
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
	r := &repairRun{db: db, config: c, machine: m, session: session, apply: opts.Apply, record: opts.Apply || opts.Record}
	sum := &RepairSummary{Applied: opts.Apply, Items: []RepairItem{}}
	var remake []string
	// at says where the item of a copy is in the summary, and -1 for a copy that has none.
	at := make([]int, len(list))
	verified := map[int64]bool{}
	settle := func(i int) error {
		item, err := r.repairOne(ctx, &list[i], mode)
		if err != nil || item == nil {
			return err
		}
		switch {
		case item.Outcome == trash.OutcomeRepaired && item.Mode == RepairFile:
			remake = append(remake, item.Path)
		case item.Outcome == OutcomeVerified:
			verified[list[i].FileID] = true
		}
		if at[i] < 0 {
			at[i] = len(sum.Items)
			sum.Items = append(sum.Items, *item)
		} else {
			sum.Items[at[i]] = *item
		}
		return nil
	}
	for i := range list {
		at[i] = -1
		if err := settle(i); err != nil {
			return nil, err
		}
	}
	// A copy found to hold its file's content, as one put right by hand, is the healthy copy that the others of its
	// file had none of: they are tried again, once, when what was found is recorded.
	for i := range list {
		if r.record && at[i] >= 0 && sum.Items[at[i]].Outcome == OutcomeNoHealthyCopy && verified[list[i].FileID] {
			if err := settle(i); err != nil {
				return nil, err
			}
		}
	}
	if opts.Apply {
		if err := session.RemoveEmptied(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		if err := session.Settle(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}
	if err := r.callConflicts(ctx); err != nil {
		return nil, err
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

// callConflicts makes a conflict of each file a copy of which was found to hold another content while another copy
// was corrupt still, once the states are worked out: a file is corrupt while a copy of it is, and a file still
// corrupt stays so, the scrub finding the conflict again from the copy never verified (specs.md §16).
func (r *repairRun) callConflicts(ctx context.Context) error {
	for _, id := range r.conflicts {
		if err := catalog.SetConflict(ctx, r.db, id, r.machine.Now()); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	r.conflicts = nil
	return nil
}

// repairOne repairs one corrupt copy, or says what a repair would do. It gives nil for a copy no longer corrupt.
func (r *repairRun) repairOne(ctx context.Context, cc *catalog.CorruptCopy, mode string) (*RepairItem, error) {
	f, err := catalog.FileByID(ctx, r.db, cc.FileID)
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
	if !r.session.Readable(c.Disk) {
		item.Outcome = trash.OutcomeDiskUnavailable
		return item, nil
	}
	chunks, err := catalog.FileChunks(ctx, r.db, f.ID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	// A copy that changed since it was found corrupt is read as it is now, which takes no healthy copy.
	if outcome, changed, err := r.changedCopy(ctx, f, c, chunks); err != nil || changed {
		item.Outcome = outcome
		return item, err
	}
	healthy, err := findHealthy(ctx, r.session, f, c.Disk, chunks)
	if err != nil {
		return nil, err
	}
	if healthy == nil {
		item.Outcome = OutcomeNoHealthyCopy
		return item, nil
	}
	outcome, bad, err := r.repairCopy(ctx, f, c, healthy, chunks, item.Mode)
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

// asBad gives what badChunks asks of a chunk that cannot be read, on a copy of a disk: a bad chunk like another when
// the disk still confirms, which is asked once (specs.md §16.1), each then added to unreadable; and the disk lost
// when it does not.
func (r *repairRun) asBad(ctx context.Context, disk, place string, unreadable *[]int64) func(int64, error) (bool, error) {
	confirmed := false
	return func(chunk int64, cause error) (bool, error) {
		if !confirmed {
			ok, err := r.session.Confirms(ctx, disk, cause)
			if err != nil {
				return false, fmt.Errorf("%w", err)
			}
			if !ok {
				return false, fmt.Errorf("%w: %w", ops.ErrDiskLost, cause)
			}
			confirmed = true
		}
		slog.Warn("a chunk of a corrupt copy cannot be read, on a disk that confirms", "disk", disk, "path", place, "chunk", chunk, "error", cause)
		*unreadable = append(*unreadable, chunk)
		return true, nil
	}
}

// changedCopy looks at a copy found corrupt, and settles it when its name holds a file that is no longer what its row
// says, which it then says (specs.md §16.1). A repair left such a copy to the next pass, as a change, and a pass leaves
// a corrupt file alone: touched by anyone, or put right by hand, it stayed corrupt for good. It is read whole as it is
// now. Holding its file's content, it is ok and verified, its row what was read. Otherwise nothing tells rot that
// someone touched from another version: its file is a conflict, every version kept (specs.md §15.7), once the change
// is settle old, as a copy still being written is no version yet (specs.md §15.4). Nothing is written to a disk.
func (r *repairRun) changedCopy(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, chunks []byte) (string, bool, error) {
	handle, err := r.session.Handle(ctx, c.Disk)
	if err != nil {
		return "", false, fmt.Errorf("%w", err)
	}
	if handle == nil {
		return "", false, nil
	}
	place := c.Place(f)
	st, there, err := handle.StatRoot(place)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return trash.OutcomeIOError, true, r.session.Lose(ctx, c.Disk, err) //nolint:wrapcheck // the session's own
	case err != nil:
		slog.Warn("a corrupt copy cannot be looked at", "disk", c.Disk, "path", place, "error", err)
		return trash.OutcomeIOError, true, nil
	case !there || st.Kind != fsx.KindFile:
		// Its name holds no file any more: a deletion, or something else there, for the next pass.
		return trash.OutcomeChanged, true, nil
	}
	found, err := watch.FileFrom(place, &st)
	if err != nil {
		return trash.OutcomeChanged, true, nil //nolint:nilerr // a time no row can hold is a look that says nothing yet
	}
	if watch.Unchanged(&found, &catalog.KnownCopy{Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}) {
		return "", false, nil
	}
	holds := false
	// A copy of another size cannot hold the file's content, and is not read to say so.
	if found.Size == f.Size {
		now := catalog.KnownCopy{
			Path: place, FileID: f.ID, Size: sql.NullInt64{Int64: found.Size, Valid: true}, MtimeNS: sql.NullInt64{Int64: found.MtimeNS, Valid: true},
			CtimeNS: sql.NullInt64{Int64: found.CtimeNS, Valid: true}, FileKey: sql.NullString{String: found.FileKey, Valid: true},
		}
		var unreadable []int64
		bad, err := badChunks(handle, &now, chunks, r.asBad(ctx, c.Disk, place, &unreadable))
		switch {
		case errors.Is(err, ops.ErrGone):
			// It moved again, before the read or during it: still being written.
			return trash.OutcomeChanged, true, nil
		case errors.Is(err, ops.ErrDiskLost):
			return trash.OutcomeIOError, true, r.session.Lose(ctx, c.Disk, err) //nolint:wrapcheck // the session's own
		case err != nil:
			slog.Warn("a corrupt copy cannot be read", "disk", c.Disk, "path", place, "error", err)
			return trash.OutcomeIOError, true, nil
		}
		holds = len(bad) == 0
	}
	now := r.machine.Now()
	if !holds {
		size := f.Size
		if found.CtimeNS+r.config.Policy(f.Path, &size).Settle.Nanoseconds() > now {
			return trash.OutcomeChanged, true, nil
		}
		slog.Warn("a corrupt copy that changed holds another content than its file's: a conflict", "disk", c.Disk, "path", place)
	}
	if r.record {
		look := catalog.ChangedCopy{
			Disk: c.Disk, FileKey: found.FileKey, Nlink: found.Nlink, Size: found.Size, MtimeNS: found.MtimeNS, CtimeNS: found.CtimeNS,
			Mode: found.Mode, UID: found.UID, GID: found.GID, Replaced: !c.FileKey.Valid || c.FileKey.String != found.FileKey,
		}
		if found.BtimeNS != nil {
			look.BtimeNS = sql.NullInt64{Int64: *found.BtimeNS, Valid: true}
		}
		conflict, err := catalog.SettleChanged(ctx, r.db, f.ID, &look, holds, now)
		if err != nil {
			return "", true, fmt.Errorf("catalog: %w", err)
		}
		if !holds && !conflict {
			r.conflicts = append(r.conflicts, f.ID)
		}
		r.session.MarkChanged()
	}
	if holds {
		return OutcomeVerified, true, nil
	}
	return OutcomeConflict, true, nil
}

// repairCopy reads a corrupt copy whole, and repairs it from a healthy one when the run applies: by chunk, or whole,
// sent to the trash for replication to make again. It gives the outcome and the bad chunks found.
func (r *repairRun) repairCopy(ctx context.Context, f *catalog.FileRow, c *catalog.CopyRow, healthy *trash.Healthy, chunks []byte, mode string) (string, int, error) {
	handle, err := r.session.Handle(ctx, c.Disk)
	if err != nil {
		return "", 0, fmt.Errorf("%w", err)
	}
	if handle == nil {
		return trash.OutcomeDiskUnavailable, 0, nil
	}
	place := c.Place(f)
	// A chunk that cannot be read is a bad chunk, when the disk still confirms, which is asked once (specs.md §16.1).
	var unreadable []int64
	bad, err := badChunks(handle, &catalog.KnownCopy{Path: place, FileID: f.ID, Size: c.Size, MtimeNS: c.MtimeNS, CtimeNS: c.CtimeNS, FileKey: c.FileKey}, chunks,
		r.asBad(ctx, c.Disk, place, &unreadable))
	switch {
	case errors.Is(err, ops.ErrGone):
		return trash.OutcomeChanged, 0, nil
	case errors.Is(err, ops.ErrDiskLost):
		return trash.OutcomeIOError, 0, r.session.Lose(ctx, c.Disk, err) //nolint:wrapcheck // the session's own
	case err != nil:
		slog.Warn("a corrupt copy cannot be read", "disk", c.Disk, "path", place, "error", err)
		return trash.OutcomeIOError, 0, nil
	}
	if len(bad) == 0 {
		if r.record {
			if err := catalog.ClearCorrupt(ctx, r.db, f.ID, c.Disk, r.machine.Now()); err != nil {
				return "", 0, fmt.Errorf("catalog: %w", err)
			}
			r.session.MarkChanged()
		}
		return OutcomeVerified, 0, nil
	}
	if !r.apply {
		return OutcomeToRepair, len(bad), nil
	}
	if mode == RepairFile {
		outcome, err := r.session.Send(ctx, f, c, catalog.ReasonRepaired)
		if outcome == trash.OutcomeTrashed {
			outcome = trash.OutcomeRepaired
		}
		return outcome, len(bad), err //nolint:wrapcheck // the session's own
	}
	outcome, err := r.session.RepairChunks(ctx, f, c, bad, unreadable, healthy, chunks)
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
