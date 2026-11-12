package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// SetSmart records a disk's last reading of SMART in smart_json (specs.md §23). The reading must be a JSON object.
func SetSmart(ctx context.Context, db *sql.DB, id string, reading []byte) error {
	if !json.Valid(reading) || len(reading) == 0 || reading[0] != '{' {
		return fmt.Errorf("disk %q: a reading of SMART that is not a JSON object", id)
	}
	res, err := db.ExecContext(ctx, "UPDATE disks SET smart_json = ? WHERE id = ?", string(reading), id)
	if err != nil {
		return fmt.Errorf("disk %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("disk %q: no such disk in the catalog", id)
	}
	return nil
}
