package jobs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
)

// BackupOutcome is what a catalog backup did: the backup written, the old ones removed, and with
// maintenance.catalog_on_disks each disk not failed, in configuration order, and why its copy failed, if it did.
type BackupOutcome struct {
	Path    string
	Removed []string
	Disks   []BackupDisk
}

// BackupDisk is a disk a backup was copied to, and the error that kept it from it.
type BackupDisk struct {
	ID  string
	Err error
}

// BackupCatalog backs the catalog up beside it, and with maintenance.catalog_on_disks copies the backup to each disk
// not failed (specs.md §9). The caller holds the catalog's lock.
func BackupCatalog(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine) (*BackupOutcome, error) {
	effective := c.Effective()
	file, _ := effective["catalog"].(string)
	written, removed, err := catalog.Backup(ctx, db, file, c.PoolID, m.Now())
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	out := &BackupOutcome{Path: written, Removed: removed}
	if on, _ := effective["maintenance.catalog_on_disks"].(bool); !on {
		return out, nil
	}
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed {
			continue
		}
		out.Disks = append(out.Disks, BackupDisk{ID: d.ID, Err: copyBackupTo(m, d, written)})
	}
	return out, nil
}

// copyBackupTo copies a backup to a disk's Atoll/catalog/, through a handle confirmed first (invariant 3). A disk
// missing or read-only takes none.
func copyBackupTo(m disks.Machine, d *config.Disk, backup string) error {
	if v := disks.Check(m, d); v.State != disks.Online {
		return fmt.Errorf("it is %s (%s)", v.State, v.Reason)
	}
	handle, err := ops.Open(m, d, 0)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer handle.Close()
	if v := disks.Confirm(m, d); v.State == disks.Missing {
		return fmt.Errorf("%s (%s)", v.Message, v.Reason)
	}
	if err := handle.Verify(); err != nil {
		return fmt.Errorf("%w", err)
	}
	src, err := fsx.OpenSystemFile(backup)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = src.Close() }()
	if err := handle.PutCatalog(src); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
