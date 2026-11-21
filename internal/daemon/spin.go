package daemon

import (
	"context"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
)

// spin tells, at each check, whether a disk the check found online is in standby, waking none of its whole disks
// (specs.md §10): one whose counters moved since the last check spins, and is asked nothing; any other is asked its
// power mode, unless it gave no answer before. A disk in any other state, or draining or failed, is left to the
// passes, which write its state. A machine that cannot tell leaves every disk online.
func (l *loop) spin(d *config.Disk, st *diskState) {
	reader, reads := l.opt.Machine.(disks.SpinReader)
	topology, tells := l.opt.Machine.(disks.Topology)
	if !reads || !tells {
		return
	}
	if st.state != disks.Online || d.State == config.DiskFailed || d.State == config.DiskDraining {
		st.counts, st.standby, st.spinWritten = nil, false, true
		return
	}
	standby := l.standbyOf(d, st, reader, topology)
	if standby != st.standby {
		st.standby, st.spinWritten = standby, false
	}
}

// standbyOf reads what a disk's whole disks say, and keeps their counters for the next check.
func (l *loop) standbyOf(d *config.Disk, st *diskState, reader disks.SpinReader, topology disks.Topology) bool {
	seen := l.opt.Machine.Mount(d.Mount)
	if seen.Entry == nil {
		return false
	}
	names, err := topology.PhysicalDisks(d.FS, seen.Entry, d.UUID)
	if err != nil || len(names) == 0 {
		return false
	}
	counts := map[string]int64{}
	list := make([]disks.WholeDiskLook, 0, len(names))
	for _, name := range names {
		w := disks.WholeDiskLook{Name: name, Refused: l.noAnswer[name]}
		if rotational, err := reader.Rotational(name); err == nil {
			w.Rotational = &rotational
		}
		if s, err := reader.BlockStat(name); err == nil {
			now := s.Completed
			w.Now, w.Busy, counts[name] = &now, s.InFlight > 0, now
			if before, ok := st.counts[name]; ok {
				w.Before = &before
			}
		}
		list = append(list, w)
	}
	st.counts = counts
	return disks.Standby(list, func(name string) disks.Power {
		p, err := reader.PowerMode(name)
		if err != nil || p == disks.PowerUnknown {
			if l.noAnswer == nil {
				l.noAnswer = map[string]bool{}
			}
			l.noAnswer[name] = true
			l.log.Info("a disk gives no answer to CHECK POWER MODE: it counts as spinning until the configuration is read again",
				"disk", d.ID, "device", name, "error", err)
			return disks.PowerUnknown
		}
		return p
	})
}

// recordSpin records in the catalog what the checks found of the disks online, in standby or spinning again, where the
// catalog may not hold it yet, for the view and the plan (specs.md §10). It takes no lock: online or standby changes no
// decision a command makes, only which disk is preferred, and a command's sync keeps standby. A daemon in shadow mode
// records nothing.
func (l *loop) recordSpin(ctx context.Context) {
	if !l.opt.Apply {
		return
	}
	var todo []string
	for i := range l.opt.Config.Disks {
		if st := l.disks[l.opt.Config.Disks[i].ID]; st != nil && !st.spinWritten {
			todo = append(todo, l.opt.Config.Disks[i].ID)
		}
	}
	if len(todo) == 0 {
		return
	}
	db, err := catalog.OpenPool(ctx, l.set.catalog, l.opt.Config.PoolID)
	if err != nil {
		l.log.Warn("which disks are in standby is not recorded: the next check tries again", "error", err)
		return
	}
	defer func() { _ = db.Close() }()
	for _, id := range todo {
		st := l.disks[id]
		written, err := catalog.SetSpin(ctx, db, id, st.standby)
		if err != nil {
			l.log.Warn("whether a disk is in standby is not recorded: the next check tries again", "disk", id, "error", err)
			continue
		}
		// A disk the catalog does not hold online or in standby yet, as one a pass has not found again, waits for it.
		st.spinWritten = written
		if written && st.standby {
			l.log.Info("a disk is in standby", "disk", id)
		}
	}
}
