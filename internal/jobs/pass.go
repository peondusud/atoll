package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/watch"
)

// What a pass says of what it did (specs.md §15.12).
const (
	OutcomeRenamed     = trash.OutcomeRenamed
	OutcomeToRename    = "to-rename"
	OutcomeToTrash     = "to-trash"
	OutcomeAlready     = "already-there"
	OutcomeHeld        = "held"
	OutcomeCanceled    = "canceled"
	OutcomeUnavailable = "disk-unavailable"
	KindRename         = "rename"
	KindFolder         = "folder"
	KindTrash          = "trash"
	KindReplicate      = "replicate"
	reasonDeleted      = "deleted"
	reasonRenamed      = "renamed"
)

// Decision is what a pass decided about one file.
type Decision struct {
	Path string
	Kind string
	To   string
}

// Operation is what ran, or with a dry run what would run, on a disk. A copy's Disk is its target, and
// Source the disk it is read from.
type Operation struct {
	Disk    string
	Kind    string
	Path    string
	To      string
	Reason  string
	Source  string
	Outcome string
}

// DiskLine says what became of a disk in a pass.
type DiskLine struct {
	ID      string
	Verdict disks.Verdict
	Walked  bool
	// Spared says that a pass of the daemon left the disk unwalked, as nothing called for a look at it: its copies read
	// as their rows say, and Verdict is its mount check's (specs.md §15.13). No part of the pass's output.
	Spared bool
	// Partial says that a pass of the daemon looked at some names of the disk alone, those the watcher gave: what it
	// says of the names a walk could not reach is no more than those (specs.md §15.13). No part of the pass's output.
	Partial bool
	// Refused counts the names its walk could not reach as they refuse the account that asks, and RefusedFirst holds
	// the first RefusedShown of them in byte order: what an alert says (specs.md §15.10, §23). They are no part of the
	// pass's output.
	Refused      int64
	RefusedFirst []string
	// TooLong counts the names its walk neither recorded nor went below, as their path is past the limit of the disk's
	// mergerfs branch, and TooLongFirst holds the first RefusedShown of them in byte order: what an alert says
	// (specs.md §15.8, §23). They are no part of the pass's output either.
	TooLong      int64
	TooLongFirst []string
	// NewFolders holds the folders where its walk found copies new to the catalog, by path in byte order: what the
	// daemon tells the media servers of (specs.md §21.5). No part of the pass's output either.
	NewFolders []string
}

// RefusedShown is how many of the names that refused it a disk's line keeps: as many as an alert names.
const RefusedShown = 10

// GuardLine is a guard tripped: the mass-deletion guard of a disk, or its mass-modification guard (specs.md §15.6).
type GuardLine struct {
	Disk  string
	Kind  string
	Count int64
}

// The kinds of a guard.
const (
	GuardDeletions     = "deletions"
	GuardModifications = "modifications"
)

// Sink is told of what a pass does as it does it: holding a rename of a whole pool until the end would
// not fit in the memory of specs.md §26. Every decision comes before the first operation.
type Sink interface {
	Disks(list []DiskLine) error
	Decision(d *Decision) error
	Operation(op *Operation) error
}

// Outcome is what a pass says at its end. With no Sink it holds what one would have been told.
type Outcome struct {
	DryRun     bool
	Disks      []DiskLine
	Decisions  []Decision
	Operations []Operation
	Guards     []GuardLine
	Waiting    int64
	NextDue    sql.NullInt64
	// Partial says that a disk was skipped or failed, an operation stayed planned or ended in an error,
	// or something due could not run for want of a writable disk. Held says that a guard holds something.
	Partial bool
	Held    bool
	// ToRead holds the files a pass of the daemon left undecided, as their changed copies must be read first, by path
	// in byte order: ReadChanged decides them, by slices (specs.md §15.13).
	ToRead []PendingRead
	// WaitDisks holds the disks where this pass's look found what the catalog does not hold yet, in the configuration's
	// order: a later look must see it again, so the daemon walks them in its next pass (specs.md §15.13). Set by a pass
	// that writes; no part of the pass's output.
	WaitDisks []string
	// Copied counts the bytes of the replicas this pass made, for the metrics (specs.md §23.1); no part of the pass's
	// output.
	Copied int64
	// Again holds, by disk, the names a look limited to names leaves for the next pass to look at again, as what it
	// found waits there: a copy found at another name, a changed copy, a folder renamed (specs.md §15.13). Whole holds the
	// disks such a look concluded nothing about a copy of, which it no longer found: the next pass walks them whole.
	// Neither is part of the pass's output.
	Again map[string][]watch.Named
	Whole []string
}

// Scope holds the disks a pass of the daemon is asked to look at, by id (specs.md §15.13). The pass looks at those, and
// at every disk whose rows or operations call for a look whatever the scope says; it spares the others. A nil scope
// walks every disk.
type Scope map[string]Looking

// Looking is how a pass looks at a disk: with Names, at those names alone and what they hold, as the watcher gave them;
// with none, it walks the whole disk.
type Looking struct {
	Names []watch.Named
}

// Whole walks a whole disk.
var Whole = Looking{}

// Add looks at more names of a disk: a disk walked whole stays so.
func (s Scope) Add(id string, names []watch.Named) {
	if was, ok := s[id]; ok && was.Names == nil {
		return
	}
	// Never nil: no name is a look at nothing, not a walk of the whole disk.
	merged := make([]watch.Named, 0, len(s[id].Names)+len(names))
	s[id] = Looking{Names: append(append(merged, s[id].Names...), names...)}
}

// OnBattery says whether the UPS runs on battery now (specs.md §20). A pass asks it before each copy and each transfer
// it would start, and balancing before each file it would move: on battery, none starts, and what runs finishes. Nil
// reads no UPS.
type OnBattery func() bool

// PendingRead is a file whose changed copies a pass left to read.
type PendingRead struct {
	FileID int64
	// AnswerID is the answer to the mass-modification guard of its disk that decided it when the pass found it, 0 with
	// none: it stays under that answer, whenever its slice comes (specs.md §15.6).
	AnswerID int64
}

type pass struct {
	// viewOps are the operations of Atoll's own view no pass has taken in, and throughView the copies, by disk and
	// inode, it sent to the trash (specs.md §21.2).
	viewOps     []catalog.ViewOp
	throughView map[string]bool
	db          *sql.DB
	config      *config.Config
	machine     disks.Machine
	session     *trash.Session
	apply       bool
	// scope is the disks this pass is asked to walk, nil for every disk, and spared those it left unwalked as nothing
	// called for a look at them: their copies read as their rows say (specs.md §15.13).
	scope  Scope
	spared map[string]bool
	// partial holds the disks this pass looked at some names of alone, as the watcher gave them, and names the names
	// each look read in the end, by disk.
	partial map[string]bool
	// added holds, by disk, the copies this pass's look recorded: the count of a disk's copies before it leaves them out.
	added map[string]int64
	names map[string][]string
	// only limits the plans of this pass to what may have changed, nil for every file; ran holds the files whose copy
	// or transfer ran in it, which a plan after them takes too (specs.md §15.13).
	only *watch.Only
	ran  map[int64]bool
	// start says that the pass is a start of the daemon, which records the physical disks of each disk (R-06).
	start bool
	// onBattery says whether the UPS runs on battery, as a copy or a transfer is about to start (specs.md §20).
	onBattery OnBattery
	now       int64
	direct    string
	debounce  time.Duration
	window    time.Duration
	files     int64
	percent   value.Percent
	// the settings of the mass-modification guard, and its state by disk (specs.md §15.6)
	modWindow   time.Duration
	modFiles    int64
	modPercent  value.Percent
	modGuards   map[string]*modGuard
	rows        map[string]int64
	walked      map[string]bool
	seen        map[string][]watch.CopySeen
	moves       map[string][]watch.FolderMove
	guards      map[string]*guardState
	plans       map[string]*diskPlan
	jobsDropped map[int64]bool // of a dry run: the files whose queued trash jobs a decision drops
	// of a dry run: the files whose jobs a decision dropped before anything ran, whose stale copies a real pass gives
	// their overwrite again at once (specs.md §15.4)
	requeued map[int64]bool
	// of a dry run: the files in conflict that a decision ends the conflict of
	conflictCleared map[int64]bool
	pulled          map[int64]bool // the files decided on an earlier page, with what wanted their name
	stayed          map[int64]bool // what was decided and did not leave its path
	undecided       map[int64]bool // of a dry run: what it saw and has not decided yet
	// the files with a job running, its operation left planned once the reprises are over: each waits for its
	// decisions, as for its modifications (specs.md §14.1, §15.12)
	running map[int64]bool
	// the folders a look found at neither name, by disk, whose rows stay for the copies below them
	unfound map[string][]string
	// where a decision of this pass found a copy back at its file's path, by disk
	cleared map[string][]string
	// the folders, by disk, found renamed and left waiting in this pass
	waited map[string][]string
	// the folders, by disk, found renamed that wait for what no decision of this pass ends, worked out before the files
	// are decided: what lands at or below the old name of one of them waits for it
	held map[string][]watch.FolderMove
	// the copies, by disk, moved to a name another file holds: their rows cannot leave where they say
	stuck map[string]map[int64]bool
	// the folders found renamed that copies came with, as the rows said before any decision of this pass
	realMoves map[folderKey]bool
	// by disk, the names of the operations left planned, which only a reprise can tell of
	planned map[string][]string
	// the operations utimens of the modifications, until every decision is out
	metaOps []metaOp
	// the files whose copy waits for settle, and the changed copies not yet due, and when the first of them is due
	copiesWaiting int64
	copiesDue     sql.NullInt64
	// closed is what the watcher saw closed and quiet since: a file in immediate mode there waits for no settle.
	closed Closed
	// linkGuardOn is trash.link_guard (specs.md §17).
	linkGuardOn bool
	// restores holds the copies a page of decisions makes again in their place, with deletes.direct = "restore".
	restores []catalog.Queue
	// settled says that the copies worked out each file, and that nothing ran since
	settled bool
	// later says that this pass reads no file whole: what must be read to be decided is left in the outcome's ToRead
	// (specs.md §15.13). takenUnder is, in a slice of those reads, the answer the file now decided was taken under.
	later      bool
	takenUnder int64
	out        *Outcome
	sink       Sink
	sinkErr    error
}

