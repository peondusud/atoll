package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// What a look limited to the names the watcher gave reads of the catalog (specs.md §15.13): the rows at or below those
// names, and those of the inodes it found there, wherever their names are. Each read goes through an index, by path
// or by inode, so that such a look costs what it looked at, never the size of the disk.

// TopNames gives the names in byte order, each once, leaving out those at or below another: a name covers what is below
// it.
func TopNames(names []string) []string {
	list := slices.Clone(names)
	slices.Sort(list)
	list = slices.Compact(list)
	out := list[:0]
	for _, n := range list {
		if len(out) > 0 {
			last := out[len(out)-1]
			if n == last || strings.HasPrefix(n, last+"/") {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// EachKnownCopyAt is EachKnownCopy for the copies at or below some names of a disk: in the byte order of the names they
// sit at, a copy at its file's name before one left behind at the same name.
func EachKnownCopyAt(ctx context.Context, db *sql.DB, disk string, names []string, fn func(*KnownCopy)) error {
	var settled, behind []KnownCopy
	for _, name := range TopNames(names) {
		lo, hi := folderRange(name)
		err := eachKnown(ctx, db, false, &settled, `
			SELECT f.lpath, `+knownColumns+`
			FROM files f CROSS JOIN copies c ON c.file_id = f.id AND c.disk_id = ?1
			WHERE (f.lpath = ?2 OR (f.lpath >= ?3 AND f.lpath < ?4)) AND c.old_lpath IS NULL AND c.state <> 'pending'`,
			disk, []byte(name), lo, hi)
		if err != nil {
			return fmt.Errorf("copies of %q: %w", disk, err)
		}
		err = eachKnown(ctx, db, true, &behind, `
			SELECT c.old_lpath, `+knownColumns+`, f.lpath
			FROM copies c JOIN files f ON f.id = c.file_id
			WHERE c.disk_id = ?1 AND (c.old_lpath = ?2 OR (c.old_lpath >= ?3 AND c.old_lpath < ?4)) AND c.state <> 'pending'`,
			disk, []byte(name), lo, hi)
		if err != nil {
			return fmt.Errorf("copies of %q: %w", disk, err)
		}
	}
	byPath := func(a, b KnownCopy) int { return cmp.Compare(a.Path, b.Path) }
	slices.SortStableFunc(settled, byPath)
	slices.SortStableFunc(behind, byPath)
	for len(settled) > 0 || len(behind) > 0 {
		if len(settled) > 0 && (len(behind) == 0 || settled[0].Path <= behind[0].Path) {
			fn(&settled[0])
			settled = settled[1:]
			continue
		}
		fn(&behind[0])
		behind = behind[1:]
	}
	return nil
}

// eachKnown reads rows of known copies into a list, the file's own name last for a copy left behind.
func eachKnown(ctx context.Context, db *sql.DB, isBehind bool, list *[]KnownCopy, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k KnownCopy
		var place, file []byte
		more := []any{}
		if isBehind {
			more = append(more, &file)
		}
		if err := scanKnown(rows, &k, &place, more...); err != nil {
			return err
		}
		k.Path = string(place)
		k.FilePath = k.Path
		if isBehind {
			k.FilePath = string(file)
		}
		*list = append(*list, k)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// KnownCopiesByKey gives the copies of a disk that have one of these inodes, through the index copies_by_inode: where
// a file a look found at a name with no row may have come from.
func KnownCopiesByKey(ctx context.Context, db *sql.DB, disk string, keys []string) ([]KnownCopy, error) {
	var out []KnownCopy
	for _, key := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		err := eachKnown(ctx, db, true, &out, `
			SELECT COALESCE(c.old_lpath, f.lpath), `+knownColumns+`, f.lpath
			FROM copies c JOIN files f ON f.id = c.file_id
			WHERE c.disk_id = ?1 AND c.file_key = ?2 AND c.state <> 'pending'`, disk, key)
		if err != nil {
			return nil, fmt.Errorf("copies of %q: %w", disk, err)
		}
	}
	return out, nil
}

// EachFolderRowAt is EachFolderRow for the folders at or below some names of a disk, in byte order.
func EachFolderRowAt(ctx context.Context, db *sql.DB, disk string, names []string, fn func(*EntryIdentity)) error {
	var list []EntryIdentity
	for _, name := range TopNames(names) {
		lo, hi := folderRange(name)
		rows, err := db.QueryContext(ctx, `
			SELECT lpath, file_key, btime_ns FROM entries
			WHERE disk_id = ?1 AND kind = 'dir' AND (lpath = ?2 OR (lpath >= ?3 AND lpath < ?4))`, disk, []byte(name), lo, hi)
		if err != nil {
			return fmt.Errorf("folders of %q: %w", disk, err)
		}
		list, err = appendIdentities(rows, list)
		if err != nil {
			return fmt.Errorf("folders of %q: %w", disk, err)
		}
	}
	slices.SortFunc(list, func(a, b EntryIdentity) int { return cmp.Compare(a.Path, b.Path) })
	for i := range list {
		fn(&list[i])
	}
	return nil
}

// FolderRowsByKey gives the folder rows of a disk that have one of these inodes: where a folder a look found with no row
// may have come from.
func FolderRowsByKey(ctx context.Context, db *sql.DB, disk string, keys []string) ([]EntryIdentity, error) {
	var list []EntryIdentity
	for _, key := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		rows, err := db.QueryContext(ctx, "SELECT lpath, file_key, btime_ns FROM entries WHERE disk_id = ? AND kind = 'dir' AND file_key = ?", disk, key)
		if err != nil {
			return nil, fmt.Errorf("folders of %q: %w", disk, err)
		}
		if list, err = appendIdentities(rows, list); err != nil {
			return nil, fmt.Errorf("folders of %q: %w", disk, err)
		}
	}
	return list, nil
}

func appendIdentities(rows *sql.Rows, list []EntryIdentity) ([]EntryIdentity, error) {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p []byte
		var e EntryIdentity
		if err := rows.Scan(&p, &e.FileKey, &e.BtimeNS); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		e.Path = string(p)
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return list, nil
}

// Clocked is a copy of a disk whose clock runs: the name it sits at, and what its row says became of it.
type Clocked struct {
	Path string
	Kind string
}

// ClockedCopies gives the copies of a disk whose clock runs, by the name they sit at, in byte order: what a look must
// read again for that clock to run out (specs.md §15.12, §15.13).
func ClockedCopies(ctx context.Context, db *sql.DB, disk string) ([]Clocked, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(c.old_lpath, f.lpath), c.gone_kind FROM copies c JOIN files f ON f.id = c.file_id
		WHERE c.disk_id = ? AND c.gone_ns IS NOT NULL`, disk)
	if err != nil {
		return nil, fmt.Errorf("copies of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Clocked
	for rows.Next() {
		var p []byte
		var kind sql.NullString
		if err := rows.Scan(&p, &kind); err != nil {
			return nil, fmt.Errorf("copies of %q: %w", disk, err)
		}
		out = append(out, Clocked{Path: string(p), Kind: kind.String})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copies of %q: %w", disk, err)
	}
	slices.SortFunc(out, func(a, b Clocked) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}
