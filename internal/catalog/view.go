package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ViewDisk is what Atoll's own view reads of a disk (specs.md §21.2).
type ViewDisk struct {
	ID             string
	Position       int
	State          string
	Cache          bool
	Capacity, Free sql.NullInt64
	// Tags are the disk's tags, which read.classes names (specs.md §7).
	Tags []string
}

// ViewDisks reads every disk, for the view.
func ViewDisks(ctx context.Context, db *sql.DB) ([]ViewDisk, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, position, state, role, capacity_bytes, free_bytes, tags FROM disks ORDER BY position")
	if err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ViewDisk
	for rows.Next() {
		var d ViewDisk
		var role, tags string
		if err := rows.Scan(&d.ID, &d.Position, &d.State, &role, &d.Capacity, &d.Free, &tags); err != nil {
			return nil, fmt.Errorf("disks: %w", err)
		}
		if err := json.Unmarshal([]byte(tags), &d.Tags); err != nil {
			return nil, fmt.Errorf("disk %q: tags: %w", d.ID, err)
		}
		d.Cache = role == "cache"
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("disks: %w", err)
	}
	return out, nil
}

// ViewCopy is a copy of a file, for the view: what its row holds, NULL where it holds nothing.
type ViewCopy struct {
	Disk, State          string
	Size, MtimeNS, Nlink sql.NullInt64
	Mode, UID, GID       sql.NullInt64
	BtimeNS              sql.NullInt64
	FirstSeenNS          int64
	// GoneKind is copies.gone_kind while the copy's clock runs, and OldPath copies.old_lpath while it has not followed
	// its file's rename: "" otherwise.
	GoneKind, OldPath string
}

// ViewFile is the file at a path, for the view.
type ViewFile struct {
	State, Original string
	ConflictHeld    bool
	Size, MtimeNS   int64
	Copies          []ViewCopy
}

// ViewEntry is an entry at a path, for the view.
type ViewEntry struct {
	Disk, Kind     string
	Mode, UID, GID sql.NullInt64
	Size, MtimeNS  sql.NullInt64
	Target         []byte
}

// ViewAt is what the catalog holds at a path, for the view: its file, nil when it has none, with its copies, and its
// entries, one a disk.
type ViewAt struct {
	File    *ViewFile
	Entries []ViewEntry
}

// ViewRows reads what the catalog holds at a path, for the view. The path is the exact logical path, "" for the root.
func ViewRows(ctx context.Context, db *sql.DB, path string) (*ViewFile, []ViewEntry, error) {
	at, err := ViewRowsOf(ctx, db, []string{path})
	if err != nil {
		return nil, nil, err
	}
	return at[path].File, at[path].Entries, nil
}

// viewBatch is how many paths one query of ViewRowsOf asks for.
const viewBatch = 500

