package jobs

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/solver"
)

// modGuard is the mass-modification guard of one disk in a pass (specs.md §15.6).
type modGuard struct {
	read     *catalog.ModifyGuardRead
	count    int64 // what read counted over the window, with the decisions of this pass
	dayCount int64 // and over the day
	tripped  bool
}

// readModifyGuards works out the mass-modification guard of each disk, from events, before any modification is
// decided: one that stands holds, whatever this pass finds. Both counts come from events too, so that neither a crash
// nor a restart starts the day again.
func (p *pass) readModifyGuards(ctx context.Context) error {
	for i := range p.config.Disks {
		d := &p.config.Disks[i]
		read, err := catalog.ReadModifyGuard(ctx, p.db, d.ID, p.now, p.modWindow.Nanoseconds(), p.dayWindow.Nanoseconds())
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		g := &modGuard{read: read, count: read.Count, dayCount: read.DayCount, tripped: read.Stands}
		p.modGuards[d.ID] = g
		if g.tripped {
			p.out.Guards = append(p.out.Guards, g.line(d.ID, read.Window))
		}
	}
	return nil
}

// line is the guard tripped on the count of a window: WindowDay, or "" for its own.
func (g *modGuard) line(disk, window string) GuardLine {
	if window == WindowDay {
		return GuardLine{Disk: disk, Kind: GuardModifications, Count: g.dayCount, Window: WindowDay}
	}
	return GuardLine{Disk: disk, Kind: GuardModifications, Count: g.count}
}

// sortGuards puts the guards tripped by disk position, the mass-deletion guard first (specs.md §15.12).
func (p *pass) sortGuards(context.Context) error {
	position := make(map[string]int, len(p.config.Disks))
	for i := range p.config.Disks {
		position[p.config.Disks[i].ID] = i
	}
	slices.SortStableFunc(p.out.Guards, func(a, b GuardLine) int {
		if c := cmp.Compare(position[a.Disk], position[b.Disk]); c != 0 {
			return c
		}
		return cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind))
	})
	return nil
}

func kindOrder(kind string) int {
	if kind == GuardDeletions {
		return 0
	}
	return 1
}

// firstChanged gives the changed copy first by position, whose disk the mass-modification guard of a file is.
func firstChanged(in *solver.ModInput) *solver.ModCopy {
	var first *solver.ModCopy
	for i := range in.Copies {
		c := &in.Copies[i]
		if isChanged(c) && (first == nil || c.Position < first.Position) {
			first = c
		}
	}
	return first
}

// heldBefore says whether the mass-modification guard of a disk holds a file whose copy there changed at that
// change time, before anything of it is read: one stands, and no answer covers that change, nor any reject.
func (p *pass) heldBefore(disk string, ctimeNS int64) bool {
	g := p.modGuards[disk]
	return g != nil && g.tripped && !p.answers(g, ctimeNS) && !g.read.Rejected(ctimeNS)
}

// answers says whether an answer to a guard decides a change at that change time: its last answer, given within its
// window of the start of this run; or, in a slice of reads, that same answer when the pass that left the file to
// read found it to decide it, however long ago (specs.md §15.6). A batch that takes hours to read is asked about once.
func (p *pass) answers(g *modGuard, ctimeNS int64) bool {
	if g.read.Answers(ctimeNS) {
		return true
	}
	return p.takenUnder != 0 && p.takenUnder == g.read.AnswerID && g.read.UpTo.Valid && ctimeNS <= g.read.UpTo.Int64
}

// answerOver gives the answer to the guard of a disk that decides a change at that change time now, 0 with none.
func (p *pass) answerOver(disk string, ctimeNS int64) int64 {
	if g := p.modGuards[disk]; g != nil && g.read.Answers(ctimeNS) {
		return g.read.AnswerID
	}
	return 0
}

// guardModification asks the mass-modification guard about a decision that would make copies stale (specs.md §15.6):
// an answer decides it, a reject making it a conflict, and any reject that covers it, whatever answer came after it and
// however long ago (flaw 27 of conformance/models/README.md, the maintainer's remedy reject-kept); otherwise it counts,
// over the window and over the day, and the one that would trip either waits, as heldBefore holds every one after it.
// The window is asked first: a burst that trips both is the window's. It says whether the decision goes, and whether a
// confirm answered it.
func (p *pass) guardModification(ctx context.Context, d *solver.ModDecision, ctimeNS int64) (goes, answered bool, err error) {
	if d.Kind != solver.ModModified || len(d.Stale) == 0 {
		return true, false, nil
	}
	disk := d.Changed[0]
	g := p.modGuards[disk]
	switch {
	case g.read.Rejected(ctimeNS), p.answers(g, ctimeNS) && g.read.Answer == catalog.EventModifyGuardReject:
		d.Kind, d.Stale, d.Hash = solver.ModConflict, nil, nil
		return true, false, nil
	case p.answers(g, ctimeNS):
		return true, true, nil
	}
	// A guard that stands held this file before anything of it was read (heldBefore): here it is clear.
	rows, err := p.rowsOf(ctx, disk)
	if err != nil {
		return false, false, err
	}
	window := ""
	switch {
	case solver.GuardTrips(&solver.GuardInput{Count: g.count + 1, CountPercent: g.count + 1, Rows: rows, Files: p.modFiles, Percent: p.modPercent}):
	case solver.GuardTrips(&solver.GuardInput{Count: g.dayCount + 1, CountPercent: g.dayCount + 1, Rows: rows, Files: p.dayFiles, Percent: p.dayPercent}):
		window = WindowDay
	default:
		g.count++
		g.dayCount++
		return true, false, nil
	}
	g.tripped = true
	p.out.Held = true
	p.out.Guards = append(p.out.Guards, g.line(disk, window))
	if p.apply {
		if err := catalog.WriteModifyGuardEvent(ctx, p.db, disk, window, p.now); err != nil {
			return false, false, fmt.Errorf("catalog: %w", err)
		}
	}
	return false, false, nil
}
