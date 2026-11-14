package jobs

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
)

// RecordDevices records, at a start of the daemon, the physical disks each disk lives on that mounted says the mount
// check found, by its order in the configuration (specs.md §10, R-06). disk add refuses a disk on one another disk of
// the pool lives on, and a repartition or a change of device-mapper may make two such later: the plan then counts them
// as one, so that it never places two copies of a file on them, and an alert tells of them; writes go on. A machine
// that cannot tell records nothing, and a disk it cannot ask keeps what was found before.
func RecordDevices(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, mounted func(i int) bool) {
	t, ok := m.(disks.Topology)
	if !ok {
		return
	}
	known := map[string]string{}
	if list, err := catalog.Disks(ctx, db); err == nil {
		for i := range list {
			known[list[i].ID] = list[i].Device
		}
	}
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed || !mounted(i) {
			continue
		}
		seen := m.Mount(d.Mount)
		if seen.Entry == nil {
			continue
		}
		physical, err := t.PhysicalDisks(d.FS, seen.Entry, d.UUID)
		if err != nil {
			slog.Warn("which physical disk a disk lives on is not known", "disk", d.ID, "error", err)
			continue
		}
		if was, ok := known[d.ID]; ok && was == catalog.JoinDevice(physical) {
			continue
		}
		if err := catalog.SetDevice(ctx, db, d.ID, physical); err != nil {
			slog.Warn("the physical disks of a disk are not recorded", "disk", d.ID, "error", err)
		}
	}
}
