package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// EventModified is written for a file whose copies a modification made stale (specs.md §9, §15.4).
const EventModified = "modified"

// ReasonOverwritten sends a stale copy to the trash (specs.md §15.4, §17).
const ReasonOverwritten = "overwritten"

// ReasonRebalance sends to the trash an Atoll copy that atoll balance made again where its file's strict settings
// want it (specs.md §13.3, §17).
const ReasonRebalance = "rebalance"

// What a modification decides (specs.md §15.4).
const (
	ModModified = "modified"
	ModMetadata = "metadata"
	ModConflict = "conflict"
)

// ChangedCopy is a copy that changed, as the look before its read found it. Replaced says that another file took
// its name.
type ChangedCopy struct {
	Disk     string
	FileKey  string
	Nlink    int64
	Size     int64
	MtimeNS  int64
	CtimeNS  int64
	BtimeNS  sql.NullInt64
	Mode     int64
	UID, GID uint32
	Replaced bool
}

// Modification is what a pass decided about a file a copy of which changed. Hash is what the file's hash becomes,
// with its chunks, nil when nothing was read: every changed copy was read when it is not; Reference is the copy read beside, where the file had none; Src is the
// first changed copy, which stale copies are refreshed from; Priority is the rule's.
type Modification struct {
	FileID    int64
	Path      string
	Kind      string
	Changed   []ChangedCopy
	Stale     []string
	Src       string
	Priority  int64
	Hash      []byte
	Chunks    []byte
	Reference string
	// Answered says that a confirm of the mass-modification guard answered it (specs.md §15.6).
	Answered bool
}

// ApplyModification records a modification in one transaction, before anything runs on a disk (specs.md §15.4).
func ApplyModification(ctx context.Context, db *sql.DB, nowNS int64, m *Modification) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		switch m.Kind {
		case ModModified:
			return applyModified(ctx, tx, nowNS, m)
		case ModMetadata:
			return applyMetadata(ctx, tx, nowNS, m)
		case ModConflict:
			return applyConflict(ctx, tx, nowNS, m)
		default:
			return fmt.Errorf("unknown modification %q", m.Kind)
		}
	})
	if err != nil {
		return fmt.Errorf("%q: %w", m.Path, err)
	}
	return nil
}

func applyModified(ctx context.Context, tx *sql.Tx, nowNS int64, m *Modification) error {
	// The event says what the mass-modification guard counts: a file whose other copies went stale (specs.md §15.6).
	if len(m.Stale) > 0 {
		data := eventRow{Disk: m.Src, Answered: m.Answered}
		data.setPath(m.Path)
		if err := insertEvent(ctx, tx, nowNS, "info", EventModified, &data); err != nil {
			return err
		}
	}
	for i := range m.Changed {
		if err := takeLook(ctx, tx, nowNS, m.FileID, &m.Changed[i], true, m.Hash != nil); err != nil {
			return err
		}
	}
	// Its size follows its original, which the end of the pass works out (specs.md §15.12, step 6).
	if _, err := tx.ExecContext(ctx, "UPDATE files SET blake3 = ?, updated_ns = ? WHERE id = ?", m.Hash, nowNS, m.FileID); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	if err := setChunks(ctx, tx, m.FileID, m.Chunks); err != nil {
		return err
	}
	// A deletion no longer stands, and a queued copy may read a stale source: the next plan decides again.
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE file_id = ? AND state = ?", m.FileID, JobQueued); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ? AND state = 'pending'", m.FileID); err != nil {
		return fmt.Errorf("pending copies: %w", err)
	}
	return markStale(ctx, tx, m)
}