// ViewRowsOf reads what the catalog holds at each of several paths, a few queries for a folder's listing rather than
// two a name: 2,000 shows cost 134 ms one at a time. A path the catalog holds nothing at has no key.
func ViewRowsOf(ctx context.Context, db *sql.DB, paths []string) (map[string]ViewAt, error) {
	out := map[string]ViewAt{}
	for from := 0; from < len(paths); from += viewBatch {
		batch := paths[from:min(from+viewBatch, len(paths))]
		args := make([]any, len(batch))
		for i, p := range batch {
			args[i] = []byte(p)
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(batch)), ", ")
		if err := viewFiles(ctx, db, marks, args, out); err != nil {
			return nil, err
		}
		if err := viewEntries(ctx, db, marks, args, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func viewFiles(ctx context.Context, db *sql.DB, marks string, args []any, out map[string]ViewAt) error {
	rows, err := db.QueryContext(ctx, `SELECT f.lpath, f.state, coalesce(f.original_disk, ''), f.conflict_held, f.size, f.mtime_ns,
			c.disk_id, c.state, c.size, c.mtime_ns, c.nlink, c.mode, c.uid, c.gid, c.btime_ns, c.first_seen_ns,
			CASE WHEN c.gone_ns IS NULL THEN '' ELSE coalesce(c.gone_kind, 'gone') END, coalesce(c.old_lpath, x'')
		FROM files f LEFT JOIN copies c ON c.file_id = f.id WHERE f.lpath IN (`+marks+`) ORDER BY f.lpath, c.disk_id`, args...)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path []byte
		var f ViewFile
		var c ViewCopy
		var disk, state, goneKind sql.NullString
		var firstSeen sql.NullInt64
		var oldPath []byte
		if err := rows.Scan(&path, &f.State, &f.Original, &f.ConflictHeld, &f.Size, &f.MtimeNS, &disk, &state, &c.Size, &c.MtimeNS, &c.Nlink, &c.Mode, &c.UID,
			&c.GID, &c.BtimeNS, &firstSeen, &goneKind, &oldPath); err != nil {
			return fmt.Errorf("files: %w", err)
		}
		at := out[string(path)]
		if at.File == nil {
			at.File = &f
		}
		if disk.Valid {
			c.Disk, c.State, c.FirstSeenNS, c.GoneKind, c.OldPath = disk.String, state.String, firstSeen.Int64, goneKind.String, string(oldPath)
			at.File.Copies = append(at.File.Copies, c)
		}
		out[string(path)] = at
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	return nil
}

func viewEntries(ctx context.Context, db *sql.DB, marks string, args []any, out map[string]ViewAt) error {
	rows, err := db.QueryContext(ctx, "SELECT lpath, disk_id, kind, mode, uid, gid, size, mtime_ns, target FROM entries WHERE lpath IN ("+marks+") ORDER BY lpath, disk_id", args...)
	if err != nil {
		return fmt.Errorf("entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path []byte
		var e ViewEntry
		if err := rows.Scan(&path, &e.Disk, &e.Kind, &e.Mode, &e.UID, &e.GID, &e.Size, &e.MtimeNS, &e.Target); err != nil {
			return fmt.Errorf("entries: %w", err)
		}
		at := out[string(path)]
		at.Entries = append(at.Entries, e)
		out[string(path)] = at
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("entries: %w", err)
	}
	return nil
}

// ViewNames gives the names directly below a folder that a file or an entry of the catalog holds, each once, in byte
// order, "" for the root (specs.md §21.2). It seeks the index once a name, whatever lies below each: the root of a
// library holds a few names and every path, and a range of every path below it read them all.
func ViewNames(ctx context.Context, db *sql.DB, dir string) ([]string, error) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	set := map[string]bool{}
	for _, table := range []string{"files", "entries"} {
		if err := namesBelow(ctx, db, table, prefix, set); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

// namesBelowQueries are namesBelow's queries, each written whole for a table it reads, so that no query is built: below
// the root, then below a folder.
var namesBelowQueries = map[string][2]string{
	"files": {
		"SELECT lpath FROM files WHERE lpath >= ? ORDER BY lpath LIMIT 1",
		"SELECT lpath FROM files WHERE lpath >= ? AND lpath < ? ORDER BY lpath LIMIT 1",
	},
	"entries": {
		"SELECT lpath FROM entries WHERE lpath >= ? ORDER BY lpath LIMIT 1",
		"SELECT lpath FROM entries WHERE lpath >= ? AND lpath < ? ORDER BY lpath LIMIT 1",
	},
}

// namesBelow adds the names directly below prefix that a table's paths hold. After a name, the next seek starts past
// every path below it: past name + "/" + anything, which is name + "0", as '0' comes right after '/'; and past the name
// itself, name + "\x00", as no path holds a NUL. A table of no query of namesBelowQueries is an error.
func namesBelow(ctx context.Context, db *sql.DB, table, prefix string, set map[string]bool) error {
	queries, known := namesBelowQueries[table]
	if !known {
		return fmt.Errorf("the view reads no table %q", table)
	}
	query := queries[0]
	var upper []byte
	if prefix != "" {
		query = queries[1]
		// The paths below prefix end before prefix with its '/' turned into '0'.
		upper = []byte(prefix[:len(prefix)-1] + "0")
	}
	from := []byte(prefix)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		var p []byte
		var err error
		if upper != nil {
			err = db.QueryRowContext(ctx, query, from, upper).Scan(&p)
		} else {
			err = db.QueryRowContext(ctx, query, from).Scan(&p)
		}
		switch {
		case err == sql.ErrNoRows:
			return nil
		case err != nil:
			return fmt.Errorf("%s: %w", table, err)
		}
		rest := string(p[len(prefix):])
		if rest == "" {
			// The folder itself, as an entry: no name below it.
			from = append([]byte(prefix), 0)
			continue
		}
		name, below, _ := strings.Cut(rest, "/")
		set[name] = true
		if below != "" || strings.HasSuffix(rest, "/") {
			from = []byte(prefix + name + "0")
		} else {
			from = []byte(prefix + name + "\x00")
		}
	}
}
