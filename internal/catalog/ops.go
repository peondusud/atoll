package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/layout"
)

// The kinds of operation this file journals (specs.md §17).
const (
	OpTrash   = "trash"
	OpRestore = "restore"
	OpUnlink  = "unlink"
	OpMkdir   = "mkdir"
	OpRename  = "rename"
	OpCreate  = "create"
	// OpLink makes the link guard's link of a copy (specs.md §17).
	OpLink = "link"
)

// Op is one operation of the journal.
type Op struct {
	ID      int64
	Disk    string
	Kind    string
	Path    string // the logical path, or for a rename where it sat
	Path2   string // the location inside the trash, or for a rename the target
	FileKey string // the identity of the file moved
	// Link is the id of the file whose link in <workdir>/links/ the operation is about, 0 for any other: the source of
	// a trash, or what a link or unlink makes or removes (specs.md §17).
	Link int64
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
	// Link says that what goes is the link guard's link of that file, rather than the copy at Path (specs.md §17).
	Link int64
}

// eventData is the data of an event (specs.md §9).
type eventData struct {
	Op      int64  `json:"op"`
	Disk    string `json:"disk"`
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Source  string `json:"source,omitempty"`
	MtimeNS *int64 `json:"mtime_ns,omitempty"`
	// Chunks holds the indexes of the bad chunks of a repair (specs.md §16.1).
	Chunks []int64 `json:"chunks,omitempty"`
	Mode   *int64  `json:"mode,omitempty"`
	UID    *int64  `json:"uid,omitempty"`
	GID    *int64  `json:"gid,omitempty"`
	// Link is the file whose link in <workdir>/links/ went to the trash (specs.md §17).
	Link int64 `json:"link,omitempty"`
}

// writeEvent writes an event, in the transaction of the operation it speaks of, so before it runs.
func writeEvent(ctx context.Context, tx *sql.Tx, nowNS int64, level, kind string, op *Op, reason string) error {
	return writeEventData(ctx, tx, nowNS, level, kind, op, eventData{Reason: reason})
}

func writeEventData(ctx context.Context, tx *sql.Tx, nowNS int64, level, kind string, op *Op, data eventData) error {
	data.Op, data.Disk, data.Link = op.ID, op.Disk, op.Link
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
	op := &Op{Disk: s.Disk, Kind: OpTrash, Path: s.Path, FileKey: s.FileKey, Link: s.Link}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state, link) VALUES (?, 'trash', ?, ?, ?, 'planned', ?)",
			s.Disk, []byte(s.Path), s.FileKey, s.NowNS, linkOf(s.Link))
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

// What the names of a file on a disk record when one of them changes, and what they are: each finds the copies of a
// disk by their inode. An operation on a file with more than one name asks once, and with the link guard on every copy
// has two: each trash, purge, restore and rename, and each link the guard makes or removes, the first pass with the
// guard on making one for each copy of the disk (specs.md §26).
const (
	writeLinksSQL  = "UPDATE copies SET nlink = ?1, ctime_ns = CASE WHEN ?2 IS NULL OR ctime_ns = ?2 THEN ?3 ELSE ctime_ns END WHERE disk_id = ?4 AND file_key = ?5"
	linkedPathsSQL = "SELECT COALESCE(c.old_lpath, f.lpath) AS place FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = ? AND c.file_key = ? ORDER BY place"
)

func writeLinks(ctx context.Context, tx *sql.Tx, disk string, links *Links) error {
	if links == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, writeLinksSQL, links.Nlink, links.PriorNS, links.CtimeNS, disk, links.FileKey)
	if err != nil {
		return fmt.Errorf("links: %w", err)
	}
	return nil
}

