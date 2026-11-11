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
	read    *catalog.ModifyGuardRead
	count   int64 // what read counted, with the decisions of this pass
	tripped bool
}

// readModifyGuards works out the mass-modification guard of each disk, from events, before any modification is
// decided: one that stands holds, whatever this pass finds.
func (p *pass) readModifyGuards(ctx context.Context) error {
	for i := range p.config.Disks {
		d := &p.config.Disks[i]
		read, err := catalog.ReadModifyGuard(ctx, p.db, d.ID, p.now, p.modWindow.Nanoseconds())
		if err != nil {
			return fmt.Errorf("catalog: %w", err)
		}
		g := &modGuard{read: read, count: read.Count, tripped: read.Stands}
		p.modGuards[d.ID] = g
		if g.tripped {
			p.out.Guards = append(p.out.Guards, GuardLine{Disk: d.ID, Kind: GuardModifications, Count: g.count})
		}
	}
	return nil
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
// change time, before anything of it is read: one stands, and no answer covers that change.
func (p *pass) heldBefore(disk string, ctimeNS int64) bool {
	g := p.modGuards[disk]
	return g != nil && g.tripped && !g.read.Answers(ctimeNS)
}

// guardModification asks the mass-modification guard about a decision that would make copies stale (specs.md §15.6):
// an answer decides it, a reject making it a conflict; otherwise it counts, and the one that would trip the guard
// waits, as heldBefore holds every one after it. It says whether the decision goes, and whether a confirm answered it.
func (p *pass) guardModification(ctx context.Context, d *solver.ModDecision, ctimeNS int64) (goes, answered bool, err error) {
	if d.Kind != solver.ModModified || len(d.Stale) == 0 {
		return true, false, nil
	}
	disk := d.Changed[0]
	g := p.modGuards[disk]
	switch {
	case g.read.Answers(ctimeNS) && g.read.Answer == catalog.EventModifyGuardReject:
		d.Kind, d.Stale, d.Hash = solver.ModConflict, nil, nil
		return true, false, nil
	case g.read.Answers(ctimeNS):
		return true, true, nil
	}
	// A guard that stands held this file before anything of it was read (heldBefore): here it is clear.
	rows := p.rows[disk]
	if !solver.GuardTrips(&solver.GuardInput{Count: g.count + 1, CountPercent: g.count + 1, Rows: rows, Files: p.modFiles, Percent: p.modPercent}) {
		g.count++
		return true, false, nil
	}
	g.tripped = true
	p.out.Held = true
	p.out.Guards = append(p.out.Guards, GuardLine{Disk: disk, Kind: GuardModifications, Count: g.count})
	if p.apply {
		if err := catalog.WriteModifyGuardEvent(ctx, p.db, disk, p.now); err != nil {
			return false, false, fmt.Errorf("catalog: %w", err)
		}
	}
	return false, false, nil
}
