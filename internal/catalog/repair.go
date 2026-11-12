package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/peondusud/atoll/internal/layout"
)

// The operation, events and trash reason of a repair (specs.md §16.1, §17).
const (
	OpRepair       = "repair"
	EventRepair    = "repair"
	EventRepaired  = "repaired"
	ReasonRepaired = "repaired"
)

// RepairStart is what a chunk repair journals before anything is written (specs.md §16.1, step 2).
type RepairStart struct {
	Disk    string
	Path    string // where the copy sits
	FileKey string
	// Bad holds the indexes of the copy's bad chunks, ascending, and Size their bytes together.
	Bad  []int64
	Size int64
	// MtimeNS is the copy's modification time, which the repair sets back once it has written.
	MtimeNS   int64
	NowNS     int64
	ExpiresNS int64
}

// finishRepairSQL gives the copy at the operation's place, of its inode, what a repair leaves: ok, verified now, with
// the change time its writes gave it.
const finishRepairSQL = `
	UPDATE copies AS c SET state = 'ok', verified_ns = ?4, ctime_ns = ?5 WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace

// keepCorruptSQL gives the copy at the operation's place, of its inode, the change time a repair that stopped left
// it: the copy stays corrupt, and its row says again what its disk holds (specs.md §16.1).
const keepCorruptSQL = `
	UPDATE copies AS c SET ctime_ns = ?4 WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace

// BeginRepair journals a chunk repair, before anything is written to its disk: the operation, its entry in the
// trash, which is where the chunks it saves go, and the event, which says which chunks those are and the time to set
// back (specs.md §16.1). Until the operation is done, its entry is not in the trash.
func BeginRepair(ctx context.Context, db *sql.DB, s *RepairStart) (*Op, error) {
	op := &Op{Disk: s.Disk, Kind: OpRepair, Path: s.Path, FileKey: s.FileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES (?, 'repair', ?, ?, ?, 'planned')",
			s.Disk, []byte(s.Path), s.FileKey, s.NowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.Path2, err = layout.ChunksPath(s.NowNS, op.ID, s.Path); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE ops SET path2 = ? WHERE id = ?", []byte(op.Path2), op.ID); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, blake3, reason, deleted_ns, expires_ns)
			VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
			[]byte(s.Path), s.Disk, []byte(op.Path2), op.ID, s.Size, ReasonRepaired, s.NowNS, s.ExpiresNS)
		if err != nil {
			return fmt.Errorf("trash: %w", err)
		}
		mtime := s.MtimeNS
		return writeEventData(ctx, tx, s.NowNS, "info", EventRepair, op, eventData{MtimeNS: &mtime, Chunks: s.Bad})
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", s.Path, err)
	}
	return op, nil
}

// RepairOf gives what the event of a repair holds: the indexes of its bad chunks and the time to set back, and
// false when there is no such event.
func RepairOf(ctx context.Context, db *sql.DB, opID int64) (bad []int64, mtimeNS int64, ok bool, err error) {
	var data string
	err = db.QueryRowContext(ctx, "SELECT data FROM events WHERE kind = ? AND json_extract(data, '$.op') = ?", EventRepair, opID).Scan(&data)
	switch {
	case isNoRows(err):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, fmt.Errorf("event: %w", err)
	}
	var e struct {
		Chunks  []int64 `json:"chunks"`
		MtimeNS *int64  `json:"mtime_ns"`
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, 0, false, fmt.Errorf("event: %w", err)
	}
	if e.MtimeNS == nil {
		return nil, 0, false, nil
	}
	return e.Chunks, *e.MtimeNS, true, nil
}

// FinishRepair ends a chunk repair (specs.md §16.1, step 7): the operation done, its entry in the trash listed from
// now, the copy ok, verified now, with the change time its writes gave it, and the event repaired. The file's state
// is worked out again by the end of the run, as a file is corrupt only while a copy is.
func FinishRepair(ctx context.Context, db *sql.DB, op *Op, ctimeNS, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, finishRepairSQL, op.Disk, []byte(op.Path), op.FileKey, nowNS, ctimeNS); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		return writeEventData(ctx, tx, nowNS, "info", EventRepaired, op, eventData{})
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// EventRepairStopped is the event of a repair that stopped before its end (specs.md §16.1).
const EventRepairStopped = "repair-stopped"

