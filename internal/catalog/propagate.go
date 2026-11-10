package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The events of propagation (specs.md §9).
const (
	EventCopyGone     = "copy-gone"
	EventCopyKept     = "copy-kept"
	EventGuard        = "guard"
	EventGuardConfirm = "guard-confirm"
	EventGuardReject  = "guard-reject"
	EventGuardClear   = "guard-clear"
)

// The states of a trash job.
const (
	JobQueued   = "queued"
	JobCanceled = "canceled"
)

// DueFiles reads the files that have a copy whose name no longer holds it, by path as they stand, a
// page at a time: a file decided leaves the list, as its clock is cleared.
func DueFiles(ctx context.Context, r *FileReader) ([]FileRow, error) {
	return r.page(ctx, `SELECT * FROM files WHERE lpath > ? AND EXISTS(SELECT 1 FROM copies c WHERE c.file_id = files.id AND c.gone_ns IS NOT NULL) ORDER BY lpath LIMIT ?`)
}

// FileAt gives the file at a path, with its copies, and nil when there is none.
func FileAt(ctx context.Context, db *sql.DB, path string) (*FileRow, error) {
	r := NewFileReader(db)
	r.after = []byte(path)
	rows, err := r.db.QueryContext(ctx, "SELECT "+fileColumns+" FROM (SELECT * FROM files WHERE lpath = ?) f LEFT JOIN copies c ON c.file_id = f.id ORDER BY c.disk_id", []byte(path))
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := r.readFiles(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil //nolint:nilnil // no file there is an answer
	}
	return &list[0], nil
}

// FileByID gives one file, with its copies, and nil when there is none.
func FileByID(ctx context.Context, db *sql.DB, id int64) (*FileRow, error) {
	r := NewFileReader(db)
	rows, err := db.QueryContext(ctx, "SELECT "+fileColumns+" FROM (SELECT * FROM files WHERE id = ?) f LEFT JOIN copies c ON c.file_id = f.id ORDER BY c.disk_id", id)
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := r.readFiles(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil //nolint:nilnil // no file there is an answer
	}
	return &list[0], nil
}

// Waiting counts the copies whose clock has not run out, and says when the first does.
func Waiting(ctx context.Context, db *sql.DB, debounceNS, nowNS int64) (count int64, nextDue sql.NullInt64, err error) {
	err = db.QueryRowContext(ctx, "SELECT count(*), min(gone_ns) FROM copies WHERE gone_ns IS NOT NULL AND gone_ns > ?", nowNS-debounceNS).Scan(&count, &nextDue)
	if err != nil {
		return 0, sql.NullInt64{}, fmt.Errorf("waiting: %w", err)
	}
	if nextDue.Valid {
		nextDue.Int64 += debounceNS
	}
	return count, nextDue, nil
}

// eventRow is the data of an event about a copy (specs.md §9).
type eventRow struct {
	Disk        string `json:"disk,omitempty"`
	Path        string `json:"path,omitempty"`
	PathB64     string `json:"path_b64,omitempty"`
	GoneNS      *int64 `json:"gone_ns,omitempty"`
	NothingLeft bool   `json:"nothing_left,omitempty"`
	UpTo        *int64 `json:"up_to,omitempty"`
}

func (e *eventRow) setPath(p string) {
	if utf8.ValidString(p) {
		e.Path = p
	} else {
		e.PathB64 = base64.StdEncoding.EncodeToString([]byte(p))
	}
}

func insertEvent(ctx context.Context, tx *sql.Tx, nowNS int64, level, kind string, data *eventRow) error {
	text, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO events(ts_ns, level, kind, data) VALUES (?, ?, ?, ?)", nowNS, level, kind, string(text)); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	return nil
}

// RowChange is what happens to the row of one copy of a file in a decision.
type RowChange struct {
	Disk string
	// Follow: it moved to the file's new path, and these come from the walk.
	Follow      bool
	FollowNlink int64
	FollowCtime sql.NullInt64 // what to record, when it should change
	Unverify    bool
	MakeUser    bool
	// Pending: it stays where it is, and says so. Clear: it sits where the file now is. ClearedFrom says that its
	// row said so of where it sits only now, as a rename back leaves it.
	Pending     string
	Clear       bool
	ClearedFrom string
	// Drop: its row goes, with the events that says.
	Drop bool
	// CountGone writes a copy-gone event, which the guard counts.
	CountGone   bool
	GoneNS      int64
	NothingLeft bool
	// KeepGone keeps a gone row under a guard, with where it was.
	KeepGone string
	// Trash queues a trash job, with Src the disk the copy went from.
	Trash bool
}

// OwnFile moves copies of a file to a new file row at a path: what is at the old path did not exist when
// the file left, or what moved to another target is a file of its own there, with the hash and chunks.
type OwnFile struct {
	Path     string
	Disks    []string
	WithHash bool
	Size     int64
	MtimeNS  int64
	First    int64
	// FollowRows are the rows that follow as a Follow does, for a copy that moved there.
	Follow []RowChange
}

// FileChange is what a decision does to one file.
type FileChange struct {
	ID            int64
	Path          string
	NewPath       string // empty when it keeps its path
	Rows          []RowChange
	Own           []OwnFile
	DropHash      bool
	ClearConflict bool
	Src           string // of a trash job
	Reason        string // of a trash job
	DropJobs      bool   // queued trash jobs go: a rename means that the deletion no longer stands
	DropFile      bool   // no copy is left
	Kept          bool   // a copy-kept event: a file a deletion did not reach
	KeptDisk      string
}

