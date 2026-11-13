package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
)

// LinkCopy is a copy of a disk as the link guard reads it (specs.md §17).
type LinkCopy struct {
	FileID int64
	// FileKey is its inode, empty for a copy not on the disk yet.
	FileKey string
	// Place is where it sits: its file's path, or the path it has not followed a rename from yet.
	Place string
	// There says that the last look found it at its place, and that it is on the disk.
	There bool
}

// LinkCopies reads the copies of a disk, by file id: room is told how many there are, then each is told of each, so
// that whoever asks holds them once, in a list of its own and of their number. A disk of 200,000 copies is most of
// what a pass may hold (specs.md §26). The copy each is given is the same one each time.
func LinkCopies(ctx context.Context, db *sql.DB, disk string, room func(n int), each func(c *LinkCopy)) error {
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM copies WHERE disk_id = ?", disk).Scan(&n); err != nil {
		return fmt.Errorf("copies: %w", err)
	}
	room(n)
	rows, err := db.QueryContext(ctx, `
		SELECT c.file_id, COALESCE(c.file_key, ''), COALESCE(c.old_lpath, f.lpath), c.state <> 'pending' AND c.gone_ns IS NULL
		FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = ? ORDER BY c.file_id`, disk)
	if err != nil {
		return fmt.Errorf("copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var c LinkCopy
	// RawBytes: each is copied once, into the string that is kept.
	var key, place sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&c.FileID, &key, &place, &c.There); err != nil {
			return fmt.Errorf("copies: %w", err)
		}
		c.FileKey, c.Place = string(key), string(place)
		each(&c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("copies: %w", err)
	}
	return nil
}

// GuardUsed says whether the link guard ever journaled an operation on a disk: only then can its links folder hold
// anything of Atoll's (specs.md §17).
func GuardUsed(ctx context.Context, db *sql.DB, disk string) (bool, error) {
	var used bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM ops WHERE disk_id = ? AND link IS NOT NULL)", disk).Scan(&used); err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	return used, nil
}

// TrashKeys gives the inodes of what a disk's trash holds: the entries whose operation is done.
func TrashKeys(ctx context.Context, db *sql.DB, disk string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.file_key FROM trash t JOIN ops o ON o.id = t.op_id WHERE t.disk_id = ? AND o.state = 'done' AND o.file_key IS NOT NULL`, disk)
	if err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("trash: %w", err)
		}
		keys[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trash: %w", err)
	}
	return keys, nil
}

// idsPerQuery is how many ids one query asks for, well under SQLite's limit on parameters.
const idsPerQuery = 500

// eachIDs runs a query once for each slice of ids, its last parameters being those ids.
func eachIDs(ids []int64, run func(marks string, args []any) error) error {
	for start := 0; start < len(ids); start += idsPerQuery {
		part := ids[start:min(start+idsPerQuery, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		if err := run(strings.TrimSuffix(strings.Repeat("?,", len(part)), ","), args); err != nil {
			return err
		}
	}
	return nil
}

// FilePaths gives the path of each of those files the catalog still has.
func FilePaths(ctx context.Context, db *sql.DB, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	err := eachIDs(ids, func(marks string, args []any) error {
		rows, err := db.QueryContext(ctx, "SELECT id, lpath FROM files WHERE id IN ("+marks+")", args...) //nolint:gosec // only marks
		if err != nil {
			return fmt.Errorf("files: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var path []byte
			if err := rows.Scan(&id, &path); err != nil {
				return fmt.Errorf("files: %w", err)
			}
			out[id] = string(path)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("files: %w", err)
		}
		return nil
	})
	return out, err
}

// GonePaths gives, for each of those files, the path the last copy-gone event of a disk gives it: where its copy there
// was when a look found it gone. It names a link whose file's row went with its last copy (specs.md §17).
func GonePaths(ctx context.Context, db *sql.DB, disk string, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	err := eachIDs(ids, func(marks string, args []any) error {
		rows, err := db.QueryContext(ctx, `
			SELECT json_extract(data, '$.file'), COALESCE(json_extract(data, '$.path'), ''), COALESCE(json_extract(data, '$.path_b64'), '')
			FROM events WHERE kind = 'copy-gone' AND json_extract(data, '$.disk') = ? AND json_extract(data, '$.file') IN (`+marks+`) ORDER BY id`, //nolint:gosec // only marks
			append([]any{disk}, args...)...)
		if err != nil {
			return fmt.Errorf("events: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var path, encoded string
			if err := rows.Scan(&id, &path, &encoded); err != nil {
				return fmt.Errorf("events: %w", err)
			}
			if encoded != "" {
				raw, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return fmt.Errorf("events: the path of file %d: %w", id, err)
				}
				path = string(raw)
			}
			if path != "" {
				out[id] = path
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("events: %w", err)
		}
		return nil
	})
	return out, err
}

// LinkStart is what an operation of the link guard on a link needs: the file it is of, the logical path of its copy
// when that is known, and the inode it leads to (specs.md §17).
type LinkStart struct {
	FileID  int64
	Path    string
	FileKey string
}

// BeginLinks journals operations of the link guard of one kind, link or unlink, in one transaction, before anything is
// written. They write no event: what one makes or removes is a second name of a file, which keeps another.
func BeginLinks(ctx context.Context, db *sql.DB, disk, kind string, list []LinkStart, nowNS int64) ([]Op, error) {
	if kind != OpLink && kind != OpUnlink {
		return nil, fmt.Errorf("journal: no link operation %q", kind)
	}
	out := make([]Op, 0, len(list))
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		insert, err := tx.PrepareContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state, link) VALUES (?, ?, ?, ?, ?, 'planned', ?)")
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		defer func() { _ = insert.Close() }()
		out = out[:0]
		for i := range list {
			l := &list[i]
			if l.FileID <= 0 {
				return fmt.Errorf("journal: a link of file %d", l.FileID)
			}
			result, err := insert.ExecContext(ctx, disk, kind, []byte(l.Path), l.FileKey, nowNS, l.FileID)
			if err != nil {
				return fmt.Errorf("journal: %w", err)
			}
			id, err := result.LastInsertId()
			if err != nil {
				return fmt.Errorf("journal: %w", err)
			}
			out = append(out, Op{ID: id, Disk: disk, Kind: kind, Path: l.Path, FileKey: l.FileKey, Link: l.FileID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LinkEnd is an operation of the link guard that went through, and what the names of its file must record then: a link
// made or removed moves the file's change time, for every name it has (specs.md §17).
type LinkEnd struct {
	ID    int64
	Links *Links
}

// EndLinks marks operations of the link guard done, with what the rows of their files' names record, and others undone,
// in one transaction.
func EndLinks(ctx context.Context, db *sql.DB, disk string, done []LinkEnd, undone []int64) error {
	if len(done) == 0 && len(undone) == 0 {
		return nil
	}
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		for i := range done {
			if err := markOp(ctx, tx, done[i].ID, "done"); err != nil {
				return err
			}
			if err := writeLinks(ctx, tx, disk, done[i].Links); err != nil {
				return err
			}
		}
		for _, id := range undone {
			if err := markOp(ctx, tx, id, "undone"); err != nil {
				return err
			}
		}
		return nil
	})
}

// EndOps marks operations done or undone, in one transaction.
func EndOps(ctx context.Context, db *sql.DB, ids []int64, state string) error {
	if len(ids) == 0 {
		return nil
	}
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, id := range ids {
			if err := markOp(ctx, tx, id, state); err != nil {
				return err
			}
		}
		return nil
	})
}

// linkOf is what the column link holds: NULL for an operation that is about no link.
func linkOf(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id > 0}
}
