package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// KnownCopy is what the catalog holds about one copy, to compare with a walk.
type KnownCopy struct {
	Path    string
	Size    sql.NullInt64
	MtimeNS sql.NullInt64
	CtimeNS sql.NullInt64
	FileKey sql.NullString
}

// EachKnownCopy gives the copies a disk is known to hold, by path in byte order, and keeps none:
// at 200,000 files a map of them took about 40 MB (specs.md §26). CROSS JOIN keeps files as the
// outer table, whose index on lpath is the order wanted, so that nothing has to be sorted.
func EachKnownCopy(ctx context.Context, db *sql.DB, disk string, fn func(*KnownCopy)) error {
	rows, err := db.QueryContext(ctx, `
		SELECT f.lpath, c.size, c.mtime_ns, c.ctime_ns, c.file_key
		FROM files f CROSS JOIN copies c ON c.file_id = f.id AND c.disk_id = ?
		ORDER BY f.lpath`, disk)
	if err != nil {
		return fmt.Errorf("copies of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p []byte
		var k KnownCopy
		if err := rows.Scan(&p, &k.Size, &k.MtimeNS, &k.CtimeNS, &k.FileKey); err != nil {
			return fmt.Errorf("copies of %q: %w", disk, err)
		}
		k.Path = string(p)
		fn(&k)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("copies of %q: %w", disk, err)
	}
	return nil
}

// NewCopy is a copy a walk found that the catalog does not hold.
type NewCopy struct {
	Path    string
	Size    int64
	MtimeNS int64
	CtimeNS int64
	BtimeNS *int64
	FileKey string
	Nlink   int64
}

// EntryRow is a folder, symlink or ignored file a walk found.
type EntryRow struct {
	Path    string
	Kind    string
	Mode    int64
	MtimeNS int64
	Target  *string
}

// WriteWalk writes what the walk of one disk found, in one transaction, so a walk is written whole
// or not at all (specs.md §15.10). Files new to the catalog take their ids in the order given. A
// copy of another size than its file makes that file a conflict.
func WriteWalk(ctx context.Context, db *sql.DB, disk string, nowNS int64, copies []NewCopy, entries []EntryRow) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := writeCopies(ctx, tx, disk, nowNS, copies); err != nil {
			return err
		}
		return writeEntries(ctx, tx, disk, entries)
	})
	if err != nil {
		return fmt.Errorf("walk of %q: %w", disk, err)
	}
	return nil
}

