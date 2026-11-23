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
	rows, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	seen := map[string]bool{}
	for i := range rows {
		seen[rows[i].ID] = rows[i].MarkerSeen && rows[i].UUID == findUUID(c, rows[i].ID)
	}
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed {
			continue
		}
		own, err := copyBackupTo(m, c.PoolID, d, seen[d.ID], written)
		if err == nil && own && !seen[d.ID] {
			if err := catalog.SetMarkerSeen(ctx, db, d.ID, d.UUID, m.Now()); err != nil {
				return nil, fmt.Errorf("catalog: %w", err)
			}
		}
		out.Disks = append(out.Disks, BackupDisk{ID: d.ID, Err: err})
	}
	return out, nil
}

// findUUID gives the identifier the configuration gives a disk, "" for none.
func findUUID(c *config.Config, id string) string {
	for i := range c.Disks {
		if c.Disks[i].ID == id {
			return c.Disks[i].UUID
		}
	}
	return ""
}

// copyBackupTo copies a backup to a disk's Atoll/catalog/, through a handle confirmed first (invariant 3), and says
// whether its marker is its own, which the catalog then records seen. A disk missing or read-only takes none, a
// marker that makes it readonly included: of another pool, disk or filesystem, or none where its own was seen
// (specs.md §10), as the backup holds every path of the pool.
func copyBackupTo(m disks.Machine, poolID string, d *config.Disk, seen bool, backup string) (bool, error) {
	if v := disks.Check(m, d); v.State != disks.Online {
		return false, fmt.Errorf("it is %s (%s)", v.State, v.Reason)
	}
	handle, err := ops.Open(m, d, 0)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	defer handle.Close()
	v := disks.Confirm(m, d)
	if v.State == disks.Missing {
		return false, fmt.Errorf("%s (%s)", v.Message, v.Reason)
	}
	text, found, err := handle.ReadMarker()
	if err != nil {
		return false, fmt.Errorf("its marker: %w", err)
	}
	v, own := disks.MarkerVerdict(v, text, found, seen, poolID, d)
	if v.State != disks.Online {
		return false, fmt.Errorf("it is %s (%s)", v.State, v.Reason)
	}
	if err := handle.Verify(); err != nil {
		return false, fmt.Errorf("%w", err)
	}
	src, err := fsx.OpenSystemFile(backup)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	defer func() { _ = src.Close() }()
	if err := handle.PutCatalog(src); err != nil {
		return false, fmt.Errorf("%w", err)
	}
	return own, nil
}