// StopRepair ends a repair that stopped before its end, the copy staying corrupt (specs.md §16.1): the operation
// done when a chunk reached the trash, its entry's size then the bytes saved, and undone with its trash row gone when
// none did; the copy's row takes the change time its disk holds now when that is given, so that the next scrub reads
// the copy as corrupt again rather than as an edit; and the event repair-stopped, with the outcome.
func StopRepair(ctx context.Context, db *sql.DB, op *Op, savedBytes int64, saved bool, ctimeNS sql.NullInt64, outcome string, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		state := "undone"
		if saved {
			state = "done"
			if _, err := tx.ExecContext(ctx, "UPDATE trash SET size = ? WHERE op_id = ?", savedBytes, op.ID); err != nil {
				return fmt.Errorf("trash: %w", err)
			}
		} else if err := dropTrashRow(ctx, tx, op); err != nil {
			return err
		}
		if err := markOp(ctx, tx, op.ID, state); err != nil {
			return err
		}
		if ctimeNS.Valid {
			if _, err := tx.ExecContext(ctx, keepCorruptSQL, op.Disk, []byte(op.Path), op.FileKey, ctimeNS.Int64); err != nil {
				return fmt.Errorf("copy: %w", err)
			}
		}
		return writeEventData(ctx, tx, nowNS, "warning", EventRepairStopped, op, eventData{Reason: outcome})
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// RepairCopy is what a repair's reprise reads of the copy its operation names: its row and its file's hashes.
type RepairCopy struct {
	FileID  int64
	Size    int64
	MtimeNS int64
	CtimeNS int64
	Chunks  []byte // the hash of each chunk, concatenated
}

// repairCopySQL reads the copy at an operation's place, of its inode, with its file's chunk hashes.
const repairCopySQL = `
	SELECT c.file_id, c.size, c.mtime_ns, c.ctime_ns, COALESCE(fc.chunks, x'')
	FROM copies c LEFT JOIN file_chunks fc ON fc.file_id = c.file_id
	WHERE c.disk_id = ?1 AND c.file_key = ?3 AND c.size IS NOT NULL AND c.mtime_ns IS NOT NULL AND c.ctime_ns IS NOT NULL AND ` + copyAtPlace

// RepairCopyOf gives the copy a repair's operation names, and false when its row is gone or holds no size or times.
func RepairCopyOf(ctx context.Context, db *sql.DB, op *Op) (RepairCopy, bool, error) {
	var c RepairCopy
	err := db.QueryRowContext(ctx, repairCopySQL, op.Disk, []byte(op.Path), op.FileKey).Scan(&c.FileID, &c.Size, &c.MtimeNS, &c.CtimeNS, &c.Chunks)
	switch {
	case isNoRows(err):
		return c, false, nil
	case err != nil:
		return c, false, fmt.Errorf("copy: %w", err)
	}
	return c, true, nil
}

// CorruptCopy is a copy in state corrupt: its file and its disk.
type CorruptCopy struct {
	FileID int64
	Disk   string
}

// CorruptCopies gives the corrupt copies of the files at a path and below it, by whole components, on one disk or
// all of them, by path in byte order, then by disk position (specs.md §16.1, atoll repair).
func CorruptCopies(ctx context.Context, db *sql.DB, under, disk string) ([]CorruptCopy, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.file_id, c.disk_id FROM copies c JOIN files f ON f.id = c.file_id JOIN disks d ON d.id = c.disk_id
		WHERE c.state = 'corrupt' AND (?1 = x'' OR f.lpath = ?1 OR substr(f.lpath, 1, length(?2)) = ?2) AND (?3 = '' OR c.disk_id = ?3)
		ORDER BY f.lpath, d.position`, []byte(under), []byte(under+"/"), disk)
	if err != nil {
		return nil, fmt.Errorf("corrupt copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []CorruptCopy
	for rows.Next() {
		var c CorruptCopy
		if err := rows.Scan(&c.FileID, &c.Disk); err != nil {
			return nil, fmt.Errorf("corrupt copies: %w", err)
		}
		list = append(list, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corrupt copies: %w", err)
	}
	return list, nil
}

// FileChunks gives the hash of each chunk of a file, concatenated: none for an empty file or one not hashed yet.
func FileChunks(ctx context.Context, db *sql.DB, fileID int64) ([]byte, error) {
	var chunks []byte
	err := db.QueryRowContext(ctx, "SELECT chunks FROM file_chunks WHERE file_id = ?", fileID).Scan(&chunks)
	switch {
	case isNoRows(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("chunks of %d: %w", fileID, err)
	}
	return chunks, nil
}

// ClearCorrupt records that a copy found corrupt reads right after all, read whole now: ok, and verified now.
func ClearCorrupt(ctx context.Context, db *sql.DB, fileID int64, disk string, nowNS int64) error {
	if _, err := db.ExecContext(ctx, "UPDATE copies SET state = 'ok', verified_ns = ? WHERE file_id = ? AND disk_id = ? AND state = 'corrupt'", nowNS, fileID, disk); err != nil {
		return fmt.Errorf("copy of %d on %q: %w", fileID, disk, err)
	}
	return nil
}