func writeCopies(ctx context.Context, tx *sql.Tx, disk string, nowNS int64, copies []NewCopy) error {
	find, err := tx.PrepareContext(ctx, "SELECT id, size FROM files WHERE lpath = ?")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = find.Close() }()
	conflict, err := tx.PrepareContext(ctx, "UPDATE files SET state = 'conflict', updated_ns = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = conflict.Close() }()
	// Its policy and state are what comes next gives it. Should that not happen, the next scan
	// gives them.
	addFile, err := tx.PrepareContext(ctx, `
		INSERT INTO files(lpath, lpath_key, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
		VALUES (?, ?, ?, ?, 0, 'deferred', 'pending', ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = addFile.Close() }()
	addCopy, err := tx.PrepareContext(ctx, `
		INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, btime_ns, created_by, first_seen_ns, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'user', ?, 'ok')`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = addCopy.Close() }()
	for i := range copies {
		c := &copies[i]
		var id, size int64
		err := find.QueryRowContext(ctx, []byte(c.Path)).Scan(&id, &size)
		switch {
		case err == nil:
			if size != c.Size {
				if _, err := conflict.ExecContext(ctx, nowNS, id); err != nil {
					return fmt.Errorf("%q: conflict: %w", c.Path, err)
				}
			}
		case isNoRows(err):
			result, err := addFile.ExecContext(ctx, []byte(c.Path), keyOf(c.Path), c.Size, c.MtimeNS, nowNS, nowNS)
			if err != nil {
				return fmt.Errorf("%q: file: %w", c.Path, err)
			}
			if id, err = result.LastInsertId(); err != nil {
				return fmt.Errorf("%q: file: %w", c.Path, err)
			}
		default:
			return fmt.Errorf("%q: file: %w", c.Path, err)
		}
		if _, err := addCopy.ExecContext(ctx, id, disk, c.FileKey, c.Nlink, c.Size, c.MtimeNS, c.CtimeNS, c.BtimeNS, nowNS); err != nil {
			return fmt.Errorf("%q: copy: %w", c.Path, err)
		}
	}
	return nil
}

// writeEntries makes the entries of a disk match a walk. A folder Atoll created stays its own, as
// only it may remove it (specs.md §6.1).
func writeEntries(ctx context.Context, tx *sql.Tx, disk string, entries []EntryRow) error {
	seen := make(map[string]bool, len(entries))
	for i := range entries {
		e := &entries[i]
		seen[e.Path] = true
		var target []byte
		if e.Target != nil {
			target = []byte(*e.Target)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO entries(lpath, lpath_key, disk_id, kind, mode, mtime_ns, target, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'user')
			ON CONFLICT(lpath, disk_id) DO UPDATE SET
				kind = excluded.kind, mode = excluded.mode, mtime_ns = excluded.mtime_ns, target = excluded.target`,
			[]byte(e.Path), keyOf(e.Path), disk, e.Kind, e.Mode, e.MtimeNS, target)
		if err != nil {
			return fmt.Errorf("entry %q: %w", e.Path, err)
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT lpath FROM entries WHERE disk_id = ?", disk)
	if err != nil {
		return fmt.Errorf("entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var gone [][]byte
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return fmt.Errorf("entries: %w", err)
		}
		if !seen[string(p)] {
			gone = append(gone, p)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("entries: %w", err)
	}
	for _, p := range gone {
		if _, err := tx.ExecContext(ctx, "DELETE FROM entries WHERE lpath = ? AND disk_id = ?", p, disk); err != nil {
			return fmt.Errorf("entry %q: %w", p, err)
		}
	}
	return nil
}

// CopyRow is one copy of a file.
type CopyRow struct {
	Disk        string
	CreatedBy   string
	State       string
	Size        sql.NullInt64
	MtimeNS     sql.NullInt64
	CtimeNS     sql.NullInt64
	BtimeNS     sql.NullInt64
	FirstSeenNS int64
	Verified    bool
}

// FileRow is one file, with its copies.
type FileRow struct {
	ID             int64
	Path           string
	Size           int64
	MtimeNS        int64
	Hash           []byte
	ReplicasTarget int
	Rule           sql.NullString
	Mode           string
	State          string
	Original       sql.NullString
	Copies         []CopyRow
}

// filePageSize is how many files FileReader reads at a time.
const filePageSize = 1000

// FileReader reads every file with its copies, by path in byte order, a page at a time: no read
// transaction stays open, so a --full that takes days does not hold the WAL back, and nothing is
// kept that the caller does not keep (specs.md §26). Strings that repeat are shared.
type FileReader struct {
	db     *sql.DB
	after  []byte
	shared map[string]string
}

// NewFileReader starts before the first file.
func NewFileReader(db *sql.DB) *FileReader {
	return &FileReader{db: db, after: []byte{}, shared: map[string]string{}}
}

func (r *FileReader) share(raw []byte) string {
	if s, ok := r.shared[string(raw)]; ok {
		return s
	}
	s := string(raw)
	r.shared[s] = s
	return s
}

func (r *FileReader) shareNull(raw []byte) sql.NullString {
	if raw == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: r.share(raw), Valid: true}
}