// ApplyDecisions writes the decisions of one page in one transaction, with what the pass said before
// anything runs on a disk (specs.md §15.12). Rows that hold each other's target pass through a value
// holding a NUL byte, which no path can, so it never outlives the transaction.
func ApplyDecisions(ctx context.Context, db *sql.DB, nowNS int64, list []FileChange) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		for i := range list {
			if c := &list[i]; c.NewPath != "" {
				if _, err := tx.ExecContext(ctx, "UPDATE files SET lpath = ? WHERE id = ?", []byte(fmt.Sprintf("\x00%d", c.ID)), c.ID); err != nil {
					return fmt.Errorf("file %q: %w", c.Path, err)
				}
			}
		}
		for i := range list {
			if c := &list[i]; c.NewPath != "" {
				if _, err := tx.ExecContext(ctx, "UPDATE files SET lpath = ?, lpath_key = ?, updated_ns = ? WHERE id = ?", []byte(c.NewPath), keyOf(c.NewPath), nowNS, c.ID); err != nil {
					return fmt.Errorf("file %q: %w", c.Path, err)
				}
			}
		}
		for i := range list {
			if err := applyFile(ctx, tx, nowNS, &list[i]); err != nil {
				return fmt.Errorf("file %q: %w", list[i].Path, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("decisions: %w", err)
	}
	return nil
}

func applyFile(ctx context.Context, tx *sql.Tx, nowNS int64, c *FileChange) error {
	for i := range c.Rows {
		if err := applyRow(ctx, tx, nowNS, c, &c.Rows[i]); err != nil {
			return fmt.Errorf("copy on %q: %w", c.Rows[i].Disk, err)
		}
	}
	for i := range c.Own {
		if err := applyOwn(ctx, tx, nowNS, c, &c.Own[i]); err != nil {
			return err
		}
	}
	if c.DropJobs {
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'trash' AND file_id = ? AND state = ?", c.ID, JobQueued); err != nil {
			return fmt.Errorf("jobs: %w", err)
		}
	}
	if c.DropHash {
		if _, err := tx.ExecContext(ctx, "UPDATE files SET blake3 = NULL, updated_ns = ? WHERE id = ?", nowNS, c.ID); err != nil {
			return fmt.Errorf("hash: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM file_chunks WHERE file_id = ?", c.ID); err != nil {
			return fmt.Errorf("chunks: %w", err)
		}
		// What they were verified against is gone, and no later hash says anything of them (specs.md §15.5).
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET verified_ns = NULL WHERE file_id = ?", c.ID); err != nil {
			return fmt.Errorf("copies: %w", err)
		}
	}
	if c.ClearConflict {
		// What comes next gives it its state, as a file in conflict keeps that one otherwise.
		if _, err := tx.ExecContext(ctx, "UPDATE files SET state = 'pending', updated_ns = ? WHERE id = ? AND state = 'conflict'", nowNS, c.ID); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if c.Kept {
		data := eventRow{Disk: c.KeptDisk}
		data.setPath(c.Path)
		if err := insertEvent(ctx, tx, nowNS, "warning", EventCopyKept, &data); err != nil {
			return err
		}
	}
	if c.DropFile {
		if err := dropFileIfEmpty(ctx, tx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

func applyRow(ctx context.Context, tx *sql.Tx, nowNS int64, c *FileChange, r *RowChange) error {
	if r.CountGone {
		data := eventRow{Disk: r.Disk, GoneNS: &r.GoneNS, NothingLeft: r.NothingLeft}
		data.setPath(c.Path)
		if err := insertEvent(ctx, tx, nowNS, "info", EventCopyGone, &data); err != nil {
			return err
		}
	}
	switch {
	case r.Drop:
		if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ? AND disk_id = ?", c.ID, r.Disk); err != nil {
			return fmt.Errorf("drop: %w", err)
		}
	case r.Follow:
		return followRow(ctx, tx, c.ID, r)
	case r.Clear:
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET old_lpath = NULL, old_lpath_key = NULL WHERE file_id = ? AND disk_id = ?", c.ID, r.Disk); err != nil {
			return fmt.Errorf("clear: %w", err)
		}
		if r.ClearedFrom != "" {
			return dropJobsAbove(ctx, tx, r.Disk, r.ClearedFrom)
		}
	case r.Pending != "":
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET old_lpath = ?, old_lpath_key = ? WHERE file_id = ? AND disk_id = ?", []byte(r.Pending), keyOf(r.Pending), c.ID, r.Disk); err != nil {
			return fmt.Errorf("pending: %w", err)
		}
	case r.KeepGone != "":
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET old_lpath = ?, old_lpath_key = ? WHERE file_id = ? AND disk_id = ?", []byte(r.KeepGone), keyOf(r.KeepGone), c.ID, r.Disk); err != nil {
			return fmt.Errorf("keep: %w", err)
		}
	case r.Trash:
		// A file decided again with a job still queued: that job stands, with what it was asked for.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) SELECT 'trash', ?1, ?2, ?3, ?4, ?5
			WHERE NOT EXISTS(SELECT 1 FROM jobs WHERE kind = 'trash' AND file_id = ?1 AND dst_disk = ?3 AND state = ?4 AND reason = ?5)`,
			c.ID, c.Src, r.Disk, JobQueued, c.Reason)
		if err != nil {
			return fmt.Errorf("job: %w", err)
		}
	}
	return nil
}

// dropJobsAbove ends the folder jobs of a disk above a place: a copy there is back where the user wants it, and a
// job would carry it away again. What is left below follows one by one (specs.md §15.12).
func dropJobsAbove(ctx context.Context, tx *sql.Tx, disk, place string) error {
	jobs, err := queryAll(ctx, tx, scanFolderJob, "SELECT id, payload FROM jobs WHERE kind = 'rename' AND dst_disk = ?", disk)
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	for _, j := range jobs {
		if !strings.HasPrefix(place, j.From+"/") {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", j.ID); err != nil {
			return fmt.Errorf("job: %w", err)
		}
	}
	return nil
}

func followRow(ctx context.Context, tx *sql.Tx, fileID int64, r *RowChange) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE copies SET old_lpath = NULL, old_lpath_key = NULL, gone_ns = NULL, gone_kind = NULL, nlink = ?1,
			ctime_ns = CASE WHEN ?2 IS NULL THEN ctime_ns ELSE ?2 END,
			verified_ns = CASE WHEN ?3 THEN NULL ELSE verified_ns END,
			created_by = CASE WHEN ?4 THEN 'user' ELSE created_by END
		WHERE file_id = ?5 AND disk_id = ?6`,
		r.FollowNlink, r.FollowCtime, r.Unverify, r.MakeUser, fileID, r.Disk)
	if err != nil {
		return fmt.Errorf("follow: %w", err)
	}
	return nil
}