// markStale makes copies stale, each that holds a file with a trash job, refreshed from Src once in the trash
// (specs.md §15.4).
func markStale(ctx context.Context, tx *sql.Tx, m *Modification) error {
	for _, disk := range m.Stale {
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET state = 'stale', verified_ns = NULL WHERE file_id = ? AND disk_id = ?", m.FileID, disk); err != nil {
			return fmt.Errorf("stale copy: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs(kind, file_id, src_disk, dst_disk, priority, state, reason) SELECT 'trash', ?1, ?2, ?3, ?4, ?5, ?6
			WHERE EXISTS(SELECT 1 FROM copies WHERE file_id = ?1 AND disk_id = ?3 AND file_key IS NOT NULL)`,
			m.FileID, m.Src, disk, m.Priority, JobQueued, ReasonOverwritten)
		if err != nil {
			return fmt.Errorf("job: %w", err)
		}
	}
	return nil
}

// applyMetadata records a metadata-only change, once its time has reached each copy it could (specs.md §15.4).
func applyMetadata(ctx context.Context, tx *sql.Tx, nowNS int64, m *Modification) error {
	for i := range m.Changed {
		if err := takeLook(ctx, tx, nowNS, m.FileID, &m.Changed[i], false, true); err != nil {
			return err
		}
	}
	if err := markStale(ctx, tx, m); err != nil {
		return err
	}
	if m.Reference == "" {
		return nil
	}
	// The file had no hash: what both copies read is its content.
	if _, err := tx.ExecContext(ctx, "UPDATE files SET blake3 = ?, updated_ns = ? WHERE id = ?", m.Hash, nowNS, m.FileID); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	if err := setChunks(ctx, tx, m.FileID, m.Chunks); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE copies SET verified_ns = ? WHERE file_id = ? AND disk_id = ?", nowNS, m.FileID, m.Reference); err != nil {
		return fmt.Errorf("reference: %w", err)
	}
	return nil
}

// applyConflict keeps every version: nothing says which one a copy's hash describes any more, and a stale copy
// that changed is no longer what its trash job was decided on.
func applyConflict(ctx context.Context, tx *sql.Tx, nowNS int64, m *Modification) error {
	if _, err := tx.ExecContext(ctx, "UPDATE files SET state = 'conflict', updated_ns = ? WHERE id = ? AND state <> 'corrupt'", nowNS, m.FileID); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	for i := range m.Changed {
		disk := m.Changed[i].Disk
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET verified_ns = NULL WHERE file_id = ? AND disk_id = ?", m.FileID, disk); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'trash' AND file_id = ? AND dst_disk = ? AND state = ? AND reason = ?",
			m.FileID, disk, JobQueued, ReasonOverwritten)
		if err != nil {
			return fmt.Errorf("job: %w", err)
		}
	}
	return nil
}

// takeLook gives a changed copy what the look before its read found. A replaced copy is another file, first seen
// now. A modification makes a copy the user's; a metadata change keeps who made it.
func takeLook(ctx context.Context, tx *sql.Tx, nowNS, fileID int64, c *ChangedCopy, makeUser, verified bool) error {
	var verifiedNS sql.NullInt64
	if verified {
		verifiedNS = sql.NullInt64{Int64: nowNS, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE copies SET file_key = ?3, nlink = ?4, size = ?5, mtime_ns = ?6, ctime_ns = ?7, btime_ns = ?8, verified_ns = ?9,
			created_by = CASE WHEN ?10 THEN 'user' ELSE created_by END,
			first_seen_ns = CASE WHEN ?11 THEN ?12 ELSE first_seen_ns END,
			mode = ?13, uid = ?14, gid = ?15
		WHERE file_id = ?1 AND disk_id = ?2`,
		fileID, c.Disk, c.FileKey, c.Nlink, c.Size, c.MtimeNS, c.CtimeNS, c.BtimeNS, verifiedNS, makeUser, c.Replaced, nowNS, c.Mode, c.UID, c.GID)
	if err != nil {
		return fmt.Errorf("copy on %q: %w", c.Disk, err)
	}
	return nil
}

// setChunks gives a file the chunk hashes of its hash, none when it has none. An empty file has no chunk, and so no
// row (specs.md §16).
func setChunks(ctx context.Context, tx *sql.Tx, fileID int64, chunks []byte) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_chunks WHERE file_id = ?", fileID); err != nil {
		return fmt.Errorf("chunks: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO file_chunks(file_id, chunks) VALUES (?, ?)", fileID, chunks); err != nil {
		return fmt.Errorf("chunks: %w", err)
	}
	return nil
}

// refreshStale keeps the row of a stale copy that reached the trash, as a pending copy with its replicate job from
// the copy that changed, and says whether it did: what the stale copy held stays held (specs.md §15.4). Without its
// trash job, which a later decision may have dropped, nothing says where to copy from, and the row goes as for any
// other reason.
func refreshStale(ctx context.Context, tx *sql.Tx, op *Op, fileID int64) (bool, error) {
	var reason string
	var deletedNS int64
	err := tx.QueryRowContext(ctx, "SELECT reason, deleted_ns FROM trash WHERE op_id = ?", op.ID).Scan(&reason, &deletedNS)
	switch {
	case isNoRows(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("trash: %w", err)
	case reason != ReasonOverwritten:
		return false, nil
	}
	var src string
	var priority int64
	err = tx.QueryRowContext(ctx, "SELECT src_disk, priority FROM jobs WHERE kind = 'trash' AND file_id = ? AND dst_disk = ? AND reason = ?",
		fileID, op.Disk, ReasonOverwritten).Scan(&src, &priority)
	switch {
	case isNoRows(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("job: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE copies SET state = 'pending', created_by = 'atoll', file_key = NULL, nlink = NULL, size = NULL, mtime_ns = NULL,
			ctime_ns = NULL, btime_ns = NULL, first_seen_ns = ?3, verified_ns = NULL, gone_ns = NULL, gone_kind = NULL,
			old_lpath = NULL, old_lpath_key = NULL, mode = NULL, uid = NULL, gid = NULL
		WHERE file_id = ?1 AND disk_id = ?2`, fileID, op.Disk, deletedNS)
	if err != nil {
		return false, fmt.Errorf("pending copy: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'trash' AND file_id = ? AND dst_disk = ?", fileID, op.Disk); err != nil {
		return false, fmt.Errorf("job: %w", err)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO jobs(kind, file_id, src_disk, dst_disk, priority, state) VALUES ('replicate', ?, ?, ?, ?, ?)",
		fileID, src, op.Disk, priority, JobQueued)
	if err != nil {
		return false, fmt.Errorf("job: %w", err)
	}
	return true, nil
}
