package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// The kinds of what Atoll's own view does on a user's behalf (specs.md §9, §21.2).
const (
	ViewCreate  = "create"
	ViewMkdir   = "mkdir"
	ViewSymlink = "symlink"
	ViewWrite   = "write"
	ViewRename  = "rename"
	ViewReplace = "replace"
	ViewUnlink  = "unlink"
	ViewRmdir   = "rmdir"
)

// The states of an operation, of ops and of view_ops alike (specs.md §9).
const (
	OpPlanned = "planned"
	OpDone    = "done"
	OpUndone  = "undone"
)

// ViewOp is one operation of the view, as view_ops holds it.
type ViewOp struct {
	ID    int64
	Kind  string
	Disk  string
	Path  string
	Path2 string // the new path of a rename or a replacement
	// Folder says that it names a folder.
	Folder bool
	// FileKey is the inode of what a rename, a replacement or a deletion moved, in decimal.
	FileKey string
	// OpID is the operation of ops that sent a copy to the trash, for a deletion or a replacement, 0 otherwise.
	OpID  int64
	State string
	AtNS  int64
}

const insertViewOpSQL = `
	INSERT INTO view_ops(kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns)
	VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, 0), ?, ?)`

func insertViewOp(ctx context.Context, tx *sql.Tx, v *ViewOp) error {
	var path2 any
	if v.Kind == ViewRename || v.Kind == ViewReplace {
		path2 = []byte(v.Path2)
	}
	result, err := tx.ExecContext(ctx, insertViewOpSQL, v.Kind, v.Disk, []byte(v.Path), path2, v.Folder, v.FileKey, v.OpID, v.State, v.AtNS)
	if err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	if v.ID, err = result.LastInsertId(); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	return nil
}

// BeginViewOp journals an operation of the view before it runs, planned; a write, which moves no name, is done at
// once (specs.md §9, §21.2).
func BeginViewOp(ctx context.Context, db *sql.DB, v *ViewOp) error {
	v.State = OpPlanned
	if v.Kind == ViewWrite {
		v.State = OpDone
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error { return insertViewOp(ctx, tx, v) })
	if err != nil {
		return fmt.Errorf("%q: %w", v.Path, err)
	}
	return nil
}

// BeginViewTrash journals a deletion or a replacement through the view: its own row, the operation that sends the
// copy presented to the trash, that copy's trash row and the event, in one transaction (specs.md §17.1, §21.2).
func BeginViewTrash(ctx context.Context, db *sql.DB, v *ViewOp, s *TrashStart) (*Op, error) {
	var op *Op
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		var err error
		if op, err = beginTrashTx(ctx, tx, s); err != nil {
			return err
		}
		v.OpID, v.State = op.ID, OpPlanned
		return insertViewOp(ctx, tx, v)
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", v.Path, err)
	}
	return op, nil
}

// FinishViewOp says what became of an operation of the view, done or undone, and of its operation of ops with it. A
// copy the view sent to the trash keeps its row: the pass that takes the operation in decides what its going means,
// as it does of a copy gone from its disk (specs.md §15.5, §21.2). An undone one loses its trash row, and warn writes
// an op-undone event, as §17.2 says.
func FinishViewOp(ctx context.Context, db *sql.DB, v *ViewOp, state string, warn bool, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE view_ops SET state = ? WHERE id = ?", state, v.ID); err != nil {
			return fmt.Errorf("view journal: %w", err)
		}
		if v.OpID == 0 {
			return nil
		}
		if err := markOp(ctx, tx, v.OpID, state); err != nil {
			return err
		}
		if state != OpUndone {
			return nil
		}
		if _, err := tx.ExecContext(ctx, undoTrashSQL, []byte(v.Path2OrPath()), v.OpID); err != nil {
			return fmt.Errorf("trash: %w", err)
		}
		if warn {
			op := &Op{ID: v.OpID, Disk: v.Disk, Kind: OpTrash, Path: v.Path2OrPath(), FileKey: v.FileKey}
			return writeEvent(ctx, tx, nowNS, "warning", "op-undone", op, "")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", v.Path, err)
	}
	v.State = state
	return nil
}

// Path2OrPath is the path of the copy an operation sent to the trash: the name a replacement landed on, the path
// of a deletion.
func (v *ViewOp) Path2OrPath() string {
	if v.Kind == ViewReplace {
		return v.Path2
	}
	return v.Path
}

const openViewOpsSQL = `
	SELECT id, kind, disk_id, lpath, COALESCE(lpath2, x''), folder, COALESCE(file_key, ''), COALESCE(op_id, 0), state, at_ns
	FROM view_ops WHERE taken_ns IS NULL AND state <> 'undone' ORDER BY id`

