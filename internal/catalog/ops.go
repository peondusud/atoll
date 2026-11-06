package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/layout"
)

// The kinds of operation this file journals (specs.md §17).
const (
	OpTrash   = "trash"
	OpRestore = "restore"
	OpUnlink  = "unlink"
	OpMkdir   = "mkdir"
)

// Op is one operation of the journal.
type Op struct {
	ID      int64
	Disk    string
	Kind    string
	Path    string // the logical path
	Path2   string // the location inside the trash
	FileKey string // the identity of the file moved
}

// TrashStart is what sending a copy to the trash needs.
type TrashStart struct {
	Disk      string
	Path      string
	FileKey   string
	Size      sql.NullInt64
	Hash      []byte
	Reason    string
	NowNS     int64
	ExpiresNS int64
}

// eventData is the data of an event (specs.md §9).
type eventData struct {
	Op      int64  `json:"op"`
	Disk    string `json:"disk"`
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// writeEvent writes an event, in the transaction of the operation it speaks of, so before it runs.
func writeEvent(ctx context.Context, tx *sql.Tx, nowNS int64, level, kind string, op *Op, reason string) error {
	data := eventData{Op: op.ID, Disk: op.Disk, Reason: reason}
	if utf8.ValidString(op.Path) {
		data.Path = op.Path
	} else {
		data.PathB64 = base64.StdEncoding.EncodeToString([]byte(op.Path))
	}
	text, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO events(ts_ns, level, kind, data) VALUES (?, ?, ?, ?)", nowNS, level, kind, string(text)); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	return nil
}

// BeginTrash journals sending a copy to the trash, before anything is written to a disk. Until its
// operation is done, a trash row says nothing of what the trash holds (specs.md §17.1).
func BeginTrash(ctx context.Context, db *sql.DB, s *TrashStart) (*Op, error) {
	op := &Op{Disk: s.Disk, Kind: OpTrash, Path: s.Path, FileKey: s.FileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES (?, 'trash', ?, ?, ?, 'planned')",
			s.Disk, []byte(s.Path), s.FileKey, s.NowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.Path2, err = layout.TrashPath(s.NowNS, op.ID, s.Path); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE ops SET path2 = ? WHERE id = ?", []byte(op.Path2), op.ID); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, blake3, reason, deleted_ns, expires_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]byte(s.Path), s.Disk, []byte(op.Path2), op.ID, s.Size, s.Hash, s.Reason, s.NowNS, s.ExpiresNS)
		if err != nil {
			return fmt.Errorf("trash: %w", err)
		}
		return writeEvent(ctx, tx, s.NowNS, "info", "trash", op, s.Reason)
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", s.Path, err)
	}
	return op, nil
}

// Restart gives an entry its times again, for a reprise: until then the entry was not listed, so its
// retention starts when it can first be seen, and the run that finishes it cannot purge it too.
type Restart struct {
	DeletedNS int64
	ExpiresNS int64
}

// Links is what the other names of a file must record: what happens to one name moves the change
// time and link count of the file for all of them.
type Links struct {
	FileKey string
	CtimeNS int64
	Nlink   int64
	// PriorNS is the change time the file had just before what happened to it, when that is known. A
	// name that recorded another has a change nobody has looked at, and keeps it for the next scan.
	PriorNS sql.NullInt64
}

func writeLinks(ctx context.Context, tx *sql.Tx, disk string, links *Links) error {
	if links == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE copies SET nlink = ?1, ctime_ns = CASE WHEN ?2 IS NULL OR ctime_ns = ?2 THEN ?3 ELSE ctime_ns END WHERE disk_id = ?4 AND file_key = ?5",
		links.Nlink, links.PriorNS, links.CtimeNS, disk, links.FileKey)
	if err != nil {
		return fmt.Errorf("links: %w", err)
	}
	return nil
}

// FinishTrash says that a copy is in the trash, once that is durable.
func FinishTrash(ctx context.Context, db *sql.DB, op *Op, restart *Restart, links *Links) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if err := dropCopyRow(ctx, tx, op.Disk, op.Path); err != nil {
			return err
		}
		if restart != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE trash SET deleted_ns = ?, expires_ns = ? WHERE op_id = ?", restart.DeletedNS, restart.ExpiresNS, op.ID); err != nil {
				return fmt.Errorf("trash: %w", err)
			}
		}
		return writeLinks(ctx, tx, op.Disk, links)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// CopyOf gives what a copy's row says, with nothing valid in it when there is none.