type folderKey struct {
	disk, from string
}

type guardState struct {
	read    *catalog.GuardRead
	wishes  map[int64]bool // the files whose copy a refused rename would send, as this pass found them
	tripped bool
}

// Run does all the work that is due, once, and gives everything in its outcome.
func Run(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool) (*Outcome, error) {
	return RunTo(ctx, db, c, m, apply, nil)
}

// Closed holds the logical paths the watcher saw closed after a write by someone other than Atoll, then left alone
// for deletes.debounce: a file in immediate mode at one of them is copied without waiting for settle (specs.md §12.3,
// §14.1).
type Closed map[string]bool

// RunTo does all the work that is due, once (specs.md §15.12), and tells a sink as it goes. Without
// apply it writes nothing to a disk, and answers as if every difference it sees were due.
func RunTo(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool, sink Sink) (*Outcome, error) {
	p := newPass(db, c, m, apply, sink, nil)
	// atoll daemon --until-idle is a start of the daemon: it records the physical disks of each disk (R-06).
	p.start = true
	return p.run(ctx)
}

// RunClosed is RunTo for the daemon that runs for good, which tells it what its watcher saw closed.
func RunClosed(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool, sink Sink, closed Closed) (*Outcome, error) {
	return newPass(db, c, m, apply, sink, closed).run(ctx)
}

// RunLater is RunClosed for a pass that reads no file whole, as those of the daemon that runs for good (specs.md
// §15.13): a file whose changed copies must be read to be decided is left in the outcome's ToRead, for ReadChanged. A
// dry run records nothing, so it reads in its pass what it must to say what it would decide.
func RunLater(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool, sink Sink, closed Closed) (*Outcome, error) {
	return RunLaterOn(ctx, db, c, m, apply, sink, closed, nil, nil)
}

// RunLaterOn is RunLater for a pass that walks only some disks (specs.md §15.13): those of the scope, and those whose
// rows or operations call for a look. It spares the others: no walk, no confirmation, and their copies read as their
// rows say, as a walk of a disk where nothing changed since its last walk would find them. A nil scope walks every disk.
// With onBattery, it starts no copy and no transfer while the UPS runs on battery: each waits (specs.md §20).
func RunLaterOn(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, apply bool, sink Sink, closed Closed, scope Scope, onBattery OnBattery) (*Outcome, error) {
	p := newPass(db, c, m, apply, sink, closed)
	p.later = apply
	p.scope = scope
	p.onBattery = onBattery
	return p.run(ctx)
}

func newPass(db *sql.DB, c *config.Config, m disks.Machine, apply bool, sink Sink, closed Closed) *pass {
	return &pass{
		db: db, config: c, machine: m, apply: apply, now: m.Now(), sink: sink, closed: closed,
		rows: map[string]int64{}, walked: map[string]bool{}, spared: map[string]bool{}, partial: map[string]bool{}, added: map[string]int64{}, seen: map[string][]watch.CopySeen{}, moves: map[string][]watch.FolderMove{},
		guards: map[string]*guardState{}, modGuards: map[string]*modGuard{}, plans: map[string]*diskPlan{}, jobsDropped: map[int64]bool{}, requeued: map[int64]bool{}, conflictCleared: map[int64]bool{}, pulled: map[int64]bool{}, stayed: map[int64]bool{},
		stuck: map[string]map[int64]bool{}, realMoves: map[folderKey]bool{}, planned: map[string][]string{}, unfound: map[string][]string{}, cleared: map[string][]string{}, waited: map[string][]string{}, out: &Outcome{DryRun: !apply},
	}
}

func (p *pass) run(ctx context.Context) (*Outcome, error) {
	if err := p.readSettings(); err != nil {
		return nil, err
	}
	session, err := trash.Open(ctx, p.db, p.config, p.machine, p.apply)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer session.Close()
	p.session = session
	for _, step := range []func(context.Context) error{p.look, p.cleanJobs, p.readGuards, p.readFolders, p.readHeld, p.decide, p.decideFolders, p.readModifyGuards, p.modify, p.sortGuards, p.queueStale, p.work, p.cleanJobs, p.findWaits, p.copies, p.transfers, p.finish} {
		if err := step(ctx); err != nil {
			return nil, err
		}
		if p.sinkErr != nil {
			return nil, p.sinkErr
		}
	}
	return p.out, nil
}

func (p *pass) decided(d Decision) {
	switch {
	case p.sink == nil:
		p.out.Decisions = append(p.out.Decisions, d)
	case p.sinkErr == nil:
		p.sinkErr = p.sink.Decision(&d)
	}
}

func (p *pass) operated(op Operation) {
	switch {
	case p.sink == nil:
		p.out.Operations = append(p.out.Operations, op)
	case p.sinkErr == nil:
		p.sinkErr = p.sink.Operation(&op)
	}
}

func (p *pass) readSettings() error {
	effective := p.config.Effective()
	p.direct, _ = effective["deletes.direct"].(string)
	text, _ := effective["deletes.debounce"].(string)
	debounce, err := value.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("deletes.debounce: %w", err)
	}
	p.debounce = debounce
	text, _ = effective["deletes.mass_guard.window"].(string)
	window, err := value.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("deletes.mass_guard.window: %w", err)
	}
	p.window = window
	text, _ = effective["deletes.mass_guard.percent"].(string)
	percent, err := value.ParsePercent(text)
	if err != nil {
		return fmt.Errorf("deletes.mass_guard.percent: %w", err)
	}
	p.percent = percent
	p.files, _ = effective["deletes.mass_guard.files"].(int64)
	text, _ = effective["modifies.mass_guard.window"].(string)
	if p.modWindow, err = value.ParseDuration(text); err != nil {
		return fmt.Errorf("modifies.mass_guard.window: %w", err)
	}
	text, _ = effective["modifies.mass_guard.percent"].(string)
	if p.modPercent, err = value.ParsePercent(text); err != nil {
		return fmt.Errorf("modifies.mass_guard.percent: %w", err)
	}
	p.modFiles, _ = effective["modifies.mass_guard.files"].(int64)
	p.linkGuardOn, _ = effective["trash.link_guard"].(bool)
	return nil
}