// FinishTrash says that a copy is in the trash, once that is durable. A link of the link guard that went there was no
// copy: no row goes with it (specs.md §17).
func FinishTrash(ctx context.Context, db *sql.DB, op *Op, restart *Restart, links *Links) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if op.Link == 0 {
			if err := finishCopy(ctx, tx, op); err != nil {
				return err
			}
		}
		if restart != nil {
			if _, err := tx.ExecContext(ctx, restartTrashSQL, restart.DeletedNS, restart.ExpiresNS, []byte(op.Path), op.ID); err != nil {
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

// What finds a copy by its place on a disk: ?1 the disk, ?2 the place, ?3 the inode. A copy sits at its old path
// while a rename is pending, at its file's path otherwise. Each half of the union reads one index: one OR over
// copies and files read every copy of the disk, once for each operation, and a pass that applied a time to 200,000
// copies did 6 a second (specs.md §26).
const (
	copyAtPlace = `c.file_id IN (
		SELECT file_id FROM copies WHERE disk_id = ?1 AND old_lpath = ?2
		UNION ALL
		SELECT o.file_id FROM files g JOIN copies o ON o.file_id = g.id WHERE g.lpath = ?2 AND o.disk_id = ?1 AND o.old_lpath IS NULL)`
	// Two rows say they sit at one place only for a moment, as when an exchange of two names is finished one rename
	// at a time: the one with the operation's inode is the one meant.
	copyOfSQL = `
		SELECT f.id, f.lpath, c.size, c.mtime_ns, c.ctime_ns, c.btime_ns, c.file_key, c.mode, c.uid, c.gid
		FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ?1 AND ` + copyAtPlace + ` ORDER BY c.file_key IS ?3 DESC LIMIT 1`
	finishCopySQL = `
		SELECT c.file_id FROM copies c WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace
	renameRowSQL = `
		SELECT c.file_id, f.lpath FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace
	finishUtimensSQL = `
		UPDATE copies AS c SET mtime_ns = ?4, ctime_ns = ?5 WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace
	finishChmodSQL = `
		UPDATE copies AS c SET mode = ?4, ctime_ns = ?5 WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace
	finishChownSQL = `
		UPDATE copies AS c SET uid = ?4, gid = ?5, mode = ?6, ctime_ns = ?7 WHERE c.disk_id = ?1 AND c.file_key = ?3 AND ` + copyAtPlace
)

// finishCopy says that a copy went for good: its row, found by disk, place and inode, the job that sent
// it, and its file if that was its last copy. A reprise comes here too, so that a run with a crash ends
// as a run without one (specs.md §15.12).
func finishCopy(ctx context.Context, tx *sql.Tx, op *Op) error {
	var fileID int64
	err := tx.QueryRowContext(ctx, finishCopySQL, op.Disk, []byte(op.Path), op.FileKey).Scan(&fileID)
	switch {
	case isNoRows(err):
		return nil
	case err != nil:
		return fmt.Errorf("copy: %w", err)
	}
	// A stale copy keeps what it held, for the copy that takes its place (specs.md §15.4).
	if refreshed, err := refreshStale(ctx, tx, op, fileID); err != nil || refreshed {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ? AND disk_id = ?", fileID, op.Disk); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'trash' AND file_id = ? AND dst_disk = ?", fileID, op.Disk); err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return dropFileIfEmpty(ctx, tx, fileID)
}

// dropFileIfEmpty removes a file with no copy left, with its chunks and jobs. A pending copy is nothing
// on a disk, and has no source left to be made from: it goes with its file (specs.md §14.1).
func dropFileIfEmpty(ctx context.Context, tx *sql.Tx, fileID int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ?1 AND state = 'pending' AND NOT EXISTS(SELECT 1 FROM copies o WHERE o.file_id = ?1 AND o.state <> 'pending')", fileID); err != nil {
		return fmt.Errorf("pending copies: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE id = ?1 AND NOT EXISTS(SELECT 1 FROM copies WHERE file_id = ?1)", fileID); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	return nil
}

// CopyOf gives what a copy's row says, by where it sits and, of two there, by its inode, with nothing valid in it
// when there is none.
func CopyOf(ctx context.Context, db *sql.DB, disk, path, fileKey string) (KnownCopy, error) {
	row := KnownCopy{Path: path, FilePath: path}
	var file []byte
	err := db.QueryRowContext(ctx, copyOfSQL, disk, []byte(path), fileKey).
		Scan(&row.FileID, &file, &row.Size, &row.MtimeNS, &row.CtimeNS, &row.BtimeNS, &row.FileKey, &row.Mode, &row.UID, &row.GID)
	if err != nil && !isNoRows(err) {
		return row, fmt.Errorf("copy: %w", err)
	}
	if file != nil {
		row.FilePath = string(file)
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
		SELECT EXISTS(SELECT 1 FROM files f JOIN copies c ON c.file_id = f.id WHERE c.disk_id = ?1 AND c.old_lpath IS NULL AND f.lpath_key = ?2 AND f.lpath <> ?3)
			OR EXISTS(SELECT 1 FROM copies WHERE disk_id = ?1 AND old_lpath_key = ?2 AND old_lpath <> ?3)
			OR EXISTS(SELECT 1 FROM entries WHERE disk_id = ?1 AND lpath_key = ?2 AND lpath <> ?3)`,
		disk, key, []byte(path)).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("names: %w", err)
	}
	return taken, nil
}

// KeyTakenExcept is KeyTaken, leaving out one name and all below it: what a rename frees as it comes.
func KeyTakenExcept(ctx context.Context, db *sql.DB, disk, path, except string) (bool, error) {
	return keyTakenBy(ctx, db, disk, path, except, nil, []string{
		"SELECT f.lpath FROM files f JOIN copies c ON c.file_id = f.id WHERE c.disk_id = ?1 AND c.old_lpath IS NULL AND f.lpath_key = ?2 AND f.lpath <> ?3",
		"SELECT old_lpath FROM copies WHERE disk_id = ?1 AND old_lpath_key = ?2 AND old_lpath <> ?3",
		"SELECT lpath FROM entries WHERE disk_id = ?1 AND lpath_key = ?2 AND lpath <> ?3",
	})
}

// KeyStaysTaken is KeyTakenExcept by what the last look still found: not a row whose clock runs, nor a name at or
// below a folder found renamed and not yet decided. A rename waits for those, and nothing goes to the trash for them.
func KeyStaysTaken(ctx context.Context, db *sql.DB, disk, path, except string, leaving []string) (bool, error) {
	return keyTakenBy(ctx, db, disk, path, except, leaving, []string{
		"SELECT f.lpath FROM files f JOIN copies c ON c.file_id = f.id WHERE c.disk_id = ?1 AND c.old_lpath IS NULL AND c.gone_ns IS NULL AND f.lpath_key = ?2 AND f.lpath <> ?3",
		"SELECT old_lpath FROM copies WHERE disk_id = ?1 AND gone_ns IS NULL AND old_lpath_key = ?2 AND old_lpath <> ?3",
		"SELECT lpath FROM entries WHERE disk_id = ?1 AND lpath_key = ?2 AND lpath <> ?3",
	})
}

func keyTakenBy(ctx context.Context, db *sql.DB, disk, path, except string, leaving, queries []string) (bool, error) {
	key := keyOf(path)
	if !key.Valid {
		return false, nil
	}
	isLeaving := func(name string) bool {
		return slices.ContainsFunc(leaving, func(from string) bool { return name == from || strings.HasPrefix(name, from+"/") })
	}
	check := func(query string) (bool, error) {
		rows, err := db.QueryContext(ctx, query, disk, key, []byte(path))
		if err != nil {
			return false, fmt.Errorf("names: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name []byte
			if err := rows.Scan(&name); err != nil {
				return false, fmt.Errorf("names: %w", err)
			}
			if s := string(name); s != except && !strings.HasPrefix(s, except+"/") && !isLeaving(s) {
				return true, nil
			}
		}
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("names: %w", err)
		}
		return false, nil
	}
	for _, query := range queries {
		taken, err := check(query)
		if err != nil || taken {
			return taken, err
		}
	}
	return false, nil
}

// TakenByRename says whether a path is the path of a file whose copy on this disk still sits elsewhere,
// so that a disk has no room for another copy of it (specs.md §15.12).
func TakenByRename(ctx context.Context, db *sql.DB, disk, path string) (bool, error) {
	var taken bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM files f JOIN copies c ON c.file_id = f.id WHERE f.lpath = ?2 AND c.disk_id = ?1 AND c.old_lpath IS NOT NULL)`,
		disk, []byte(path)).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("names: %w", err)
	}
	return taken, nil
}

// LinkedPaths gives the paths whose copy on a disk is that file, by what the catalog knows.
func LinkedPaths(ctx context.Context, db *sql.DB, disk, fileKey string) ([]string, error) {
	rows, err := db.QueryContext(ctx, linkedPathsSQL, disk, fileKey)
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

// MkdirLike is the mode and owner a folder made below the root is to take: those of the folder it stands for
// (specs.md §17.4).
type MkdirLike struct {
	Mode, UID, GID int64
}

// BeginMkdir journals a folder made below the root (specs.md §6.1, §17.4). With like, its event holds the mode and
// owner the folder is to take: it is made, then given them, and a reprise that finds it made and not yet given them
// has nothing else to say whose it is (§17.2).
func BeginMkdir(ctx context.Context, db *sql.DB, disk, path string, like *MkdirLike, nowNS int64) (*Op, error) {
	op := &Op{Disk: disk, Kind: OpMkdir, Path: path}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES (?, 'mkdir', ?, ?, 'planned')", disk, []byte(path), nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if like == nil {
			return nil
		}
		return writeEventData(ctx, tx, nowNS, "info", OpMkdir, op, eventData{Mode: &like.Mode, UID: &like.UID, GID: &like.GID})
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", path, err)
	}
	return op, nil
}

