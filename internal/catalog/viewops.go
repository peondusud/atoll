package catalog

import (
	"context"
	"database/sql"
	"errors"
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
	// ViewXattr is an extended attribute of the user. namespace set or removed on a user's behalf: on the disk a file is
	// presented from, FileKey its inode, or on every disk that holds a folder (specs.md §21.2).
	ViewXattr   = "xattr"
	ViewRename  = "rename"
	ViewReplace = "replace"
	// ViewLink is a hard link the view made on one disk: Path the name it was made from, Path2 the new name, FileKey
	// the inode both lead to (specs.md §21.2).
	ViewLink   = "link"
	ViewUnlink = "unlink"
	ViewRmdir  = "rmdir"
	// ViewMove is a new file the view moved to another disk once a write ran out of space: Disk the disk it goes to,
	// Disk2 the full disk it leaves, FileKey its inode there, and OpID the operation of ops the move runs now, the create
	// of its part on Disk, then the trash of the copy it leaves on Disk2, then the unlink that purges it (specs.md §9,
	// §21.2).
	ViewMove = "move"
)

// The states of an operation, of ops and of view_ops alike (specs.md §9).
const (
	OpPlanned = "planned"
	OpDone    = "done"
	OpUndone  = "undone"
	// ViewHeld is a write, or a new file, that the view holds open, view_ops alone: done once it lets go of its last
	// handle on the file, and never taken in before (specs.md §21.2; conformance/models/README.md, flaw 10).
	ViewHeld = "open"
)

// ViewOp is one operation of the view, as view_ops holds it.
type ViewOp struct {
	ID    int64
	Kind  string
	Disk  string
	Path  string
	Path2 string // the new path of a rename, a replacement or a hard link
	// Folder says that it names a folder.
	Folder bool
	// FileKey is the inode of what a rename, a replacement or a deletion moved, or a hard link gave a name, in decimal.
	FileKey string
	// OpID is the operation of ops that sent a copy to the trash, for a deletion or a replacement, the one a part or a
	// move runs, 0 otherwise.
	OpID  int64
	State string
	AtNS  int64
	// Disk2 is, for a move, the full disk the file leaves; OpKind the kind of the operation of ops OpID names, and
	// OpFileKey its file_key, as OpenViewOps reads them: of a replacement, the inode of the copy it sent to the trash.
	Disk2     string
	OpKind    string
	OpFileKey string
	// Asker is who asked it, as the kernel named the caller of its FUSE request: nil for a row written outside one,
	// and for one from before migration 0025 (specs.md §9, §21.2).
	Asker *ViewAsker
}

// ViewAsker is who asked an operation of the view: the account, and the thread, 0 when the kernel named none.
type ViewAsker struct {
	UID, PID uint32
}

// askerArgs gives what view_ops.uid and view_ops.pid hold of who asked.
func askerArgs(a *ViewAsker) (uid, pid sql.NullInt64) {
	if a == nil {
		return uid, pid
	}
	uid = sql.NullInt64{Int64: int64(a.UID), Valid: true}
	pid = sql.NullInt64{Int64: int64(a.PID), Valid: a.PID > 0}
	return uid, pid
}

// AtSource says that a move still presents the copy on the disk it leaves: planned, and the operation it names still
// the create of its part, whose copy is not at its name on the disk it goes to yet (specs.md §21.2).
func (v *ViewOp) AtSource() bool {
	return v.Kind == ViewMove && v.State == OpPlanned && v.OpKind == OpCreate
}

const insertViewOpSQL = `
	INSERT INTO view_ops(kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, disk2_id, uid, pid)
	VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, 0), ?, ?, NULLIF(?, ''), ?, ?)`

func insertViewOp(ctx context.Context, tx *sql.Tx, v *ViewOp) error {
	var path2 any
	if v.Kind == ViewRename || v.Kind == ViewReplace || v.Kind == ViewLink {
		path2 = []byte(v.Path2)
	}
	disk2 := ""
	if v.Kind == ViewMove {
		disk2 = v.Disk2
	}
	uid, pid := askerArgs(v.Asker)
	result, err := tx.ExecContext(ctx, insertViewOpSQL, v.Kind, v.Disk, []byte(v.Path), path2, v.Folder, v.FileKey, v.OpID, v.State, v.AtNS, disk2, uid, pid)
	if err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	if v.ID, err = result.LastInsertId(); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	return nil
}

// BeginViewOp journals an operation of the view before it runs, planned; a write, which moves no name, is done at
// once (specs.md §9, §21.2), or held, while the view holds the file open to write. A held write waits while a move of
// Atoll's own of the copy it writes is planned: ErrMovePlanned, checked in the transaction that journals it, as the
// transfer of the SSD cache checks the view's operations in its own (flaw 10).
func BeginViewOp(ctx context.Context, db *sql.DB, v *ViewOp) error {
	held := v.State == ViewHeld && v.Kind == ViewWrite
	v.State = OpPlanned
	if v.Kind == ViewWrite {
		v.State = OpDone
		if held {
			v.State = ViewHeld
		}
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if held {
			if planned, err := movePlannedAt(ctx, tx, v.Disk, v.Path); err != nil || planned {
				return cmpErr(err, ErrMovePlanned)
			}
		}
		return insertViewOp(ctx, tx, v)
	})
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