// look walks each disk that is not missing, and gives each copy on it its verdict. A pass with a scope spares the disks
// it need not walk.
func (p *pass) look(ctx context.Context) error {
	planned, err := catalog.PlannedOps(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if err := p.readViewOps(ctx); err != nil {
		return err
	}
	toWalk, err := p.toWalk(ctx, planned)
	if err != nil {
		return err
	}
	if p.start && p.apply {
		RecordDevices(ctx, p.db, p.config, p.machine, func(i int) bool {
			return p.session.VerdictOf(i).State != disks.Missing && p.session.Readable(p.config.Disks[i].ID)
		})
	}
	for i := range p.config.Disks {
		d := &p.config.Disks[i]
		if d.State == config.DiskFailed {
			// Never read again: no walk and no line, as it is neither missing nor skipped (specs.md §18).
			continue
		}
		verdict := p.session.VerdictOf(i)
		if !p.session.Readable(d.ID) && verdict.State != disks.Missing {
			verdict = disks.Verdict{State: disks.Missing, Reason: disks.IOError, Message: "the disk failed its confirmation"}
		}
		// A dry run confirms nothing, and asks only whether this session still holds the disk: a look at its own
		// folders may have lost it.
		look := &watch.Look{Write: p.apply, Own: p.ownFiles, Confirm: p.confirm, Planned: append(plannedOn(planned, d.ID), p.viewPlannedOn(d.ID)...)}
		p.planned[d.ID] = look.Planned
		looking, asked := toWalk[d.ID]
		if toWalk != nil && !asked && verdict.State != disks.Missing {
			// Nothing called for a look at it: nothing reads it, and its copies read as their rows say (specs.md §15.13).
			p.spared[d.ID] = true
			p.out.Disks = append(p.out.Disks, DiskLine{ID: d.ID, Verdict: verdict, Spared: true})
			continue
		}
		if looking.Names != nil {
			// Only the names the watcher gave, and what they hold (specs.md §15.13).
			look.Names = looking.Names
			p.partial[d.ID] = true
		}
		report, err := watch.LookAtDisk(ctx, p.db, p.config, p.machine, d, verdict, p.now, look)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if report.Whole {
			p.out.Whole = append(p.out.Whole, d.ID)
		}
		if p.apply {
			p.added[d.ID] = int64(report.New)
		}
		if p.partial[d.ID] && report.Walked {
			if p.names == nil {
				p.names = map[string][]string{}
			}
			p.names[d.ID] = report.Looked
		}
		if !report.Walked {
			p.out.Partial = true
			if err := p.session.MarkMissing(ctx, d.ID); err != nil {
				return fmt.Errorf("%w", err)
			}
		} else {
			p.walked[d.ID] = true
			watch.SortSeen(report.Seen)
			p.seen[d.ID] = report.Seen
			p.moves[d.ID] = topMoves(report.FolderMoves)
			p.unfound[d.ID] = report.UnfoundFolders
		}
		line := DiskLine{ID: d.ID, Verdict: report.Verdict, Walked: report.Walked, Partial: report.Walked && p.partial[d.ID]}
		if report.Walked {
			line.Refused, line.RefusedFirst = firstRefused(report.Refused)
			line.TooLong, line.TooLongFirst = firstRefused(report.TooLong)
			line.NewFolders = slices.Sorted(slices.Values(report.NewFolders))
		}
		p.out.Disks = append(p.out.Disks, line)
	}
	if p.sink != nil {
		p.sinkErr = p.sink.Disks(p.out.Disks)
	}
	return nil
}

// toWalk gives the disks a pass with a scope looks at, nil for every disk: those of its scope, and those a look must
// see again whatever the scope says (specs.md §15.13). A copy whose row says it went or moved has a clock that only a
// look lets run out. A copy that went is found again at its name, or not: the names of such copies are looked at
// again. A copy that moved, or is uncertain, is found again at the name a look found it at, which its row does not
// hold: its disk is walked whole, unless the scope gives names for it, those a look limited to names left to see again,
// the names it found such copies at among them. An operation of Atoll's own left planned is found again by a walk; and an
// operation of the view is taken in once the looks of its disks find what it did: the names it touched, a folder's on
// every disk that holds it, by a look limited to names, as the watcher would give them.
func (p *pass) toWalk(ctx context.Context, planned []catalog.Op) (Scope, error) {
	if p.scope == nil {
		return nil, nil
	}
	walk := maps.Clone(p.scope)
	if walk == nil {
		walk = Scope{}
	}
	gone, err := catalog.DisksWithGoneCopies(ctx, p.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for _, id := range gone {
		clocked, err := catalog.ClockedCopies(ctx, p.db, id)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		names := make([]watch.Named, 0, len(clocked))
		whole := false
		for i := range clocked {
			names = append(names, watch.Named{Path: clocked[i].Path})
			whole = whole || (clocked[i].Kind != string(solver.VerdictGone) && clocked[i].Kind != string(solver.VerdictOwn))
		}
		if given, ok := p.scope[id]; whole && (!ok || given.Names == nil) {
			walk[id] = Whole
			continue
		}
		walk.Add(id, names)
	}
	for i := range planned {
		walk[planned[i].Disk] = Whole
	}
	viewed, err := p.viewNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(viewed)) {
		if h := viewed[id]; h.Whole {
			walk[id] = Whole
		} else {
			walk.Add(id, h.Names)
		}
	}
	return walk, nil
}

// viewNames gives, by disk, what this pass looks at for the operations of Atoll's own view no pass has taken in: the
// names each touched, and below a folder renamed or removed what the catalog holds there, on every disk that holds it,
// rather than the whole of each disk (specs.md §15.13, §21.2; the maintainer, 2026-11-19).
func (p *pass) viewNames(ctx context.Context) (map[string]watch.Heard, error) {
	if len(p.viewOps) == 0 {
		return nil, nil
	}
	ops := make([]watch.ViewOp, 0, len(p.viewOps))
	for i := range p.viewOps {
		v := &p.viewOps[i]
		if v.Kind == catalog.ViewPart {
			// A part written in parallel is in tmp/: nothing of the root to look at.
			continue
		}
		op := watch.ViewOp{Kind: v.Kind, Disk: v.Disk, Path: v.Path, Path2: v.Path2, Folder: v.Folder}
		if v.Folder || v.Kind == catalog.ViewRmdir {
			for _, at := range []string{v.Path, v.Path2} {
				if at == "" {
					continue
				}
				held, err := catalog.FolderDisks(ctx, p.db, at)
				if err != nil {
					return nil, fmt.Errorf("catalog: %w", err)
				}
				op.Holders = append(op.Holders, held...)
			}
		}
		ops = append(ops, op)
	}
	return watch.ViewNames(ops, watch.MaxNames), nil
}

// looked says that this pass knows what a disk holds: it walked it, or spared it, which reads its copies as their rows
// say.
func (p *pass) looked(id string) bool {
	return p.walked[id] || p.spared[id]
}

// firstRefused counts the names that refused a walk, or that it left past the limit of the disk's mergerfs branch, and
// gives the first RefusedShown of them in byte order.
func firstRefused(names []string) (int64, []string) {
	if len(names) == 0 {
		return 0, nil
	}
	first := slices.Clone(names)
	slices.Sort(first)
	return int64(len(names)), first[:min(len(first), RefusedShown)]
}

// plannedOn gives the logical paths of the operations left planned on a disk, which are only left on a
// disk the reprise could not write to. Only a reprise can tell where those stand, and what a walk finds
// there is no user's doing.
func plannedOn(list []catalog.Op, disk string) []string {
	var paths []string
	for i := range list {
		op := &list[i]
		// What the link guard does leaves the root as it is.
		if op.Disk != disk || op.Link != 0 {
			continue
		}
		switch op.Kind {
		case catalog.OpRename:
			paths = append(paths, op.Path, op.Path2)
		case catalog.OpTrash, catalog.OpRestore, catalog.OpMkdir, catalog.OpRmdir, catalog.OpCreate, catalog.OpUtimens, catalog.OpChmod, catalog.OpChown, catalog.OpXattr:
			paths = append(paths, op.Path)
		}
	}
	return paths
}

// cleanJobs forgets what a rejection said of a copy no longer behind, however it caught up: a later
// refused rename of it is another question.
func (p *pass) cleanJobs(ctx context.Context) error {
	if !p.apply {
		return nil
	}
	if err := catalog.DropStaleCanceled(ctx, p.db); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// queueStale gives its trash job again to each stale copy that lost it, once the decisions are made and before
// anything runs (specs.md §15.4). A decision that drops every trash job of a file drops a stale copy's too: a rename
// seen after a deletion, a guard rejected, a deletion that no longer stands. Nothing else queues it, so the copy
// stayed in media/ with the version that came before, where mergerfs serves it when its branch comes first, and no
// replica of the current version was ever made. A dry run lists them with the jobs that are queued.
func (p *pass) queueStale(ctx context.Context) error {
	if !p.apply {
		return nil
	}
	list, err := p.staleWithoutJob(ctx, "")
	if err != nil {
		return err
	}
	for i := range list {
		s := &list[i]
		size := s.Size
		priority := int64(p.config.Policy(s.Path, &size).Priority)
		if err := catalog.QueueOverwrite(ctx, p.db, s.FileID, s.Src, s.Disk, priority); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
	}
	return nil
}

// staleWithoutJob gives the stale copies with no trash job queued whose overwrite would stand, on one disk or on all,
// leaving out a copy this pass's look did not find as its row says: that one is a change, which a decision is made
// of first (specs.md §15.4).
func (p *pass) staleWithoutJob(ctx context.Context, disk string) ([]catalog.StaleCopy, error) {
	list, err := catalog.StaleWithoutJob(ctx, p.db, disk)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	kept := list[:0]
	for i := range list {
		s := &list[i]
		f, err := catalog.FileByID(ctx, p.db, s.FileID)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if f == nil {
			continue
		}
		at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == s.Disk })
		if at < 0 || !isAsItsRow(p.verdictOf(f, &f.Copies[at])) && p.looked(s.Disk) {
			continue
		}
		kept = append(kept, *s)
	}
	return kept, nil
}

func (p *pass) confirm(ctx context.Context, d *config.Disk) (bool, error) {
	handle, err := p.session.Handle(ctx, d.ID)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	return handle != nil, nil
}

