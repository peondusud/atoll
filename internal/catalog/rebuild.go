package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// ReasonUnknown is the reason of an entry of the trash a catalog rebuild listed again: nothing on its disk says why it
// went there (specs.md §9, §17).
const ReasonUnknown = "unknown"

// FoundTrash is an entry of a disk's trash a catalog rebuild lists again (specs.md §9, §17): the operation whose folder
// it is, the chunks of a repair or one file, its logical path and its place in the trash, its size, and the inode of a
// file. Deleted and Expires are its times.
type FoundTrash struct {
	OpID      int64
	Chunks    bool
	Path      string
	TrashPath string
	Size      int64
	FileKey   string
	DeletedNS int64
	ExpiresNS int64
}

// ListTrashAgain records each entry a rebuild found on a disk's trash, in one transaction: its operation, done, with
// the id its folder has, which no new operation is given (StartOpsAfter), and its entry, of the reason repaired for the
// chunks of a repair and unknown otherwise, with no hash.
func ListTrashAgain(ctx context.Context, db *sql.DB, disk string, list []FoundTrash) error {
	if len(list) == 0 {
		return nil
	}
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		for i := range list {
			e := &list[i]
			kind, reason, key := OpTrash, ReasonUnknown, sql.NullString{String: e.FileKey, Valid: e.FileKey != ""}
			if e.Chunks {
				kind, reason, key = OpRepair, ReasonRepaired, sql.NullString{}
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state) VALUES (?, ?, ?, ?, ?, ?, ?, 'done')",
				e.OpID, disk, kind, []byte(e.Path), []byte(e.TrashPath), key, e.DeletedNS)
			if err != nil {
				return fmt.Errorf("journal: operation %d: %w", e.OpID, err)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, blake3, reason, deleted_ns, expires_ns)
				VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
				[]byte(e.Path), disk, []byte(e.TrashPath), e.OpID, e.Size, reason, e.DeletedNS, e.ExpiresNS)
			if err != nil {
				return fmt.Errorf("trash: operation %d: %w", e.OpID, err)
			}
		}
		return nil
	})
}

// StartOpsAfter makes the next operation's id come after id, at the least: a rebuilt catalog's operations must not
// meet the folders and parts the old ones left on the disks (specs.md §9, §17).
func StartOpsAfter(ctx context.Context, db *sql.DB, id int64) error {
	return startAfter(ctx, db, "ops", id)
}

// StartFilesAfter makes the next file's id come after id, at the least: a link of the guard an old catalog left on a
// disk must not name a new file (specs.md §9, §17).
func StartFilesAfter(ctx context.Context, db *sql.DB, id int64) error {
	return startAfter(ctx, db, "files", id)
}

// startAfter moves the counter of a table's ids past id, never back.
func startAfter(ctx context.Context, db *sql.DB, table string, id int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		var seq sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&seq)
		switch {
		case isNoRows(err):
			_, err = tx.ExecContext(ctx, "INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)", table, id)
		case err != nil:
		case seq.Int64 < id:
			_, err = tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq = ? WHERE name = ?", id, table)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		return nil
	})
}

// Checkpoint moves what a catalog's write-ahead log holds into the catalog itself, and empties the log, without
// reading the catalog's schema: a catalog this version refuses, of a later one or damaged, is checkpointed too, when
// SQLite can. The log then holds nothing a rebuild could set aside apart from it.
func Checkpoint(ctx context.Context, file string) (err error) {
	dsn := (&url.URL{Scheme: "file", Path: file}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("catalog %s: %w", file, err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var busy, logged, moved int64
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &moved); err != nil {
		return fmt.Errorf("catalog %s: %w", file, err)
	}
	if busy != 0 {
		return fmt.Errorf("catalog %s: the checkpoint was kept from finishing", file)
	}
	return nil
}

// OldNotSetAside says that a rebuilt catalog is in place, and that the old one could not take the name it was to go to,
// <catalog>.old-<UTC time>: it stays at the name the new one was made at, which a rebuild run again removes, while its
// write-ahead log and shared memory, when it had them, went beside the name it was to take. Renaming it there puts
// them together (specs.md §9).
type OldNotSetAside struct {
	// File is the catalog's name, where the new one is.
	File string
	// At is where the old catalog stays, and Want the name it was to take.
	At, Want string
	// Log is where its log and shared memory went, in that order, without what it did not have.
	Log []string
	Err error
}

func (e *OldNotSetAside) Error() string {
	where := ""
	if len(e.Log) > 0 {
		where = ", its log at " + strings.Join(e.Log, " and ")
	}
	return fmt.Sprintf("the new catalog is in place at %s, but the old one stays at %s%s, as it could not be renamed to %s: %v",
		e.File, e.At, where, e.Want, e.Err)
}

func (e *OldNotSetAside) Unwrap() error { return e.Err }

// SetAside puts a rebuilt catalog, built at fresh, in place of the one at file, which goes to the name it gives,
// <file>.old-<UTC time>, with its write-ahead log and shared memory beside it. The swap leaves no moment with no
// catalog at file, so that a command that waits for the lock finds one, and locks it (specs.md §9). With no catalog
// at file, fresh is only renamed, and the name given is "".
func SetAside(file, fresh string, nowNS int64) (string, error) {
	there, err := fsx.Exists(file)
	if err != nil {
		return "", fmt.Errorf("catalog %s: %w", file, err)
	}
	if !there {
		if err := fsx.RenameFile(fresh, file); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return "", nil
	}
	old := file + ".old-" + time.Unix(0, nowNS).UTC().Format("20060102T150405Z")
	// The log goes first: left beside the new catalog, SQLite would play the old one's frames into it. A swap that
	// fails puts it back: the old catalog stays at its name, and what its log holds, committed and not yet moved into
	// it when no checkpoint could run, would be lost to it.
	var moved []string
	putBack := func(err error) error {
		for _, suffix := range moved {
			if back := fsx.RenameFile(old+suffix, file+suffix); back != nil {
				err = errors.Join(err, back)
			}
		}
		return fmt.Errorf("catalog: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		err := fsx.RenameFile(file+suffix, old+suffix)
		switch {
		case err == nil:
			moved = append(moved, suffix)
		case !fsx.IsAbsent(err):
			return "", putBack(err)
		}
	}
	if err := fsx.ExchangeFiles(fresh, file); err != nil {
		return "", putBack(err)
	}
	// The new catalog is in place from here on. The old one, at fresh now, is left where it is should it not take its
	// name, as moving its log back beside it would leave both where a rebuild run again removes them: the error says
	// where each is, and that renaming it puts them together.
	if err := fsx.RenameFile(fresh, old); err != nil {
		half := &OldNotSetAside{File: file, At: fresh, Want: old, Err: err}
		for _, suffix := range moved {
			half.Log = append(half.Log, old+suffix)
		}
		return "", fmt.Errorf("catalog: %w", half)
	}
	return old, nil
}