func applyOwn(ctx context.Context, tx *sql.Tx, nowNS int64, c *FileChange, o *OwnFile) error {
	var id int64
	// Another file of this step may have taken that path. Its row goes, and the next walk says what that
	// copy is, as it does of any file found there, with the checks of a copy not yet in the catalog.
	err := tx.QueryRowContext(ctx, "SELECT id FROM files WHERE lpath = ?", []byte(o.Path)).Scan(&id)
	switch {
	case err == nil:
		for _, disk := range o.Disks {
			if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ? AND disk_id = ?", c.ID, disk); err != nil {
				return fmt.Errorf("copy on %q: %w", disk, err)
			}
		}
		return nil
	case isNoRows(err):
		err := tx.QueryRowContext(ctx, `
			INSERT INTO files(lpath, lpath_key, size, mtime_ns, blake3, replicas_target, mode, state, first_seen_ns, updated_ns)
			SELECT ?1, ?2, ?3, ?4, CASE WHEN ?5 THEN blake3 ELSE NULL END, 0, 'deferred', 'pending', ?6, ?7 FROM files WHERE id = ?8
			RETURNING id`,
			[]byte(o.Path), keyOf(o.Path), o.Size, o.MtimeNS, o.WithHash, o.First, nowNS, c.ID).Scan(&id)
		if err != nil {
			return fmt.Errorf("file %q: %w", o.Path, err)
		}
		if o.WithHash {
			if _, err := tx.ExecContext(ctx, "INSERT INTO file_chunks(file_id, chunks) SELECT ?, chunks FROM file_chunks WHERE file_id = ?", id, c.ID); err != nil {
				return fmt.Errorf("chunks: %w", err)
			}
		}
	case err != nil:
		return fmt.Errorf("file %q: %w", o.Path, err)
	}
	for _, disk := range o.Disks {
		// With no hash to its file, it has nothing it was verified against.
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET file_id = ?1, verified_ns = CASE WHEN ?2 THEN verified_ns ELSE NULL END WHERE file_id = ?3 AND disk_id = ?4", id, o.WithHash, c.ID, disk); err != nil {
			return fmt.Errorf("copy on %q: %w", disk, err)
		}
	}
	for i := range o.Follow {
		if err := followRow(ctx, tx, id, &o.Follow[i]); err != nil {
			return err
		}
	}
	return nil
}

// queryAll gives what scan makes of each row of a query, and closes them before anything else runs on
// the transaction.
func queryAll[T any](ctx context.Context, tx *sql.Tx, scan func(*sql.Rows) (T, error), query string, args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return out, nil
}

func scanBytes(rows *sql.Rows) (string, error) {
	var p []byte
	if err := rows.Scan(&p); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return string(p), nil
}

func scanFolderJob(rows *sql.Rows) (FolderJob, error) {
	var j FolderJob
	var payload string
	if err := rows.Scan(&j.ID, &payload); err != nil {
		return j, fmt.Errorf("read: %w", err)
	}
	var err error
	j.From, j.To, err = decodeFolderPayload(payload)
	return j, err
}

// FilesBelow gives the files at or below a folder, with their copies, a page at a time.
func FilesBelow(ctx context.Context, r *FileReader, folder string) ([]FileRow, error) {
	if len(r.after) == 0 {
		r.after = []byte(folder + "/")
	}
	list, err := r.page(ctx, "SELECT * FROM files WHERE lpath > ? ORDER BY lpath LIMIT ?")
	if err != nil {
		return nil, err
	}
	for i := range list {
		if !strings.HasPrefix(list[i].Path, folder+"/") {
			return list[:i], nil
		}
	}
	return list, nil
}

// EntryIdentity is what a folder's row says of it.
type EntryIdentity struct {
	Path    string
	FileKey sql.NullString
	BtimeNS sql.NullInt64
}