func (p *pass) ownFiles(d *config.Disk, inodes map[string]bool) (map[string][]solver.FileLook, error) {
	ctx := context.Background()
	handle, err := p.session.Handle(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	if handle == nil {
		return nil, nil //nolint:nilnil // the confirmation that follows says what became of the disk
	}
	own, err := handle.OwnFiles(ctx, p.db, inodes, watch.LimitsOf(p.config, d).Own())
	if errors.Is(err, ops.ErrDiskLost) {
		if err := p.session.Lose(ctx, d.ID, err); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		return nil, nil //nolint:nilnil // likewise
	}
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return own, nil
}

// readGuards works out the guard of each disk, once, before anything is decided, from rows and events,
// so that no crash can lose a hold (specs.md §15.6).
func (p *pass) readGuards(ctx context.Context) error {
	for i := range p.config.Disks {
		d := &p.config.Disks[i]
		read, err := catalog.ReadGuard(ctx, p.db, d.ID, p.now, p.window.Nanoseconds())
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if !p.apply {
			if err := p.countUnwritten(ctx, d.ID, read); err != nil {
				return err
			}
		}
		wishes, err := p.findWishes(ctx, d)
		if err != nil {
			return err
		}
		state := &guardState{read: read, wishes: wishes}
		p.guards[d.ID] = state
		n := int64(len(wishes))
		// Nothing counts against the copies of a disk with nothing gone and nothing wished: they are not counted.
		var rows int64
		switch {
		case read.HasGuard:
			if rows, err = catalog.CopiesBefore(ctx, p.db, d.ID, read.GuardNS); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		case read.Gone+read.Events+n > 0:
			if rows, err = p.rowsOf(ctx, d.ID); err != nil {
				return err
			}
		}
		state.tripped = solver.GuardTrips(&solver.GuardInput{
			Count: read.Gone + read.Events + n, CountPercent: read.GoneLeft + read.EventsLeft + n,
			Rows: rows + read.Events, Files: p.files, Percent: p.percent,
		})
		if !state.tripped {
			// The hold ended by itself: the next trip is another one, with its own event.
			if p.apply && read.HasGuard {
				if err := catalog.WriteGuardClear(ctx, p.db, d.ID, p.now); err != nil {
					return fmt.Errorf("catalog: %w", err)
				}
			}
			continue
		}
		p.out.Guards = append(p.out.Guards, GuardLine{Disk: d.ID, Kind: GuardDeletions, Count: read.Gone + read.Events + n})
		if read.Gone > 0 || n > 0 {
			p.out.Held = true
		}
		if p.apply && !read.HasGuard {
			if err := catalog.WriteGuardEvent(ctx, p.db, d.ID, p.now); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		}
	}
	return nil
}

// countUnwritten counts what a dry run saw and did not record, as a real pass would read it from the
// rows it wrote. The rows of a disk not walked stand, as a copy this walk did not reach does.
func (p *pass) countUnwritten(ctx context.Context, id string, read *catalog.GuardRead) error {
	if !p.walked[id] {
		return nil
	}
	read.Gone, read.GoneLeft = 0, 0
	for i := range p.seen[id] {
		s := &p.seen[id][i]
		if s.Verdict != solver.VerdictGone && s.Verdict != solver.VerdictUnknown {
			continue
		}
		f, err := catalog.FileByID(ctx, p.db, s.FileID)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if f == nil {
			continue
		}
		c := copyOn(f, id)
		if s.Verdict == solver.VerdictUnknown && c.GoneKind.String != string(solver.VerdictGone) {
			continue
		}
		gone := p.goneSince(c, s.Verdict)
		if c.OldPath != nil || (read.UpTo.Valid && gone <= read.UpTo.Int64) || hasMoved(p, f) || slices.ContainsFunc(f.Copies, func(o catalog.CopyRow) bool { return p.isMovedOutOfReach(&o, p.verdictOf(f, &o).Verdict) }) {
			continue
		}
		read.Gone++
		if slices.ContainsFunc(f.Copies, func(o catalog.CopyRow) bool { return o.Disk != id && p.isLeft(f, &o) }) {
			read.GoneLeft++
		}
	}
	return nil
}

// isLeft says that a copy is something left beside one gone. Where a look at its disk could not get to it, its row
// stands, as this look wrote nothing of it.
func (p *pass) isLeft(f *catalog.FileRow, c *catalog.CopyRow) bool {
	v := p.verdictOf(f, c).Verdict
	if v == solver.VerdictUnknown && p.looked(c.Disk) {
		return !c.GoneKind.Valid
	}
	return !isGoneLike(v)
}

func isGoneLike(v solver.CopyVerdict) bool {
	return v == solver.VerdictGone || v == solver.VerdictUncertain || v == solver.VerdictOwn
}

// findWishes gives the files whose copy on a disk a refused rename would send to the trash, as this pass
// found them (specs.md §15.3, §15.12).
func (p *pass) findWishes(ctx context.Context, d *config.Disk) (map[int64]bool, error) {
	wishes := map[int64]bool{}
	if !p.looked(d.ID) {
		return wishes, nil
	}
	err := eachRefused(ctx, p.db, d, p.leavingOn(d.ID), func(r *catalog.RenameRow, f *catalog.FileRow) {
		if watch.SeenOf(p.seen[d.ID], r.FileID) == nil && p.goodAtTarget(f, d.ID) {
			wishes[r.FileID] = true
		}
	})
	if err != nil {
		return nil, err
	}
	return wishes, nil
}

// leavingOn gives the folders of a disk found renamed and not yet decided, or found at neither name: names that are
// on their way out.
func (p *pass) leavingOn(disk string) []string {
	list := slices.Clone(p.unfound[disk])
	for _, m := range p.moves[disk] {
		list = append(list, m.From)
	}
	return list
}

func (p *pass) heldBy(disk string, goneNS int64) bool {
	g := p.guards[disk]
	if g == nil || !g.tripped {
		return false
	}
	return !g.read.UpTo.Valid || goneNS > g.read.UpTo.Int64
}

// verdictOf says what this pass found of one copy. A disk lost since its walk says nothing any more, and one spared
// holds its copies as their rows say.
func (p *pass) verdictOf(f *catalog.FileRow, c *catalog.CopyRow) *watch.CopySeen {
	if !p.looked(c.Disk) {
		return &watch.CopySeen{FileID: f.ID, Verdict: solver.VerdictUnknown}
	}
	if s := watch.SeenOf(p.seen[c.Disk], f.ID); s != nil {
		return s
	}
	return &watch.CopySeen{FileID: f.ID, Verdict: solver.VerdictThere}
}

// isMovedOutOfReach says that a copy's row says it moved, and that a look at its disk could not get there.
func (p *pass) isMovedOutOfReach(c *catalog.CopyRow, v solver.CopyVerdict) bool {
	return v == solver.VerdictUnknown && p.walked[c.Disk] && c.GoneKind.String == string(solver.VerdictMoved)
}

// goneSince says what the clock of a copy says, as this pass's look records it, which a dry run does not write.
func (p *pass) goneSince(c *catalog.CopyRow, v solver.CopyVerdict) int64 {
	if !c.GoneNS.Valid || (v == solver.VerdictGone && c.GoneKind.String == string(solver.VerdictMoved)) {
		return p.now
	}
	return c.GoneNS.Int64
}

func (p *pass) isDue(c *catalog.CopyRow) bool {
	return !p.apply || (c.GoneNS.Valid && c.GoneNS.Int64 <= p.now-p.debounce.Nanoseconds())
}

func (p *pass) propFile(f *catalog.FileRow, follow string, positions map[string]int) solver.PropFile {
	file := solver.PropFile{Path: f.Path, State: solver.State(f.State), ConflictHeld: f.ConflictHeld, Follow: follow}
	for i := range f.Copies {
		c := &f.Copies[i]
		// A pending copy is nothing on its disk yet: it neither moves nor goes, and its job finds the
		// file's path when it runs (specs.md §14.1, §14.2).
		if c.State == string(solver.CopyPending) {
			continue
		}
		s := p.verdictOf(f, c)
		gone := p.goneSince(c, s.Verdict)
		verdict, due := s.Verdict, p.isDue(c)
		// What becomes of its file waits for a look that can get there, as the guard does not count what is gone
		// beside it, and a deletion would go around that (specs.md §15.6).
		if p.isMovedOutOfReach(c, s.Verdict) {
			verdict, due = solver.VerdictUncertain, false
		}
		file.Copies = append(file.Copies, solver.PropCopy{
			Disk: c.Disk, Position: positions[c.Disk], Verdict: verdict, Target: s.Target, Due: due, GoneNS: gone,
			CreatedBy: c.CreatedBy, FirstSeenNS: c.FirstSeenNS, Verified: c.VerifiedNS.Valid, OldPath: c.OldPath,
			GuardHeld: p.heldBy(c.Disk, gone), Size: c.Size.Int64, ThroughView: p.wentThroughView(c.Disk, c.FileKey.String),
		})
	}
	return file
}

func (p *pass) positions(ctx context.Context) (map[string]int, error) {
	rows, err := catalog.Disks(ctx, p.db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out := make(map[string]int, len(rows))
	for i := range rows {
		out[rows[i].ID] = rows[i].Position
	}
	return out, nil
}

// decide goes over the files with a copy whose name no longer holds it, a page at a time. Files that
// hold each other's target come together, at the turn of the first (specs.md §15.12).
func (p *pass) decide(ctx context.Context) error {
	positions, err := p.positions(ctx)
	if err != nil {
		return err
	}
	if p.running, err = catalog.RunningFiles(ctx, p.db); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if !p.apply {
		return p.decideUnwritten(ctx, positions)
	}
	reader := catalog.NewFileReader(p.db)
	for {
		page, err := catalog.DueFiles(ctx, reader)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return nil
		}
		page = slices.DeleteFunc(page, func(f catalog.FileRow) bool { return p.pulled[f.ID] })
		if len(page) == 0 {
			continue
		}
		if err := p.decidePage(ctx, page, positions); err != nil {
			return err
		}
	}
}

// decideUnwritten goes over the files a dry run saw: no clock says so in the catalog, so it reads every
// file, a page at a time, and keeps none it does not need.
func (p *pass) decideUnwritten(ctx context.Context, positions map[string]int) error {
	p.undecided = map[int64]bool{}
	for _, list := range p.seen {
		for i := range list {
			if v := list[i].Verdict; v == solver.VerdictMoved || isGoneLike(v) {
				p.undecided[list[i].FileID] = true
			}
		}
	}
	if len(p.undecided) == 0 {
		return nil
	}
	reader := catalog.NewFileReader(p.db)
	for {
		page, err := reader.Next(ctx)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return nil
		}
		page = slices.DeleteFunc(page, func(f catalog.FileRow) bool { return !p.undecided[f.ID] })
		if len(page) == 0 {
			continue
		}
		for i := range page {
			delete(p.undecided, page[i].ID)
		}
		if err := p.decidePage(ctx, page, positions); err != nil {
			return err
		}
	}
}

