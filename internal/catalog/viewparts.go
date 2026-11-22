package catalog

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/peondusud/atoll/internal/layout"
)

// ViewPart is a copy Atoll's own view writes in parallel, as a new file in immediate mode is written, into a part of
// <workdir>/tmp/ on one of its replicas' targets (specs.md §12.3, §21.2): disk_id the target, lpath the file's path,
// op_id the operation create of ops that names the part. Planned while the program writes, done once it closed the
// file and the part holds what the original does, and taken in once a pass's job adopts the part, or drops it.
const ViewPart = "part"

// BeginViewPart journals a part the view writes in parallel, before it is made: an operation create of ops on the
// target, planned, whose part is <op id>.part, journaled with its replicate event naming the disk of the original as
// source, and the view's row that names it, in one transaction. Neither a command's reprise nor the daemon's touches
// it, as a view's operation; and tmp/ keeps its part while it is planned. by is who asked for the file the part copies,
// nil outside a request (specs.md §9).
func BeginViewPart(ctx context.Context, db *sql.DB, disk, path, source string, nowNS int64, by *ViewAsker) (*Op, *ViewOp, error) {
	op := &Op{Disk: disk, Kind: OpCreate, Path: path}
	v := &ViewOp{Kind: ViewPart, Disk: disk, Path: path, State: OpPlanned, AtNS: nowNS, Asker: by}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES (?, 'create', ?, ?, 'planned')", disk, []byte(path), nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		op.Path2 = layout.PartName(op.ID)
		if _, err := tx.ExecContext(ctx, "UPDATE ops SET path2 = ? WHERE id = ?", []byte(op.Path2), op.ID); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if err := writeEventData(ctx, tx, nowNS, "info", "replicate", op, eventData{Source: source}); err != nil {
			return err
		}
		v.OpID = op.ID
		return insertViewOp(ctx, tx, v)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%q: %w", path, err)
	}
	return op, v, nil
}

// ViewPartWritten says that the program closed the file and that the part holds what the original holds, synced: the
// view's row is done, and its operation create stays planned, as the part is not at its name yet.
func ViewPartWritten(ctx context.Context, db *sql.DB, v *ViewOp) error {
	if _, err := db.ExecContext(ctx, "UPDATE view_ops SET state = 'done' WHERE id = ? AND kind = 'part'", v.ID); err != nil {
		return fmt.Errorf("view journal: %w", err)
	}
	v.State = OpDone
	return nil
}

// DismissViewPart says that a pass dropped a part the view wrote, which no job adopted: its operation create is undone,
// and the view's row taken in, done as it was written.
func DismissViewPart(ctx context.Context, db *sql.DB, v *ViewOp, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, v.OpID, OpUndone); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE view_ops SET taken_ns = ? WHERE id = ? AND taken_ns IS NULL", nowNS, v.ID); err != nil {
			return fmt.Errorf("view journal: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", v.Path, err)
	}
	return nil
}

// adoptViewPartsSQL gives each part the view wrote, done and not taken in, to the replicate job of the file at its path
// on its disk that has no part yet, queued: the job's payload names it.
const adoptViewPartsSQL = `
	SELECT v.id, v.op_id, CAST(o.path2 AS TEXT), j.id
	FROM view_ops v
	JOIN ops o ON o.id = v.op_id AND o.state = 'planned'
	JOIN files f ON f.lpath = v.lpath
	JOIN jobs j ON j.file_id = f.id AND j.kind = 'replicate' AND j.dst_disk = v.disk_id AND j.state = 'queued'
		AND json_extract(j.payload, '$.part') IS NULL
		AND NOT EXISTS (SELECT 1 FROM job_chunks k WHERE k.job_id = j.id)
	WHERE v.kind = 'part' AND v.state = 'done' AND v.taken_ns IS NULL
	ORDER BY v.id`