// IgnoredPaths gives the names a disk holds as ignored entries: logical files of no one's, and no place a
// copy can have gone to.
func IgnoredPaths(ctx context.Context, db *sql.DB, disk string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT lpath FROM entries WHERE disk_id = ? AND kind = 'ignored'", disk)
	if err != nil {
		return nil, fmt.Errorf("entries of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("entries of %q: %w", disk, err)
		}
		out[string(p)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entries of %q: %w", disk, err)
	}
	return out, nil
}

// EachFolderRow gives the folder rows of a disk.
func EachFolderRow(ctx context.Context, db *sql.DB, disk string, fn func(*EntryIdentity)) error {
	rows, err := db.QueryContext(ctx, "SELECT lpath, file_key, btime_ns FROM entries WHERE disk_id = ? AND kind = 'dir' ORDER BY lpath", disk)
	if err != nil {
		return fmt.Errorf("folders of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p []byte
		var e EntryIdentity
		if err := rows.Scan(&p, &e.FileKey, &e.BtimeNS); err != nil {
			return fmt.Errorf("folders of %q: %w", disk, err)
		}
		e.Path = string(p)
		fn(&e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("folders of %q: %w", disk, err)
	}
	return nil
}

// EachEntryBelow gives the paths of the entries of a disk below a folder.
func EntriesBelow(ctx context.Context, db *sql.DB, disk, folder string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT lpath FROM entries WHERE disk_id = ? AND lpath > ? ORDER BY lpath", disk, []byte(folder+"/"))
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("entries: %w", err)
		}
		if !strings.HasPrefix(string(p), folder+"/") {
			break
		}
		list = append(list, string(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	return list, nil
}

// What reads below a folder: ?1 the disk and ?2, ?3 the bounds of folderRange, unless said otherwise. Each reads the files
// by the range of their paths, then each copy by its key, or the copies of a disk by their old path. Led by the disk,
// as SQLite chooses with no statistics, it read every copy of that disk, and a pass asks once for each folder a disk
// lost: about 800 folders and 200,000 copies took two minutes (specs.md §26). CROSS JOIN keeps the files first.
const (
	copiesBelowSQL = `
		SELECT (SELECT count(*) FROM files f CROSS JOIN copies c ON c.file_id = f.id WHERE c.disk_id = ?1 AND c.old_lpath IS NULL AND f.lpath >= ?2 AND f.lpath < ?3)
			+ (SELECT count(*) FROM copies WHERE disk_id = ?1 AND old_lpath >= ?2 AND old_lpath < ?3)`
	settledBelowSQL = `
		SELECT (SELECT count(*) FROM files f CROSS JOIN copies c ON c.file_id = f.id WHERE c.disk_id = ?1 AND c.gone_ns IS NULL AND c.old_lpath IS NULL AND f.lpath >= ?2 AND f.lpath < ?3)
			+ (SELECT count(*) FROM copies WHERE disk_id = ?1 AND gone_ns IS NULL AND old_lpath >= ?2 AND old_lpath < ?3)`
	atOwnPathBelowSQL = `
		SELECT f.lpath FROM files f CROSS JOIN copies c ON c.file_id = f.id
		WHERE c.disk_id = ?1 AND c.old_lpath IS NULL AND f.lpath >= ?2 AND f.lpath < ?3 ORDER BY f.lpath`
	// ?1, ?2 the bounds, and ?3 the disk left out.
	disksBelowSQL = `
		SELECT c.disk_id AS d FROM copies c JOIN files f ON f.id = c.file_id WHERE c.old_lpath IS NULL AND f.lpath >= ?1 AND f.lpath < ?2 AND c.disk_id <> ?3
		UNION SELECT id FROM disks WHERE id <> ?3 AND EXISTS(SELECT 1 FROM copies o WHERE o.disk_id = disks.id AND o.old_lpath >= ?1 AND o.old_lpath < ?2)
		UNION SELECT disk_id FROM entries WHERE lpath >= ?1 AND lpath < ?2 AND disk_id <> ?3
		ORDER BY d`
	// ?4 for a reprise.
	folderRowsSQL = `
		SELECT c.file_id, c.old_lpath, f.lpath FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ?1 AND (c.gone_ns IS NULL OR (?4 AND c.gone_kind = 'moved')) AND c.old_lpath >= ?2 AND c.old_lpath < ?3
		UNION ALL
		SELECT c.file_id, f.lpath, f.lpath FROM files f CROSS JOIN copies c ON c.file_id = f.id
		WHERE ?4 AND c.disk_id = ?1 AND (c.gone_ns IS NULL OR c.gone_kind = 'moved') AND c.old_lpath IS NULL AND f.lpath >= ?2 AND f.lpath < ?3
			AND EXISTS(SELECT 1 FROM copies o WHERE o.file_id = c.file_id AND o.disk_id <> c.disk_id AND o.old_lpath IS NULL)`
)

// rewriteEntries gives the entries of a disk at or below a folder the new folder's path, as what a
// rename does to them.
func rewriteEntries(ctx context.Context, tx *sql.Tx, disk, from, to string) error {
	paths, err := queryAll(ctx, tx, scanBytes, "SELECT lpath FROM entries WHERE disk_id = ? AND (lpath = ? OR lpath > ?) ORDER BY lpath", disk, []byte(from), []byte(from+"/"))
	if err != nil {
		return fmt.Errorf("entries: %w", err)
	}
	for _, p := range paths {
		if p != from && !strings.HasPrefix(p, from+"/") {
			continue
		}
		next := to + strings.TrimPrefix(p, from)
		_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO entries(lpath, lpath_key, disk_id, kind, mode, mtime_ns, target, created_by, file_key, btime_ns) SELECT ?, ?, disk_id, kind, mode, mtime_ns, target, created_by, file_key, btime_ns FROM entries WHERE disk_id = ? AND lpath = ?",
			[]byte(next), keyOf(next), disk, []byte(p))
		if err != nil {
			return fmt.Errorf("entry %q: %w", p, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM entries WHERE disk_id = ? AND lpath = ?", disk, []byte(p)); err != nil {
			return fmt.Errorf("entry %q: %w", p, err)
		}
	}
	return nil
}

// CopiesBelow counts the copies of a disk that sit below a folder.
func CopiesBelow(ctx context.Context, db *sql.DB, disk, folder string) (int64, error) {
	lo, hi := folderRange(folder)
	var count int64
	err := db.QueryRowContext(ctx, copiesBelowSQL, disk, lo, hi).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("copies: %w", err)
	}
	return count, nil
}

// SettledBelow counts the copies of a disk that sit below a folder with no clock running: none of these
// went at the last look. A pass goes by this to say that a copy moved with a folder (specs.md §15.12).
func SettledBelow(ctx context.Context, db *sql.DB, disk, folder string) (int64, error) {
	lo, hi := folderRange(folder)
	var count int64
	err := db.QueryRowContext(ctx, settledBelowSQL, disk, lo, hi).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("copies: %w", err)
	}
	return count, nil
}

// AtOwnPathBelow gives the paths of the copies of a disk that sit at their file's path, below a folder.
func AtOwnPathBelow(ctx context.Context, db *sql.DB, disk, folder string) ([]string, error) {
	lo, hi := folderRange(folder)
	rows, err := db.QueryContext(ctx, atOwnPathBelowSQL, disk, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("copies: %w", err)
		}
		list = append(list, string(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copies: %w", err)
	}
	return list, nil
}

// PathHeld says whether a file row holds a path.
func PathHeld(ctx context.Context, db *sql.DB, path string) (bool, error) {
	var held bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM files WHERE lpath = ?)", []byte(path)).Scan(&held); err != nil {
		return false, fmt.Errorf("files: %w", err)
	}
	return held, nil
}

// folderRange gives the bounds of what sits below a folder: / is followed by 0 in byte order.
func folderRange(folder string) (lo, hi []byte) {
	return []byte(folder + "/"), []byte(folder + "0")
}

// DisksBelow gives the disks that hold a copy or an entry below a folder, but for one.
func DisksBelow(ctx context.Context, db *sql.DB, folder, except string) ([]string, error) {
	lo, hi := folderRange(folder)
	rows, err := db.QueryContext(ctx, disksBelowSQL, lo, hi, except)
	if err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("disks: %w", err)
		}
		list = append(list, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	return list, nil
}

// CatchUp says that a copy reached its file's path by itself: only the row follows.
func CatchUp(ctx context.Context, db *sql.DB, disk string, fileID int64, done *RenameDone) error {
	_, err := db.ExecContext(ctx, `
		UPDATE copies SET old_lpath = NULL, old_lpath_key = NULL, gone_ns = NULL, gone_kind = NULL, nlink = ?, ctime_ns = COALESCE(?, ctime_ns), verified_ns = NULL
		WHERE file_id = ? AND disk_id = ?`, done.Nlink, done.Ctime, fileID, disk)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// SetConflict says that the copies of a file differ, every version kept.
func SetConflict(ctx context.Context, db *sql.DB, fileID, nowNS int64) error {
	if _, err := db.ExecContext(ctx, "UPDATE files SET state = 'conflict', updated_ns = ? WHERE id = ? AND state <> 'corrupt'", nowNS, fileID); err != nil {
		return fmt.Errorf("file %d: %w", fileID, err)
	}
	return nil
}

// JobDisks gives the disks with a queued trash job for a file.
func JobDisks(ctx context.Context, db *sql.DB, fileID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT dst_disk FROM jobs WHERE kind = 'trash' AND file_id = ? AND state = ? ORDER BY dst_disk", fileID, JobQueued)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		list = append(list, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return list, nil
}

// FolderRow says a folder renamed on the disk that holds it: its entries follow, and the other disks get
// a job, or an earlier job moves on (specs.md §15.12).
func FolderRow(ctx context.Context, db *sql.DB, disk, from, to string, others []string) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := rewriteEntries(ctx, tx, disk, from, to); err != nil {
			return err
		}
		if err := moveFolderJobs(ctx, tx, from, to); err != nil {
			return err
		}
		for _, other := range others {
			if err := queueFolderJob(ctx, tx, disk, other, from, to); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("folder %q: %w", from, err)
	}
	return nil
}

// FolderJob is a folder to rename on a disk.
type FolderJob struct {
	ID       int64
	Src, Dst string
	From, To string
}

type folderPayload struct {
	From string `json:"from_b64"`
	To   string `json:"to_b64"`
}

func encodeFolderPayload(from, to string) (string, error) {
	text, err := json.Marshal(folderPayload{
		From: base64.StdEncoding.EncodeToString([]byte(from)),
		To:   base64.StdEncoding.EncodeToString([]byte(to)),
	})
	if err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	return string(text), nil
}

func decodeFolderPayload(text string) (from, to string, err error) {
	var p folderPayload
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return "", "", fmt.Errorf("payload: %w", err)
	}
	a, err := base64.StdEncoding.DecodeString(p.From)
	if err != nil {
		return "", "", fmt.Errorf("payload: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(p.To)
	if err != nil {
		return "", "", fmt.Errorf("payload: %w", err)
	}
	return string(a), string(b), nil
}

func queueFolderJob(ctx context.Context, tx *sql.Tx, src, dst, from, to string) error {
	payload, err := encodeFolderPayload(from, to)
	if err != nil {
		return err
	}
	// One folder renamed on two disks: one operation for each other disk.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs(kind, src_disk, dst_disk, payload, state) SELECT 'rename', ?1, ?2, ?3, ?4
		WHERE NOT EXISTS(SELECT 1 FROM jobs WHERE kind = 'rename' AND dst_disk = ?2 AND payload = ?3 AND state = ?4)`,
		src, dst, payload, JobQueued)
	if err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return nil
}

// moveFolderJobs gives a further rename to the jobs whose target was that folder or below it, and drops
// one whose two paths become equal.
func moveFolderJobs(ctx context.Context, tx *sql.Tx, from, to string) error {
	jobs, err := queryAll(ctx, tx, scanFolderJob, "SELECT id, payload FROM jobs WHERE kind = 'rename'")
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	for _, j := range jobs {
		next, moved := FurtherRename(j.To, from, to)
		if !moved {
			continue
		}
		if next == j.From {
			if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", j.ID); err != nil {
				return fmt.Errorf("job: %w", err)
			}
			continue
		}
		payload, err := encodeFolderPayload(j.From, next)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", payload, j.ID); err != nil {
			return fmt.Errorf("job: %w", err)
		}
	}
	return nil
}

// FurtherRename says where a job's target goes when a folder is renamed from one name to another, and
// whether it is at or below that folder at all. A job whose paths then become equal goes.
func FurtherRename(target, from, to string) (string, bool) {
	if target != from && !strings.HasPrefix(target, from+"/") {
		return "", false
	}
	return to + strings.TrimPrefix(target, from), true
}

// FolderJobs gives the folders to rename on a disk, by their old path.
func FolderJobs(ctx context.Context, db *sql.DB, disk string) ([]FolderJob, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, src_disk, dst_disk, payload FROM jobs WHERE kind = 'rename' AND dst_disk = ? AND state = ? ORDER BY id", disk, JobQueued)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []FolderJob
	for rows.Next() {
		var j FolderJob
		var payload string
		if err := rows.Scan(&j.ID, &j.Src, &j.Dst, &payload); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		if j.From, j.To, err = decodeFolderPayload(payload); err != nil {
			return nil, err
		}
		list = append(list, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return list, nil
}

// DropJob removes a job that is over.
func DropJob(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", id); err != nil {
		return fmt.Errorf("job %d: %w", id, err)
	}
	return nil
}

// RenameRow is a copy that has not followed its file's rename yet.
type RenameRow struct {
	FileID   int64
	Place    string
	Target   string
	Size     sql.NullInt64
	MtimeNS  sql.NullInt64
	CtimeNS  sql.NullInt64
	BtimeNS  sql.NullInt64
	FileKey  sql.NullString
	Verified sql.NullInt64
}

// PendingRenames gives the copies of a disk that have not followed a rename, by where they sit.
func PendingRenames(ctx context.Context, db *sql.DB, disk string, after string, limit int) ([]RenameRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.file_id, c.old_lpath, f.lpath, c.size, c.mtime_ns, c.ctime_ns, c.btime_ns, c.file_key, c.verified_ns
		FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ? AND c.old_lpath > ?
		ORDER BY c.old_lpath LIMIT ?`, disk, []byte(after), limit)
	if err != nil {
		return nil, fmt.Errorf("renames: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []RenameRow{}
	for rows.Next() {
		var r RenameRow
		var place, target []byte
		if err := rows.Scan(&r.FileID, &place, &target, &r.Size, &r.MtimeNS, &r.CtimeNS, &r.BtimeNS, &r.FileKey, &r.Verified); err != nil {
			return nil, fmt.Errorf("renames: %w", err)
		}
		r.Place, r.Target = string(place), string(target)
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("renames: %w", err)
	}
	return list, nil
}

// BeginRename journals a rename on a disk, before anything is written to it.
func BeginRename(ctx context.Context, db *sql.DB, disk, path, target, fileKey string, nowNS int64) (*Op, error) {
	op := &Op{Disk: disk, Kind: OpRename, Path: path, Path2: target, FileKey: fileKey}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, path2, file_key, started_ns, state) VALUES (?, 'rename', ?, ?, ?, ?, 'planned')",
			disk, []byte(path), []byte(target), fileKey, nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		return writeEvent(ctx, tx, nowNS, "info", OpRename, op, "")
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", path, err)
	}
	return op, nil
}

// RenameDone is what a rename of a file leaves.
type RenameDone struct {
	Nlink    int64
	Ctime    sql.NullInt64 // what to record, when it should change
	Unverify bool          // a reprise cannot say whether an edit came before the move
}

// FinishRename says that a copy sits at the operation's target. It keeps old_lpath when the file has
// moved on meanwhile, so that no row is ever said to be at a name it is not at (specs.md §17.2).
func FinishRename(ctx context.Context, db *sql.DB, op *Op, done *RenameDone, links *Links) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if err := renameRow(ctx, tx, op.Disk, op.Path, op.Path2, op.FileKey, done); err != nil {
			return err
		}
		return writeLinks(ctx, tx, op.Disk, links)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

func renameRow(ctx context.Context, tx *sql.Tx, disk, place, target, fileKey string, done *RenameDone) error {
	var fileID int64
	var path []byte
	// At its place as the row says, whichever way: a rename back while this was planned clears old_lpath.
	err := tx.QueryRowContext(ctx, `
		SELECT c.file_id, f.lpath FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ?1 AND c.file_key = ?3 AND (c.old_lpath = ?2 OR (c.old_lpath IS NULL AND f.lpath = ?2))`, disk, []byte(place), fileKey).Scan(&fileID, &path)
	switch {
	case isNoRows(err):
		return nil
	case err != nil:
		return fmt.Errorf("copy: %w", err)
	}
	var old, key any
	if string(path) != target {
		old, key = []byte(target), keyOf(target)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE copies SET old_lpath = ?1, old_lpath_key = ?2, gone_ns = NULL, gone_kind = NULL,
			nlink = ?3, ctime_ns = COALESCE(?4, ctime_ns), verified_ns = CASE WHEN ?5 THEN NULL ELSE verified_ns END
		WHERE file_id = ?6 AND disk_id = ?7`,
		old, key, done.Nlink, done.Ctime, done.Unverify, fileID, disk)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// FinishFolderRename says that a folder and everything below it sit at the operation's target: each
// copy below it says so, the entries below it follow, and the job goes.
func FinishFolderRename(ctx context.Context, db *sql.DB, op *Op, jobID int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		if err := folderRows(ctx, tx, op.Disk, op.Path, op.Path2, jobID == 0); err != nil {
			return err
		}
		if err := rewriteEntries(ctx, tx, op.Disk, op.Path, op.Path2); err != nil {
			return err
		}
		if jobID != 0 {
			_, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", jobID)
			if err != nil {
				return fmt.Errorf("job: %w", err)
			}
			return nil
		}
		return dropJobOfFolder(ctx, tx, op.Disk, op.Path)
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

func dropJobOfFolder(ctx context.Context, tx *sql.Tx, disk, from string) error {
	jobs, err := queryAll(ctx, tx, scanFolderJob, "SELECT id, payload FROM jobs WHERE kind = 'rename' AND dst_disk = ?", disk)
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	for _, j := range jobs {
		if j.From != from {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", j.ID); err != nil {
			return fmt.Errorf("job: %w", err)
		}
	}
	return nil
}

// folderRows moves the copies of a disk that are behind, below a folder, to below its target. A row whose
// clock runs was not where it says at the last look, and stays as it says, as does a row at its own file's
// path, which nothing told to follow: the next look says what became of each (specs.md §15.12). In a reprise
// a row can also be at its file's path as a rename back cleared it, with another copy there to say so: that
// copy moved, and must not read as a user's rename. Nor must a row whose clock says moved, as a scan between
// the crash and the reprise reads this very move.
func folderRows(ctx context.Context, tx *sql.Tx, disk, from, to string, reprise bool) error {
	type row struct {
		fileID      int64
		place, file string
	}
	lo, hi := folderRange(from)
	list, err := queryAll(ctx, tx, func(rows *sql.Rows) (row, error) {
		var r row
		var place, file []byte
		if err := rows.Scan(&r.fileID, &place, &file); err != nil {
			return r, fmt.Errorf("read: %w", err)
		}
		r.place, r.file = string(place), string(file)
		return r, nil
	}, folderRowsSQL, disk, lo, hi, reprise)
	if err != nil {
		return fmt.Errorf("copies: %w", err)
	}
	for _, r := range list {
		next := to + strings.TrimPrefix(r.place, from)
		var old, key any
		if next != r.file {
			old, key = []byte(next), keyOf(next)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET old_lpath = ?, old_lpath_key = ?, gone_ns = NULL, gone_kind = NULL WHERE file_id = ? AND disk_id = ?", old, key, r.fileID, disk); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	return nil
}

// TrashJob is a copy to send to the trash.
type TrashJob struct {
	ID     int64
	FileID int64
	Src    string
	Dst    string
	Reason string
	State  string
}

// TrashJobs gives the trash jobs of a disk, by id.
func TrashJobs(ctx context.Context, db *sql.DB, disk string) ([]TrashJob, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, file_id, src_disk, dst_disk, reason, state FROM jobs WHERE kind = 'trash' AND dst_disk = ? ORDER BY id", disk)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []TrashJob
	for rows.Next() {
		var j TrashJob
		if err := rows.Scan(&j.ID, &j.FileID, &j.Src, &j.Dst, &j.Reason, &j.State); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		list = append(list, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return list, nil
}

// DropFileJobs removes every queued trash job of a file: the deletion no longer stands.
func DropFileJobs(ctx context.Context, db *sql.DB, fileID int64) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'trash' AND file_id = ? AND state = ?", fileID, JobQueued); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	return nil
}

// QueueRenamedJob queues the trash job of a copy whose rename a disk refuses.
func QueueRenamedJob(ctx context.Context, db *sql.DB, fileID int64, disk string) error {
	_, err := db.ExecContext(ctx, "INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', ?, ?, ?, ?, 'renamed')", fileID, disk, disk, JobQueued)
	if err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return nil
}

// GuardRead is what the guard of a disk reads, from rows and events, so that no crash can lose a hold
// (specs.md §15.6).
type GuardRead struct {
	UpTo        sql.NullInt64 // of the last confirm or reject
	LastEventID int64         // of the last confirm or reject
	HasGuard    bool          // a guard event since then
	GuardNS     int64         // the time of that event
	Gone        int64         // copies with gone_kind = gone and no old_lpath, of a file with no copy moved, later than UpTo
	GoneLeft    int64         // those of them with something left to send to the trash
	Events      int64         // copy-gone events within the window, later than UpTo
	EventsLeft  int64         // those with something left to send
}

// ReadGuard reads what the guard of a disk looks at.
func ReadGuard(ctx context.Context, db *sql.DB, disk string, nowNS, windowNS int64) (*GuardRead, error) {
	g := &GuardRead{}
	err := db.QueryRowContext(ctx, `
		SELECT json_extract(data, '$.up_to') FROM events
		WHERE kind IN ('guard-confirm', 'guard-reject') AND json_extract(data, '$.disk') = ?
		ORDER BY id DESC LIMIT 1`, disk).Scan(&g.UpTo)
	if err != nil && !isNoRows(err) {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, `
		SELECT id FROM events
		WHERE kind IN ('guard-confirm', 'guard-reject', 'guard-clear') AND json_extract(data, '$.disk') = ?
		ORDER BY id DESC LIMIT 1`, disk).Scan(&g.LastEventID)
	if err != nil && !isNoRows(err) {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, `
		SELECT ts_ns FROM events WHERE kind = 'guard' AND id > ? AND json_extract(data, '$.disk') = ? ORDER BY id LIMIT 1`, g.LastEventID, disk).Scan(&g.GuardNS)
	switch {
	case err == nil:
		g.HasGuard = true
	case !isNoRows(err):
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	upTo := int64(-1 << 63)
	if g.UpTo.Valid {
		upTo = g.UpTo.Int64
	}
	err = db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(EXISTS(SELECT 1 FROM copies o WHERE o.file_id = c.file_id AND o.disk_id <> c.disk_id
			AND (o.gone_kind IS NULL OR o.disk_id IN (SELECT id FROM disks WHERE state IN ('missing', 'retired'))))), 0)
		FROM copies c
		WHERE c.disk_id = ?1 AND c.gone_kind = 'gone' AND c.old_lpath IS NULL AND c.gone_ns > ?2
			AND NOT EXISTS(`+movedHere+`)`, disk, upTo).Scan(&g.Gone, &g.GoneLeft)
	if err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(COALESCE(json_extract(data, '$.nothing_left'), 0) = 0), 0) FROM events
		WHERE kind = 'copy-gone' AND ts_ns > ?1 AND json_extract(data, '$.disk') = ?2 AND json_extract(data, '$.gone_ns') > ?3`,
		nowNS-windowNS, disk, upTo).Scan(&g.Events, &g.EventsLeft)
	if err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return g, nil
}

// movedHere finds a copy of the same file found moved, on a disk a look can reach: a kind left from before
// a disk went away says nothing now, and must not hide a deletion from the guard (specs.md §15.6).
const movedHere = `SELECT 1 FROM copies o WHERE o.file_id = c.file_id AND o.gone_kind = 'moved' AND o.disk_id IN (SELECT id FROM disks WHERE state NOT IN ('missing', 'retired'))`

// CopiesBefore counts the copies on a disk first seen before a time: those it had when a guard tripped,
// as what landed since must not dilute a hold that stands (specs.md §15.6).
func CopiesBefore(ctx context.Context, db *sql.DB, disk string, ns int64) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM copies WHERE disk_id = ? AND first_seen_ns < ?", disk, ns).Scan(&count); err != nil {
		return 0, fmt.Errorf("copies of %q: %w", disk, err)
	}
	return count, nil
}

// CopyCount counts the copies on a disk.
func CopyCount(ctx context.Context, db *sql.DB, disk string) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM copies WHERE disk_id = ?", disk).Scan(&count); err != nil {
		return 0, fmt.Errorf("copies of %q: %w", disk, err)
	}
	return count, nil
}

// WriteGuardClear records that a hold ended by itself, so that the next trip is another one, with a time
// of its own for a confirm to go by.
func WriteGuardClear(ctx context.Context, db *sql.DB, disk string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		return insertEvent(ctx, tx, nowNS, "info", EventGuardClear, &eventRow{Disk: disk})
	})
}

// TrashJobState says the state of a copy's trash job, or "" with none.
func TrashJobState(ctx context.Context, db *sql.DB, fileID int64, disk string) (string, error) {
	var state string
	err := db.QueryRowContext(ctx, "SELECT state FROM jobs WHERE kind = 'trash' AND file_id = ? AND dst_disk = ? LIMIT 1", fileID, disk).Scan(&state)
	if err != nil && !isNoRows(err) {
		return "", fmt.Errorf("job: %w", err)
	}
	return state, nil
}

// QueuedTrash says whether a copy has a trash job queued for that reason: a decision leaves it as it is.
func QueuedTrash(ctx context.Context, db *sql.DB, fileID int64, disk, reason string) (bool, error) {
	var queued bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE kind = 'trash' AND file_id = ? AND dst_disk = ? AND state = ? AND reason = ?)",
		fileID, disk, JobQueued, reason).Scan(&queued)
	if err != nil {
		return false, fmt.Errorf("job: %w", err)
	}
	return queued, nil
}

// DropStaleCanceled forgets what a rejection said of a copy no longer behind: whichever way it caught up,
// a later refused rename of it is another question.
func DropStaleCanceled(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM jobs WHERE kind = 'trash' AND state = ? AND NOT EXISTS(
			SELECT 1 FROM copies c WHERE c.file_id = jobs.file_id AND c.disk_id = jobs.dst_disk AND c.old_lpath IS NOT NULL)`, JobCanceled)
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	return nil
}