func CopyOf(ctx context.Context, db *sql.DB, disk, path string) (KnownCopy, error) {
	row := KnownCopy{Path: path}
	err := db.QueryRowContext(ctx, "SELECT c.size, c.mtime_ns, c.ctime_ns, c.file_key FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = ? AND f.lpath = ?", disk, []byte(path)).
		Scan(&row.Size, &row.MtimeNS, &row.CtimeNS, &row.FileKey)
	if err != nil && !isNoRows(err) {
		return row, fmt.Errorf("copy: %w", err)
	}
	return row, nil
}

// KeyTaken says whether a disk holds a name with the same lpath_key as path, and other bytes
// (specs.md §15.7). A name without a key never collides.
func KeyTaken(ctx context.Context, db *sql.DB, disk, path string) (bool, error) {
	key := keyOf(path)
	if !key.Valid {
		return false, nil
	}
	var taken bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM files f JOIN copies c ON c.file_id = f.id WHERE c.disk_id = ?1 AND f.lpath_key = ?2 AND f.lpath <> ?3)
			OR EXISTS(SELECT 1 FROM entries WHERE disk_id = ?1 AND lpath_key = ?2 AND lpath <> ?3)`,
		disk, key, []byte(path)).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("names: %w", err)
	}
	return taken, nil
}

// LinkedPaths gives the paths whose copy on a disk is that file, by what the catalog knows.
func LinkedPaths(ctx context.Context, db *sql.DB, disk, fileKey string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT f.lpath FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = ? AND c.file_key = ? ORDER BY f.lpath", disk, fileKey)
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var path []byte
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("links: %w", err)
		}
		paths = append(paths, string(path))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	return paths, nil
}

// CopyCtimes gives the change time each copy of a file records now, by disk.
func CopyCtimes(ctx context.Context, db *sql.DB, fileID int64) (map[string]sql.NullInt64, error) {
	rows, err := db.QueryContext(ctx, "SELECT disk_id, ctime_ns FROM copies WHERE file_id = ?", fileID)
	if err != nil {
		return nil, fmt.Errorf("copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]sql.NullInt64{}
	for rows.Next() {
		var disk string
		var ctime sql.NullInt64
		if err := rows.Scan(&disk, &ctime); err != nil {
			return nil, fmt.Errorf("copies: %w", err)
		}
		out[disk] = ctime
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copies: %w", err)
	}
	return out, nil
}

// BeginMkdir journals a folder made below the root (specs.md §6.1, §17.4).
func BeginMkdir(ctx context.Context, db *sql.DB, disk, path string, nowNS int64) (*Op, error) {
	result, err := db.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES (?, 'mkdir', ?, ?, 'planned')", disk, []byte(path), nowNS)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return &Op{ID: id, Disk: disk, Kind: OpMkdir, Path: path}, nil
}

// FinishMkdir says that a folder is there, and Atoll's own.
func FinishMkdir(ctx context.Context, db *sql.DB, op *Op, e *EntryRow) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO entries(lpath, lpath_key, disk_id, kind, mode, mtime_ns, created_by)
			VALUES (?, ?, ?, 'dir', ?, ?, 'atoll')
			ON CONFLICT(lpath, disk_id) DO UPDATE SET kind = excluded.kind, mode = excluded.mode, mtime_ns = excluded.mtime_ns, created_by = 'atoll'`,
			[]byte(e.Path), keyOf(e.Path), op.Disk, e.Mode, e.MtimeNS)
		if err != nil {
			return fmt.Errorf("entry %q: %w", e.Path, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

func markOp(ctx context.Context, tx *sql.Tx, id int64, state string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE ops SET state = ? WHERE id = ?", state, id); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

func dropCopyRow(ctx context.Context, tx *sql.Tx, disk, path string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE disk_id = ? AND file_id = (SELECT id FROM files WHERE lpath = ?)", disk, []byte(path))
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// Undo says that an operation did not happen. warn says that what was found is not what an
// operation of Atoll leaves (specs.md §17.2).
func Undo(ctx context.Context, db *sql.DB, op *Op, warn bool, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "undone"); err != nil {
			return err
		}
		// Only a trash operation has a row of its own: the row of a restore or a purge is the
		// entry's, whose operation was done long ago, and it stays.
		if _, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE op_id = ?", op.ID); err != nil {
			return fmt.Errorf("trash: %w", err)
		}
		if warn {
			return writeEvent(ctx, tx, nowNS, "warning", "op-undone", op, "")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// PlannedOps gives the operations left planned, by id.
func PlannedOps(ctx context.Context, db *sql.DB) ([]Op, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, disk_id, kind, path, COALESCE(path2, x''), COALESCE(file_key, '') FROM ops WHERE state = 'planned' ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []Op{}
	for rows.Next() {
		var op Op
		var path, path2 []byte
		if err := rows.Scan(&op.ID, &op.Disk, &op.Kind, &path, &path2, &op.FileKey); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
		op.Path, op.Path2 = string(path), string(path2)
		list = append(list, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return list, nil
}

// TrashEntry is what a disk's trash holds at one location.
type TrashEntry struct {
	Path      string
	Disk      string
	TrashPath string
	OpID      int64
	Size      sql.NullInt64
	Hash      []byte
	Reason    string
	DeletedNS int64
	ExpiresNS int64
	FileKey   string
}

// trashPageSize is how many entries EachTrashEntry reads at a time.
const trashPageSize = 1000

// EachTrashEntry gives each entry whose operation is done, by path in byte order, then by operation,
// a page at a time: no read transaction stays open while a reader is slow, and nothing is kept.
func EachTrashEntry(ctx context.Context, db *sql.DB, fn func(*TrashEntry) error) error {
	after, afterOp := []byte{}, int64(0)
	for {
		page, err := trashPage(ctx, db, after, afterOp)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for i := range page {
			if err := fn(&page[i]); err != nil {
				return err
			}
		}
		last := &page[len(page)-1]
		after, afterOp = []byte(last.Path), last.OpID
	}
}

func trashPage(ctx context.Context, db *sql.DB, after []byte, afterOp int64) ([]TrashEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.lpath, t.disk_id, t.trash_path, t.op_id, t.size, t.blake3, t.reason, t.deleted_ns, t.expires_ns, COALESCE(o.file_key, '')
		FROM trash t JOIN ops o ON o.id = t.op_id
		WHERE o.state = 'done' AND (t.lpath, t.op_id) > (?, ?)
		ORDER BY t.lpath, t.op_id
		LIMIT ?`, after, afterOp, trashPageSize)
	if err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []TrashEntry{}
	for rows.Next() {
		var e TrashEntry
		var path, trashPath []byte
		if err := rows.Scan(&path, &e.Disk, &trashPath, &e.OpID, &e.Size, &e.Hash, &e.Reason, &e.DeletedNS, &e.ExpiresNS, &e.FileKey); err != nil {
			return nil, fmt.Errorf("trash: %w", err)
		}
		e.Path, e.TrashPath = string(path), string(trashPath)
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	return list, nil
}

// TrashEntries gives every entry EachTrashEntry would.
func TrashEntries(ctx context.Context, db *sql.DB) ([]TrashEntry, error) {
	list := []TrashEntry{}
	err := EachTrashEntry(ctx, db, func(e *TrashEntry) error {
		list = append(list, *e)
		return nil
	})
	return list, err
}

// BeginFromTrash journals an operation on an entry of the trash: a restore, or a purge.
func BeginFromTrash(ctx context.Context, db *sql.DB, kind string, e *TrashEntry, nowNS int64) (*Op, error) {
	op := &Op{Disk: e.Disk, Kind: kind, Path: e.Path, Path2: e.TrashPath, FileKey: e.FileKey}
	event := "purge"
	if kind == OpRestore {
		event = "restore"
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, path2, file_key, started_ns, state) VALUES (?, ?, ?, ?, ?, ?, 'planned')",
			e.Disk, kind, []byte(e.Path), []byte(e.TrashPath), e.FileKey, nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		return writeEvent(ctx, tx, nowNS, "info", event, op, "")
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", e.Path, err)
	}
	return op, nil
}

func dropTrashRow(ctx context.Context, tx *sql.Tx, op *Op) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE disk_id = ? AND trash_path = ?", op.Disk, []byte(op.Path2)); err != nil {
		return fmt.Errorf("trash: %w", err)
	}
	return nil
}

// FinishPurge says that an entry is gone.
func FinishPurge(ctx context.Context, db *sql.DB, op *Op, links *Links) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if err := dropTrashRow(ctx, tx, op); err != nil {
			return err
		}
		return writeLinks(ctx, tx, op.Disk, links)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// FinishRestore says that an entry is back at its path. The copy follows what a scan does with a
// copy not yet in the catalog (specs.md §15.10, §17.4), and a row left from before says nothing of
// it, as the path was free when the file came back.
func FinishRestore(ctx context.Context, db *sql.DB, op *Op, nowNS int64, c *NewCopy, links *Links) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if err := dropTrashRow(ctx, tx, op); err != nil {
			return err
		}
		if err := dropCopyRow(ctx, tx, op.Disk, op.Path); err != nil {
			return err
		}
		if err := writeCopies(ctx, tx, op.Disk, nowNS, []NewCopy{*c}); err != nil {
			return err
		}
		return writeLinks(ctx, tx, op.Disk, links)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}