func (p *pass) decidePage(ctx context.Context, page []catalog.FileRow, positions map[string]int) error {
	in := &solver.PropInput{Direct: p.direct}
	byPath := make(map[string]*catalog.FileRow, len(page))
	var queue []*catalog.FileRow
	for i := range page {
		byPath[page[i].Path] = &page[i]
		queue = append(queue, &page[i])
	}
	// What holds a target comes along, as it may be leaving it.
	for i := 0; i < len(queue); i++ {
		for _, t := range targetsOf(p, queue[i]) {
			if byPath[t] != nil {
				continue
			}
			holder, err := catalog.FileAt(ctx, p.db, t)
			if err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			if holder == nil {
				continue
			}
			switch {
			case !hasMoved(p, holder), p.stayed[holder.ID]:
				in.Taken = append(in.Taken, t)
			case !p.apply && !p.undecided[holder.ID]:
				// Decided already: a dry run's rows do not say that it left.
			default:
				p.pulled[holder.ID] = true
				delete(p.undecided, holder.ID)
				byPath[t] = holder
				queue = append(queue, holder)
			}
		}
	}
	rows := make(map[string]*catalog.FileRow, len(queue))
	for _, f := range queue {
		file := p.propFile(f, "", positions)
		// A file with a job running waits for its decisions: what that job makes may be at its target, and only the
		// reprise of its operation can say (specs.md §14.1, §15.12).
		file.Running = p.running[f.ID]
		p.holdLanding(&file)
		in.Files = append(in.Files, file)
		rows[f.Path] = f
	}
	decisions, err := solver.Decide(in)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	return p.applyDecisions(ctx, decisions, rows)
}

func targetsOf(p *pass, f *catalog.FileRow) []string {
	var list []string
	for i := range f.Copies {
		if s := p.verdictOf(f, &f.Copies[i]); s.Verdict == solver.VerdictMoved && s.Target != f.Path {
			list = append(list, s.Target)
		}
	}
	return list
}

func hasMoved(p *pass, f *catalog.FileRow) bool {
	return slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return p.verdictOf(f, &c).Verdict == solver.VerdictMoved })
}

func (p *pass) applyDecisions(ctx context.Context, decisions []solver.PropDecision, rows map[string]*catalog.FileRow) error {
	var changes []catalog.FileChange
	p.restores = p.restores[:0]
	for i := range decisions {
		d := &decisions[i]
		switch d.Kind {
		case solver.PropRename, solver.PropDeletion, solver.PropKept, solver.PropGone:
			change := p.changeOf(d, rows[d.Path])
			if !p.apply {
				if err := p.planDecided(ctx, d, rows[d.Path], &change); err != nil {
					return err
				}
			}
			changes = append(changes, change)
		case solver.PropHeld:
			p.out.Held = true
		case solver.PropPathTaken:
			p.stayed[rows[d.Path].ID] = true
		case solver.PropNone, solver.PropWait:
		}
		p.noteStuck(d, rows[d.Path])
		if p.keepsHeld(rows[d.Path]) {
			p.out.Held = true
		}
	}
	if p.apply && len(changes) > 0 {
		if err := catalog.ApplyDecisions(ctx, p.db, p.now, changes); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		p.session.MarkChanged()
		p.machine.Checkpoint("decided")
		// A copy gone with deletes.direct = "restore" is made again in its place (specs.md §15.5). Its row went with the
		// decision: a crash here leaves the file below its level, which the plan places as any. With replication off,
		// nothing is copied, and none is queued, as the plan queues none.
		if enabled, _ := p.config.Effective()["replication.enabled"].(bool); enabled {
			if err := catalog.QueueReplicate(ctx, p.db, p.now, p.restores); err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
		}
	}
	for i := range decisions {
		p.reportDecision(&decisions[i], rows[decisions[i].Path])
	}
	return nil
}

// keepsHeld says that a file has a copy gone the guard holds: its row stays for that, whatever the file does.
func (p *pass) keepsHeld(f *catalog.FileRow) bool {
	return slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
		v := p.verdictOf(f, &c).Verdict
		return v == solver.VerdictGone && p.heldBy(c.Disk, p.goneSince(&c, v))
	})
}

// noteStuck remembers the copies that moved to a name another file holds, as decided: no pass takes their
// rows from where they sit until that name is free.
func (p *pass) noteStuck(d *solver.PropDecision, f *catalog.FileRow) {
	for i := range d.Copies {
		a := &d.Copies[i]
		taken := d.Kind == solver.PropPathTaken || (d.Kind == solver.PropRename && (a.Action == solver.ActPending || a.Action == solver.ActClear))
		if !taken || p.verdictOf(f, copyOn(f, a.Disk)).Verdict != solver.VerdictMoved {
			continue
		}
		if p.stuck[a.Disk] == nil {
			p.stuck[a.Disk] = map[int64]bool{}
		}
		p.stuck[a.Disk][f.ID] = true
	}
}

// reportDecision says what was decided about a file: one line, and one more for each copy that went its
// own way, as nothing else would say so.
func (p *pass) reportDecision(d *solver.PropDecision, f *catalog.FileRow) {
	switch d.Kind {
	case solver.PropRename:
		p.decided(Decision{Path: d.Path, Kind: "renamed", To: d.Target})
		var told []string
		for i := range d.Copies {
			a := &d.Copies[i]
			c := copyOn(f, a.Disk)
			switch s := p.verdictOf(f, c); {
			case a.Action == solver.ActOtherTarget && !slices.Contains(told, a.Target):
				told = append(told, a.Target)
				p.decided(Decision{Path: d.Path, Kind: "renamed", To: a.Target})
			case (a.Action == solver.ActPending || a.Action == solver.ActClear) && s.Verdict == solver.VerdictMoved:
				p.decided(Decision{Path: d.Path, Kind: string(solver.PropPathTaken), To: s.Target})
			}
		}
	case solver.PropPathTaken:
		p.decided(Decision{Path: d.Path, Kind: string(solver.PropPathTaken), To: d.Target})
	case solver.PropDeletion:
		p.decided(Decision{Path: d.Path, Kind: "deleted"})
	case solver.PropKept, solver.PropGone:
		p.decided(Decision{Path: d.Path, Kind: string(d.Kind)})
	case solver.PropNone, solver.PropWait, solver.PropHeld:
	}
}

