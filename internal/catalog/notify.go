package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// copiesOnBelowSQL says whether a file below a folder has a copy on one of n disks: the disks first, then the bounds
// of the folder. Every path below a folder lies between "<folder>/" and "<folder>0", '0' being the byte after '/'.
// CROSS JOIN keeps the files first, read by the range of their paths: led by the disk, as SQLite chooses with no
// statistics, it read every copy of a missing disk for each folder that holds none of them, and the daemon asks for
// each folder it has to tell a media server of, at each pass while a disk is away (specs.md §21.4, §26).
func copiesOnBelowSQL(n int) string {
	return `SELECT EXISTS (SELECT 1 FROM files f CROSS JOIN copies c ON c.file_id = f.id
		WHERE f.lpath >= ?` + strconv.Itoa(n+1) + ` AND f.lpath < ?` + strconv.Itoa(n+2) + ` AND c.disk_id IN (` + numbered(n) + `))`
}

// numbered gives the parameters ?1 to ?n, with commas between them.
func numbered(n int) string {
	marks := make([]string, n)
	for i := range marks {
		marks[i] = "?" + strconv.Itoa(i+1)
	}
	return strings.Join(marks, ", ")
}

// FoldersWithCopiesOn gives, of these folders, those below which a file has a copy on one of these disks, in the
// order given: while a disk is missing, a scan of such a folder would find its file gone (specs.md §21.4). A folder
// is a path relative to the top of the view, "" for the top itself.
func FoldersWithCopiesOn(ctx context.Context, db *sql.DB, folders, disks []string) ([]string, error) {
	if len(folders) == 0 || len(disks) == 0 {
		return nil, nil
	}
	in := strings.Repeat(", ?", len(disks))[2:]
	below := copiesOnBelowSQL(len(disks))
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
