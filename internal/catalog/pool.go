package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// Create makes a catalog for a pool, with its folder, if it does not exist, and reports whether it
// did. It may run again, and refuses a catalog of another pool (specs.md §9, §25).
func Create(ctx context.Context, file, poolID string) (created bool, err error) {
	existed, err := fsx.Exists(file)
	if err != nil {
		return false, fmt.Errorf("catalog %q: %w", file, err)
	}
	if !existed {
		if err := fsx.MkdirAll(path.Dir(file)); err != nil {
			return false, fmt.Errorf("catalog %q: %w", file, err)
		}
	}
	db, err := Open(ctx, file)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := writeMeta(ctx, db, poolID); err != nil {
		return false, fmt.Errorf("catalog %q: %w", file, err)
	}
	return !existed, nil
}

func writeMeta(ctx context.Context, db *sql.DB, poolID string) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		current, err := metaValue(ctx, tx, "pool_id")
		switch {
		case err != nil:
			return err
		case current != nil && *current != poolID:
			return fmt.Errorf("it belongs to the pool %q, and the configuration says %q", *current, poolID)
		case current != nil:
			return nil
		}
		for _, row := range [][2]string{{"pool_id", poolID}, {"lpath_key_version", names.CollisionKeyVersion}} {
			if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES (?, ?)", row[0], row[1]); err != nil {
				return fmt.Errorf("meta: %w", err)
			}
		}
		return nil
	})
}

func metaValue(ctx context.Context, tx *sql.Tx, key string) (*string, error) {
	var value string
	err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil //nolint:nilnil // no value is an answer here
	case err != nil:
		return nil, fmt.Errorf("meta: %w", err)
	}
	return &value, nil
}

// OpenPool opens the catalog of a pool, which atoll init created: a catalog that does not exist or
// that belongs to another pool is refused, so a mistyped path cannot start again from nothing in
// silence. Keys computed by another version are computed again (specs.md §9, §15.8).
func OpenPool(ctx context.Context, file, poolID string) (*sql.DB, error) {
	existed, err := fsx.Exists(file)
	if err != nil {
		return nil, fmt.Errorf("catalog %q: %w", file, err)
	}
	if !existed {
		return nil, fmt.Errorf("catalog %q: it does not exist: run atoll init", file)
	}
	db, err := Open(ctx, file)
	if err != nil {
		return nil, err
	}
	if err := checkMeta(ctx, db, poolID); err != nil {
		return nil, errors.Join(fmt.Errorf("catalog %q: %w", file, err), db.Close())
	}
	return db, nil
}

func checkMeta(ctx context.Context, db *sql.DB, poolID string) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		pool, err := metaValue(ctx, tx, "pool_id")
		switch {
		case err != nil:
			return err
		case pool == nil:
			return errors.New("it has no pool: run atoll init")
		case *pool != poolID:
			return fmt.Errorf("it belongs to the pool %q, and the configuration says %q", *pool, poolID)
		}
		version, err := metaValue(ctx, tx, "lpath_key_version")
		if err != nil {
			return err
		}
		if version != nil && *version == names.CollisionKeyVersion {
			return nil
		}
		return recomputeKeys(ctx, tx)
	})
}

// recomputeKeys runs when the catalog's keys come from another implementation or release: such
// keys are never compared (specs.md §15.8).
func recomputeKeys(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"files", "entries"} {
		if err := recomputeTable(ctx, tx, table); err != nil {
			return fmt.Errorf("keys of %s: %w", table, err)
		}
	}
	if err := recomputeOldKeys(ctx, tx); err != nil {
		return fmt.Errorf("keys of copies: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key, value) VALUES ('lpath_key_version', ?)", names.CollisionKeyVersion); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	return nil
}

func recomputeTable(ctx context.Context, tx *sql.Tx, table string) error {
	// The table's name comes from the list above.
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT lpath FROM "+table)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths [][]byte
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET lpath_key = ? WHERE lpath = ?", keyOf(string(p)), p); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	return nil
}

func recomputeOldKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT old_lpath FROM copies WHERE old_lpath IS NOT NULL")
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths [][]byte
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET old_lpath_key = ? WHERE old_lpath = ?", keyOf(string(p)), p); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	return nil
}

// keyOf gives lpath_key, or NULL for a path that is not valid UTF-8.
func keyOf(p string) sql.NullString {
	key, ok := names.CollisionKey(p)
	return sql.NullString{String: key, Valid: ok}
}

// Disk is one disk of the configuration, with what a mount check found. Seen says that the check
// confirmed it, so Capacity, Free and LastSeenNS are worth recording.
type Disk struct {
	ID         string
	UUID       string
	FS         string
	Mount      string
	Root       string
	Workdir    string
	Role       string
	Position   int
	Domain     string // empty when the disk forms its own domain
	Tags       []string
	State      string
	Seen       bool
	Capacity   int64
	Free       int64
	LastSeenNS int64
	// TimePrecisionNS is how finely the disk holds a time (specs.md §9), 0 when no check read it.
	TimePrecisionNS int64
}