// OpenViewOps gives the operations of the view that no pass has taken in, planned or done, by id: what the view
// presents over the catalog's rows (specs.md §21.2).
func OpenViewOps(ctx context.Context, db *sql.DB) ([]ViewOp, error) {
	rows, err := db.QueryContext(ctx, openViewOpsSQL)
	if err != nil {
		return nil, fmt.Errorf("view journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []ViewOp{}
	for rows.Next() {
		var v ViewOp
		var path, path2 []byte
		if err := rows.Scan(&v.ID, &v.Kind, &v.Disk, &path, &path2, &v.Folder, &v.FileKey, &v.OpID, &v.State, &v.AtNS); err != nil {
			return nil, fmt.Errorf("view journal: %w", err)
		}
		v.Path, v.Path2 = string(path), string(path2)
		list = append(list, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("view journal: %w", err)
	}
	return list, nil
}

// TakeViewOps says that a pass took operations of the view in (specs.md §21.2).
func TakeViewOps(ctx context.Context, db *sql.DB, ids []int64, nowNS int64) error {
	if len(ids) == 0 {
		return nil
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		for chunk := range slices.Chunk(ids, 500) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, nowNS)
			for _, id := range chunk {
				args = append(args, id)
			}
			query := "UPDATE view_ops SET taken_ns = ? WHERE taken_ns IS NULL AND id IN (?" + strings.Repeat(", ?", len(chunk)-1) + ")"
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("view journal: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	return nil
}

// longestBelowSQL reads the copies below a folder through files' unique index on lpath, those behind at an old path
// through copies_old, and the entries through their key, as each holds the paths in byte order: ?2 is the folder
// followed by "/", ?3 by "0", the byte after it.
const longestBelowSQL = `
	SELECT COALESCE(max(n), 0) FROM (
		SELECT length(f.lpath) AS n FROM files f JOIN copies c ON c.file_id = f.id
			WHERE f.lpath >= ?2 AND f.lpath < ?3 AND c.disk_id = ?1 AND c.old_lpath IS NULL
		UNION ALL SELECT length(old_lpath) FROM copies WHERE disk_id = ?1 AND old_lpath >= ?2 AND old_lpath < ?3
		UNION ALL SELECT length(lpath) FROM entries WHERE disk_id = ?1 AND lpath >= ?2 AND lpath < ?3)`

// LongestBelow gives the length in bytes of the longest path a disk holds below a folder, 0 when it holds none: what a
// rename of that folder must still fit in its branch (specs.md §15.8, §21.2).
func LongestBelow(ctx context.Context, db *sql.DB, disk, folder string) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, longestBelowSQL, disk, []byte(folder+"/"), []byte(folder+"0")).Scan(&n); err != nil {
		return 0, fmt.Errorf("catalog: %w", err)
	}
	return n, nil
}

// OpByID gives an operation of the journal, and false when there is none of that id.
func OpByID(ctx context.Context, db *sql.DB, id int64) (Op, bool, error) {
	var op Op
	var path, path2 []byte
	err := db.QueryRowContext(ctx, "SELECT id, disk_id, kind, path, COALESCE(path2, x''), COALESCE(file_key, ''), COALESCE(link, 0) FROM ops WHERE id = ?", id).
		Scan(&op.ID, &op.Disk, &op.Kind, &path, &path2, &op.FileKey, &op.Link)
	switch {
	case isNoRows(err):
		return Op{}, false, nil
	case err != nil:
		return Op{}, false, fmt.Errorf("journal: %w", err)
	}
	op.Path, op.Path2 = string(path), string(path2)
	return op, true, nil
}

// FolderDisks gives the disks whose entry says a folder is at a path, in byte order.
func FolderDisks(ctx context.Context, db *sql.DB, path string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT disk_id FROM entries WHERE lpath = ? AND kind = 'dir' ORDER BY disk_id", []byte(path))
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var disk string
		if err := rows.Scan(&disk); err != nil {
			return nil, fmt.Errorf("entries: %w", err)
		}
		out = append(out, disk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	return out, nil
}

// IsViewOp says whether an operation of the journal is one of Atoll's own view's.
func IsViewOp(ctx context.Context, db *sql.DB, opID int64) (bool, error) {
	var is bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM view_ops WHERE op_id = ?)", opID).Scan(&is); err != nil {
		return false, fmt.Errorf("view journal: %w", err)
	}
	return is, nil
}