// WriteGuardEvent records that the guard of a disk tripped.
func WriteGuardEvent(ctx context.Context, db *sql.DB, disk string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		return insertEvent(ctx, tx, nowNS, "warning", EventGuard, &eventRow{Disk: disk})
	})
}

// GuardPaths gives the paths the guard of a disk holds, in byte order: what is gone there, and the
// trash jobs that wait for it.
func GuardPaths(ctx context.Context, db *sql.DB, disk string, upTo sql.NullInt64) ([]string, error) {
	after := int64(-1 << 63)
	if upTo.Valid {
		after = upTo.Int64
	}
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(c.old_lpath, f.lpath) AS p FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ?1 AND c.gone_kind = 'gone' AND c.gone_ns > ?2
			AND NOT EXISTS(`+movedHere+`)
		UNION
		SELECT COALESCE(c.old_lpath, f.lpath) AS p FROM jobs j JOIN files f ON f.id = j.file_id
		LEFT JOIN copies c ON c.file_id = j.file_id AND c.disk_id = j.dst_disk
		WHERE j.kind = 'trash' AND j.state = 'queued' AND j.src_disk = ?1
		ORDER BY p`, disk, after)
	if err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	list := []string{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("guard of %q: %w", disk, err)
		}
		list = append(list, string(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return list, nil
}

// ConfirmGuard says that what the guard of a disk held up to its last guard event may go: a copy a
// refused rename would send gets its job, as it has no time to be compared with. It says nothing was
// valid when there was nothing to confirm.
func ConfirmGuard(ctx context.Context, db *sql.DB, disk string, nowNS int64, refused []RefusedCopy) (sql.NullInt64, error) {
	var upTo sql.NullInt64
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		var err error
		if upTo, err = lastGuard(ctx, tx, disk); err != nil || !upTo.Valid {
			return err
		}
		for _, r := range refused {
			_, err := tx.ExecContext(ctx, "INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', ?, ?, ?, ?, 'renamed')", r.FileID, disk, disk, JobQueued)
			if err != nil {
				return fmt.Errorf("job: %w", err)
			}
		}
		// Nobody was asked about what the guard left out, beside a copy that moved: should that copy stop reading
		// so, the guard asks then.
		_, err = tx.ExecContext(ctx, `
			UPDATE copies AS c SET gone_ns = max(?1, ?2 + 1)
			WHERE c.disk_id = ?3 AND c.gone_kind = 'gone' AND c.old_lpath IS NULL AND c.gone_ns <= ?2 AND EXISTS(`+movedHere+`)`, nowNS, upTo.Int64, disk)
		if err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		return insertEvent(ctx, tx, nowNS, "info", EventGuardConfirm, &eventRow{Disk: disk, UpTo: &upTo.Int64})
	})
	if err != nil {
		return upTo, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return upTo, nil
}

func lastGuard(ctx context.Context, tx *sql.Tx, disk string) (sql.NullInt64, error) {
	var last int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM events WHERE kind IN ('guard-confirm', 'guard-reject', 'guard-clear') AND json_extract(data, '$.disk') = ? ORDER BY id DESC LIMIT 1`, disk).Scan(&last)
	if err != nil && !isNoRows(err) {
		return sql.NullInt64{}, fmt.Errorf("read: %w", err)
	}
	var at int64
	err = tx.QueryRowContext(ctx, "SELECT ts_ns FROM events WHERE kind = 'guard' AND id > ? AND json_extract(data, '$.disk') = ? ORDER BY id LIMIT 1", last, disk).Scan(&at)
	switch {
	case isNoRows(err):
		return sql.NullInt64{}, nil
	case err != nil:
		return sql.NullInt64{}, fmt.Errorf("read: %w", err)
	}
	return sql.NullInt64{Int64: at, Valid: true}, nil
}