// SyncDisks rewrites the disks from the configuration, which wins (specs.md §9). A disk it no longer
// lists is retired, after the others, and what the catalog knows about a disk's copies is never read
// against another disk.
func SyncDisks(ctx context.Context, db *sql.DB, list []Disk) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		for i := range list {
			if err := syncOne(ctx, tx, &list[i]); err != nil {
				return fmt.Errorf("disk %q: %w", list[i].ID, err)
			}
		}
		return retireOthers(ctx, tx, list)
	})
}

func syncOne(ctx context.Context, tx *sql.Tx, d *Disk) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, fs_uuid FROM disks WHERE id = ? OR fs_uuid = ?", d.ID, d.UUID)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var clashes []string
	for rows.Next() {
		var id, uuid string
		if err := rows.Scan(&id, &uuid); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if id != d.ID || uuid != d.UUID {
			clashes = append(clashes, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := checkPlaces(ctx, tx, d); err != nil {
		return err
	}
	for _, id := range clashes {
		holds, err := holdsAnything(ctx, tx, id)
		if err != nil {
			return err
		}
		if holds {
			return fmt.Errorf("the catalog has copies on the disk %q with another identifier or id: use atoll disk replace, or put the configuration back", id)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM disks WHERE id = ?", id); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	domain := sql.NullString{String: d.Domain, Valid: d.Domain != ""}
	capacity, free, lastSeen := sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}
	if d.Seen {
		capacity = sql.NullInt64{Int64: d.Capacity, Valid: true}
		free = sql.NullInt64{Int64: d.Free, Valid: true}
		lastSeen = sql.NullInt64{Int64: d.LastSeenNS, Valid: true}
	}
	// A check that did not read the head keeps what an earlier one read.
	precision := sql.NullInt64{Int64: d.TimePrecisionNS, Valid: d.TimePrecisionNS > 0}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, domain, tags, capacity_bytes, free_bytes, state, last_seen_ns, time_precision_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			fs_uuid = excluded.fs_uuid, fs_type = excluded.fs_type, mount_path = excluded.mount_path,
			root = excluded.root, workdir = excluded.workdir, role = excluded.role,
			position = excluded.position, domain = excluded.domain, tags = excluded.tags,
			capacity_bytes = COALESCE(excluded.capacity_bytes, capacity_bytes),
			free_bytes = COALESCE(excluded.free_bytes, free_bytes),
			state = excluded.state,
			last_seen_ns = COALESCE(excluded.last_seen_ns, last_seen_ns),
			time_precision_ns = COALESCE(excluded.time_precision_ns, time_precision_ns)`,
		d.ID, d.UUID, d.FS, d.Mount, d.Root, d.Workdir, d.Role, d.Position, domain, string(tagsJSON), capacity, free, d.State, lastSeen, precision)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// checkPlaces refuses another root or work folder for a disk that holds anything: the catalog's paths
// are relative to them, so a copy would read as absent and an entry of the trash as gone (specs.md
// §9, §17.2).
func checkPlaces(ctx context.Context, tx *sql.Tx, d *Disk) error {
	var root, workdir string
	err := tx.QueryRowContext(ctx, "SELECT root, workdir FROM disks WHERE id = ?", d.ID).Scan(&root, &workdir)
	switch {
	case isNoRows(err):
		return nil
	case err != nil:
		return fmt.Errorf("read: %w", err)
	case root == d.Root && workdir == d.Workdir:
		return nil
	}
	holds, err := holdsAnything(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	if holds {
		return fmt.Errorf("the catalog has copies or a trash on it at the root %q and the work folder %q: put the configuration back", root, workdir)
	}
	return nil
}

func holdsAnything(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var holds bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM copies WHERE disk_id = ?1) OR EXISTS(SELECT 1 FROM entries WHERE disk_id = ?1)
			OR EXISTS(SELECT 1 FROM trash WHERE disk_id = ?1) OR EXISTS(SELECT 1 FROM ops WHERE disk_id = ?1 AND state = 'planned')`, id).Scan(&holds)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	return holds, nil
}

func retireOthers(ctx context.Context, tx *sql.Tx, list []Disk) error {
	listed := make(map[string]bool, len(list))
	for i := range list {
		listed[list[i].ID] = true
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM disks ORDER BY id")
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var others []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if !listed[id] {
			others = append(others, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	for i, id := range others {
		if _, err := tx.ExecContext(ctx, "UPDATE disks SET state = 'retired', position = ? WHERE id = ?", len(list)+i, id); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	return nil
}

// SetDiskState records what became of a disk during a scan.
func SetDiskState(ctx context.Context, db *sql.DB, id, state string) error {
	if _, err := db.ExecContext(ctx, "UPDATE disks SET state = ? WHERE id = ?", state, id); err != nil {
		return fmt.Errorf("disk %q: %w", id, err)
	}
	return nil
}

// inTransaction commits what fn did, or nothing.
func inTransaction(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // after a commit there is nothing to roll back
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
