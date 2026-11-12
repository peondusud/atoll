package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// FoldersWithCopiesOn gives, of these folders, those below which a file has a copy on one of these disks, in the
// order given: while a disk is missing, a scan of such a folder would find its file gone (specs.md §21.4). A folder
// is a path relative to the top of the view, "" for the top itself.
func FoldersWithCopiesOn(ctx context.Context, db *sql.DB, folders, disks []string) ([]string, error) {
	if len(folders) == 0 || len(disks) == 0 {
		return nil, nil
	}
	in := strings.Repeat(", ?", len(disks))[2:]
	// Every path below a folder lies between "<folder>/" and "<folder>0", '0' being the byte after '/'.
	below := `SELECT EXISTS (SELECT 1 FROM files f JOIN copies c ON c.file_id = f.id
		WHERE c.disk_id IN (` + in + `) AND f.lpath >= ? AND f.lpath < ?)`
	anywhere := `SELECT EXISTS (SELECT 1 FROM copies WHERE disk_id IN (` + in + `))`
	var out []string
	for _, folder := range folders {
		args := make([]any, 0, len(disks)+2)
		for _, d := range disks {
			args = append(args, d)
		}
		query := anywhere
		if folder != "" {
			query = below
			args = append(args, []byte(folder+"/"), []byte(folder+"0"))
		}
		var held bool
		if err := db.QueryRowContext(ctx, query, args...).Scan(&held); err != nil {
			return nil, fmt.Errorf("copies: %w", err)
		}
		if held {
			out = append(out, folder)
		}
	}
	return out, nil
}