// RefusedCopy is a copy whose rename a disk refuses, for the guard and for reject.
type RefusedCopy struct {
	FileID int64
	Place  string
}

// RejectGuard says that what the guard of a disk held stays: its gone rows go with nothing sent to the
// trash, a file left with no copy goes, its trash jobs go, and a copy that a refused rename would send
// is told never to go.
func RejectGuard(ctx context.Context, db *sql.DB, disk string, nowNS int64, refused []RefusedCopy) (sql.NullInt64, error) {
	var upTo sql.NullInt64
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		var err error
		if upTo, err = lastGuard(ctx, tx, disk); err != nil || !upTo.Valid {
			return err
		}
		if err := rejectRows(ctx, tx, disk, upTo.Int64, nowNS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state = ? WHERE kind = 'trash' AND state = ? AND src_disk = ? AND reason = 'renamed'", JobCanceled, JobQueued, disk); err != nil {
			return fmt.Errorf("jobs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'trash' AND state = ? AND src_disk = ?", JobQueued, disk); err != nil {
			return fmt.Errorf("jobs: %w", err)
		}
		for _, r := range refused {
			_, err := tx.ExecContext(ctx, "INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', ?, ?, ?, ?, 'renamed')", r.FileID, disk, disk, JobCanceled)
			if err != nil {
				return fmt.Errorf("job: %w", err)
			}
		}
		return insertEvent(ctx, tx, nowNS, "info", EventGuardReject, &eventRow{Disk: disk, UpTo: &upTo.Int64})
	})
	if err != nil {
		return upTo, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return upTo, nil
}