// changeOf says what a decision does to the rows of a file (specs.md §15.12).
func (p *pass) changeOf(d *solver.PropDecision, f *catalog.FileRow) catalog.FileChange {
	change := catalog.FileChange{ID: f.ID, Path: f.Path, DropHash: d.DropHash, ClearConflict: d.ClearConflict, Src: d.Src, Reason: reasonDeleted}
	if d.Kind == solver.PropRename {
		// A user acted on a copy, so a deletion no longer stands. A file that only follows a folder has
		// had nothing done to it, and what was decided about it stands.
		change.DropJobs = hasMoved(p, f)
		if d.Target != f.Path {
			change.NewPath = d.Target
		}
	}
	change.DropFile = d.Kind == solver.PropGone
	followers := 0
	for i := range d.Copies {
		if d.Copies[i].Action == solver.ActFollow {
			followers++
		}
	}
	var owns []catalog.OwnFile
	for i := range d.Copies {
		a := &d.Copies[i]
		c := copyOn(f, a.Disk)
		s := p.verdictOf(f, c)
		row := catalog.RowChange{Disk: a.Disk}
		switch a.Action {
		case solver.ActFollow:
			followOf(&row, c, s, followers < len(d.Copies))
		case solver.ActPending:
			row.Pending = c.Place(f)
		case solver.ActClear:
			row.Clear = true
			if c.OldPath != nil {
				row.ClearedFrom = *c.OldPath
				p.cleared[a.Disk] = append(p.cleared[a.Disk], *c.OldPath)
			}
		case solver.ActKeepGone:
			row.KeepGone = c.Place(f)
		case solver.ActTrash:
			row.Trash = true
		case solver.ActDrop:
			row.Drop = true
			p.dropEvents(d, c, s, &row, &change)
		case solver.ActRestore:
			// No deletion is propagated: its row goes with no copy-gone, and a copy is queued in its place.
			row.Drop = true
			size := f.Size
			p.restores = append(p.restores, catalog.Queue{FileID: f.ID, Src: d.Src, Dst: a.Disk, Priority: int64(p.config.Policy(f.Path, &size).Priority)})
		case solver.ActOwnFile:
			// One file, for the copies of one size. Another has no place in it: its row goes, and the next walk adds
			// it with the checks of a copy not yet in the catalog.
			if at := slices.IndexFunc(owns, func(o catalog.OwnFile) bool { return o.Size == c.Size.Int64 }); at >= 0 {
				owns[at].Disks = append(owns[at].Disks, a.Disk)
				continue
			}
			owns = append(owns, catalog.OwnFile{Path: f.Path, Disks: []string{a.Disk}, Size: c.Size.Int64, MtimeNS: c.MtimeNS.Int64, First: c.FirstSeenNS})
			continue
		case solver.ActOtherTarget:
			followed := catalog.RowChange{Disk: a.Disk}
			followOf(&followed, c, s, true)
			// One file, for the copies that moved to one name. Another size has no place in it: its row goes,
			// and the next walk adds it with the checks of a copy not yet in the catalog.
			if at := slices.IndexFunc(change.Own, func(o catalog.OwnFile) bool { return o.Path == a.Target && o.Size == c.Size.Int64 }); at >= 0 {
				change.Own[at].Disks = append(change.Own[at].Disks, a.Disk)
				change.Own[at].Follow = append(change.Own[at].Follow, followed)
				continue
			}
			change.Own = append(change.Own, catalog.OwnFile{
				// In a conflict, or one a corrupt file holds, the hash may be another version's: it goes with a copy read
				// against it, and with no other (specs.md §4).
				Path: a.Target, Disks: []string{a.Disk}, WithHash: (f.State != string(solver.StateConflict) && !f.ConflictHeld) || c.VerifiedNS.Valid,
				Size: c.Size.Int64, MtimeNS: c.MtimeNS.Int64, First: f.FirstSeenNS,
				Follow: []catalog.RowChange{followed},
			})
			continue
		case solver.ActStay:
			continue
		}
		change.Rows = append(change.Rows, row)
	}
	change.Own = append(change.Own, owns...)
	return change
}

// dropEvents says what a row going writes: copy-gone counts for the guard, and copy-kept is a warning.
// A copy found in its own folders writes neither: it was an operation of Atoll's, not a user's action.
func (p *pass) dropEvents(d *solver.PropDecision, c *catalog.CopyRow, s *watch.CopySeen, row *catalog.RowChange, change *catalog.FileChange) {
	if d.Kind == solver.PropRename || s.Verdict == solver.VerdictOwn {
		return
	}
	if s.Verdict == solver.VerdictGone && c.OldPath == nil {
		row.CountGone = true
		row.GoneNS = c.GoneNS.Int64
		row.NothingLeft = d.Kind == solver.PropGone
		if !c.GoneNS.Valid {
			row.GoneNS = p.now
		}
	}
	if d.Kind == solver.PropKept && !change.Kept {
		change.Kept = true
		change.KeptDisk = c.Disk
	}
}

func followOf(row *catalog.RowChange, c *catalog.CopyRow, s *watch.CopySeen, makeUser bool) {
	row.Follow = true
	row.FollowNlink = s.Nlink
	row.MakeUser = makeUser
	if s.SameDate {
		row.FollowCtime = sql.NullInt64{Int64: s.CtimeNS, Valid: true}
	}
	// The move hides any edit made before it: read again, a mismatch is a conflict (specs.md §15.12).
	row.Unverify = !c.CtimeNS.Valid || c.CtimeNS.Int64 != s.CtimeNS
}

func copyOn(f *catalog.FileRow, disk string) *catalog.CopyRow {
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
	return &f.Copies[at]
}

// decideFolders says what a folder renamed means, once the copies of that disk below it are decided:
// every file below it with copies on other disks only follows it, its entries do, and the other disks
// get a job (specs.md §15.12).
func (p *pass) decideFolders(ctx context.Context) error {
	positions, err := p.positions(ctx)
	if err != nil {
		return err
	}
	if !p.apply {
		if err := p.planFolderJobs(ctx); err != nil {
			return err
		}
	}
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		for _, move := range movesInOrder(p.moves[id]) {
			if err := p.decideFolder(ctx, id, move, positions); err != nil {
				return err
			}
		}
	}
	return nil
}

// movesInOrder gives the folders a disk found renamed in the order of their jobs: one that lands in another's old name
// came after that one left, and must not read as having been in it.
func movesInOrder(list []watch.FolderMove) []watch.FolderMove {
	jobs := make([]catalog.FolderJob, len(list))
	for i, m := range list {
		jobs[i] = catalog.FolderJob{From: m.From, To: m.To}
	}
	ordered, cycle := folderOrder(jobs)
	moves := make([]watch.FolderMove, 0, len(list))
	for _, j := range append(ordered, cycle...) {
		moves = append(moves, watch.FolderMove{From: j.From, To: j.To})
	}
	return moves
}

// readFolders says of each folder found renamed whether copies came with it, before a decision of this pass gives
// a row a place that says nothing of where it came from.
func (p *pass) readFolders(ctx context.Context) error {
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		for _, move := range p.moves[id] {
			real, err := p.isRealRename(ctx, id, move)
			if err != nil {
				return err
			}
			p.realMoves[folderKey{id, move.From}] = real
		}
	}
	return nil
}

// readHeld works out, before the files are decided, the folders found renamed that wait for what no decision of this
// pass ends: a clock of a copy below them on another disk, the guard, or a look that cannot get there. A copy that
// moved to a name at or below the old name of one of them waits for it, as it would otherwise take that name before
// the one that waits leaves it; a folder renamed to there waits with its copies. Only a wait of that kind holds
// anything back, so that what waits never waits for itself (specs.md §15.12).
func (p *pass) readHeld(ctx context.Context) error {
	positions, err := p.positions(ctx)
	if err != nil {
		return err
	}
	held := map[string][]watch.FolderMove{}
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		for _, move := range p.moves[id] {
			if !p.realMoves[folderKey{id, move.From}] {
				continue
			}
			waits, err := p.folderWaits(ctx, id, move, positions)
			if err != nil {
				return err
			}
			if waits {
				held[id] = append(held[id], move)
			}
		}
	}
	p.held = held
	return nil
}

// landsIn says whether a name is at or below the old name of one of those folders.
func landsIn(folders []watch.FolderMove, name string) bool {
	return slices.ContainsFunc(folders, func(h watch.FolderMove) bool { return isAtOrBelow(name, h.From) })
}

// holdLanding keeps a copy that moved to a name at or below the old name of a folder held on its disk from being due:
// its file waits, as the copies below that folder's old name must leave it first (readHeld).
func (p *pass) holdLanding(f *solver.PropFile) {
	for i := range f.Copies {
		c := &f.Copies[i]
		if c.Verdict == solver.VerdictMoved && landsIn(p.held[c.Disk], c.Target) {
			c.Due = false
		}
	}
}

// topMoves leaves out the folders that only moved with a folder above them.
func topMoves(list []watch.FolderMove) []watch.FolderMove {
	return slices.DeleteFunc(slices.Clone(list), func(m watch.FolderMove) bool { return isBelowAnother(list, m) })
}

// isBelowAnother says that a folder only moved with a folder above it.
func isBelowAnother(list []watch.FolderMove, move watch.FolderMove) bool {
	return slices.ContainsFunc(list, func(m watch.FolderMove) bool {
		return strings.HasPrefix(move.From, m.From+"/") && strings.HasPrefix(move.To, m.To+"/") &&
			strings.TrimPrefix(move.From, m.From) == strings.TrimPrefix(move.To, m.To)
	})
}

