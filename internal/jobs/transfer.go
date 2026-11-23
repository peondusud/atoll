package jobs

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// The kind and outcomes of a transfer of the SSD cache in a pass's output (specs.md §15.12, §19).
const (
	KindTransfer       = "transfer"
	OutcomeTransferred = "transferred"
	OutcomeToTransfer  = "to-transfer"
)

// transfers removes from the SSD cache each copy of a file the data disks now hold (specs.md §19). It comes after the
// copies, as those just made are in no plan made before them: each file leaving the cache is planned again alone, from
// the catalog as it stands, and each transfer job of that plan runs at once. A dry run lists them.
func (p *pass) transfers(ctx context.Context) error {
	// What stays on the cache waits for its transfer: the daemon wakes when the first may leave (specs.md §15.12).
	waiting, next, err := watch.CacheWaits(ctx, p.db, p.config, p.now)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	p.copiesWaiting += waiting
	p.copiesDue = earliest(p.copiesDue, next)
	list, err := watch.CacheTransfers(ctx, p.db, p.config, p.now, p.session.LeavesWildcards)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if len(list) == 0 {
		return nil
	}
	diskList, err := catalog.Disks(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	viewOps, err := catalog.OpenViewOps(ctx, p.db)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for i := range list {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("transfer: %w", err)
		}
		t := &list[i]
		if touchedByView(viewOps, t.Path) {
			// The view presents that path from what its operation made, on the disk it made it on, which the transfer
			// would empty: it waits for a pass to take the operation in, as this one does as it ends (specs.md §19,
			// §21.2). It is due again a debounce on.
			p.copiesWaiting++
			p.copiesDue = earliest(p.copiesDue, sql.NullInt64{Int64: p.now + p.debounce.Nanoseconds(), Valid: true})
			continue
		}
		if p.batteryHolds() {
			continue
		}
		if !p.apply {
			p.operated(Operation{Disk: t.Src, Kind: KindTransfer, Path: t.Path, Outcome: OutcomeToTransfer})
			continue
		}
		outcome, err := p.transfer(ctx, t, diskList)
		if err != nil {
			return err
		}
		if outcome == trash.OutcomeViewHolds {
			// An operation of the view came since the pass read them: it waits as above (flaw 10).
			p.copiesWaiting++
			p.copiesDue = earliest(p.copiesDue, sql.NullInt64{Int64: p.now + p.debounce.Nanoseconds(), Valid: true})
			continue
		}
		p.ranFile(t.FileID)
		p.record(Operation{Disk: t.Src, Kind: KindTransfer, Path: t.Path, Outcome: outcome})
		if p.sinkErr != nil {
			return p.sinkErr
		}
	}
	return nil
}

// touchedByView says whether an operation of the view that no pass has taken in touches a path: at it, above it, or
// below it.
func touchedByView(ops []catalog.ViewOp, path string) bool {
	for i := range ops {
		for _, at := range []string{ops[i].Path, ops[i].Path2} {
			if at != "" && (isAtOrBelow(path, at) || isAtOrBelow(at, path)) {
				return true
			}
		}
	}
	return false
}

// transfer removes one copy from the SSD cache (specs.md §19). When it is the file's last user copy, it first hands its
// part to a copy on a data disk, in one transaction that makes it an Atoll copy, as atoll balance --move-originals does
// (§13.3): a file never has fewer user copies, a crash included. It then goes to its disk's trash and is purged from
// there at once.
func (p *pass) transfer(ctx context.Context, t *watch.Transfer, diskList []catalog.Disk) (string, error) {
	f, err := catalog.FileByID(ctx, p.db, t.FileID)
	if err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	if f == nil || f.Path != t.Path {
		return trash.OutcomeChanged, nil
	}
	at := slices.IndexFunc(f.Copies, func(c catalog.CopyRow) bool { return c.Disk == t.Src })
	if at < 0 {
		return trash.OutcomeChanged, nil
	}
	if !p.session.Writable(t.Src) {
		return OutcomeUnavailable, nil
	}
	c := &f.Copies[at]
	if c.CreatedBy == solver.CreatedByUser {
		heir := transferHeir(f, diskList)
		if heir == "" {
			return trash.OutcomeChanged, nil
		}
		if err := catalog.HandOver(ctx, p.db, f.ID, t.Src, heir); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
	}
	outcome, err := p.session.Transfer(ctx, f, c)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if outcome != trash.OutcomeTrashed {
		if outcome == trash.OutcomeChanged {
			// A transfer undone put its copy back on the cache, maybe as the file's original again.
			p.settled = false
		}
		return outcome, nil
	}
	// The file is worked out again at the end of the pass: its original is now on a data disk.
	p.settled = false
	return OutcomeTransferred, nil
}

// transferHeir gives the copy on a data disk a copy leaving the SSD cache hands its user part to: a copy ok, on a disk
// that can be read, a user copy first, then the first seen, then by disk position. None when there is none, as the
// copy on the cache is then needed.
func transferHeir(f *catalog.FileRow, diskList []catalog.Disk) string {
	type heir struct {
		disk      string
		user      bool
		firstSeen int64
		position  int
	}
	var list []heir
	for i := range f.Copies {
		c := &f.Copies[i]
		at := slices.IndexFunc(diskList, func(d catalog.Disk) bool { return d.ID == c.Disk })
		if at < 0 || c.State != string(solver.CopyOK) {
			continue
		}
		d := &diskList[at]
		switch solver.DiskState(d.State) {
		case solver.DiskOnline, solver.DiskStandby, solver.DiskReadOnly:
		case solver.DiskMissing, solver.DiskDraining, solver.DiskFailed, solver.DiskRetired:
			continue
		}
		if d.Role == solver.RoleData {
			list = append(list, heir{disk: c.Disk, user: c.CreatedBy == solver.CreatedByUser, firstSeen: c.FirstSeenNS, position: d.Position})
		}
	}
	if len(list) == 0 {
		return ""
	}
	slices.SortFunc(list, func(a, b heir) int {
		return cmp.Or(boolFirst(a.user, b.user), cmp.Compare(a.firstSeen, b.firstSeen), cmp.Compare(a.position, b.position))
	})
	return list[0].disk
}