func rejectRows(ctx context.Context, tx *sql.Tx, disk string, upTo, nowNS int64) error {
	ids, err := queryAll(ctx, tx, func(rows *sql.Rows) (int64, error) {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("read: %w", err)
		}
		return id, nil
	}, `
		SELECT c.file_id FROM copies c
		WHERE c.disk_id = ?1 AND c.gone_kind = 'gone' AND c.gone_ns <= ?2`, disk, upTo)
	if err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	for _, id := range ids {
		var first int64
		if err := tx.QueryRowContext(ctx, "SELECT first_seen_ns FROM copies WHERE file_id = ? AND disk_id = ?", id, disk).Scan(&first); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ? AND disk_id = ?", id, disk); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if err := dropFileIfEmpty(ctx, tx, id); err != nil {
			return err
		}
		// What is left is as after a decision: nothing else would ever say so (specs.md §15.12).
		var sizes, verified int64
		var earliest sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT count(DISTINCT size), count(verified_ns), min(first_seen_ns) FROM copies WHERE file_id = ?", id).Scan(&sizes, &verified, &earliest); err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		change := FileChange{ID: id, DropHash: earliest.Int64 > first || verified == 0, ClearConflict: sizes == 1}
		if err := applyFile(ctx, tx, nowNS, &change); err != nil {
			return err
		}
	}
	return nil
}