// MkdirLikeOf gives the mode and owner the event of a folder's operation holds, and nil when it holds none: a folder
// that stood for none, or one journaled before its event held them.
func MkdirLikeOf(ctx context.Context, db *sql.DB, opID int64) (*MkdirLike, error) {
	var mode, uid, gid sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT json_extract(data, '$.mode'), json_extract(data, '$.uid'), json_extract(data, '$.gid')
		FROM events WHERE kind = ? AND json_extract(data, '$.op') = ?`, OpMkdir, opID).Scan(&mode, &uid, &gid)
	switch {
	case isNoRows(err):
		return nil, nil //nolint:nilnil // no event is an answer
	case err != nil:
		return nil, fmt.Errorf("event: %w", err)
	case !mode.Valid || !uid.Valid || !gid.Valid:
		return nil, nil //nolint:nilnil // likewise
	}
	return &MkdirLike{Mode: mode.Int64, UID: uid.Int64, GID: gid.Int64}, nil
}

// FinishMkdir says that a folder is there, and Atoll's own.
func FinishMkdir(ctx context.Context, db *sql.DB, op *Op, e *EntryRow) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO entries(lpath, lpath_key, disk_id, kind, mode, mtime_ns, created_by, file_key, btime_ns)
			VALUES (?, ?, ?, 'dir', ?, ?, 'atoll', ?, ?)
			ON CONFLICT(lpath, disk_id) DO UPDATE SET kind = excluded.kind, mode = excluded.mode, mtime_ns = excluded.mtime_ns, created_by = 'atoll'`,
			[]byte(e.Path), keyOf(e.Path), op.Disk, e.Mode, e.MtimeNS, e.FileKey, e.BtimeNS)
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