// Next gives the next files, and none at the end. What it gives is the caller's.
func (r *FileReader) Next(ctx context.Context) ([]FileRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.id, f.lpath, f.size, f.mtime_ns, f.blake3, f.replicas_target, f.rule, f.mode, f.state, f.original_disk,
			c.disk_id, c.size, c.mtime_ns, c.ctime_ns, c.btime_ns, c.created_by, c.first_seen_ns, c.verified_ns IS NOT NULL, c.state
		FROM (SELECT * FROM files WHERE lpath > ? ORDER BY lpath LIMIT ?) f
		LEFT JOIN copies c ON c.file_id = f.id
		ORDER BY f.lpath, c.disk_id`, r.after, filePageSize)
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	files := []FileRow{}
	// RawBytes: most of these are shared, and copying each first is what took the memory.
	var p, rule, mode, state, original, disk, createdBy, copyState sql.RawBytes
	for rows.Next() {
		var f FileRow
		var c CopyRow
		var firstSeen sql.NullInt64
		var verified sql.NullBool
		err := rows.Scan(&f.ID, &p, &f.Size, &f.MtimeNS, &f.Hash, &f.ReplicasTarget, &rule, &mode, &state, &original,
			&disk, &c.Size, &c.MtimeNS, &c.CtimeNS, &c.BtimeNS, &createdBy, &firstSeen, &verified, &copyState)
		if err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
		if n := len(files); n == 0 || files[n-1].ID != f.ID {
			f.Path = string(p)
			f.Rule, f.Original = r.shareNull(rule), r.shareNull(original)
			f.Mode, f.State = r.share(mode), r.share(state)
			files = append(files, f)
		}
		if disk != nil {
			c.Disk, c.CreatedBy, c.State = r.share(disk), r.share(createdBy), r.share(copyState)
			c.FirstSeenNS, c.Verified = firstSeen.Int64, verified.Bool
			last := &files[len(files)-1]
			last.Copies = append(last.Copies, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	if len(files) > 0 {
		r.after = []byte(files[len(files)-1].Path)
	}
	return files, nil
}

// EntryKey says what an entry is, for the plan.
type EntryKey struct {
	Disk string
	Path string
	Kind string
}

// Entries gives every entry.
func Entries(ctx context.Context, db *sql.DB) ([]EntryKey, error) {
	rows, err := db.QueryContext(ctx, "SELECT disk_id, lpath, kind FROM entries")
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := []EntryKey{}
	shared := FileReader{shared: map[string]string{}}
	var disk, p, kind []byte
	for rows.Next() {
		if err := rows.Scan(&disk, &p, &kind); err != nil {
			return nil, fmt.Errorf("entries: %w", err)
		}
		entries = append(entries, EntryKey{Disk: shared.share(disk), Path: string(p), Kind: shared.share(kind)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	return entries, nil
}

// Disks gives every disk, retired ones included, by position.
func Disks(ctx context.Context, db *sql.DB) ([]Disk, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, fs_uuid, fs_type, mount_path, root, workdir, role, position, COALESCE(domain, ''), tags,
			COALESCE(capacity_bytes, 0), COALESCE(free_bytes, 0), state, COALESCE(last_seen_ns, 0)
		FROM disks ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := []Disk{}
	for rows.Next() {
		var d Disk
		var tags string
		if err := rows.Scan(&d.ID, &d.UUID, &d.FS, &d.Mount, &d.Root, &d.Workdir, &d.Role, &d.Position, &d.Domain, &tags, &d.Capacity, &d.Free, &d.State, &d.LastSeenNS); err != nil {
			return nil, fmt.Errorf("disks: %w", err)
		}
		if err := json.Unmarshal([]byte(tags), &d.Tags); err != nil {
			return nil, fmt.Errorf("disk %q: tags: %w", d.ID, err)
		}
		list = append(list, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	return list, nil
}

// FileUpdate is what a file's policy, original and plan give it.
type FileUpdate struct {
	ID             int64
	Size           int64
	MtimeNS        int64
	ReplicasTarget int
	Rule           sql.NullString
	Mode           string
	State          string
	Original       sql.NullString
}

// CopyMark is a new state for a copy.
type CopyMark struct {
	FileID int64
	Disk   string
	State  string
}

// StateChange is a new state for a file, and nothing else.
type StateChange struct {
	ID    int64
	State string
}

// ApplyPlan writes what the plan and the policies say, in one transaction.
func ApplyPlan(ctx context.Context, db *sql.DB, nowNS int64, files []FileUpdate, states []StateChange, marks []CopyMark) error {
	if len(files) == 0 && len(states) == 0 && len(marks) == 0 {
		return nil
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		update, err := tx.PrepareContext(ctx, `
			UPDATE files SET size = ?, mtime_ns = ?, replicas_target = ?, rule = ?, mode = ?, state = ?, original_disk = ?, updated_ns = ?
			WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		defer func() { _ = update.Close() }()
		for i := range files {
			f := &files[i]
			if _, err := update.ExecContext(ctx, f.Size, f.MtimeNS, f.ReplicasTarget, f.Rule, f.Mode, f.State, f.Original, nowNS, f.ID); err != nil {
				return fmt.Errorf("file %d: %w", f.ID, err)
			}
		}
		state, err := tx.PrepareContext(ctx, "UPDATE files SET state = ?, updated_ns = ? WHERE id = ?")
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		defer func() { _ = state.Close() }()
		for _, s := range states {
			if _, err := state.ExecContext(ctx, s.State, nowNS, s.ID); err != nil {
				return fmt.Errorf("file %d: %w", s.ID, err)
			}
		}
		mark, err := tx.PrepareContext(ctx, "UPDATE copies SET state = ? WHERE file_id = ? AND disk_id = ?")
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		defer func() { _ = mark.Close() }()
		for _, m := range marks {
			if _, err := mark.ExecContext(ctx, m.State, m.FileID, m.Disk); err != nil {
				return fmt.Errorf("copy of file %d on %q: %w", m.FileID, m.Disk, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	return nil
}

// HashWrite is what reading the copies of one file gives. Hash is nil when the file already had
// one, which stays.
type HashWrite struct {
	FileID   int64
	Hash     []byte
	Chunks   []byte
	Verified []string // the disks whose copy has the file's hash
	Conflict bool
}

// WriteHashes writes what reading one file gave, in its own transaction, so that an interrupted
// scan loses little (specs.md §15.10).
func WriteHashes(ctx context.Context, db *sql.DB, nowNS int64, w *HashWrite) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if w.Hash != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE files SET blake3 = ?, updated_ns = ? WHERE id = ?", w.Hash, nowNS, w.FileID); err != nil {
				return fmt.Errorf("hash: %w", err)
			}
			if len(w.Chunks) > 0 {
				if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO file_chunks(file_id, chunks) VALUES (?, ?)", w.FileID, w.Chunks); err != nil {
					return fmt.Errorf("chunks: %w", err)
				}
			}
		}
		for _, disk := range w.Verified {
			if _, err := tx.ExecContext(ctx, "UPDATE copies SET verified_ns = ? WHERE file_id = ? AND disk_id = ?", nowNS, w.FileID, disk); err != nil {
				return fmt.Errorf("copy on %q: %w", disk, err)
			}
		}
		if w.Conflict {
			if _, err := tx.ExecContext(ctx, "UPDATE files SET state = 'conflict', updated_ns = ? WHERE id = ?", nowNS, w.FileID); err != nil {
				return fmt.Errorf("conflict: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("file %d: %w", w.FileID, err)
	}
	return nil
}

// ConflictPaths gives every file in conflict, in byte order.
func ConflictPaths(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT lpath FROM files WHERE state = 'conflict' ORDER BY lpath")
	if err != nil {
		return nil, fmt.Errorf("conflicts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := []string{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("conflicts: %w", err)
		}
		paths = append(paths, string(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conflicts: %w", err)
	}
	return paths, nil
}

func isNoRows(err error) bool {
	return err == sql.ErrNoRows //nolint:errorlint // database/sql returns it as is, from Scan
}
