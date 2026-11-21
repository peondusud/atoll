package daemon

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/metrics"
)

// What each step of the loop records for the metrics (specs.md §23.1). Without Options.Metrics, nothing is recorded,
// and nothing is read for them.

// recordConfig sets the disks of the configuration: at the start, and after each reload.
func (l *loop) recordConfig() {
	list := make([]metrics.Disk, 0, len(l.opt.Config.Disks))
	for i := range l.opt.Config.Disks {
		d := &l.opt.Config.Disks[i]
		list = append(list, metrics.Disk{ID: d.ID, FS: string(d.FS), Role: d.Role, Mount: d.Mount})
	}
	l.opt.Metrics.Update(func(s *metrics.Snapshot) { s.Configure(list) })
}

// recordDisks records the state of each disk, and with measure the space of each that is there, which a statfs gives,
// as a pass's records it: after each mount check, every 30 s. A disk it cannot measure keeps what was measured before.
func (l *loop) recordDisks(measure bool) {
	if l.opt.Metrics == nil {
		return
	}
	type found struct {
		state   string
		standby bool
		space   *metrics.Space
	}
	all := map[string]found{}
	for i := range l.opt.Config.Disks {
		d := &l.opt.Config.Disks[i]
		st := l.disks[d.ID]
		if st == nil {
			continue
		}
		one := found{state: metricState(d, st), standby: st.standby && st.state != disks.Missing && d.State != config.DiskFailed}
		if measure && one.state != "missing" && one.state != "failed" {
			if capacity, free, err := l.opt.Machine.Space(d.Mount); err == nil {
				one.space = &metrics.Space{Capacity: capacity, Free: free}
			}
		}
		all[d.ID] = one
	}
	l.opt.Metrics.Update(func(s *metrics.Snapshot) {
		for id, one := range all {
			if d := s.DiskOf(id); d != nil {
				d.State, d.Standby = one.state, one.standby
				if one.space != nil {
					d.Space = one.space
				}
			}
		}
	})
}

// metricState is the state of a disk as the metrics give it: failed or draining as the configuration says, standby as
// the last check found it asleep, otherwise as that check found it (specs.md §4, §10).
func metricState(d *config.Disk, st *diskState) string {
	switch {
	case d.State == config.DiskFailed:
		return config.DiskFailed
	case st.state == disks.Missing:
		return string(disks.Missing)
	case st.state == disks.Readonly:
		return string(disks.Readonly)
	case d.State == config.DiskDraining:
		return config.DiskDraining
	case st.standby:
		return "standby"
	default:
		return string(disks.Online)
	}
}

// passCount is what a pass read of the catalog for the metrics, under its lock.
type passCount struct {
	counted *catalog.Counted
	smart   map[string]*metrics.Smart
}

// countForMetrics reads, at the end of a pass, the counts of the catalog and each disk's last reading of SMART. One
// that fails leaves the metrics as the last pass left them, with a line of the log.
func (l *loop) countForMetrics(ctx context.Context, db *sql.DB) *passCount {
	if l.opt.Metrics == nil {
		return nil
	}
	counted, err := catalog.CountForMetrics(ctx, db, l.opt.Machine.Now()-l.set.scrubEvery.Nanoseconds())
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("the metrics could not count the catalog: the next pass tries again", "error", err)
		}
		return nil
	}
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("the metrics could not read the disks of the catalog: the next pass tries again", "error", err)
		}
		return nil
	}
	out := &passCount{counted: counted, smart: map[string]*metrics.Smart{}}
	for i := range list {
		var r disks.SmartRecord
		if list[i].SmartJSON != "" && json.Unmarshal([]byte(list[i].SmartJSON), &r) == nil {
			out.smart[list[i].ID] = smartOf(r.ReadNS, &r.Smart)
		}
	}
	return out
}

func smartOf(readNS int64, s *disks.Smart) *metrics.Smart {
	return &metrics.Smart{ReadNS: readNS, Failing: s.Failing, Reallocated: s.Reallocated, Pending: s.Pending, Uncorrectable: s.Uncorrectable,
		Temperature: s.Temperature}
}

// recordPass records a pass that ended: when, how long it took, what it did, and what it counted.
func (l *loop) recordPass(startNS, endNS int64, outcome *jobs.Outcome, sink *passSink, count *passCount) {
	failed := map[string]bool{}
	for i := range l.opt.Config.Disks {
		failed[l.opt.Config.Disks[i].ID] = l.opt.Config.Disks[i].State == config.DiskFailed
	}
	l.opt.Metrics.Update(func(s *metrics.Snapshot) {
		s.Passes++
		s.LastPass = &metrics.LastPass{EndNS: endNS, DurationNS: endNS - startNS, Partial: outcome.Partial, Held: outcome.Held}
		waiting := outcome.Waiting
		s.Waiting = &waiting
		s.CopiedBytes += outcome.Copied
		s.AddOperations(sink.ops)
		if count == nil {
			return
		}
		s.Files = map[string]metrics.Count{}
		for state, n := range count.counted.Files {
			s.Files[state] = metrics.Count{Files: n.Files, Bytes: n.Bytes}
		}
		for i := range s.Disks {
			d := &s.Disks[i]
			c := count.counted.Disks[d.ID]
			if c == nil {
				c = &catalog.CountedDisk{}
			}
			d.Catalog = &metrics.DiskCatalog{Copies: c.Copies, Bytes: c.Bytes, TrashEntries: c.TrashEntries, TrashBytes: c.TrashBytes,
				ScrubDueCopies: c.ScrubDueCopies, ScrubDueBytes: c.ScrubDueBytes}
			// The reading of a failed disk is no news of it; a reading the daemon made since, that a dry run did not
			// record, is newer than the catalog's.
			switch r := count.smart[d.ID]; {
			case failed[d.ID]:
				d.Smart = nil
			case r != nil && (d.Smart == nil || r.ReadNS > d.Smart.ReadNS):
				d.Smart = r
			}
		}
	})
}

// recordAlerts records the conditions that hold, as the alerts after a pass work them out, whether or not a channel
// is set (specs.md §23).
func (l *loop) recordAlerts(holding []alerts.Holding) {
	list := make([]metrics.Alert, 0, len(holding))
	for _, h := range holding {
		list = append(list, metrics.Alert{Key: h.Key, Count: h.Count})
	}
	l.opt.Metrics.Update(func(s *metrics.Snapshot) { s.Alerts = list })
}

// recordSmart records readings of SMART as they come, each of a disk awake.
func (l *loop) recordSmart(readNS int64, readings map[string]disks.Smart) {
	l.opt.Metrics.Update(func(s *metrics.Snapshot) {
		for id, r := range readings {
			if d := s.DiskOf(id); d != nil {
				d.Smart = smartOf(readNS, &r)
			}
		}
	})
}

// recordScrub adds what a slice of the scrub read, and found corrupt, on each disk.
func (l *loop) recordScrub(out *jobs.ScrubOutcome) {
	l.opt.Metrics.Update(func(s *metrics.Snapshot) {
		for i := range out.Disks {
			if d := s.DiskOf(out.Disks[i].ID); d != nil {
				d.ScrubReadCopies += out.Disks[i].Read
				d.ScrubReadBytes += out.Disks[i].Bytes
				d.ScrubCorrupt += int64(len(out.Disks[i].Corrupt))
			}
		}
	})
}