// dropCopyRow removes what a row left from before says of a path that was found free. A row that still
// sits elsewhere is not at this path, whatever its file's path is. A file that sat behind at this path goes if
// that was its last copy, as it would otherwise hold its own path for good.
func dropCopyRow(ctx context.Context, tx *sql.Tx, disk, path string) error {
	behind, err := queryAll(ctx, tx, func(rows *sql.Rows) (int64, error) {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("read: %w", err)
		}
		return id, nil
	}, "SELECT file_id FROM copies WHERE disk_id = ? AND old_lpath = ?", disk, []byte(path))
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM copies WHERE disk_id = ?1 AND ((old_lpath IS NULL AND file_id = (SELECT id FROM files WHERE lpath = ?2)) OR old_lpath = ?2)", disk, []byte(path))
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	for _, id := range behind {
		if err := dropFileIfEmpty(ctx, tx, id); err != nil {
			return err
		}
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
		if _, err := tx.ExecContext(ctx, undoTrashSQL, []byte(op.Path), op.ID); err != nil {
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
	rows, err := db.QueryContext(ctx, "SELECT id, disk_id, kind, path, COALESCE(path2, x''), COALESCE(file_key, ''), COALESCE(link, 0) FROM ops WHERE state = 'planned' ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []Op{}
	for rows.Next() {
		var op Op
		var path, path2 []byte
		if err := rows.Scan(&op.ID, &op.Disk, &op.Kind, &path, &path2, &op.FileKey, &op.Link); err != nil {
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

// plannedAtSQL looks among the operations left planned only, which their index holds.
const plannedAtSQL = "SELECT EXISTS(SELECT 1 FROM ops WHERE state = 'planned' AND disk_id = ?1 AND path = ?2)"

// PlannedAt says whether an operation on a path of a disk is still planned: one that stopped where nothing could
// resolve it, which waits for the reprise (specs.md §17.2).
func PlannedAt(ctx context.Context, db *sql.DB, disk, path string) (bool, error) {
	var planned bool
	if err := db.QueryRowContext(ctx, plannedAtSQL, disk, []byte(path)).Scan(&planned); err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	return planned, nil
}

// TrashEntry is what a disk's trash holds at one location. RowID is its row, which finds it again at once.
type TrashEntry struct {
	RowID     int64
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

// What finds an entry of the trash names its path, with its operation or its place: trash_by_path holds the entries by
// path, then by operation, and no index holds them by operation or by place alone. An entry is written with the path of
// the operation that sends it to the trash, and a restore or a purge is journaled with the path of its entry, so each
// operation knows the path. Each purge, restore and undone operation asks once, and with no index each read the whole
// trash (specs.md §26).
const (
	undoTrashSQL    = "DELETE FROM trash WHERE lpath = ?1 AND op_id = ?2"
	restartTrashSQL = "UPDATE trash SET deleted_ns = ?1, expires_ns = ?2 WHERE lpath = ?3 AND op_id = ?4"
	staleTrashSQL   = "SELECT reason, deleted_ns FROM trash WHERE lpath = ?1 AND op_id = ?2"
	repairedSizeSQL = "UPDATE trash SET size = ?1 WHERE lpath = ?2 AND op_id = ?3"
	dropTrashRowSQL = "DELETE FROM trash WHERE lpath = ?1 AND disk_id = ?2 AND trash_path = ?3"
	trashEntrySQL   = `
		SELECT t.id, t.lpath, t.disk_id, t.trash_path, t.op_id, t.size, t.blake3, t.reason, t.deleted_ns, t.expires_ns, COALESCE(o.file_key, '')
		FROM trash t JOIN ops o ON o.id = t.op_id
		WHERE o.state = 'done'`
	// A page of the list, in the index's own order: no sort, and nothing read before the page.
	trashPageSQL = trashEntrySQL + ` AND (t.lpath, t.op_id) > (?, ?) ORDER BY t.lpath, t.op_id LIMIT ?`
	// A page of the entries from one path up to another, through trash_by_path: what a restore of a path reads, and
	// nothing of the rest of the trash.
	trashRangeSQL = trashEntrySQL + ` AND t.lpath >= ?1 AND t.lpath < ?2 AND (t.lpath, t.op_id) > (?3, ?4) ORDER BY t.lpath, t.op_id LIMIT ?5`
	// A page of the entries whose time has come, through trash_by_expiry, which holds each row after its expiry: what
	// a purge reads first, whatever else the trash holds.
	trashExpiredSQL = trashEntrySQL + ` AND t.expires_ns <= ?1 AND (t.expires_ns, t.id) > (?2, ?3) ORDER BY t.expires_ns, t.id LIMIT ?4`
	trashByRowSQL   = trashEntrySQL + ` AND t.id = ?`
	trashDueSQL     = `SELECT EXISTS(SELECT 1 FROM trash t JOIN ops o ON o.id = t.op_id WHERE t.expires_ns <= ? AND o.state = 'done')`
	// Every entry once, in the order the table holds them, which costs no sort.
	trashWeightsSQL = `
		SELECT t.id, t.disk_id, t.op_id, t.reason, t.size, t.deleted_ns, t.expires_ns
		FROM trash t JOIN ops o ON o.id = t.op_id
		WHERE o.state = 'done' AND (?1 = '' OR t.disk_id = ?1)
		ORDER BY t.id`
)

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
	return trashEntries(ctx, db, trashPageSQL, after, afterOp, trashPageSize)
}

// trashEntries reads the entries a query of trashEntrySQL gives.
func trashEntries(ctx context.Context, db *sql.DB, query string, args ...any) ([]TrashEntry, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []TrashEntry{}
	for rows.Next() {
		var e TrashEntry
		var path, trashPath []byte
		if err := rows.Scan(&e.RowID, &path, &e.Disk, &trashPath, &e.OpID, &e.Size, &e.Hash, &e.Reason, &e.DeletedNS, &e.ExpiresNS, &e.FileKey); err != nil {
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

// EachTrashEntryBelow gives each entry at a path or below it, by whole components, whose operation is done, by path in
// byte order, then by operation: read through trash_by_path, a page at a time, and nothing of the rest of the trash.
func EachTrashEntryBelow(ctx context.Context, db *sql.DB, under string, fn func(*TrashEntry) error) error {
	// Between a path and what follows everything below it lie the names that begin as it does and go on with a byte
	// before the slash: those are left aside.
	lo, hi := []byte(under), []byte(under+"0")
	after, afterOp := []byte{}, int64(0)
	for {
		page, err := trashEntries(ctx, db, trashRangeSQL, lo, hi, after, afterOp, trashPageSize)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for i := range page {
			e := &page[i]
			if e.Path != under && !strings.HasPrefix(e.Path, under+"/") {
				continue
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		last := &page[len(page)-1]
		after, afterOp = []byte(last.Path), last.OpID
	}
}

// TrashExpired gives a page of the entries whose time has come by nowNS and whose operation is done, by expiry then
// by row, after the entry of afterNS and afterRow: read through trash_by_expiry (specs.md §17).
func TrashExpired(ctx context.Context, db *sql.DB, nowNS, afterNS, afterRow int64) ([]TrashEntry, error) {
	return trashEntries(ctx, db, trashExpiredSQL, nowNS, afterNS, afterRow, trashPageSize)
}

// TrashEntryByRow gives the entry of a row, and false when it is gone, or its operation is not done.
func TrashEntryByRow(ctx context.Context, db *sql.DB, row int64) (TrashEntry, bool, error) {
	list, err := trashEntries(ctx, db, trashByRowSQL, row)
	if err != nil || len(list) == 0 {
		return TrashEntry{}, false, err
	}
	return list[0], true, nil
}

// TrashDue says whether an entry whose operation is done has reached its time by nowNS.
func TrashDue(ctx context.Context, db *sql.DB, nowNS int64) (bool, error) {
	var due bool
	if err := db.QueryRowContext(ctx, trashDueSQL, nowNS).Scan(&due); err != nil {
		return false, fmt.Errorf("trash: %w", err)
	}
	return due, nil
}

// TrashUsed gives, for each disk whose trash holds entries, the sum of their sizes, an entry of unknown size counting
// for nothing, and a sum past the largest integer staying there (specs.md §17).
func TrashUsed(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.disk_id, t.size FROM trash t JOIN ops o ON o.id = t.op_id
		WHERE o.state = 'done' AND t.size > 0`)
	if err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	used := map[string]int64{}
	for rows.Next() {
		var disk string
		var size int64
		if err := rows.Scan(&disk, &size); err != nil {
			return nil, fmt.Errorf("trash: %w", err)
		}
		if size > math.MaxInt64-used[disk] {
			used[disk] = math.MaxInt64
		} else {
			used[disk] += size
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	return used, nil
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

// TrashWeight is what choosing what a purge reaches needs of an entry of the trash: its disk, its operation, why and
// when it came, and its size. No path.
type TrashWeight struct {
	RowID     int64
	Disk      string
	OpID      int64
	Reason    string
	Size      sql.NullInt64
	DeletedNS int64
	ExpiresNS int64
}

// TrashWeights gives that for every entry whose operation is done, in the order they reached the trash. The daemon asks
// after each pass whether a purge is due: the paths of a large trash, in their order, are not for that (specs.md
// §15.13, §26).
func TrashWeights(ctx context.Context, db *sql.DB) ([]TrashWeight, error) {
	return TrashWeightsOn(ctx, db, "")
}

// TrashWeightsOn gives that for the entries of one disk, or of all with "": a purge for room is chosen among the
// entries of a disk whose trash holds too much, and of no other (specs.md §17).
func TrashWeightsOn(ctx context.Context, db *sql.DB, disk string) ([]TrashWeight, error) {
	rows, err := db.QueryContext(ctx, trashWeightsSQL, disk)
	if err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []TrashWeight{}
	for rows.Next() {
		var w TrashWeight
		if err := rows.Scan(&w.RowID, &w.Disk, &w.OpID, &w.Reason, &w.Size, &w.DeletedNS, &w.ExpiresNS); err != nil {
			return nil, fmt.Errorf("trash: %w", err)
		}
		list = append(list, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	return list, nil
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
	if _, err := tx.ExecContext(ctx, dropTrashRowSQL, []byte(op.Path), op.Disk, []byte(op.Path2)); err != nil {
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
		// The file at that path may have a copy here still sitting elsewhere, renamed since this was
		// planned: a disk holds one copy of a file, and the next walk reports this one (specs.md §15.10).
		var behind bool
		err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM files f JOIN copies k ON k.file_id = f.id WHERE f.lpath = ? AND k.disk_id = ?)", []byte(op.Path), op.Disk).Scan(&behind)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if !behind {
			if err := writeCopies(ctx, tx, op.Disk, nowNS, []NewCopy{*c}); err != nil {
				return err
			}
		}
		return writeLinks(ctx, tx, op.Disk, links)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// What a pass asks of the journal about a copy a walk no longer finds at its name (invariant 11): where the operations
// on its disk put the file of that inode, through ops_by_key, and whether an operation is one of that disk's.
const (
	placesOfSQL = "SELECT path2 FROM ops WHERE disk_id = ?1 AND file_key = ?2 AND path2 IS NOT NULL ORDER BY id"
	opOnDiskSQL = "SELECT EXISTS(SELECT 1 FROM ops WHERE id = ?1 AND disk_id = ?2)"
)

// PlacesOf gives where the journal says its operations on a disk put the file of an inode: the second path of each,
// in the order of the operations. An operation is journaled before it runs (invariant 12), so a copy Atoll moved is at
// one of them, or was moved on since.
func PlacesOf(ctx context.Context, db *sql.DB, disk, fileKey string) ([]string, error) {
	rows, err := db.QueryContext(ctx, placesOfSQL, disk, fileKey)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var place []byte
		if err := rows.Scan(&place); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
		list = append(list, string(place))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return list, nil
}

// OpOnDisk says whether the journal holds an operation of that id on that disk.
func OpOnDisk(ctx context.Context, db *sql.DB, disk string, id int64) (bool, error) {
	var held bool
	if err := db.QueryRowContext(ctx, opOnDiskSQL, id, disk).Scan(&held); err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	return held, nil
}
