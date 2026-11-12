package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// EventCorrupt is the event of a copy the scrub found corrupt (specs.md §9, §16).
const EventCorrupt = "corrupt"

// ReasonQuarantined sends a corrupt copy to the trash (specs.md §16, §17).
const ReasonQuarantined = "quarantined"

// ScrubCopy is a copy the scrub reads: where it sits, what its row says, and its file's hashes.
type ScrubCopy struct {
	FileID     int64
	Path       string // where it sits on its disk: its old path while a rename waits for it
	Size       int64
	MtimeNS    int64
	CtimeNS    int64
	FileKey    sql.NullString
	VerifiedNS sql.NullInt64
	Hash       []byte
	Chunks     []byte // the hash of each chunk, concatenated; none for an empty file
}

// ScrubKey is where a page of due copies ends: the next page starts after it.
type ScrubKey struct {
	VerifiedNS int64 // -1 for a copy never verified
	FileID     int64
}

// scrubColumns reads a ScrubCopy, from copies c, files f and file_chunks fc.
const scrubColumns = `c.file_id, COALESCE(c.old_lpath, f.lpath), c.size, c.mtime_ns, c.ctime_ns, c.file_key, c.verified_ns, f.blake3,
	COALESCE(fc.chunks, x'')`

// scrubDue is the condition of a copy c of a disk ?1 the scrub has to read: one that counts, of a file hashed,
// never verified or verified at or before ?2, its size and times known.
const scrubDue = `c.disk_id = ?1 AND c.state IN ('ok', 'surplus') AND f.blake3 IS NOT NULL
	AND (c.verified_ns IS NULL OR c.verified_ns <= ?2) AND c.size IS NOT NULL AND c.mtime_ns IS NOT NULL AND c.ctime_ns IS NOT NULL`

// ScrubDue gives up to limit copies of a disk that are due, after the key: those never verified first, then by the
// time they were, then by file id. A non-empty under keeps the files at that path and below it, by whole components.
func ScrubDue(ctx context.Context, db *sql.DB, disk string, dueNS int64, after ScrubKey, limit int, under string) ([]ScrubCopy, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+scrubColumns+`
		FROM copies c JOIN files f ON f.id = c.file_id LEFT JOIN file_chunks fc ON fc.file_id = f.id
		WHERE `+scrubDue+` AND (COALESCE(c.verified_ns, -1), c.file_id) > (?3, ?4)
			AND (?6 = x'' OR f.lpath = ?6 OR substr(f.lpath, 1, length(?7)) = ?7)
		ORDER BY COALESCE(c.verified_ns, -1), c.file_id LIMIT ?5`, disk, dueNS, after.VerifiedNS, after.FileID, limit, []byte(under), []byte(under+"/"))
	if err != nil {
		return nil, fmt.Errorf("copies of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	var list []ScrubCopy
	for rows.Next() {
		cp, err := scanScrubCopy(rows)
		if err != nil {
			return nil, fmt.Errorf("copies of %q: %w", disk, err)
		}
		list = append(list, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copies of %q: %w", disk, err)
	}
	return list, nil
}

// ScrubCopyOf gives a copy as the scrub reads it, whether due or not, and false when there is no such copy that
// counts.
func ScrubCopyOf(ctx context.Context, db *sql.DB, disk string, fileID int64) (ScrubCopy, bool, error) {
	row := db.QueryRowContext(ctx, `SELECT `+scrubColumns+`
		FROM copies c JOIN files f ON f.id = c.file_id LEFT JOIN file_chunks fc ON fc.file_id = f.id
		WHERE c.file_id = ?3 AND `+scrubDue, disk, int64(1<<62), fileID)
	cp, err := scanScrubCopy(row)
	switch {
	case isNoRows(err):
		return ScrubCopy{}, false, nil
	case err != nil:
		return ScrubCopy{}, false, fmt.Errorf("copy of %d on %q: %w", fileID, disk, err)
	}
	return cp, true, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanScrubCopy(s scanner) (ScrubCopy, error) {
	var cp ScrubCopy
	var path []byte
	err := s.Scan(&cp.FileID, &path, &cp.Size, &cp.MtimeNS, &cp.CtimeNS, &cp.FileKey, &cp.VerifiedNS, &cp.Hash, &cp.Chunks)
	cp.Path = string(path)
	return cp, err //nolint:wrapcheck // the callers say what was read
}

// ScrubCursor is where the scrub of a disk stopped, its budget spent.
type ScrubCursor struct {
	Disk    string
	FileID  int64
	Chunk   int64
	Size    int64
	MtimeNS int64
	CtimeNS int64
}

// ReadScrubCursor gives where the scrub of a disk stopped, or false when it ended between two copies.
func ReadScrubCursor(ctx context.Context, db *sql.DB, disk string) (ScrubCursor, bool, error) {
	c := ScrubCursor{Disk: disk}
	err := db.QueryRowContext(ctx, "SELECT file_id, chunk, size, mtime_ns, ctime_ns FROM scrub_cursor WHERE disk_id = ?", disk).
		Scan(&c.FileID, &c.Chunk, &c.Size, &c.MtimeNS, &c.CtimeNS)
	switch {
	case isNoRows(err):
		return ScrubCursor{}, false, nil
	case err != nil:
		return ScrubCursor{}, false, fmt.Errorf("scrub of %q: %w", disk, err)
	}
	return c, true, nil
}

// WriteScrubCursor records where the scrub of a disk stopped.
func WriteScrubCursor(ctx context.Context, db *sql.DB, c *ScrubCursor) error {
	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO scrub_cursor(disk_id, file_id, chunk, size, mtime_ns, ctime_ns)
		VALUES (?, ?, ?, ?, ?, ?)`, c.Disk, c.FileID, c.Chunk, c.Size, c.MtimeNS, c.CtimeNS); err != nil {
		return fmt.Errorf("scrub of %q: %w", c.Disk, err)
	}
	return nil
}

// ClearScrubCursor says that the scrub of a disk stopped between two copies.
func ClearScrubCursor(ctx context.Context, db *sql.DB, disk string) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM scrub_cursor WHERE disk_id = ?", disk); err != nil {
		return fmt.Errorf("scrub of %q: %w", disk, err)
	}
	return nil
}

// SetCopyVerified records that a copy was read whole and matched its file's hashes.
func SetCopyVerified(ctx context.Context, db *sql.DB, fileID int64, disk string, nowNS int64) error {
	if _, err := db.ExecContext(ctx, "UPDATE copies SET verified_ns = ? WHERE file_id = ? AND disk_id = ?", nowNS, fileID, disk); err != nil {
		return fmt.Errorf("copy of %d on %q: %w", fileID, disk, err)
	}
	return nil
}

// MarkCopyCorrupt records that a copy no longer holds its file's content, its size and times unchanged: the copy
// is corrupt, and so is its file, which nothing else remembers (specs.md §4, §16). The event comes first.
func MarkCopyCorrupt(ctx context.Context, db *sql.DB, fileID int64, disk, path string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		data := &eventRow{Disk: disk}
		data.setPath(path)
		if err := insertEvent(ctx, tx, nowNS, "warning", EventCorrupt, data); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET state = 'corrupt', verified_ns = NULL WHERE file_id = ? AND disk_id = ?", fileID, disk); err != nil {
			return fmt.Errorf("copy of %d on %q: %w", fileID, disk, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE files SET state = 'corrupt', updated_ns = ? WHERE id = ?", nowNS, fileID); err != nil {
			return fmt.Errorf("file %d: %w", fileID, err)
		}
		return nil
	})
}