// AdoptViewParts gives each part the view wrote, done and not taken in, to the replicate job of its file on its disk
// that has none yet, in one transaction (specs.md §12.3, §21.2): the job's payload names it, so that the job resumes
// from it, comparing what it holds with the original rather than writing it again; the view's operation create is
// undone, the job's own naming the part from then on; and the view's row is taken in. It says how many it gave.
func AdoptViewParts(ctx context.Context, db *sql.DB, nowNS int64) (int, error) {
	given := 0
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		list, err := adoptions(ctx, tx)
		if err != nil {
			return err
		}
		taken := map[int64]bool{}
		for _, a := range list {
			// One part a job: a second the view wrote for the same job is dropped as no job's.
			if taken[a.job] {
				continue
			}
			taken[a.job] = true
			if _, err := tx.ExecContext(ctx, "UPDATE jobs SET payload = json_set(COALESCE(payload, '{}'), '$.part', ?) WHERE id = ?", a.part, a.job); err != nil {
				return fmt.Errorf("job: %w", err)
			}
			if err := markOp(ctx, tx, a.op, OpUndone); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE view_ops SET taken_ns = ? WHERE id = ?", nowNS, a.view); err != nil {
				return fmt.Errorf("view journal: %w", err)
			}
			given++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("parts: %w", err)
	}
	return given, nil
}

// adoption is a part the view handed over, and the job that takes it.
type adoption struct {
	view, op, job int64
	part          string
}

func adoptions(ctx context.Context, tx *sql.Tx) ([]adoption, error) {
	rows, err := tx.QueryContext(ctx, adoptViewPartsSQL)
	if err != nil {
		return nil, fmt.Errorf("view journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var list []adoption
	for rows.Next() {
		var a adoption
		if err := rows.Scan(&a.view, &a.op, &a.part, &a.job); err != nil {
			return nil, fmt.Errorf("view journal: %w", err)
		}
		list = append(list, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("view journal: %w", err)
	}
	return list, nil
}

const openViewPartsSQL = `
	SELECT v.id, v.disk_id, v.lpath, v.op_id, v.state, v.at_ns, CAST(COALESCE(o.path2, x'') AS TEXT), o.state
	FROM view_ops v JOIN ops o ON o.id = v.op_id
	WHERE v.kind = 'part' AND v.taken_ns IS NULL AND v.state <> 'undone' ORDER BY v.id`

// ViewPartRow is a part the view wrote or writes, which no job adopted yet: its row, the name of its part in tmp/, and
// the state of its operation create.
type ViewPartRow struct {
	ViewOp
	Part    string
	OpState string
}

// OpenViewParts gives the parts the view writes or wrote that no job adopted yet, nor any pass dropped, by id.
func OpenViewParts(ctx context.Context, db *sql.DB) ([]ViewPartRow, error) {
	rows, err := db.QueryContext(ctx, openViewPartsSQL)
	if err != nil {
		return nil, fmt.Errorf("view journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ViewPartRow
	for rows.Next() {
		var r ViewPartRow
		var path []byte
		if err := rows.Scan(&r.ID, &r.Disk, &path, &r.OpID, &r.State, &r.AtNS, &r.Part, &r.OpState); err != nil {
			return nil, fmt.Errorf("view journal: %w", err)
		}
		r.Kind, r.Path = ViewPart, string(path)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("view journal: %w", err)
	}
	return out, nil
}

// ReplicateJobPart gives the part the replicate job of a file to a disk names, queued or running, "" before its first
// operation, and false with no such job.
func ReplicateJobPart(ctx context.Context, db *sql.DB, fileID int64, disk string) (string, bool, error) {
	var part string
	err := db.QueryRowContext(ctx, "SELECT COALESCE(json_extract(payload, '$.part'), '') FROM jobs WHERE kind = 'replicate' AND file_id = ? AND dst_disk = ? AND state IN ('queued', 'running')",
		fileID, disk).Scan(&part)
	switch {
	case isNoRows(err):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("jobs: %w", err)
	}
	return part, true, nil
}