func (p *pass) decideFolder(ctx context.Context, disk string, move watch.FolderMove, positions map[string]int) error {
	// The copies that are there: one yet to make was never below the old name, and would hold the folder for good.
	waiting, err := catalog.MadeBelow(ctx, p.db, disk, move.From)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if !p.apply {
		waiting -= p.planOf(disk).emptied[move.From]
	}
	if waiting > 0 {
		left, err := p.cannotLeave(ctx, disk, move.From)
		if err != nil {
			return err
		}
		waiting -= left
	}
	// A folder renamed inside it that waits keeps its rows where they are: its entries would carry them away.
	if waiting > 0 || slices.ContainsFunc(p.waited[disk], func(f string) bool { return strings.HasPrefix(f, move.From+"/") }) {
		p.waited[disk] = append(p.waited[disk], move.From)
		return nil
	}
	if !p.realMoves[folderKey{disk, move.From}] {
		if !p.apply {
			p.moveFolderJobs(move)
			return nil
		}
		if err := catalog.FolderRow(ctx, p.db, disk, move.From, move.To, nil); err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		return nil
	}
	// Before anything follows: what goes alone, its operation can no longer carry.
	waits, err := p.folderWaits(ctx, disk, move, positions)
	if err != nil {
		return err
	}
	if waits {
		p.waited[disk] = append(p.waited[disk], move.From)
		return nil
	}
	if err := p.follow(ctx, disk, move, positions); err != nil {
		return err
	}
	others, err := catalog.DisksBelow(ctx, p.db, move.From, disk)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	// A copy found back where it was stays there: an operation would carry it away, as of a job already queued.
	others = slices.DeleteFunc(others, func(id string) bool {
		return slices.ContainsFunc(p.cleared[id], func(place string) bool { return strings.HasPrefix(place, move.From+"/") })
	})
	if !p.apply {
		p.moveFolderJobs(move)
		for _, other := range others {
			plan := p.planOf(other)
			if !slices.ContainsFunc(plan.folders, func(j catalog.FolderJob) bool { return j.From == move.From && j.To == move.To }) {
				plan.folders = append(plan.folders, catalog.FolderJob{From: move.From, To: move.To})
			}
		}
		return nil
	}
	if err := catalog.FolderRow(ctx, p.db, disk, move.From, move.To, others); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	p.session.MarkChanged()
	return nil
}

// cannotLeave counts the copies of a disk below a folder that no pass can take from there, and so hold it back
// for nothing: one replaced, which nothing decides, one moved to a name another file holds, and one this pass
// decided in from elsewhere, which never sat in the folder renamed (specs.md §15.12).
func (p *pass) cannotLeave(ctx context.Context, disk, folder string) (int64, error) {
	var count int64
	list := p.seen[disk]
	for i := range list {
		s := &list[i]
		stays := s.Verdict == solver.VerdictReplaced || p.stuck[disk][s.FileID]
		if !stays && !strings.HasPrefix(s.Target, folder+"/") {
			continue
		}
		f, err := catalog.FileByID(ctx, p.db, s.FileID)
		if err != nil {
			return 0, fmt.Errorf("catalog: %w", err)
		}
		at := copyAt(f, disk)
		if at < 0 && !stays {
			// One that went its own way left that file for one of its own, at its target.
			if f, err = catalog.FileAt(ctx, p.db, s.Target); err != nil {
				return 0, fmt.Errorf("catalog: %w", err)
			}
			at = copyAt(f, disk)
		}
		if at < 0 {
			continue
		}
		// One still on its way there has a clock, and its row says where it was.
		if place := f.Copies[at].Place(f); strings.HasPrefix(place, folder+"/") && (stays || place == s.Target) {
			count++
		}
	}
	return count, nil
}

// copyAt says where in a file row, if there is one, the copy of a disk is.
func copyAt(f *catalog.FileRow, disk string) int {
	if f == nil {
		return -1
	}
	return slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
}

// isRealRename says whether a copy moved with the folder: an empty folder renamed says nothing about
// what other disks hold below it, and a copy moved in from elsewhere changes nothing of that. A walk gives
// what is new below the new name no row until this is decided, so a copy that sits there came with it, as does
// one found there that sat below the old name, whose row may never follow (specs.md §15.10, §15.12).
func (p *pass) isRealRename(ctx context.Context, disk string, move watch.FolderMove) (bool, error) {
	count, err := catalog.SettledBelow(ctx, p.db, disk, move.To)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	if count > 0 {
		return true, nil
	}
	list := p.seen[disk]
	for i := range list {
		s := &list[i]
		// Only a copy that moved has a target.
		if !strings.HasPrefix(s.Target, move.To+"/") {
			continue
		}
		f, err := catalog.FileByID(ctx, p.db, s.FileID)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if f == nil {
			continue
		}
		at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
		if at >= 0 && strings.HasPrefix(f.Copies[at].Place(f), move.From+"/") {
			return true, nil
		}
	}
	return false, nil
}

// followers works out what is asked of the files of a page below a folder renamed with copies on other disks
// only. A file with a copy that moved was decided already, on what that copy did, and until then it says that
// the folder waits: its operation would carry the other copies, and the file would read as two. In a dry run a
// copy gone there still has its row, which a real pass decided away.
func (p *pass) followers(ctx context.Context, page []catalog.FileRow, disk string, move watch.FolderMove, positions map[string]int) (in *solver.PropInput, rows map[string]*catalog.FileRow, waits bool, err error) {
	in = &solver.PropInput{Direct: p.direct}
	rows = make(map[string]*catalog.FileRow, len(page))
	for i := range page {
		f := &page[i]
		held := slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
			return c.Disk == disk && (p.apply || !isGoneLike(p.verdictOf(f, &c).Verdict))
		})
		if held {
			continue
		}
		if hasMoved(p, f) {
			// A clock still runs: a row that followed in this pass has none, whatever the walk found of it.
			waits = waits || slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
				v := p.verdictOf(f, &c).Verdict
				return c.GoneNS.Valid && (v == solver.VerdictMoved || isGoneLike(v)) && !p.isDue(&c)
			})
			continue
		}
		// What the guard counts on a disk not looked at: its row stands, and with a new path would leave that count.
		waits = waits || slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool {
			return c.GoneNS.Valid && c.GoneKind.String == string(solver.VerdictGone) && p.verdictOf(f, &c).Verdict == solver.VerdictUnknown && p.heldBy(c.Disk, c.GoneNS.Int64)
		})
		// Gone from every disk: decided already, and nothing is left to follow, as a dry run's rows do not say, unless
		// the guard holds it, as then a real pass has its rows too.
		if !p.apply && !slices.ContainsFunc(f.Copies, func(c catalog.CopyRow) bool { return !isGoneLike(p.verdictOf(f, &c).Verdict) }) && !p.keepsHeld(f) {
			continue
		}
		target := move.To + strings.TrimPrefix(f.Path, move.From)
		holder, err := catalog.FileAt(ctx, p.db, target)
		if err != nil {
			return nil, nil, false, fmt.Errorf("catalog: %w", err)
		}
		if holder != nil {
			in.Taken = append(in.Taken, target)
		}
		in.Files = append(in.Files, p.propFile(f, target, positions))
		rows[f.Path] = f
	}
	return in, rows, waits, nil
}

// folderWaits says whether a file below a folder renamed has a copy not yet due, a debounce at most, as what
// becomes of it is decided at the path it has, or a copy the guard holds, until it is answered. It only reads.
func (p *pass) folderWaits(ctx context.Context, disk string, move watch.FolderMove, positions map[string]int) (bool, error) {
	reader := catalog.NewFileReader(p.db)
	for {
		page, err := catalog.FilesBelow(ctx, reader, move.From)
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return false, nil
		}
		in, _, waits, err := p.followers(ctx, page, disk, move, positions)
		if err != nil {
			return false, err
		}
		if waits {
			return true, nil
		}
		decisions, err := solver.Decide(in)
		if err != nil {
			return false, fmt.Errorf("%w", err)
		}
		if slices.ContainsFunc(decisions, func(d solver.PropDecision) bool { return d.Kind == solver.PropWait || d.Kind == solver.PropHeld }) {
			return true, nil
		}
	}
}

// follow gives a new path to each file below a folder renamed with copies on other disks only.
func (p *pass) follow(ctx context.Context, disk string, move watch.FolderMove, positions map[string]int) error {
	reader := catalog.NewFileReader(p.db)
	for {
		page, err := catalog.FilesBelow(ctx, reader, move.From)
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		if len(page) == 0 {
			return nil
		}
		in, rows, _, err := p.followers(ctx, page, disk, move, positions)
		if err != nil {
			return err
		}
		if len(in.Files) == 0 {
			continue
		}
		decisions, err := solver.Decide(in)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if err := p.applyDecisions(ctx, decisions, rows); err != nil {
			return err
		}
	}
}

// finish works out each file again, and says what still waits.
func (p *pass) finish(ctx context.Context) error {
	if !p.apply {
		return nil
	}
	// What the walks found has served, and working out each file again reads them all.
	p.seen, p.moves = nil, nil
	if err := p.session.RemoveEmptied(ctx); err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := p.takeInViewOps(ctx); err != nil {
		return err
	}
	if err := p.linkGuard(ctx); err != nil {
		return err
	}
	if !p.settled {
		if err := p.settleAgain(ctx); err != nil {
			return err
		}
	}
	count, next, err := catalog.Waiting(ctx, p.db, p.debounce.Nanoseconds(), p.now)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	// The copies that wait for settle count too, so that a timer knows when to run the next pass (§15.12).
	p.out.Waiting, p.out.NextDue = count+p.copiesWaiting, earliest(next, p.copiesDue)
	p.out.Partial = p.out.Partial || p.session.Lost()
	return nil
}