// ErrMovePlanned says that a move of Atoll's own of the copy at a path is planned: what would write to it waits.
var ErrMovePlanned = errors.New("a move of Atoll's own of that copy is planned")

// ErrViewHolds says that an operation of the view no pass has taken in touches a path: Atoll's own move waits.
var ErrViewHolds = errors.New("an operation of the view touches that path")

// cmpErr gives err, or want when err is nil.
func cmpErr(err, want error) error {
	if err != nil {
		return err
	}
	return want
}

// movePlannedAt says whether a move to the trash of the copy at a path on a disk is planned, of Atoll's own: an
// operation of ops no row of view_ops names.
func movePlannedAt(ctx context.Context, tx *sql.Tx, disk, path string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ops o WHERE o.state = 'planned' AND o.kind = 'trash' AND o.disk_id = ?
		AND o.path = ? AND NOT EXISTS (SELECT 1 FROM view_ops v WHERE v.op_id = o.id)`, disk, []byte(path)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	return n > 0, nil
}

// MovePlannedAt is movePlannedAt, on its own.
func MovePlannedAt(ctx context.Context, db *sql.DB, disk, path string) (bool, error) {
	var planned bool
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		var err error
		planned, err = movePlannedAt(ctx, tx, disk, path)
		return err
	})
	return planned, err
}

// viewTouches says whether an operation of the view no pass has taken in touches a path: at it, above it or below it.
func viewTouches(ctx context.Context, tx *sql.Tx, path string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT lpath, COALESCE(lpath2, x'') FROM view_ops WHERE taken_ns IS NULL AND state <> 'undone'")
	if err != nil {
		return false, fmt.Errorf("view journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	below := func(p, at string) bool { return p == at || strings.HasPrefix(p, at+"/") }
	for rows.Next() {
		var lpath, lpath2 []byte
		if err := rows.Scan(&lpath, &lpath2); err != nil {
			return false, fmt.Errorf("view journal: %w", err)
		}
		for _, at := range []string{string(lpath), string(lpath2)} {
			if at != "" && (below(path, at) || below(at, path)) {
				return true, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("view journal: %w", err)
	}
	return false, nil
}

// viewHoldsRename says whether an operation of the view no pass has taken in, of an id at since or past it, is at, below
// or above path or target, other than the rename of path to target itself (ViewHold), and other than one before such a
// rename not taken in: what Atoll's rename carries out then is that rename of the view, which undoes the earlier ones at
// its names (conformance/models/README.md, observation 5, the maintainer's choice, 2026-11-24).
func viewHoldsRename(ctx context.Context, tx *sql.Tx, path, target string, since int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, kind, lpath, COALESCE(lpath2, x'') FROM view_ops WHERE taken_ns IS NULL AND state <> 'undone' ORDER BY id")
	if err != nil {
		return false, fmt.Errorf("view journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type open struct {
		id            int64
		kind          string
		lpath, lpath2 string
	}
	var list []open
	var same int64
	for rows.Next() {
		var o open
		var lpath, lpath2 []byte
		if err := rows.Scan(&o.id, &o.kind, &lpath, &lpath2); err != nil {
			return false, fmt.Errorf("view journal: %w", err)
		}
		o.lpath, o.lpath2 = string(lpath), string(lpath2)
		if o.kind == ViewRename && o.lpath == path && o.lpath2 == target {
			same = o.id
			continue
		}
		list = append(list, o)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("view journal: %w", err)
	}
	below := func(p, at string) bool { return p == at || strings.HasPrefix(p, at+"/") }
	for _, o := range list {
		if o.id < since || o.id < same {
			continue
		}
		for _, at := range []string{o.lpath, o.lpath2} {
			for _, p := range []string{path, target} {
				if at != "" && (below(p, at) || below(at, p)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// ViewHoldsRename says what BeginHeldRename would read of the view's journal for that rename, before anything is made
// for it, such as the folders missing on the way to its target.
func ViewHoldsRename(ctx context.Context, db *sql.DB, path, target string, hold *ViewHold) (bool, error) {
	var held bool
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		var err error
		held, err = viewHoldsRename(ctx, tx, path, target, hold.Since)
		return err
	})
	return held, err
}

// NextViewOp gives the id the view's next operation will have, at least: one past the last journaled (specs.md §21.2).
func NextViewOp(ctx context.Context, db *sql.DB) (int64, error) {
	var next int64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) + 1 FROM view_ops").Scan(&next); err != nil {
		return 0, fmt.Errorf("view journal: %w", err)
	}
	return next, nil
}

// NameViewFolder says, of a folder's rename through the view still planned, the disk its next rename runs on and that
// folder's inode there, committed before that rename, so that its reprise knows the folder by its inode (specs.md
// §9, §21.2; conformance/models/README.md, flaw 19, the maintainer's remedy, 2026-11-24).
func NameViewFolder(ctx context.Context, db *sql.DB, id int64, disk, fileKey string) error {
	if _, err := db.ExecContext(ctx, "UPDATE view_ops SET disk_id = ?, file_key = ? WHERE id = ? AND state = 'planned'", disk, fileKey, id); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	return nil
}

// HoldViewOp says that the view holds open again a write or a new file it journaled, done and not taken in yet. It
// waits, as a new held write does, while a move of Atoll's own of that copy is planned: ErrMovePlanned. No such move
// is journaled while the row is not taken in (TrashStart.ViewFree), and this keeps it so whatever journaled one.
func HoldViewOp(ctx context.Context, db *sql.DB, id int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		var disk string
		var path []byte
		switch err := tx.QueryRowContext(ctx, "SELECT disk_id, lpath FROM view_ops WHERE id = ?", id).Scan(&disk, &path); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("view journal: %w", err)
		}
		if planned, err := movePlannedAt(ctx, tx, disk, string(path)); err != nil || planned {
			return cmpErr(err, ErrMovePlanned)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE view_ops SET state = 'open' WHERE id = ? AND state = 'done' AND taken_ns IS NULL", id); err != nil {
			return fmt.Errorf("view journal: %w", err)
		}
		return nil
	})
}

// ReleaseViewOp says that the view let go of its last handle on a file it held open: done.
func ReleaseViewOp(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, "UPDATE view_ops SET state = 'done' WHERE id = ? AND state = 'open'", id); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	return nil
}

// ReleaseViewOps says that the view holds nothing open, as at its start: every row held open is done.
func ReleaseViewOps(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "UPDATE view_ops SET state = 'done' WHERE state = 'open'"); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
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
	SELECT id, kind, disk_id, lpath, COALESCE(lpath2, x''), folder, COALESCE(file_key, ''), COALESCE(op_id, 0), state, at_ns,
		COALESCE(disk2_id, ''), COALESCE((SELECT o.kind FROM ops o WHERE o.id = view_ops.op_id), ''),
		COALESCE((SELECT o.file_key FROM ops o WHERE o.id = view_ops.op_id), ''), uid, pid
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
		var uid, pid sql.NullInt64
		if err := rows.Scan(&v.ID, &v.Kind, &v.Disk, &path, &path2, &v.Folder, &v.FileKey, &v.OpID, &v.State, &v.AtNS, &v.Disk2, &v.OpKind, &v.OpFileKey, &uid, &pid); err != nil {
			return nil, fmt.Errorf("view journal: %w", err)
		}
		v.Path, v.Path2 = string(path), string(path2)
		if uid.Valid {
			v.Asker = &ViewAsker{UID: uint32(uid.Int64), PID: uint32(pid.Int64)} //nolint:gosec // the schema keeps both in range
		}
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

// viewDeletionOf says that an operation of ops sent a copy to the trash for a deletion or a replacement through the
// view, which a user made: ?1 the operation's id.
const viewDeletionOf = "SELECT 1 FROM view_ops v WHERE v.op_id = ?1 AND v.kind IN ('unlink', 'replace')"

// IsViewDeletion says whether an operation of the journal sent a copy to the trash for a deletion or a replacement a
// user made through Atoll's own view: a user's, never Atoll's own. What a move of the view sent there is Atoll's own
// (specs.md §15.5, §21.2).
func IsViewDeletion(ctx context.Context, db *sql.DB, opID int64) (bool, error) {
	var is bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS("+viewDeletionOf+")", opID).Scan(&is); err != nil {
		return false, fmt.Errorf("view journal: %w", err)
	}
	return is, nil
}

// groupHoldersSQL reads the files below a folder through files' unique index on lpath, ?1 the folder followed by "/",
// ?2 by "0", the byte after it.
const groupHoldersSQL = `
	SELECT c.disk_id, f.lpath FROM files f JOIN copies c ON c.file_id = f.id
	WHERE f.lpath >= ?1 AND f.lpath < ?2 AND c.state <> 'pending'`

// GroupHolders counts, for each disk, the files below a folder with a copy there that is not pending: those directly in
// it, or those at any depth below it when deep says so, that in says are of the group. It is what colocate weighs of a
// disk for a file new in that folder (specs.md §12.4).
func GroupHolders(ctx context.Context, db *sql.DB, folder string, deep bool, in func(path string) bool) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, groupHoldersSQL, []byte(folder+"/"), []byte(folder+"0"))
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var disk string
		var path []byte
		if err := rows.Scan(&disk, &path); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if (deep || !strings.Contains(string(path[len(folder)+1:]), "/")) && in(string(path)) {
			out[disk]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return out, nil
}
