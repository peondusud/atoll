package daemon

import (
	"path/filepath"
	"slices"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/ops"
)

// foreignWorkdirs gives the disks a pass found mounted whose work folder, or the tmp/, links/ or trash/ below it,
// belongs to an account that is none of Atoll's, in the configuration's order, for the alert that says so (specs.md
// §10, §23). atoll disk add refuses such a
// disk; one that becomes so later is told of, and written to all the same, so that nothing stops unattended. Each is
// looked at once a pass, at its top and in its work folder, which wakes no disk whose folders are in the cache. A disk
// that cannot be looked at says nothing.
func (l *loop) foreignWorkdirs(outcome *jobs.Outcome) []alerts.ForeignWorkdir {
	trusted := ops.TrustedOwners(l.set.catalog)
	var out []alerts.ForeignWorkdir
	for i := range l.opt.Config.Disks {
		d := &l.opt.Config.Disks[i]
		mounted := slices.ContainsFunc(outcome.Disks, func(line jobs.DiskLine) bool {
			return line.ID == d.ID && (line.Verdict.State == disks.Online || line.Verdict.State == disks.Readonly)
		})
		if !mounted {
			continue
		}
		place, uid, foreign, err := ops.ForeignWork(d, trusted)
		if err != nil {
			l.log.Warn("whose a disk's work folder is could not be read", "disk", d.ID, "error", err)
			continue
		}
		if foreign {
			out = append(out, alerts.ForeignWorkdir{ID: d.ID, Path: filepath.Join(d.Mount, filepath.FromSlash(place)), UID: uid, Want: ops.WorkdirOwner(trusted)})
		}
	}
	return out
}