// planOnly works out what the plans of this pass take (specs.md §15.13): every file, unless the pass was given a scope,
// walked no disk whole, and every disk is online or in standby, as a disk in any other state changes what its files
// are, with time too; otherwise the files at the names its looks read, and those catalog.PlanSet adds.
func (p *pass) planOnly(ctx context.Context) error {
	p.only = nil
	if p.scope == nil {
		return nil
	}
	for id := range p.walked {
		if !p.partial[id] {
			return nil
		}
	}
	list, err := catalog.Disks(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i := range list {
		if list[i].State != string(solver.DiskOnline) && list[i].State != string(solver.DiskStandby) {
			return nil
		}
	}
	p.only = &watch.Only{Names: maps.Clone(p.names)}
	if p.only.Names == nil {
		p.only.Names = map[string][]string{}
	}
	return nil
}

// ranFile records a file whose copy or transfer ran in this pass.
func (p *pass) ranFile(id int64) {
	if p.ran == nil {
		p.ran = map[int64]bool{}
	}
	p.ran[id] = true
}

// settleAgain works out each file again once copies ran, as the plan of this pass took them: every file, or what may
// have changed, with the files whose copy or transfer ran.
func (p *pass) settleAgain(ctx context.Context) error {
	if p.only == nil {
		if err := p.session.Settle(ctx); err != nil {
			return fmt.Errorf("%w", err)
		}
		return nil
	}
	only := &watch.Only{Names: p.only.Names, IDs: p.ran}
	if err := watch.SettleOnly(ctx, p.db, p.config, p.now, only); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// rowsOf counts the copies a disk had before this pass's look: what the look adds says nothing of how many it had
// (specs.md §15.6). Counted once, when a guard needs them.
func (p *pass) rowsOf(ctx context.Context, id string) (int64, error) {
	if n, ok := p.rows[id]; ok {
		return n, nil
	}
	n, err := catalog.CopyCount(ctx, p.db, id)
	if err != nil {
		return 0, fmt.Errorf("catalog: %w", err)
	}
	p.rows[id] = n - p.added[id]
	return p.rows[id], nil
}

// findWaits finds the disks where this pass's look found what the catalog does not hold yet, which a later look must
// see again (specs.md §15.13): a changed copy whose row does not hold the change time its walk found, as one that waits
// for settle, for its read or for an answer to a guard, or one replaced by what is no file; a folder found renamed and
// left waiting, or found at neither name. It runs once the work is done, before the plan lets go of what the walks found.
func (p *pass) findWaits(ctx context.Context) error {
	if !p.apply {
		return nil
	}
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		if !p.walked[id] {
			continue
		}
		if p.partial[id] {
			// What waits there is at names this look read: the next pass reads those again, not the whole disk.
			names, err := p.waitingNames(ctx, id)
			if err != nil {
				return err
			}
			if len(names) > 0 {
				if p.out.Again == nil {
					p.out.Again = map[string][]watch.Named{}
				}
				p.out.Again[id] = names
			}
			continue
		}
		waits := len(p.waited[id])+len(p.held[id])+len(p.unfound[id]) > 0
		for j := 0; !waits && j < len(p.seen[id]); j++ {
			s := &p.seen[id][j]
			if s.Verdict != solver.VerdictEdited && s.Verdict != solver.VerdictReplaced {
				continue
			}
			f, err := catalog.FileByID(ctx, p.db, s.FileID)
			if err != nil {
				return fmt.Errorf("catalog: %w", err)
			}
			waits = !holdsSeen(f, id, s)
		}
		if waits {
			p.out.WaitDisks = append(p.out.WaitDisks, id)
		}
	}
	return nil
}

// waitingNames gives the names a look limited to names leaves waiting on a disk, which the next pass looks at again
// (specs.md §15.13): where a copy was found at another name, whose old name the clock of its row brings back; a changed
// copy whose row does not hold the change time the look found; and each folder found renamed and left waiting, or found
// at neither name, at both its names.
func (p *pass) waitingNames(ctx context.Context, id string) ([]watch.Named, error) {
	var out []watch.Named
	for i := range p.seen[id] {
		s := &p.seen[id][i]
		switch s.Verdict {
		case solver.VerdictMoved:
			out = append(out, watch.Named{Path: s.Target})
		case solver.VerdictEdited, solver.VerdictReplaced:
			f, err := catalog.FileByID(ctx, p.db, s.FileID)
			if err != nil {
				return nil, fmt.Errorf("catalog: %w", err)
			}
			if f == nil || holdsSeen(f, id, s) {
				continue
			}
			at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == id })
			path := f.Path
			if at >= 0 && f.Copies[at].OldPath != nil {
				path = *f.Copies[at].OldPath
			}
			out = append(out, watch.Named{Path: path})
		case solver.VerdictUnknown, solver.VerdictThere, solver.VerdictUncertain, solver.VerdictOwn, solver.VerdictGone:
		}
	}
	for _, folder := range p.waited[id] {
		out = append(out, watch.Named{Path: folder, Folder: true})
	}
	for _, m := range p.held[id] {
		out = append(out, watch.Named{Path: m.From, Folder: true}, watch.Named{Path: m.To, Folder: true})
	}
	for _, folder := range p.unfound[id] {
		out = append(out, watch.Named{Path: folder, Folder: true})
	}
	slices.SortFunc(out, func(a, b watch.Named) int { return strings.Compare(a.Path, b.Path) })
	return slices.CompactFunc(out, func(a, b watch.Named) bool { return a.Path == b.Path && a.Folder == b.Folder }), nil
}

// holdsSeen says that a file's row on a disk holds the change time a look found of a changed copy there.
func holdsSeen(f *catalog.FileRow, disk string, s *watch.CopySeen) bool {
	if f == nil || (s.Verdict == solver.VerdictReplaced && !s.Regular) {
		return false
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == disk })
	return at >= 0 && f.Copies[at].CtimeNS.Valid && f.Copies[at].CtimeNS.Int64 == s.CtimeNS
}

// linkGuard keeps the links of the guard of each disk the pass walked in step with its copies, once everything else of
// the pass has run, and reports each link it sends to the trash as an operation (specs.md §17). With the guard off, it
// removes what a guard on left. A disk spared has its links kept in step too once the pass has read or written it, as
// it is awake then: its replicas get their links at once, rather than at its next walk.
func (p *pass) linkGuard(ctx context.Context) error {
	for i := range p.config.Disks {
		id := p.config.Disks[i].ID
		// A look limited to names read none of the links: as a disk spared, it keeps them in step once opened.
		whole := p.walked[id] && !p.partial[id]
		if !whole && (!p.spared[id] && !p.partial[id] || !p.session.Opened(id)) {
			continue
		}
		err := p.session.Guard(ctx, id, p.linkGuardOn, func(item *trash.LinkItem) error {
			p.operated(Operation{Disk: item.Disk, Kind: catalog.OpTrash, Path: item.Path, Reason: item.Reason, Outcome: item.Outcome})
			return p.sinkErr
		})
		if err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}

// refusal says whether a disk would refuse a name: its filesystem, the limit of its branch, and on NTFS
// the names it holds, but for the name except, which goes as this one comes (specs.md §15.7, §15.8).
func refusal(ctx context.Context, db *sql.DB, d *config.Disk, target, except string) (bool, error) {
	return refusalBy(d, target, func(name string) (bool, error) { return catalog.KeyTakenExcept(ctx, db, d.ID, name, except) })
}

// lastingRefusal is a refusal that a look still found: what it asks to go to the trash must not be for a name
// that is on its way out.
func lastingRefusal(ctx context.Context, db *sql.DB, d *config.Disk, target, except string, leaving []string) (bool, error) {
	return refusalBy(d, target, func(name string) (bool, error) { return catalog.KeyStaysTaken(ctx, db, d.ID, name, except, leaving) })
}

func refusalBy(d *config.Disk, target string, isTaken func(string) (bool, error)) (bool, error) {
	refused, err := refusalOfPath(d, target)
	if err != nil || refused {
		return refused, err
	}
	if d.FS != names.NTFS {
		return false, nil
	}
	parts := strings.Split(target, "/")
	for i := range parts {
		taken, err := isTaken(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return false, fmt.Errorf("catalog: %w", err)
		}
		if taken {
			return true, nil
		}
	}
	return false, nil
}

// refusalOfPath is a refusal that looks at no other name: what moves together cannot collide.
func refusalOfPath(d *config.Disk, target string) (bool, error) {
	problems, err := names.CheckPath(target, d.FS)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	fits, err := names.FitsBranch(d.Mount, d.Root, target)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	return len(problems) > 0 || !fits, nil
}
